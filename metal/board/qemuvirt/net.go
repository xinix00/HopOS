package qemuvirt

import (
	"fmt"

	"github.com/usbarmory/tamago/dma"

	"hop-os/metal/dev"
	"hop-os/metal/layout"
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
	dma.Init(layout.DMABase, layout.DMASize)
}

// ProbeVirtioNet zoekt het virtio-mmio-slot met de netwerkkaart en geeft de
// registerbasis + SPI-interruptnummer terug (0,0 = niet gevonden).
func ProbeVirtioNet() (base uint64, irq int) {
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

// DumpVirtioSlots print magic/version/device-id van de eerste mmio-slots
// (diagnose: welke transport gebruikt QEMU?).
func DumpVirtioSlots() {
	for i := 0; i < 4; i++ { // beperkt: hoge slots kunnen niet-backed zijn (abort)
		b := uintptr(virtioMMIOBase + i*virtioMMIOStride)
		fmt.Printf("mmio[%d] @%#x magic=%#x ver=%d id=%d\n",
			i, b, dev.Read32(b+regMagic), dev.Read32(b+regVersion), dev.Read32(b+regDeviceID))
	}
}
