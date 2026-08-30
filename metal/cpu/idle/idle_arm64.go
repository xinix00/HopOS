//go:build arm64

// De ARM64-helft van de idle-governor: WFE + de generic-timer-event-stream.
// Eén WFE per scheduler-ronde, en de event-stream begrenst elke slaap: de
// generic-timer-teller genereert elke ~1ms een wakeup-event
// (CNTKCTL_EL1.EVNTEN, geen GIC of interrupt-plumbing nodig), dus de
// scheduler kijkt hooguit ~1ms later weer naar zijn timers. Timers kunnen
// daardoor tot ~1ms later vuren — irrelevant voor jobs, en een SEV/interrupt
// wekt de core direct.
//
// Elke core roept Enable aan in zijn eigen hwinit1 (ná arm64.Init, die de
// default governor zet); CNTKCTL is per core. De RISC-V-helft
// (idle_riscv64.go) heeft geen WFE-equivalent en bereikt dezelfde slaap via
// de M-mode-switcher (yield met wektijd) of, voor HOP zelf, direct
// (UseMSleep). De gedeelde helft — tellers, publicatie, wakeAt — staat in
// idle.go.

package idle

import (
	"runtime/goos"

	"github.com/xinix00/HopOS/metal/dev"
)

// wfeIdle/hvcYield/cntkctlSet/cntfrq/counterNow/mmfr0/wfiUntil: zie idle_arm64.s.
func wfeIdle() uint64
func wfiUntil(ticks uint64) uint64
func hvcYield(deadline uint64) uint64
func cntkctlSet(v uint64)
func cntfrq() uint64
func mmfr0() uint64

// Enable zet de event-stream aan en hangt de WFE-governor in de runtime.
// EVNTI kiest de counterbit waarvan de 0→1-flank het wek-event is; we pakken
// de bit die het dichtst bij ~1ms periode blijft (2^(EVNTI+1)/CNTFRQ):
// bit 15 op de Pi's 54MHz (1,2ms) en QEMU's 62,5MHz (1,05ms), bit 14 op de
// Altra's 25MHz (1,3ms — een vaste 15 gaf daar 2,6ms wek-granulariteit).
//
// EVNTI is 4 bits, dus bit 15 is het plafond — en dat plafond is op een
// GHz-teller te laag. GEMETEN 29-08 op de Mac mini M4 (CNTFRQ 1GHz): 2^16
// ticks = 65µs, vijftien keer vaker wakker dan bedoeld. FEAT_ECV
// (ID_AA64MMFR0_EL1.ECV ≥ 1) heeft daar een schaalbit voor — CNTKCTL.EVNTIS
// schuift de gekozen bit 8 posities op (×256) — en die zetten we zodra zelfs
// bit 15 onder een halve milliseconde uitkomt. Op elk board met een teller
// onder ~131MHz (Pi, QEMU, Altra, RK3566) verandert er niets: daar blijft de
// oude keuze staan en wordt EVNTIS niet gezet.
func Enable() {
	hz := cntfrq()
	shift := uint64(0)
	if mmfr0()>>60&0xF != 0 && uint64(1)<<16 < hz/2000 { // FEAT_ECV én bit 15 < 0,5ms
		shift = 8
	}
	i := uint64(15)                                     // EVNTI is 4 bits: 15 is tegelijk het maximum én de start
	for i > 4 && (uint64(1)<<(i+1+shift))*2000 > hz*3 { // periode > 1,5ms → fijnere bit
		i--
	}
	v := uint64(1<<2 | i<<4) // EVNTEN | EVNTI
	if shift != 0 {
		v |= 1 << 17 // EVNTIS: EVNTI telt in stappen van 256
	}
	cntkctlSet(v)

	// De "echt geslapen"-grens in TICKS is tellerafhankelijk (zie
	// wfeMinSleep): op 1GHz was de vaste 64 nog geen 64 nanoseconden, en dan
	// telt de instructielatentie van de WFE zelf al als slaap — de drain-lus
	// stopte na één poging en de core spinde (3,6M wakes/s, gemeten 29-08).
	if t := hz / 500_000; t > wfeMinSleep {
		wfeMinSleep = t
	}
	timerCap = hz / 1000 // 1ms, de bovengrens van één deadline-slaap
	goos.Idle = governor
}

// timerSleep: mag deze core, als de WFE-drain niets opleverde, alsnog op de
// FYSIEKE TIMER slapen (WFI met CNTP_TVAL als deadline)? Uit op elk board dat
// het niet aanzet — de WFE-governor blijft daar exact wat hij was.
//
// WAAROM DIT BESTAAT (gemeten 29-08, Mac mini M4). Een kale burst van 1000
// WFE's sliep daar keurig 1,046ms per stuk: de event-stream doet zijn werk.
// Maar ín de governor keerde elke WFE meteen terug — 3,3M idle-rondes per
// seconde met 120ns "slaap" per ronde. Het verschil is de Go-scheduler
// eromheen: elke exclusive (LDAXR/STLXR in findRunnable) zet het event-
// register, dus er staat áltijd een event klaar en de drain-lus van vier
// verliest die race. WFI kent dat probleem niet — die wacht op een echte
// interrupt-gebeurtenis, en de fysieke timer levert er precies één: gemeten
// 1.000163 ticks voor een deadline van 1ms, en 5.000133 voor 5ms.
//
// Waarom een board-schakelaar en geen default: WFI raakt de interrupt-wereld
// van het board (een pending interrupt die niemand afhandelt maakt hem een
// no-op) en QEMU-TCG modelleert hem anders dan ijzer. Dit is dus dezelfde
// afspraak als UseMSleep op RISC-V: alleen aan waar het board het gemeten
// heeft (board/apple — zie docs/archief/apple-m4.md).
var (
	timerSleep bool
	timerCap   uint64
)

// UseTimerSleep zet de deadline-slaap aan voor deze core. Aanroepen ná Enable
// (die zet timerCap), door een board dat WFI+CNTP op dít silicium bewezen heeft.
func UseTimerSleep() { timerSleep = true }

// sleepUntil slaapt tot de wektijd (absolute counterstand; 0 = geen deadline),
// begrensd op timerCap, en geeft de werkelijk verstreken ticks terug.
func sleepUntil(wake uint64) uint64 {
	d := timerCap
	if wake != 0 {
		now := counterNow()
		if wake <= now {
			return 0 // deadline al verstreken: niet slapen
		}
		if r := wake - now; r < d {
			d = r
		}
	}
	if d == 0 {
		return 0
	}
	return wfiUntil(d)
}

// CounterHz is de eenheid van de teller: generic-timer-ticks per seconde
// (CNTFRQ). Een vólledig idle core accumuleert ~CounterHz per seconde —
// wie de teller leest (dvfs-beleid, per-slot CPU-meting in kern/slotmgr)
// normeert tegen dít tempo. LET OP QEMU-TCG: WFE is daar een no-op, dus
// idle-tijd meet er ~0 — idle-metingen zijn ijzer-metingen.
func CounterHz() uint64 { return cntfrq() }

// AccountsDedicated meldt of de idle-teller óók op een DEDICATED core loopt.
// Waar op ARM: de governor WFE't daar en meet de geslapen tijd. Op een gedeelde
// core meten beide architecturen (de yield beslaat de hele descheduled-periode),
// dus alleen dít geval verschilt — en wie een cpu-percentage rapporteert moet het
// weten: een teller die stilstaat leest als "100% bezig". Zie idle_riscv64.go.
func AccountsDedicated() bool { return true }

// wfeMinSleep (counter-ticks): de grens tussen "de WFE consumeerde alleen een
// verschaald event" en "de core heeft echt geslapen". Het getal is TIJD, geen
// tikken: 64 ticks was ~1-2,5µs op de 25-64MHz-tellers waarvoor het geschreven
// werd, maar op de M4's 1GHz-teller 64 nanoseconden — minder dan de WFE zelf
// kost, dus élke poging telde als slaap en de drain-lus hield op vóór de core
// ooit sliep (gemeten 29-08: 3,6M wakes/s bij 33% "slaap"). Enable tilt de
// waarde daarom naar ~2µs zodra de teller sneller is dan 32MHz; de 64 blijft
// de bodem, zodat elk bestaand board precies houdt wat het had.
var wfeMinSleep uint64 = 64

// governor: WFE's tot er écht geslapen is, met de counterstand eromheen — de
// geslapen tijd gaat de teller in. De lus is nodig omdat het event-register
// vrijwel altijd vol zit als we hier komen: elke exclusive (LDXR/STXR — de
// scheduler-transit én onze eigen atomics) zet op de N1 een wek-event, en de
// eerste WFE keert daardoor per direct terug (GEMETEN 18-07 op de Altra:
// 4,7M wakes/s, slaap 0,0µs — "idle" cores spinden op volle kracht en de
// idle-teller was ruis). De herhaalde WFE slaapt wél: tussen de iteraties
// staat geen enkele monitor-touch. Events wegslikken is veilig — tamago's
// Ms pollen (geen SEV-wek-afhankelijkheid) en de event-stream begrenst elke
// slaap op ~1,3ms; de cap dekt een externe event-storm (dan meten we eerlijk
// "geen slaap" en draait de scheduler gewoon door). De WFE-kant is bewust
// ongevoelig voor pollUntil (de event stream begrenst elke slaap op ~1,3ms,
// dus timers vuren hooguit ~1-2 periodes later — irrelevant voor jobs); de
// yield-kant geeft pollUntil juist wél door, als wektijd waar de rotatie deze
// bewoner tot die tijd mee overslaat.
func governor(pollUntil int64) {
	// De doorbell eerst: ligt er RX, dan is de pomp nu gewekt en is slapen
	// precies verkeerd; ligt er niets, dan is de drempel nu gewapend en
	// bewaakt de rotatie-peek de rest van deze slaap (zie rxdoor.go).
	if rxDoor() {
		countWake()
		return
	}
	var slept uint64
	if a := sharedAddr.Load(); a != 0 && dev.Read64(a) != 0 {
		// Gedeelde core: expliciet yielden, mét de wektijd. De HVC trapt naar
		// de EL2-switch, die onze staat opslaat, de core laat slapen, de
		// mede-bewoner draait en ons hier hervat — maar niet vóór de wektijd
		// (CtxWake), dus twee wachtende buren pingpongen niet. Eén yield per
		// idle-ronde: de switch doet zelf de WFE-slaap (power) en de rotatie.
		// Testbaar op QEMU, waar een WFE-trap dat niet zou zijn.
		slept = hvcYield(wakeAt(pollUntil))
	} else {
		// Dedicated core: WFE's tot er écht geslapen is (drain-lus, zie boven).
		for i := 0; slept < wfeMinSleep && i < 4; i++ {
			slept += wfeIdle()
		}
		// Bleef het bij het opeten van klaarstaande events, dan is er op dit
		// board nog één route naar echte slaap: de fysieke timer als deadline
		// (zie timerSleep). Boards die hem niet aanzetten draaien exact het
		// oude pad.
		if slept < wfeMinSleep && timerSleep {
			slept += sleepUntil(wakeAt(pollUntil))
		}
	}
	n := ticks.Add(slept)
	if a := pubAddr.Load(); a != 0 {
		dev.Write64(a, n)
	}
	countWake()
}
