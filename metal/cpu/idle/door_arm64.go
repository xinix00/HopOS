//go:build tamago && arm64

package idle

// De RX-doorbell als interrupt. De doorbell in rxdoor.go leeft in de idle-
// governor: wekt HOP een app-core met zijn fast-IPI, dan ziet de governor bij
// zijn volgende ronde dat er RX ligt en wekt hij de pomp. Dat werkt zolang de
// core idle wordt — een core die druk is (de GC na een reeks MiB-allocaties,
// een rekenlus) komt daar niet, en dan wacht elke response op de poll-timer
// van de pomp: ~1ms per call, gemeten 04-09 op de M4. Hier wordt HOP's kick
// een échte interrupt: de EL2-switcher zet bij de IPI een virtuele FIQ
// (switch.s fiq:), tamago's EL1-vector maakt er zijn interrupt-signaal van,
// en deze ISR wekt de pomp. Ack via HVC #5 (de switcher haalt VF weer weg).
// Alleen voor een app met één core: HCR_EL2 is per core, en de ack moet op
// dezelfde core landen als de injectie (appnet bewaakt dat).

import (
	"runtime"
	"sync/atomic"

	"github.com/usbarmory/tamago/arm64"
)

// hvcDoorAck: HVC #5 naar de switcher van deze core — zie door_arm64.s.
func hvcDoorAck()

// DoorIRQs/DoorIRQWoken: hoe vaak de interrupt kwam en hoe vaak hij de pomp
// echt wekte (de rest was al wakker, of er lag niets meer).
var DoorIRQs, DoorIRQWoken atomic.Uint64

// ServeDoorIRQ start de ISR-goroutine en meldt of deze architectuur het kan.
// De aanroeper zet daarna layout.CtrlDoorIRQ op zijn control-page; tot dan
// kickt HOP een draaiende core niet en injecteert de switcher niets.
func ServeDoorIRQ() bool {
	go arm64.ServiceInterrupts(doorISR)
	return true
}

// doorISR: level-triggered, net als rxDoor — kijk of er RX ligt, wek de pomp
// als hij slaapt, en ack. De deurbel-arming laat hij aan de governor.
func doorISR() {
	DoorIRQs.Add(1)
	if f := rxStatus.Load(); f != nil {
		if _, pending := (*f)(); pending {
			if gp := rxPumpG.Load(); gp != 0 && runtime.WakeSleeper(uint(gp)) {
				DoorIRQWoken.Add(1)
			}
		}
	}
	hvcDoorAck()
}
