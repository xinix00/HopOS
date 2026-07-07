// Package pcie doet PCIe-configruimte via ECAM en een minimale enumeratie
// van bus 0 — het fase-3-voorwerk dat rechtstreeks naar de Pi 5 (NVMe-HAT)
// en de O6N (RTL8126, NVMe) overdraagt. Omdat wij zonder firmware-hulp booten
// wijst niemand BAR's toe: dat doet HOP zelf, uit het MMIO-venster van het
// board. Op de O6N komt de ECAM-basis straks uit de ACPI MCFG-tabel; hier is
// hij een boardconstante (QEMU virt met highmem-ecam=off).
package pcie

import (
	"fmt"

	"hop-os/metal/board"
	"hop-os/metal/dev"
)

// Config-space-registers (type 0 header).
const (
	cfgVendorID = 0x00
	cfgCommand  = 0x04
	cfgClass    = 0x08
	cfgBAR0     = 0x10

	cmdMem    = 1 << 1
	cmdMaster = 1 << 2
)

// Device is één functie op bus 0 (fn 0; multi-function hebben we niet nodig).
type Device struct {
	Dev      int
	VendorID uint16
	DeviceID uint16
	Class    uint32  // 24-bit klassecode (base<<16 | sub<<8 | progif)
	ecam     uintptr // ECAM-basis van het board (gezet door Scan)
}

func (d *Device) String() string {
	return fmt.Sprintf("00:%02x.0 %04x:%04x klasse %06x", d.Dev, d.VendorID, d.DeviceID, d.Class)
}

// cfg geeft het ECAM-adres van register off van functie (bus 0, d.Dev, fn 0).
func (d *Device) cfg(off uintptr) uintptr {
	return d.ecam + uintptr(d.Dev)<<15 + off
}

// Scan enumereert bus 0 (fn 0 per device) in het ECAM-venster van het board.
// De ECAM-basis is board-specifiek (QEMU virt vs O6N/ACPI MCFG), dus komt via
// de PCIeWindow mee i.p.v. als package-constante.
func Scan(win board.PCIeWindow) []*Device {
	var found []*Device
	for devno := 0; devno < 32; devno++ {
		d := &Device{Dev: devno, ecam: win.ECAMBase}
		id := dev.Read32(d.cfg(cfgVendorID))
		if id == 0xffffffff || id&0xffff == 0 {
			continue
		}
		d.VendorID = uint16(id)
		d.DeviceID = uint16(id >> 16)
		d.Class = dev.Read32(d.cfg(cfgClass)) >> 8
		found = append(found, d)
	}
	return found
}

// SetBAR64 programmeert een 64-bit memory-BAR (idx = BAR-nummer) op addr en
// geeft de door het device gerapporteerde grootte terug.
func (d *Device) SetBAR64(idx int, addr uint64) uint64 {
	lo := d.cfg(cfgBAR0 + uintptr(idx)*4)
	hi := lo + 4

	dev.Write32(lo, 0xffffffff)
	dev.Write32(hi, 0xffffffff)
	szLo := dev.Read32(lo)
	szHi := dev.Read32(hi)
	size := ^(uint64(szHi)<<32 | uint64(szLo&^0xf)) + 1

	dev.Write32(lo, uint32(addr)|uint32(szLo&0xf))
	dev.Write32(hi, uint32(addr>>32))
	return size
}

// Enable zet memory-decode en bus-mastering (DMA) aan.
func (d *Device) Enable() {
	cmd := dev.Read32(d.cfg(cfgCommand))
	dev.Write32(d.cfg(cfgCommand), cmd|cmdMem|cmdMaster)
	dev.MB()
}
