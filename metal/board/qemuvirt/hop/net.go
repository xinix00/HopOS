package hop

import (
	"github.com/usbarmory/tamago/dma"

	"hop-os/metal/abi/layout"
	"hop-os/metal/dev"
)

// QEMU virt plaatst 32 virtio-mmio-transports vanaf 0x0a000000 (stride 0x200,
// SPI-interrupt 16+n). Welk slot een -device krijgt is een QEMU-detail, dus
// we scannen op DeviceID.
const (
	virtioMMIOBase   = 0x0a000000
	virtioMMIOStride = 0x200
	virtioMMIOSlots  = 32

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
