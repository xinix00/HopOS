//go:build tamago && arm64

package idle

import (
	"os"
	"os/signal"

	"github.com/usbarmory/tamago/arm64"
)

// irqEnable: zie door_arm64.s — alleen I open, F dicht.
func irqEnable()

// ServeIRQ is tamago's ServiceInterrupts met alleen I open: de IRQ-vector
// maskeert bij binnenkomst, meldt het signaal, en pas als deze goroutine weer
// wacht gaat I opnieuw open. Voor cpu/irq.Use op een board waar F van de
// timer is (Apple); elders volstaat arm64.ServiceInterrupts.
func ServeIRQ(isr func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, arm64.IRQ_SIGNAL)
	for n := 0; ; n++ {
		go irqEnable()
		<-c
		if n == 0 {
			println("irq: first IRQ exception reached Go")
		}
		isr()
	}
}
