package kernflip

// De vluchtrecorder. Een flip eindigt in een sprong waarna de vorige kern niet
// meer bestaat; gaat er iets mis, dan reset de watchdog de node en is élke
// diagnose weg. De console helpt daar principieel niet bij — de node serveert
// hem zelf, dus de laatste regels vóór een crash halen de lezer nooit en de
// reboot wist de ring. DRAM overleeft dat wél.
//
// Twee woorden, op een board-adres buiten élke RAM-declaratie
// (layout.FlipStagePA) en dus van geen enkele kern eigendom:
//
//	+0  de LOPENDE poging — elke stap schrijft waar hij is
//	+8  het ARCHIEF — de laatste poging die niet landde
//
// Beide dragen een tag in de bovenste 32 bits, zodat willekeurige DRAM-rommel
// na een koude start nooit als "vorige flip" gelezen wordt, en de sprong draagt
// er de doelgeneratie bij in (zie stageJump).
//
// WAAROM HET ARCHIEF BESTAAT. Een gecrashte flip laat de node terugvallen op de
// geïnstalleerde kern, en de flip waarmee je hem daarna weer optilt schrijft
// zijn eigen stappen over het spoor heen dat je wilde lezen. De eerste twee
// gevallen flips op de M4 (06-09) waren daardoor onverklaarbaar: de reparatie
// at het bewijs op. Nu archiveert elke nieuwe poging eerst wat er stond.
//
// Zie docs/meetinstrumenten.md voor het hele gereedschapskistje, inclusief de
// les die dit instrument zelf opleverde.

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	// stageTag ("FLIP") scheidt een echte stand van rommel; hij zit in de
	// bovenste 32 bits, de stap in de onderste 16 en de generatie ertussen.
	stageTag = uint64(0x464C4950) << 32

	// archiveOff is het tweede woord: de laatste poging die niet landde.
	archiveOff = 8
)

// De stappen, in de volgorde waarin Flip ze doorloopt. De namen zijn wat de
// console zegt als een boot ze aantreft; de recorder zelf heeft alleen het
// nummer nodig.
//
// Een nieuwe stap hoort ACHTERAAN. Er eentje tussen schuiven hernummert de
// rest, en dan meldt een oudere kern zijn eigen stand verkeerd — dat is 06-09
// één keer gebeurd en het kostte een ronde.
const (
	stFetched = iota + 1
	stWindowHeld
	stBundleOK
	stBorrowed
	stVectors
	stScrubbed
	stPlaced
	stRebased
	stCaptured
	stHandoff
	stJumping

	// Vanaf hier schrijft de NIEUWE kern; de sprong is dan gelukt en de vraag
	// wordt hoe ver zijn eigen boot komt.
	stEarlyMain
	stLanded
	stNetUp

	// Het ene faalpad in de overdracht dat de kern bewust laat staan in plaats
	// van door te lopen: een blob dat niet decodeert (adopted.go).
	stAdoptBlobBad
)

var stageNames = [...]string{
	stFetched:      "bundle fetched and verified",
	stWindowHeld:   "slot lifecycle window held",
	stBundleOK:     "bundle validated",
	stBorrowed:     "window borrowed from the pool",
	stVectors:      "vectors installed",
	stScrubbed:     "window scrubbed",
	stPlaced:       "segments placed",
	stRebased:      "relocations applied",
	stCaptured:     "residents, NAT and agent state captured",
	stHandoff:      "handoff blob written",
	stJumping:      "about to jump into the new kernel",
	stEarlyMain:    "the new kernel reached main but died before the handover",
	stLanded:       "jump landed and the handover was consumed — died later in the new kernel's boot",
	stNetUp:        "the new kernel's network is up — died between net and agent",
	stAdoptBlobBad: "the handoff blob did not decode; the kernel refused to continue and waited for the watchdog",
}

// stage legt de lopende stand vast. Rechtstreeks naar DRAM geveegd: dit woord
// moet een crash één instructie later nog overleven.
func stage(n int) { writeStage(uint64(n), 0) }

// stageJump is de laatste stap vóór de sprong, MÉT de generatie waar we naartoe
// gaan. Daarmee herkent een landende kern zijn eigen sprong, en blijft een
// stJumping van een ándere generatie herkenbaar als een sprong die nooit landde.
func stageJump(gen uint64) { writeStage(stJumping, gen) }

func writeStage(step, gen uint64) {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	dev.Write64(pa, stageTag|step|gen<<16)
	dev.CleanInv(pa, 8)
	dev.MB()
}

// stageClear wist de lopende stand.
func stageClear() {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	dev.Write64(pa, 0)
	dev.CleanInv(pa, 8)
	dev.MB()
}

// stageOf pelt stap en generatie uit een recorderwoord; ok=false is "hier staat
// geen stand".
func stageOf(v uint64) (step int, gen uint64, ok bool) {
	if v&^0xFFFFFFFF != stageTag {
		return 0, 0, false
	}
	return int(v & 0xFFFF), v >> 16 & 0xFFFF, true
}

// describe geeft de consoletekst bij een stand.
func describe(step int) string {
	if step >= 0 && step < len(stageNames) && stageNames[step] != "" {
		return stageNames[step]
	}
	return "unknown stage"
}

// archiveStage schuift een onafgemaakte stand naar het archief. Aangeroepen aan
// het begin van een nieuwe flip (die de lopende stand meteen overschrijft) en
// door een landende kern vóór hij stLanded schrijft.
//
// Alles wat ONZE eigen generatie schreef is eigen voortgang en geen mislukking.
// Zonder die toets archiveert elke geslaagde adoptie zijn eigen laatste stand,
// en meldt de volgende boot een mislukking die er niet was.
func archiveStage(mine uint64) {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	v := dev.Read64(pa)
	_, gen, ok := stageOf(v)
	if !ok || gen == mine {
		return
	}
	dev.Write64(pa+archiveOff, v)
	dev.CleanInv(pa+archiveOff, 8)
	dev.MB()
}

// report drukt één stand af als waarschuwing. Geeft true zodat de aanroeper
// weet dat er iets te melden viel.
func report(what string, step int, gen uint64) bool {
	if step == stJumping {
		fmt.Printf("kernflip: WARNING — %s jumped into generation %d and that kernel never came up HOPOS_FLIP_STALLED\n", what, gen)
		return true
	}
	fmt.Printf("kernflip: WARNING — %s did not complete its handover; it got as far as step %d/%d (%s) HOPOS_FLIP_STALLED\n",
		what, step, stJumping, describe(step))
	return true
}

// ReportArchivedStage meldt en wist de laatste poging die niet landde. Elke
// boot roept hem aan; stilte betekent dat elke eerdere flip netjes eindigde.
func ReportArchivedStage() bool {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return false
	}
	step, gen, ok := stageOf(dev.Read64(pa + archiveOff))
	if !ok {
		return false
	}
	dev.Write64(pa+archiveOff, 0)
	dev.CleanInv(pa+archiveOff, 8)
	dev.MB()
	return report("an earlier flip", step, gen)
}

// ReportLastFlip meldt en wist de lopende stand. Alleen op een KOUDE boot
// aanroepen: op een flip-boot staat daar de sprong die ons hier bracht, en dat
// is geen mislukking maar de normale gang.
func ReportLastFlip() bool {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return false
	}
	step, gen, ok := stageOf(dev.Read64(pa))
	if !ok {
		return false
	}
	stageClear()
	return report("the previous flip", step, gen)
}

// MarkEarlyBoot is de eerste regel van main van een geflipte kern: stond er nog
// "springen", dan zijn wij die sprong en hebben we main gehaald. Een koude boot
// heeft hier geen stand staan en houdt dus zijn eigen spoor.
func MarkEarlyBoot() {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	if step, gen, ok := stageOf(dev.Read64(pa)); ok && step == stJumping {
		writeStage(stEarlyMain, gen)
	}
}

// MarkNetUp: de geflipte kern heeft zijn netwerk op, zodat een dood tússen net
// en agent zich onderscheidt van een dood in de bring-up.
func MarkNetUp() { stage(stNetUp) }

// BootLanded wist de recorder: de geflipte kern heeft zijn agent draaiend en
// daarmee is de flip pas ECHT geland. Niet eerder — een kern die de handoff al
// gelezen had maar daarna in zijn boot stierf, hoort een spoor achter te laten.
func BootLanded() { stageClear() }
