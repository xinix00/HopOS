package systemapi

import (
	"bytes"
	"testing"
)

func TestFrameRoundtripEnFragmentatie(t *testing.T) {
	p := bytes.Repeat([]byte{0xa5}, MaxIOChunk)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, KindCall, p); err != nil {
		t.Fatal(err)
	}
	kind, got, err := ReadFrame(&oneByteReader{r: &wire})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindCall || !bytes.Equal(got, p) {
		t.Fatalf("kind=%d bytes=%d", kind, len(got))
	}
}

type oneByteReader struct{ r *bytes.Buffer }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}

func TestRejectsOversizeBeforeWrite(t *testing.T) {
	if err := WriteFrame(&bytes.Buffer{}, KindCall, make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
