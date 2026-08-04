package slots

import (
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// De ABI-staart moet alles dragen wat er in hoort: control page, hop-ABI-ringen
// en beide frame-ringen, zonder overlap en zonder over de rand te lopen. Dat is
// puur rekenwerk op constanten — het soort fout dat je op een bordje pas ziet
// als een ring stil in andermans buffer schrijft.
func TestAbiTailIndelingPastEnOverlaptNiet(t *testing.T) {
	type span struct {
		naam       string
		start, end uint64
	}
	spans := []span{
		{"control page", layout.AbiCtrlOff, layout.AbiCtrlOff + layout.CtrlStride},
		{"hop-ABI-ringen", layout.AbiRingOff, layout.AbiRingOff + layout.RingStride},
		{"net TX", layout.AbiNetOff + layout.NetTXOff, layout.AbiNetOff + layout.NetTXOff + layout.NetRingDataCap + 0x1000},
		{"net RX", layout.AbiNetOff + layout.NetRXOff, layout.AbiNetOff + layout.NetRXOff + layout.NetRingDataCap + 0x1000},
	}
	for i, s := range spans {
		if s.end > layout.AbiTail {
			t.Errorf("%s loopt tot %#x, voorbij de staart (%#x)", s.naam, s.end, uint64(layout.AbiTail))
		}
		for _, o := range spans[i+1:] {
			if s.start < o.end && o.start < s.end {
				t.Errorf("%s [%#x,%#x) overlapt %s [%#x,%#x)", s.naam, s.start, s.end, o.naam, o.start, o.end)
			}
		}
	}
}

// Beide kanten rekenen met dezelfde functies: HOP met de fysieke partitiebasis,
// de app met wat HOP in RamStart/RamSize patchte. Bij gelijke basis moeten er
// dus identieke adressen uitkomen — anders schrijft de een waar de ander niet
// leest.
func TestAbiAdressenZijnEenBronVanWaarheid(t *testing.T) {
	const base, appRAM = 0x8800_0000, 62 << 20
	tail := uint64(layout.AbiTailAt(base, appRAM))
	if tail != base+appRAM {
		t.Fatalf("staart op %#x, verwacht %#x", tail, uint64(base+appRAM))
	}
	for _, c := range []struct {
		naam string
		got  uintptr
		want uint64
	}{
		{"control page", layout.CtrlPageAt(base, appRAM), tail + layout.AbiCtrlOff},
		{"outbox", layout.RingOutboxAt(base, appRAM), tail + layout.AbiRingOff + layout.OutboxOff},
		{"inbox", layout.RingInboxAt(base, appRAM), tail + layout.AbiRingOff + layout.InboxOff},
		{"net TX", layout.NetRingTXAt(base, appRAM), tail + layout.AbiNetOff + layout.NetTXOff},
		{"net RX", layout.NetRingRXAt(base, appRAM), tail + layout.AbiNetOff + layout.NetRXOff},
	} {
		if uint64(c.got) != c.want {
			t.Errorf("%s = %#x, verwacht %#x", c.naam, c.got, c.want)
		}
	}
}

// De twee regio's in de ABI-staart die HOP zelf gebruikt en de app nooit: de
// scratch van de kooi-stub en de map-tabel. Ze staan in de slack tussen de
// hop-ABI-ringen en de frame-ringen, en dat is precies het soort plek dat je
// stil kwijtraakt als de indeling schuift — de stub schrijft er zijn
// voortgangswoord, en de hardware-walker leest de tabel.
func TestAbiStaartHeeftRuimteVoorStubEnMap(t *testing.T) {
	// Orde en niet-overlap: ringen → stub-scratch → map → frame-ringen.
	if !(layout.AbiRingOff+layout.RingStride <= layout.AbiStubOff) {
		t.Errorf("stub-scratch (%#x) overlapt de hop-ABI-ringen (%#x + %#x)",
			layout.AbiStubOff, layout.AbiRingOff, layout.RingStride)
	}
	if layout.AbiMapOff <= layout.AbiStubOff {
		t.Errorf("map (%#x) ligt niet ná de stub-scratch (%#x)", layout.AbiMapOff, layout.AbiStubOff)
	}
	if layout.AbiMapOff >= layout.AbiNetOff {
		t.Errorf("map (%#x) loopt de frame-ringen in (%#x)", layout.AbiMapOff, layout.AbiNetOff)
	}
	// De stub leest zijn scratch als twee cachelines (stubScratchLen); de map
	// vraagt minstens twee pagina's (wortel + één niveau).
	if got := layout.AbiMapOff - layout.AbiStubOff; got < 0x1000 {
		t.Errorf("stub-scratch heeft %#x, wil minstens één pagina", got)
	}
	if got := layout.AbiNetOff - layout.AbiMapOff; got < 2*0x1000 {
		t.Errorf("map heeft %#x, wil minstens twee pagina's", got)
	}
}
