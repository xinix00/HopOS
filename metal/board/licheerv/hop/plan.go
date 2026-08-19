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
//	0x80000000   4MB  vuil gebied: hier decomprimeert de FSBL U-Boot naar
//	                  0x80200020 (~600KB) vóórdat hij ons aanspringt. Ons image
//	                  mag er dus niet staan. Vroeger ging dit stuk ná de boot als
//	                  pool B alsnog naar apps; sinds HOP hier direct bovenop
//	                  woont is die 4MB de prijs van één aaneengesloten pool.
//	0x80400000  32MB  HOP: image + Go-heap (licheerv.HopBase/HopSize; 64→32 op
//	                  14-08, gemeten — zie het HopSize-comment in licheerv.go)
//	0x82400000 218MB  DE POOL — één regio, alle app-partities. SlotBase
//	                  (0x88000000, het link-adres van elk app-image) ligt hier
//	                  MIDDEN in en is geen grens meer: de kooi verplaatst elk
//	                  image naar de partitie die het slot kreeg.
//	0x8FE00000   2MB  HOP's eigen structuren: boot-scratch, de control-pages van
//	                  HOP's node-cores, de per-slot blokken (ctx-levensteken +
//	                  park-mailboxen) en de NIC-DMA-regio. De ABI van een slot
//	                  (control page, ringen, frame-ringen) staat hier NIET: die
//	                  woont in de staart van zijn eigen partitie.
//
// WAAROM HOP ONDERAAN STAAT (19-08): hij stond op 0x84000000, midden in het
// DRAM, en knipte de pool daarmee in drie stukken van 126, 64 en 32MB. Drie
// regio's betekent dat 60MB vrij kan zijn terwijl er nergens 36MB aan één stuk
// ligt; de toelating rekent met de som en laat zo'n job toe, de plaatser moet
// hem weigeren, en die hand-back-lus verstikte de agent tot de node door
// gemiste watchdog-pets omviel — drie keer op één dag. Onderaan tegen het vuile
// gebied is de pool één stuk en bestaat die klasse fouten niet meer.
//
// Wat het NIET verandert: SlotBase. Dat blijft 0x88000000, dus geen enkel
// gepubliceerd artifact wordt ongeldig. Dat een app buiten zijn linkbasis kan
// draaien is geen aanname maar gemeten: stulp liep op 0x81c00000 en
// stulp-weather op 0x86a00000 (ijzer, 19-08 en 18-08), en het mechanisme staat
// in kern/cage/relocate.go — de PMP-whitelist begrenst, die tabel verplaatst.
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
	//
	// 2MB sinds 14-08: in 1MB pasten maar vijf kooien, en dat was een
	// papieren plafond — de boekhouding kost 68KB per kooi, terwijl de échte
	// grens op dit board het app-RAM bij plaatsing is (HOP weigert daar al
	// luid). Eén MB uit de app-pool (191→190MB) koopt zestien kooien: geen
	// artificiële limiet meer, de fysieke telt (ontwerpprincipe).
	osSize = 0x200000 // 2MB
	osBase = dramTop - osSize

	bootScratchPA = osBase
	nodeCtrlPA    = osBase + 0x1000

	// slotBlockPA is Plan.Stage2PA. De naam komt van ARM (daar staan er ook
	// stage-2-tabellen en EL2-vectoren in), maar op deze architectuur draagt de
	// regio alleen HOP's eigen per-slot boekhouding: het ctx-levensteken dat
	// slots.Get leest, de sched-blokken die de switcher gebruikt (cpu/mmode) en
	// de park-mailboxen. Zonder plek leest dat vanaf nul.
	//
	// (MaxSlots+1) × Stage2Stride: met zestien kooien is dat 17 × 64KB =
	// 0x110000, dus tot osBase+0x130000 — daar begint de NIC-DMA.
	slotBlockPA = osBase + 0x20000

	// netDMAPA is Plan.NetDMAPA: de ringen en frame-buffers van de DWMAC.
	// 448KB i.p.v. layout.NetDMASize (8MB): die 8MB hoort bij gigabit-MAC's met
	// grote ringen, en op een board met 256MB DRAM geef je niet 3% weg voor een
	// 100Mbit-poort die met 432KB toe kan (driver/nic/dwmac.NeedBytes).
	//
	// Was 256KB toen de ring 64 diep was; 10-08 mee omhoog met de verdieping naar
	// 128 descriptors (dwmac legt uit waarom: de ring moet een scheduler-quantum
	// overleven, niet een trage lus). In de 2MB-staart (14-08) begint de regio
	// op +0x130000, ná de zestien slot-blokken, en blijft er boven +0x1A0000
	// lucht over; een diepere ring of méér kooien vraagt eerst een grotere
	// osSize — en die komt uit de app-pool.
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
	netDMAPA   = osBase + 0x130000
	netDMASize = 0x70000 // 448KB — tot +0x1A0000, ruim binnen de 2MB-staart
)

func init() {
	// Eén app-hart (de C906L) maar ZESTIEN kooien: sinds de app in
	// supervisor-modus draait en de PMP-entries niet meer gelockt zijn, kan HOP
	// dat hart preempten en de kooi omschakelen (cpu/mmode + kern/cage). Meer
	// kooien dan cores is precies waar de coöperatieve deling voor bestaat — de
	// bewoners geven af via ecall (cpu/idle), net zoals ARM dat met HVC doet.
	//
	// Zestien en niet SlotCap: de kooi-boekhouding kost 68KB per slot (64KB
	// blok + 4KB ctrl-page) en de 2MB-staart draagt er zestien met ruimte
	// over; de ECHTE grens op dit board is app-RAM bij plaatsing (HOP weigert
	// daar luid), en zestien ligt daar ruim boven — zelfs op 12MB per app is
	// de pool eerder op dan de kooien. Twee bleek op ijzer te krap ("slot 3
	// out of range 1..2", nacht 12-08 punt 3), en vijf was alleen wat er
	// toevallig in 1MB paste: een papieren plafond, geen fysieke grens.
	// Eén logische app-core per app-hart. Uit de lijst zelf en niet als literaal,
	// want dít is de plek waar de twee tellingen moeten kloppen: kern/slots rekent
	// met aaneengesloten logische nummers 1..N en vertaalt ze via hartOf naar de
	// hart-ID's uit AppHarts. Een board dat er twee zegt en één levert (of andersom)
	// adverteert slots die nergens kunnen draaien.
	layout.SetAppCores(len(machine{}.AppHarts()))
	layout.SetMaxSlots(16)

	// Past alles in de staart (osSize)? De reeks is strak op orde: elke regio moet
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
		// ÉÉN regio, sinds HOP 19-08 naar de onderkant van het DRAM ging: van
		// net boven HOP's venster tot aan de 2MB-staart, 218MB aan één stuk.
		//
		// Hier stonden drie regio's (126 boven SlotBase, 64 eronder, 32 ertussen)
		// omdat HOP in het midden lag. Dat is de fragmentatie die 19-08 de node
		// drie keer velde: 60MB vrij, nergens 36MB aaneen, dus liet de toelating
		// een job toe die de plaatser moest weigeren — en die hand-back-lus
		// verstikte de agent. Eén regio maakt dat probleem niet kleiner maar
		// onmogelijk: vrij is vrij.
		//
		// SlotBase (0x88000000) ligt nu MIDDEN in deze regio en dat is geen
		// probleem: de kooi verplaatst elk image naar de partitie die het slot
		// kreeg (kern/cage/relocate.go). De oude opmerking hier — "zonder tweede
		// translatiefase draait een app op zijn link-adres, dus staat de regio op
		// SlotBase vooraan" — was achterhaald: op ijzer draaide stulp op
		// 0x81c00000 en stulp-weather op 0x86a00000, allebei buiten hun linkbasis.
		Pool: []layout.Region{
			{Base: licheerv.HopBase + licheerv.HopSize, Size: osBase - (licheerv.HopBase + licheerv.HopSize)},
		},
	})
}
