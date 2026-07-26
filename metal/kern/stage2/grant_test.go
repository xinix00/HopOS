package stage2

import (
	"testing"

	"hop-os/metal/abi/layout"
)

// tFbPA: een verzonnen framebuffer in de firmware-carve (GB 0, buiten het
// IPA-beeld van partitie/ctrl/net) — bewust niet 2MB-aligned, zoals een
// echte mailbox-framebuffer.
const tFbPA = 0x3E10_8000

// grantMapped geeft de fysieke pagina waarop de kooi van slot de fysieke
// pagina p afbeeldt (0 = niet gemapt), en controleert onderweg dat elke
// gebruikte descriptor geldig en Normal-NC is. Het loopt de tabellen af zoals
// de walker: L2 → blok, of L2 → L3 → pagina (de randen van het venster).
func grantMapped(t *testing.T, slot int, lo, p uint64) uint64 {
	t.Helper()
	base := uint64(layout.Stage2TablePA(slot))
	gbBase := (uint64(layout.FbIPA) >> 30) << 30
	ipa := uint64(layout.FbIPA) + (p - lo)
	e := rd(base + l2FbOff + ((ipa-gbBase)>>21)*8)
	switch e & 0x3 {
	case 0:
		return 0
	case descBlock:
		if e&attrAF == 0 {
			t.Fatalf("IPA %#x: blok %#x zonder AF", ipa, e)
		}
		if a := (e >> 2) & 0xF; a != 0x5 {
			t.Fatalf("IPA %#x: MemAttr %#x, wil Normal-NC (0x5)", ipa, a)
		}
		return paOf(e) + (ipa & ((2 << 20) - 1))
	default: // tabel → de rand-L3
		pe := rd(paOf(e) + ((ipa&((2<<20)-1))>>12)*8)
		if pe == 0 {
			return 0
		}
		if pe&0x3 != descPage || pe&attrAF == 0 {
			t.Fatalf("IPA %#x: pagina-descriptor %#x ongeldig", ipa, pe)
		}
		if a := (pe >> 2) & 0xF; a != 0x5 {
			t.Fatalf("IPA %#x: MemAttr %#x, wil Normal-NC (0x5)", ipa, a)
		}
		return paOf(pe) + (ipa & 0xFFF)
	}
}

// TestGrantWindow: de framebuffer komt Normal-NC in de kooi op FbIPA, élke
// pagina van de buffer is bereikbaar — en geen pagina daarbuiten. Dat laatste
// is de kern: met alleen 2MB-blokken kreeg de grant-houder tot ~4MB
// firmware-geheugen róndom de buffer erbij (RW, en FB_BASE verbergt dat niet).
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
	fbGB := uint64(layout.FbIPA) >> 30

	// L1 van het FbIPA-GB → de fb-L2.
	l1e := rd(base + l1Off + fbGB*8)
	if paOf(l1e) != base+l2FbOff || l1e&0x3 != descTable {
		t.Fatalf("L1[FbIPA-GB] = %#x, wil fb-L2-tabel", l1e)
	}

	// Elke pagina van de buffer is identity bereikbaar (blok of rand-L3).
	pgLo := uint64(tFbPA) &^ 0xFFF
	pgHi := (uint64(tFbPA) + size + 0xFFF) &^ 0xFFF
	for p := pgLo; p < pgHi; p += 0x1000 {
		if got := grantMapped(t, slot, lo, p); got != p {
			t.Fatalf("pagina %#x: kooi mapt naar %#x, wil identity", p, got)
		}
	}
	// En niets daarbuiten — de pagina's vóór en ná de buffer binnen dezelfde
	// 2MB-blokken horen bij de firmware en blijven ongemapt.
	if got := grantMapped(t, slot, lo, pgLo-0x1000); got != 0 {
		t.Fatalf("pagina %#x vóór de buffer is gemapt (naar %#x) — overmapping", pgLo-0x1000, got)
	}
	if got := grantMapped(t, slot, lo, pgHi); got != 0 {
		t.Fatalf("pagina %#x ná de buffer is gemapt (naar %#x) — overmapping", pgHi, got)
	}

	// Idempotent op hetzelfde slot — en een fb boven de 4GB (de QEMU-ramfb-
	// vondst) moet gewoon werken: het venster is de vertaling.
	if err := GrantWindow(slot, tFbPA, size); err != nil {
		t.Fatalf("her-grant: %v", err)
	}
	const highFb = 0x1_BC7A_0000
	if err := GrantWindow(slot, highFb, size); err != nil {
		t.Fatalf("fb boven 4GB: %v", err)
	}
	hLo := uint64(highFb) &^ ((2 << 20) - 1)
	if got := grantMapped(t, slot, hLo, highFb); got != highFb {
		t.Fatalf("hoge fb: eerste pagina mapt naar %#x, wil %#x", got, uint64(highFb))
	}
	if got := grantMapped(t, slot, hLo, uint64(highFb)-0x1000); got != 0 {
		t.Fatalf("hoge fb: pagina vóór de buffer gemapt (naar %#x)", got)
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
