package hop

import (
	"fmt"
	"time"

	"github.com/usbarmory/tamago/arm64"
	"github.com/usbarmory/tamago/dma"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/gicv3"
)

// QEMU virt plaatst 32 virtio-mmio-transports vanaf 0x0a000000 (stride 0x200,
// SPI-interrupt 16+n). Welk slot een -device krijgt is een QEMU-detail, dus
// we scannen op DeviceID.
//
// De GICv3 van virt (hw/arm/virt.c memmap): distributor op 0x08000000, de
// redistributor-frames vanaf 0x080a0000, 128KB per core — HOP is core 0.
const (
	virtioMMIOBase   = 0x0a000000
	virtioMMIOStride = 0x200
	virtioMMIOSlots  = 32

	gicdBase = 0x08000000
	gicrBase = 0x080a0000
	firstSPI = 32 // INTID van SPI 0

	regMagic    = 0x000 // "virt" = 0x74726976
	regVersion  = 0x004 // 2 = modern
	regDeviceID = 0x008 // 1 = netwerkkaart

	virtioMagic = 0x74726976
	deviceNet   = 1
)

func init() {
	// Globale DMA-regio voor virtio-ringen en -buffers: gereserveerd stuk
	// bovenin de HOP-partitie, buiten de RAM-declaratie (→ niet gecached).
	// In de hop-helft, niet de basis: alleen de HOP-kern draait drivers — een
	// app-image heeft geen DMA-allocator nodig (en de regio ligt buiten zijn
	// kooi). LET OP: alleen de net-subregio (NetDMASize), NIET de volle
	// DMASize — de NVMe-driver krijgt zijn eigen subregio (NVMeDMABase/
	// NVMeDMASize) expliciet via nvme.Probe. Claimde de globale
	// tamago-allocator de volle 16MB, dan kon dma.Alloc geheugen uit de
	// NVMe-subregio uitdelen → botsing met de NVMe-DMA-buffers.
	dma.Init(layout.DMABase, layout.NetDMASize)
}

// probeVirtioNet zoekt het virtio-mmio-slot met de netwerkkaart en geeft de
// registerbasis + SPI-interruptnummer terug (0,0 = niet gevonden).
func probeVirtioNet() (base uint64, irq int) {
	for i := range virtioMMIOSlots {
		b := uintptr(virtioMMIOBase + i*virtioMMIOStride)
		if dev.Read32(b+regMagic) != virtioMagic {
			continue
		}
		if dev.Read32(b+regVersion) != 2 {
			continue // legacy transport: QEMU met force-legacy=false draaien
		}
		if dev.Read32(b+regDeviceID) == deviceNet {
			return uint64(b), 16 + i
		}
	}
	return 0, 0
}

// nicLine is de interruptlijn van de virtio-net, gezet door ProbeNIC zodra de
// GIC staat; ID 0 = geen (dan pollt hopnet).
var nicLine irq.Line

// wireNICIRQ zet de GIC op, registreert de RX-lijn van de virtio-net en hangt
// tamago's servicer eraan (board.NICInterrupter → irq.Wait). Elke stap die
// faalt laat de node gewoon pollen: interrupts zijn een verbetering, geen
// voorwaarde.
func wireNICIRQ(spi int, ack func()) {
	if spi <= 0 {
		return
	}
	ctrl := gicv3.New(gicdBase, gicrBase)
	irq.Use(ctrl, arm64.ServiceInterrupts)
	l := irq.Line{ID: firstSPI + spi, Ack: ack}
	if err := irq.Enable(l); err != nil {
		fmt.Printf("net: virtio-net IRQ not enabled (%v) — RX stays polled\n", err)
		return
	}
	nicLine = l
	fmt.Printf("net: virtio-net RX on interrupt (SPI %d, INTID %d, GICv3 @ %#x)\n", spi, l.ID, gicdBase)
}

// WaitNIC (board.NICInterrupter): wacht op de RX-interrupt van de NIC, of max.
func (machine) WaitNIC(max time.Duration) bool {
	if nicLine.ID == 0 {
		time.Sleep(max)
		return false
	}
	return irq.Wait(nicLine, max)
}
