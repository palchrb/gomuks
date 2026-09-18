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
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// The test input is built here rather than checked in, so the expectations
// are visible: a WebM shaped like the one MediaRecorder writes, with one
// Opus track and one frame per SimpleBlock.
func ebmlElement(id uint32, body []byte) []byte {
	var out []byte
	switch {
	case id <= 0xFF:
		out = []byte{byte(id)}
	case id <= 0xFFFF:
		out = []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		out = []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		out = []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
	// Four-byte data size, which is always long enough here.
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(body)))
	size[0] |= 0x10
	return append(append(out, size...), body...)
}

func opusHead(channels byte) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1 // version
	head[9] = channels
	binary.LittleEndian.PutUint16(head[10:], 312) // pre-skip
	binary.LittleEndian.PutUint32(head[12:], 48000)
	return head
}

func simpleBlock(track byte, frame []byte) []byte {
	body := []byte{0x80 | track, 0, 0, 0x80}
	return ebmlElement(idSimpleBlock, append(body, frame...))
}

// A 20 ms SILK mono frame: config 1, one frame per packet.
func opusFrame(size int) []byte {
	frame := make([]byte, size)
	frame[0] = 1 << 3
	return frame
}

func buildWebM(frames ...[]byte) []byte {
	track := ebmlElement(idTrackEntry, bytes.Join([][]byte{
		ebmlElement(idTrackNumber, []byte{1}),
		ebmlElement(idCodecID, []byte("A_OPUS")),
		ebmlElement(idCodecPrivate, opusHead(1)),
	}, nil))
	var blocks []byte
	for _, frame := range frames {
		blocks = append(blocks, simpleBlock(1, frame)...)
	}
	segment := append(ebmlElement(idTracks, track), ebmlElement(idCluster, blocks)...)
	// EBML header, contents irrelevant to us, then the Segment.
	return append(ebmlElement(0x1A45DFA3, []byte{0x42, 0x86, 0x81, 0x01}),
		ebmlElement(idSegment, segment)...)
}

func TestReadWebM(t *testing.T) {
	webm := buildWebM(opusFrame(10), opusFrame(20), opusFrame(30))
	frames, err := ReadWebM(webm)
	if err != nil {
		t.Fatalf("ReadWebM: %v", err)
	}
	if string(frames.Header[:8]) != "OpusHead" {
		t.Errorf("header is %q, want OpusHead", frames.Header[:8])
	}
	if len(frames.Packets) != 3 {
		t.Fatalf("got %d packets, want 3", len(frames.Packets))
	}
	for i, want := range []int{10, 20, 30} {
		if len(frames.Packets[i]) != want {
			t.Errorf("packet %d is %d bytes, want %d", i, len(frames.Packets[i]), want)
		}
	}
}

func TestReadWebMRejectsOtherInput(t *testing.T) {
	if _, err := ReadWebM([]byte("this is not a webm file at all")); err == nil {
		t.Error("expected an error for input that isn't EBML")
	}
	// Valid EBML, but the track is Vorbis rather than Opus.
	track := ebmlElement(idTrackEntry, bytes.Join([][]byte{
		ebmlElement(idTrackNumber, []byte{1}),
		ebmlElement(idCodecID, []byte("A_VORBIS")),
	}, nil))
	webm := append(ebmlElement(0x1A45DFA3, []byte{0x42, 0x86, 0x81, 0x01}),
		ebmlElement(idSegment, ebmlElement(idTracks, track))...)
	if _, err := ReadWebM(webm); !errors.Is(err, ErrNoOpusTrack) {
		t.Errorf("got %v, want ErrNoOpusTrack", err)
	}
}

func TestRemuxProducesReadableOgg(t *testing.T) {
	ogg, err := Remux(buildWebM(opusFrame(40), opusFrame(50)))
	if err != nil {
		t.Fatalf("Remux: %v", err)
	}
	if !bytes.HasPrefix(ogg, []byte("OggS")) {
		t.Fatal("output does not start with an Ogg page")
	}
	pages := readPages(t, ogg)
	if len(pages) != 3 {
		t.Fatalf("got %d pages, want 3 (head, tags, audio)", len(pages))
	}
	if !bytes.HasPrefix(pages[0].body, []byte("OpusHead")) {
		t.Error("first page is not OpusHead")
	}
	if !bytes.HasPrefix(pages[1].body, []byte("OpusTags")) {
		t.Error("second page is not OpusTags")
	}
	if pages[0].headerType&headerTypeBOS == 0 {
		t.Error("first page is not marked as the start of the stream")
	}
	if pages[len(pages)-1].headerType&headerTypeEOS == 0 {
		t.Error("last page is not marked as the end of the stream")
	}
	// Two 20 ms frames at 48 kHz.
	if pages[2].granule != 960*2 {
		t.Errorf("granule is %d, want 1920", pages[2].granule)
	}
	if !bytes.Equal(pages[2].body, make([]byte, 90)) && len(pages[2].body) != 90 {
		t.Errorf("audio page holds %d bytes, want 90", len(pages[2].body))
	}
}

type oggPage struct {
	headerType byte
	granule    uint64
	serial     uint32
	sequence   uint32
	body       []byte
}

// readPages parses the output back, checking each page's checksum on the way,
// which is the part most likely to be subtly wrong.
func readPages(t *testing.T, data []byte) []oggPage {
	t.Helper()
	var pages []oggPage
	for pos := 0; pos < len(data); {
		if pos+27 > len(data) || string(data[pos:pos+4]) != "OggS" {
			t.Fatalf("no page header at %d", pos)
		}
		segCount := int(data[pos+26])
		headerLen := 27 + segCount
		if pos+headerLen > len(data) {
			t.Fatalf("truncated segment table at %d", pos)
		}
		bodyLen := 0
		for _, seg := range data[pos+27 : pos+headerLen] {
			bodyLen += int(seg)
		}
		end := pos + headerLen + bodyLen
		if end > len(data) {
			t.Fatalf("truncated page body at %d", pos)
		}
		page := append([]byte(nil), data[pos:end]...)
		want := binary.LittleEndian.Uint32(page[22:])
		binary.LittleEndian.PutUint32(page[22:], 0)
		if got := oggCRC(page); got != want {
			t.Errorf("page at %d has checksum %08x, want %08x", pos, got, want)
		}
		pages = append(pages, oggPage{
			headerType: data[pos+5],
			granule:    binary.LittleEndian.Uint64(data[pos+6:]),
			serial:     binary.LittleEndian.Uint32(data[pos+14:]),
			sequence:   binary.LittleEndian.Uint32(data[pos+18:]),
			body:       data[pos+headerLen : end],
		})
		pos = end
	}
	return pages
}

func TestOggPagesAreNumberedAndShareASerial(t *testing.T) {
	ogg, err := Remux(buildWebM(opusFrame(10), opusFrame(10), opusFrame(10)))
	if err != nil {
		t.Fatalf("Remux: %v", err)
	}
	pages := readPages(t, ogg)
	for i, page := range pages {
		if page.sequence != uint32(i) {
			t.Errorf("page %d has sequence %d", i, page.sequence)
		}
		if page.serial != pages[0].serial {
			t.Errorf("page %d has a different stream serial", i)
		}
	}
}

// A packet whose length is a multiple of 255 needs a trailing zero-length
// segment, or a reader joins it with the next one.
func TestLongPacketsAreSegmented(t *testing.T) {
	ogg, err := Remux(buildWebM(opusFrame(255), opusFrame(510), opusFrame(7)))
	if err != nil {
		t.Fatalf("Remux: %v", err)
	}
	pages := readPages(t, ogg)
	audio := pages[len(pages)-1]
	if len(audio.body) != 255+510+7 {
		t.Errorf("audio page holds %d bytes, want %d", len(audio.body), 255+510+7)
	}
}

func TestFrameSamples(t *testing.T) {
	for _, tc := range []struct {
		name string
		toc  byte
		want uint64
	}{
		{"silk 10ms", 0 << 3, 480},
		{"silk 20ms", 1 << 3, 960},
		{"silk 60ms", 3 << 3, 2880},
		{"hybrid 10ms", 12 << 3, 480},
		{"hybrid 20ms", 13 << 3, 960},
		{"celt 2.5ms", 16 << 3, 120},
		{"celt 20ms", 19 << 3, 960},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := frameSamples([]byte{tc.toc, 0}); got != tc.want {
				t.Errorf("got %d samples, want %d", got, tc.want)
			}
		})
	}
	t.Run("two frames per packet", func(t *testing.T) {
		if got := frameSamples([]byte{1<<3 | 1, 0}); got != 1920 {
			t.Errorf("got %d samples, want 1920", got)
		}
	})
	t.Run("empty packet", func(t *testing.T) {
		if got := frameSamples(nil); got != 0 {
			t.Errorf("got %d samples, want 0", got)
		}
	})
}
