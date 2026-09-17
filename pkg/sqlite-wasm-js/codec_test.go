// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlite_wasm_js

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// decodeParams mirrors bindParams in sqlite_bridge.ts.
func decodeParams(t *testing.T, buf []byte) map[int]any {
	t.Helper()
	out := map[int]any{}
	off := 0
	count := binary.LittleEndian.Uint32(buf[off:])
	off += 4
	for range count {
		index := int(binary.LittleEndian.Uint16(buf[off:]))
		off += 2
		tag := buf[off]
		off++
		switch tag {
		case tagNull:
			out[index] = nil
		case tagInt:
			out[index] = int64(binary.LittleEndian.Uint64(buf[off:]))
			off += 8
		case tagFloat:
			out[index] = math.Float64frombits(binary.LittleEndian.Uint64(buf[off:]))
			off += 8
		case tagText, tagBlob:
			n := int(binary.LittleEndian.Uint32(buf[off:]))
			off += 4
			if tag == tagText {
				out[index] = string(buf[off : off+n])
			} else {
				out[index] = append([]byte{}, buf[off:off+n]...)
			}
			off += n
		default:
			t.Fatalf("unknown tag %d", tag)
		}
	}
	if off != len(buf) {
		t.Fatalf("trailing bytes: %d != %d", off, len(buf))
	}
	return out
}

func TestParamEncoder(t *testing.T) {
	var e paramEncoder
	e.reset(7)
	e.null(1)
	e.int64(2, math.MinInt64)
	e.int64(3, 1<<53+1)
	e.float64(4, math.NaN())
	e.text(5, "")
	e.text(6, "hé\x00llo")
	e.blob(7, []byte{})
	got := decodeParams(t, e.buf)
	if got[1] != nil || got[2] != int64(math.MinInt64) || got[3] != int64(1<<53+1) {
		t.Errorf("bad ints/null: %v", got)
	}
	if f, ok := got[4].(float64); !ok || !math.IsNaN(f) {
		t.Errorf("bad float: %v", got[4])
	}
	if got[5] != "" || got[6] != "hé\x00llo" {
		t.Errorf("bad text: %q %q", got[5], got[6])
	}
	if b, ok := got[7].([]byte); !ok || len(b) != 0 {
		t.Errorf("bad blob: %v", got[7])
	}
	// reset must reuse the buffer and drop old contents
	e.reset(1)
	e.int64(1, 5)
	if got = decodeParams(t, e.buf); len(got) != 1 || got[1] != int64(5) {
		t.Errorf("reset didn't clear: %v", got)
	}
}

func TestResultDecoder(t *testing.T) {
	var e resultEncoder
	e.header(0, 0, true, []string{"id", "name", "ts", "data"}, []string{"INTEGER", "TEXT", "timestamp", "BLOB"}, 4)
	e.row(int64(1), "alice", "2024-01-02 03:04:05.000000000+00:00", []byte{1, 2, 3})
	e.row(int64(-1), "", nil, []byte{})
	e.row(int64(math.MaxInt64), "\x00", 1.5, nil)
	e.trailer(3, 42)

	d, err := newResultDecoder(e.buf)
	if err != nil {
		t.Fatal(err)
	}
	if d.rc != 0 || !d.done || d.ncol != 4 || !d.hasColumns || d.nrows != 3 {
		t.Fatalf("bad header: %+v", d)
	}
	if d.columns[1] != "name" || d.decltypes[2] != "timestamp" {
		t.Fatalf("bad columns: %v %v", d.columns, d.decltypes)
	}
	dest := make([]any, 4)
	if err = d.row(dest); err != nil {
		t.Fatal(err)
	}
	if dest[0] != int64(1) || dest[1] != "alice" || string(dest[3].([]byte)) != "\x01\x02\x03" {
		t.Errorf("row 1: %v", dest)
	}
	if d.lastTags[3] != tagBlob || d.lastTags[2] != tagText {
		t.Errorf("tags: %v", d.lastTags)
	}
	if err = d.row(dest); err != nil {
		t.Fatal(err)
	}
	if dest[0] != int64(-1) || dest[1] != "" || dest[2] != nil {
		t.Errorf("row 2: %v", dest)
	}
	if b, ok := dest[3].([]byte); !ok || b == nil || len(b) != 0 {
		t.Errorf("empty blob must decode as empty non-nil slice: %#v", dest[3])
	}
	if err = d.row(dest); err != nil {
		t.Fatal(err)
	}
	if dest[0] != int64(math.MaxInt64) || dest[1] != "\x00" || dest[2] != 1.5 || dest[3] != nil {
		t.Errorf("row 3: %v", dest)
	}
	if d.hasRows() {
		t.Error("expected no more rows")
	}
	changes, rowid, err := d.trailer()
	if err != nil || changes != 3 || rowid != 42 {
		t.Errorf("trailer: %d %d %v", changes, rowid, err)
	}
}

func TestResultDecoderDecodedValuesDontAliasBuffer(t *testing.T) {
	var e resultEncoder
	e.header(0, 0, true, nil, nil, 2)
	e.row("text", []byte("blob"))
	d, err := newResultDecoder(e.buf)
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]any, 2)
	if err = d.row(dest); err != nil {
		t.Fatal(err)
	}
	for i := range e.buf {
		e.buf[i] = 0
	}
	if dest[0] != "text" || string(dest[1].([]byte)) != "blob" {
		t.Errorf("values alias the buffer: %v", dest)
	}
}

func TestResultDecoderTrailerSkipsUnreadRows(t *testing.T) {
	var e resultEncoder
	e.header(0, 0, true, nil, nil, 1)
	e.row(int64(7))
	e.row(int64(8))
	e.trailer(2, 8)
	d, err := newResultDecoder(e.buf)
	if err != nil {
		t.Fatal(err)
	}
	changes, rowid, err := d.trailer()
	if err != nil || changes != 2 || rowid != 8 {
		t.Errorf("trailer: %d %d %v", changes, rowid, err)
	}
}

func TestResultDecoderError(t *testing.T) {
	var e resultEncoder
	e.header(5, phaseStep, false, nil, nil, 0)
	d, err := newResultDecoder(e.buf)
	if err != nil {
		t.Fatal(err)
	}
	if d.rc != 5 || d.phase != phaseStep {
		t.Errorf("bad error header: %+v", d)
	}
}

func TestResultDecoderNotDone(t *testing.T) {
	var e resultEncoder
	e.header(0, 0, false, nil, nil, 1)
	e.row(int64(1))
	d, err := newResultDecoder(e.buf)
	if err != nil {
		t.Fatal(err)
	}
	if d.done {
		t.Error("expected done=false")
	}
	if _, _, err = d.trailer(); err == nil {
		t.Error("trailer must fail when not done")
	}
}

func TestResultDecoderTruncated(t *testing.T) {
	var e resultEncoder
	e.header(0, 0, true, []string{"a"}, []string{"TEXT"}, 1)
	e.row("hello")
	e.trailer(1, 1)
	for n := 0; n < len(e.buf); n++ {
		d, err := newResultDecoder(e.buf[:n])
		if err != nil {
			if !errors.Is(err, errTruncated) {
				t.Errorf("len %d: unexpected error %v", n, err)
			}
			continue
		}
		dest := make([]any, 1)
		for d.hasRows() {
			if err = d.row(dest); err != nil {
				break
			}
		}
		if err == nil {
			_, _, err = d.trailer()
		}
		if err == nil {
			t.Errorf("len %d: truncated buffer decoded without error", n)
		}
	}
}

func FuzzResultDecoder(f *testing.F) {
	var e resultEncoder
	e.header(0, 0, true, []string{"a", "b"}, []string{"TEXT", "INTEGER"}, 2)
	e.row("x", int64(1))
	e.trailer(1, 1)
	f.Add(e.buf)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		d, err := newResultDecoder(buf)
		if err != nil {
			return
		}
		dest := make([]any, d.ncol)
		for d.hasRows() {
			if d.row(dest) != nil {
				return
			}
		}
		_, _, _ = d.trailer()
	})
}
