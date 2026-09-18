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

package oggopus

import (
	"errors"
	"fmt"
)

// Just enough EBML to walk what a browser's MediaRecorder writes: find the
// Opus track, its OpusHead, and every audio frame in order.
const (
	idSegment      = 0x18538067
	idTracks       = 0x1654AE6B
	idTrackEntry   = 0xAE
	idTrackNumber  = 0xD7
	idCodecID      = 0x86
	idCodecPrivate = 0x63A2
	idCluster      = 0x1F43B675
	idSimpleBlock  = 0xA3
	idBlockGroup   = 0xA0
	idBlock        = 0xA1
)

// Elements whose children we walk into. Everything else is skipped whole.
var containers = map[uint32]bool{
	idSegment:    true,
	idTracks:     true,
	idTrackEntry: true,
	idCluster:    true,
	idBlockGroup: true,
}

var (
	// ErrNotWebM is returned for input that isn't an EBML document at all.
	ErrNotWebM = errors.New("not a webm/matroska file")
	// ErrNoOpusTrack is returned when the file holds no Opus audio.
	ErrNoOpusTrack = errors.New("no opus audio track")
	errTruncated   = errors.New("truncated webm data")
)

type reader struct {
	data []byte
	pos  int
}

func (r *reader) remaining() int {
	return len(r.data) - r.pos
}

// elementID reads a class-A to class-D identifier, which keeps its length
// marker as part of the value.
func (r *reader) elementID() (uint32, error) {
	if r.remaining() < 1 {
		return 0, errTruncated
	}
	first := r.data[r.pos]
	var length int
	switch {
	case first&0x80 != 0:
		length = 1
	case first&0x40 != 0:
		length = 2
	case first&0x20 != 0:
		length = 3
	case first&0x10 != 0:
		length = 4
	default:
		return 0, fmt.Errorf("%w: invalid element id at %d", ErrNotWebM, r.pos)
	}
	if r.remaining() < length {
		return 0, errTruncated
	}
	var id uint32
	for i := 0; i < length; i++ {
		id = id<<8 | uint32(r.data[r.pos+i])
	}
	r.pos += length
	return id, nil
}

// size reads a data size, where the length marker is stripped from the value.
// An all-ones value means "unknown", which browsers use for the live Segment.
func (r *reader) size() (int64, bool, error) {
	if r.remaining() < 1 {
		return 0, false, errTruncated
	}
	first := r.data[r.pos]
	length := 0
	for mask := byte(0x80); mask != 0; mask >>= 1 {
		length++
		if first&mask != 0 {
			break
		}
	}
	if length > 8 || r.remaining() < length {
		return 0, false, errTruncated
	}
	value := int64(first & (0xFF >> uint(length)))
	unknown := value == int64(0xFF>>uint(length))
	for i := 1; i < length; i++ {
		b := r.data[r.pos+i]
		value = value<<8 | int64(b)
		unknown = unknown && b == 0xFF
	}
	r.pos += length
	return value, unknown, nil
}

// vint reads a track number inside a block, same encoding as a data size.
func vint(data []byte) (value uint64, read int, err error) {
	if len(data) < 1 {
		return 0, 0, errTruncated
	}
	length := 0
	for mask := byte(0x80); mask != 0; mask >>= 1 {
		length++
		if data[0]&mask != 0 {
			break
		}
	}
	if length > 8 || len(data) < length {
		return 0, 0, errTruncated
	}
	value = uint64(data[0] & (0xFF >> uint(length)))
	for i := 1; i < length; i++ {
		value = value<<8 | uint64(data[i])
	}
	return value, length, nil
}
