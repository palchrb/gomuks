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

// Package oggopus repackages Opus audio from a WebM container into an Ogg
// one. Browsers record voice messages as WebM, while the Matrix voice message
// spec and clients such as Element X expect Ogg, and the server build of
// gomuks converts between them with ffmpeg. The audio is identical either
// way, so this only moves the packets to a different container: no codec is
// involved, and nothing is re-encoded.
package oggopus

import (
	"fmt"
)

// Frames holds the Opus packets of a track together with the header the
// container carried for them.
type Frames struct {
	// OpusHead as stored in CodecPrivate, which Ogg needs as its first packet.
	Header  []byte
	Packets [][]byte
}

// ReadWebM finds the Opus track in a WebM file and returns its packets in
// order. It tolerates the unknown-length Segment and single-frame Clusters
// that MediaRecorder produces.
func ReadWebM(data []byte) (*Frames, error) {
	r := &reader{data: data}
	out := &Frames{}
	opusTrack := int64(-1)
	// Tracks appear before any Cluster, so a single pass is enough.
	var walk func(end int) error
	walk = func(end int) error {
		for r.pos < end {
			id, err := r.elementID()
			if err != nil {
				if err == errTruncated {
					// A recording cut short mid-element: keep what we have.
					return nil
				}
				return err
			}
			size, unknown, err := r.size()
			if err != nil {
				return nil
			}
			childEnd := end
			if !unknown {
				childEnd = r.pos + int(size)
				if childEnd > end {
					// Length runs past its parent, which browsers do for the
					// last Cluster of an interrupted recording.
					childEnd = end
				}
			}
			if containers[id] {
				if err = walk(childEnd); err != nil {
					return err
				}
				r.pos = childEnd
				continue
			}
			if r.pos+int(size) > end || unknown {
				return nil
			}
			value := r.data[r.pos : r.pos+int(size)]
			switch id {
			case idTrackNumber:
				var num int64
				for _, b := range value {
					num = num<<8 | int64(b)
				}
				// Remembered until the CodecID says whether it's the one.
				opusTrack = -num
			case idCodecID:
				if string(value) == "A_OPUS" && opusTrack < 0 {
					opusTrack = -opusTrack
				}
			case idCodecPrivate:
				if len(out.Header) == 0 {
					out.Header = append([]byte(nil), value...)
				}
			case idSimpleBlock, idBlock:
				if opusTrack <= 0 {
					break
				}
				track, read, err := vint(value)
				if err != nil || len(value) < read+3 {
					break
				}
				if int64(track) != opusTrack {
					break
				}
				// Two bytes of timestamp and one of flags, then the frame.
				// MediaRecorder writes one unlaced frame per block.
				frame := value[read+3:]
				if len(frame) > 0 {
					out.Packets = append(out.Packets, append([]byte(nil), frame...))
				}
			}
			r.pos += int(size)
		}
		return nil
	}
	if _, err := r.elementID(); err != nil {
		return nil, ErrNotWebM
	}
	size, _, err := r.size()
	if err != nil {
		return nil, ErrNotWebM
	}
	r.pos += int(size)
	if err = walk(len(data)); err != nil {
		return nil, err
	}
	if opusTrack <= 0 || len(out.Packets) == 0 {
		return nil, ErrNoOpusTrack
	}
	if len(out.Header) < 19 || string(out.Header[:8]) != "OpusHead" {
		return nil, fmt.Errorf("%w: missing or invalid OpusHead", ErrNoOpusTrack)
	}
	return out, nil
}

// Remux turns WebM-contained Opus into an Ogg Opus file. Input that isn't
// WebM with an Opus track is reported with ErrNotWebM or ErrNoOpusTrack so
// the caller can fall back to sending the original.
func Remux(webm []byte) ([]byte, error) {
	frames, err := ReadWebM(webm)
	if err != nil {
		return nil, err
	}
	return WriteOgg(frames)
}
