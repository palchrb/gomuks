//go:build js

// Microbenchmark for pkg/sqlite-wasm-js, using a table shaped like gomuks'
// `event` table (22 columns). Variants:
//
//	current                   - the driver via database/sql with the pre-EXCLUSIVE pragmas, one prepare per call
//	reuse                     - same, but with a prepared statement reused for all rows
//	current+exclusive+persist - the driver with its default pragmas (EXCLUSIVE locking, PERSIST journal)
//
// Results are posted to JS as a JSON object on globalThis.benchResult.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"syscall/js"
	"time"

	_ "go.mau.fi/gomuks/pkg/sqlite-wasm-js"
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

func benchDriver(mode string, n int, reuse bool, extraPragmas string) (result, error) {
	res := result{}
	// The driver defaults to EXCLUSIVE+PERSIST on OPFS; "current" pins the
	// pre-change modes so the baseline stays comparable over time.
	uri := fmt.Sprintf("file:/bench-%s-%v-%d.db?_txlock=immediate&connection_mode=%s", mode, reuse, len(extraPragmas), mode)
	if extraPragmas == "" {
		uri += "&_locking_mode=NORMAL&_journal_mode=DELETE"
	}
	db, err := sql.Open("sqlite-wasm-js", uri)
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
	_ = db.QueryRow("PRAGMA journal_mode").Scan(&jm)
	_ = db.QueryRow("PRAGMA locking_mode").Scan(&lm)
	res["db_info"] = fmt.Sprintf(`{"journal_mode":%q,"locking_mode":%q}`, jm, lm)
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

func main() {
	n := js.Global().Get("benchN").Int()
	reps := 3
	all := result{"n": n, "reps": reps, "crossing": benchCrossing()}
	for rep := 0; rep < reps; rep++ {
		for _, mode := range []string{"memory", "opfs-sahpool"} {
			variants := []string{"current", "reuse", "current+exclusive+persist"}
			if v := js.Global().Get("benchVariants"); v.Type() == js.TypeString && v.String() != "" {
				variants = strings.Split(v.String(), ",")
			}
			for _, variant := range variants {
				var r result
				var err error
				switch variant {
				case "current":
					r, err = benchDriver(mode, n, false, "")
				case "reuse":
					r, err = benchDriver(mode, n, true, "")
				case "current+exclusive+persist":
					if mode == "memory" {
						continue
					}
					// Driver defaults (EXCLUSIVE locking, PERSIST journal).
					r, err = benchDriver(mode, n, false, "PRAGMA foreign_keys = ON")
				}
				key := mode + "/" + variant
				if err != nil {
					all[key] = result{"error": err.Error()}
					clog("bench error", key, err.Error())
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
