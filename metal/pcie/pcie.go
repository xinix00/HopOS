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

// Config-space-registers (type 0/1 header).
const (
	cfgVendorID = 0x00
	cfgCommand  = 0x04
	cfgClass    = 0x08
	cfgHdrType  = 0x0c // dword: cacheline/latency/headertype[23:16]/BIST
	cfgBAR0     = 0x10
	cfgPriBus   = 0x18 // type-1: primary[7:0]/secondary[15:8]/subordinate[23:16]

	cmdMem    = 1 << 1
	cmdMaster = 1 << 2

	hdrTypeMask = 0x7f // headertype[6:0] (bit 7 = multifunction)
	hdrBridge   = 0x01 // type-1 header = PCI-PCI-bridge
)

// Device is één PCIe-functie. Bus 0, fn 0 blijft de default (Bus/Fn = 0), zodat
// de vlakke bus-0-Scan onveranderd werkt; de bus-walk (WalkBridges) vult Bus/Fn
// voor endpoints achter root-poorten op de O6N.
type Device struct {
	Bus      int
	Dev      int
	Fn       int
	VendorID uint16
	DeviceID uint16
	Class    uint32  // 24-bit klassecode (base<<16 | sub<<8 | progif)
	HdrType  uint8   // headertype[6:0]: 0 = endpoint, 1 = PCI-PCI-bridge
	ecam     uintptr // ECAM-basis van het board (gezet door Scan/ScanBus)
}

func (d *Device) String() string {
	return fmt.Sprintf("%02x:%02x.%x %04x:%04x class %06x", d.Bus, d.Dev, d.Fn, d.VendorID, d.DeviceID, d.Class)
}

// IsBridge meldt of dit een type-1 PCI-PCI-bridge is (root-poort/switch),
// waarachter nog een bus met endpoints hangt.
func (d *Device) IsBridge() bool { return d.HdrType == hdrBridge }

// cfg geeft het ECAM-adres van register off van deze functie. Standaard ECAM:
// base + (bus<<20)|(dev<<15)|(fn<<12) + off. Voor bus 0, fn 0 valt dit terug op
// base + dev<<15 + off — identiek aan de oorspronkelijke vlakke formule.
func (d *Device) cfg(off uintptr) uintptr {
	return d.ecam + uintptr(d.Bus)<<20 + uintptr(d.Dev)<<15 + uintptr(d.Fn)<<12 + off
}

// Scan enumereert bus 0 (fn 0 per device) in het ECAM-venster van het board.
// De ECAM-basis is board-specifiek (QEMU virt vs O6N/ACPI MCFG), dus komt via
// de PCIeWindow mee i.p.v. als package-constante. Dunne wrapper om ScanBus(0):
// het bestaande vlakke pad (QEMU-virt NVMe via metal/nvme) blijft ongewijzigd.
func Scan(win board.PCIeWindow) []*Device {
	return ScanBus(win, 0)
}

// ScanBus enumereert één bus (32 devices, fn 0) in het ECAM-venster. Additief
// naast Scan: dezelfde per-device-uitlezing, maar met een bus-parameter zodat de
// bus-walk buiten bus 0 kan kijken. Leest ook het headertype (type-1 = bridge).
func ScanBus(win board.PCIeWindow, bus int) []*Device {
	var found []*Device
	for devno := 0; devno < 32; devno++ {
		if d, _, ok := probeFn(win, bus, devno, 0); ok {
			found = append(found, d)
		}
	}
	return found
}

// probeFn leest één (bus,dev,fn) uit de config-space en vult een Device.
// Geeft (device, ruw-headertype-byte, aanwezig): het ruwe headertype draagt
// bit 7 (multifunctie), dat ScanConfigured nodig heeft en HdrType wegmaskt.
// Eén decode-plek voor beide scanners (vlak en hiërarchisch).
func probeFn(win board.PCIeWindow, bus, devno, fn int) (*Device, uint8, bool) {
	d := &Device{Bus: bus, Dev: devno, Fn: fn, ecam: win.ECAMBase}
	id := dev.Read32(d.cfg(cfgVendorID))
	if id == 0xffffffff || id&0xffff == 0 {
		return nil, 0, false
	}
	hdr := uint8(dev.Read32(d.cfg(cfgHdrType)) >> 16)
	d.VendorID = uint16(id)
	d.DeviceID = uint16(id >> 16)
	d.Class = dev.Read32(d.cfg(cfgClass)) >> 8
	d.HdrType = hdr & hdrTypeMask
	return d, hdr, true
}

// setBridgeBuses schrijft de primary/secondary/subordinate-busnummers in het
// type-1-header (offset 0x18); de secondary-latency-timer [31:24] blijft staan.
func (d *Device) setBridgeBuses(primary, secondary, subordinate int) {
	reg := d.cfg(cfgPriBus)
	v := dev.Read32(reg) & 0xff000000
	v |= uint32(primary&0xff) | uint32(secondary&0xff)<<8 | uint32(subordinate&0xff)<<16
	dev.Write32(reg, v)
	dev.MB()
}

// WalkBridges enumereert de hele PCIe-hiërarchie diepte-eerst vanaf bus 0 en
// programmeert op elke aangetroffen type-1-bridge de secondary/subordinate-
// busnummers (het klassieke enumeratie-recept: ken een bridge een secondary-bus
// toe, open subordinate wijd, daal af, knijp subordinate daarna dicht op het
// hoogst gebruikte busnummer). Geeft álle endpoints (type-0) over elke bus terug.
//
// Additief en NIET aangeroepen door bestaande code: de QEMU-virt/Pi-paden
// gebruiken de vlakke Scan. Bedoeld voor de O6N, waar endpoints (RTL8126, NVMe)
// achter root-poort-bridges hangen. STATUS: de busnummer-programmering is nog
// niet op silicium geverifieerd (QEMU-virt en de Pi's hebben geen bridge op hun
// pad) — additief en onbereikbaar vanuit het huidige pad, dus zonder regressie,
// maar het bridge-programmeer-deel moet op de O6N nog worden bewezen.
func WalkBridges(win board.PCIeWindow) []*Device {
	var endpoints []*Device
	next := 1 // volgend vrij uit te delen busnummer
	var walk func(bus int)
	walk = func(bus int) {
		for _, d := range ScanBus(win, bus) {
			if !d.IsBridge() {
				endpoints = append(endpoints, d)
				continue
			}
			sec := next
			next++
			d.setBridgeBuses(bus, sec, 0xff) // subordinate wijd tijdens de afdaling
			walk(sec)
			d.setBridgeBuses(bus, sec, next-1) // dichtknijpen op het echt gebruikte bereik
		}
	}
	walk(0)
	return endpoints
}

// ScanConfigured enumereert een hiërarchie die de firmware al geconfigureerd
// heeft (UEFI/ACPI-platforms zoals de Ampere Altra): puur read-only — de
// secondary-busnummers komen uit de bridges zelf, in plaats van dat wij ze
// uitdelen (dat doet WalkBridges, voor kale fabrics zonder firmware). Meldt
// óók bridges (root-poorten) en álle functies van multifunctie-devices — een
// dual-port-NIC is twee functies, en juist bij discovery wil je beide zien.
func ScanConfigured(win board.PCIeWindow, startBus int) []*Device {
	var out []*Device
	seen := map[int]bool{} // lussen breken op malafide/dubbele bridge-config
	var walk func(bus int)
	walk = func(bus int) {
		if bus < 0 || bus > 0xff || seen[bus] {
			return
		}
		seen[bus] = true
		for devno := 0; devno < 32; devno++ {
			for fn := 0; fn < 8; fn++ {
				d, hdr, ok := probeFn(win, bus, devno, fn)
				if !ok {
					if fn == 0 {
						break // geen device: functies >0 bestaan dan ook niet
					}
					continue // gat in een multifunctie-reeks is legaal
				}
				out = append(out, d)
				if d.IsBridge() {
					walk(int(dev.Read32(d.cfg(cfgPriBus)) >> 8 & 0xff))
				}
				if fn == 0 && hdr&0x80 == 0 {
					break // bit 7 = multifunctie; niet gezet → alleen fn 0
				}
			}
		}
	}
	walk(startBus)
	return out
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

// BAR leest het adres van memory-BAR idx zoals de firmware het toewees —
// read-only, voor al geconfigureerde platforms (UEFI/ACPI: de Altra); kale
// fabrics wijzen zelf toe met SetBAR64. 64-bit BARs (type-bits 0b10) lezen
// de hoge helft uit BAR idx+1.
func (d *Device) BAR(idx int) uint64 {
	lo := dev.Read32(d.cfg(cfgBAR0 + uintptr(idx)*4))
	addr := uint64(lo) &^ 0xf
	if lo>>1&0b11 == 0b10 {
		addr |= uint64(dev.Read32(d.cfg(cfgBAR0+uintptr(idx+1)*4))) << 32
	}
	return addr
}

// Enable zet memory-decode en bus-mastering (DMA) aan.
func (d *Device) Enable() {
	cmd := dev.Read32(d.cfg(cfgCommand))
	dev.Write32(d.cfg(cfgCommand), cmd|cmdMem|cmdMaster)
	dev.MB()
}
