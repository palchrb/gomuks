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
	"encoding/binary"
	"math/rand/v2"
)

// Ogg framing, as much of it as an Opus stream needs. See RFC 3533 for the
// pages and RFC 7845 for what Opus puts in them.
const (
	headerTypeContinued = 0x01
	headerTypeBOS       = 0x02
	headerTypeEOS       = 0x04
	// A page carries at most 255 segments of at most 255 bytes.
	maxSegmentsPerPage = 255
	// Opus always decodes to 48 kHz for the purposes of granule positions.
	opusSampleRate = 48000
)

// Ogg uses CRC-32 with the same polynomial as Ethernet but without the usual
// reflection or final inversion, so the standard library's tables don't fit
// and this one is built by hand.
func oggCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = crc<<8 ^ oggCRCLookup[byte(crc>>24)^b]
	}
	return crc
}

var oggCRCLookup = func() [256]uint32 {
	var table [256]uint32
	for i := range table {
		crc := uint32(i) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
	return table
}()

type oggWriter struct {
	out      []byte
	serial   uint32
	sequence uint32
}

// page appends one Ogg page. Packets are split into 255-byte segments, and a
// packet whose length is a multiple of 255 gets a trailing zero-length
// segment so the reader knows it ended.
func (w *oggWriter) page(headerType byte, granule uint64, packets [][]byte) {
	var segments []byte
	var body []byte
	for _, packet := range packets {
		remaining := len(packet)
		for remaining >= 255 {
			segments = append(segments, 255)
			remaining -= 255
		}
		segments = append(segments, byte(remaining))
		body = append(body, packet...)
	}
	header := make([]byte, 27, 27+len(segments))
	copy(header, "OggS")
	header[4] = 0 // stream structure version
	header[5] = headerType
	binary.LittleEndian.PutUint64(header[6:], granule)
	binary.LittleEndian.PutUint32(header[14:], w.serial)
	binary.LittleEndian.PutUint32(header[18:], w.sequence)
	// The checksum is computed over the page with this field zeroed.
	header[26] = byte(len(segments))
	header = append(header, segments...)
	page := append(header, body...)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
	w.out = append(w.out, page...)
	w.sequence++
}

// frameSamples reads how many 48 kHz samples an Opus packet decodes to, from
// its table of contents byte. Needed for the granule positions, which are
// what players use to show a duration and to seek.
func frameSamples(packet []byte) uint64 {
	if len(packet) < 1 {
		return 0
	}
	toc := packet[0]
	config := toc >> 3
	var frameDuration uint64 // in samples at 48 kHz
	switch {
	case config < 12:
		// SILK: 10, 20, 40 or 60 ms
		switch config % 4 {
		case 0:
			frameDuration = 480
		case 1:
			frameDuration = 960
		case 2:
			frameDuration = 1920
		case 3:
			frameDuration = 2880
		}
	case config < 16:
		// Hybrid: 10 or 20 ms
		if config%2 == 0 {
			frameDuration = 480
		} else {
			frameDuration = 960
		}
	default:
		// CELT: 2.5, 5, 10 or 20 ms
		switch config % 4 {
		case 0:
			frameDuration = 120
		case 1:
			frameDuration = 240
		case 2:
			frameDuration = 480
		case 3:
			frameDuration = 960
		}
	}
	frames := uint64(1)
	switch toc & 0x03 {
	case 1, 2:
		frames = 2
	case 3:
		if len(packet) < 2 {
			return frameDuration
		}
		frames = uint64(packet[1] & 0x3F)
	}
	return frameDuration * frames
}

// WriteOgg wraps Opus packets in an Ogg stream: the OpusHead page, an
// OpusTags page, then the audio.
func WriteOgg(frames *Frames) ([]byte, error) {
	w := &oggWriter{serial: rand.Uint32()}
	w.page(headerTypeBOS, 0, [][]byte{frames.Header})
	tags := append([]byte("OpusTags"), 0, 0, 0, 0 /* empty vendor string */)
	tags = append(tags, 0, 0, 0, 0 /* no comments */)
	w.page(0, 0, [][]byte{tags})

	var granule uint64
	batch := make([][]byte, 0, maxSegmentsPerPage)
	batchSegments := 0
	flush := func(last bool) {
		if len(batch) == 0 {
			return
		}
		headerType := byte(0)
		if last {
			headerType = headerTypeEOS
		}
		w.page(headerType, granule, batch)
		batch = batch[:0]
		batchSegments = 0
	}
	for i, packet := range frames.Packets {
		needed := len(packet)/255 + 1
		if batchSegments+needed > maxSegmentsPerPage {
			flush(false)
		}
		batch = append(batch, packet)
		batchSegments += needed
		granule += frameSamples(packet)
		if i == len(frames.Packets)-1 {
			flush(true)
		}
	}
	return w.out, nil
}
