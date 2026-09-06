package place

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func streamHeader() []byte {
	b := make([]byte, 120)
	copy(b, "\x7fELF\x02\x01")
	binary.LittleEndian.PutUint64(b[0x20:], 64)
	binary.LittleEndian.PutUint16(b[0x36:], 56)
	binary.LittleEndian.PutUint16(b[0x38:], 1)
	binary.LittleEndian.PutUint32(b[64:], 1)
	binary.LittleEndian.PutUint64(b[64+0x18:], tLinkBase)
	binary.LittleEndian.PutUint64(b[64+0x20:], 120)
	binary.LittleEndian.PutUint64(b[64+0x28:], 120)
	return b
}

func TestStreamRejectsRangesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"header overflow", func(b []byte) { binary.LittleEndian.PutUint64(b[0x20:], ^uint64(7)) }},
		{"file range overflow", func(b []byte) { binary.LittleEndian.PutUint64(b[64+0x08:], ^uint64(7)) }},
		{"file range beyond image", func(b []byte) { binary.LittleEndian.PutUint64(b[64+0x08:], 100) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := streamHeader()
			tc.mutate(b)
			m := &memSink{b: bytes.Repeat([]byte{0xa5}, 4096)}
			s := NewStream(m, int64(len(b)), tLinkBase, 4096, 0, 1, 0)
			if _, err := s.Write(b); err == nil {
				t.Fatal("invalid range accepted")
			}
			if !bytes.Equal(m.b, bytes.Repeat([]byte{0xa5}, 4096)) {
				t.Fatal("invalid header wrote partition")
			}
		})
	}
}

func TestStreamCountsBufferedHeaderAgainstImageSize(t *testing.T) {
	s := NewStream(&memSink{b: make([]byte, 4096)}, 64, tLinkBase, 4096, 0, 1, 0)
	if _, err := s.Write(make([]byte, 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(make([]byte, 40)); err == nil {
		t.Fatal("buffered bytes exceeded announced image size")
	}
}

func TestPlacementRejectsInvalidWindow(t *testing.T) {
	for _, tc := range []struct{ base, size, lo, top uint64 }{
		{^uint64(7), 16, 0, 16}, {0x1000, 7, 0, 7},
		{0x1000, 4096, 4096, 4096}, {0x1000, 4096, 0, 8192},
	} {
		if _, err := Build(bytes.NewReader(nil), 0, tc.base, tc.size, tc.lo, tc.top, 1, 0); err == nil {
			t.Fatal("invalid placement window accepted")
		}
		s := NewStream(&memSink{}, 120, tc.base, tc.size, tc.lo, 1, 0)
		if tc.top <= tc.size && s.fail == nil {
			t.Fatal("stream accepted invalid placement window")
		}
	}
}
