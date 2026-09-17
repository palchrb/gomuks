// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build js

package sqlite_wasm_js

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

type Rows struct {
	ctx       context.Context
	closeStmt bool
	*Stmt

	// Current chunk of decoded rows. When it's exhausted and the statement
	// isn't done, the next chunk is fetched with batchedStep.
	dec     *resultDecoder
	scratch []any
}

var (
	_ driver.Rows                           = &Rows{}
	_ driver.RowsColumnTypeScanType         = &Rows{}
	_ driver.RowsColumnTypeDatabaseTypeName = &Rows{}
)

func (r *Rows) Columns() []string {
	return r.cols
}

func (r *Rows) Close() error {
	if r.closeStmt {
		return r.Stmt.Close()
	}
	return r.reset(r.ctx)
}

func (r *Rows) Next(dest []driver.Value) (retErr error) {
	defer catchIntoError(&retErr)
	if len(dest) != len(r.cols) {
		return fmt.Errorf("can't scan %d columns into %d values", len(r.cols), len(dest))
	}
	for !r.dec.hasRows() {
		if r.dec.done {
			return io.EOF
		}
		dec, err := r.c.batchedStep(r.cptr)
		if err != nil {
			return err
		}
		r.dec = dec
	}
	if r.scratch == nil {
		r.scratch = make([]any, len(dest))
	}
	if err := r.dec.row(r.scratch); err != nil {
		return err
	}
	for i, val := range r.scratch {
		if r.decls[i] == "timestamp" {
			destStr, _ := val.(string)
			if destStr == "" {
				dest[i] = time.Time{}
				continue
			}
			ts, err := time.ParseInLocation(sqliteTimeFormat, destStr, time.UTC)
			if err != nil {
				return fmt.Errorf("failed to parse timestamp %v: %w", val, err)
			}
			dest[i] = ts
			continue
		}
		dest[i] = val
	}
	return nil
}

func (r *Rows) ColumnTypeScanType(index int) reflect.Type {
	if r.decls[index] == "timestamp" {
		return reflect.TypeOf(time.Time{})
	}
	if r.dec.rowsRead > 0 {
		switch r.dec.lastTags[index] {
		case tagInt:
			return reflect.TypeOf(int64(0))
		case tagFloat:
			return reflect.TypeOf(float64(0))
		case tagText:
			return reflect.TypeOf("")
		case tagBlob:
			return reflect.TypeOf([]byte{})
		}
	}
	switch r.decls[index] {
	case "integer", "int", "bigint":
		return reflect.TypeOf(int64(0))
	case "real", "float", "double":
		return reflect.TypeOf(float64(0))
	case "text":
		return reflect.TypeOf("")
	case "blob":
		return reflect.TypeOf([]byte{})
	}
	return nil
}

func (r *Rows) ColumnTypeDatabaseTypeName(index int) string {
	return strings.ToUpper(r.decls[index])
}
