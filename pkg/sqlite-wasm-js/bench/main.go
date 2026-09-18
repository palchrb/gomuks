//go:build js

// Microbenchmark for pkg/sqlite-wasm-js, using a table shaped like gomuks'
// `event` table (22 columns). Variants:
//
//	upstream                   - the driver as it stood in gomuks v26.09 (see upstream/):
//	                             one crossing into JS per value, no statement cache
//	upstream+reuse             - the same, with one prepared statement reused for every
//	                             row, which is the closest thing to giving it a cache
//	upstream+exclusive+persist - the same driver with EXCLUSIVE locking and a PERSIST journal
//	current                    - this driver (batched bridge, statement cache) with the old pragmas
//	reuse                      - same, but with a prepared statement reused for all rows
//	current+exclusive+persist  - this driver with its defaults: what gomuks ships
//	page4k .. page64k          - the shipping configuration with a different SQLite
//	                             page size; the sqlite-wasm build defaults to 8 KiB
//	nosync                     - the shipping configuration without flushing each write
//	journalmem                 - the shipping configuration with the journal in memory
//
// upstream vs current isolates the bridge; the two +exclusive+persist variants
// isolate the locking mode. Every variant also runs against an in-memory
// database, which is the floor when storage is taken out of the picture.
//
// Results are posted to JS as a JSON object on globalThis.benchResult.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	_ "go.mau.fi/gomuks/pkg/sqlite-wasm-js"
	_ "go.mau.fi/gomuks/pkg/sqlite-wasm-js/bench/upstream"
)

const (
	currentDriver  = "sqlite-wasm-js"
	upstreamDriver = "sqlite-wasm-js-upstream"
)

func clog(args ...any) { js.Global().Get("console").Call("log", args...) }

const createTable = `CREATE TABLE IF NOT EXISTS event (
	rowid             INTEGER PRIMARY KEY,
	room_id           TEXT    NOT NULL,
	event_id          TEXT    NOT NULL,
	sender            TEXT    NOT NULL,
	type              TEXT    NOT NULL,
	state_key         TEXT,
	timestamp         INTEGER NOT NULL,
	content           TEXT    NOT NULL,
	decrypted         TEXT,
	decrypted_type    TEXT,
	unsigned          TEXT    NOT NULL,
	local_content     TEXT,
	transaction_id    TEXT,
	redacted_by       TEXT,
	relates_to        TEXT,
	relation_type     TEXT,
	megolm_session_id TEXT,
	decryption_error  TEXT,
	send_error        TEXT,
	reactions         TEXT,
	last_edit_rowid   INTEGER,
	unread_type       INTEGER NOT NULL DEFAULT 0,
	sticky_duration   INTEGER,
	CONSTRAINT event_id_unique_key UNIQUE (event_id),
	CONSTRAINT transaction_id_unique_key UNIQUE (transaction_id)
) STRICT;
CREATE INDEX IF NOT EXISTS event_room_id_idx ON event (room_id);`

const insertQuery = `INSERT INTO event (
	room_id, event_id, sender, type, state_key, timestamp, content, decrypted, decrypted_type,
	unsigned, local_content, transaction_id, redacted_by, relates_to, relation_type,
	megolm_session_id, decryption_error, send_error, reactions, last_edit_rowid, unread_type, sticky_duration
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
RETURNING rowid`

const selectQuery = `SELECT rowid, room_id, event_id, sender, type, state_key, timestamp, content, decrypted, decrypted_type,
	unsigned, local_content, transaction_id, redacted_by, relates_to, relation_type,
	megolm_session_id, decryption_error, send_error, reactions, last_edit_rowid, unread_type, sticky_duration
FROM event WHERE room_id = $1 ORDER BY timestamp DESC LIMIT $2`

const updateQuery = `UPDATE event SET unread_type = $1 WHERE event_id = $2`

var selectOneQuery = strings.Replace(selectQuery, "WHERE room_id = $1 ORDER BY timestamp DESC LIMIT $2", "WHERE event_id = $1", 1)

// Realistic-ish payloads: a 400-byte encrypted content blob, 300-byte decrypted body, 120-byte unsigned.
var (
	sampleContent   = `{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"` + strings.Repeat("AwgAEpABv0K7", 30) + `","device_id":"ABCDEFGHIJ","sender_key":"` + strings.Repeat("x", 43) + `","session_id":"` + strings.Repeat("y", 43) + `"}`
	sampleDecrypted = `{"msgtype":"m.text","body":"` + strings.Repeat("hello family chat ", 12) + `","format":"org.matrix.custom.html","formatted_body":"<p>` + strings.Repeat("hello family chat ", 4) + `</p>"}`
	sampleUnsigned  = `{"age":1234,"transaction_id":null,"membership":"join","prev_content":null,"redacted_because":null}`
	roomID          = "!abcdefghijklmnopqr:example.org"
)

type eventRow struct {
	RowID           int64
	RoomID          string
	EventID         string
	Sender          string
	Type            string
	StateKey        *string
	Timestamp       int64
	Content         string
	Decrypted       *string
	DecryptedType   *string
	Unsigned        string
	LocalContent    *string
	TransactionID   *string
	RedactedBy      *string
	RelatesTo       *string
	RelationType    *string
	MegolmSessionID *string
	DecryptionError *string
	SendError       *string
	Reactions       *string
	LastEditRowID   *int64
	UnreadType      int64
	StickyDuration  *int64
}

func eventID(i int) string { return fmt.Sprintf("$%038d", i) }

func insertArgs(i int) []any {
	sess := strings.Repeat("s", 43)
	return []any{
		roomID, eventID(i), "@alice:example.org", "m.room.encrypted", nil, int64(1700000000000 + i*1000),
		sampleContent, sampleDecrypted, "m.room.message", sampleUnsigned, nil, nil, nil, nil, nil,
		sess, nil, nil, nil, nil, int64(0), nil,
	}
}

func scanRow(rows *sql.Rows) (*eventRow, error) {
	var r eventRow
	err := rows.Scan(&r.RowID, &r.RoomID, &r.EventID, &r.Sender, &r.Type, &r.StateKey, &r.Timestamp, &r.Content,
		&r.Decrypted, &r.DecryptedType, &r.Unsigned, &r.LocalContent, &r.TransactionID, &r.RedactedBy, &r.RelatesTo,
		&r.RelationType, &r.MegolmSessionID, &r.DecryptionError, &r.SendError, &r.Reactions, &r.LastEditRowID,
		&r.UnreadType, &r.StickyDuration)
	return &r, err
}

type result map[string]any

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

// ---------------------------------------------------------------------------
// Variant 1+2: the real driver through database/sql
// ---------------------------------------------------------------------------

func benchDriver(driverName, tag, mode string, n int, reuse bool, extraPragmas string) (result, error) {
	res := result{}
	// Each variant gets its own file so nothing is shared between them.
	uri := fmt.Sprintf("file:/bench-%s-%s.db?_txlock=immediate&connection_mode=%s", mode, tag, mode)
	if driverName == currentDriver && extraPragmas == "" {
		// This driver defaults to EXCLUSIVE+PERSIST on OPFS; pin the old modes
		// so the baseline stays comparable over time. The upstream driver does
		// not read these parameters and already defaults to them, and the
		// variants that want the new modes set them as pragmas below.
		uri += "&_locking_mode=NORMAL&_journal_mode=DELETE"
	}
	db, err := sql.Open(driverName, uri)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, p := range strings.Split(extraPragmas, ";") {
		if p == "" {
			continue
		}
		if _, err = db.Exec(p); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	var jm, lm string
	var pageSize, cacheSize int
	_ = db.QueryRow("PRAGMA journal_mode").Scan(&jm)
	_ = db.QueryRow("PRAGMA locking_mode").Scan(&lm)
	_ = db.QueryRow("PRAGMA page_size").Scan(&pageSize)
	_ = db.QueryRow("PRAGMA cache_size").Scan(&cacheSize)
	res["db_info"] = fmt.Sprintf(
		`{"journal_mode":%q,"locking_mode":%q,"page_size":%d,"cache_size":%d}`,
		jm, lm, pageSize, cacheSize)
	if _, err = db.Exec("DROP TABLE IF EXISTS event"); err != nil {
		return nil, err
	}
	if _, err = db.Exec(createTable); err != nil {
		return nil, err
	}

	// --- insert ---
	start := time.Now()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	var stmt *sql.Stmt
	if reuse {
		if stmt, err = tx.Prepare(insertQuery); err != nil {
			return nil, err
		}
	}
	for i := 0; i < n; i++ {
		var rowid int64
		if reuse {
			err = stmt.QueryRow(insertArgs(i)...).Scan(&rowid)
		} else {
			err = tx.QueryRow(insertQuery, insertArgs(i)...).Scan(&rowid)
		}
		if err != nil {
			return nil, fmt.Errorf("insert %d: %w", i, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	res["insert_ms"] = ms(time.Since(start))

	// --- select all rows of the room (timeline load) ---
	start = time.Now()
	rows, err := db.Query(selectQuery, roomID, n)
	if err != nil {
		return nil, err
	}
	var out []*eventRow
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	res["select_all_ms"] = ms(time.Since(start))
	res["select_all_rows"] = len(out)
	if len(out) != n {
		return nil, fmt.Errorf("expected %d rows, got %d", n, len(out))
	}
	// Rows come back newest first; verify a few decode exactly as inserted.
	for _, i := range []int{0, n / 2, n - 1} {
		r := out[n-1-i]
		if r.EventID != eventID(i) || r.Content != sampleContent || r.Decrypted == nil || *r.Decrypted != sampleDecrypted ||
			r.StateKey != nil || r.Timestamp != int64(1700000000000+i*1000) || r.UnreadType != 0 || r.StickyDuration != nil {
			return nil, fmt.Errorf("row %d decoded incorrectly: %+v", i, r)
		}
	}

	// --- point lookups ---
	start = time.Now()
	lookups := min(n, 500)
	var selStmt *sql.Stmt
	if reuse {
		if selStmt, err = db.Prepare(selectOneQuery); err != nil {
			return nil, err
		}
	}
	for i := 0; i < lookups; i++ {
		var rows *sql.Rows
		if reuse {
			rows, err = selStmt.Query(eventID(i))
		} else {
			rows, err = db.Query(selectOneQuery, eventID(i))
		}
		if err != nil {
			return nil, err
		}
		if !rows.Next() {
			return nil, fmt.Errorf("missing row %d", i)
		}
		if _, err = scanRow(rows); err != nil {
			return nil, err
		}
		rows.Close()
	}
	res["point_lookup_ms"] = ms(time.Since(start))
	res["point_lookups"] = lookups

	// --- small updates, each in its own transaction ---
	// This is what gomuks does constantly outside of syncs: mark something
	// read, bump a room, store a receipt. It is also the workload that pays
	// for the page size, because a rollback journal copies whole pages
	// whatever the size of the change.
	start = time.Now()
	updates := min(n, 500)
	for i := 0; i < updates; i++ {
		// The affected row count is not checked here: the vendored upstream
		// driver returns the last insert rowid for it (its stmt.go has the two
		// swapped), and it is a reference copy, not something to fix. The rows
		// are verified below instead.
		if _, err = db.Exec(updateQuery, i%4, eventID(i)); err != nil {
			return nil, fmt.Errorf("update %d: %w", i, err)
		}
	}
	res["update_ms"] = ms(time.Since(start))
	res["updates"] = updates
	for _, i := range []int{0, updates / 2, updates - 1} {
		var unreadType int
		if err = db.QueryRow(
			"SELECT unread_type FROM event WHERE event_id = $1", eventID(i),
		).Scan(&unreadType); err != nil {
			return nil, fmt.Errorf("verify update %d: %w", i, err)
		}
		if unreadType != i%4 {
			return nil, fmt.Errorf("update %d did not apply: unread_type is %d", i, unreadType)
		}
	}

	// --- the same updates, but in one transaction ---
	// The difference between this and the phase above is what the application
	// could save by collecting small writes instead of committing each one.
	start = time.Now()
	tx, err = db.Begin()
	if err != nil {
		return nil, err
	}
	for i := 0; i < updates; i++ {
		if _, err = tx.Exec(updateQuery, (i+1)%4, eventID(i)); err != nil {
			return nil, fmt.Errorf("batched update %d: %w", i, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	res["update_batched_ms"] = ms(time.Since(start))
	return res, nil
}

// raw bridge cost: how long does one trivial syscall/js call take?
func benchCrossing() result {
	capi := js.Global().Get("sqlite3").Get("capi")
	const iters = 200000
	start := time.Now()
	for i := 0; i < iters; i++ {
		capi.Call("sqlite3_libversion_number")
	}
	perCall := time.Since(start) / iters
	start = time.Now()
	for i := 0; i < iters; i++ {
		_ = capi.Call("sqlite3_libversion").String()
	}
	perStrCall := time.Since(start) / iters
	return result{
		"call_int_ns":    perCall.Nanoseconds(),
		"call_string_ns": perStrCall.Nanoseconds(),
	}
}

// runVariant runs one variant once. It returns nil, nil for combinations that
// don't apply: locking mode is meaningless without a file, so the exclusive
// variants only run against storage.
func runVariant(variant, mode string, n int) (result, error) {
	switch variant {
	case "upstream":
		return benchDriver(upstreamDriver, "up", mode, n, false, "")
	case "upstream+reuse":
		// The upstream driver has no statement cache, so reusing one prepared
		// statement in the caller is the closest thing to giving it one. It
		// separates the cache's share of upstream -> current from the bridge's.
		return benchDriver(upstreamDriver, "upr", mode, n, true, "")
	case "upstream+exclusive+persist":
		if mode == "memory" {
			return nil, nil
		}
		// The upstream driver ignores the URI parameters for these, so set
		// them the only way it understands.
		return benchDriver(upstreamDriver, "upx", mode, n, false,
			"PRAGMA locking_mode = EXCLUSIVE;PRAGMA journal_mode = PERSIST")
	case "current":
		return benchDriver(currentDriver, "cur", mode, n, false, "")
	case "reuse":
		return benchDriver(currentDriver, "reuse", mode, n, true, "")
	case "current+exclusive+persist":
		if mode == "memory" {
			return nil, nil
		}
		// Driver defaults (EXCLUSIVE locking, PERSIST journal).
		return benchDriver(currentDriver, "curx", mode, n, false, "PRAGMA foreign_keys = ON")
	case "nosync":
		if mode == "memory" {
			return nil, nil
		}
		// The shipping configuration without the flush at the end of each
		// write. Faster, and a tab killed mid-write can corrupt the database.
		return benchDriver(currentDriver, variant, mode, n, false,
			"PRAGMA synchronous = OFF;PRAGMA foreign_keys = ON")
	case "journalmem":
		if mode == "memory" {
			return nil, nil
		}
		// The shipping configuration with the rollback journal held in memory
		// instead of a file, so a small write touches only the database file.
		// Same risk: nothing on disk to roll back with.
		return benchDriver(currentDriver, variant, mode, n, false,
			"PRAGMA journal_mode = MEMORY;PRAGMA foreign_keys = ON")
	case "page4k", "page16k", "page32k", "page64k":
		if mode == "memory" {
			return nil, nil
		}
		// The sqlite-wasm build already defaults to 8 KiB pages, so these are
		// the shipping configuration with a different page size. The size is
		// fixed when the file is created, hence the VACUUM; on a fresh file it
		// costs nothing.
		size := strings.TrimSuffix(strings.TrimPrefix(variant, "page"), "k")
		kib, err := strconv.Atoi(size)
		if err != nil {
			return nil, fmt.Errorf("bad page size in variant %q", variant)
		}
		return benchDriver(currentDriver, variant, mode, n, false,
			fmt.Sprintf("PRAGMA page_size = %d;VACUUM;PRAGMA foreign_keys = ON", kib*1024))
	default:
		return nil, fmt.Errorf("unknown variant %q", variant)
	}
}

// warmupRows is small enough to be quick and large enough to get the bridge
// and the row decoding through V8's optimiser. Without it the variant that
// happens to run first carries the whole warm-up cost, in every repetition,
// so taking the minimum doesn't remove it: it looked like a real difference
// of about a third on the read.
const warmupRows = 200

var modes = []string{"memory", "opfs-sahpool"}

func main() {
	n := js.Global().Get("benchN").Int()
	reps := 3
	all := result{"n": n, "reps": reps}
	// The default is the three rows of the summary: where this started, where
	// it is now, and the floor. "current" also supplies the in-memory floor,
	// since locking mode means nothing without a file. The rest exist for
	// attributing the improvement and are asked for by name.
	variants := []string{"upstream", "current", "current+exclusive+persist"}
	if v := js.Global().Get("benchVariants"); v.Type() == js.TypeString && v.String() != "" {
		variants = strings.Split(v.String(), ",")
	}
	for _, mode := range modes {
		for _, variant := range variants {
			if _, err := runVariant(variant, mode, min(n, warmupRows)); err != nil {
				clog("warmup error", mode+"/"+variant, err.Error())
			}
		}
	}
	all["crossing"] = benchCrossing()
	for rep := 0; rep < reps; rep++ {
		for _, mode := range modes {
			for _, variant := range variants {
				r, err := runVariant(variant, mode, n)
				key := mode + "/" + variant
				if err != nil {
					all[key] = result{"error": err.Error()}
					clog("bench error", key, err.Error())
					continue
				} else if r == nil {
					continue
				}
				if prev, ok := all[key].(result); ok {
					for k, v := range r {
						if pf, ok := v.(float64); ok {
							if qf, ok := prev[k].(float64); ok && qf < pf {
								r[k] = qf
							}
						}
					}
				}
				all[key] = r
				clog("bench done", key)
			}
		}
	}
	encoded, _ := json.Marshal(all)
	js.Global().Set("benchResult", string(encoded))
	select {}
}
