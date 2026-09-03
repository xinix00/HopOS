//go:build arm64

// De ARM64-helft van de idle-governor: WFE + de generic-timer-event-stream als
// default slaap, WFI op de fysieke timer als bewezen alternatief, en de
// HVC-yield voor een gedeelde core. Eén WFE per scheduler-ronde, en de
// event-stream begrenst elke slaap: de generic-timer-teller genereert elke
// ~1ms een wakeup-event (CNTKCTL_EL1.EVNTEN, geen GIC of interrupt-plumbing
// nodig), dus de scheduler kijkt hooguit ~1ms later weer naar zijn timers.
// Timers kunnen daardoor tot ~1ms later vuren — irrelevant voor jobs, en een
// SEV/interrupt wekt de core direct.
//
// Elke core roept Enable aan in zijn eigen hwinit1 (ná arm64.Init, die de
// default governor zet); CNTKCTL is per core. De RISC-V-helft
// (idle_riscv64.go) heeft geen WFE-equivalent en bereikt dezelfde slaap via
// de M-mode-switcher (yield met wektijd) of, voor HOP zelf, direct (de
// CLINT-Sleeper van het board). De gedeelde helft — Sleeper, tellers,
// publicatie, wakeAt, de idle-modus van het board — staat in idle.go.

package idle

import (
	"runtime"
	"runtime/goos"
	"sync/atomic"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
)

// wfeIdle/hvcYield/cntkctlSet/cntfrq/counterNow/mmfr0/wfiUntil: zie idle_arm64.s.
func wfeIdle() uint64
func wfiUntil(ticks uint64) uint64
func hvcYield(deadline uint64) uint64
func cntkctlSet(v uint64)
func cntfrq() uint64
func mmfr0() uint64

// Enable zet de event-stream aan en hangt de governor in de runtime, met
// WFESleep als slaap zolang het board niets anders koos (Use).
//
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
// oude keuze staan en wordt EVNTIS niet gezet. (Op de M4 komt dit uit op
// EVNTI 11: 2^20 ticks = 1,048ms = 954 wekken/s — precies wat er gemeten is.)
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
	if sleeper == nil {
		sleeper = WFESleep
	}
	goos.Idle = governor
}

// timerCap is de bovengrens van één deadline-slaap (WFISleep), in ticks.
var timerCap uint64

// WFESleep is de default-Sleeper: WFE's tot er écht geslapen is, met de
// counterstand eromheen. De lus is nodig omdat het event-register vrijwel
// altijd vol zit als we hier komen: elke exclusive (LDXR/STXR — de
// scheduler-transit én onze eigen atomics) zet op de N1 een wek-event, en de
// eerste WFE keert daardoor per direct terug (GEMETEN 18-07 op de Altra:
// 4,7M wakes/s, slaap 0,0µs — "idle" cores spinden op volle kracht en de
// idle-teller was ruis). De herhaalde WFE slaapt wél: tussen de iteraties
// staat geen enkele monitor-touch. Events wegslikken is veilig — tamago's
// Ms pollen (geen SEV-wek-afhankelijkheid) en de event-stream begrenst elke
// slaap op ~1,3ms; de cap dekt een externe event-storm (dan meten we eerlijk
// "geen slaap" en draait de scheduler gewoon door). Bewust ongevoelig voor
// wake: de event-stream begrenst elke slaap op ~1-2ms, dus timers vuren
// hooguit ~1-2 periodes later — irrelevant voor jobs.
func WFESleep(wake uint64) uint64 {
	var slept uint64
	for i := 0; slept < wfeMinSleep && i < 4; i++ {
		slept += wfeIdle()
	}
	return slept
}

// WFISleep is de Sleeper voor silicium waar WFE ín de scheduler-lus niet
// slaapt: eerst de WFE-drain, en leverde die niets op, dan de FYSIEKE TIMER
// als deadline (WFI met CNTP_TVAL). Alleen kiezen (Use) waar het board dit
// gemeten heeft — WFI raakt de interrupt-wereld van het board (een pending
// interrupt die niemand afhandelt maakt hem een no-op, een timer-FIQ die de
// core niet bereikt maakt hem een eeuwige slaap) en QEMU-TCG modelleert hem
// anders dan ijzer.
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
func WFISleep(wake uint64) uint64 {
	slept := WFESleep(wake)
	if slept < wfeMinSleep {
		slept += sleepUntil(wake)
	}
	return slept
}

// yieldSleep is de Sleeper voor silicium waar een app-core op EL1 niet kan
// slapen (de M4, gemeten 02-09: WFE keert direct terug en geen FIQ wekt een
// WFI — timer noch IPI). De slaap gebeurt dan waar hij wél werkt: op EL2,
// in de switcher (cpu/el2/switch.s), dezelfde weg als een gedeelde core —
// alleen woont deze app er alleen. De switcher slaapt daar met WFI en HOP's
// wekker kickt hem met een IPI als de wektijd (CtxWake) verstreken is of er
// RX ligt: m1n1's park-recept op ditzelfde silicium, met de wekker in HOP.
func yieldSleep(wake uint64) uint64 { return hvcYield(wake) }

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
// Waar op ARM: de governor slaapt daar en meet de geslapen tijd. Op een gedeelde
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

// governor: één melding per scheduler-ronde — "niets te doen tot T" — en dan
// óf de beurt afgeven (gedeelde core: HVC-yield naar de EL2-switch, mét de
// wektijd zodat twee wachtende buren niet pingpongen), óf slapen met de
// Sleeper van dit board. De geslapen tijd gaat de teller in.
func governor(pollUntil int64) {
	// Eerst de idle-modus van het board (CtrlIdleMode): op Apple is dat de
	// yield, en dan is alles hieronder de weg náár EL2.
	idleMode()
	// Een M zonder P is geen idle-core maar een wachter (runtime semasleep:
	// een mutex-wachter, een M in stopm tijdens een stop-the-world) — dat
	// komt alleen op een SMP-app voor. Voor hem alleen de slaap: geen
	// doorbell, geen WakeSleeper, niets dat een P nodig heeft (zie waitSleep).
	if _, pp := runtime.GetG(); pp == 0 {
		waitSleep(pollUntil)
		return
	}
	// Re-entrantie: de governor kan zichzelf aanroepen — WakeSleeper neemt de
	// timer-lock, en is die bezet (de andere core), dan slaapt lock2 via
	// semasleep → goos.Idle → hier. Dan alleen slapen; de unlocker kickt ons
	// (semawakeup → goos.Wake). Anders: governor → WakeSleeper → lock2 →
	// semasleep → governor → … tot de stack op is, terwijl de andere core
	// 200 miljoen keer per twee minuten kickt (QEMU 03-09, gdb-stackwalk).
	c := coreIndex()
	if nested[c].Swap(true) {
		waitSleep(pollUntil)
		return
	}
	defer nested[c].Store(false)
	// De doorbell: ligt er RX, dan is de pomp nu gewekt en is slapen precies
	// verkeerd; ligt er niets, dan is de drempel nu gewapend en bewaakt de
	// rotatie-peek de rest van deze slaap (zie rxdoor.go).
	if rxDoor() || workDoor() {
		countWake()
		return
	}
	var slept uint64
	if a := sharedAddr.Load(); a != 0 && dev.Read64(a) != 0 {
		// Gedeelde core: expliciet yielden, mét de wektijd. De HVC trapt naar
		// de EL2-switch, die onze staat opslaat, de core laat slapen, de
		// mede-bewoner draait en ons hier hervat — maar niet vóór de wektijd
		// (CtxWake). Eén yield per idle-ronde: de switch doet zelf de
		// WFE-slaap (power) en de rotatie. Testbaar op QEMU, waar een WFE-trap
		// dat niet zou zijn.
		slept = hvcYield(wakeAt(pollUntil))
	} else {
		if yieldMode.Load() {
			slept = yieldSleep(wakeAt(pollUntil))
		} else {
			slept = sleeper(wakeAt(pollUntil))
		}
	}
	n := ticks.Add(slept)
	if a := pubAddr.Load(); a != 0 {
		dev.Write64(a, n)
	}
	countWake()
}

// yieldMode: het board vraagt om yield-idle (layout.IdleYield). Een vlag en
// géén func-waarde in `sleeper`: de governor draait óók op een M zonder P
// (semasleep tijdens een stop-the-world, alleen op een SMP-app), en een
// pointer-store draagt daar een write barrier die p.wbBuf van een nil P
// leest — de stage-2-fault op de tweede core (QEMU 03-09: gcWriteBarrier,
// FAR 0x2550). Een uint32 heeft geen barrier.
var yieldMode atomic.Bool

// applyIdleMode neemt de idle-modus van het board over (layout.Idle*).
func applyIdleMode(mode uint64) { yieldMode.Store(mode&layout.IdleYield != 0) }

// waitSleep is de slaap van een M zonder P (runtime semasleep: een
// mutex-wachter, een M in stopm tijdens een stop-the-world). Niets van de
// governor: geen doorbell, geen WakeSleeper, geen pointer-store (een write
// barrier leest daar p.wbBuf van een nil P — de stage-2-fault van 03-09).
// Alleen slapen tot de wektijd of tot een sibling hem via goos.Wake kickt
// (semawakeup) — dat is wat zo'n wachter nodig heeft. Zonder yield: de Sleeper.
func waitSleep(deadline int64) {
	if yieldMode.Load() {
		hvcYield(wakeAt(deadline) | layout.CtxWakeNoPeek)
	} else {
		sleeper(wakeAt(deadline))
	}
}

// nested: per core "de governor loopt al" (zie governor). Geïndexeerd op de
// affiniteit uit MPIDR: aff0 (core in cluster) en twee bits van aff1 (het
// cluster), zodat een app die over clusters heen spant geen slot deelt.
var nested [64]atomic.Bool

func coreIndex() int {
	m := dev.MPIDR()
	return int(m&0xF) | int((m>>8)&0x3)<<4
}
