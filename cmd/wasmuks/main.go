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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall/js"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/exbytes"
	"go.mau.fi/util/exstrings"
	"go.mau.fi/util/ptr"
	"go.mau.fi/zeroconfig"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/gomuks/pkg/gomuks"
	"go.mau.fi/gomuks/pkg/hicli"
	"go.mau.fi/gomuks/pkg/hicli/jsoncmd"
	_ "go.mau.fi/gomuks/pkg/sqlite-wasm-js"
	"go.mau.fi/gomuks/version"
)

var gmx *gomuks.Gomuks

// Errors are posted as a plain string, the same as hicli and pkg/ffi send
// them. Posting the structured error instead made the frontend's generic
// handling show "[object Object]" and hide the real message.
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

// recoverCommandPanic keeps a panic in one command from taking the whole
// client down. The server build recovers per command and only drops that
// connection; here there is no connection to drop, and an unrecovered panic
// exits the Go runtime and forces the user to reload.
func recoverCommandPanic(action string, reqID int64) {
	err := recover()
	if err == nil {
		return
	}
	logEvt := gmx.Log.Error().
		Bytes(zerolog.ErrorStackFieldName, debug.Stack()).
		Str("action", action)
	if realErr, ok := err.(error); ok {
		logEvt = logEvt.Err(realErr)
	} else {
		logEvt = logEvt.Any(zerolog.ErrorFieldName, err)
	}
	logEvt.Msg("Panic while handling command")
	postMessage(jsoncmd.RespError, reqID, fmt.Sprintf("panic while handling %s: %v", action, err))
}

func jsMessageListener(_ js.Value, args []js.Value) any {
	data := args[0].Get("data")
	wrappedCmd := &hicli.JSONCommand{
		Command:   jsoncmd.Name(data.Get("command").String()),
		RequestID: int64(data.Get("request_id").Int()),
		Data:      exstrings.UnsafeBytes(data.Get("data").String()),
	}
	if wrappedCmd.Command == "wasm-upload" {
		// The parameters are the same ones the server build takes as query
		// parameters, so the upload dialog's options work the same way.
		var uploadParams struct {
			jsoncmd.UploadMediaParams
			uploadExtras
		}
		if err := json.Unmarshal(wrappedCmd.Data, &uploadParams); err != nil {
			postMessage(jsoncmd.RespError, wrappedCmd.RequestID,
				fmt.Sprintf("failed to parse upload parameters: %v", err))
			return nil
		}
		payloadVal := data.Get("payload")
		payload := make([]byte, payloadVal.Length())
		js.CopyBytesToGo(payload, payloadVal)
		var thumbnail []byte
		if thumbVal := data.Get("thumbnail"); thumbVal.Type() == js.TypeObject && thumbVal.Length() > 0 {
			thumbnail = make([]byte, thumbVal.Length())
			js.CopyBytesToGo(thumbnail, thumbVal)
		}
		go func() {
			defer recoverCommandPanic("upload", wrappedCmd.RequestID)
			ctx := gmx.Log.With().Str("action", "wasmuks upload").Logger().WithContext(context.Background())
			resp, err := uploadMedia(
				ctx, uploadParams.UploadMediaParams, uploadParams.uploadExtras, payload, thumbnail,
			)
			if err != nil {
				postMessage(jsoncmd.RespError, wrappedCmd.RequestID, gomuks.ToRespError(err).Error())
			} else {
				postMessage(jsoncmd.RespSuccess, wrappedCmd.RequestID, resp)
			}
		}()
		return nil
	}
	// Commands the shared client doesn't implement because the server build
	// answers them over HTTP instead. pkg/ffi dispatches the same set for the
	// mobile apps; without this they are rejected as unknown and the frontend
	// falls back to an HTTP endpoint that doesn't exist here.
	if handler, ok := serverCommands[wrappedCmd.Command]; ok {
		go func() {
			defer recoverCommandPanic(string(wrappedCmd.Command), wrappedCmd.RequestID)
			ctx := gmx.Log.With().Stringer("action", wrappedCmd.Command).Logger().WithContext(context.Background())
			resp, err := handler(ctx, wrappedCmd.Data)
			if err != nil {
				postMessage(jsoncmd.RespError, wrappedCmd.RequestID, gomuks.ToRespError(err).Error())
			} else {
				postMessage(jsoncmd.RespSuccess, wrappedCmd.RequestID, resp)
			}
		}()
		return nil
	}
	if wrappedCmd.Command == jsoncmd.ReqRestoreKeyBackup {
		// The native server streams this over an HTTP endpoint; in wasm the
		// progress goes out as events and the final state as the response.
		go func() {
			defer recoverCommandPanic("restore key backup", wrappedCmd.RequestID)
			ctx := gmx.Log.With().Str("action", "restore key backup").Logger().WithContext(context.Background())
			resp, err := jsoncmd.RestoreKeyBackup.RunCtx(ctx, wrappedCmd.Data, restoreKeyBackup(wrappedCmd.RequestID))
			if err != nil {
				postMessage(jsoncmd.RespError, wrappedCmd.RequestID, gomuks.ToRespError(err).Error())
			} else {
				postMessage(jsoncmd.RespSuccess, wrappedCmd.RequestID, resp)
			}
		}()
		return nil
	}
	go func() {
		defer recoverCommandPanic(string(wrappedCmd.Command), wrappedCmd.RequestID)
		resp := gmx.Client.SubmitJSONCommand(context.Background(), wrappedCmd)
		postMessage(resp.Command, resp.RequestID, resp.Data)
	}()
	return nil
}

var serverCommands = map[jsoncmd.Name]func(context.Context, json.RawMessage) (any, error){
	jsoncmd.ReqGetURLPreview: func(ctx context.Context, data json.RawMessage) (any, error) {
		return jsoncmd.GetURLPreview.RunCtx(ctx, data, func(
			ctx context.Context, params *jsoncmd.GetURLPreviewParams,
		) (*event.BeeperLinkPreview, error) {
			return gmx.GetURLPreview(ctx, params.URL, params.Encrypt)
		})
	},
	jsoncmd.ReqExportKeys: func(ctx context.Context, data json.RawMessage) (any, error) {
		return jsoncmd.ExportKeys.RunCtx(ctx, data, func(
			ctx context.Context, params *jsoncmd.ExportKeysParams,
		) (string, error) {
			var sessions dbutil.RowIter[*crypto.InboundGroupSession]
			if params.RoomID == "" {
				sessions = gmx.Client.CryptoStore.GetAllGroupSessions(ctx)
			} else {
				sessions = gmx.Client.CryptoStore.GetGroupSessionsForRoom(ctx, params.RoomID)
			}
			export, err := crypto.ExportKeysIter(params.Passphrase, sessions)
			return string(export), err
		})
	},
	jsoncmd.ReqImportKeys: func(ctx context.Context, data json.RawMessage) (any, error) {
		return jsoncmd.ImportKeys.RunCtx(ctx, data, func(
			ctx context.Context, params *jsoncmd.ImportKeysParams,
		) (*jsoncmd.ImportKeysResponse, error) {
			imported, total, err := gmx.Client.Crypto.ImportKeys(ctx, params.Passphrase, []byte(params.Export))
			if err != nil {
				return nil, err
			}
			return &jsoncmd.ImportKeysResponse{Imported: imported, Total: total}, nil
		})
	},
}

func restoreKeyBackup(reqID int64) func(context.Context, *jsoncmd.RestoreKeyBackupParams) (*jsoncmd.KeyBackupRestoreProgress, error) {
	return func(ctx context.Context, params *jsoncmd.RestoreKeyBackupParams) (*jsoncmd.KeyBackupRestoreProgress, error) {
		var last jsoncmd.KeyBackupRestoreProgress
		err := gmx.Client.RestoreKeyBackup(ctx, params.RoomID, func(progress jsoncmd.KeyBackupRestoreProgress) {
			last = progress
			// Tagged with the request ID so the frontend can tell restores apart.
			postMessage(jsoncmd.EventKeyBackupRestoreProgress, reqID, &progress)
		})
		if err != nil {
			return nil, err
		}
		last.Stage = "done"
		return &last, nil
	}
}

// runMigrations runs schema migrations on a throwaway multi-connection pool
// with normal locking. dbutil's sqlite-fkey-off upgrade path never releases
// the connection it acquires, which with the real single-connection EXCLUSIVE
// pool would leave the only connection (and the file lock) stuck forever.
// Leaked connections on this pool are idle and hold no locks, so they are
// harmless once the pool is closed.
func runMigrations() error {
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
	err = hicli.UpgradeDatabases(log.WithContext(context.Background()), rawDB, nil, log, gmx.PickleKey())
	if closeErr := rawDB.Close(); closeErr != nil {
		log.Warn().Err(closeErr).Msg("Failed to close migration pool")
	}
	if err != nil {
		return err
	}
	// Only now open the real (single-connection, EXCLUSIVE) pool.
	return gmx.InitClient()
}

// wasmuksInit mirrors WasmuksInit in web/src/api/wasmclient.ts. It's passed
// as the worker's name so it's available before Go starts, without a
// message-ordering race.
type wasmuksInit struct {
	LastServerTS int64 `json:"last_server_ts"`
	// From the "wasm" section of config.json, see docs/wasmuks.md.
	MemoryLimitMB int    `json:"memory_limit_mb,omitempty"`
	GCBallastMB   *int   `json:"gc_ballast_mb,omitempty"`
	LogLevel      string `json:"log_level,omitempty"`
}

const (
	defaultMemoryLimitMB = 512
	defaultGCBallastMB   = 64
)

// gcBallast keeps the GC's heap target up. The live heap in wasm is tiny
// between syncs (a few MB), so with the default GOGC every few MB of
// allocation triggered a full stop-the-world collection on the single
// thread: scrolling history caused ~140 collections per GB allocated. The
// ballast is never written, so it costs address space rather than memory.
var gcBallast []byte

var initParams wasmuksInit

var processStart = time.Now()

// logMemStats periodically logs Go heap statistics so memory use is visible
// in the browser console without a profiler.
func logMemStats() {
	var stats runtime.MemStats
	for {
		time.Sleep(30 * time.Second)
		runtime.ReadMemStats(&stats)
		gmx.Log.Info().
			Uint64("heap_alloc_mb", stats.HeapAlloc>>20).
			Uint64("heap_sys_mb", stats.HeapSys>>20).
			Uint64("total_alloc_mb", stats.TotalAlloc>>20).
			Uint32("num_gc", stats.NumGC).
			Msg("Memory stats")
	}
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
		// The same defaults the server build uses. Without the presence one
		// the client sends no set_presence at all, and the homeserver then
		// tells everyone you are online whenever the tab is open.
		Matrix: gomuks.MatrixConfig{
			SetPresence: ptr.Ptr(event.PresenceOffline),
		},
		Media: gomuks.MediaConfig{
			ThumbnailSize: 120,
		},
	}
	initParams = readInit()
	if initParams.LogLevel != "" {
		if level, err := zerolog.ParseLevel(initParams.LogLevel); err == nil {
			gmx.Config.Logging.MinLevel = ptr.Ptr(level)
		}
	}
	// One connection with the driver's defaults, EXCLUSIVE locking and a
	// PERSIST journal (see pkg/sqlite-wasm-js/conn.go). Holding the file lock
	// for the session is what brings point lookups on OPFS down to in-memory
	// speed, measured in pkg/sqlite-wasm-js/bench; a multi-connection pool
	// with normal locking was an order of magnitude slower on them.
	gmx.GetDBConfig = func() dbutil.PoolConfig {
		return dbutil.PoolConfig{
			Type:         "sqlite-wasm-js",
			URI:          "file:/gomuks.db?_txlock=immediate",
			MaxOpenConns: 1,
			MaxIdleConns: 1,
		}
	}
	// Go's GC otherwise lets the heap grow to twice the live size, which
	// on top of V8's compiled code for a 30 MB module is too much for small
	// client machines. A soft limit makes the GC work harder near it.
	memoryLimitMB := cmp.Or(initParams.MemoryLimitMB, defaultMemoryLimitMB)
	debug.SetMemoryLimit(int64(memoryLimitMB) << 20)
	ballastMB := defaultGCBallastMB
	if initParams.GCBallastMB != nil && *initParams.GCBallastMB >= 0 {
		ballastMB = *initParams.GCBallastMB
	}
	if ballastMB > 0 {
		gcBallast = make([]byte, ballastMB<<20)
	}
	go logMemStats()
	// A pool with one connection turns any "query outside the transaction
	// while inside DoTxn" bug into a hang, so make dbutil panic instead.
	dbutil.ForceDeadlockDetection = true

	gmx.RemoveDataFunc = removeData
	gmx.UploadMediaFunc = uploadMediaFromReader
	gmx.EventBuffer = gomuks.NewEventBuffer(0)
	gmx.EventBuffer.Subscribe(0, nil, func(evt *gomuks.BufferedEvent) {
		if !holdEvent(evt) {
			postMessage(evt.Command, evt.RequestID, evt.Data)
		}
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
	// The worker generates a random pickle key per installation and stores it
	// in IndexedDB (see wasmuks.ts); it's exposed as a global before Go starts.
	if key := js.Global().Get("wasmuksPickleKey"); key.Type() == js.TypeObject && key.Length() > 0 {
		gmx.PickleKeyOverride = make([]byte, key.Length())
		js.CopyBytesToGo(gmx.PickleKeyOverride, key)
	} else {
		gmx.Log.Warn().Msg("No pickle key provided by worker, using default")
	}
	if err := runMigrations(); err != nil {
		gmx.Log.WithLevel(zerolog.FatalLevel).Err(err).Msg("Failed to run database migrations")
		postMessage("wasm-connection", 0, json.RawMessage(`{"connected":false,"reconnecting":false,"error":"Database migration failed"}`))
		return
	}
	gmx.Client.SingleConnectionDB = true
	gmx.Log.Info().
		Int("memory_limit_mb", memoryLimitMB).
		Int("gc_ballast_mb", ballastMB).
		Msg("wasm configuration")
	gmx.StartClient()
	if stats := gmx.Client.DB.RawDB.Stats(); stats.InUse > 0 {
		gmx.Log.Error().Int("in_use", stats.InUse).Msg("Database connections still in use after startup, expect hangs")
	}
	gmx.Log.Info().Msg("Initialization complete")
	postMessage(jsoncmd.EventClientState, 0, gmx.Client.State())
	postMessage(jsoncmd.EventSyncStatus, 0, gmx.Client.SyncStatus.Load())
	if gmx.Client.IsLoggedInAndVerified() {
		ctx := gmx.Log.WithContext(context.Background())
		// If the frontend restored its room list from IndexedDB, only send
		// what changed since then (same as the websocket path does).
		gmx.Log.Info().Int64("catchup_since", initParams.LastServerTS).Msg("Sending initial sync")
		initStart := time.Now()
		var roomCount int
		startHoldingEvents()
		for payload := range gmx.Client.GetInitialSync(ctx, 100, initParams.LastServerTS) {
			roomCount += len(payload.Rooms)
			postMessage(jsoncmd.EventSyncComplete, 0, payload)
		}
		heldCount := releaseHeldEvents()
		postMessage(jsoncmd.EventInitComplete, 0, gmx.Client.SyncStatus.Load())
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		gmx.Log.Info().
			Dur("duration", time.Since(initStart)).
			Dur("since_start", time.Since(processStart)).
			Int("rooms", roomCount).
			Int("held_events", heldCount).
			Uint64("heap_alloc_mb", stats.HeapAlloc>>20).
			Uint64("heap_sys_mb", stats.HeapSys>>20).
			Msg("Initial room list sent")
	}

	select {}
}

// The sync loop is already running while the initial room list is read from
// the database and posted to the frontend. A page can be read before a sync
// commits but posted after that sync's event, in which case the stale snapshot
// would overwrite the fresher data in the frontend (and, through the IndexedDB
// cache and its server timestamp, stay stale across reloads). Events that
// carry room metadata are therefore held back until the whole initial list is
// out, then replayed in order. A replayed event may be older than the page it
// touches, but every later change to that room also produced a held event
// that is replayed after it, so the frontend ends up with the newest state.
// Other events (send status, typing, client state) don't touch room metadata
// and pass through, so sending a message during startup still gets its
// confirmation right away.
var (
	heldEventsLock sync.Mutex
	holdingEvents  bool
	heldEvents     []*gomuks.BufferedEvent
)

func holdEvent(evt *gomuks.BufferedEvent) bool {
	switch evt.Data.(type) {
	case *jsoncmd.SyncComplete, *jsoncmd.EventsDecrypted:
	default:
		return false
	}
	heldEventsLock.Lock()
	defer heldEventsLock.Unlock()
	if !holdingEvents {
		return false
	}
	heldEvents = append(heldEvents, evt)
	return true
}

func startHoldingEvents() {
	heldEventsLock.Lock()
	holdingEvents = true
	heldEventsLock.Unlock()
}

// releaseHeldEvents posts everything that was held, in order, and then stops
// holding. The replay happens under the lock so an event arriving meanwhile
// waits in holdEvent and is posted directly afterwards, never in between.
func releaseHeldEvents() int {
	heldEventsLock.Lock()
	defer heldEventsLock.Unlock()
	for _, evt := range heldEvents {
		postMessage(evt.Command, evt.RequestID, evt.Data)
	}
	count := len(heldEvents)
	heldEvents = nil
	holdingEvents = false
	return count
}
