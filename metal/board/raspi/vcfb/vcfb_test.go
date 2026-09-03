package vcfb

import (
	"testing"

	"github.com/xinix00/HopOS/metal/v2/driver/fb"
)

func validDesc() fb.Desc {
	return fb.Desc{Base: 0x100000, Width: 1920, Height: 1080, Stride: 1920 * 4, BPP: 32}
}

func TestDiscoveryFailureIsCached(t *testing.T) {
	var state discoveryState
	calls := 0

	if _, ok := state.get(func() (fb.Desc, bool) {
		calls++
		return fb.Desc{}, false
	}); ok {
		t.Fatal("mislukte discovery werd als framebuffer geaccepteerd")
	}
	if _, ok := state.get(func() (fb.Desc, bool) {
		calls++
		return validDesc(), true
	}); ok {
		t.Fatal("tweede discovery mocht na de fail-once niet meer draaien")
	}
	if calls != 1 {
		t.Fatalf("discovery %d keer uitgevoerd, wil precies 1", calls)
	}
}

func TestDiscoveryMalformedAllocationIsCachedAsFailure(t *testing.T) {
	var state discoveryState
	calls := 0
	bad := validDesc()
	bad.Stride = 0 // allocate kan gelukt zijn, maar de pitch-tag was ongeldig

	for range 2 {
		if _, ok := state.get(func() (fb.Desc, bool) {
			calls++
			return bad, true
		}); ok {
			t.Fatal("onsane framebufferdescriptor werd geaccepteerd")
		}
	}
	if calls != 1 {
		t.Fatalf("onsane allocatie werd %d keer geprobeerd, wil precies 1", calls)
	}
}

func TestDiscoverySuccessIsCached(t *testing.T) {
	var state discoveryState
	want := validDesc()
	calls := 0
	for range 2 {
		got, ok := state.get(func() (fb.Desc, bool) {
			calls++
			return want, true
		})
		if !ok || got != want {
			t.Fatalf("cached discovery = (%+v, %v), wil (%+v, true)", got, ok, want)
		}
	}
	if calls != 1 {
		t.Fatalf("geslaagde discovery %d keer uitgevoerd, wil precies 1", calls)
	}
}
