//go:build tamago && arm64

// Package aic is de Apple Interrupt Controller (AIC2/AIC3: M2 en later, hier
// de t8132 van de Mac mini M4) achter het contract van cpu/irq. Registers en
// indeling zoals m1n1 (src/aic.c) ze uit de ADT afleidt: één config-woord per
// IRQ vanaf extint-baseaddress, daarna SW_SET, SW_CLR, MASK_SET, MASK_CLR en
// HW_STATE, elk max_irq/32 woorden; de event-lees (ack) op aic-iack-offset.
//
// Twee eigenschappen die het verschil met een GIC maken:
//   - de AIC maskeert een hardware-IRQ zelf zodra hij uit het event-register
//     gelezen is; Complete zet het masker weer open (MASK_CLR);
//   - het doel is een 4-bit "target" in het config-woord van de IRQ, en de
//     firmware laat dat op 0 (niemand) — het board kiest hem (SetTarget) na
//     een meting met een software-IRQ (SoftRaise), niet uit een aanname.
package aic

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Props: de offsets die de ADT-node /arm-io/aic meegeeft (m1n1 leest dezelfde).
type Props struct {
	Cap0, MaxNumIRQ, ExtIntBase, Iack, GlbCfg uintptr
}

type Ctrl struct {
	base                                         uintptr
	cfg, swSet, swClr, maskSet, maskClr, hwState uintptr
	event                                        uintptr
	nrIRQ, maxIRQ                                int
	target                                       uint32
	version                                      uint32
}

// New leest de maten uit het silicium en legt de registerblokken vast; het
// schrijft niets.
func New(base uintptr, p Props) (*Ctrl, error) {
	if base == 0 || p.ExtIntBase == 0 || p.Iack == 0 {
		return nil, fmt.Errorf("aic: incomplete description (base %#x extint %#x iack %#x)", base, p.ExtIntBase, p.Iack)
	}
	c := &Ctrl{base: base, event: base + p.Iack, version: dev.Read32(base)}
	cap0 := dev.Read32(base + p.Cap0)
	maxn := dev.Read32(base + p.MaxNumIRQ)
	c.nrIRQ, c.maxIRQ = int(cap0&0xffff), int(maxn&0xffff)
	if c.maxIRQ == 0 || c.maxIRQ&31 != 0 || c.nrIRQ > c.maxIRQ {
		return nil, fmt.Errorf("aic: implausible sizes (cap0 %#x maxnumirq %#x)", cap0, maxn)
	}
	words := uintptr(c.maxIRQ) >> 5
	c.cfg = base + p.ExtIntBase
	c.swSet = c.cfg + 4*uintptr(c.maxIRQ)
	c.swClr = c.swSet + 4*words
	c.maskSet = c.swClr + 4*words
	c.maskClr = c.maskSet + 4*words
	c.hwState = c.maskClr + 4*words
	c.glbOn(p.GlbCfg)
	return c, nil
}

// glbOn zet de controller aan (glbcfg bit 0, zoals Linux' AIC2_CONFIG_ENABLE);
// iBoot laat hem uit — zonder dit komt er nooit een IRQ bij een core.
func (c *Ctrl) glbOn(off uintptr) {
	if off == 0 {
		return
	}
	dev.Write32(c.base+off, dev.Read32(c.base+off)|1)
	dev.MB()
}

func (c *Ctrl) Describe() string {
	return fmt.Sprintf("AIC v%#x @ %#x: %d of %d irq(s), event %#x, cfg %#x, masks %#x/%#x", c.version, c.base, c.nrIRQ, c.maxIRQ, c.event, c.cfg, c.maskSet, c.maskClr)
}

// NrIRQ: het aantal hardware-IRQ's dat dit silicium meldt.
func (c *Ctrl) NrIRQ() int { return c.nrIRQ }

// SetTarget kiest het 4-bit doel dat Enable in het config-woord schrijft.
func (c *Ctrl) SetTarget(t uint32) { c.target = t & 0xf }

func (c *Ctrl) word(id int) (uintptr, uint32) { return uintptr(id>>5) * 4, 1 << (uint(id) & 31) }

// Enable: doel in het config-woord en het masker open.
func (c *Ctrl) Enable(l irq.Line) error {
	if l.ID < 0 || l.ID >= c.nrIRQ {
		return fmt.Errorf("aic: irq %d outside 0..%d", l.ID, c.nrIRQ-1)
	}
	dev.Write32(c.cfg+4*uintptr(l.ID), dev.Read32(c.cfg+4*uintptr(l.ID))&^0xf|c.target)
	w, b := c.word(l.ID)
	dev.Write32(c.maskClr+w, b)
	dev.MB()
	return nil
}

func (c *Ctrl) Disable(l irq.Line) {
	if l.ID < 0 || l.ID >= c.nrIRQ {
		return
	}
	w, b := c.word(l.ID)
	dev.Write32(c.maskSet+w, b)
	dev.MB()
}

// Claim leest het event-register: die<<24 | type<<16 | nummer; type 1 is een
// hardware-IRQ, 0 is "niets". De lees is tegelijk de ack, en de AIC heeft de
// IRQ dan al gemaskeerd — Complete maakt hem weer scherp.
func (c *Ctrl) Claim() (irq.Line, bool) {
	ev := dev.Read32(c.event)
	if ev>>16&0xff != 1 {
		return irq.Line{}, false
	}
	return irq.Line{ID: int(ev>>24&0xff)*c.maxIRQ + int(ev&0xffff)}, true
}

func (c *Ctrl) Complete(l irq.Line) {
	w, b := c.word(l.ID)
	dev.Write32(c.maskClr+w, b)
	dev.MB()
}

// SoftRaise/SoftClear: een IRQ vanuit software laten vuren (SW_SET/SW_CLR) —
// de meetlat waarmee het board het doel vindt zonder een device nodig te
// hebben.
func (c *Ctrl) SoftRaise(id int) { w, b := c.word(id); dev.Write32(c.swSet+w, b); dev.MB() }
func (c *Ctrl) SoftClear(id int) { w, b := c.word(id); dev.Write32(c.swClr+w, b); dev.MB() }

// Pending: staat de hardware-IRQ (HW_STATE) op dit moment aan?
func (c *Ctrl) Pending(id int) bool {
	w, b := c.word(id)
	return dev.Read32(c.hwState+w)&b != 0
}

// Cfg geeft het config-woord van een IRQ (diagnose).
func (c *Ctrl) Cfg(id int) uint32 { return dev.Read32(c.cfg + 4*uintptr(id)) }
