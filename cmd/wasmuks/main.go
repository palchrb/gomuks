// gomuks - A Matrix client written in Go.
// Copyright (C) 2025 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

//go:build js

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"syscall/js"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/exbytes"
	"go.mau.fi/util/exstrings"
	"go.mau.fi/util/ptr"
	"go.mau.fi/zeroconfig"

	"go.mau.fi/gomuks/pkg/gomuks"
	"go.mau.fi/gomuks/pkg/hicli"
	"go.mau.fi/gomuks/pkg/hicli/jsoncmd"
	_ "go.mau.fi/gomuks/pkg/sqlite-wasm-js"
	"go.mau.fi/gomuks/version"
)

var gmx *gomuks.Gomuks

func postMessage(cmd jsoncmd.Name, reqID int64, data any) {
	var dataJSON json.RawMessage
	var ok bool
	if dataJSON, ok = data.(json.RawMessage); !ok {
		var err error
		dataJSON, err = json.Marshal(data)
		if err != nil {
			gmx.Log.Err(err).Msg("Failed to marshal data for postMessage")
			return
		}
	}
	js.Global().Call("postMessage", js.ValueOf(map[string]any{
		"command":    string(cmd),
		"request_id": int(reqID),
		"data":       exbytes.UnsafeString(dataJSON),
	}))
}

func jsMessageListener(_ js.Value, args []js.Value) any {
	data := args[0].Get("data")
	wrappedCmd := &hicli.JSONCommand{
		Command:   jsoncmd.Name(data.Get("command").String()),
		RequestID: int64(data.Get("request_id").Int()),
		Data:      exstrings.UnsafeBytes(data.Get("data").String()),
	}
	if wrappedCmd.Command == "wasm-upload" {
		fileName := data.Get("filename").String()
		encrypt := data.Get("encrypt").Bool()
		payloadVal := data.Get("payload")
		payload := make([]byte, payloadVal.Length())
		js.CopyBytesToGo(payload, payloadVal)
		go func() {
			ctx := gmx.Log.With().Str("action", "wasmuks upload").Logger().WithContext(context.Background())
			resp, err := uploadMedia(ctx, fileName, encrypt, payload)
			if err != nil {
				postMessage(jsoncmd.RespError, wrappedCmd.RequestID, ptr.Ptr(gomuks.ToRespError(err)))
			} else {
				postMessage(jsoncmd.RespSuccess, wrappedCmd.RequestID, resp)
			}
		}()
		return nil
	}
	go func() {
		resp := gmx.Client.SubmitJSONCommand(context.Background(), wrappedCmd)
		postMessage(resp.Command, resp.RequestID, resp.Data)
	}()
	return nil
}

// runMigrations runs schema migrations on a throwaway multi-connection pool
// with normal locking. dbutil's sqlite-fkey-off upgrade path never releases
// the connection it acquires, which with the real single-connection EXCLUSIVE
// pool would leave the only connection (and the file lock) stuck forever.
// Leaked connections on this pool are idle and hold no locks, so they are
// harmless once the pool is closed.
func runMigrations() error {
	if err := gmx.InitClient(); err != nil {
		return err
	}
	log := gmx.Log.With().Str("component", "migrations").Logger()
	rawDB, err := dbutil.NewFromConfig("gomuks", dbutil.Config{
		PoolConfig: dbutil.PoolConfig{
			Type:         "sqlite-wasm-js",
			URI:          "file:/gomuks.db?_txlock=immediate&_locking_mode=NORMAL&_journal_mode=DELETE",
			MaxOpenConns: 5,
			MaxIdleConns: 1,
		},
	}, dbutil.ZeroLogger(log))
	if err != nil {
		return fmt.Errorf("failed to open migration pool: %w", err)
	}
	defer func() {
		_ = rawDB.Close()
	}()
	return hicli.UpgradeDatabases(log.WithContext(context.Background()), rawDB, nil, log, gmx.PickleKey())
}

// wasmuksInit mirrors WasmuksInit in web/src/api/wasmclient.ts. It's passed
// as the worker's name so it's available before Go starts, without a
// message-ordering race.
type wasmuksInit struct {
	LastServerTS int64 `json:"last_server_ts"`
}

func readInit() (init wasmuksInit) {
	name := js.Global().Get("name")
	if name.Type() != js.TypeString || name.String() == "" {
		return
	}
	if err := json.Unmarshal([]byte(name.String()), &init); err != nil {
		gmx.Log.Warn().Err(err).Msg("Failed to parse worker init parameters")
	}
	return
}

// awaitPromise blocks the goroutine until a JS promise settles.
func awaitPromise(promise js.Value) (js.Value, error) {
	done := make(chan struct{})
	var result js.Value
	var rejected bool
	then := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			result = args[0]
		}
		close(done)
		return nil
	})
	catch := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			result = args[0]
		}
		rejected = true
		close(done)
		return nil
	})
	defer then.Release()
	defer catch.Release()
	promise.Call("then", then, catch)
	<-done
	if rejected {
		return result, fmt.Errorf("promise rejected: %s", result.String())
	}
	return result, nil
}

// removeData wipes everything the worker stored: the OPFS database files and
// the decrypted media cache. The main thread clears IndexedDB and
// localStorage itself when the logout call returns.
func removeData(ctx context.Context, _ *hicli.HiClient) error {
	log := zerolog.Ctx(ctx)
	poolUtil := js.Global().Get("sqlite3").Get("PoolUtil")
	if _, err := awaitPromise(poolUtil.Call("wipeFiles")); err != nil {
		return fmt.Errorf("failed to wipe OPFS files: %w", err)
	}
	log.Info().Msg("Wiped OPFS database files")
	if caches := js.Global().Get("caches"); caches.Type() == js.TypeObject {
		if _, err := awaitPromise(caches.Call("delete", "wasmuks-media-v1")); err != nil {
			log.Warn().Err(err).Msg("Failed to delete media cache")
		} else {
			log.Info().Msg("Deleted media cache")
		}
	}
	return nil
}

func main() {
	hicli.InitialDeviceDisplayName = "gomuks web"
	gmx = gomuks.NewGomuks()
	gmx.Config = gomuks.Config{
		Logging: zeroconfig.Config{
			Writers: []zeroconfig.WriterConfig{{
				Type: zeroconfig.WriterTypeJS,
			}},
			Timestamp: ptr.Ptr(false),
		},
	}
	// The driver defaults to EXCLUSIVE locking + PERSIST journal on OPFS,
	// which requires that only one connection uses the file. See
	// pkg/sqlite-wasm-js/conn.go for why.
	gmx.GetDBConfig = func() dbutil.PoolConfig {
		return dbutil.PoolConfig{
			Type:         "sqlite-wasm-js",
			URI:          "file:/gomuks.db?_txlock=immediate",
			MaxOpenConns: 1,
			MaxIdleConns: 1,
		}
	}
	// A pool with one connection turns any "query outside the transaction
	// while inside DoTxn" bug into a hang, so make dbutil panic instead.
	dbutil.ForceDeadlockDetection = true

	gmx.RemoveDataFunc = removeData
	gmx.EventBuffer = gomuks.NewEventBuffer(0)
	gmx.EventBuffer.Subscribe(0, nil, func(evt *gomuks.BufferedEvent) {
		postMessage(evt.Command, evt.RequestID, evt.Data)
	})
	gomuks.DisablePush = true
	js.Global().Call("addEventListener", "message", js.FuncOf(jsMessageListener))
	js.Global().Set("meowDownloadMedia", js.FuncOf(jsDownloadCallback))
	postMessage("wasm-connection", 0, json.RawMessage(`{"connected":true,"reconnecting":false,"error":null}`))

	gmx.SetupLog()
	gmx.Log.Info().
		Str("version", version.Gomuks.FormattedVersion).
		Str("go_version", runtime.Version()).
		Time("built_at", version.Gomuks.BuildTime).
		Msg("Initializing gomuks in wasm")
	if err := runMigrations(); err != nil {
		gmx.Log.WithLevel(zerolog.FatalLevel).Err(err).Msg("Failed to run database migrations")
		postMessage("wasm-connection", 0, json.RawMessage(`{"connected":false,"reconnecting":false,"error":"Database migration failed"}`))
		return
	}
	gmx.Client.SingleConnectionDB = true
	gmx.StartClient()
	if stats := gmx.Client.DB.RawDB.Stats(); stats.InUse > 0 {
		gmx.Log.Error().Int("in_use", stats.InUse).Msg("Database connections still in use after startup, expect hangs")
	}
	gmx.Log.Info().Msg("Initialization complete")
	postMessage(jsoncmd.EventClientState, 0, gmx.Client.State())
	postMessage(jsoncmd.EventSyncStatus, 0, gmx.Client.SyncStatus.Load())
	if gmx.Client.IsLoggedInAndVerified() {
		ctx := gmx.Log.WithContext(context.Background())
		init := readInit()
		// If the frontend restored its room list from IndexedDB, only send
		// what changed since then (same as the websocket path does).
		gmx.Log.Info().Int64("catchup_since", init.LastServerTS).Msg("Sending initial sync")
		for payload := range gmx.Client.GetInitialSync(ctx, 100, init.LastServerTS) {
			postMessage(jsoncmd.EventSyncComplete, 0, payload)
		}
		postMessage(jsoncmd.EventInitComplete, 0, gmx.Client.SyncStatus.Load())
	}

	select {}
}
