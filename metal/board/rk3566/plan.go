package rk3566

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/fdt"
)

// Het PA-plan van de Radxa Zero 3E. GEMETEN 05-08 (proberk3566): U-Boot's
// /memory-node is 0x200000..0x80000000 op de 2GB-variant — DRAM begint dus NIET
// op nul (de eerste 2MB is TF-A) en /memreserve/ bevat verder niets, dus geen
// OP-TEE-carveout om rond te plannen.
//
//	0x00200000    30MB  van de firmware-keten: TF-A bl31 en wat U-Boot er tijdens
//	                    de boot neerlegt. Wij blijven eraf; niet in de pool.
//	0x02200000    64MB  HOP: image + Go-heap (RamBase, RamStart/RamSize)
//	0x06200000     1MB  HOP's eigen structuren (zie hieronder), buiten élke
//	                    RAM-declaratie en dus device-gemapt → coherent met een
//	                    core die met MMU uit binnenkomt, zonder cache-onderhoud
//	0x06300000     1MB  de levenstekenwoorden van de cores (WakeBase) — óók
//	                    buiten het venster, en dat is gemeten: binnenin bleef een
//	                    gewekte core stil (zie park.go)
//	0x06400000     8MB  NIC-DMA (NetDMAPA) — idem ongecachet, voor de GMAC
//	0x06C00000     2MB  USB-DMA (USBDMAPA) — xHCI-ringen/contexten, idem
//	0x07000000     8MB  framebuffer (fbPA) — een RAM-buffer, geen scanout: dit
//	                    bord heeft geen firmware-buffer, dus het beeld gaat over
//	                    het netwerk de deur uit (zie hop/board.go)
//	0x07800000  ~1,8GB  pool: de partities van de slots, tot aan wat U-Boot
//	                    bovenin bezet houdt
//
// De pool komt uit de DTB (CarvePool over de gemeten banken, minus onze eigen
// gaten) en niet uit een constante: dit bord bestaat in 1/2/4GB en een vaste
// pool zou op de kleine variant fantoom-RAM uitdelen.
const (
	// De 1MB met HOP's eigen structuren, opgedeeld zoals de andere boards het
	// doen. Alle drie 4KB/2KB-uitgelijnd (UsePlan valideert dat).
	structBase  = 0x06200000
	nodeCtrlPA  = structBase + 0x00000 // control-pages van HOP's eigen cores
	stage2PA    = structBase + 0x20000 // app-core-vectoren + stage-2-tabelblokken
	revokeVecPA = structBase + 0xF0000 // EL2-vectortabel van core 0 (cpuinit-vast!)

	netDMAPA = 0x06400000 // NIC-DMA-ringen/buffers (NetDMASize)

	// USB-DMA (USBDMASize = 2MB) in het gat tussen de NIC-regio en de
	// framebuffer. Hij ligt ónder poolBase, dus het bestaande pool-gat
	// ({Base: 0, Size: poolBase}) sluit hem al uit — net als fbPA.
	usbDMAPA = 0x06C00000

	// De framebuffer. Dit bord heeft geen firmware-buffer (gemeten: U-Boot
	// patcht geen simple-framebuffer, en zijn video is niet eens meegecompileerd
	// — `Out: serial@fe660000`, geen vidconsole), dus declareren WIJ er een in
	// DRAM. Vandaag leest alleen de display-app hem (FB=1-grant → /kvm, beeld
	// over het netwerk); zodra de VOP2-scanout er is, is dít de regio die de
	// display-controller uitscant naar HDMI. Eén buffer, twee lezers — daarom
	// staat hij in het PLAN en niet in een driver.
	//
	// 1920x1080 bij 32bpp = 8100KB; de regio is 8MB, dus past hij met marge en
	// blijft hij 2MB-uitgelijnd. Hij ligt ONDER poolBase, dus het bestaande
	// pool-gat ({Base: 0, Size: poolBase}) sluit hem al uit — geen tweede gat
	// nodig, en dat is precies waarom die grens hier zit.
	fbPA     = 0x07000000
	fbSize   = 0x00800000
	poolBase = 0x07800000 // vanaf hier app-partities
)

// FB geeft de framebuffer-regio uit het plan (adres, breedte, hoogte, bytes per
// rij). Board-kennis en geen driverwerk: de resolutie is hier een VRIJE KEUZE
// omdat niemand hem uitscant — er hangt geen monitor aan deze buffer, hij wordt
// over HTTP bekeken. 1920x1080 omdat de rest van de GUI-config daarop staat.
func FB() (base uintptr, w, h, stride int) {
	const width, height, bpp = 1920, 1080, 4
	// Vangnet, en geen formaliteit: direct boven deze regio begint de
	// partitie-pool (poolBase). Wie de resolutie verhoogt zonder fbSize mee te
	// nemen laat de scanout stil in de partitie van een app lezen én schrijven —
	// een fout die zich als willekeurige app-corruptie voordoet en nooit naar het
	// beeld wijst.
	if width*bpp*height > fbSize {
		panic("rk3566: framebuffer past niet in de plan-regio (fbSize verhogen én poolBase meeschuiven)")
	}
	return fbPA, width, height, width * bpp
}

// SetupPlan registreert het plan. Alleen op de HOP-core (core 0): een app-core
// die dit pakket linkt heeft er niets te zoeken, en de scratch is daar
// read-only.
func SetupPlan() {
	if dev.MPIDR()&0xFFFFFF != 0 {
		return
	}
	p := layout.Plan{
		NodeCtrlPA:    nodeCtrlPA,
		CagePA:        stage2PA,
		TrapVecPA:     revokeVecPA,
		BootScratchPA: BootScratch,
		NetDMAPA:      netDMAPA,
		USBDMAPA:      usbDMAPA,
		RAMBase:       0x200000, // gemeten: waar het DRAM van dit bord begint
	}
	// De pool uit de gemeten banken, met onze eigen regio's eruit gesneden. De
	// terugval is LUID: op dit bord hóórt de DTB er te zijn (booti geeft hem in
	// x0), dus als dit pad loopt is er iets mis met de bootketen en mag dat niet
	// stil een halve pool opleveren.
	dtb := uintptr(dev.Read64(DTBPtr))
	banks, ok := fdt.MemRegions(dtb)
	if ok {
		var regs []layout.Region
		for _, b := range banks {
			regs = append(regs, layout.Region{Base: b.Addr, Size: b.Size})
		}
		// De gaten: alles onder poolBase (firmware, HOP, onze structuren) plus
		// wat U-Boot bovenin liet liggen en wij LIVE blijven lezen.
		//
		// Die tweede groep is GEMETEN en niet geschat, en dat is het verschil
		// tussen werkend en stil corrupt. Een vaste bovengrens (eerst 32MB onder
		// de 2GB-grens) deed twee dingen fout: het gemeten DTB-adres op dít
		// bordje was ~0x7ce9d000 en lag er 17MB ónder — dus in de pool — én de
		// grens is 2GB-specifiek terwijl dit bord in 1/2/4GB bestaat, dus op de
		// 1GB-variant beschermde hij niets. En de DTB is geen wegwerpartikel:
		// bootparam.go dereferenceert hem bij élke BootParam-aanroep en
		// SerialSuffix parseert hem opnieuw, dus een app-partitie eroverheen is
		// een node die zijn eigen naam kwijtraakt.
		holes := []layout.Region{{Base: 0, Size: poolBase}}
		if sz := fdt.BlobSize(dtb); sz > 0 {
			holes = append(holes, layout.Region{Base: uint64(dtb), Size: sz})
		}
		if s, e, ok := fdt.InitrdRegion(dtb); ok && e > s {
			holes = append(holes, layout.Region{Base: uint64(s), Size: uint64(e - s)})
		}
		p.Pool = layout.CarvePool(regs, holes, 2<<20)
	}
	if len(p.Pool) == 0 {
		fmt.Printf("WARNING HOPOS_POOL_FALLBACK: no usable DTB /memory (dtb=%#x, valid=%v) — partition pool falls back to a fixed 512MB\n",
			dtb, fdt.Valid(dtb))
		p.Pool = []layout.Region{{Base: poolBase, Size: 0x20000000}}
	}
	layout.UsePlan(p)

	// Pariteit ná UsePlan (vóórdat het plan bestaat weigert layout élke
	// accessor — die volgorde kostte de eerste agent-boot op 05-08). Dit vangt
	// wie het plan verzet zonder cpuinit.s mee te nemen: dan zet core 0 zijn
	// VBAR_EL2 op een ánder adres dan waar InitVectors de revoke-handler plugt,
	// en verdwijnt de hard-kill stil.
	if uint64(layout.TrapVecPA()) != revokeVecPA {
		panic("rk3566: REVOKE_VEC in cpuinit.s wijkt af van het PA-plan")
	}
}
