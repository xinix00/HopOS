//go:build gui

package stage2

import (
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// De surface-grant geeft de display leestoegang tot het RAM van een ándere
// app. Dat is de enige plek in HopOS waar een kooi iets van een andere kooi
// mag zien, dus staan hier niet de "werkt het"-tests maar de invarianten:
// read-only, precies de toegezegde blokken, en écht weg na intrekking.

// surfDesc geeft de rauwe L2-descriptor voor blok blk in de kooi van slot.
func surfDesc(slot, blk int) uint64 {
	l2 := uint64(layout.Stage2TablePA(slot)) + l2SurfOff
	return rd(l2 + uint64(blk)*8)
}

// surfCheck ontleedt een descriptor en faalt op alles wat niet de bedoeling is.
func surfCheck(t *testing.T, slot, blk int) uint64 {
	t.Helper()
	d := surfDesc(slot, blk)
	if d == 0 {
		return 0
	}
	if d&0x3 != descBlock {
		t.Fatalf("blok %d: descriptor %#x is geen 2MB-blok", blk, d)
	}
	if d&attrAF == 0 {
		t.Fatalf("blok %d: geen access flag", blk)
	}
	// DE invariant. S2AP=0b01 is read-only; 0b11 zou de display schrijfrecht
	// geven in de partitie van een andere app.
	if ap := d & (0x3 << 6); ap != attrRO {
		t.Fatalf("blok %d: S2AP %#x, wil read-only (%#x) — display mag NOOIT in het RAM van een app schrijven",
			blk, ap>>6, uint64(attrRO)>>6)
	}
	if a := (d >> 2) & 0xF; a != 0xF {
		t.Fatalf("blok %d: MemAttr %#x, wil Normal cacheable (0xF) — de lezer is een CPU-core, geen scanout", blk, a)
	}
	return paOf(d)
}

func TestMapSurfaceIsReadOnlyEnPreciesGroot(t *testing.T) {
	const (
		slot   = 5
		blk    = 12
		blocks = 5 // 10MB: een 1920x1080x32-venster (8,29MB) afgerond op 2MB
	)
	if _, err := Build(slot, layout.SlotBase(1), tPoolPA, 4<<20); err != nil {
		t.Fatal(err)
	}
	pa := uint64(tPoolPA) + 64<<20 // de partitie van een andere app
	if err := MapSurface(slot, blk, pa, blocks); err != nil {
		t.Fatal(err)
	}
	for n := range blocks {
		got := surfCheck(t, slot, blk+n)
		want := pa + uint64(n)*layout.SurfBlock
		if got != want {
			t.Errorf("blok %d wijst naar %#x, wil %#x", blk+n, got, want)
		}
	}
	// Geen byte meer dan toegezegd: de buren van het venster blijven leeg.
	// Zonder deze grens zou een app door een te ruime grant het geheugen van
	// zijn buren aan de display kunnen laten zien.
	if d := surfDesc(slot, blk-1); d != 0 {
		t.Errorf("blok vóór het venster is gemapt (%#x)", d)
	}
	if d := surfDesc(slot, blk+blocks); d != 0 {
		t.Errorf("blok ná het venster is gemapt (%#x)", d)
	}
}

func TestUnmapSurfaceLaatNietsStaan(t *testing.T) {
	// Het pad dat ertoe doet: blijft er één descriptor staan nadat de app weg
	// is, dan leest de display straks de partitie van de vólgende job.
	const (
		slot   = 6
		blk    = 0
		blocks = 3
	)
	if _, err := Build(slot, layout.SlotBase(1), tPoolPA, 4<<20); err != nil {
		t.Fatal(err)
	}
	pa := uint64(tPoolPA) + 128<<20
	if err := MapSurface(slot, blk, pa, blocks); err != nil {
		t.Fatal(err)
	}
	if err := UnmapSurface(slot, blk, blocks); err != nil {
		t.Fatal(err)
	}
	for n := range blocks {
		if d := surfDesc(slot, blk+n); d != 0 {
			t.Errorf("blok %d staat na intrekking nog op %#x", blk+n, d)
		}
	}
}

func TestMapSurfaceWeigertWatNietUitgelijndOfBuitenBereikIs(t *testing.T) {
	const slot = 8
	if _, err := Build(slot, layout.SlotBase(1), tPoolPA, 4<<20); err != nil {
		t.Fatal(err)
	}
	pa := uint64(tPoolPA) + 32<<20

	// Niet-2MB-uitgelijnde PA: zou de blok-descriptor stilzwijgend naar
	// beneden afronden en de app een ander venster geven dan hij vroeg —
	// inclusief de bytes vóór zijn buffer.
	if err := MapSurface(slot, 0, pa+0x1000, 1); err == nil {
		t.Error("niet-uitgelijnde PA geaccepteerd")
	}
	if err := MapSurface(slot, 0, pa, 0); err == nil {
		t.Error("nul blokken geaccepteerd")
	}
	if err := MapSurface(slot, -1, pa, 1); err == nil {
		t.Error("negatieve blokindex geaccepteerd")
	}
	// Voorbij het GB: de lus zou in de buurtabellen van dit stage-2-blok
	// schrijven — dezelfde soort fout als de slot-105-les in Build.
	if err := MapSurface(slot, layout.SurfBlocks-1, pa, 2); err == nil {
		t.Error("venster over de GB-grens geaccepteerd")
	}
	if err := MapSurface(0, 0, pa, 1); err == nil {
		t.Error("slot 0 geaccepteerd")
	}
}

func TestMapSurfaceRaaktDeAndereVenstersNiet(t *testing.T) {
	// Alle surfaces delen één L2. Een tweede grant mag de eerste dus niet
	// wissen — anders verdwijnt het venster van app A zodra app B er een opent.
	const slot = 9
	if _, err := Build(slot, layout.SlotBase(1), tPoolPA, 4<<20); err != nil {
		t.Fatal(err)
	}
	paA := uint64(tPoolPA) + 16<<20
	paB := uint64(tPoolPA) + 256<<20
	if err := MapSurface(slot, 2, paA, 2); err != nil {
		t.Fatal(err)
	}
	if err := MapSurface(slot, 40, paB, 5); err != nil {
		t.Fatal(err)
	}
	if got := surfCheck(t, slot, 2); got != paA {
		t.Errorf("venster A is weg na de grant van B (blok 2 = %#x)", got)
	}
	// En intrekken van B laat A staan.
	if err := UnmapSurface(slot, 40, 5); err != nil {
		t.Fatal(err)
	}
	if got := surfCheck(t, slot, 2); got != paA {
		t.Errorf("venster A verdween bij het intrekken van B (blok 2 = %#x)", got)
	}
}

func TestSurfaceGBBotstNietMetHetCanoniekeBeeld(t *testing.T) {
	// SurfIPA moet in een GB liggen dat Build niet zelf al vult, anders
	// overschrijft de grant de partitie of de ctrl-regio van de display.
	gbSurf := uint64(layout.SurfIPA) >> 30
	for _, other := range []struct {
		name string
		ipa  uint64
	}{
		{"FbIPA", uint64(layout.FbIPA)},
		{"CtrlBase", uint64(layout.CtrlBase)},
		{"partitie (SlotBase(1))", layout.SlotBase(1)},
	} {
		if other.ipa>>30 == gbSurf {
			t.Errorf("SurfIPA (%#x) deelt GB %d met %s (%#x)", layout.SurfIPA, gbSurf, other.name, other.ipa)
		}
	}
}
