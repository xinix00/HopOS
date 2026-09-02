//go:build riscv64

// De RISC-V64-helft van de idle-governor; de gedeelde helft — tellers,
// publicatie, wakeAt — staat in idle.go. Eén governor, één principe (Derek,
// 01-08): élke bewoner van een hart meldt "ik doe niets tot T" langs hetzelfde
// pad, en HOP is daarin gewoon de eerste bewoner van zíjn hart. Wat per rol
// verschilt is alleen de laatste stap — hoe je bij de M-mode-slaapcode komt:
//
//   - **Slot-app: yielden, altijd.** Gedeeld hart óf alleen — de governor trapt
//     met `ecall` naar HOP's M-mode-switcher (cpu/mmode), mét de wektijd in a0.
//     De switcher bewaart de staat, geeft een buur zijn beurt als die er is, en
//     laat het hart anders slapen op de CLINT-wekker (park). Voor een app die
//     alléén woont is de yield dus geen beurt-wissel maar de weg naar de slaap:
//     een S-mode-bewoner kan niet zelf bij mtimecmp (de CLINT zit bewust buiten
//     elke kooi — DoS-kanaal), dus de ecall is zijn enige route naar een wfi.
//   - **HOP's eigen hart: dezelfde stap, zonder trap.** HOP draait al in
//     machine mode, dus die roept de slaap-primitief direct aan (de Sleeper —
//     het board levert hem ná zijn CLINT-probe met idle.Use,
//     board/licheerv/clint.go). Een ecall is voor S-mode de manier om deze
//     code te bereiken; HOP staat er al.
//   - **Terugval: doorlopen.** Geen switcher boven je en geen waker van het
//     board (probe gefaald, of een app buiten applib) → precies doen wat dit
//     board zonder ons deed. Spinnen kost stroom maar kan niet hangen, en dat
//     is de goede kant om naar te falen.
//
// De energieknop van dit board was eerst de KLOK (gemeten 30-07: divider 1→2
// halveert de benchmark). Dat blijft waar, maar de klok is de Pi-knop (dvfs,
// een serieuzer thermisch budget); hier is de knop de wekker: een hart in wfi
// is clock-gated op élke kloksnelheid.
package idle

import (
	"runtime/goos"

	"github.com/xinix00/HopOS/metal/dev"
)

// ecallYield/exitTrap/counterNow: zie idle_riscv64.s.
func ecallYield(deadline uint64) uint64
func exitTrap()

// ExitTrap meldt HOP dat deze bewoner klaar is: de switcher (cpu/mmode) zet zijn
// ctx-staat op dood en roteert weg, dus het hart draait door voor zijn buren.
// Zonder dit is een geëxiteerde app "hervatbaar" en moet HOP het hele hart
// resetten om hem kwijt te raken — wat op een gedeeld hart de buren meeneemt.
// Alleen zinvol als er een laag boven ons zit; de aanroeper weet dat (CtrlShared)
// en valt anders terug op wachten tot HOP het hart ophaalt.
func ExitTrap() { exitTrap() }

// prev is de governor die er stond vóór Enable (de runtime-default): de
// terugval als er switcher noch waker is. Eén schrijver, in Enable, vóór het
// eerste scheduler-punt.
var prev func(int64)

// De Sleeper (idle.go Use) is hier de directe M-mode-slaap van het EIGEN hart,
// alleen gezet op HOP (het board levert hem ná zijn CLINT-probe). Apps krijgen
// hem nooit: hun weg naar dezelfde slaap is de ecall-yield.

// Enable hangt de governor in de runtime, mét de governor die er al stond als
// terugval. Geen event-stream te configureren zoals op ARM (CNTKCTL): wat er te
// configureren valt zit in de twee zetters (WatchShared voor een slot-app, Use
// voor HOP), en zonder één van beide is de governor de runtime-default met een
// omweg — bewust, want dat is de bewezen stand.
func Enable() {
	prev = goos.Idle
	goos.Idle = governor
}

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

// AccountsDedicated is sinds 01-08 WAAR: een slot-app yieldt óók als hij alleen
// woont (zie de pakket-doc), en de ecall beslaat zijn hele weg-tijd — de meting
// is dus identiek aan de gedeelde. Dit stond op onwaar toen een dedicated hart
// bewust doorliep; die keuze is teruggedraaid (idlen overal, Derek 01-08).
func AccountsDedicated() bool { return true }

// governor: één melding per scheduler-ronde — "niets te doen tot T" — langs het
// pad dat bij de rol van dit hart hoort (zie de pakket-doc). De verstreken tijd
// is in alle gevallen de wall-tijd waarin dit slot niets deed: bij een yield de
// tijd waarin een buur draaide óf het hart sliep, bij MSleep de slaap zelf.
func governor(pollUntil int64) {
	// Eerst de idle-modus van het board (CtrlIdleMode; op deze architectuur
	// nog zonder waarden, maar dezelfde ronde als op ARM), dan de doorbell —
	// zelfde twee redenen als op ARM (zie rxdoor.go).
	idleMode()
	if rxDoor() {
		countWake()
		return
	}
	if sharedAddr.Load() != 0 {
		// Slot-app: yield naar de switcher, alleen wonend of niet.
		n := ticks.Add(ecallYield(wakeAt(pollUntil)))
		if p := pubAddr.Load(); p != 0 {
			dev.Write64(p, n)
		}
		countWake()
		return
	}
	if sleeper != nil {
		// HOP's eigen hart: de slaap-primitief van het board. De rem op de
		// slaap zit in die primitief zelf (board/licheerv/clint.go: MSleep
		// klemt élke slaap op SleepCapTicks, dus niets wacht ooit langer dan
		// die cap op HOP).
		// wakeAt geeft 0 als er niets te slapen valt (deadline verstreken, of
		// geen timer — dat laatste komt op HOP niet voor: de RX-pompen alleen
		// al leggen er elke 10-200µs één neer), en MSleep doet dan niets; de
		// scheduler kijkt gewoon nog een ronde.
		ticks.Add(sleeper(wakeAt(pollUntil)))
		countWake()
		return
	}
	// Geen switcher, geen waker: precies doen wat dit board zonder ons deed.
	if prev != nil {
		prev(pollUntil)
	}
}

// applyIdleMode: nog geen modi op deze architectuur. Het woord komt wél
// langs — dezelfde ronde, hetzelfde woord — zodat een RISC-V-board dat ooit
// een eigen idle heeft alleen hier een waarde hoeft te leren.
func applyIdleMode(mode uint64) {}
