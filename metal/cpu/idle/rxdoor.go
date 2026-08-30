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
//  3. WEKKEN (hier, zodra de ronde wél iets ziet): runtime.Wake zet de
//     slaap-timer van de pomp-goroutine op "nu" — het primitief dat tamago
//     voor bare-metal interrupt-handlers heeft, en dit is er precies één.
//
// Het gewapend-teken (bit 63) is een grens, geen detail: een app die zijn
// ring nooit leest (geen netstack) wapent nooit, dus de peek laat hem met
// rust — anders maakte élke ARP-flood hem permanent "due" en at de
// resume/yield-pingpong de gedeelde core op.

import (
	"runtime"
	"sync/atomic"

	"github.com/xinix00/HopOS/metal/dev"
)

// rxArmed is het gewapend-teken in het CtrlRXDoor-woord. Head is een
// byte-index en haalt bit 63 nooit.
const rxArmed = uint64(1) << 63

var (
	rxStatus atomic.Pointer[func() (uint64, bool)] // head + pending van de RX-ring
	rxDoorPA atomic.Uintptr                        // CtrlRXDoor op de eigen control-page
	rxPumpG  atomic.Uint64                         // g-pointer van de slapende RX-pomp
)

// WatchRXRing hangt de doorbell aan: door is het CtrlRXDoor-woord van de
// eigen control-page, status leest (head, pending) van de RX-ring. Eén keer,
// vanuit appnet.Up.
func WatchRXRing(door uintptr, status func() (uint64, bool)) {
	rxDoorPA.Store(door)
	rxStatus.Store(&status)
}

// RXPumpG registreert de goroutine van de RX-pomp als wek-doel; aanroepen op
// de pomp-goroutine zelf, vóór zijn eerste slaap (runtime.GetG geeft de gp).
func RXPumpG(gp uint) { rxPumpG.Store(uint64(gp)) }

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
	head, pending := (*f)()
	door := rxDoorPA.Load()
	if !pending {
		dev.Write64(door, head|rxArmed)
		return false
	}
	dev.Write64(door, 0) // ontwapenen: er wordt aan gewerkt
	if gp := rxPumpG.Load(); gp != 0 && runtime.Wake(uint(gp)) {
		return true
	}
	return false
}
