package stage2

import (
	"testing"

	"hop-os/metal/abi/layout"
)

// tFbPA: een verzonnen framebuffer in de firmware-carve (GB 0, buiten het
// IPA-beeld van partitie/ctrl/net) — bewust niet 2MB-aligned, zoals een
// echte mailbox-framebuffer.
const tFbPA = 0x3E10_8000

// TestGrantWindow: het venster komt identity en Normal-NC in de kooi, de
// randen liggen op 2MB, en de fout-paden weigeren wat ze moeten weigeren.
func TestGrantWindow(t *testing.T) {
	const slot = 7
	if _, err := Build(slot, layout.SlotBase(1), tPoolPA, 4<<20, tNetPA(slot)); err != nil {
		t.Fatal(err)
	}
	size := uint64(1920*4*1080) - 3 // ~8MB, expres geen mooi getal
	if err := GrantWindow(slot, tFbPA, size); err != nil {
		t.Fatal(err)
	}

	base := uint64(layout.Stage2TablePA(slot))
	lo := uint64(tFbPA) &^ ((2 << 20) - 1)
	hi := (uint64(tFbPA) + size + (2 << 20) - 1) &^ ((2 << 20) - 1)
	fbGB := uint64(layout.FbIPA) >> 30
	gbBase := fbGB << 30

	// L1 van het FbIPA-GB → de fb-L2.
	l1e := rd(base + l1Off + fbGB*8)
	if paOf(l1e) != base+l2FbOff || l1e&0x3 != descTable {
		t.Fatalf("L1[FbIPA-GB] = %#x, wil fb-L2-tabel", l1e)
	}
	// Elke 2MB van het venster: IPA op FbIPA+offset → PA van de fb, blok,
	// AF én Normal-NC.
	for off := lo; off < hi; off += 2 << 20 {
		ipa := uint64(layout.FbIPA) + (off - lo)
		e := rd(base + l2FbOff + ((ipa-gbBase)>>21)*8)
		if paOf(e) != off {
			t.Fatalf("IPA %#x: PA %#x, wil fb-blok %#x", ipa, paOf(e), off)
		}
		if e&0x3 != descBlock || e&attrAF == 0 {
			t.Fatalf("IPA %#x: descriptor %#x geen geldig blok", ipa, e)
		}
		if memAttr := (e >> 2) & 0xF; memAttr != 0x5 {
			t.Fatalf("IPA %#x: MemAttr %#x, wil Normal-NC (0x5)", ipa, memAttr)
		}
	}
	// Buiten het venster: leeg (geen byte meer gemapt dan de grant).
	endIPA := uint64(layout.FbIPA) + (hi - lo)
	if e := rd(base + l2FbOff + ((endIPA-gbBase)>>21)*8); e != 0 {
		t.Fatalf("blok ná het venster gemapt: %#x", e)
	}

	// Idempotent op hetzelfde slot — en een fb boven de 4GB (de QEMU-ramfb-
	// vondst) moet gewoon werken: het venster is de vertaling.
	if err := GrantWindow(slot, tFbPA, size); err != nil {
		t.Fatalf("her-grant: %v", err)
	}
	if err := GrantWindow(slot, 0x1_BC7A_0000, size); err != nil {
		t.Fatalf("fb boven 4GB: %v", err)
	}
	if e := rd(base + l2FbOff + ((uint64(layout.FbIPA)-gbBase)>>21)*8); paOf(e) != 0x1_BC60_0000 {
		t.Fatalf("hoge fb: eerste blok %#x, wil 0x1bc600000", paOf(e))
	}

	// De kooi van een ánder slot blijft leeg (isolatie: de grant is per slot).
	const other = 8
	if _, err := Build(other, layout.SlotBase(1), tPoolPA+(64<<20), 4<<20, tNetPA(other)); err != nil {
		t.Fatal(err)
	}
	if e := rd(uint64(layout.Stage2TablePA(other)) + l1Off + fbGB*8); e != 0 {
		t.Fatalf("slot %d kreeg óók een fb-GB: %#x", other, e)
	}
}
