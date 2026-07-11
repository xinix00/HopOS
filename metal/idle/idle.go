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

// wfe/cntkctlSet: zie idle_arm64.s.
func wfe()
func cntkctlSet(v uint64)

// De idle-tik-teller: één increment per governor-ronde. Omdat de
// event-stream een idle core élke ~1,2ms wekt, is het tempo van deze teller
// hét idle-signaal — vol tempo = idle, stilstand = er draait code. Apps
// publiceren hem op hun control-page (Publish → layout.CtrlIdle) zodat de
// klokwachter (metal/klok) op de HOP-core hem ziet; zonder Publish telt hij
// alleen intern (Ticks — zo leest de HOP-core zijn eigen tempo).
var (
	ticks   atomic.Uint64
	pubAddr atomic.Uintptr
)

// Enable zet de event-stream aan en hangt de WFE-governor in de runtime.
// EVNTI=15: event bij elke 0→1-flank van tellerbit 15 → periode 2^16 ticks
// (~1,2ms bij 54MHz op de Pi 5, ~1ms bij QEMU's 62,5MHz).
func Enable() {
	cntkctlSet(1<<2 | 15<<4) // EVNTEN | EVNTI=15
	goos.Idle = governor
}

// Publish laat de teller vanaf nu óók op addr landen — het CtrlIdle-woord
// van de eigen control-page (device-gemapt: gealigneerde 64-bit store, door
// HOP fysiek leesbaar). Bij SMP delen de cores van het slot dit woord; de
// wachter deelt het verwachte tempo door CtrlCores.
func Publish(addr uintptr) { pubAddr.Store(addr) }

// Ticks geeft de interne tellerstand.
func Ticks() uint64 { return ticks.Load() }

// governor: één WFE per idle-ronde. Bewust ongevoelig voor pollUntil: de
// event-stream begrenst de slaap, de scheduler doet de timer-administratie —
// geen deadline-rekenwerk, geen WFI-zonder-wekker-risico. De tel + store
// erna kosten ~ns op een rondetijd van ~1,2ms.
func governor(pollUntil int64) {
	wfe()
	n := ticks.Add(1)
	if a := pubAddr.Load(); a != 0 {
		dev.Write64(a, n)
	}
}
