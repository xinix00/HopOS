package hvs

import "testing"

// TestParseList: de NEXT-walk volgt entries en stopt op END — ook op rommel.
func TestParseList(t *testing.T) {
	words := make([]uint32, 64)
	// Entry 1 op woord 4: VALID, NEXT=8, fmt=7, unity.
	words[4] = 1<<30 | 8<<24 | 1<<15 | 7
	for i := 5; i < 12; i++ {
		words[i] = uint32(0x1000 + i)
	}
	// Entry 2 op woord 12: VALID, NEXT=6, fmt=1.
	words[12] = 1<<30 | 6<<24 | 1
	// END op woord 18.
	words[18] = 1 << 31

	planes := ParseList(words, 4)
	if len(planes) != 2 {
		t.Fatalf("wil 2 planes, kreeg %d", len(planes))
	}
	if planes[0].Index != 4 || planes[0].Format != 7 || !planes[0].Unity || len(planes[0].Words) != 8 {
		t.Fatalf("plane 0 fout: %+v", planes[0])
	}
	if planes[1].Index != 12 || planes[1].Format != 1 || len(planes[1].Words) != 6 {
		t.Fatalf("plane 1 fout: %+v", planes[1])
	}

	// Rommel (NEXT=0 overal, nooit END): de walk moet begrensd stoppen.
	junk := make([]uint32, 16)
	for i := range junk {
		junk[i] = 1 << 30
	}
	if p := ParseList(junk, 0); len(p) > 64 {
		t.Fatalf("walk niet begrensd: %d", len(p))
	}
}
