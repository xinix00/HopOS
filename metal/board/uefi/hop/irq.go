package hop

import (
	"fmt"
	"strconv"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/gicv3"
)

// irq.go — de NIC-interrupt op een UEFI/ACPI-platform: de GICv3 uit de MADT
// (distributor + de redistributor-range, waarin het frame van HOP's core op
// MPIDR gezocht wordt) en één SPI naar HOP's core. Wélke lijn de NIC heeft
// is bordkennis (de INTx-uitgang van zijn root-port staat in de DSDT-_PRT,
// die wij niet interpreteren): het concrete bord geeft hem (board/o6n/hop),
// eventueel via hopos.nicirq=. Zonder lijn blijft alles gepold.

// nicLine is de bedrade lijn (ID 0 = geen: pollen); nicIRQ het device erachter.
var (
	nicLine irq.Line
	nicIRQ  NICInterrupt
	nicCtrl *gicv3.Ctrl
)

// NICInterrupt: wat een NIC-driver moet kunnen om aan een lijn te hangen —
// dezelfde drie werkwoorden op de Realtek en de igb.
type NICInterrupt interface {
	EnableIRQ()
	AckIRQ()
	RearmIRQ()
}

// KnownNICLine: bordkennis (O6N: de DT-tabel per rootpoort), 0 = onbekend.
var KnownNICLine func(rootBus int) int

// DefaultNICIRQ: "known" = de lijn uit KnownNICLine (de O6N-tabel) of anders
// pollen; een getal = die INTID; "0" = pollen. hopos.nicirq= in de config
// wint. Een onbekend UEFI-board pollt: op de Altra is de INTx-lijn van de
// NIC fataal op SoC-niveau (L83, 19-09), en de discovery-code van die jacht
// is weg (20-09) — wie een lijn kent, geeft hem op.
var DefaultNICIRQ = "known"

// setupNICIRQ: de lijn kiezen, bedraden en de chip openzetten. Elke stap die
// faalt laat de node pollen, met de reden op de console.
func setupNICIRQ(dev NICInterrupt, rootBus int) {
	sel := DefaultNICIRQ
	if v := uefi.BootConfigAll("hopos.nicirq"); len(v) > 0 {
		sel = v[0]
	}
	if sel == "0" {
		fmt.Println("irq: NIC interrupt off (hopos.nicirq=0) — RX stays polled")
		return
	}
	line := 0
	if n, err := strconv.Atoi(sel); err == nil {
		line = n
	} else if KnownNICLine != nil {
		line = KnownNICLine(rootBus)
	}
	if line == 0 {
		fmt.Println("irq: no known NIC line for this platform — RX stays polled (hopos.nicirq=<INTID> wires one)")
		return
	}
	if err := WireNICIRQ(line, dev.AckIRQ); err != nil {
		fmt.Printf("irq: NIC interrupt not wired (%v) — RX stays polled\n", err)
		return
	}
	nicIRQ = dev
	dev.EnableIRQ()
}

// WaitNIC (board.NICInterrupter): wacht op de NIC-lijn; zonder bedrade lijn
// de oude 300µs-poll, zodat een mislukte bedrading nooit trager is dan
// vroeger. De ack sloot het masker van de chip; hier, ná de pomp-ronde, gaat
// het weer open (zie de driver).
func (Machine) WaitNIC(max time.Duration) bool {
	if nicLine.ID == 0 {
		time.Sleep(300 * time.Microsecond)
		return false
	}
	// Geen rearm hier: dat doet de RX-pomp zelf op zijn slaapmoment
	// (hopnet.rxLoop, netdev.IRQRearmer), ná een lege ronde.
	return irq.Wait(nicLine, max)
}

// controller zet de GICv3 op (eenmalig) uit de MADT.
func controller() (*gicv3.Ctrl, error) {
	if nicCtrl != nil {
		return nicCtrl, nil
	}
	t := uefi.Tables()
	if t == nil {
		return nil, fmt.Errorf("no ACPI tables")
	}
	_, gicd, err := t.MADT()
	if err != nil || gicd == 0 {
		return nil, fmt.Errorf("no GICD in the MADT")
	}
	gicrBase, gicrLen, _ := t.GIC()
	if gicrBase == 0 {
		return nil, fmt.Errorf("no GICR range in the MADT")
	}
	if !uefi.MapHigh(gicd, 0x10000) || !uefi.MapHigh(gicrBase, gicrLen) {
		return nil, fmt.Errorf("GICD %#x / GICR %#x+%#x unreachable", gicd, gicrBase, gicrLen)
	}
	gicr, ok := gicv3.FindRedistributor(gicrBase, gicrLen, dev.MPIDR())
	if !ok {
		return nil, fmt.Errorf("no redistributor for MPIDR %#x in %#x+%#x", dev.MPIDR()&0xffffff, gicrBase, gicrLen)
	}
	nicCtrl = gicv3.New(uintptr(gicd), uintptr(gicr))
	fmt.Printf("irq: %s\n", nicCtrl.Describe())
	return nicCtrl, nil
}

// NICLine geeft de bedrade NIC-interruptlijn (ID 0 = geen).
func NICLine() irq.Line { return nicLine }

// WireNICIRQ zet de GIC op en maakt INTID intid scherp met ack als device-
// bevestiging. Elke stap die faalt laat de node gewoon pollen: interrupts
// zijn een verbetering, geen voorwaarde. Aanroepen op HOP's core.
func WireNICIRQ(intid int, ack func()) error {
	ctrl, err := controller()
	if err != nil {
		return err
	}
	irq.Use(ctrl, idle.ServeIRQFlag) // de IRQ-deur: geen runtime-code in exception-context (cpu/idle)
	l := irq.Line{ID: intid, Ack: ack}
	if err := irq.Enable(l); err != nil {
		return err
	}
	nicLine = l
	fmt.Printf("irq: NIC on INTID %d (SPI %d) routed to MPIDR %#x — RX wakes on the interrupt\n",
		intid, intid-32, dev.MPIDR()&0xffffff)
	return nil
}

// gicrRange geeft de GICR-range uit de MADT (voor een core zonder eigen
// GICC-redistributoradres).
func gicrRange() (ok bool, base, length uint64) {
	t := uefi.Tables()
	if t == nil {
		return false, 0, 0
	}
	b, l, _ := t.GIC()
	return b != 0, b, l
}
