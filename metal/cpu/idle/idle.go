// Package idle laat een core écht slapen als zijn Go-runtime niets te doen
// heeft — het antwoord op "jobs die vooral staan te idlen" (Derek), zonder
// DVFS-beleid: een core in WFE is clock-gated en verbruikt vrijwel niets,
// op elke kloksnelheid.
//
// tamago's scheduler roept goos.Idle aan bij een lege runqueue, maar de
// default governor slaapt alleen als er *helemaal geen* timer meer loopt —
// elke idle job heeft timers (heartbeat, polls), dus in de praktijk spint
// hij. Deze governor doet in plaats daarvan één WFE per scheduler-ronde en
// leunt op de ARM event-stream: de generic-timer-teller genereert elke
// ~1ms een wakeup-event (CNTKCTL_EL1.EVNTEN, geen GIC of interrupt-plumbing
// nodig), dus de scheduler kijkt hooguit ~1ms later weer naar zijn timers.
// Timers kunnen daardoor tot ~1ms later vuren — irrelevant voor jobs, en
// een SEV/interrupt wekt de core direct.
//
// Elke core roept Enable aan in zijn eigen hwinit1 (ná arm64.Init, die de
// default governor zet); CNTKCTL is per core.
package idle

import (
	"runtime/goos"
	"sync/atomic"

	"hop-os/metal/dev"
)

// wfeIdle/hvcYield/cntkctlSet/cntfrq: zie idle_arm64.s.
func wfeIdle() uint64
func hvcYield() uint64
func cntkctlSet(v uint64)
func cntfrq() uint64

// De idle-teller: geaccumuleerde idle-TIJD in generic-timer-ticks (CNTFRQ-
// eenheden) — de counterstand vóór en ná elke WFE, delta erbij. Een vol
// idle core stijgt dus ~CounterHz per seconde, een rekenende core staat
// stil; de verhouding ís de idle-fractie. Apps publiceren hem op hun
// control-page (Publish → layout.CtrlIdle) zodat HOP hem ziet (dvfs-beleid,
// per-slot CPU-meting); zonder Publish telt hij alleen intern (Ticks).
//
// Waarom tijd en niet rondes (de eerste vorm, herzien 18-07): WFE wekt óók
// op SEV's van andere cores en spurious events — op de drukke Altra tikte
// een slapende app daardoor ver bóven het event-stream-tempo en las elke
// deels-idle app als "vol idle" (ijzer-meting: DUTY=25/50/75 → allemaal
// cpu=0%, alleen 100 klopte). Tijd tellen is ruis-immuun: een valse wake
// telt zijn echte (micro)duur mee in plaats van een volle tik, zonder
// per-core-status.
var (
	ticks      atomic.Uint64
	pubAddr    atomic.Uintptr
	sharedAddr atomic.Uintptr // CtrlShared-woord van de eigen control-page (0 = niet gezet)
)

// Enable zet de event-stream aan en hangt de WFE-governor in de runtime.
// EVNTI kiest de counterbit waarvan de 0→1-flank het wek-event is; we pakken
// de bit die het dichtst bij ~1ms periode blijft (2^(EVNTI+1)/CNTFRQ):
// bit 15 op de Pi's 54MHz (1,2ms) en QEMU's 62,5MHz (1,05ms), bit 14 op de
// Altra's 25MHz (1,3ms — een vaste 15 gaf daar 2,6ms wek-granulariteit).
func Enable() {
	i := uint64(15) // EVNTI is 4 bits: 15 is tegelijk het maximum én de start
	for i > 4 && (uint64(1)<<(i+1))*2000 > cntfrq()*3 { // periode > 1,5ms → fijnere bit
		i--
	}
	cntkctlSet(1<<2 | i<<4) // EVNTEN | EVNTI
	goos.Idle = governor
}

// Publish laat de teller vanaf nu óók op addr landen — het CtrlIdle-woord
// van de eigen control-page (device-gemapt: gealigneerde 64-bit store, door
// HOP fysiek leesbaar). Bij SMP delen de cores van het slot dit woord; de
// wachter deelt het verwachte tempo door CtrlCores.
func Publish(addr uintptr) { pubAddr.Store(addr) }

// WatchShared laat de governor het CtrlShared-woord op addr lezen (de eigen
// control-page): is het ≠ 0, dan deelt dit slot zijn core en yieldt de
// governor expliciet via HVC i.p.v. te WFE'en. HOP zet/wist het woord
// dynamisch (kern/slots), dus we lezen het élke idle-ronde vers — één
// device-lees, verwaarloosbaar op een idle core. applib roept dit in Init.
func WatchShared(addr uintptr) { sharedAddr.Store(addr) }

// Ticks geeft de interne tellerstand.
func Ticks() uint64 { return ticks.Load() }

// CounterHz is de eenheid van de teller: generic-timer-ticks per seconde
// (CNTFRQ). Een vólledig idle core accumuleert ~CounterHz per seconde —
// wie de teller leest (dvfs-beleid, per-slot CPU-meting in kern/slotmgr)
// normeert tegen dít tempo. LET OP QEMU-TCG: WFE is daar een no-op, dus
// idle-tijd meet er ~0 — idle-metingen zijn ijzer-metingen.
func CounterHz() uint64 { return cntfrq() }

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
// "geen slaap" en draait de scheduler gewoon door). Bewust ongevoelig voor
// pollUntil: de scheduler doet de timer-administratie, timers kunnen dus
// ~1-2 event-periodes later vuren — irrelevant voor jobs.
func governor(pollUntil int64) {
	var slept uint64
	if a := sharedAddr.Load(); a != 0 && dev.Read64(a) != 0 {
		// Gedeelde core: expliciet yielden. De HVC trapt naar de EL2-switch,
		// die onze staat opslaat, de core laat slapen, de mede-bewoner draait
		// en ons hier hervat. Eén yield per idle-ronde: de switch doet zelf de
		// WFE-slaap (power) en de rotatie. Testbaar op QEMU, waar een WFE-trap
		// dat niet zou zijn.
		slept = hvcYield()
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
}
