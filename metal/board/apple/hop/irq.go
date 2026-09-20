//go:build tamago && arm64

package hop

// irq.go — de NIC-interrupt op de Mac mini M4: de tg3 stuurt een MSI naar de
// doorbell van zijn rootpoort, de poort maakt daar AIC-IRQ (msi-vector-offset +
// vectorbasis van de poort + vector) van, en de AIC levert hem aan HOP's core.
// Alles hier is meting vóór aanname: het doel-veld van de AIC wordt gevonden
// met een software-IRQ, en het nummer dat de MSI werkelijk oplevert met een
// scan over HW_STATE — pas dan wordt de lijn scherp gezet. Elke stap die
// faalt laat de node pollen, met de reden op de console.

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board/apple"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/aic"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/tg3"
)

// DefaultNICIRQ: "auto" = bedraden (sinds 19-09 de default: INTx via de
// rootpoort, gemeten gelijk aan of beter dan pollen — rtt p50 50-66 µs tegen
// 48-63, upload 43-47 tegen 37-38 MB/s), "0" = pollen. hopos.nicirq= in de
// config wint; bouwtijd via -X …/board/apple/hop.DefaultNICIRQ=0.
var DefaultNICIRQ = "auto"

const (
	portMSIcfg      = 0x124  // PORT_MSICFG: bit 0 aan (m1n1 schrijft 0x100 bij init)
	portMSIMapT8122 = 0x3000 // MSI-kaart t8122/t8132: 16 ingangen, bit 31 = aan
)

var (
	nicLine irq.Line
	nicDev  *tg3.Net
)

func wireNICIRQ(nic *tg3.Net) error {
	t, ok := apple.ADT()
	if !ok {
		return fmt.Errorf("no ADT")
	}
	node, ok := t.Path("/arm-io/aic")
	if !ok {
		return fmt.Errorf("no /arm-io/aic")
	}
	ctrl, err := aic.New(uintptr(apple.ADTReg("/arm-io/aic", 0)), aic.Props{
		Cap0: uintptr(t.U32(node, "cap0-offset", 4)), MaxNumIRQ: uintptr(t.U32(node, "maxnumirq-offset", 0xc)),
		ExtIntBase: uintptr(t.U32(node, "extint-baseaddress", 0)), Iack: uintptr(t.U64(node, "aic-iack-offset", 0)),
		GlbCfg: uintptr(t.U32(node, "aicglbcfg-offset", 0x14))})
	if err != nil {
		return err
	}
	fmt.Printf("irq: %s\n", ctrl.Describe())
	apcie, _ := t.Path("/arm-io/apcie")
	bridge, ok := t.Path(fmt.Sprintf("/arm-io/apcie/pci-bridge%d", apple.EthPortDev))
	if !ok {
		return fmt.Errorf("no pci-bridge%d in the ADT", apple.EthPortDev)
	}
	doorbell := t.U64(apcie, "msi-address", 0)
	vbase := t.U32(bridge, "msi-vector-base", 0)
	nvec := t.U32(bridge, "#msi-vectors", 32)
	first := int(t.U32(apcie, "msi-vector-offset", 0)) + int(vbase)
	pb := apple.PortBase(apple.EthPortDev)
	if doorbell == 0 || pb == 0 || first == 0 {
		return fmt.Errorf("msi facts incomplete (doorbell %#x port base %#x first irq %d)", doorbell, pb, first)
	}
	irq.Use(ctrl, idle.ServeIRQ)

	// 1. Het doel: welk 4-bit target brengt een IRQ bij DEZE core? Software-IRQ
	// op de eerste MSI-lijn, per kandidaat, en kijken of Fired oploopt.
	line := irq.Line{ID: first, Ack: func() { ctrl.SoftClear(first) }}
	target := -1
	for tgt := 0; tgt < 16 && target < 0; tgt++ {
		ctrl.SetTarget(uint32(tgt))
		if err := irq.Enable(line); err != nil {
			return err
		}
		before := irq.Fired()
		ctrl.SoftRaise(first)
		for end := time.Now().Add(3 * time.Millisecond); time.Now().Before(end) && irq.Fired() == before; {
			time.Sleep(100 * time.Microsecond)
		}
		ctrl.SoftClear(first)
		ctrl.Disable(line)
		if irq.Fired() != before {
			target = tgt
		}
	}
	if target < 0 {
		return fmt.Errorf("no AIC target delivers a software irq %d to mpidr %#x", first, dev.MPIDR()&0xffffff)
	}
	fmt.Printf("irq: AIC target %d reaches this core (mpidr %#x), msi irqs %d..%d\n", target, dev.MPIDR()&0xffffff, first, first+int(nvec)-1)
	ctrl.SetTarget(uint32(target))

	// 2. INTx: de tg3 trekt INTA# (level) zodra zijn status-blok verandert en
	// de interrupt-mailbox open staat; de rootpoort latcht dat in INTSTAT
	// (+0x100) en trekt zijn eigen AIC-lijn — het nummer staat in de ADT bij de
	// brug ("interrupts"). INTMSK (+0x104, m1n1: 0xfffffff0) laat INTA..D door.
	// Geen MSI, geen doorbell, geen DART: dezelfde weg als de Realtek op de O6N.
	pirq := -1
	if a, n, ok := t.Prop(bridge, "interrupts"); ok && n >= 4 {
		pirq = int(dev.Read32(a))
		fmt.Printf("irq: bridge interrupts prop: %d word(s), first %d", n/4, pirq)
		for k := uintptr(1); k < uintptr(n/4) && k < 4; k++ {
			fmt.Printf(" %d", dev.Read32(a+4*k))
		}
		fmt.Println()
	}
	if a, n, ok := t.Prop(apcie, "interrupts"); ok && int(n/4) > apple.EthPortDev {
		// Eén lijn per rootpoort, in poortvolgorde (t8103: 695/696/697).
		pirq = int(dev.Read32(a + 4*uintptr(apple.EthPortDev)))
		fmt.Printf("irq: apcie interrupts prop: %d word(s):", n/4)
		for k := uintptr(0); k < uintptr(n/4) && k < 8; k++ {
			fmt.Printf(" %d", dev.Read32(a+4*k))
		}
		fmt.Printf(" — port %d takes %d\n", apple.EthPortDev, pirq)
	}
	if pirq <= 0 || pirq >= ctrl.NrIRQ() {
		return fmt.Errorf("no usable port interrupt number in the ADT (%d)", pirq)
	}
	// De lijn van INTA is niet het eerste woord van de poort maar een van de
	// negen erachter (gemeten 19-09: 1249 + 4 = 1253 voor poort 2); dus alle
	// negen kort scherp met een ack die alleen telt, en de treffer wordt de
	// lijn. INTSTAT (+0x100) is alleen leesbaar — een W1C-schrijf geeft een
	// synchrone externe abort (ESR 0x96000410, bundel 24). Hoeft ook niet: de
	// lijn is level en zakt zodra de tg3 zijn mailbox dicht heeft.
	fmt.Printf("irq: port intstat %#x intmsk %#x; scanning AIC irqs %d..%d for INTA\n", dev.Read32(pb+0x100), dev.Read32(pb+0x104), pirq, pirq+8)
	var hitLine, hitCount atomic.Int32
	hitLine.Store(-1)
	for id := pirq; id <= pirq+8 && id < ctrl.NrIRQ(); id++ {
		id := id
		irq.Enable(irq.Line{ID: id, Ack: func() { hitLine.CompareAndSwap(-1, int32(id)); hitCount.Add(1); nic.AckIRQ() }})
	}
	nic.IRQUnmask()
	found := -1
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); {
		if h := hitLine.Load(); h >= 0 {
			found = int(h)
			break
		}
		time.Sleep(time.Millisecond)
	}
	st, hc, ps := nic.IRQDiag()
	fmt.Printf("irq: hit line %d (%d hits), port intstat %#x, tg3 status %#x hostcc %#x pci status %#x\n", hitLine.Load(), hitCount.Load(), dev.Read32(pb+0x100), st, hc, ps)
	for id := pirq; id <= pirq+8 && id < ctrl.NrIRQ(); id++ {
		if id != found {
			ctrl.Disable(irq.Line{ID: id})
		}
	}
	if found < 0 {
		nic.AckIRQ()
		return fmt.Errorf("no INTx reached the AIC on irqs %d..%d within 4 s", pirq, pirq+8)
	}
	ack := nic.AckIRQ // mailbox 1: INTA# valt, de poortlijn zakt mee
	line = irq.Line{ID: found, Ack: ack}
	nicLine, nicDev = line, nic
	fmt.Printf("irq: NIC INTx on AIC irq %d (root port %d) routed to target %d — RX wakes on the interrupt\n", found, apple.EthPortDev, target)
	return nil
}

// WaitNIC (board.NICInterrupter): wacht op de NIC-lijn, of val terug op de
// 300µs-poll zolang er geen bedrade lijn is. De rearm ná de pomp-ronde, zoals
// op de O6N.
func (machine) WaitNIC(max time.Duration) bool {
	if nicLine.ID == 0 {
		time.Sleep(300 * time.Microsecond)
		return false
	}
	// Geen rearm hier: dat doet de RX-pomp zelf op zijn slaapmoment
	// (hopnet.rxLoop, netdev.IRQRearmer), ná een lege ronde.
	return irq.Wait(nicLine, max)
}
