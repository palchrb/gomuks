// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlite_wasm_js

import (
	"encoding/binary"
	"fmt"
	"math"
)

// resultEncoder mirrors the JS side; used by tests and kept next to the
// decoder so the two can't drift apart.
type resultEncoder struct {
	buf      []byte
	nrowsOff int
	nrows    uint32
}

func (e *resultEncoder) header(rc int, phase byte, done bool, columns, decltypes []string, ncol int) {
	e.buf = binary.LittleEndian.AppendUint32(e.buf[:0], uint32(rc))
	e.buf = append(e.buf, phase)
	if rc != 0 {
		return
	}
	if done {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
	e.buf = binary.LittleEndian.AppendUint32(e.buf, uint32(ncol))
	if columns != nil {
		e.buf = append(e.buf, 1)
		for i := range columns {
			e.str(columns[i])
			e.str(decltypes[i])
		}
	} else {
		e.buf = append(e.buf, 0)
	}
	e.nrowsOff = len(e.buf)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, 0)
	e.nrows = 0
}

func (e *resultEncoder) str(s string) {
	e.buf = binary.LittleEndian.AppendUint32(e.buf, uint32(len(s)))
	e.buf = append(e.buf, s...)
}

func (e *resultEncoder) row(values ...any) {
	for _, v := range values {
		switch tv := v.(type) {
		case nil:
			e.buf = append(e.buf, tagNull)
		case int64:
			e.buf = append(e.buf, tagInt)
			e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(tv))
		case float64:
			e.buf = append(e.buf, tagFloat)
			e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(tv))
		case string:
			e.buf = append(e.buf, tagText)
			e.str(tv)
		case []byte:
			e.buf = append(e.buf, tagBlob)
			e.buf = binary.LittleEndian.AppendUint32(e.buf, uint32(len(tv)))
			e.buf = append(e.buf, tv...)
		default:
			panic(fmt.Sprintf("unsupported value %T", v))
		}
	}
	e.nrows++
	binary.LittleEndian.PutUint32(e.buf[e.nrowsOff:], e.nrows)
}

func (e *resultEncoder) trailer(changes, lastInsertRowID int64) {
	e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(changes))
	e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(lastInsertRowID))
}
