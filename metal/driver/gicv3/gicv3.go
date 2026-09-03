//go:build tamago && arm64

// Package gicv3 is de irq.Controller over tamago's GICv3-driver
// (arm64/gic): distributor + redistributor van de aanroepende core, Group 0,
// system-register-interface. Eén adapter voor elk ARM-board met een GICv3 —
// QEMU virt, de Radxa (GIC-600), de Altra. De Pi's hebben een GIC-400 (v2)
// en krijgen hun eigen driver.
//
// Eén ding doet dit pakket bóvenop tamago: de route van een SPI expliciet
// naar déze core zetten, met IRM=0. tamago schrijft het rauwe MPIDR in
// GICD_IROUTER, en daar staat bit 31 RES1 — dat is precies IROUTER's
// "any participating PE"-bit. Met alleen HOP's redistributor wakker komt
// het toch bij HOP terecht, maar de isolatieregel is niet "meestal": een
// app-core mag nooit een target zijn, dus de route is hard.
package gicv3

import (
	"github.com/usbarmory/tamago/arm64/gic"

	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	gicdIROUTER  = 0x6100
	firstSPI     = 32
	firstSpecial = 1020                        // 1020..1023: geen echte interrupt
	affMask      = uint64(0xFF)<<32 | 0xFFFFFF // Aff3:Aff2:Aff1:Aff0, IRM (bit 31) = 0
)

// Ctrl is één GICv3, geïnitialiseerd voor de aanroepende core.
type Ctrl struct {
	hw    gic.GIC
	mpidr uint64
}

// New initialiseert de GIC: gicd is de distributor, gicr het redistributor-
// frame van DEZE core (per core 128KB verder). Aanroepen op HOP's core —
// de core die de interrupts gaat nemen.
func New(gicd, gicr uintptr) *Ctrl {
	c := &Ctrl{hw: gic.GIC{GICD: uint32(gicd), GICR: uint32(gicr)}}
	c.hw.Init()
	c.mpidr = dev.MPIDR()
	return c
}

func (c *Ctrl) Enable(l irq.Line) error {
	c.hw.EnableInterrupt(l.ID)
	if l.ID >= firstSPI {
		dev.Write64(uintptr(c.hw.GICD)+gicdIROUTER+8*uintptr(l.ID), c.mpidr&affMask)
	}
	return nil
}

func (c *Ctrl) Disable(l irq.Line) { c.hw.DisableInterrupt(l.ID) }

// Claim leest ICC_IAR0 en doet meteen de EOI (tamago's GetInterrupt doet
// beide); een speciaal nummer (1020+) is "niets".
func (c *Ctrl) Claim() (irq.Line, bool) {
	id := c.hw.GetInterrupt()
	if id >= firstSpecial {
		return irq.Line{}, false
	}
	return irq.Line{ID: id}, true
}

// Complete: de EOI zat al in Claim.
func (c *Ctrl) Complete(irq.Line) {}
