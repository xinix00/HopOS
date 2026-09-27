//go:build tamago && arm64

// Package gicv2 is de irq.Controller voor een GIC-400 (GICv2): distributor
// plus de memory-mapped CPU-interface van de aanroepende core. De Pi 4 en de
// Pi 5 (BCM2711/BCM2712) hebben deze GIC; de v3-boards (Radxa, O6N, Altra,
// QEMU) draaien driver/gicv3. Registers en volgorde naar Linux'
// drivers/irqchip/irq-gic.c en include/linux/irqchip/arm-gic.h.
//
// Dezelfde twee regels als de v3-adapter: alleen de lijnen aanraken die wij
// enablen (de firmware heeft er ook), en elke SPI expliciet naar déze core
// routeren (GICD_ITARGETSR) — een app-core is nooit een target. Non-secure
// Group 1: op de Pi's staat TF-A op EL3 en zijn de Group-0-registers niet van
// ons; TF-A laat alle SPI's als Group 1 achter (gicv2_distif_init) en heeft
// de CPU-interface van de boot-core al aangezet. Wat hier gebeurt is
// idempotent: PMR open, EnableGrp1 in GICC_CTLR en GICD_CTLR (NS-view bit 0).
//
// Geen SGI's hier: HOP wekt een app-core op een GIC-400 met SEV (board/raspi/
// hop), want GICD_ISENABLER0 is per core gebankt en alleen door die core zelf
// te schrijven.
package gicv2

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Distributor (GICD) en CPU-interface (GICC), arm-gic.h.
const (
	gicdCTLR      = 0x000
	gicdIIDR      = 0x008
	gicdISENABLER = 0x100
	gicdICENABLER = 0x180
	gicdPRI       = 0x400 // één byte per INTID
	gicdTARGET    = 0x800 // één byte per INTID: CPU-masker
	gicdICFGR     = 0xc00 // twee bits per INTID, bit 1 = edge

	giccCTRL   = 0x00
	giccPMR    = 0x04
	giccIAR    = 0x0c
	giccEOIR   = 0x10
	giccIIDR   = 0xfc
	enableGrp1 = 1 // NS-view van GICD_CTLR en GICC_CTLR

	firstSPI     = 32
	firstSpecial = 1020 // IAR: 1020..1023 = niets (1023 = spurious)
	defaultPri   = 0xa0 // GICD_INT_DEF_PRI: onder de firmware, boven de vloer
)

// Ctrl is de adapter van één core.
type Ctrl struct {
	gicd, gicc uintptr
	cpuMask    uint8 // dit CPU-interface in GICD_ITARGETSR-termen
}

// New koppelt distributor en CPU-interface. Het eigen CPU-masker staat in de
// (gebankte) target-bytes van SGI 0-3: elk byte is het masker van de lezer
// (gic_get_cpumask in irq-gic.c).
func New(gicd, gicc uintptr) *Ctrl {
	c := &Ctrl{gicd: gicd, gicc: gicc, cpuMask: uint8(dev.Read32(gicd + gicdTARGET))}
	dev.Write32(gicc+giccPMR, 0xff)
	dev.Write32(gicc+giccCTRL, dev.Read32(gicc+giccCTRL)|enableGrp1)
	dev.Write32(gicd+gicdCTLR, dev.Read32(gicd+gicdCTLR)|enableGrp1)
	dev.MB()
	return c
}

// Describe is één regel voor de bootlog.
func (c *Ctrl) Describe() string {
	return fmt.Sprintf("GICD %#x (IIDR %#x, CTLR %#x), GICC %#x (IIDR %#x, CTLR %#x), cpu mask %#x",
		c.gicd, dev.Read32(c.gicd+gicdIIDR), dev.Read32(c.gicd+gicdCTLR),
		c.gicc, dev.Read32(c.gicc+giccIIDR), dev.Read32(c.gicc+giccCTRL), c.cpuMask)
}

// SetEdge maakt SPI id flankgevoelig (ICFGR bit 1 van zijn paar) — vóór
// Enable, voor lijnen die een flank zijn (de MSI-vectoren van de MIP).
func (c *Ctrl) SetEdge(id int) {
	if id < firstSPI || id >= firstSpecial {
		return
	}
	reg := c.gicd + gicdICFGR + uintptr(id/16)*4
	dev.Write32(reg, dev.Read32(reg)|2<<uint(2*(id%16)))
	dev.MB()
}

// Enable: prioriteit, route naar deze core, scherp. Alleen SPI's: de
// gebankte SGI/PPI-registers zijn van de core zelf, niet van de bedrading.
func (c *Ctrl) Enable(l irq.Line) error {
	if l.ID < firstSPI || l.ID >= firstSpecial {
		return fmt.Errorf("gicv2: INTID %d is not an SPI", l.ID)
	}
	dev.Write8(c.gicd+gicdPRI+uintptr(l.ID), defaultPri)
	dev.Write8(c.gicd+gicdTARGET+uintptr(l.ID), c.cpuMask)
	dev.MB()
	dev.Write32(c.gicd+gicdISENABLER+uintptr(l.ID/32)*4, 1<<uint(l.ID%32))
	dev.MB()
	return nil
}

func (c *Ctrl) Disable(l irq.Line) {
	if l.ID >= firstSPI && l.ID < firstSpecial {
		dev.Write32(c.gicd+gicdICENABLER+uintptr(l.ID/32)*4, 1<<uint(l.ID%32))
		dev.MB()
	}
}

// Claim leest GICC_IAR en doet meteen de EOI met dezelfde waarde (EOImode 0:
// priority drop én deactivate); 1020+ is "niets".
func (c *Ctrl) Claim() (irq.Line, bool) {
	raw := dev.Read32(c.gicc + giccIAR)
	id := int(raw & 0x3ff)
	if id >= firstSpecial {
		return irq.Line{}, false
	}
	dev.Write32(c.gicc+giccEOIR, raw)
	return irq.Line{ID: id}, true
}

// Complete: de EOI zat al in Claim.
func (c *Ctrl) Complete(irq.Line) {}
