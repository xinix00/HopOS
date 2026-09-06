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
	// GEEN cache-onderhoud vóór deze read, en dat is een keuze. De verleiding
	// is groot (wij lezen cacheable wat de vorige kern met device-writes
	// neerzette), maar het enige primitief dat er is, is CleanInv — en die
	// CLEANT eerst: vuile lijnen van de vorige huurder gaan dan over het verse
	// blob heen. Gemeten 06-09: met die "verbetering" erin overleefden de
	// conntrack-flows de tweede flip niet meer. Komt er ooit een
	// invalidate-zonder-clean, dan hoort hij hier.
	b := make([]byte, handoffTail)
	dev.CopyOut(b, uintptr(ptr))
	h, err := decodeHandoff(b)
	if err != nil {
		// NIET stil terugvallen op een koude boot. De pointer én de magic
		// klopten, dus er is écht geflipt en er kunnen bewoners leven — en de
		// koude boot veegt juist de regio waarin die bewoners draaien. Dat is
		// hoe een onleesbaar blob een node sloopt in plaats van hem een
		// generatie te laten missen. Adoptie-stand AAN (niets vegen) en dan
		// blijven staan: de watchdog van de flip-boot (armBootGuard) haalt ons
		// binnen seconden op, en de node komt schoon terug.
		stage(stAdoptBlobBad)
		setAdopting(true)
		fmt.Printf("kernflip: FATAL — handoff blob at %#x is unusable (%v); residents may be live, so this kernel refuses to continue as a cold boot. Waiting for the watchdog. HOPOS_FLIP_BLOB_BAD\n", ptr, err)
		for {
		}
	}
	// Vanaf hier weet de rest van de kern dat dit een adoptie is: de arch-laag
	// laat de app-core-regio met rust (thunks, parkeerlus, sched-blokken,
	// ctx-staten blijven staan) en verifieert dat de zittende switch-code de
	// onze is.
	//
	// Eerst nog archiveren wat er in de recorder stond. Is dat niet ónze
	// sprong, dan is het het spoor van een eerdere poging die nooit landde, en
	// dit is het laatste moment waarop dat spoor bestaat (stage.go).
	archiveStage(h.Gen)
	stage(stLanded)
	setAdopting(len(h.Slots) > 0)
	curGen, curSum = h.Gen, h.BundleSum
	return h, true
}

// PresumeAdopting zet de adoptie-stand ALVAST, vóór de kern iets doet dat de
// app-core-regio kan raken.
//
// WAAROM (gemeten 06-09 op de M4). De stand werd pas in Adopted gezet, en dat
// is laat: daarvóór draaien Privilege(), de firmware-probe en het board-opzetten
// al, terwijl de bewoners op hun eigen cores gewoon doorgaan — ze nemen HVC's,
// lezen hun ctx-blok en lopen door de switch-code in de plan-regio. Een kern
// die zichzelf in dat venster nog als KOUDE boot ziet, mag die regio vers
// neerzetten, en dat is precies onder een levende core. De val was dan ook
// alleen te reproduceren MET bewoners: acht flips op rij zonder bewoners
// landden allemaal (generatie 2 t/m 9), met bewoners viel ongeveer één op drie.
//
// Adopted verfijnt de stand later (nul bewoners = alsnog vers neerzetten); dit
// is de veilige kant van de twijfel voor het venster ervóór.
func PresumeAdopting() {
	if !FlipPending() {
		return
	}
	setAdopting(true)
}

// FlipPending kijkt of er een overdracht KLAARLIGT, zonder hem te consumeren:
// het pointer/magic-paar op de boot-scratch. Dat is het enige harde bewijs dat
// deze boot uit een sprong komt.
//
// Bewust NIET de vluchtrecorder: die houdt zijn laatste stand vast tot de agent
// draait, dus na een geslaagde flip staat er "geland" — en een KOUDE boot
// daarna zou zichzelf dan als adoptie zien en de app-core-regio niet vers
// neerzetten. Precies de fout die dit hele onderzoek onderzoekt, maar dan
// andersom.
func FlipPending() bool {
	ptrPA := layout.HandoffPtrPA()
	return dev.Read64(ptrPA+8) == handMagic && dev.Read64(ptrPA) != 0
}
