//go:build tamago && arm64

// apcie.go — de PCIe-controller zelf opbrengen.
//
// Dit is het stuk dat tot nu toe van m1n1 kwam. Zijn `kboot_boot` roept
// `pcie_init()` aan vóór hij naar ons springt, en daarom werkte het netwerk wél
// onder de loader en niet toen we zelf het bootobject speelden — daar meldde de
// probe eerlijk `pcie: nothing on bus 0`. Wat hier staat is dezelfde
// bring-up, in Go, uit dezelfde bron als m1n1 hem heeft: de ADT.
//
// De keten is: power-domein aan (pmgr.go) → tunables op de AXI-, RC- en
// PHY-blokken (tunables.go) → de PHY-klokken aanvragen en de PHY uit reset →
// de gedeelde referentieklok starten → per poort de registers zetten, de
// poortklok aanvragen en de poort uit reset halen. Pas daarna doet `LinkUp`
// zijn werk (PERST over de GPIO en LTSSM starten) — die twee stappen zijn van
// Linux, niet van m1n1, en stonden hier al.
//
// Meevaller op deze generatie: t8132 heeft GEEN fuse-programmering. Op een
// M1/M2 moet je bits uit de efuses in de PHY-IP-registers overschrijven
// (m1n1's `pcie_fuse_bits_*`-tabellen); vanaf t8122 is `fuse_bits = NULL` en
// doet de firmware dat zelf. Dat scheelt de enige tabel die niet uit de boom
// te lezen was.
//
// Alle magische offsets hieronder komen letterlijk uit m1n1 src/pcie.c, tak
// `compat == APCIE_T8122` met `type == APCIE_T8132`. Ze zijn niet
// gedocumenteerd — Apple levert geen TRM voor dit blok — dus ze staan er zoals
// ze daar staan, met de bron erbij, en niet als een verzonnen verklaring.
package apple

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/adt"
)

const (
	// De registerindeling van /arm-io/apcie voor t8132 (m1n1 regs_t8132):
	// reg[0] config (ECAM-achtig, per poort 32KB), reg[1] RC, reg[2] PHY,
	// reg[3] PHY-IP, reg[4] AXI, reg[5] fuses. Daarna 7 gedeelde vensters,
	// en de rest is per poort.
	apcieConfigIdx  = 0
	apcieRCIdx      = 1
	apciePHYIdx     = 2
	apciePHYIPIdx   = 3
	apcieAXIIdx     = 4
	apcieSharedRegs = 7

	// Op t8122/t8132 zijn drie vensters van de oudere chips samengevoegd tot
	// reg[2]; PHY en PHY-common liggen er op een vaste offset in.
	apciePHYOff    = 0x8000
	apciePHYCmnOff = 0x4000

	// PHY-registers.
	phyCtrl           = 0x000
	phyCtrlCLK0REQ    = 1 << 0
	phyCtrlCLK1REQ    = 1 << 1
	phyCtrlCLK0ACK    = 1 << 2
	phyCtrlCLK1ACK    = 1 << 3
	phyCtrlResetT8132 = 1 << 4
	phyCmnClkMode     = 0b11

	// Poortregisters die pcie.go nog niet kende.
	portResetT602X = 0x82c // op deze generatie is DIT de poort-reset, niet 0x814
	portMSIMap     = 0x3800

	// DesignWare-kern in de config-space van de poort.
	dwcDBIROWr      = 0x8bc
	dwcDBIROWrEn    = 1 << 0
	dwcPortLinkCtl  = 0x710
	dwcPortLinkMode = 0x3F << 16
	dwcLinkWidthCtl = 0x80c
	dwcLinkWidth    = 0x1F << 8
	dwcSpeedChange  = 1 << 17
	pcieCapBase     = 0x70
	pcieLNKCAP      = 0x0c
	pcieLNKCAPSLS   = 0xF
	pcieLNKCAPMLW   = 0x3F << 4
	pcieLNKCAP2     = 0x2c
	pcieLNKCAP2SLS  = 0x7F << 1
	pcieLNKCTL2     = 0x30
	pcieLNKCTL2TLS  = 0xF

	apciePath = "/arm-io/apcie"
)

var (
	pcieUp     bool
	pcieReport string

	// PCIeLog is een optioneel spoor voor de bring-up. Nieuw silicium zonder
	// documentatie faalt met een abort die pas bij een LATERE toegang landt —
	// schrijven is gebufferd — dus "waar stierf hij" is alleen te beantwoorden
	// als elke stap zichzelf meldt vóórdat hij hem doet. De probe zet hem aan.
	PCIeLog func(string)
)

// step meldt de volgende stap en zet een barrière, zodat een uitgestelde abort
// bij de stap landt die hem veroorzaakte en niet drie stappen verderop.
func step(s string) {
	dev.MB()
	if PCIeLog != nil {
		PCIeLog(s)
	}
}

// rmw32 is lees-wijzig-schrijf op één register.
func rmw32(a uintptr, clear, set uint32) {
	dev.Write32(a, dev.Read32(a)&^clear|set)
	dev.MB()
}

// poll32 wacht tot (reg & mask) == want.
func poll32(a uintptr, mask, want uint32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if dev.Read32(a)&mask == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// PCIeUp meldt of de controller opgebracht is, en door wie. Lege string = nog
// niet geprobeerd.
func PCIeUp() string { return pcieReport }

// InitPCIe brengt de PCIe-controller en zijn poorten op. Idempotent: draait de
// controller al — omdat m1n1 ons boot en het al deed — dan blijft alles staan.
// Dat onderscheid is geen luxe: de bring-up twee keer doen zou een werkende
// link neerhalen.
func InitPCIe() error {
	if pcieUp {
		return nil
	}
	// Antwoordt de rootpoort in de config-space, dan staat de controller al.
	if v := dev.Read32(uintptr(ECAMBase)); v != 0xFFFFFFFF && v != 0 {
		pcieUp, pcieReport = true, fmt.Sprintf("apcie: already up (root port %04x:%04x) — left as the boot loader set it", v&0xFFFF, v>>16)
		return nil
	}

	t, ok := ADT()
	if !ok {
		return fmt.Errorf("apcie: no device tree")
	}
	chain, ok := t.PathTrace(apciePath)
	if !ok {
		return fmt.Errorf("apcie: no %s node", apciePath)
	}
	node := chain[len(chain)-1]

	reg := func(i int) uintptr {
		base, _, ok := t.RegAt(chain, i)
		if !ok {
			return 0
		}
		return uintptr(base)
	}
	configBase, rc, phyIP, axi := reg(apcieConfigIdx), reg(apcieRCIdx), reg(apciePHYIPIdx), reg(apcieAXIIdx)
	phy := reg(apciePHYIdx) + apciePHYOff
	phyCmn := reg(apciePHYIdx) + apciePHYCmnOff
	if configBase == 0 || rc == 0 || phy == apciePHYOff || phyIP == 0 || axi == 0 {
		return fmt.Errorf("apcie: incomplete reg set in the device tree")
	}

	nPorts := int(t.U32(node, "#ports", 0))
	_, regSize, _ := t.Prop(node, "reg")
	nRegs := int(regSize / 16)
	if nPorts == 0 || nRegs <= apcieSharedRegs {
		return fmt.Errorf("apcie: %d ports, %d reg windows — cannot divide", nPorts, nRegs)
	}
	portRegCnt := (nRegs - apcieSharedRegs) / nPorts

	tune := 0
	apply := func(n adt.Node, prop string, base uintptr) {
		c, ok := applyTunables(t, n, prop, base)
		if ok {
			tune += c
		}
		step(fmt.Sprintf("apcie:   %s @ %#x: %d line(s), present=%v", prop, base, c, ok))
	}

	step(fmt.Sprintf("apcie: config %#x rc %#x phy %#x phy-ip %#x axi %#x, %d ports, %d reg/port",
		configBase, rc, phy, phyIP, axi, nPorts, portRegCnt))

	// 1. Stroom. Zonder dit antwoordt geen enkel register hieronder.
	gates := PowerEnable(apciePath)
	step(fmt.Sprintf("apcie: %d power gate(s) enabled", gates))

	// 2. De gedeelde blokken: AXI-brug, root complex, PHY.
	apply(node, "apcie-axi2af-tunables", axi)
	dev.Write32(rc+0x4, 0) // m1n1 zet dit zonder toelichting; wij ook
	dev.MB()
	apply(node, "apcie-common-tunables", rc)
	apply(node, "apcie-phy-tunables", reg(apciePHYIdx))

	// 3. De PHY: twee klokken aanvragen, wachten op de bevestiging, uit reset.
	rmw32(phy+phyCtrl, 0, phyCtrlCLK0REQ)
	if !poll32(phy+phyCtrl, phyCtrlCLK0ACK, phyCtrlCLK0ACK, 50*time.Millisecond) {
		return fmt.Errorf("apcie: PHY CLK0 not acknowledged (CTRL %#x)", dev.Read32(phy+phyCtrl))
	}
	rmw32(phy+phyCtrl, 0, phyCtrlCLK1REQ)
	if !poll32(phy+phyCtrl, phyCtrlCLK1ACK, phyCtrlCLK1ACK, 50*time.Millisecond) {
		return fmt.Errorf("apcie: PHY CLK1 not acknowledged (CTRL %#x)", dev.Read32(phy+phyCtrl))
	}
	rmw32(phy+phyCtrl, phyCtrlResetT8132, 0)
	time.Sleep(time.Millisecond)

	dev.Write32(phy+4, dev.Read32(phy+4)|0x01)
	dev.MB()
	apply(node, "apcie-phy-ip-pll-tunables", phyIP)
	apply(node, "apcie-phy-ip-auspma-tunables", phyIP)
	dev.Write32(phy+4, dev.Read32(phy+4)|0x10)
	dev.MB()

	// 4. De gedeelde referentieklok aanzetten en het root complex starten.
	rmw32(phyCmn, phyCmnClkMode, 1)
	if !poll32(phy+0x8, 1, 1, 250*time.Millisecond) {
		return fmt.Errorf("apcie: PHY clock did not start (%#x)", dev.Read32(phy+0x8))
	}
	rmw32(phy+phyCtrl, 0, 0x200)
	dev.Write32(rc+0x54, 0x140)
	dev.Write32(rc+0x50, 0x1)
	dev.MB()
	if !poll32(rc+0x58, 1, 1, 250*time.Millisecond) {
		return fmt.Errorf("apcie: root complex did not start (RC+0x58 %#x)", dev.Read32(rc+0x58))
	}

	// 5. De poorten. Het config-venster van poort N is dat van device N op bus
	// 0 (32KB per device) — hier RECHTSTREEKS uit het poortnummer, waar m1n1
	// een teller meeschuift die alleen opschuift voor poorten mét een brug in
	// de boom. Dat verschil is zichtbaar op deze machine: er is geen
	// pci-bridge1, dus m1n1 schrijft de DesignWare-instellingen van poort 2 in
	// de configruimte van device 1. Onschadelijk gebleken, maar niet wat er
	// bedoeld is — en één getal minder om mee te slepen.
	ports := 0
	for port := 0; port < nPorts; port++ {
		bridge, ok := t.Path(fmt.Sprintf("%s/pci-bridge%d", apciePath, port))
		if !ok {
			continue
		}
		i := port*portRegCnt + apcieSharedRegs
		pb, ltssm, pphy := reg(i), reg(i+1), reg(i+2)
		if pb == 0 || pphy == 0 {
			return fmt.Errorf("apcie: port %d has no register windows", port)
		}
		_ = ltssm // alleen de t602x-tak gebruikt hem

		cfg := configBase + uintptr(port)<<15
		step(fmt.Sprintf("apcie: port %d base %#x phy %#x config %#x", port, pb, pphy, cfg))
		if err := initPort(t, bridge, pb, pphy, cfg, apply); err != nil {
			return err
		}
		ports++
	}

	pcieUp = true
	pcieReport = fmt.Sprintf("apcie: brought up by HopOS — %d power gate(s), %d tunable(s), %d of %d port(s)",
		gates, tune, ports, nPorts)
	return nil
}

// initPort doet de per-poort-bring-up. De reeks vaste schrijfacties komt
// letterlijk uit m1n1's T8122-tak; ze zijn ongedocumenteerd en staan hier in
// dezelfde volgorde, want de volgorde is het enige wat we ervan weten.
func initPort(t adt.Tree, bridge adt.Node, pb, pphy, cfg uintptr, apply func(adt.Node, string, uintptr)) error {
	for _, w := range []struct {
		off uintptr
		val uint32
	}{
		{0x088, 0x110}, {0x100, 0xffffffff}, {0x148, 0xffffffff}, {0x210, 0xffffffff},
		{0x080, 0}, {0x084, 0}, {0x104, 0xfffffff0}, {0x124, 0x100}, {0x16c, 0},
		{0x13c, 0x10}, {0x800, 0x100100}, {0x808, 0x1000ff}, {0x82c, 0},
	} {
		dev.Write32(pb+w.off, w.val)
	}
	for i := uintptr(0); i < 16; i++ { // RID→stream-tabel leeg
		dev.Write32(pb+portRID2SID+i*4, 0)
	}
	for i := uintptr(0); i < 512; i++ { // MSI-kaart leeg
		dev.Write32(pb+portMSIMap+i*4, 0)
	}
	for _, w := range []struct {
		off uintptr
		val uint32
	}{
		{0x130, 0x3000000}, {0x140, 0x10}, {0x144, 0x253770},
		{0x21c, 0}, {0x834, 0}, {0x83c, 0},
	} {
		dev.Write32(pb+w.off, w.val)
	}
	dev.MB()

	apply(bridge, "apcie-config-tunables", pb)
	rmw32(pb+portAPPCLK, 0, 1)

	// De poort-PHY: dezelfde twee klokken als de gedeelde PHY, maar per poort.
	rmw32(pphy+phyCtrl, phyCtrlCLK0REQ|phyCtrlCLK1REQ, 0)
	rmw32(pphy+phyCtrl, 0, phyCtrlCLK0REQ)
	if !poll32(pphy+phyCtrl, phyCtrlCLK0ACK, phyCtrlCLK0ACK, 50*time.Millisecond) {
		return fmt.Errorf("apcie: port PHY CLK0 not acknowledged (CTRL %#x)", dev.Read32(pphy+phyCtrl))
	}
	rmw32(pphy+phyCtrl, 0, phyCtrlCLK1REQ)
	if !poll32(pphy+phyCtrl, phyCtrlCLK1ACK, phyCtrlCLK1ACK, 50*time.Millisecond) {
		return fmt.Errorf("apcie: port PHY CLK1 not acknowledged (CTRL %#x)", dev.Read32(pphy+phyCtrl))
	}
	rmw32(pphy+phyCtrl, 0x10, 0)
	rmw32(pphy+phyCtrl, 0, 0x200)
	rmw32(pphy+phyCtrl, 0, 0x400)

	rmw32(pb+portResetT602X, 0, 1)
	if !poll32(pb+portSTATUS, 1, 1, 250*time.Millisecond) {
		return fmt.Errorf("apcie: port did not come up (STATUS %#x)", dev.Read32(pb+portSTATUS))
	}
	if !poll32(pb+portLINKSTS, 1<<2, 0, 250*time.Millisecond) {
		return fmt.Errorf("apcie: port stayed busy (LINKSTS %#x)", dev.Read32(pb+portLINKSTS))
	}

	// touch tikt na élke schrijf het poortblok aan. Een externe abort op deze
	// bus is UITGESTELD: hij landt bij de eerstvolgende toegang, niet bij de
	// schrijf die hem veroorzaakte. Zonder deze tik is "waar ging het mis" een
	// gok; mét is het een regel.
	touch := func(what string) {
		dev.MB()
		if PCIeLog != nil {
			PCIeLog(fmt.Sprintf("apcie:   %-22s LINKSTS %#x", what, dev.Read32(pb+portLINKSTS)))
		}
	}
	touch("port ready")

	// De DesignWare-kern: alleen-lezen-registers even beschrijfbaar maken, de
	// tunables van de brug erin, snelheid en breedte vastzetten, dicht.
	rmw32(cfg+dwcDBIROWr, 0, dwcDBIROWrEn)
	touch("DBI open")
	apply(bridge, "pcie-rc-tunables", cfg)
	touch("rc-tunables")
	apply(bridge, "pcie-rc-gen3-shadow-tunables", cfg)
	touch("gen3-shadow")
	apply(bridge, "pcie-rc-gen4-shadow-tunables", cfg)
	touch("gen4-shadow")

	if speed := maxLinkSpeed(t, bridge); speed > 0 {
		rmw32(cfg+pcieCapBase+pcieLNKCAP, pcieLNKCAPSLS, speed)
		rmw32(cfg+pcieCapBase+pcieLNKCAP2, pcieLNKCAP2SLS, (1<<speed-1)<<1)
		a := cfg + pcieCapBase + pcieLNKCTL2
		dev.Write16(a, dev.Read16(a)&^uint16(pcieLNKCTL2TLS)|uint16(speed))
		touch(fmt.Sprintf("speed %d", speed))
		rmw32(cfg+dwcLinkWidthCtl, 0, dwcSpeedChange)
		touch("speed change")
	}
	rmw32(cfg+dwcPortLinkCtl, dwcPortLinkMode, 1<<16) // 1 lane
	touch("lane mode")
	rmw32(cfg+dwcLinkWidthCtl, dwcLinkWidth, 1<<8) // breedte 1
	touch("link width")
	rmw32(cfg+pcieCapBase+pcieLNKCAP, pcieLNKCAPMLW, 1<<4)
	touch("LNKCAP width")
	rmw32(cfg+dwcDBIROWr, dwcDBIROWrEn, 0)
	touch("DBI closed")
	return nil
}

// maxLinkSpeed leest de maximale linksnelheid van een brug. Staat er 1, dan
// mag het kind hem verhogen — Apple hangt die override aan een eigenschap die
// per device anders heet (m1n1: het 10GB-ethernet gebruikt target-link-speed,
// de kaartlezer expected-link-speed).
func maxLinkSpeed(t adt.Tree, bridge adt.Node) uint32 {
	speed := t.U32(bridge, "maximum-link-speed", 0)
	if speed != 1 {
		return speed
	}
	t.Children(bridge, func(n adt.Node) bool {
		if v := t.U32(n, "target-link-speed", 0); v > 0 {
			speed = v
		} else if v := t.U32(n, "expected-link-speed", 0); v > 0 {
			speed = v
		}
		return false // alleen het eerste kind, zoals m1n1
	})
	return speed
}
