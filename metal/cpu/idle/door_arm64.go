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
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"

	"github.com/usbarmory/tamago/arm64"
)

// hvcDoorAck: HVC #5 naar de switcher van deze core; fiqEnable: alleen F
// open (DAIFClr #1) — zie door_arm64.s. Niet tamago's irq_enable: die opent
// ook I, en op een app-core kan een fysieke IRQ pending staan die niemand
// ackt (de boot-core van de M4 slaapt om die reden al niet in WFI). Met I
// open wordt dat een storm die de app op 100% houdt vóór zijn eerste
// logregel — gemeten 04-09 op core 2. Wij hebben alleen de virtuele FIQ nodig.
func hvcDoorAck()
func fiqEnable()

// DoorIRQs/DoorIRQWoken: hoe vaak de interrupt kwam en hoe vaak hij de pomp
// echt wekte (de rest was al wakker, of er lag niets meer).
var DoorIRQs, DoorIRQWoken atomic.Uint64

// ServeDoorIRQ start de ISR-goroutine en meldt of deze architectuur het kan.
// De aanroeper zet daarna layout.CtrlDoorIRQ op zijn control-page; tot dan
// kickt HOP een draaiende core niet en injecteert de switcher niets.
func ServeDoorIRQ() bool {
	go serveDoor()
	return true
}

// serveDoor is tamago's ServiceInterrupts-lus met alleen F open: de EL1-
// vector maskeert bij binnenkomst, meldt het signaal, en pas als deze
// goroutine weer wacht gaat F opnieuw open — een verloren vFIQ bestaat niet,
// hij blijft pending tot de ack (level).
func serveDoor() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, arm64.IRQ_SIGNAL)
	for {
		go fiqEnable()
		<-c
		doorISR()
	}
}

// doorISR: level-triggered, net als rxDoor — kijk of er RX ligt, wek de pomp
// als hij slaapt, en ack. De deurbel-arming is van de pomp (PumpSleep).
func doorISR() {
	DoorIRQs.Add(1)
	if f := rxStatus.Load(); f != nil {
		if _, pending := (*f)(); pending {
			if gp := rxPumpG.Load(); gp != 0 {
				if woken, _ := runtime.WakeSleeper(uint(gp)); woken {
					DoorIRQWoken.Add(1)
				}
			}
		}
	}
	hvcDoorAck()
}
