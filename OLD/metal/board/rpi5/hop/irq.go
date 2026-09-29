//go:build tamago && arm64

package hop

// irq.go — de NIC-interrupt op de Pi 5. De GEM zit in de RP1 achter PCIe en
// heeft geen INTx: de RP1 vuurt élke peripheral-interrupt als MSI-X af naar
// de MIP van de BCM2712, die er GIC-SPI 128+vector van maakt (GIC-400). Drie
// stukken bedrading, elk naar de Linux-bron: de MSI-X-tabel van de RP1
// (PCI-capability 0x11, generiek), het MSIX_CFG-woord van de RP1 (rp1.c),
// en de MIP (irq-bcm2712-mip.c). Vector = het RP1-IRQ-nummer, zoals Linux
// ze 1:1 toewijst (pci_alloc_irq_vectors(RP1_IRQS)). Elke stap die faalt
// laat de node pollen, met de reden op de console.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board/rpi5"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/brcmpcie"
	"github.com/xinix00/HopOS/metal/v2/driver/gicv2"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/gem"
)

const (
	pciCapPtr      = 0x34
	pciCapMSIX     = 0x11
	msixEnable     = 1 << 31 // Message Control bit 15, in het dword op de cap
	msixFuncMask   = 1 << 30 // Message Control bit 14
	msixEntryBytes = 16
)

var nicLine irq.Line

// wireNICIRQ bedraadt de GEM-interrupt: RP1-MSI-X-entry → MIP → GIC → HOP.
func wireNICIRQ(rc *brcmpcie.RC, nic *gem.Net) error {
	// 1. De MSI-X-capability van de RP1 (bus 1, dev 0): tabel-BAR en -offset.
	ptr := uintptr(rc.CfgRead32(1, 0, 0, pciCapPtr) & 0xff)
	cap := uintptr(0)
	for i := 0; ptr >= 0x40 && i < 48; i++ {
		hdr := rc.CfgRead32(1, 0, 0, ptr)
		if hdr&0xff == pciCapMSIX {
			cap = ptr
			break
		}
		ptr = uintptr(hdr >> 8 & 0xff)
	}
	if cap == 0 {
		return fmt.Errorf("rp1: no MSI-X capability")
	}
	hdr := rc.CfgRead32(1, 0, 0, cap)
	entries := int(hdr>>16&0x7ff) + 1
	tab := rc.CfgRead32(1, 0, 0, cap+4)
	bir, off := tab&7, uintptr(tab&^7)
	bar := uintptr(rc.CfgRead32(1, 0, 0, 0x10+4*uintptr(bir)) &^ 0xf)
	if entries <= rpi5.RP1IntETH {
		return fmt.Errorf("rp1: MSI-X table has %d entries, need %d", entries, rpi5.RP1IntETH+1)
	}
	// De tabel ligt in de RP1-BAR; ons outbound-window legt PCIe 0.. op RP1Base.
	entry := uintptr(rpi5.RP1Base) + bar + off + msixEntryBytes*uintptr(rpi5.RP1IntETH)
	vec := rpi5.RP1IntETH
	addr := uint64(rpi5.MIPMSIAddr)
	dev.Write32(entry+0, uint32(addr))
	dev.Write32(entry+4, uint32(addr>>32))
	dev.Write32(entry+8, uint32(vec))
	dev.Write32(entry+12, 0) // vector control: niet gemaskeerd
	dev.MB()
	rc.CfgWrite32(1, 0, 0, cap, hdr&^msixFuncMask|msixEnable)
	fmt.Printf("irq: rp1 MSI-X cap %#x, %d entries, table BAR%d+%#x; entry %d → MIP vector %d\n", cap, entries, bir, off, vec, vec)

	// 2. De MIP open naar de host, de GIC-400 aan deze core, de lijn scherp
	//    (flank: de MIP levert een MSI als edge, bcm2712.dtsi).
	rpi5.MIPInit()
	ctrl := gicv2.New(uintptr(rpi5.GICDBase), uintptr(rpi5.GICCBase))
	fmt.Printf("irq: %s\n", ctrl.Describe())
	irq.Use(ctrl, idle.ServeIRQFlag) // de IRQ-deur: geen runtime-code in exception-context (cpu/idle)
	id := rpi5.MIPINTID(vec)
	ctrl.SetEdge(id)
	line := irq.Line{ID: id, Ack: nic.AckIRQ}
	if err := irq.Enable(line); err != nil {
		return err
	}

	// 3. De RP1: MSI aan in IACK-modus, en de GEM zelf open. De rearm van de
	//    pomp doet IER én de IACK (gem.Net.IRQAck).
	nic.IRQAck = func() { rpi5.RP1MSIXAck(rpi5.RP1IntETH) }
	rpi5.RP1MSIXEnable(rpi5.RP1IntETH)
	nic.EnableIRQ()
	nicLine = line
	fmt.Printf("irq: NIC on INTID %d (SPI %d) via the MIP, routed to this core — RX wakes on the interrupt\n", id, id-32)
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
