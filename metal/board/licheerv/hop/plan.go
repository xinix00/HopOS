package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board/licheerv"
	"github.com/xinix00/HopOS/metal/driver/nic/dwmac"
)

// Het PA-plan van dit board: waar HOP's eigen structuren liggen en welk DRAM
// vrij is voor app-partities. De 256MB DDR3 van de SG2002 ligt op
// 0x80000000..0x90000000 en er blijft niets ongebruikt:
//
//	0x80000000  64MB  pool B — hier decomprimeert de FSBL U-Boot naar
//	                  0x80200020 (~600KB) vóórdat hij ons aanspringt. Daarna
//	                  raakt niemand het meer aan, dus het is gewoon vrij DRAM:
//	                  alleen ons IMAGE mag er niet staan (dat zou de FSBL
//	                  overschrijven), een app-partitie wel.
//	0x84000000  64MB  HOP: image + Go-heap (licheerv.HopBase, RamStart/RamSize)
//	0x88000000 127MB  pool A — de partities van de slots. Slot 1 landt op de
//	                  basis van deze regio, en die is licheerv.SlotBase: het
//	                  link-adres van app-images (zie hieronder).
//	0x8FF00000   1MB  HOP's eigen structuren: boot-scratch, de control-pages van
//	                  HOP's node-cores, de per-slot blokken (ctx-levensteken +
//	                  park-mailboxen) en de NIC-DMA-regio. De ABI van een slot
//	                  (control page, ringen, frame-ringen) staat hier NIET: die
//	                  woont in de staart van zijn eigen partitie.
//
// Pool-ORDE is functioneel, geen smaak: zonder tweede translatiefase draait een
// app op zijn fysieke partitie, dus zijn link-adres moet die partitie zijn.
// Daarom staat de regio die op licheerv.SlotBase begint vóóraan — slot 1 landt
// daar, en dat is waar image/licheerv-agent.sh de app linkt.
//
// Wat hier ontbreekt t.o.v. de ARM-boards is precies wat deze architectuur niet
// heeft: Stage2PA en RevokeVecPA (geen stage-2-tabellen, geen EL2-vectortabel —
// de kooi is een PMP-whitelist en die zit in registers per hart). NetDMAPA blijft 0
// tot de DWMAC in bedrijf is.
const (
	dramBase = 0x80000000
	dramTop  = 0x90000000

	// osSize is de staart van het DRAM voor HOP's per-slot-structuren. Wat er
	// écht nodig is rekent init() na — liever hard falen dan een control-page
	// die stil buiten de reservering valt.
	osSize = 0x100000 // 1MB
	osBase = dramTop - osSize

	bootScratchPA = osBase
	nodeCtrlPA    = osBase + 0x1000

	// slotBlockPA is Plan.Stage2PA. De naam komt van ARM (daar staan er ook
	// stage-2-tabellen en EL2-vectoren in), maar op deze architectuur draagt de
	// regio alleen HOP's eigen per-slot boekhouding: het ctx-levensteken dat
	// slots.Get leest, de sched-blokken die de switcher gebruikt (cpu/mmode) en
	// de park-mailboxen. Zonder plek leest dat vanaf nul.
	//
	// (MaxSlots+1) × Stage2Stride: met twee kooien is dat 3 × 64KB = 0x30000, dus
	// tot osBase+0x50000.
	slotBlockPA = osBase + 0x20000

	// netDMAPA is Plan.NetDMAPA: de ringen en frame-buffers van de DWMAC.
	// 448KB i.p.v. layout.NetDMASize (8MB): die 8MB hoort bij gigabit-MAC's met
	// grote ringen, en op een board met 256MB DRAM geef je niet 3% weg voor een
	// 100Mbit-poort die met 432KB toe kan (driver/nic/dwmac.NeedBytes).
	//
	// Was 256KB toen de ring 64 diep was; 10-08 mee omhoog met de verdieping naar
	// 128 descriptors (dwmac legt uit waarom: de ring moet een scheduler-quantum
	// overleven, niet een trage lus). Dit is het maximum dat nog in de 1MB-staart
	// past: de regio begint op +0x80000 en de staart eindigt op +0x100000, dus
	// dieper vraagt eerst een grotere osSize — en die komt uit de app-pool.
	//
	// LET OP: op de ARM-boards is deze regio ongecachet omdat hij buiten élke
	// RAM-declaratie valt. Hier valt hij dat ook, maar dat maakt hem niet
	// ongecachet — de C906 heeft in M-mode geen MMU en de sysmap bepaalt de
	// attributen. De driver doet daarom cache-onderhoud per overdracht; dit is
	// puur een gebied dat niemand anders aanraakt.
	// LET OP de plek: dit stond op +0x40000 en dat botste vanaf de tweede kooi
	// midden in de slot-blokken (die lopen dan tot +0x50000). De check onderaan
	// init() zag dat niet — die nam het MAXIMUM van de eindes en een overlap heeft
	// geen groter einde. Nu loopt de reeks strak op orde en checkt init elke grens.
	netDMAPA   = osBase + 0x80000
	netDMASize = 0x70000 // 448KB — tot het einde van de staart (+0xF0000 < 1MB)
)

func init() {
	// Eén app-hart (de C906L) maar TWEE kooien: sinds de app in supervisor-modus
	// draait en de PMP-entries niet meer gelockt zijn, kan HOP dat hart preempten
	// en de kooi omschakelen (cpu/mmode + kern/cage). Meer kooien dan cores is
	// precies waar de coöperatieve deling voor bestaat — de bewoners geven af via
	// ecall (cpu/idle), net zoals ARM dat met HVC doet.
	//
	// Twee en niet SlotCap: elke kooi kost een 64KB-blok in de 1MB-staart
	// hieronder, en er is één hart om ze op te draaien. Wie meer wil, verhoogt dit
	// getal en de check erna zegt of het past.
	// Eén logische app-core per app-hart. Uit de lijst zelf en niet als literaal,
	// want dít is de plek waar de twee tellingen moeten kloppen: kern/slots rekent
	// met aaneengesloten logische nummers 1..N en vertaalt ze via hartOf naar de
	// hart-ID's uit AppHarts. Een board dat er twee zegt en één levert (of andersom)
	// adverteert slots die nergens kunnen draaien.
	layout.SetAppCores(len(machine{}.AppHarts()))
	layout.SetMaxSlots(2)

	// Past alles in de 1MB-staart? De reeks is strak op orde: elke regio moet
	// vóór de volgende beginnen. Dat is een echte overlap-check en niet het
	// maximum-van-de-eindes dat hier eerst stond — dat laatste zag níet dat de
	// slot-blokken bij een tweede kooi in de NIC-DMA-regio zouden lopen (die maat
	// schaalt immers met MaxSlots).
	regions := []struct {
		name string
		base uint64
		size uint64
	}{
		{"boot-scratch", bootScratchPA, 0x1000},
		{"node-ctrl", nodeCtrlPA, uint64(layout.MaxSlots+1) * layout.CtrlStride},
		{"slot-blokken", slotBlockPA, uint64(layout.MaxSlots+1) * layout.Stage2Stride},
		{"nic-dma", netDMAPA, netDMASize},
	}
	if netDMASize < dwmac.NeedBytes {
		panic(fmt.Sprintf("licheerv: NIC-DMA-regio %d KB, driver vraagt %d KB",
			netDMASize>>10, dwmac.NeedBytes>>10))
	}
	end := uint64(osBase)
	for _, r := range regions {
		if r.base < end {
			panic(fmt.Sprintf("licheerv: %s (%#x) begint vóór het einde van de vorige regio (%#x)",
				r.name, r.base, end))
		}
		end = r.base + r.size
	}
	if need := end - osBase; need > osSize {
		panic(fmt.Sprintf("licheerv: OS-structuren vragen %d KB, reservering is %d KB",
			need>>10, osSize>>10))
	}

	layout.UsePlan(layout.Plan{
		RAMBase:       dramBase,
		NodeCtrlPA:    nodeCtrlPA,
		BootScratchPA: bootScratchPA,
		Stage2PA:      slotBlockPA,
		NetDMAPA:      netDMAPA,
		Pool: []layout.Region{
			{Base: licheerv.SlotBase, Size: osBase - licheerv.SlotBase}, // 127MB
			{Base: dramBase, Size: licheerv.HopBase - dramBase},         // 64MB
		},
	})
}
