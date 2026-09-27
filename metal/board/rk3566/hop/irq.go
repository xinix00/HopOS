//go:build tamago && arm64

package hop

// irq.go — de NIC-interrupt op de RK3566: de macirq van GMAC1 is een gewone
// SPI op de GIC-600, dezelfde weg als de RTL op de O6N (board/uefi/hop):
// GICv3-adapter aan deze core, de IRQ-deur, ack/rearm in de dwmac4-driver.
// De GIC-adressen komen uit de DT (rk356x-base.dtsi), de redistributor van
// deze core uit de GICR-range op MPIDR. Elke stap die faalt laat de node
// pollen, met de reden op de console. Op ijzer bewezen 20-09 (L83 p47), en
// sindsdien de default: 56 claims/s in rust, 944/s onder last, na twee fixes
// uit de Linux-bron in de driver — NIE is bit 15 op deze 4.20a-core, en de
// RX-descriptor draagt RDES3_INT_ON_COMPLETION_EN, anders komt de lijn nooit.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board/rk3566"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/gicv3"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/dwmac4"
)

var nicLine irq.Line

func wireNICIRQ(nic *dwmac4.Net) error {
	gicr, ok := gicv3.FindRedistributor(rk3566.GICRBase, rk3566.GICRLen, dev.MPIDR())
	if !ok {
		return fmt.Errorf("no redistributor for MPIDR %#x in %#x+%#x", dev.MPIDR()&0xffffff, rk3566.GICRBase, rk3566.GICRLen)
	}
	ctrl := gicv3.New(uintptr(rk3566.GICDBase), uintptr(gicr))
	fmt.Printf("irq: %s\n", ctrl.Describe())
	irq.Use(ctrl, idle.ServeIRQFlag)
	line := irq.Line{ID: rk3566.GMAC1INTID, Ack: nic.AckIRQ}
	if err := irq.Enable(line); err != nil {
		return err
	}
	nicLine = line
	nic.EnableIRQ()
	fmt.Printf("irq: NIC on INTID %d (SPI %d) routed to MPIDR %#x — RX wakes on the interrupt\n", line.ID, line.ID-32, dev.MPIDR()&0xffffff)
	return nil
}

// WaitNIC (board.NICInterrupter): wacht op de NIC-lijn, of val terug op de
// 300µs-poll zolang er geen bedrade lijn is. Geen rearm hier: dat doet de
// RX-pomp op zijn slaapmoment (hopnet.rxLoop, netdev.IRQRearmer).
func (machine) WaitNIC(max time.Duration) bool {
	if nicLine.ID == 0 {
		time.Sleep(300 * time.Microsecond)
		return false
	}
	return irq.Wait(nicLine, max)
}
