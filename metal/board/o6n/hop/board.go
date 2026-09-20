// Package hop is het board van de Radxa Orion O6N (Cix P1 / CD8180: 12
// Armv9-cores in drie clusters, 2× RTL8126/8125, M.2 NVMe, HDMI/DP) — het
// primaire productiedoel van HopOS. Alleen een HOP-helft: de basis-helft ís
// board/uefi, want een app-image heeft geen O6N-kennis nodig.
//
// De O6N boot via zijn eigen EDK2-firmware met ACPI, dus dit board is het
// UEFI-board (board/uefi + board/uefi/hop) plús wat de O6N eigen heeft. Alles
// wat de firmware vertelt komt langs de universele weg: cores en clusterklassen
// uit de MADT, RAM uit de memory-map, PCIe (NIC + NVMe) uit de MCFG, beeld uit
// GOP, console uit de SPCR (DesignWare 8250), watchdog uit de GTDT, CPU_ON
// via PSCI. Wat dit pakket toevoegt is de board-KENNIS die geen ACPI-tabel
// draagt: de klasse-terugval als de firmware zwijgt, straks de thermometer,
// de klok en de NIC-interruptlijn — en die kennis staat hier, niet in het
// universele pad.
//
// Registratie: dit pakket importeert board/uefi/hop (die registreert zich in
// zijn init) en registreert daarná zichzelf; Go initialiseert een dependency
// vóór zijn importeur, dus board.Current() is dít board. Bouwen met
// -tags "o6n linkcpuinit" (cmd/hopos/board_o6n.go; image/uefi-run.sh BOARD=o6n).
// Bewust board/o6n/hop en geen board/o6n: de importregels (tools/importcheck)
// laten drivers en het contract alleen in een hop-helft toe.
package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	uefihop "github.com/xinix00/HopOS/metal/v2/board/uefi/hop"
)

// machine is het O6N-board: het UEFI-board met de O6N-kennis eroverheen.
type machine struct{ uefihop.Machine }

// HeaderUART is UART2, de 3-pins debug-header van de Orion O6/O6N (PL011,
// 115200 8n1). De SPCR van de Cix-firmware wijst naar UART0 of UART3
// (gemeten in dmesg: ttyAMA0 op 0x040b0000), dus zonder deze spiegel ziet
// de kabel niets.
const HeaderUART = 0x040d0000

// OEMID is de XSDT-OEM-ID van de Cix-firmware: de runtime-check dat dit
// werkelijk een Cix P1 is vóór er board-kennis (mailbox-adressen) in MMIO
// gaat. Een generieke UEFI-doos met dit image krijgt dan gewoon het
// UEFI-gedrag.
const OEMID = "CIXTEK"

// IsCix meldt of de firmware zich als Cix identificeert.
func IsCix() bool {
	t := uefi.Tables()
	return t != nil && t.OEMID == OEMID
}

func init() {
	board.Use(machine{})
	if IsCix() && uefi.MapHigh(HeaderUART, 0x1000) {
		uefi.MirrorConsole(HeaderUART)
	}
}

var _ board.Board = machine{}

// De NIC-interrupt: INTx van de root-port waar de NIC achter hangt, als
// GIC-INTID. Bron: de mainline-DT (sky1.dtsi interrupt-map, GIC_SPI n →
// INTID n+32) — x1_0 (bus 0x00, LAN 1) SPI 436, x1_1 (0x30, LAN 2) 445,
// x2 (0x60) 427, x4 (0x90) 417, x8 (0xc0) 407; alle INTA, level-high. De
// Cix-DSDT noemt dezelfde lijnen in haar _PRT. Onbewezen tot de probe hem
// ziet vuren; hopos.nicirq=N overschrijft (0 = pollen).
var intxByRootBus = map[int]int{0x00: 436 + 32, 0x30: 445 + 32, 0x60: 427 + 32, 0x90: 417 + 32, 0xc0: 407 + 32}

func init() {
	// De lijn per rootpoort uit de DT is een hint voor het generieke UEFI-pad
	// (uefi/hop/irq.go); zonder hint ontdekt dat pad de lijn zelf.
	uefihop.KnownNICLine = func(rootBus int) int {
		if !IsCix() { // deze image draait ook op andere UEFI-machines (Ampere, 19-09)
			return 0
		}
		return intxByRootBus[rootBus]
	}
}

var _ board.NICInterrupter = machine{}

// Name is de consolenaam van dit board.
const Name = "Radxa Orion O6N (Cix P1)"

// classNames: de klassen in oplopende efficiëntie-volgorde van de firmware
// (ACPI "Processor Power Efficiency Class": lager = zuiniger). Drie clusters
// → small/mid/big; twee → small/big; één → big.
var classNames = [][]string{
	{"big"},
	{"small", "big"},
	{"small", "mid", "big"},
}

// CoreClass geeft de clusterklasse van logische app-core i (1..N; de
// fysieke MADT-index komt via Cores().Phys — sinds de eigen core niet meer
// per se index 0 is, zijn logisch en fysiek verschoven). Twee bronnen, in
// deze volgorde:
//
//  1. de "Processor Power Efficiency Class" per GICC in de MADT — de
//     universele vorm van "welk cluster is dit". De Cix-firmware vult hem
//     niet (Madt.aslc: 0 voor elke core), dus dan:
//  2. de HighestPerformance uit de _CPC per core: het laagste maximum is
//     "small" (de A520's), het hoogste "big", alles ertussen "mid". Op de
//     firmware-snapshot: 2000 → small ×4, 2200/2600 → mid ×6, 2800 → big ×2.
//
// Zegt geen van beide iets, dan is dit board homogeen "big": een verzonnen
// indeling maakt jobs onplaatsbaar (de qemuvirt-les), een homogene niet.
func (m machine) CoreClass(i int) string {
	phys, ok := m.Cores().Phys(i)
	if !ok {
		return "big"
	}
	return physClass(phys)
}

// physClass is CoreClass op een MADT-index.
func physClass(phys int) string {
	cpus := uefi.MADTCPUs()
	if phys < 0 || phys >= len(cpus) {
		return "big"
	}
	if classes := effClasses(); len(classes) >= 2 && len(classes) <= 3 {
		names := classNames[len(classes)-1]
		for k, c := range classes {
			if cpus[phys].EffClass == c {
				return names[k]
			}
		}
		return "big"
	}
	return cpcClass(phys)
}

// cpcClass rangschikt op HighestPerformance uit de _CPC (zie CoreClass).
// cpcClass leidt de klasse af uit de _CPC-"highest performance" van de core,
// geclusterd op afstand: waarden die binnen 25% van elkaar liggen zijn één
// klasse. Op de O6N (17-09) verschillen de vier A720-paren onderling in
// maximum (8192/7876/7246/6931: binning per DVFS-domein) en de A520's staan
// op 2232 — "distinct = klasse" gaf toen 1 big, 6 mid, 4 small, en de
// scheduler zag zeven cores van hetzelfde type als twee soorten. Gebruikt
// alleen als de MADT geen efficiency-classes draagt.
func cpcClass(phys int) string {
	c, ok := cpcOf(phys)
	if !ok || c.Highest == 0 {
		return "big"
	}
	classes := cpcClasses()
	if len(classes) < 2 {
		return "big"
	}
	names := classNames[len(classes)-1]
	for k, top := range classes {
		if c.Highest <= top {
			return names[k]
		}
	}
	return "big"
}

// cpcClasses: de bovengrens per klasse, oplopend (hoogstens drie; meer
// clusters worden op drie samengevoegd door de kleinste gaten te sluiten).
func cpcClasses() []uint32 {
	t := uefi.Tables()
	if t == nil {
		return nil
	}
	var vals []uint32
	for _, x := range t.CPCs() {
		if x.Highest != 0 {
			vals = append(vals, x.Highest)
		}
	}
	for i := 1; i < len(vals); i++ { // insertion sort: hooguit twaalf
		for j := i; j > 0 && vals[j-1] > vals[j]; j-- {
			vals[j-1], vals[j] = vals[j], vals[j-1]
		}
	}
	var tops []uint32 // per cluster de hoogste waarde
	for i, v := range vals {
		if i == 0 || v*100 > vals[i-1]*125 { // gat > 25%: nieuwe klasse
			tops = append(tops, v)
		} else {
			tops[len(tops)-1] = v
		}
	}
	for len(tops) > 3 { // te veel clusters: het kleinste relatieve gat sluiten
		k := 1
		for i := 2; i < len(tops); i++ {
			if tops[i]*tops[k-1] < tops[k]*tops[i-1] {
				k = i
			}
		}
		tops = append(tops[:k], tops[k+1:]...)
	}
	return tops
}

func effClasses() []uint8 {
	cpus := uefi.MADTCPUs()
	var out []uint8
	for _, c := range cpus[min(1, len(cpus)):] {
		found := false
		for _, e := range out {
			if e == c.EffClass {
				found = true
				break
			}
		}
		if !found {
			out = append(out, c.EffClass)
		}
	}
	for i := 1; i < len(out); i++ { // insertion sort: 3 elementen
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Describe is één consoleregel over de klasse-indeling die dit board
// afleidde — de bootlog hoort te zeggen waar de placement op leunt.
func Describe() string {
	m := machine{}
	count := map[string]int{}
	app := m.Cores().App()
	for i := range app {
		count[m.CoreClass(i+1)]++
	}
	src := "MADT efficiency classes"
	if classes := effClasses(); len(classes) < 2 || len(classes) > 3 {
		src = "_CPC highest performance (MADT carries no efficiency classes)"
	}
	return fmt.Sprintf("%s: %d app cores, classes from %s - small %d, mid %d, big %d",
		Name, len(app), src, count["small"], count["mid"], count["big"])
}
