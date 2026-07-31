//go:build riscv64

// De RISC-V64-helft van de idle-governor. Eén van de twee paden bestaat hier
// écht, en dat verschil is silicium en geen luiheid:
//
//   - **Gedeeld hart: yielden.** Deelt dit slot zijn hart met een ander, dan is
//     idle-tijd geen stroom maar een BEURT. De governor trapt dan met `ecall`
//     naar HOP's M-mode-switcher (cpu/mmode), die onze staat bewaart, de
//     mede-bewoner laat draaien en ons hier hervat. Exact het pad dat op ARM de
//     HVC-yield is; het contract (bewaar, roteer, hervat) staat daar al.
//   - **Eigen hart: doorlopen.** WFI is alleen veilig als er een interrupt-bron
//     aan staat die hem wekt, en die interrupt-plumbing (CLINT/PLIC, timer-PPI,
//     vector) heeft HopOS nergens anders nodig. Zonder wekker is WFI een hang.
//     Dus loopt een dedicated hart hier door, en dat is bewust: op dit board is
//     de energieknop niet idle maar de KLOK (gemeten 30-07: divider 1→2 halveert
//     de benchmark, PLL onaangeroerd, tijdrekening blijft kloppen omdat rdtime
//     aan de vaste osc hangt) — en voor een fanloze 24/7-node is dát de knop die
//     telt. Een idle-hart kost hier dus stroom; gedocumenteerde keuze, geen
//     omissie.
//
// Waarom de gedeelde kant geen wekker nodig heeft: de yield ÍS de wekker. Er
// wordt niet gewacht op een gebeurtenis, er wordt afgegeven — en wie afgeeft
// krijgt zijn beurt terug van de rotatie, niet van een timer.
package idle

import (
	"runtime/goos"
	"sync/atomic"

	"hop-os/metal/dev"
)

// ecallYield/exitTrap: zie idle_riscv64.s.
func ecallYield() uint64
func exitTrap()

// ExitTrap meldt HOP dat deze bewoner klaar is: de switcher (cpu/mmode) zet zijn
// ctx-staat op dood en roteert weg, dus het hart draait door voor zijn buren.
// Zonder dit is een geëxiteerde app "hervatbaar" en moet HOP het hele hart
// resetten om hem kwijt te raken — wat op een gedeeld hart de buren meeneemt.
// Alleen zinvol als er een laag boven ons zit; de aanroeper weet dat (CtrlShared)
// en valt anders terug op wachten tot HOP het hart ophaalt.
func ExitTrap() { exitTrap() }

// prev is de governor die er stond vóór Enable (de runtime-default): het
// dedicated-hart-pad. Eén schrijver, in Enable, vóór het eerste scheduler-punt.
var prev func(int64)

var (
	ticks      atomic.Uint64
	pubAddr    atomic.Uintptr
	sharedAddr atomic.Uintptr // CtrlShared-woord van de eigen control-page
)

// Enable hangt de governor in de runtime, mét de governor die er al stond als
// terugval. Geen event-stream te configureren zoals op ARM (CNTKCTL): het
// wisselmoment is hier een expliciete trap, geen wekker.
//
// De vorige governor bewaren en aanroepen is geen netheid maar voorzichtigheid:
// op een dedicated hart hoort het gedrag exact te blijven wat het was (de
// runtime-default van tamago, die op dit silicium bewezen draait). Wij voegen
// alléén het gedeelde-hart-pad toe.
func Enable() {
	prev = goos.Idle
	goos.Idle = governor
}

// Publish laat de teller vanaf nu óók op addr landen — het CtrlIdle-woord van de
// eigen control-page, waar HOP hem leest (dvfs-beleid, per-slot CPU-meting).
func Publish(addr uintptr) { pubAddr.Store(addr) }

// WatchShared laat de governor het CtrlShared-woord op addr lezen: is het ≠ 0,
// dan deelt dit slot zijn hart en yieldt de governor via ecall in plaats van door
// te lopen. HOP zet en wist dat woord dynamisch (kern/slots refreshShared), dus
// we lezen het élke idle-ronde vers. applib roept dit in Init.
func WatchShared(addr uintptr) { sharedAddr.Store(addr) }

// Ticks geeft de interne tellerstand: geaccumuleerde idle-tijd in
// rdtime-eenheden. Op een dedicated hart blijft die nul — eerlijk, want daar
// wordt niet geslapen (zie de pakket-doc).
func Ticks() uint64 { return ticks.Load() }

// CounterHz is de eenheid van de teller: de timebase van dit board. RISC-V heeft
// geen register waaruit die volgt (waar ARM CNTFRQ_EL0 heeft), en cpu ligt ónder
// board — dus levert de board-laag hem aan met UseCounterHz. Default is de
// SG2002-waarde: vandaag het enige RISC-V-board, en nul zou de normalisatie bij
// de lezers (dvfs, per-slot CPU-meting) door nul laten delen.
var counterHz uint64 = 25_000_000

// UseCounterHz zet de timebase. Aanroepen in het init() van de basis-helft van
// het board, vóór de eerste Ticks-lees.
func UseCounterHz(hz uint64) {
	if hz > 0 {
		counterHz = hz
	}
}

func CounterHz() uint64 { return counterHz }

// AccountsDedicated is hier ONWAAR: een dedicated hart loopt door (geen WFI
// zonder wekker, zie de pakket-doc), dus zijn idle-teller blijft nul. Wie daar
// een cpu-percentage van maakt leest élk dedicated slot als 100% bezig — dat is
// geen meting maar een meetgat, en een verkeerd cijfer is erger dan geen. Een
// GEDEELD hart meet wél, net als op ARM: de ecall-yield beslaat de hele periode
// waarin de buur draaide.
func AccountsDedicated() bool { return false }

// governor: op een gedeeld hart één yield per scheduler-ronde, met de tellerstand
// eromheen. De verstreken tijd is de wall-tijd waarin een buur draaide — precies
// wat "wij deden niets" betekent op een gedeeld hart, en dezelfde meting als de
// ARM-kant rond zijn HVC doet.
func governor(pollUntil int64) {
	a := sharedAddr.Load()
	if a == 0 || dev.Read64(a) == 0 {
		// Eigen hart: precies doen wat dit board zonder ons zou doen.
		if prev != nil {
			prev(pollUntil)
		}
		return
	}
	n := ticks.Add(ecallYield())
	if p := pubAddr.Load(); p != 0 {
		dev.Write64(p, n)
	}
}
