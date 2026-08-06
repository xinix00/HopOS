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

// wfeIdle/hvcYield/cntkctlSet/cntfrq/counterNow: zie idle_arm64.s.
func wfeIdle() uint64
func hvcYield(deadline uint64) uint64
func cntkctlSet(v uint64)
func cntfrq() uint64

// Enable zet de event-stream aan en hangt de WFE-governor in de runtime.
// EVNTI kiest de counterbit waarvan de 0→1-flank het wek-event is; we pakken
// de bit die het dichtst bij ~1ms periode blijft (2^(EVNTI+1)/CNTFRQ):
// bit 15 op de Pi's 54MHz (1,2ms) en QEMU's 62,5MHz (1,05ms), bit 14 op de
// Altra's 25MHz (1,3ms — een vaste 15 gaf daar 2,6ms wek-granulariteit).
func Enable() {
	i := uint64(15)                                     // EVNTI is 4 bits: 15 is tegelijk het maximum én de start
	for i > 4 && (uint64(1)<<(i+1))*2000 > cntfrq()*3 { // periode > 1,5ms → fijnere bit
		i--
	}
	cntkctlSet(1<<2 | i<<4) // EVNTEN | EVNTI
	goos.Idle = governor
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

// wfeMinSleep (counter-ticks, ~1-2,5µs op 25-64MHz): de grens tussen "de WFE
// consumeerde alleen een verschaald event" en "de core heeft echt geslapen".
const wfeMinSleep = 64

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
	}
	n := ticks.Add(slept)
	if a := pubAddr.Load(); a != 0 {
		dev.Write64(a, n)
	}
	countWake()
}
