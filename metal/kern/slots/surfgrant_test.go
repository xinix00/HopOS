//go:build gui

package slots

import (
	"testing"
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
)

// De surface-grant is de enige plek waar een kooi in het RAM van een andere
// kooi mag kijken. Deze tests gaan dus niet over "werkt het" maar over de
// grenzen: wat een app NIET mag verlenen, en dat een grant echt weg is als hij
// weg hoort te zijn. Een gat hier is niet een verkeerd venster maar de partitie
// van een willekeurige andere job op het scherm.

// surfTestS2 draagt de stage-2-tabellen die MapSurface echt beschrijft;
// package-var zodat de GC hem niet opruimt terwijl layout er met een uintptr
// naar wijst.
var surfTestS2 []byte

const (
	surfTestPoolPA = 0x4000_0000
	// Ruim: één test vult het hele SurfIPA-GB en heeft daarvoor partities nodig
	// die groter zijn dan een GB aan surfaces.
	surfTestPoolLen = 4 << 30
	surfDisplaySlot = 1
	surfAppSlot     = 2
)

// surfSetup zet een plan met echte tabellen, een verse pool en een display die
// de framebuffer houdt. Geeft de partitie van de app-slot terug.
func surfSetup(t *testing.T) (base, size uint64) {
	t.Helper()
	if surfTestS2 == nil {
		surfTestS2 = make([]byte, (layout.SlotCap+2)*layout.Stage2Stride)
	}
	s2 := (uintptr(unsafe.Pointer(&surfTestS2[0])) + 0x7FF) &^ 0x7FF
	layout.UsePlan(layout.Plan{
		NodeCtrlPA:    0x1_0000_0000,
		Stage2PA:      uint64(s2),
		RevokeVecPA:   0x1_4000_0000,
		BootScratchPA: 0x1_3000_0000,
		Pool:          []layout.Region{{Base: surfTestPoolPA, Size: surfTestPoolLen}},
	})
	poolReset(t, []layout.Region{{Base: surfTestPoolPA, Size: surfTestPoolLen}})

	// De display houdt het glas — dat is de enige ontvanger die bestaat.
	RegisterGrant(GrantHooks{Holder: func() int { return surfDisplaySlot }})
	t.Cleanup(func() {
		RegisterGrant(GrantHooks{})
		surfMu.Lock()
		surfResetLocked()
		surfMu.Unlock()
	})
	surfMu.Lock()
	surfResetLocked()
	surfMu.Unlock()

	base, size, err := partAlloc(surfAppSlot, 64<<20)
	if err != nil {
		t.Fatalf("partitie voor de app: %v", err)
	}
	return base, size
}

func TestSurfaceGrantGeeftDisplayEenIPA(t *testing.T) {
	surfSetup(t)
	ipa, err := SurfaceGrant(surfAppSlot, 0, 4*layout.SurfBlock)
	if err != nil {
		t.Fatal(err)
	}
	if ipa < uint64(layout.SurfIPA) || ipa >= uint64(layout.SurfIPA)+(1<<30) {
		t.Errorf("IPA %#x valt buiten het SurfIPA-GB", ipa)
	}
	if ipa%layout.SurfBlock != 0 {
		t.Errorf("IPA %#x is niet blok-uitgelijnd", ipa)
	}
}

func TestSurfaceGrantWeigertBuitenHetEigenRAM(t *testing.T) {
	_, size := surfSetup(t)
	appRAM, err := appRAMSize(size)
	if err != nil {
		t.Fatal(err)
	}

	// Voorbij het eigen app-RAM: dit is DE aanval. Lukt dit, dan verleent een
	// app zicht op de partitie van zijn buurman, of op zijn eigen ABI-staart
	// (control-page en ringen) — waar HOP's kant van het contract in staat.
	if _, err := SurfaceGrant(surfAppSlot, appRAM, layout.SurfBlock); err == nil {
		t.Error("venster voorbij het app-RAM geaccepteerd")
	}
	if _, err := SurfaceGrant(surfAppSlot, appRAM-layout.SurfBlock, 2*layout.SurfBlock); err == nil {
		t.Error("venster dat over de RAM-grens heen steekt geaccepteerd")
	}
	// De ABI-staart ligt tussen appRAM en de partitiegrens: expliciet dicht.
	if _, err := SurfaceGrant(surfAppSlot, appRAM, size-appRAM); err == nil {
		t.Error("de ABI-staart (ctrl-page + ringen) kon verleend worden")
	}
	// Overflow: off+n wrapt en zou zonder de check binnen de grens 'passen'.
	if _, err := SurfaceGrant(surfAppSlot, ^uint64(0)&^(layout.SurfBlock-1), 2*layout.SurfBlock); err == nil {
		t.Error("overflowend venster geaccepteerd")
	}
}

func TestSurfaceGrantEistBlokUitlijning(t *testing.T) {
	surfSetup(t)
	// Een niet-uitgelijnd venster zou als 2MB-blok naar beneden afronden en de
	// display bytes vóór de buffer laten zien.
	if _, err := SurfaceGrant(surfAppSlot, 0x1000, layout.SurfBlock); err == nil {
		t.Error("niet-uitgelijnde offset geaccepteerd")
	}
	if _, err := SurfaceGrant(surfAppSlot, 0, layout.SurfBlock+0x1000); err == nil {
		t.Error("niet-uitgelijnde lengte geaccepteerd")
	}
	if _, err := SurfaceGrant(surfAppSlot, 0, 0); err == nil {
		t.Error("leeg venster geaccepteerd")
	}
}

func TestSurfaceGrantZonderDisplayWeigert(t *testing.T) {
	surfSetup(t)
	RegisterGrant(GrantHooks{}) // geen provider = kale build, geen houder
	if _, err := SurfaceGrant(surfAppSlot, 0, layout.SurfBlock); err == nil {
		t.Error("grant zonder display-houder geaccepteerd")
	}
}

func TestSurfaceGrantAanZichzelfWeigert(t *testing.T) {
	surfSetup(t)
	if _, _, err := partAlloc(surfDisplaySlot, 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := SurfaceGrant(surfDisplaySlot, 0, layout.SurfBlock); err == nil {
		t.Error("de display kon aan zichzelf verlenen")
	}
}

func TestSurfaceHergrantHoudtMaarEenVensterBezet(t *testing.T) {
	// Een app die zijn venster laat groeien doet keer op keer een nieuwe grant.
	// Blijft de oude bezet staan, dan loopt het GB vol en stopt de desktop met
	// vensters openen — een lek dat pas na een uur opvalt.
	surfSetup(t)
	for range 20 {
		if _, err := SurfaceGrant(surfAppSlot, 0, 5*layout.SurfBlock); err != nil {
			t.Fatalf("hergrant faalde: %v", err)
		}
	}
	if used, _ := surfCount(); used != 5 {
		t.Errorf("%d blokken in gebruik na 20 hergrants, wil 5", used)
	}
}

func TestSurfaceRevokeZetBlokkenInQuarantaine(t *testing.T) {
	// Niet meteen vrij: de display kan zijn slice nog vasthebben tot hij de
	// dode sessie opruimt. Pas ná de quarantaine mag een blok naar een andere
	// app — anders leest de display met een oude slice in vers geheugen.
	surfSetup(t)
	if _, err := SurfaceGrant(surfAppSlot, 0, 3*layout.SurfBlock); err != nil {
		t.Fatal(err)
	}
	SurfaceRevoke(surfAppSlot)

	used, quar := surfCount()
	if used != 0 {
		t.Errorf("%d blokken nog bezet na revoke", used)
	}
	if quar != 3 {
		t.Errorf("%d blokken in quarantaine, wil 3", quar)
	}
	surfMu.Lock()
	if surfOf[surfAppSlot].blocks != 0 {
		t.Error("de administratie van het slot staat er nog")
	}
	// De quarantaine terugdraaien in de tijd: dan horen ze weer uitdeelbaar.
	for b := range surfFreeAt {
		if !surfFreeAt[b].IsZero() {
			surfFreeAt[b] = time.Now().Add(-time.Second)
		}
	}
	surfMu.Unlock()
	if _, quar := surfCount(); quar != 0 {
		t.Errorf("%d blokken nog in quarantaine nadat de termijn verliep", quar)
	}
	if _, err := SurfaceGrant(surfAppSlot, 0, 3*layout.SurfBlock); err != nil {
		t.Errorf("blokken niet herbruikbaar na de quarantaine: %v", err)
	}
}

func TestSurfaceRevokeLaatGeenGatMaarNullen(t *testing.T) {
	// DE anti-crash-invariant. Een ingetrokken blok moet naar de nulregio
	// wijzen, niet naar niets: de display kan de slice nog lezen en een genulde
	// descriptor maakt daar een stage-2-abort van — dan neemt een gecrashte app
	// de hele desktop mee.
	surfSetup(t)
	ipa, err := SurfaceGrant(surfAppSlot, 0, 2*layout.SurfBlock)
	if err != nil {
		t.Fatal(err)
	}
	blk := int((ipa - uint64(layout.SurfIPA)) / layout.SurfBlock)
	SurfaceRevoke(surfAppSlot)

	l2 := uint64(layout.Stage2TablePA(surfDisplaySlot)) + 0xA000
	for n := range 2 {
		d := dev.Read64(uintptr(l2 + uint64(blk+n)*8))
		if d == 0 {
			t.Fatalf("blok %d is ontmapt — een late lezing van de display faultt nu", blk+n)
		}
		if pa := d & 0x0000_FFFF_FFFF_F000; pa != surfZeroPA {
			t.Errorf("blok %d wijst naar %#x, wil de nulregio %#x", blk+n, pa, surfZeroPA)
		}
		if ap := d & (0x3 << 6); ap != 0x1<<6 {
			t.Errorf("blok %d is niet read-only (S2AP %#x)", blk+n, ap>>6)
		}
	}
}

// surfCount telt bezette en in-quarantaine-staande blokken.
func surfCount() (used, quarantined int) {
	surfMu.Lock()
	defer surfMu.Unlock()
	now := time.Now()
	for b := range layout.SurfBlocks {
		switch {
		case surfUsed[b]:
			used++
		case now.Before(surfFreeAt[b]):
			quarantined++
		}
	}
	return used, quarantined
}

func TestSurfaceRevokeIsIdempotent(t *testing.T) {
	// releaseSlot roept dit voor ÉLK slot aan, ook slots die nooit een surface
	// hadden. Dat mag geen paniek of dubbele vrijgave geven.
	surfSetup(t)
	SurfaceRevoke(surfAppSlot)
	SurfaceRevoke(surfAppSlot)
	SurfaceRevoke(0)
	SurfaceRevoke(layout.SlotCap + 5)
}

func TestSurfaceHolderGoneMaaktDeBitmapLeeg(t *testing.T) {
	// De display herstart: zijn stage-2 wordt bij Build toch schoongeveegd,
	// maar de bitmap moet mee — anders raakt het GB na een paar herstarts vol
	// met blokken die niemand houdt.
	surfSetup(t)
	if _, err := SurfaceGrant(surfAppSlot, 0, 6*layout.SurfBlock); err != nil {
		t.Fatal(err)
	}
	SurfaceHolderGone(surfDisplaySlot)
	surfMu.Lock()
	defer surfMu.Unlock()
	for b, on := range surfUsed {
		if on {
			t.Fatalf("blok %d nog bezet nadat de display verdween", b)
		}
	}
}

func TestSurfaceGrantVolGBWeigertNetjes(t *testing.T) {
	// Op is op: een node met heel veel vensters hoort een fout te krijgen, niet
	// een grant die over de GB-grens heen in de buurtabellen schrijft.
	//
	// Partities van 600MB, want de app-RAM-grens hoort hier NIET te vuren — we
	// toetsen de blok-bitmap, en een test die per ongeluk op de vorige check
	// afketst bewijst deze niet.
	surfSetup(t)
	half := layout.SurfBlocks / 2
	if _, _, err := partAlloc(4, 600<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := SurfaceGrant(4, 0, uint64(half)*layout.SurfBlock); err != nil {
		t.Fatal(err)
	}
	// Tweede app vraagt meer dan er over is.
	if _, _, err := partAlloc(5, 600<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := SurfaceGrant(5, 0, uint64(half+1)*layout.SurfBlock); err == nil {
		t.Error("grant groter dan de vrije ruimte geaccepteerd")
	}
}
