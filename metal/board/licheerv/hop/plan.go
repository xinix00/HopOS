package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board/licheerv"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/dwmac"
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
//	0x84000000  32MB  HOP: image + Go-heap (licheerv.HopBase/HopSize; 64→32 op
//	                  14-08, gemeten — zie het HopSize-comment in licheerv.go)
//	0x86000000  32MB  pool C — de hersnit-winst, als éigen regio zodat
//	                  SlotBase (het link-adres van elk app-image) blijft staan
//	0x88000000 126MB  pool A — de partities van de slots. Slot 1 landt op de
//	                  basis van deze regio, en die is licheerv.SlotBase.
//	0x8FE00000   2MB  HOP's eigen structuren: boot-scratch, de control-pages van
//	                  HOP's node-cores, de per-slot blokken (ctx-levensteken +
//	                  park-mailboxen) en de NIC-DMA-regio. De ABI van een slot
//	                  (control page, ringen, framequeues) staat hier NIET: die
//	                  woont in de staart van zijn eigen partitie.
//
// DRIE REGIO'S, EN WAAROM DAT ZO BLIJFT (19-08): HOP staat tussen het lage en
// het hoge DRAM, dus de pool valt in stukken. Eén regio van 218MB zou dat
// oplossen — HOP onderaan, op 0x80400000 — maar dat is geprobeerd en HET BOARD
// BOOT DAAR NIET. De FSBL doet onder RUNADDR meer dan zijn U-Boot-decompressie;
// wat precies is niet gemeten. Zie licheerv.HopBase voor de terugweg en de
// voorwaarde (UART eraan).
//
// Wat de fragmentatie NIET meer kan: de node vellen. De toelating vraagt sinds
// 19-08 het grootste GAT (slots.PoolLargest) in plaats van de som en weigert een
// onplaatsbare job meteen, zonder te reserveren — dat was de hand-back-lus die
// de agent verstikte. De boot-regel "pool is N region(s), largest placeable X MB"
// laat de vorm zien vóór er iets geplaatst is.
//
// Wat hier ontbreekt t.o.v. de ARM-boards is precies wat deze architectuur niet
// heeft: CagePA en TrapVecPA (geen stage-2-tabellen, geen EL2-vectortabel —
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

	// slotBlockPA is Plan.CagePA. De naam komt van ARM (daar staan er ook
	// stage-2-tabellen en EL2-vectoren in), maar op deze architectuur draagt de
	// regio alleen HOP's eigen per-slot boekhouding: het ctx-levensteken dat
	// slots.Get leest, de sched-blokken die de switcher gebruikt (cpu/mmode) en
	// de park-mailboxen. Zonder plek leest dat vanaf nul.
	//
	// (MaxSlots+1) × CageStride: met zestien kooien is dat 17 × 64KB =
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
	// met aaneengesloten logische nummers 1..N en vertaalt ze via physCore naar de
	// hart-ID's uit Cores().App. Een board dat er twee zegt en één levert (of andersom)
	// adverteert slots die nergens kunnen draaien.
	layout.SetAppCores(len(appHarts()))
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
		{"slot-blokken", slotBlockPA, uint64(layout.MaxSlots+1) * layout.CageStride},
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
		CagePA:        slotBlockPA,
		NetDMAPA:      netDMAPA,
		// DRIE regio's, omdat HOP tussen het lage en het hoge DRAM in staat.
		//
		// Dat is niet mooi — één regio zou de klasse "vrij maar niet plaatsbaar"
		// onmogelijk maken — en 19-08 is geprobeerd HOP naar 0x80400000 te
		// verhuizen om precies dat te bereiken (218MB aan één stuk). Het BOARD
		// BOOT DAAR NIET: de vendor-FSBL gebruikt onder RUNADDR meer dan alleen
		// zijn U-Boot-decompressie. Zie het comment bij licheerv.HopBase; de
		// volgende poging hoort met de UART eraan.
		//
		// Wat de drie regio's kosten en wat je ertegen beschermt: een pool in
		// stukken kan 60MB vrij hebben zonder ergens 36MB aaneen, en dan liet de
		// toelating vroeger een job toe die de plaatser moest weigeren — elke vijf
		// seconden opnieuw, tot de node door gemiste watchdog-pets omviel. Sinds
		// 19-08 vraagt de toelating het GAT (slots.PoolLargest via
		// hopos.PoolReporter) en weigert hij meteen, zonder te reserveren. De
		// fragmentatie blijft, de storing is weg.
		//
		// Pool A staat vooraan omdat element 0 het linkadres van elk app-image
		// bepaalt (cageLinkBase): slot 1 landt op licheerv.SlotBase. Dat een app
		// óók buiten zijn linkbasis kan draaien is gemeten (stulp op 0x81c00000,
		// stulp-weather op 0x86a00000) — de kooi verplaatst, zie
		// kern/cage/relocate.go — maar de orde hier blijft functioneel.
		Pool: []layout.Region{
			{Base: licheerv.SlotBase, Size: osBase - licheerv.SlotBase}, // 126MB
			{Base: dramBase, Size: licheerv.HopBase - dramBase},         // 64MB
			// Pool C: wat de HopSize-hersnit (64→32MB, 14-08) vrijgaf.
			{Base: licheerv.HopBase + licheerv.HopSize, Size: licheerv.SlotBase - licheerv.HopBase - licheerv.HopSize},
		},
	})
}
