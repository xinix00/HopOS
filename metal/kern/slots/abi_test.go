package slots

import (
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// Het control-blok uit de systeempot draagt control page, bootstrap-ringen,
// kooi-scratch en maptabellen zonder overlap.
func TestControlBlockIndelingPastEnOverlaptNiet(t *testing.T) {
	type span struct {
		naam       string
		start, end uint64
	}
	spans := []span{
		{"control page", layout.AbiCtrlOff, layout.AbiCtrlOff + layout.CtrlStride},
		{"hop-ABI-ringen", layout.AbiRingOff, layout.AbiRingOff + layout.RingStride},
		{"kooi-stub", layout.AbiStubOff, layout.AbiStubOff + 0x1000},
		{"kooi-map", layout.AbiMapOff, layout.SlotControlStride},
	}
	for i, s := range spans {
		if s.end > layout.SlotControlStride {
			t.Errorf("%s loopt tot %#x, voorbij control-blok %#x", s.naam, s.end, uint64(layout.SlotControlStride))
		}
		for _, o := range spans[i+1:] {
			if s.start < o.end && o.start < s.end {
				t.Errorf("%s [%#x,%#x) overlapt %s [%#x,%#x)", s.naam, s.start, s.end, o.naam, o.start, o.end)
			}
		}
	}
}

// Alle control- en NIC-vensters blijven in hun eigen GB en overlappen niet.
func TestVasteBufferVenstersBlijvenBinnenHunGB(t *testing.T) {
	ctrlGBEnd := uint64(layout.CtrlBase) + 1<<30
	netGBEnd := uint64(layout.NetRingBase) + 1<<30
	for i := 1; i <= layout.SlotCap; i++ {
		ctrl := uint64(layout.SlotControl(i))
		tx, rx := uint64(layout.NetRingTX(i)), uint64(layout.NetRingRX(i))
		if ctrl < layout.CtrlBase || ctrl+layout.SlotControlStride > ctrlGBEnd {
			t.Fatalf("slot %d: controlvenster [%#x,%#x) buiten ctrl-GB", i, ctrl, ctrl+layout.SlotControlStride)
		}
		if rx-tx != layout.NetRingWindowHalf {
			t.Fatalf("slot %d: TX/RX-afstand %#x", i, rx-tx)
		}
		if tx < layout.NetRingBase || rx+layout.NetRingWindowHalf > netGBEnd {
			t.Fatalf("slot %d: netvenster [%#x,%#x) buiten net-GB", i, tx, rx+layout.NetRingWindowHalf)
		}
		if i < layout.SlotCap {
			if ctrl+layout.SlotControlStride > uint64(layout.SlotControl(i+1)) {
				t.Fatalf("control slot %d overlapt slot %d", i, i+1)
			}
			if rx+layout.NetRingWindowHalf > uint64(layout.NetRingTX(i+1)) {
				t.Fatalf("net slot %d overlapt slot %d", i, i+1)
			}
		}
	}
}

func TestBufferAdressenZijnEenBronVanWaarheid(t *testing.T) {
	const slot = 7
	ctrl := uint64(layout.SlotControl(slot))
	if want := uint64(layout.SlotControlBase + (slot-1)*layout.SlotControlStride); ctrl != want {
		t.Errorf("slot 7 control = %#x, verwacht %#x", ctrl, want)
	}
	if got, want := uint64(layout.RingOutbox(slot)), ctrl+layout.AbiRingOff+layout.OutboxOff; got != want {
		t.Errorf("slot 7 outbox = %#x, verwacht %#x", got, want)
	}
	if got, want := uint64(layout.RingInbox(slot)), ctrl+layout.AbiRingOff+layout.InboxOff; got != want {
		t.Errorf("slot 7 inbox = %#x, verwacht %#x", got, want)
	}
	if got, want := uint64(layout.NetRingTX(slot)), uint64(layout.NetRingBase+(slot-1)*layout.NetRingStride); got != want {
		t.Errorf("slot 7 net TX = %#x, verwacht %#x", got, want)
	}
	if got, want := uint64(layout.NetRingRX(slot)), uint64(layout.NetRingBase+(slot-1)*layout.NetRingStride+layout.NetRXOff); got != want {
		t.Errorf("slot 7 net RX = %#x, verwacht %#x", got, want)
	}
}

func TestControlBlockHeeftRuimteVoorStubEnMap(t *testing.T) {
	if !(layout.AbiRingOff+layout.RingStride <= layout.AbiStubOff) {
		t.Errorf("stub-scratch (%#x) overlapt de hop-ABI-ringen", layout.AbiStubOff)
	}
	if layout.AbiMapOff <= layout.AbiStubOff {
		t.Errorf("map (%#x) ligt niet na scratch (%#x)", layout.AbiMapOff, layout.AbiStubOff)
	}
	if got := layout.SlotControlStride - layout.AbiMapOff; got < 6*0x1000 {
		t.Errorf("map heeft %#x, wil minstens zes pagina's", got)
	}
}

func TestAppRAMIsVolledigePartitie(t *testing.T) {
	const size = 512 << 20
	got, err := appRAMSize(size)
	if err != nil {
		t.Fatal(err)
	}
	if got != size {
		t.Fatalf("app-RAM %#x, partitie %#x", got, uint64(size))
	}
}
