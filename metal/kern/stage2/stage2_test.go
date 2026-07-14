// Host-tests voor Build: het PA-plan wijst naar een heap-buffer, zodat de
// stage-2-tabellen in gewoon testgeheugen landen; een walker leest ze terug
// zoals de MMU dat zou doen. De overige plan-adressen zijn verzonnen PA's —
// Build schrijft ze alleen als descriptor-wáárde, dereferentie gebeurt nooit.
// Dít is de plek waar de isolatiebelofte een toetsbare eigenschap is: de map
// van slot i bevat geen enkel fysiek adres van slot j.
package stage2

import (
	"os"
	"testing"
	"unsafe"

	"hop-os/metal/abi/layout"
	"hop-os/metal/dev"
)

// Verzonnen PA's voor alles wat Build alleen als waarde encodeert.
const (
	tCtrlPA        = 0x1_0000_0000
	tRingPA        = 0x1_1000_0000
	tNetRingPA     = 0x1_2000_0000
	tBootScratchPA = 0x1_3000_0000
	tRevokeVecPA   = 0x1_4000_0000
	tPoolPA        = 0x2_0000_0000
)

// s2buf draagt de echte tabellen; package-var zodat de GC hem nooit opruimt
// terwijl layout er nog met een uintptr naar wijst.
var s2buf []byte

func TestMain(m *testing.M) {
	s2buf = make([]byte, (layout.MaxSlots+2)*layout.Stage2Stride)
	base := (uintptr(unsafe.Pointer(&s2buf[0])) + 0x7FF) &^ 0x7FF // VBAR-uitlijning
	layout.UsePlan(layout.Plan{
		CtrlPA:        tCtrlPA,
		RingPA:        tRingPA,
		Stage2PA:      uint64(base),
		RevokeVecPA:   tRevokeVecPA,
		BootScratchPA: tBootScratchPA,
		Pool:          []layout.Region{{Base: tPoolPA, Size: 1 << 30}},
	})
	// Net-ringen zijn geen plan-veld meer maar per-slot runtime-registraties
	// (partitie-staart, kern/slots). De tests bouwen kooien zonder slots.Start,
	// dus registreer hier per slot een eigen 2MB-blok op de oude testplek.
	for i := 1; i <= layout.MaxSlots; i++ {
		layout.SetNetRingPA(i, tNetRingPA+uint64(i-1)*layout.NetRingStride)
	}
	os.Exit(m.Run())
}

func rd(pa uint64) uint64 { return dev.Read64(uintptr(pa)) }

// paOf haalt het uitgangsadres uit een descriptor (OA-bits [47:12]).
func paOf(d uint64) uint64 { return d & 0x0000_FFFF_FFFF_F000 }

func TestBuildBereik(t *testing.T) {
	if _, err := Build(0, layout.SlotBase(1), tPoolPA, 2<<20); err == nil {
		t.Error("slot 0 geaccepteerd")
	}
	if _, err := Build(layout.MaxSlots+1, layout.SlotBase(1), tPoolPA, 2<<20); err == nil {
		t.Error("slot buiten MaxSlots geaccepteerd")
	}
}

// De volledige map van slot 1, descriptor voor descriptor.
func TestBuildSlot1(t *testing.T) {
	const size = 64 << 20 // 64MB-partitie
	ipa := layout.SlotBase(1)
	l1, err := Build(1, ipa, tPoolPA, size)
	if err != nil {
		t.Fatal(err)
	}
	base := uint64(layout.Stage2TablePA(1))
	if l1 != base+l1Off {
		t.Fatalf("VTTBR-adres %#x, verwacht %#x", l1, base+l1Off)
	}

	// L1: het slot-GB → L2part, het ctrl-GB → L2dev, verder niets.
	if got := rd(base + l1Off + (ipa>>30)*8); got != base+l2PartOff|descTable {
		t.Fatalf("L1[slot-GB] = %#x", got)
	}
	if got := rd(base + l1Off + 2*8); got != base+l2DevOff|descTable {
		t.Fatalf("L1[ctrl-GB] = %#x", got)
	}
	for _, idx := range []uint64{0, 3} {
		if got := rd(base + l1Off + idx*8); got != 0 {
			t.Fatalf("L1[%d] = %#x, hoort leeg", idx, got)
		}
	}

	// L2part: precies size/2MB blokken, IPA→partitie-PA, en verder leeg.
	first := (ipa - ipa&^((1<<30)-1)) >> 21
	for idx := uint64(0); idx < 512; idx++ {
		got := rd(base + l2PartOff + idx*8)
		if idx >= first && idx < first+size>>21 {
			want := tPoolPA + (idx-first)<<21 | blockRW
			if got != want {
				t.Fatalf("L2part[%d] = %#x, verwacht %#x", idx, got, want)
			}
		} else if got != 0 {
			t.Fatalf("L2part[%d] = %#x, hoort leeg", idx, got)
		}
	}

	// L2dev: ctrl-L3 op 384, ring-L3 op 392, eigen net-ring-blok op 408.
	for idx := uint64(0); idx < 512; idx++ {
		got := rd(base + l2DevOff + idx*8)
		var want uint64
		switch idx {
		case 384:
			want = base + l3CtrlOff | descTable
		case 392:
			want = base + l3RingOff | descTable
		case 408:
			want = uint64(layout.NetRingTXPA(1)) | blockRW
		}
		if got != want {
			t.Fatalf("L2dev[%d] = %#x, verwacht %#x", idx, got, want)
		}
	}

	// L3ctrl: boot-scratch read-only op page 0, eigen ctrl-page RW op page 1.
	if got := rd(base + l3CtrlOff); got != tBootScratchPA|pageRO {
		t.Fatalf("L3ctrl[0] = %#x (boot-scratch hoort RO)", got)
	}
	if got := rd(base + l3CtrlOff + 1*8); got != uint64(layout.CtrlPagePA(1))|pageRW {
		t.Fatalf("L3ctrl[1] = %#x", got)
	}
	for idx := uint64(2); idx < 512; idx++ {
		if got := rd(base + l3CtrlOff + idx*8); got != 0 {
			t.Fatalf("L3ctrl[%d] = %#x, hoort leeg (andermans ctrl-page?)", idx, got)
		}
	}

	// L3ring: de eigen 64KB ring-regio, pagina voor pagina.
	for idx := uint64(0); idx < 512; idx++ {
		got := rd(base + l3RingOff + idx*8)
		var want uint64
		if idx < layout.RingStride>>12 {
			want = uint64(layout.RingOutboxPA(1)) + idx<<12 | pageRW
		}
		if got != want {
			t.Fatalf("L3ring[%d] = %#x, verwacht %#x", idx, got, want)
		}
	}
}

// Slot 3 linkt op 0x90000000 — hetzelfde GB als de ctrl/ring-regio, dus de
// partitie deelt zijn L2 met de device-L3's. Op de maximale venstermaat
// (512MB) moet de hoogste partitie-index (383) nog vóór de eerste
// device-index (384) blijven — de "indexes botsen niet"-claim uit de code.
func TestBuildSlot3DeeltGBMetDevices(t *testing.T) {
	const size = 512 << 20
	ipa := layout.SlotBase(3)
	if ipa>>30 != layout.CtrlBase>>30 {
		t.Fatalf("testaanname stuk: SlotBase(3)=%#x ligt niet in het ctrl-GB", ipa)
	}
	if _, err := Build(3, ipa, tPoolPA, size); err != nil {
		t.Fatal(err)
	}
	base := uint64(layout.Stage2TablePA(3))

	// Beide L1-entries wijzen naar dezelfde L2 (dev), L2part blijft leeg.
	if got := rd(base + l1Off + 2*8); got != base+l2DevOff|descTable {
		t.Fatalf("L1[2] = %#x", got)
	}
	for idx := uint64(0); idx < 512; idx++ {
		if got := rd(base + l2PartOff + idx*8); got != 0 {
			t.Fatalf("L2part[%d] = %#x, hoort ongebruikt", idx, got)
		}
	}

	first := (ipa - layout.CtrlBase&^((1<<30)-1)) >> 21
	last := first + size>>21 - 1
	if last >= 384 {
		t.Fatalf("partitie-index %d botst met device-index 384", last)
	}
	for idx := uint64(0); idx < 512; idx++ {
		got := rd(base + l2DevOff + idx*8)
		var want uint64
		switch {
		case idx >= first && idx <= last:
			want = tPoolPA + (idx-first)<<21 | blockRW
		case idx == 384:
			want = base + l3CtrlOff | descTable
		case idx == 392:
			want = base + l3RingOff | descTable
		case idx == 408+3-1:
			want = uint64(layout.NetRingTXPA(3)) | blockRW
		}
		if got != want {
			t.Fatalf("L2dev[%d] = %#x, verwacht %#x", idx, got, want)
		}
	}
	// De eigen ctrl-page: index = slotnummer.
	if got := rd(base + l3CtrlOff + 3*8); got != uint64(layout.CtrlPagePA(3))|pageRW {
		t.Fatalf("L3ctrl[3] = %#x", got)
	}
}

type leaf struct {
	pa, size uint64
	ro       bool
}

// walk leest de tabellen van slot i terug zoals de MMU: L1 → L2 → L3, en
// verzamelt elk uitdeelbaar bereik (blok of pagina).
func walk(t *testing.T, i int) []leaf {
	t.Helper()
	var out []leaf
	seen := map[uint64]bool{}
	var walkTbl func(tbl uint64, level int)
	walkTbl = func(tbl uint64, level int) {
		if seen[tbl] {
			return
		}
		seen[tbl] = true
		for idx := uint64(0); idx < 512; idx++ {
			d := rd(tbl + idx*8)
			switch {
			case d == 0:
			case level < 3 && d&3 == descTable:
				walkTbl(paOf(d), level+1)
			case level == 2 && d&3 == descBlock:
				out = append(out, leaf{paOf(d), 2 << 20, d>>6&3 == 1})
			case level == 3 && d&3 == descPage:
				out = append(out, leaf{paOf(d), 4 << 10, d>>6&3 == 1})
			default:
				t.Fatalf("onverwachte descriptor %#x op level %d idx %d", d, level, idx)
			}
		}
	}
	walkTbl(uint64(layout.Stage2TablePA(i))+l1Off, 1)
	return out
}

// Dé isolatiebelofte, als eigenschap: bouw slot 1 en slot 2 naast elkaar en
// bewijs dat slot 1 geen byte van slot 2 kan raken — geen partitie, geen
// ctrl-page, geen ringen — en al helemaal niet de stage-2-tabellen zelf.
// Het enige toegestane gedeelde adres is de boot-scratch, en die is read-only.
func TestIsolatieTussenSlots(t *testing.T) {
	const size = 64 << 20
	pa1, pa2 := uint64(tPoolPA), uint64(tPoolPA+size)
	if _, err := Build(1, layout.SlotBase(1), pa1, size); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(2, layout.SlotBase(2), pa2, size); err != nil {
		t.Fatal(err)
	}

	vanAnder := []struct {
		naam       string
		base, size uint64
	}{
		{"partitie", pa2, size},
		{"ctrl-page", uint64(layout.CtrlPagePA(2)), layout.CtrlStride},
		{"hop-ringen", uint64(layout.RingOutboxPA(2)), layout.RingStride},
		{"net-ringen", uint64(layout.NetRingTXPA(2)), layout.NetRingStride},
		{"stage-2-tabellen", uint64(layout.Stage2TablePA(0)), uint64(layout.MaxSlots+1) * layout.Stage2Stride},
	}
	roGezien := 0
	for _, l := range walk(t, 1) {
		for _, r := range vanAnder {
			if l.pa < r.base+r.size && r.base < l.pa+l.size {
				t.Errorf("slot 1 mapt %s van slot 2/HOP: PA %#x (+%#x)", r.naam, l.pa, l.size)
			}
		}
		if l.ro {
			roGezien++
			if l.pa != tBootScratchPA {
				t.Errorf("onverwacht read-only bereik op %#x", l.pa)
			}
		} else if l.pa == tBootScratchPA {
			t.Error("boot-scratch staat RW in de map")
		}
	}
	if roGezien != 1 {
		t.Errorf("%d read-only entries, verwacht precies 1 (de boot-scratch)", roGezien)
	}
}

// Een re-Start met een kleinere partitie mag niets van de oude, grotere map
// laten staan (Build hoort eerst te vegen).
func TestRebuildVeegtOudeMap(t *testing.T) {
	ipa := layout.SlotBase(1)
	if _, err := Build(1, ipa, tPoolPA, 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(1, ipa, tPoolPA, 8<<20); err != nil {
		t.Fatal(err)
	}
	blokken := 0
	for _, l := range walk(t, 1) {
		if l.pa >= tPoolPA && l.pa < tPoolPA+(1<<30) {
			blokken++
		}
	}
	if blokken != 4 {
		t.Fatalf("%d partitie-blokken na rebuild naar 8MB, verwacht 4", blokken)
	}
}
