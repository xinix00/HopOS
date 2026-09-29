package drbg

import (
	"encoding/hex"
	"testing"
)

// Tests are deliberately sequential: they exercise the package generator.
func resetGenerator() {
	state = [32]byte{}
	ctr, sinceReseed = 0, 0
	source = "jitter"
	fill = nil
}

func fixedFill(b []byte) (string, bool) {
	for i := range b {
		b[i] = byte(i)
	}
	return "test", true
}

func noFill([]byte) (string, bool) { return "", false }

func TestGeneratorVectors(t *testing.T) {
	// Vectors independently computed with Python hashlib, using the existing
	// 48-byte seed and Hash(state || little-endian counter || suffix) format.
	for _, tc := range []struct {
		name string
		fill func([]byte) (string, bool)
		want string
	}{
		{"hardware", fixedFill, "608c25ca8a54d6b4968d01a847e7f07a373bef7e6dccb225f74be64a6f8109599e9dec39542d75a9a201e2985ea2f8d784531d4b3ff71d517ccf9745a44159fe305a5424184b6f0f3ab0a60dd683296e"},
		{"jitter", noFill, "d2945313752ac54b559edd5bde8340381a746e32670dec5c84ef506d5ac14712"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGenerator()
			defer resetGenerator()
			var ticks uint64
			Init(tc.fill, func() uint64 { ticks++; return ticks*101 + 19 })
			got := make([]byte, len(tc.want)/2)
			Read(got)
			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("output changed: %x", got)
			}
		})
	}
}

func TestBootPathDoesNotAllocate(t *testing.T) {
	resetGenerator()
	defer resetGenerator()
	var output [32]byte
	var ticks uint64
	counter := func() uint64 { ticks++; return ticks }
	for _, tc := range []struct {
		name string
		fill func([]byte) (string, bool)
	}{{"hardware", fixedFill}, {"jitter", noFill}} {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(10, func() {
				Init(tc.fill, counter)
				Read(output[:])
			})
			if allocs != 0 {
				t.Fatalf("Init + initial Read allocated %v times", allocs)
			}
		})
	}
}

func TestInitWorkspaceIsCleared(t *testing.T) {
	resetGenerator()
	defer resetGenerator()
	for i := range initSeed {
		initSeed[i] = 0xa5
	}
	Init(func(b []byte) (string, bool) {
		for _, v := range b {
			if v != 0 {
				t.Fatal("fill callback received stale seed bytes")
			}
		}
		b[0] = 0x37
		return "test", true
	}, nil)
	for _, v := range initSeed {
		if v != 0 {
			t.Fatal("seed material retained after Init")
		}
	}
}
