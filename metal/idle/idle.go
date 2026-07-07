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

import "runtime/goos"

// wfe/cntkctlSet: zie idle_arm64.s.
func wfe()
func cntkctlSet(v uint64)

// Enable zet de event-stream aan en hangt de WFE-governor in de runtime.
// EVNTI=15: event bij elke 0→1-flank van tellerbit 15 → periode 2^16 ticks
// (~1,2ms bij 54MHz op de Pi 5, ~1ms bij QEMU's 62,5MHz).
func Enable() {
	cntkctlSet(1<<2 | 15<<4) // EVNTEN | EVNTI=15
	goos.Idle = governor
}

// governor: één WFE per idle-ronde. Bewust ongevoelig voor pollUntil: de
// event-stream begrenst de slaap, de scheduler doet de timer-administratie —
// geen deadline-rekenwerk, geen WFI-zonder-wekker-risico.
func governor(pollUntil int64) {
	wfe()
}
