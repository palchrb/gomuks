// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build js

package sqlite_wasm_js

import (
	"container/list"
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall/js"

	"go.mau.fi/util/exerrors"
)

var noContextFunc = context.Background()

type Conn struct {
	d    *Driver
	ptr  js.Value
	cptr js.Value

	closed atomic.Bool

	txlock      string
	sahpool     bool
	lockingMode string
	journalMode string

	// Buffers for the batched bridge. A driver.Conn is only used by one
	// goroutine at a time and syscall/js calls are synchronous, so the
	// parameter buffers can be reused; result buffers are per call because
	// a Rows may still be reading one when the next statement runs.
	params     paramEncoder
	scratchJS  js.Value
	scratchLen int

	// Prepared statement cache, keyed by SQL. database/sql prepares and
	// finalizes a statement for every Query/Exec call that goes through
	// QueryerContext/ExecerContext, and sqlite3_prepare_v2 on a few hundred
	// bytes of SQL costs more than the query itself for point lookups.
	// Statements in the cache are idle; handing one out removes it, and
	// Stmt.Close puts it back (or finalizes it if the cache is full).
	stmtCache    map[string]*list.Element
	stmtCacheLRU *list.List
}

// stmtCacheSize is the number of idle prepared statements kept per connection.
const stmtCacheSize = 64

type cachedStmt struct {
	query string
	stmt  *Stmt
}

// chunkLimitBytes is the approximate size at which the JS side stops
// stepping and returns a partial result; Rows.Next fetches the next chunk.
const chunkLimitBytes = 1 << 20

// Defaults for the OPFS SAHPool VFS. The pool has no shared memory, so
// journal_mode=WAL silently falls back to a rollback journal. In rollback
// mode with normal locking, every read transaction has to probe for a hot
// journal and re-read the change counter from OPFS, which makes point lookups
// an order of magnitude slower than in memory. EXCLUSIVE locking keeps the
// file lock across transactions (it must be set before journal_mode), and
// PERSIST keeps the journal file around instead of creating and deleting it
// on every transaction. Both require that only one connection uses the file.
const (
	defaultLockingMode = "EXCLUSIVE"
	defaultJournalMode = "PERSIST"
)

var (
	_ driver.Conn               = &Conn{}
	_ driver.ConnPrepareContext = &Conn{}
	_ driver.ConnBeginTx        = &Conn{}
	_ driver.Execer             = &Conn{}
	_ driver.ExecerContext      = &Conn{}
	_ driver.Queryer            = &Conn{}
	_ driver.QueryerContext     = &Conn{}
	//_ driver.NamedValueChecker = &Conn{}
	_ driver.Validator = &Conn{}
	_ driver.Pinger    = &Conn{}
)

func (c *Conn) IsValid() bool {
	return !c.closed.Load()
}

func (c *Conn) Ping(ctx context.Context) error {
	return nil
}

func (c *Conn) Close() error {
	c.closed.Store(true)
	c.flushStmtCache()
	rc := c.d.CAPI.Call("sqlite3_close_v2", c.cptr).Int()
	if rc != SQLITE_OK {
		return c.d.MakeError(c, "sqlite3_close_v2", rc)
	}
	return nil
}

func (c *Conn) connectHook(ctx context.Context) (dc driver.Conn, err error) {
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	_, err = c.ExecContext(ctx, "PRAGMA foreign_keys = ON", nil)
	if err != nil {
		return
	}
	if c.sahpool {
		_, err = c.ExecContext(ctx, "PRAGMA locking_mode = "+c.lockingMode, nil)
		if err != nil {
			return
		}
		_, err = c.ExecContext(ctx, "PRAGMA journal_mode = "+c.journalMode, nil)
		if err != nil {
			return
		}
		_, err = c.ExecContext(ctx, "PRAGMA synchronous = NORMAL", nil)
		if err != nil {
			return
		}
		// SQLite silently keeps the previous mode if the VFS can't support the
		// requested one (e.g. WAL without shared memory), so verify.
		var effective string
		effective, err = c.queryString(ctx, "PRAGMA journal_mode")
		if err != nil {
			return
		} else if !strings.EqualFold(effective, c.journalMode) {
			err = fmt.Errorf("requested journal_mode %s but got %s", c.journalMode, effective)
			return
		}
		effective, err = c.queryString(ctx, "PRAGMA locking_mode")
		if err != nil {
			return
		} else if !strings.EqualFold(effective, c.lockingMode) {
			err = fmt.Errorf("requested locking_mode %s but got %s", c.lockingMode, effective)
			return
		}
	}
	_, err = c.ExecContext(ctx, "PRAGMA busy_timeout = 10000", nil)
	if err != nil {
		return
	}
	return c, nil
}

// batchedQuery binds the parameters currently in c.params to stmt, steps it
// (up to the chunk limit when keepRows is set, otherwise to completion) and
// returns the decoded result.
func (c *Conn) batchedQuery(stmt js.Value, wantColumns, keepRows bool) (*resultDecoder, error) {
	n := len(c.params.buf)
	if n > c.scratchLen {
		c.scratchLen = max(n*2, 4096)
		c.scratchJS = js.Global().Get("Uint8Array").New(c.scratchLen)
	}
	js.CopyBytesToJS(c.scratchJS, c.params.buf)
	out := c.d.Meow.Call("batchedQuery", stmt, c.scratchJS, n, wantColumns, keepRows, chunkLimitBytes)
	return c.decodeResult(out)
}

// batchedStep continues stepping a statement whose previous chunk was exhausted.
func (c *Conn) batchedStep(stmt js.Value) (*resultDecoder, error) {
	return c.decodeResult(c.d.Meow.Call("batchedStep", stmt, true, chunkLimitBytes))
}

func (c *Conn) decodeResult(out js.Value) (*resultDecoder, error) {
	buf := make([]byte, out.Length())
	if n := js.CopyBytesToGo(buf, out); n != len(buf) {
		return nil, fmt.Errorf("copied %d of %d result bytes", n, len(buf))
	}
	dec, err := newResultDecoder(buf)
	if err != nil {
		return nil, err
	}
	if dec.rc != SQLITE_OK {
		funcName := "sqlite3_step"
		if dec.phase == phaseBind {
			funcName = "sqlite3_bind"
		}
		return nil, c.d.MakeError(c, funcName, dec.rc)
	}
	return dec, nil
}

// queryString runs a query and returns the first column of the first row as a string.
func (c *Conn) queryString(ctx context.Context, query string) (string, error) {
	rows, err := c.QueryContext(ctx, query, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = rows.Close()
	}()
	dest := make([]driver.Value, len(rows.Columns()))
	if err = rows.Next(dest); err != nil {
		return "", err
	}
	str, _ := dest[0].(string)
	return str, nil
}

//func (c *Conn) CheckNamedValue(value *driver.NamedValue) error {
//	return nil
//}

// isDDL reports whether the query may change the schema, which would make
// the column metadata cached on prepared statements stale.
func isDDL(query string) bool {
	query = strings.TrimSpace(query)
	for _, prefix := range []string{"CREATE", "ALTER", "DROP"} {
		if len(query) >= len(prefix) && strings.EqualFold(query[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func (c *Conn) flushStmtCache() {
	for _, elem := range c.stmtCache {
		_ = elem.Value.(*cachedStmt).stmt.finalize()
	}
	c.stmtCache = nil
	c.stmtCacheLRU = nil
}

func (c *Conn) PrepareContext(ctx context.Context, query string) (stmt driver.Stmt, retErr error) {
	defer catchIntoError(&retErr)
	if isDDL(query) {
		c.flushStmtCache()
	}
	if elem, ok := c.stmtCache[query]; ok {
		delete(c.stmtCache, query)
		c.stmtCacheLRU.Remove(elem)
		return elem.Value.(*cachedStmt).stmt, nil
	}
	res := c.d.Meow.Call("prepare", c.cptr, query)
	rc := res.Get("rc")
	ptr := res.Get("ptr")
	if !rc.IsUndefined() {
		return nil, c.d.MakeError(c, "sqlite3_prepare_v2", rc.Int())
	} else if ptr.IsUndefined() {
		return nil, fmt.Errorf("sqlite3_prepare_v2 returned no error and no statement")
	} else {
		return &Stmt{d: c.d, c: c, cptr: ptr, query: query}, nil
	}
}

// releaseStmt returns a statement to the cache instead of finalizing it.
// The statement must already be reset.
func (c *Conn) releaseStmt(s *Stmt) error {
	if c.closed.Load() {
		return s.finalize()
	}
	if c.stmtCache == nil {
		c.stmtCache = make(map[string]*list.Element, stmtCacheSize)
		c.stmtCacheLRU = list.New()
	}
	if _, exists := c.stmtCache[s.query]; exists {
		// Two statements with the same SQL were in use at once; keep one.
		return s.finalize()
	}
	c.stmtCache[s.query] = c.stmtCacheLRU.PushFront(&cachedStmt{query: s.query, stmt: s})
	if c.stmtCacheLRU.Len() > stmtCacheSize {
		oldest := c.stmtCacheLRU.Back()
		c.stmtCacheLRU.Remove(oldest)
		entry := oldest.Value.(*cachedStmt)
		delete(c.stmtCache, entry.query)
		return entry.stmt.finalize()
	}
	return nil
}

func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) == 0 {
		if isDDL(query) {
			c.flushStmtCache()
		}
		rc := c.d.CAPI.Call("sqlite3_exec", c.cptr, query, 0, 0, 0).Int()
		if rc != SQLITE_OK {
			return nil, c.d.MakeError(c, "sqlite3_exec", rc)
		}
		return &Result{
			lastInsertID: c.lastInsertRowID(),
			rowsAffected: c.rowsAffected(),
		}, nil
	}
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	res, err := stmt.(*Stmt).ExecContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return res, stmt.Close()
}

func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	res, err := stmt.(*Stmt).QueryContext(ctx, args)
	if err != nil {
		_ = stmt.Close()
		return nil, err
	}
	res.(*Rows).closeStmt = true
	return res, nil
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	rc := c.d.CAPI.Call("sqlite3_exec", c.cptr, "BEGIN "+c.txlock, 0, 0, 0).Int()
	if rc != SQLITE_OK {
		return nil, c.d.MakeError(c, "sqlite3_exec", rc)
	}
	return &Tx{c}, nil
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(noContextFunc, query)
}

func valuesToNamedValues(args []driver.Value) []driver.NamedValue {
	values := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		values[i] = driver.NamedValue{
			Ordinal: i + 1,
			Value:   arg,
		}
	}
	return values
}

func parseStrOrNumber(val js.Value) (int64, error) {
	switch val.Type() {
	case js.TypeNumber:
		return int64(val.Int()), nil
	case js.TypeString:
		return strconv.ParseInt(val.String(), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected JS type %s for integer", val.Type().String())
	}
}

func (c *Conn) lastInsertRowID() int64 {
	return exerrors.Must(parseStrOrNumber(c.d.Meow.Call("last_insert_rowid", c.cptr)))
}

func (c *Conn) rowsAffected() int64 {
	// TODO this could use sqlite3_changes64 instead to get a bigint
	return int64(c.d.CAPI.Call("sqlite3_changes", c.cptr).Int())
}

func (c *Conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	return c.ExecContext(noContextFunc, query, valuesToNamedValues(args))
}

func (c *Conn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return c.QueryContext(noContextFunc, query, valuesToNamedValues(args))
}

func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(noContextFunc, driver.TxOptions{})
}
