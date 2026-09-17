// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlite_wasm_js

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Wire format shared with web/src/api/wasm/sqlite_bridge.ts. Every syscall/js
// call costs a few microseconds, so instead of binding and reading each value
// with separate calls, a whole parameter set and a whole chunk of result rows
// cross the Go<->JS boundary as one byte buffer each. All integers are little
// endian.
//
// Parameters (Go -> JS):
//
//	u32 count
//	count × { u16 index, u8 tag, payload }
//
// Result (JS -> Go), one per batchedQuery/batchedStep call:
//
//	u32 rc          SQLite result code of the failing call, 0 on success
//	u8  phase       which call failed (phaseBind/phaseStep), only set if rc != 0
//	u8  done        1 if sqlite3_step returned SQLITE_DONE
//	u32 ncol
//	u8  hasColumns  1 if column names and declared types follow
//	if hasColumns: ncol × { u32 len, name, u32 len, decltype }
//	u32 nrows
//	nrows × ncol × { u8 tag, payload }
//	if done: i64 changes, i64 lastInsertRowID
//
// Value payloads by tag: NULL has none, INT64 is i64, FLOAT64 is f64,
// TEXT and BLOB are u32 length followed by the bytes.

const (
	tagNull  = 0
	tagInt   = 1
	tagFloat = 2
	tagText  = 3
	tagBlob  = 4

	phaseBind = 1
	phaseStep = 2
)

var errTruncated = errors.New("truncated result buffer")

// paramEncoder builds the parameter buffer. The buffer is reused between
// statements, so it must be consumed before the next reset.
type paramEncoder struct {
	buf []byte
}

func (e *paramEncoder) reset(count int) {
	e.buf = binary.LittleEndian.AppendUint32(e.buf[:0], uint32(count))
}

func (e *paramEncoder) header(index int, tag byte) {
	e.buf = binary.LittleEndian.AppendUint16(e.buf, uint16(index))
	e.buf = append(e.buf, tag)
}

func (e *paramEncoder) null(index int) {
	e.header(index, tagNull)
}

func (e *paramEncoder) int64(index int, v int64) {
	e.header(index, tagInt)
	e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(v))
}

func (e *paramEncoder) float64(index int, v float64) {
	e.header(index, tagFloat)
	e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(v))
}

func (e *paramEncoder) text(index int, v string) {
	e.header(index, tagText)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, uint32(len(v)))
	e.buf = append(e.buf, v...)
}

func (e *paramEncoder) blob(index int, v []byte) {
	e.header(index, tagBlob)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, uint32(len(v)))
	e.buf = append(e.buf, v...)
}

// resultDecoder reads a result buffer. Decoded strings and blobs are copies,
// so the buffer can be discarded after decoding.
type resultDecoder struct {
	buf []byte
	off int

	rc         int
	phase      byte
	done       bool
	ncol       int
	hasColumns bool
	columns    []string
	decltypes  []string
	nrows      int
	rowsRead   int

	// tags of the most recently decoded row, indexed by column
	lastTags []byte
}

func newResultDecoder(buf []byte) (*resultDecoder, error) {
	d := &resultDecoder{buf: buf}
	rc, err := d.u32()
	if err != nil {
		return nil, err
	}
	d.rc = int(rc)
	if d.phase, err = d.u8(); err != nil {
		return nil, err
	}
	if d.rc != 0 {
		return d, nil
	}
	var done byte
	if done, err = d.u8(); err != nil {
		return nil, err
	}
	d.done = done == 1
	ncol, err := d.u32()
	if err != nil {
		return nil, err
	}
	d.ncol = int(ncol)
	var hasColumns byte
	if hasColumns, err = d.u8(); err != nil {
		return nil, err
	}
	d.hasColumns = hasColumns == 1
	if d.hasColumns {
		d.columns = make([]string, d.ncol)
		d.decltypes = make([]string, d.ncol)
		for i := range d.columns {
			if d.columns[i], err = d.str(); err != nil {
				return nil, err
			}
			if d.decltypes[i], err = d.str(); err != nil {
				return nil, err
			}
		}
	}
	nrows, err := d.u32()
	if err != nil {
		return nil, err
	}
	d.nrows = int(nrows)
	d.lastTags = make([]byte, d.ncol)
	return d, nil
}

func (d *resultDecoder) need(n int) error {
	if d.off+n > len(d.buf) {
		return errTruncated
	}
	return nil
}

func (d *resultDecoder) u8() (byte, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	v := d.buf[d.off]
	d.off++
	return v, nil
}

func (d *resultDecoder) u32() (uint32, error) {
	if err := d.need(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(d.buf[d.off:])
	d.off += 4
	return v, nil
}

func (d *resultDecoder) u64() (uint64, error) {
	if err := d.need(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(d.buf[d.off:])
	d.off += 8
	return v, nil
}

func (d *resultDecoder) bytes() ([]byte, error) {
	n, err := d.u32()
	if err != nil {
		return nil, err
	}
	if err = d.need(int(n)); err != nil {
		return nil, err
	}
	v := d.buf[d.off : d.off+int(n)]
	d.off += int(n)
	return v, nil
}

func (d *resultDecoder) str() (string, error) {
	b, err := d.bytes()
	return string(b), err
}

// hasRows reports whether the current chunk still has undecoded rows.
func (d *resultDecoder) hasRows() bool {
	return d.rowsRead < d.nrows
}

// value decodes one tagged value as nil, int64, float64, string or []byte.
func (d *resultDecoder) value() (byte, any, error) {
	tag, err := d.u8()
	if err != nil {
		return 0, nil, err
	}
	switch tag {
	case tagNull:
		return tag, nil, nil
	case tagInt:
		v, err := d.u64()
		return tag, int64(v), err
	case tagFloat:
		v, err := d.u64()
		return tag, math.Float64frombits(v), err
	case tagText:
		v, err := d.str()
		return tag, v, err
	case tagBlob:
		v, err := d.bytes()
		if err != nil {
			return tag, nil, err
		}
		if len(v) == 0 {
			return tag, []byte{}, nil
		}
		return tag, bytes.Clone(v), nil
	default:
		return tag, nil, fmt.Errorf("unknown value tag %d", tag)
	}
}

// row decodes the next row into dest, which must have ncol entries.
func (d *resultDecoder) row(dest []any) error {
	if !d.hasRows() {
		return errors.New("no rows left in chunk")
	}
	if len(dest) != d.ncol {
		return fmt.Errorf("can't decode %d columns into %d values", d.ncol, len(dest))
	}
	for i := range dest {
		tag, v, err := d.value()
		if err != nil {
			return err
		}
		d.lastTags[i] = tag
		dest[i] = v
	}
	d.rowsRead++
	return nil
}

// trailer returns the change count and last insert rowid. Only valid after
// all rows of a chunk with done set have been decoded.
func (d *resultDecoder) trailer() (changes, lastInsertRowID int64, err error) {
	if !d.done {
		return 0, 0, errors.New("statement not finished")
	}
	for d.hasRows() {
		// Skip rows the caller didn't want.
		if err = d.row(make([]any, d.ncol)); err != nil {
			return
		}
	}
	var c, r uint64
	if c, err = d.u64(); err != nil {
		return
	}
	if r, err = d.u64(); err != nil {
		return
	}
	return int64(c), int64(r), nil
}
