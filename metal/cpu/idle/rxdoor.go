package idle

// De DOORBELL: de app-helft van "een stille core die tóch direct reageert".
//
// De RX-pomp (applib/appnet) polde zijn frame-ring elke 300µs, en dat was op
// een idle app ~99% van alles wat hij deed: 3.333 scheduler-rondes per
// seconde, op een gedeelde core elk een volledige context-wissel (gemeten
// 29-08, schedbench). De adaptieve slaap drukte dat al 14×, maar kocht dat
// met wek-latency: het eerste pakket ná stilte wacht de hele cap.
//
// De doorbell haalt die koppeling uit elkaar, met het EVENT_IDX-idee van
// virtio: de consument zegt waar hij gebleven is, de wereld wekt hem alleen
// als er iets NIEUWS is. Drie stukken, alle drie hier of in de switcher:
//
//  1. WAPENEN (hier, elke idle-ronde): ligt er niets, dan schrijft de
//     governor "gezien tot head H, wek me erna" op CtrlRXDoor (H | bit 63)
//     en slaapt gewoon. De pomp mag dan een cap van seconden hebben.
//  2. PEEKEN (cpu/el2 + cpu/mmode): de rotatie vergelijkt bij een bewoner
//     wiens wektijd nog niet om is de drempel met het live head-woord
//     (CtxRingHeadPA). Verschil = er kwam verkeer = hij is tóch due. De
//     core zelf wordt sowieso elke ~1-2ms wakker (ARM event-stream,
//     RISC-V SleepCap), dus dit kost geen extra wekker.
//  3. WEKKEN (hier, zodra de ronde wél iets ziet): runtime.WakeSleeper stopt
//     de slaap-timer van de pomp-goroutine onder de timer-lock en zet hem
//     klaar via het gewone ready-pad. NIET runtime.Wake (tamago's WakeG):
//     die herschrijft de timer-heap zonder lock, wat op één core klopt (de
//     scheduler staat stil als de idle-hook draait) en op een SMP-app de
//     heap van de andere core sloopt — een geheapte timer zonder heap,
//     gemeten op de M4 03-09. WakeSleeper is onze toevoeging aan de
//     tamago-go-fork (tools/tamago-go/).
//
// Het gewapend-teken (bit 63) is een grens, geen detail: een app die zijn
// ring nooit leest (geen netstack) wapent nooit, dus de peek laat hem met
// rust — anders maakte élke ARP-flood hem permanent "due" en at de
// resume/yield-pingpong de gedeelde core op.

import (
	"runtime"
	"sync/atomic"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// rxArmed is het gewapend-teken in het CtrlRXDoor-woord. Head is een
// byte-index en haalt bit 63 nooit.
const rxArmed = uint64(1) << 63

var (
	rxStatus atomic.Pointer[func() (uint64, bool)] // head + pending van de RX-ring
	rxDoorPA atomic.Uintptr                        // CtrlRXDoor op de eigen control-page
	rxPumpG  atomic.Uint64                         // g-pointer van de slapende RX-pomp
	work     atomic.Pointer[func() bool]           // HOP: gedeeld werk achter een doorbell
	workWake atomic.Pointer[func()]                // maakt HOP's lokale pomp runnable
)

// WatchRXRing hangt de doorbell aan: door is het CtrlRXDoor-woord van de
// eigen control-page, status leest (head, pending) van de RX-ring. Eén keer,
// vanuit appnet.Up.
func WatchRXRing(door uintptr, status func() (uint64, bool)) {
	rxDoorPA.Store(door)
	rxStatus.Store(&status)
}

// PumpSleep wapent de deurbel namens de pomp vlak vóór zijn slaap: "ik slaap
// vanaf kop H — is er iets bij, wek me". Meldt onwaar als er intussen tóch
// iets ligt; dan slaapt de pomp niet maar pompt hij. Dat is de klassieke
// wapen-en-hercontroleer-stap: zonder de hercontrole valt een frame dat net
// vóór het wapenen kwam tussen wal en schip. HOP's kick (ook de vFIQ voor een
// draaiende core, door_arm64.go) komt alleen als de deurbel gewapend is —
// één kick per burst, niet per frame: een kick per frame kostte de bulk
// HOP → app 3,5× (534 → 150 MB/s, gemeten 04-09).
func PumpSleep() bool {
	f := rxStatus.Load()
	door := rxDoorPA.Load()
	if f == nil || door == 0 {
		return true
	}
	head, pending := (*f)()
	if pending {
		return false
	}
	dev.Write64(door, head|rxArmed)
	dev.Push(door, 8)
	if _, pending := (*f)(); pending {
		PumpAwake()
		return false
	}
	return true
}

// PumpAwake ontwapent de deurbel: de pomp draait, kicks zijn nu overbodig.
func PumpAwake() {
	if door := rxDoorPA.Load(); door != 0 {
		dev.Write64(door, 0)
		dev.Push(door, 8)
	}
}

// RXPumpG registreert de goroutine van de RX-pomp als wek-doel; aanroepen op
// de pomp-goroutine zelf, vóór zijn eerste slaap (runtime.GetG geeft de gp).
func RXPumpG(gp uint) { rxPumpG.Store(uint64(gp)) }

// WatchWork is de HOP-variant van hetzelfde level-triggered contract. Een
// producer doet dev.Notify na leeg→niet-leeg; zodra het event de idle core
// wekt, kijkt de governor naar status en belt hij de lokale pomp. Die bel is
// bewust een callback: de HOP-pomp wacht op een Go-kanaal, niet op de
// time.Sleep-timer waarvoor runtime.WakeSleeper bedoeld is. Apps registreren
// hier niets; hun RX gebruikt de control-pagevariant boven.
func WatchWork(status func() bool, wake func()) {
	workWake.Store(&wake)
	work.Store(&status)
}

func workDoor() bool {
	f := work.Load()
	if f == nil {
		return false
	}
	// Wekken is hier "een goroutine runnable maken" (kanaal-send, en ook de
	// mutex-Unlock in status met wachters), en dat mag alleen in de idle van
	// de scheduler zelf: P vast, geen lock2 in gang. De hook draait óók vanuit
	// semasleep — een M zonder P, of een mutex-wachter — en dan sloopt
	// ready/wakep de scheduler: runqput op een nil-P (gemeten 03-09, de eerste
	// WakeSleeper), of een tweede slaap op dezelfde m.mWaitList. Het werk
	// blijft dan liggen tot de volgende echte idle-ronde of de failsafe van de
	// switch (hopswitch.loop); rxDoor hoeft dit niet — WakeSleeper is per
	// ontwerp vanaf elke M veilig.
	if !runtime.IdleMayReady() {
		WorkNotReady.Add(1)
		return false
	}
	if !(*f)() {
		WorkIdle.Add(1)
		return false
	}
	wake := workWake.Load()
	if wake == nil {
		WorkWakeFailed.Add(1)
		return false
	}
	(*wake)()
	WorkWoken.Add(1)
	return true
}

// rxDoor is de eerste stap van élke governor-ronde. Niets te doen → drempel
// wapenen en false (ga gewoon slapen; de peek waakt). Wel iets → drempel
// ontwapenen en de pomp wekken; true betekent "niet slapen, de scheduler
// heeft er net werk bij". Een mislukte Wake (de pomp draait al — SMP, of hij
// is nog niet geregistreerd) is geen fout: de check is level-triggered, dus
// zolang er iets ligt komt elke volgende ronde hier terug.
func rxDoor() bool {
	f := rxStatus.Load()
	if f == nil {
		return false
	}
	if _, pending := (*f)(); !pending {
		return false
	}
	// De deurbel zelf blijft van de pomp: PumpSleep wapent hem vlak vóór de
	// slaap, PumpAwake ontwapent hem erna. De governor schreef hem vroeger
	// ook, maar op een 2-core-app is dat een race met de pomp op de andere
	// core (een ontwapening ná diens wapening = verloren wek, tot een
	// seconde). Wekken volstaat: slaapt de pomp nog tussen wapenen en parkeren,
	// dan mislukt de wek, blijft de deur gewapend en kickt HOP's wekker ons
	// binnen een milliseconde opnieuw.
	gp := rxPumpG.Load()
	if gp == 0 {
		DoorNoPump.Add(1)
		return false
	}
	woken, remote := runtime.WakeSleeper(uint(gp))
	if !woken {
		DoorWakeFailed.Add(1)
		return false
	}
	DoorWoken.Add(1)
	if remote {
		// De pomp hoort bij de andere core en die is gekickt; terug naar
		// onze scheduler levert niets op — die vond net niets en zou hier
		// meteen weer staan, met een IPI naar de pomp-core per ronde (319k/s
		// op de M4, 04-09: de pomp-core verloor de helft van zijn tijd aan
		// het afhandelen). Slapen dus; de volgende burst kickt ons wel weer.
		DoorWokenRemote.Add(1)
		return false
	}
	return true
}

// DoorNoPump/DoorWoken/DoorWakeFailed: de meetlat van de bel — rondes zonder
// geregistreerde pomp, gelukte en mislukte wekpogingen (WakeSleeper: mislukt =
// de pomp sliep net niet). Een app kan ze tonen (vitals).
var DoorNoPump, DoorWoken, DoorWakeFailed, DoorWokenRemote atomic.Uint64

// WakeSleeperIdleP/Self: de lokale gevallen van WakeSleeper (runtime-tellers).
func WakeSleeperIdleP() uint64 { return runtime.WakeSleeperIdleP.Load() }
func WakeSleeperSelf() uint64  { return runtime.WakeSleeperSelf.Load() }

var WorkWoken, WorkWakeFailed atomic.Uint64

// WorkNotReady/WorkIdle: de twee manieren waarop workDoor níet wekt — de hook
// draaide buiten de scheduler-idle (IdleMayReady onwaar), of er lag niets.
var WorkNotReady, WorkIdle atomic.Uint64
