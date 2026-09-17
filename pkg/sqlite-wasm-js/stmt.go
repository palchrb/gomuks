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
	"reflect"
	"strings"
	"syscall/js"
	"time"
	"unsafe"
)

type Stmt struct {
	d *Driver
	c *Conn

	cptr js.Value

	numInput int

	// Column names and declared types, fetched with the first query result
	// and cached for the lifetime of the statement.
	colsLoaded bool
	cols       []string
	decls      []string
}

var (
	_ driver.Stmt             = &Stmt{}
	_ driver.StmtExecContext  = &Stmt{}
	_ driver.StmtQueryContext = &Stmt{}
)

func (s *Stmt) Close() error {
	rc := s.d.CAPI.Call("sqlite3_finalize", s.cptr).Int()
	if rc != 0 {
		return s.d.MakeError(s.c, "sqlite3_finalize", rc)
	}
	return nil
}

func (s *Stmt) NumInput() int {
	if s.numInput == 0 {
		s.numInput = s.d.CAPI.Call("sqlite3_bind_parameter_count", s.cptr).Int() + 1
	}
	return s.numInput - 1
}

const sqliteTimeFormat = "2006-01-02 15:04:05.999999999-07:00"

// encodeValue appends one parameter to the connection's parameter buffer.
func (s *Stmt) encodeValue(index int, val any) error {
	enc := &s.c.params
	switch typedVal := val.(type) {
	case nil:
		enc.null(index)
	case string:
		enc.text(index, typedVal)
	case []byte:
		enc.blob(index, typedVal)
	case float32:
		enc.float64(index, float64(typedVal))
	case float64:
		enc.float64(index, typedVal)
	case bool:
		if typedVal {
			enc.int64(index, 1)
		} else {
			enc.int64(index, 0)
		}
	case int64:
		enc.int64(index, typedVal)
	case uint64:
		// Values above MaxInt64 wrap around, matching what SQLite would store anyway.
		enc.int64(index, int64(typedVal))
	case time.Time:
		enc.text(index, typedVal.UTC().Format(sqliteTimeFormat))
	case *time.Time:
		if typedVal == nil {
			enc.null(index)
		} else {
			enc.text(index, typedVal.UTC().Format(sqliteTimeFormat))
		}
	case int:
		enc.int64(index, int64(typedVal))
	case int8:
		enc.int64(index, int64(typedVal))
	case int16:
		enc.int64(index, int64(typedVal))
	case int32:
		enc.int64(index, int64(typedVal))
	case uint:
		enc.int64(index, int64(typedVal))
	case uint8:
		enc.int64(index, int64(typedVal))
	case uint16:
		enc.int64(index, int64(typedVal))
	case uint32:
		enc.int64(index, int64(typedVal))
	default:
		return s.encodeReflected(index, reflect.ValueOf(val))
	}
	return nil
}

// encodeReflected handles pointers and named types of the supported kinds.
func (s *Stmt) encodeReflected(index int, reflectVal reflect.Value) error {
	enc := &s.c.params
	for {
		switch reflectVal.Kind() {
		case reflect.Pointer:
			if reflectVal.IsNil() {
				enc.null(index)
				return nil
			}
			reflectVal = reflectVal.Elem()
			continue
		case reflect.Slice:
			if reflectVal.Elem().Kind() != reflect.Uint8 {
				return fmt.Errorf("unsupported slice type %T", reflectVal.Interface())
			}
			enc.blob(index, unsafe.Slice((*byte)(reflectVal.UnsafePointer()), reflectVal.Len()))
			return nil
		case reflect.String:
			enc.text(index, reflectVal.String())
			return nil
		case reflect.Bool:
			if reflectVal.Bool() {
				enc.int64(index, 1)
			} else {
				enc.int64(index, 0)
			}
			return nil
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			enc.int64(index, reflectVal.Int())
			return nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			enc.int64(index, int64(reflectVal.Uint()))
			return nil
		case reflect.Float32, reflect.Float64:
			enc.float64(index, reflectVal.Float())
			return nil
		default:
			return fmt.Errorf("unsupported type %T", reflectVal.Interface())
		}
	}
}

func (s *Stmt) encodeArgs(args []driver.NamedValue) error {
	s.c.params.reset(len(args))
	for _, arg := range args {
		index := arg.Ordinal
		if arg.Name != "" {
			index = s.d.CAPI.Call("sqlite3_bind_parameter_index", s.cptr, arg.Name).Int()
			if index == 0 {
				return fmt.Errorf("no parameter named %q found", arg.Name)
			}
		}
		if err := s.encodeValue(index, arg.Value); err != nil {
			return fmt.Errorf("failed to bind %d: %w", arg.Ordinal, err)
		}
	}
	return nil
}

func (s *Stmt) reset(_ context.Context) error {
	rc := s.d.CAPI.Call("sqlite3_reset", s.cptr).Int()
	if rc != SQLITE_OK {
		return s.d.MakeError(s.c, "sqlite3_reset", rc)
	}
	return nil
}

func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (res driver.Result, retErr error) {
	defer catchIntoError(&retErr)
	err := s.encodeArgs(args)
	if err != nil {
		return nil, err
	}
	dec, err := s.c.batchedQuery(s.cptr, false, false)
	if err != nil {
		return nil, err
	}
	changes, lastInsertRowID, err := dec.trailer()
	if err != nil {
		return nil, err
	}
	if err = s.reset(ctx); err != nil {
		return nil, err
	}
	return &Result{lastInsertID: lastInsertRowID, rowsAffected: changes}, nil
}

func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (rows driver.Rows, retErr error) {
	defer catchIntoError(&retErr)
	err := s.encodeArgs(args)
	if err != nil {
		return nil, err
	}
	dec, err := s.c.batchedQuery(s.cptr, !s.colsLoaded, true)
	if err != nil {
		return nil, err
	}
	if !s.colsLoaded {
		s.cols = dec.columns
		s.decls = make([]string, len(dec.decltypes))
		for i, decl := range dec.decltypes {
			s.decls[i] = strings.ToLower(decl)
		}
		s.colsLoaded = true
	}
	return &Rows{ctx: ctx, Stmt: s, dec: dec}, nil
}

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(noContextFunc, valuesToNamedValues(args))
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(noContextFunc, valuesToNamedValues(args))
}
