//go:build gui

package stage2

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
)

// Surface-grants: de tweede grant-soort naast GrantWindow, en bewust een eigen
// bestand — hij lijkt erop maar verschilt op de drie dingen die ertoe doen.
//
//	                  fb-grant (GrantWindow)      surface-grant (hier)
//	wat              firmware-framebuffer        het RAM van een ándere app
//	rechten          read-write                  READ-ONLY
//	moment           vóór de dispatch            terwijl de kooi DRAAIT
//	aantal           één per node                veel, komen en gaan
//
// Die derde regel is de reden dat dit meer is dan een tweede constante. De
// fb-grant wordt geschreven in een kooi die nog niet loopt, dus er staat per
// definitie niets in de TLB. Een surface-grant landt in een kooi die op dat
// moment vensters aan het tekenen is — dus hoort er een TLB-intrekking bij, en
// hoort de volgorde (tabel schrijven → cache vegen → TLBI) net zo heilig te
// zijn als bij Revoke, en om exact dezelfde reden: de walker van de display
// draait en leest deze tabellen cacheable.
//
// Waarom read-only écht read-only moet zijn: dit is het geheugen van een andere
// app. De display mag de pixels zien — die kreeg hij hiervoor over de socket
// ook — maar een display met schrijfrechten in de partitie van elke GUI-app is
// een gat waar de hele kooi-isolatie doorheen valt. attrRO is hier geen
// nettigheid maar de enige reden dat deze grant mag bestaan.

// MapSurface mapt blocks × 2MB vanaf pa read-only in de kooi van slot i, op
// IPA layout.SurfIPA + blk*SurfBlock. pa moet 2MB-uitgelijnd zijn; de aanroeper
// (kern/slots) heeft dan al bewezen dat het bereik in de partitie van de
// verlenende app ligt.
//
// Idempotent op dezelfde argumenten: opnieuw granten van hetzelfde venster
// schrijft dezelfde descriptors en is dus veilig na een display-herstart.
func MapSurface(i, blk int, pa uint64, blocks int) error {
	l2, err := surfL2(i, blk, blocks)
	if err != nil {
		return err
	}
	if pa&(layout.SurfBlock-1) != 0 {
		return fmt.Errorf("surface-grant: PA %#x is niet 2MB-uitgelijnd", pa)
	}
	if pa+uint64(blocks)*layout.SurfBlock < pa {
		return fmt.Errorf("surface-grant: venster %#x + %d blokken overflowt", pa, blocks)
	}
	for n := range blocks {
		off := uint64(n) * layout.SurfBlock
		dev.Write64(uintptr(l2)+uintptr(blk+n)*8, (pa+off)|blockRO)
	}
	return surfCommit(i, l2)
}

// UnmapSurface haalt de blokken er weer uit. Dít is het pad dat ertoe doet: een
// grant die blijft staan nadat het slot van de app is vrijgegeven, wijst naar
// geheugen dat de pool zo meteen aan een ándere job uitdeelt. Dan leest de
// display de partitie van een willekeurige volgende app — een lek dat er precies
// zo uitziet als "het werkt gewoon". Daarom roept releaseSlot dit onvoorwaardelijk
// aan, ook voor slots die nooit een surface hadden (dan is het een no-op).
func UnmapSurface(i, blk, blocks int) error {
	l2, err := surfL2(i, blk, blocks)
	if err != nil {
		return err
	}
	for n := range blocks {
		dev.Write64(uintptr(l2)+uintptr(blk+n)*8, 0)
	}
	return surfCommit(i, l2)
}

// RemapSurfaceZero wijst blokken naar één gedeelde nulregio in plaats van ze te
// ontmappen. Dit is het intrek-pad dat gebruikt WORDT (UnmapSurface is voor het
// geval dat er gegarandeerd geen lezer meer is), en de reden staat in
// kern/slots/surfgrant.go: de display houdt zijn zicht als een gewone slice
// vast en hoort van niemand dat de grant weg is. Een genulde descriptor maakt
// zijn eerstvolgende lezing dan een stage-2-abort — een dode app die de hele
// desktop meeneemt. Zwart lezen is het juiste antwoord.
//
// Read-only, net als de grant zelf: de nulregio is van HOP en blijft dat.
func RemapSurfaceZero(i, blk, blocks int, zeroPA uint64) error {
	l2, err := surfL2(i, blk, blocks)
	if err != nil {
		return err
	}
	if zeroPA == 0 || zeroPA&(layout.SurfBlock-1) != 0 {
		return fmt.Errorf("surface-grant: nulregio %#x is niet 2MB-uitgelijnd", zeroPA)
	}
	for n := range blocks {
		dev.Write64(uintptr(l2)+uintptr(blk+n)*8, zeroPA|blockRO)
	}
	return surfCommit(i, l2)
}

// surfL2 valideert de argumenten en geeft de surface-L2 van slot i, waarbij de
// L1-entry voor het SurfIPA-GB zo nodig wordt aangehangen.
func surfL2(i, blk, blocks int) (uint64, error) {
	if i < 1 || i > layout.MaxSlots {
		return 0, fmt.Errorf("surface-grant: slot %d buiten bereik", i)
	}
	if blocks <= 0 {
		return 0, fmt.Errorf("surface-grant: %d blokken", blocks)
	}
	if blk < 0 || blk+blocks > layout.SurfBlocks {
		return 0, fmt.Errorf("surface-grant: blok %d+%d valt buiten het SurfIPA-GB (%d blokken)",
			blk, blocks, layout.SurfBlocks)
	}
	base := layout.Stage2TablePA(i)
	l2 := uint64(base + l2SurfOff)
	gb := uint64(layout.SurfIPA) >> 30
	l1e := base + l1Off + uintptr(gb)*8
	switch cur := dev.Read64(l1e); cur {
	case 0:
		dev.Write64(l1e, l2|descTable)
	case l2 | descTable:
		// al aangehangen — surfaces komen en gaan, de tabel blijft
	default:
		return 0, fmt.Errorf("surface-grant: GB %d van de kooi is al gemapt (%#x)", gb, cur)
	}
	return l2, nil
}

// surfCommit maakt de tabelwijziging zichtbaar voor een DRAAIENDE kooi. Zelfde
// volgorde en zelfde reden als Revoke: eerst DRAM goed zetten, dan de lines
// weggooien die de walker cachede, dan pas de TLB. Andersom kan de walker
// tussen de veeg en de schrijfactie de oude tabel opnieuw cachen.
//
// De TLBI is grover dan nodig (hij raakt alle stage-2-vertalingen, niet alleen
// dit GB) omdat hvcRevoke het enige intrek-pad is dat we hebben. Dat mag hier:
// een grant hoort bij het openen of sluiten van een venster, niet bij een frame.
func surfCommit(i int, l2 uint64) error {
	dev.CleanInv(layout.Stage2TablePA(i)+l1Off, 0x1000)
	dev.CleanInv(uintptr(l2), 0x1000)
	dev.MB()
	hvcRevoke()
	return nil
}
