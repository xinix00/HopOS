//go:build gui

// surfgrant.go — de surface-grant (docs/archief/gui-ontwerp.md, fase P3): een
// GUI-app laat de display-houder READ-ONLY in zijn vensterbuffer kijken.
//
// Waarom dit in kern/slots hoort en niet in gui/: dit is het énige mechanisme
// waarmee een kooi iets van een andere kooi te zien krijgt. Wie mag verlenen,
// wat er verleend mag worden en wanneer het weer weg moet, is kern-beleid — de
// kant die weet welk slot de display is, blijft beleid en komt via een hook
// (GrantHooks.Holder) binnen.
//
// Wat het vervangt: de app stuurde elke frame zijn pixels over TCP naar de
// display, die ze in een eigen back/front-buffer overnam. Elke pixel stond dus
// twee keer in DRAM. Gemeten op de Radxa 06-08: zes vensters brachten de
// display op 78 MB van zijn 96 en toen viel hij om. Dat schaalt met het aantal
// vensters — een display die alleen leest, schaalt niet mee.
//
// De drie regels waar dit mechanisme op staat:
//
//  1. je verleent alleen over je EIGEN RAM (offset+lengte binnen je partitie,
//     en binnen het app-deel ervan — niet je ABI-staart met ctrl-page en ringen);
//  2. je verleent alleen aan de HOUDER van de device-grant, wie dat ook is. Een
//     app kiest de ontvanger niet;
//  3. de grant is read-only en verdwijnt als je slot vrijkomt. Dat laatste is de
//     harde: een grant die blijft staan wijst naar geheugen dat de pool zo
//     meteen aan een volgende job geeft.
package slots

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// INTREKKEN IS HET MOEILIJKE DEEL, niet verlenen.
//
// De display houdt zijn zicht vast als een gewone []byte. Op het moment dat
// HOP een grant intrekt weet die slice van niets, en er is geen kanaal om het
// hem te vertellen: de hop-ABI kent alleen antwoorden op vragen, geen duwtjes.
// Twee dingen kunnen dan misgaan, en ze zijn allebei erger dan het probleem dat
// we oplossen:
//
//  1. de descriptors op nul → de eerstvolgende lezing is een stage-2-abort en
//     de DISPLAY gaat dood. Een app die crasht neemt dan de hele desktop mee;
//  2. de blokken meteen opnieuw uitdelen → de display leest met zijn oude slice
//     ineens in het RAM van de vólgende app. Een lek tussen twee kooien, precies
//     wat dit mechanisme niet mag veroorzaken.
//
// Daarom twee maatregelen samen:
//
//   - een ingetrokken blok wijst naar een NULREGIO in plaats van naar niets. Een
//     late lezing geeft dan zwarte pixels — zichtbaar verkeerd, nooit fataal en
//     nooit andermans geheugen;
//   - een vrijgegeven blok gaat in QUARANTAINE voordat het opnieuw uitgedeeld
//     wordt. De display ruimt een sessie op zodra die 15s zwijgt (de leesdeadline
//     in hop-os-surf, gemeten 19-07), en met die opruiming laat hij de slice
//     los. Ruim daarboven gaan zitten kost niets: er zijn 512 blokken.
//
// De prijs is 2MB DRAM voor de nulregio, één keer per node, en pas op het moment
// dat er echt een grant ingetrokken wordt.

// surfQuarantine is hoeveel langer een blok vast blijft dan de display nodig
// heeft om een dode sessie op te ruimen (15s leesdeadline). Vier keer zo lang:
// blokken zijn er genoeg en de kosten van te kort zijn een lek.
const surfQuarantine = 60 * time.Second

// surfMu beschermt de blok-bitmap en de per-slot administratie. De servicer van
// élk slot kan hier binnenkomen (elke app heeft zijn eigen servicer-goroutine),
// en releaseSlot komt er vanuit de lifecycle bij.
var surfMu sync.Mutex

// surfUsed is de vrije-blokken-bitmap van het SurfIPA-GB, en surfOf onthoudt per
// verlenend slot welk stuk het heeft. Eén grant per slot: een app heeft één
// venster, en een tweede grant vervangt de eerste. Meer is speculatie tot er een
// app is die het vraagt.
var (
	surfUsed [layout.SurfBlocks]bool
	surfOf   [layout.SlotCap + 1]surfSpan
	// surfHolder is het slot waaraan we op dít moment verlenen. Wisselt de
	// houder (display herstart, of een andere app pakt de framebuffer), dan is
	// de hele administratie van de vorige houder betekenisloos — zijn
	// stage-2-blok is bij Build toch al schoongeveegd.
	surfHolder int
	// surfFreeAt is per blok het moment waarop het weer uitgedeeld mag worden
	// (nul = vrij). Zie de quarantaine-uitleg boven.
	surfFreeAt [layout.SurfBlocks]time.Time
	// surfZero is de nulregio waar ingetrokken blokken naar wijzen, en surfZeroPA
	// het 2MB-uitgelijnde adres erbinnen. De backing []byte blijft in leven zodat
	// de GC hem nooit opruimt terwijl er stage-2-descriptors naar wijzen.
	surfZero   []byte
	surfZeroPA uint64
)

type surfSpan struct {
	blk    int
	blocks int
}

// surfZeroRegion levert (lui) het 2MB-blok met nullen. HOP draait identity
// gemapt, dus het adres uit zijn eigen heap ís het fysieke adres. 4MB vragen om
// 2MB uitgelijnd te kunnen snijden — Go's allocator kent geen uitlijning boven
// 8 bytes.
func surfZeroRegion() (uint64, bool) {
	if surfZeroPA != 0 {
		return surfZeroPA, true
	}
	surfZero = make([]byte, 2*layout.SurfBlock)
	p := uint64(uintptr(unsafe.Pointer(&surfZero[0])))
	surfZeroPA = (p + layout.SurfBlock - 1) &^ (layout.SurfBlock - 1)
	return surfZeroPA, true
}

// SurfaceGrant is de OpSurfGrant-kant: verleen [off, off+n) uit het RAM van slot
// i read-only aan de display-houder en geef het IPA terug waarop díe het ziet.
//
// off en n zijn 2MB-uitgelijnd (zie layout.SurfBlock voor waarom); de app
// alloceert zo'n buffer met applib.SurfaceBuf.
func SurfaceGrant(i int, off, n uint64) (uint64, error) {
	holder := grantHolder()
	if holder == 0 {
		return 0, fmt.Errorf("surface grant: no display holds the framebuffer on this node")
	}
	if holder == i {
		// De display die zijn eigen buffer aan zichzelf verleent is geen
		// bedrijfsgeval maar wel een manier om de bitmap leeg te trekken.
		return 0, fmt.Errorf("surface grant: slot %d holds the display itself", i)
	}
	base, size, ok := partitionOf(i)
	if !ok {
		return 0, fmt.Errorf("surface grant: slot %d has no partition", i)
	}
	appRAM, err := appRAMSize(size)
	if err != nil {
		return 0, err
	}
	if n == 0 || off%layout.SurfBlock != 0 || n%layout.SurfBlock != 0 {
		return 0, fmt.Errorf("surface grant: window %#x+%#x must be a whole number of %d MB blocks",
			off, n, uint64(layout.SurfBlock)>>20)
	}
	// De optelsom vóór de vergelijking, anders schuift een overflow het venster
	// stilletjes binnen de grens.
	if off+n < off || off+n > appRAM {
		return 0, fmt.Errorf("surface grant: window %#x+%#x falls outside the app RAM of slot %d (%d MB)",
			off, n, i, appRAM>>20)
	}
	blocks := int(n / layout.SurfBlock)

	surfMu.Lock()
	defer surfMu.Unlock()
	if surfHolder != holder {
		// Verse houder: alles wat we voor de vorige bijhielden is weg.
		surfResetLocked()
		surfHolder = holder
	}
	// Een hergrant van hetzelfde slot geeft eerst zijn oude blokken terug —
	// anders lekt de bitmap vol bij een app die zijn venster laat groeien.
	surfDropLocked(i)
	blk, ok := surfAllocLocked(blocks)
	if !ok {
		return 0, fmt.Errorf("surface grant: no room for %d MB in the display's surface window", n>>20)
	}
	if err := cageMapSurface(holder, blk, base+off, blocks); err != nil {
		surfFreeLocked(blk, blocks)
		return 0, err
	}
	surfOf[i] = surfSpan{blk: blk, blocks: blocks}
	ipa := uint64(layout.SurfIPA) + uint64(blk)*layout.SurfBlock
	fmt.Printf("slot %d: surface granted to display slot %d — %d MB at %#x (zero-copy)\n",
		i, holder, n>>20, ipa)
	return ipa, nil
}

// SurfaceRevoke trekt de grant van slot i in. Idempotent: een slot zonder grant
// is een no-op, want releaseSlot roept dit voor élk slot aan.
func SurfaceRevoke(i int) {
	surfMu.Lock()
	defer surfMu.Unlock()
	surfDropLocked(i)
}

// SurfaceHolderGone maakt de administratie leeg omdat de display zelf weg is.
// Zijn stage-2-tabellen worden bij een herstart toch door Build schoongeveegd,
// dus hier hoeft niets ontmapt te worden — maar de bitmap moet leeg, anders
// raakt het SurfIPA-GB na een paar display-herstarts vol met blokken die
// niemand meer houdt.
func SurfaceHolderGone(i int) {
	surfMu.Lock()
	defer surfMu.Unlock()
	if surfHolder == i {
		surfResetLocked()
	}
}

// surfDropLocked ontmapt en vergeet de grant van slot i.
func surfDropLocked(i int) {
	if i < 0 || i > layout.SlotCap {
		return
	}
	s := surfOf[i]
	if s.blocks == 0 {
		return
	}
	if surfHolder != 0 {
		// NIET ontmappen maar naar de nulregio wijzen: de display kan de slice
		// nog vasthebben, en een lezing op een genulde descriptor is een
		// stage-2-abort die hém doodt. Zwarte pixels zijn het juiste antwoord.
		//
		// Faalt dit, dan is het enige veilige alternatief het blok NIET
		// teruggeven: liever een blok kwijt dan een display die in het RAM van
		// de volgende app kijkt.
		zero, _ := surfZeroRegion()
		if err := cageRemapSurfaceZero(surfHolder, s.blk, s.blocks, zero); err != nil {
			fmt.Printf("slot %d: surface revoke failed (%v) — %d blocks quarantined for good HOPOS_SURF_STALE\n",
				i, err, s.blocks)
			surfOf[i] = surfSpan{}
			return
		}
	}
	surfFreeLocked(s.blk, s.blocks)
	surfOf[i] = surfSpan{}
}

func surfResetLocked() {
	surfUsed = [layout.SurfBlocks]bool{}
	surfOf = [layout.SlotCap + 1]surfSpan{}
	surfFreeAt = [layout.SurfBlocks]time.Time{}
	surfHolder = 0
}

// surfAllocLocked zoekt het eerste aaneengesloten gat van n blokken die niet in
// quarantaine zitten. First-fit over 512 blokken is niets: dit draait bij het
// openen van een venster.
func surfAllocLocked(n int) (int, bool) {
	now := time.Now()
	run := 0
	for b := range layout.SurfBlocks {
		if surfUsed[b] || now.Before(surfFreeAt[b]) {
			run = 0
			continue
		}
		run++
		if run == n {
			start := b - n + 1
			for k := start; k <= b; k++ {
				surfUsed[k] = true
				surfFreeAt[k] = time.Time{}
			}
			return start, true
		}
	}
	return 0, false
}

// surfFreeLocked geeft blokken terug, maar pas ná de quarantaine — zolang kan
// de display nog een slice op deze adressen hebben.
func surfFreeLocked(blk, blocks int) {
	until := time.Now().Add(surfQuarantine)
	for k := blk; k < blk+blocks && k < layout.SurfBlocks; k++ {
		surfUsed[k] = false
		surfFreeAt[k] = until
	}
}
