// Package idle laat een core écht slapen als zijn Go-runtime niets te doen
// heeft — het antwoord op "jobs die vooral staan te idlen" (Derek), zonder
// DVFS-beleid: een slapende core is clock-gated en verbruikt vrijwel niets,
// op elke kloksnelheid.
//
// tamago's scheduler roept goos.Idle aan bij een lege runqueue, maar de
// default governor slaapt alleen als er *helemaal geen* timer meer loopt —
// elke idle job heeft timers (heartbeat, polls), dus in de praktijk spint
// hij. De governor hier meldt per scheduler-ronde "ik doe niets tot T" langs
// het pad dat bij de architectuur en de rol van de core hoort; hóe er dan
// geslapen wordt staat in idle_arm64.go (WFE + event-stream, HVC-yield) en
// idle_riscv64.go (ecall-yield, MSleep). Dit bestand is de gedeelde helft:
// de tellers, hun publicatie en de wektijd-vertaling.
package idle

import (
	"sync/atomic"
	_ "unsafe" // voor go:linkname naar runtime.nanotime
)

// De idle-teller: geaccumuleerde idle-TIJD in counter-ticks (CounterHz-
// eenheden) — de counterstand vóór en ná elke slaap, delta erbij. Een vol
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

// counterNow is de rauwe stand van de teller waarin wektijden worden
// uitgedrukt: CNTVCT_EL0 op arm64, de TIME-CSR op riscv64 (idle_<arch>.s).
func counterNow() uint64

// nanotime is de klok waarin de scheduler zijn pollUntil uitdrukt; wakeAt
// rekent die om naar de counterstand die de switcher of slaap-primitief van
// deze architectuur begrijpt.
//
//go:linkname nanotime runtime.nanotime
func nanotime() int64

// wakeAt vertaalt de pollUntil van de scheduler naar de counterstand waarop
// deze bewoner op zijn vroegst terug wil; 0 = nu.
//
// pollUntil is de eerstvolgende timer-deadline van dit slot, of 0 als er
// helemaal geen timer loopt. Alles wat geen bruikbare toekomst oplevert wordt
// 0 = "nu meteen weer", en dat is precies het gedrag van vóór de wektijden —
// de terugval is dus altijd de oude, bewezen kant.
//
// Verleidelijk maar NIET gedaan: "geen timer" als oneindig lezen. Dat klopt
// theoretisch (zonder timer kan er zonder gebeurtenis van buiten niets
// veranderen), maar zolang HOP nog geen wek-IPI stuurt is er dan ook niets
// dat zo'n bewoner ooit nog terughaalt. Dat is geen zuinigheid maar een hang,
// en die mag niet in de code staan wachten op de dag dat iemand hem aanzet.
func wakeAt(pollUntil int64) uint64 {
	if pollUntil <= 0 {
		return 0
	}
	d := pollUntil - nanotime()
	if d <= 0 {
		return 0
	}
	return counterNow() + uint64(d)*CounterHz()/1_000_000_000
}

// Publish laat de teller vanaf nu óók op addr landen — het CtrlIdle-woord
// van de eigen control-page (device-gemapt: gealigneerde 64-bit store, door
// HOP fysiek leesbaar). Bij SMP delen de cores van het slot dit woord; de
// wachter deelt het verwachte tempo door CtrlCores.
func Publish(addr uintptr) { pubAddr.Store(addr) }

// WatchShared geeft de governor het CtrlShared-woord van de eigen
// control-page in handen; applib roept dit in Init aan, op beide
// architecturen. Wat het woord betekent verschilt per kant: op arm64 kiest de
// WAARDE per idle-ronde het pad (≠ 0 = gedeelde core → HVC-yield i.p.v. WFE;
// HOP zet/wist het woord dynamisch via kern/slots, dus élke ronde één verse
// device-lees — verwaarloosbaar op een idle core), op riscv64 is het ADRES
// zelf het signaal dat er een switcher boven ons zit en yieldt een slot-app
// áltijd — alleen wonen is daar een rotatie van één (zie idle_riscv64.go).
func WatchShared(addr uintptr) { sharedAddr.Store(addr) }

// Ticks geeft de interne tellerstand.
func Ticks() uint64 { return ticks.Load() }
