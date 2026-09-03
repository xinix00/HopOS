package kernflip

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// curGen is de generatie van DEZE boot: 0 = van de firmware geboot, n = na de
// n-de flip. Elke flip geeft curGen+1 door, zodat een kern altijd weet
// hoeveel keer de node al onder zichzelf vandaan gesprongen is zonder reboot.
var curGen uint64

// curSum is de som van de bundel waar deze kern uit geplaatst is (0 = van de
// firmware geboot). FlipFromURL gebruikt hem om niet in een lus te springen.
var curSum uint64

// Generation geeft die teller (0 = deze kern kwam van de firmware).
func Generation() uint64 { return curGen }

// Adopted meldt of deze boot een kern-flip was en CONSUMEERT het handoff-blob:
// het pointer/magic-paar op de boot-scratch wordt genuld vóór het blob wordt
// vertrouwd, zodat een latere (watchdog-)reboot nooit een stale blob adopteert.
//
// ZEER VROEG in main aanroepen — vóór slots/stage2 iets initialiseren, want
// deze functie zet ook de adoptie-stand van stage2: die bepaalt of InitVectors
// de app-core-regio vers neerzet (gewone boot) of met rust laat (er draaien
// bewoners in). Verse DRAM is geen nul, dus alleen het pointer+magic-PAAR telt
// als bewijs; alles daarbuiten is een gewone boot.
func Adopted() (Handoff, bool) {
	ptrPA := layout.HandoffPtrPA()
	ptr := dev.Read64(ptrPA)
	magic := dev.Read64(ptrPA + 8)
	if ptr == 0 && magic == 0 {
		return Handoff{}, false // gewone boot (of al geconsumeerd)
	}
	// Consumeren vóór vertrouwen — óók als het paar niet klopt: half garbage
	// mag geen tweede boot besmetten.
	dev.Write64(ptrPA, 0)
	dev.Write64(ptrPA+8, 0)
	dev.MB()
	if magic != handMagic || ptr%8 != 0 {
		fmt.Printf("kernflip: stray handoff pointer on the boot scratch (%#x/%#x) — ignored, cold boot\n", ptr, magic)
		return Handoff{}, false
	}
	// De pointer moet exact op het einde van onze eigen RAM-declaratie wijzen.
	// Dat is geen extra afspraak maar een eigenschap van de constructie: de
	// flip legt het blob in de staart van het geleende venster, direct boven
	// wat hij als RamSize patcht. Eén vergelijking, en de hele klasse "lees op
	// een adres dat een ander daar neerlegde" is weg — tot nu was de magic op
	// de scratch het enige dat deze read afdekte.
	if end := ownRamEnd(); end != 0 && ptr != end {
		fmt.Printf("kernflip: handoff pointer %#x is not at our RAM end (%#x) — ignored, cold boot\n", ptr, end)
		return Handoff{}, false
	}
	// Het blob uit de staart van ons eigen venster lezen. De kop eerst (die
	// draagt de lengte-informatie via slotCount), dan ruim genoeg voor de
	// records: de staart is handoffTail groot en het blob paste daar per
	// constructie in.
	b := make([]byte, handoffTail)
	dev.CopyOut(b, uintptr(ptr))
	h, err := decodeHandoff(b)
	if err != nil {
		fmt.Printf("kernflip: handoff blob at %#x is unusable (%v) — ignored, cold boot\n", ptr, err)
		return Handoff{}, false
	}
	// Vanaf hier weet de rest van de kern dat dit een adoptie is: de arch-laag
	// laat de app-core-regio met rust (thunks, parkeerlus, sched-blokken,
	// ctx-staten blijven staan) en verifieert dat de zittende switch-code de
	// onze is.
	// Geland: de sprong is gelukt en de handoff is geldig. De recorder gaat
	// NIET leeg maar naar stLanded — sterft deze kern verderop in zijn boot,
	// dan is dát het spoor dat de volgende boot vindt. Wissen gebeurt pas als
	// de agent draait (BootLanded).
	stage(stLanded)
	setAdopting(len(h.Slots) > 0)
	curGen, curSum = h.Gen, h.BundleSum
	return h, true
}
