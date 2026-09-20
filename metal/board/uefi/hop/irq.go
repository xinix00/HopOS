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
// pollen; "auto" = bekend of anders ontdekken via de pending-bits; een getal =
// die INTID; "0" = pollen. hopos.nicirq= in de config wint. De standaard is
// "known" en niet "auto": op de Ampere (Altra) wordt de ontdekte lijn netjes
// afgeleverd (claims van INTID 156 binnen een seconde) en seconden later hangt
// HOP's core en reset de watchdog de node — zes flips op rij op 19-09, één
// uitzondering (bundel 49). Tot dat begrepen is pollt een onbekend UEFI-board
// en is hopos.nicirq=auto de opt-in voor de meting.
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
	ctrl, err := controller()
	if err != nil {
		fmt.Printf("irq: NIC interrupt not wired (%v) — RX stays polled\n", err)
		return
	}
	if line == 0 && sel != "auto" {
		fmt.Println("irq: no known NIC line for this platform — RX stays polled (hopos.nicirq=auto discovers one; see L83 before doing that on an Altra)")
		return
	}
	if line == 0 {
		line, err = discoverNICLine(ctrl, dev)
		if err != nil {
			fmt.Printf("irq: NIC interrupt not wired (%v) — RX stays polled\n", err)
			return
		}
	}
	if err := WireNICIRQ(line, dev.AckIRQ); err != nil {
		fmt.Printf("irq: NIC interrupt not wired (%v) — RX stays polled\n", err)
		return
	}
	nicIRQ = dev
	dev.EnableIRQ()
}

// discoverNICLine vindt de SPI van de NIC zonder iets scherp te zetten: de
// pending-bits van de distributor (GICD_ISPENDR) laten een level-lijn zien,
// ook als hij uit staat. Kandidaat = een SPI die opkomt nadat de chip zijn
// masker opent; bewijs = hij volgt dat masker (dicht → hij zakt, open → hij
// komt terug). Zonder dat bewijs is "de eerste nieuwe pending-lijn" een gok:
// op de Ampere kwam vanaf koud eerst SPI 52 op (niet de NIC) en pas daarna
// SPI 124 (wél), en met de verkeerde lijn loopt de pomp op de 10ms-vangrail
// (bundels 47/50, 19-09). Daarom een heel venster verzamelen, elke kandidaat
// toetsen, en de afgewezen onthouden.
func discoverNICLine(ctrl *gicv3.Ctrl, dev NICInterrupt) (int, error) {
	pending := func() map[int]bool {
		m := map[int]bool{}
		for _, id := range ctrl.PendingSPIs(32, 1019) {
			m[id] = true
		}
		return m
	}
	before := pending()
	dev.EnableIRQ()
	defer dev.AckIRQ() // masker weer dicht tot de lijn bedraad is
	tried := map[int]bool{}
	var rejected []int
	for end := time.Now().Add(12 * time.Second); time.Now().Before(end); {
		// Een venster lang verzamelen — de eerste die opkomt is niet per se de onze.
		cands := map[int]bool{}
		for w := time.Now().Add(2 * time.Second); time.Now().Before(w); {
			for id := range pending() {
				if !before[id] && !tried[id] {
					cands[id] = true
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		for id := range cands {
			tried[id] = true
			dev.AckIRQ()
			time.Sleep(time.Millisecond)
			if pending()[id] {
				fmt.Printf("irq: SPI %d (INTID %d) stays pending with the chip masked — not the NIC\n", id-32, id)
				rejected = append(rejected, id)
				dev.EnableIRQ()
				continue
			}
			dev.EnableIRQ()
			back := false
			for w := time.Now().Add(3 * time.Second); time.Now().Before(w) && !back; {
				back = pending()[id]
				time.Sleep(10 * time.Millisecond)
			}
			if !back {
				fmt.Printf("irq: SPI %d (INTID %d) dropped with the mask but did not return within 3 s — not the NIC\n", id-32, id)
				rejected = append(rejected, id)
				continue
			}
			fmt.Printf("irq: NIC line discovered: SPI %d (INTID %d) follows the chip's mask (rejected %v, %d SPIs already pending)\n", id-32, id, rejected, len(before))
			return id, nil
		}
	}
	return 0, fmt.Errorf("NIC line discovery: no SPI followed the chip's mask within 12 s (rejected %v, %d already pending)", rejected, len(before))
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
