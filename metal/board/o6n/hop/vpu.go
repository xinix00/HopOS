package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/cpu/memattr"
	"github.com/xinix00/HopOS/metal/v2/cpu/psci"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/scmi"
)

// vpu.go — het videocodec-blok van de Cix P1: een Arm China Linlon V8 met vier
// cores, 8K60 decode. Dit bestand draagt precies wat geen enkele tabel ons
// vertelt: hoe je hem aan krijgt. De rest (registers, firmware, protocol) is
// board-vrij en woont in media/driver/vpu/mve. Dit bestand importeert die
// driver bewust niet — board mag media niet importeren, net zoals het gui niet
// mag (indeling.md). De knoop zit in cmd/hopos/media_o6n.go, alleen in de
// media-smaak; in headless en gui roept niemand PowerVPU aan en blijft het blok
// zonder stroom (en deze code buiten de link).
//
// De adressen staan in de DSDT van het bord, device _HID "CIXH3010": twee
// Memory32Fixed-vensters (0x14230000 en 0x14240000, elk 64KB), één Extended
// Interrupt (0x166 = SPI 326, dus INTID 358) en _CCA = 0. Dat laatste is geen
// detail: de VPU is NIET cache-coherent, dus de arena moet ongecached geheugen
// zijn — precies zoals de NIC- en NVMe-regio's.
//
// Wij lezen die vensters niet uit de AML maar zetten ze hier vast. Ze zijn een
// eigenschap van het SoC, niet van het bord, en de echte verificatie is het
// HARDWARE_ID-register: leest dat geen 0x5664, dan weigert de driver. Een AML-
// interpreter zou hier niets aan zekerheid toevoegen.
const (
	VPURCSU = 0x14230000 // reset/strap-registers naast het blok
	VPUBase = 0x14240000 // het codec-blok zelf
	VPUSize = 0x10000
	VPUIRQ  = 358 // SPI 326

	// Stroomdomeinen (SKY1_PD_*). De VPU zelf is de top plus vier cores, maar
	// hij hangt aan de multimedia-hub: staat die uit, dan komt er geen bus bij
	// het blok en leest élk register nul — ook met stroom op het blok zelf en
	// een lopende klok. Gemeten: precies dat beeld, tot de hub erbij kwam.
	pdMMHub     = 4
	pdMMHubSMMU = 5
	pdVPUTop    = 11
	pdVPUCore0  = 12
	pdVPUCores  = 4

	// Het DVFS-domein van de VPU (VPU_DFS_DOMAIN_ID). De device-tree hangt het
	// blok naast zijn stroomdomeinen ook aan een performance-domein; staat dat
	// op niveau nul, dan krijgt de core geen frequentie.
	perfVPUDomain = 9

	// De APB-klok van het blok (CLK_TREE_VPU_APBCLK). Stroom alleen is niet
	// genoeg: zonder klok tikt er niets en leest élk register nul — precies
	// wat we op ijzer zagen (0x0 op beide vensters, ook na een reset).
	clkVPUAPB = 67

	// De klok van de multimedia-interconnect (CLK_TREE_MM_NI700_CLK). De VPU
	// hangt niet rechtstreeks aan de CPU-bus maar aan een NI-700 network-on-
	// chip, en die heeft zijn eigen klok. Staat die stil, dan komt er geen
	// enkele transactie bij het blok aan: stroom en blokklok zijn dan aan en
	// élk register leest nog steeds nul.
	clkMMNI700 = 80

	// Het derde SCMI-kanaal: dat van de SCP zelf (in Linux' device-tree
	// "ap2pm", shmem@6590000 met het clock-protocol). De twee kanalen die we
	// al kenden dragen het niet — gemeten: de mailbox biedt 0x11/0x12/0x13/
	// 0x15 en de TF-A alleen 0x11. Onze DVFS-fastchannels zitten in hetzelfde
	// venster, dus het adres is bewezen bereikbaar.
	pmSCMIShmem = 0x06590000

	// Het SCMI-kanaal naar de TF-A. Dit is een ánder kanaal dan de mailbox in
	// scp.go: de doorbell is hier een SMC, en dit is het kanaal dat de
	// stroomdomeinen van GPU/VPU/NPU schakelt. De TF-A zet daarbij ook de
	// interconnect open voor niet-beveiligde toegang — zonder deze call geeft
	// de eerste registerlees op de VPU een SError die niemand meer opvangt.
	tfaSCMIShmem = 0x84380000
	tfaSCMISize  = 0x1000
	tfaSMCFuncID = 0xc2000001

	// De system reset controller. Onder ACPI regelt de TF-A de resets al bij
	// de power-on; dit is de terugval als het blok daarna toch stil blijft.
	srcBase        = 0x16000000
	srcSize        = 0x1000
	srcVPUReset    = 0x400 // bit 11: 1 = uit reset
	srcVPUResetBit = 1 << 11
	srcRCSUReset   = 0x804 // bit 19: idem voor het RCSU
	srcRCSUBit     = 1 << 19

	// De resets van de multimedia-interconnect en zijn SMMU: dezelfde
	// gedachte als de klok hierboven — het pad naar het blok moet open zijn
	// vóór het blok zelf iets kan zeggen.
	srcRCSUGroup0 = 0x800
	srcNI700Bit   = 1 << 31 // NI700_MMHUB_RCSU_RESET_N
	srcSMMUBit    = 1 << 5  // RCSU_SMMU_MMHUB_RESET_N (in group 1, 0x804)
)

// PowerVPU brengt het blok zo ver dat een driver ermee kan praten: vensters
// gemapt, arena ongecached, stroom, klok, perf-domein en reset. Het geeft de
// twee registervensters terug; wat daarna komt (firmware, page tables,
// sessies) is werk van media/driver/vpu/mve. Eén keer bij het opstarten.
func PowerVPU(arena uintptr, size uint64) (base, rcsu uintptr, err error) {
	if !IsCix() {
		return 0, 0, fmt.Errorf("vpu: not a Cix board")
	}
	if arena == 0 || size < 16<<20 {
		return 0, 0, fmt.Errorf("vpu: arena of %d MB is too small", size>>20)
	}
	if !uefi.MapHigh(VPUBase, VPUSize) || !uefi.MapHigh(VPURCSU, VPUSize) {
		return 0, 0, fmt.Errorf("vpu: cannot map the register windows")
	}

	// De arena moet ongecached zijn. De DSDT zegt _CCA = 0: dit blok loopt
	// niet mee in de cache-coherentie, dus wat wij in een cacheregel
	// achterlaten ziet de VPU niet — en zijn firmware-code, zijn ringen en
	// zijn page tables staan er allemaal in. Gemeten 22-09: met gecachte
	// arena start de firmware wel maar antwoordt hij nooit.
	if err := memattr.NormalNC(arena, uintptr(size)); err != nil {
		return 0, 0, fmt.Errorf("vpu: cannot make the arena uncached: %w", err)
	}
	if err := vpuPower(); err != nil {
		return 0, 0, err
	}

	if !uefi.MapHigh(pmSCMIShmem, 0x1000) {
		fmt.Println("vpu: cannot map the SCP SCMI channel")
	}
	vpuClock()
	vpuPerf()
	if !vpuUnreset() {
		return 0, 0, fmt.Errorf("vpu: cannot reach the reset controller")
	}

	// Wat staat er werkelijk in het eerste register? Op een bord dat we voor
	// het eerst aanraken is dat het enige getal dat telt: 0x5664 betekent dat
	// het blok wakker is, 0 of 0xffffffff dat er niets antwoordt, en iets
	// anders dat we naar onbekend silicium kijken. Eén regel op de console
	// scheelt een hele boot-cyclus raden.
	fmt.Printf("vpu: id %#x rcsu %#x (windows %#x/%#x)\n",
		dev.Read32(VPUBase), dev.Read32(VPURCSU), VPUBase, VPURCSU)
	return VPUBase, VPURCSU, nil
}

// vpuPower zet het topdomein en de vier cores aan via de TF-A. Alle vijf: de
// hardware-scheduler telt de cores die stroom hebben, en een blok dat zich als
// viercore meldt terwijl er drie uit staan loopt vast op zijn eerste job.
//
// De volgorde is opgebouwd van veilig naar riskant, want de eerste keer op een
// nieuw bord wil je weten wáár het misgaat en niet alleen dát:
//
//  1. het kanaal een versievraag stellen — puur lezen, kan niets breken;
//  2. de domeinen aanzetten;
//  3. terugvragen of ze echt aan staan.
//
// Pas als stap 3 klopt raakt de aanroeper de registers van de VPU aan. Zonder
// die volgorde is de eerste registerlees een SError die geen handler opvangt,
// en dan weet je alleen dat de node weg is.
func vpuPower() error {
	if !uefi.MapHigh(tfaSCMIShmem, tfaSCMISize) {
		return fmt.Errorf("vpu: cannot map the TF-A SCMI channel")
	}
	ch := &scmi.Channel{
		Base: tfaSCMIShmem,
		Ring: func() {
			psci.SMC(tfaSMCFuncID, tfaSCMIShmem>>12, tfaSCMIShmem&0xfff, 0)
		},
	}
	ver, err := ch.Version(scmi.ProtoPower)
	if err != nil {
		return fmt.Errorf("vpu: TF-A SCMI power protocol: %w", err)
	}
	fmt.Printf("vpu: TF-A SCMI channel alive, power protocol v%d.%d\n", ver>>16, ver&0xffff)

	domains := []uint32{pdMMHub, pdMMHubSMMU, pdVPUTop}
	for i := 0; i < pdVPUCores; i++ {
		domains = append(domains, uint32(pdVPUCore0+i))
	}
	for _, domain := range domains {
		if err := ch.PowerOn(domain, scmi.PowerOn); err != nil {
			return fmt.Errorf("vpu: power domain %d: %w", domain, err)
		}
		state, err := ch.PowerState(domain)
		if err != nil {
			return fmt.Errorf("vpu: power domain %d did not report back: %w", domain, err)
		}
		if state != scmi.PowerOn {
			return fmt.Errorf("vpu: power domain %d reports state %#x, not on", domain, state)
		}
	}
	fmt.Printf("vpu: power domains %v on (confirmed by the firmware)\n", domains)
	return nil
}

// vpuPerf haalt het performance-domein van de VPU uit stilstand (niveau nul).
// De mailbox draagt het perf-protocol (0x13) — dat is hetzelfde kanaal
// waarlangs de klok-fastchannels van de CPU's lopen. Een hogere stand kiezen
// laten we aan de firmware: zonder niveaulijst is elke keuze een gok.
func vpuPerf() {
	thermMu.Lock() // de mailbox deelt HOP met de thermometer
	defer thermMu.Unlock()
	ch := &scmi.Channel{Base: SCMIChannel}
	cur, err := ch.PerfLevel(perfVPUDomain)
	if err != nil {
		fmt.Printf("vpu: perf domain %d: %v\n", perfVPUDomain, err)
		return
	}
	fmt.Printf("vpu: perf domain %d is at level %d\n", perfVPUDomain, cur)
	if cur != 0 {
		return
	}
	// Zonder de niveaulijst is de veiligste zet een bescheiden waarde: de
	// firmware klemt naar de dichtstbijzijnde die hij kent.
	if err := ch.SetPerfLevel(perfVPUDomain, 1); err != nil {
		fmt.Printf("vpu: raising perf domain %d: %v\n", perfVPUDomain, err)
		return
	}
	if lv, err := ch.PerfLevel(perfVPUDomain); err == nil {
		fmt.Printf("vpu: perf domain %d now at level %d\n", perfVPUDomain, lv)
	}
}

// vpuClock zet de klokken van het blok en van zijn interconnect aan: stroom
// alleen is niet genoeg, zonder klok leest élk register nul. Welk SCMI-kanaal
// het clock-protocol draagt staat niet in een tabel die wij kunnen lezen — de
// device-tree van Linux wijst naar "scmi_clk". Gemeten draagt alleen ap2pm
// hem, maar we vragen elk kanaal zijn protocollen en nemen het eerste met
// klokken: een andere firmware-indeling geeft dan een regel op de console en
// geen stil blok.
func vpuClock() {
	thermMu.Lock() // de mailbox deelt HOP met de thermometer
	defer thermMu.Unlock()
	tfaRing := func() { psci.SMC(tfaSMCFuncID, tfaSCMIShmem>>12, tfaSCMIShmem&0xfff, 0) }
	channels := []struct {
		name string
		c    *scmi.Channel
	}{
		{"SCP mailbox", &scmi.Channel{Base: SCMIChannel}},
		{"SCP ap2pm", &scmi.Channel{Base: pmSCMIShmem}},
		{"TF-A", &scmi.Channel{Base: tfaSCMIShmem, Ring: tfaRing}},
	}
	for _, ch := range channels {
		if !offersClock(ch.c) {
			continue
		}
		for _, k := range []struct {
			id   uint32
			name string
		}{{clkMMNI700, "mm ni700"}, {clkVPUAPB, "vpu apb"}} {
			if err := ch.c.ClockEnable(k.id, true); err != nil {
				fmt.Printf("vpu: enabling the %s clock (%d) on the %s channel: %v\n", k.name, k.id, ch.name, err)
				continue
			}
			hz, _ := ch.c.ClockRate(k.id)
			fmt.Printf("vpu: %s clock on via the %s channel, %d MHz\n", k.name, ch.name, hz/1_000_000)
		}
		return
	}
	fmt.Println("vpu: no SCMI channel offers the clock protocol - clocks left as the firmware set them")
}

// offersClock zegt of een kanaal het clock-protocol aanbiedt.
func offersClock(c *scmi.Channel) bool {
	list, err := c.Protocols()
	if err != nil {
		return false
	}
	for _, p := range list {
		if p == scmi.ProtoClock {
			return true
		}
	}
	return false
}

// vpuUnreset haalt het blok en zijn RCSU uit reset. De bits zijn actief-laag
// (1 = uit reset), dus dit is idempotent: staat hij al te draaien, dan
// verandert er niets.
func vpuUnreset() bool {
	if !uefi.MapHigh(srcBase, srcSize) {
		return false
	}
	for _, r := range []struct {
		off uintptr
		bit uint32
	}{
		{srcRCSUGroup0, srcNI700Bit},
		{srcRCSUReset, srcSMMUBit},
		{srcRCSUReset, srcRCSUBit},
		{srcVPUReset, srcVPUResetBit},
	} {
		v := dev.Read32(srcBase + r.off)
		dev.Write32(srcBase+r.off, v|r.bit)
	}
	dev.MB()
	return true
}
