// Package brcmpcie brengt een Broadcom-STB PCIe-root-complex op — de
// BCM2712-variant (Raspberry Pi 5: pcie0/1/2, waarvan pcie2 → RP1 met de
// netwerkcontroller en pcie1 → de FFC voor M.2/NVMe). GEMETEN 2026-07-10
// (probe5): de Pi 5-firmware traint GEEN enkele PCIe-link — PHYLINKUP=0,
// DL_ACTIVE=0, alle reads door het venster geven 0xdeaddead. De VideoCore
// laadt alleen de RP1-firmware (via I²C in RP1-SRAM, vóór er PCIe bestaat);
// de link zelf is aan het OS. Linux traint 'm elke boot — wij dus ook.
//
// Het recept is 1-op-1 de Linux-referentie drivers/pci/controller/
// pcie-brcmstb.c (raspberrypi/linux rpi-6.12.y, "brcm,bcm2712-pcie" →
// de BCM7712-codepaden). De BCM2712-eigenaardigheden die dáár verstopt
// zitten en zonder welke niets werkt:
//
//  1. RESCAL: één gedeeld analoog kalibratieblok voor alle drie de
//     controllers — één keer starten en pollen vóór de eerste bridge.
//  2. Bridge-reset via de externe SW_INIT-resetcontroller (bank/bit),
//     niet via het RGR1_SW_INIT_1-register van oudere chips.
//  3. PERST# via PCIE_MISC_PCIE_CTRL bit 2 (PERSTB, actief-laag-invers:
//     1 = deassert), niet via HARD_DEBUG.
//  4. HARD_DEBUG ligt op 0x4304 (oudere chips: 0x4204); SerDes komt uit
//     IDDQ door bit 27 te wissen.
//  5. De PLL van de PHY verwacht af fabriek een 100MHz-refclk; de Pi 5
//     heeft een 54MHz-kristal. Zonder de MDIO-herprogrammering in
//     PostSetup (blok 0x1600) traint de link NOOIT — dit was ons
//     0xdeaddead-raadsel.
//  6. Elk inbound-window (RC-"BAR") heeft óók een UBUS-remap-register
//     met ACCESS_EN nodig; oudere chips hebben die niet.
//
// Alleen voor GOOS=tamago GOARCH=arm64 (MMIO via metal/dev).
package brcmpcie

import (
	"fmt"
	"math/bits"
	"time"

	"hop-os/metal/dev"
)

// Registeroffsets vanaf de controller-basis (pcie_offsets_bcm7712 +
// PCIE_MISC_*-constanten uit pcie-brcmstb.c).
const (
	// RC-configruimte (bus 0 = de root-poort zelf, direct op basis+0).
	cfgCommand     = 0x04 // standaard type-1-header
	cfgPrimaryBus  = 0x18 // primary/secondary/subordinate
	cfgMemBase     = 0x20 // bridge memory base/limit
	cfgLnkSta      = 0xbc // PCIe-cap 0xac + 0x10: LNKCTL | LNKSTA<<16
	cfgLnkCtl2     = 0xdc // PCIe-cap 0xac + 0x30: target link speed [3:0]
	cfgVendorSpec1 = 0x0188
	cfgIDVal3      = 0x043c // klassecode [23:0]
	cfgLinkCap     = 0x04dc // ASPM-support [11:10], max link speed [3:0]
	cfgPhyCtl15    = 0x184c // PM-klokperiode [7:0]

	// Interne PHY-MDIO (de refclk-PLL-registers).
	mdioAddr   = 0x1100
	mdioWrData = 0x1104

	// MISC-blok.
	miscCtrl         = 0x4008
	miscWin0Lo       = 0x400c // + 8*win
	miscWin0Hi       = 0x4010 // + 8*win
	miscRCBar1Lo     = 0x402c // RC-BAR n≤3: +8*(n-1); HI op +4
	miscRCBar4Lo     = 0x40d4 // RC-BAR n≥4: +8*(n-4)
	miscCfgRetryTmo  = 0x405c
	miscPCIeCtrl     = 0x4064 // bit 2 = PERSTB (1 = PERST# lossen)
	miscPCIeStatus   = 0x4068
	miscWin0BaseLim  = 0x4070 // + 4*win: base-MB [15:4], limit-MB [31:20]
	miscWin0BaseHi   = 0x4080 // + 8*win: base-MB >> 12 [7:0]
	miscWin0LimitHi  = 0x4084 // + 8*win
	miscCtrl1        = 0x40a0
	miscVDMQoSMapHi  = 0x4164 // VDM-prioriteit → AXI-QoS-map (hoge nibbles)
	miscVDMQoSMapLo  = 0x4168 // idem, lage nibbles
	rcTLVDMCtl0      = 0x0a20 // VDM_ENABLED|IGNORETAG|IGNOREVNDRID [18:16]
	rcTLVDMCtl1      = 0x0a0c // VDM VendorID-match
	miscUBUSCtrl     = 0x40a4
	miscUBUSTmo      = 0x40a8
	miscUBUSBar1Rmp  = 0x40ac // UBUS-remap n≤3: +8*(n-1); HI op +4
	miscUBUSBar4Rmp  = 0x410c // n≥4: +8*(n-4)
	miscAXIIntfCtrl  = 0x416c
	miscAXIRdErrData = 0x4170

	hardDebug = 0x4304 // BCM2712/7712 (oudere chips: 0x4204!)

	extCfgData  = 0x8000 // 4KB-configvenster van de geselecteerde functie
	extCfgIndex = 0x9000 // ECAM-index: bus<<20 | dev<<15 | fn<<12

	// PCIE_MISC_PCIE_STATUS-bits.
	statusPHYLinkUp = 1 << 4
	statusDLActive  = 1 << 5
	statusRCMode    = 1 << 7

	// PCIE_MISC_HARD_PCIE_HARD_DEBUG-bits.
	hdClkreqDebug   = 1 << 1
	hdRefclkOvrdEn  = 1 << 16
	hdRefclkOvrdOut = 1 << 20
	hdL1SSEnable    = 1 << 21
	hdSerdesIDDQ    = 1 << 27
)

// Rescal kalibreert het gedeelde analoge blok (brcm,bcm7216-pcie-sata-rescal;
// BCM2712: 0x10_00119500). Eén keer per boot, vóór de eerste bridge-reset.
// Sequence uit reset-brcmstb-rescal.c: START zetten, teruglezen, STATUS
// pollen, START wissen.
func Rescal(base uintptr) bool {
	dev.Write32(base, dev.Read32(base)|1) // BRCM_RESCAL_START
	if dev.Read32(base)&1 == 0 {
		return false // schrijft niet — blok bestaat hier niet
	}
	ok := false
	for range 20 { // ruim boven Linux' 1ms-timeout
		if dev.Read32(base+8)&1 != 0 { // BRCM_RESCAL_STATUS
			ok = true
			break
		}
		time.Sleep(100 * time.Microsecond)
	}
	dev.Write32(base, dev.Read32(base)&^uint32(1))
	return ok
}

// InWin is één inbound-window (RC-BAR): PCIe-adres → CPU/DRAM-adres.
// Size moet een macht van twee zijn (4KB..64GB).
type InWin struct{ PCIe, CPU, Size uint64 }

// OutWin is het outbound-window: CPU-adres → PCIe-adres (MB-granulariteit).
type OutWin struct{ CPU, PCIe, Size uint64 }

// RC is één root-complex-instantie. SWInit* wijst naar de gedeelde
// brcm,brcmstb-reset-controller (BCM2712: 0x10_01504318; ID 42/43/44 voor
// pcie0/1/2).
type RC struct {
	Base     uintptr
	SWInit   uintptr
	SWInitID uint
	Gen      int     // link-snelheidsplafond (Pi 5-DT: 2 = 5GT/s)
	Out      OutWin  // win0; meer windows hebben we (nog) niet nodig
	In       []InWin // RC-BAR 1..n (max 10 op de BCM2712)
}

func (rc *RC) rd(off uintptr) uint32           { return dev.Read32(rc.Base + off) }
func (rc *RC) wr(off uintptr, v uint32)        { dev.Write32(rc.Base+off, v) }
func (rc *RC) mod(off uintptr, mask, v uint32) { rc.wr(off, rc.rd(off)&^mask|v) }

// bridgeReset bedient de SW_INIT-resetcontroller: bank = ID>>5 (stride 0x18),
// SET op +0, CLEAR op +4, bit = ID&31.
func (rc *RC) bridgeReset(assert bool) {
	off := uintptr(rc.SWInitID>>5) * 0x18
	if !assert {
		off += 4
	}
	dev.Write32(rc.SWInit+off, 1<<(rc.SWInitID&31))
	dev.MB()
}

// inSizeEnc codeert een inbound-window-grootte (SIZE-veld [4:0] van
// RC_BAR*_CONFIG_LO): log2 12..15 → 0x1c+log2-12, log2 16..36 → log2-15.
func inSizeEnc(size uint64) uint32 {
	l := bits.Len64(size) - 1
	switch {
	case l >= 12 && l <= 15:
		return uint32(0x1c + l - 12)
	case l >= 16 && l <= 36:
		return uint32(l - 15)
	}
	return 0 // uit
}

// Setup voert brcm_pcie_setup + post_setup_bcm2712 uit: bridge-resetcyclus,
// SerDes wekken, windows programmeren en de 54MHz-refclk-PLL zetten. De link
// blijft down (PERST# nog geasserteerd) tot StartLink. Geeft (rcMode, mdioOK):
// rcMode=false betekent dat de controller niet als root-complex strapt
// (fataal); mdioOK=false dat de PLL-writes niet bevestigd werden.
func (rc *RC) Setup() (rcMode, mdioOK bool) {
	// Bridge-resetcyclus. Na de reset staat PERSTB=0, dus PERST# is
	// geasserteerd — precies wat we willen tot StartLink.
	rc.bridgeReset(true)
	time.Sleep(200 * time.Microsecond)
	rc.bridgeReset(false)
	time.Sleep(200 * time.Microsecond)

	// SerDes uit IDDQ (power-down) halen en laten stabiliseren.
	rc.mod(hardDebug, hdSerdesIDDQ, 0)
	time.Sleep(200 * time.Microsecond)

	// MISC_CTRL: SCB_ACCESS_EN | CFG_READ_UR_MODE (config-reads naar niets
	// geven 0xffffffff i.p.v. een bus-abort) | RCB_MPS | RCB_64B;
	// MAX_BURST_SIZE [21:20] = 0x1 = 128B — de BCM2712-SPECIFIEKE waarde
	// (pcie-brcmstb.c: `else if (type == BCM2712) burst = 0x1 /* 128 bytes */`,
	// zelfde patroon als de beruchte 2711-quirk). Eerder stond hier 0x2/512B
	// ("generiek pad") — 4× wat Linux dit silicium toestaat, op precies het
	// inbound-pad (GEM-DMA → SDC/DRAM) van de stille totale freeze
	// (freeze-jacht 2026-07-13, referentie-agent ronde 2).
	v := rc.rd(miscCtrl)
	v |= 0x1000 | 0x2000 | 0x400 | 0x80
	v = v&^uint32(0x300000) | 0x100000
	rc.wr(miscCtrl, v)

	// Inbound-windows (RC-BAR's, 1-genummerd) — élk met zijn UBUS-remap
	// (ACCESS_EN), anders komt er stroomopwaarts niets door.
	for i, w := range rc.In {
		n := i + 1
		bar, ubus := miscRCBar1Lo+uintptr(n-1)*8, miscUBUSBar1Rmp+uintptr(n-1)*8
		if n >= 4 {
			bar, ubus = miscRCBar4Lo+uintptr(n-4)*8, miscUBUSBar4Rmp+uintptr(n-4)*8
		}
		rc.wr(bar, uint32(w.PCIe)|inSizeEnc(w.Size))
		rc.wr(bar+4, uint32(w.PCIe>>32))
		rc.wr(ubus, uint32(w.CPU)&^uint32(0xfff)|1)
		rc.wr(ubus+4, uint32(w.CPU>>32))
	}

	if rc.rd(miscPCIeStatus)&statusRCMode == 0 {
		return false, false // EP-modus gestrapt — hier valt niets te trainen
	}

	// ASPM-support = alleen L1 (Pi 5-DT: aspm-no-l0s) en klassecode
	// PCI-PCI-bridge; wij zetten ASPM zelf nooit aan, dit adverteert alleen.
	rc.mod(cfgLinkCap, 0xc00, 2<<10)
	rc.mod(cfgIDVal3, 0xffffff, 0x060400)

	// Outbound-window 0: CPU → PCIe, MB-granulariteit, hoge MB-bits apart.
	baseMB := rc.Out.CPU >> 20
	limitMB := (rc.Out.CPU + rc.Out.Size - 1) >> 20
	rc.wr(miscWin0Lo, uint32(rc.Out.PCIe))
	rc.wr(miscWin0Hi, uint32(rc.Out.PCIe>>32))
	rc.wr(miscWin0BaseLim, uint32(baseMB&0xfff)<<4|uint32(limitMB&0xfff)<<20)
	rc.mod(miscWin0BaseHi, 0xff, uint32(baseMB>>12)&0xff)
	rc.mod(miscWin0LimitHi, 0xff, uint32(limitMB>>12)&0xff)

	// Little-endian op BAR2 (default, expliciet zoals Linux).
	rc.mod(cfgVendorSpec1, 0xc, 0)

	return true, rc.postSetup2712()
}

// postSetup2712 is het verplichte BCM2712-slot van Setup: de PHY-PLL via
// MDIO omzetten naar de 54MHz-kristalrefclk (register-blok 0x1600) — zonder
// dit traint de link nooit — plus de UBUS/AXI-foutonderdrukking zodat een
// mislukte read 0xffffffff oplevert in plaats van een SError.
func (rc *RC) postSetup2712() bool {
	ok := rc.mdioWrite(0, 0x1f, 0x1600) // blokselect: refclk/PLL
	for _, w := range [...]struct {
		reg int
		val uint16
	}{
		{0x16, 0x50b9}, {0x17, 0xbda1}, {0x18, 0x0094}, {0x19, 0x97b4},
		{0x1b, 0x5030}, {0x1c, 0x5030}, {0x1e, 0x0007},
	} {
		ok = rc.mdioWrite(0, w.reg, w.val) && ok
	}
	time.Sleep(200 * time.Microsecond)
	rc.mod(cfgPhyCtl15, 0xff, 0x12) // PM-klokperiode 18.52ns = 1/54MHz

	rc.mod(miscUBUSCtrl, 0, 1<<13|1<<19) // reply-err/decerr onderdrukken
	rc.wr(miscAXIRdErrData, 0xffffffff)  // mislukte read → all-ones
	rc.wr(miscUBUSTmo, 0x0B2D0000)       // UBUS-timeout ~250ms
	rc.wr(miscCfgRetryTmo, 0x0ABA0000)   // config-retry-timeout ~240ms

	v := rc.rd(miscAXIIntfCtrl)
	v = v&^uint32(1<<7) | 1<<13 | 1<<12 | 1<<11
	rc.wr(miscAXIIntfCtrl, v)
	if rc.rd(miscAXIIntfCtrl)&(1<<12) == 0 {
		// C1-silicium: de échte QoS-fixes (bits 11/12/13, "chicken bits for
		// 2712D0") zijn hier Reserved-0 — de kapotte QoS-forwarding-search in
		// het AXI→SDC-pad is op C1 alleen te DEMPEN door outstanding requests
		// te knijpen (Linux: best-effort 15). Wij knijpen harder: 4 — de
		// outstanding-sweep van de freeze-jacht (13-07, agent-ronde 3); elke
		// stap omlaag verkleint het race-venster van het erratum.
		rc.mod(miscAXIIntfCtrl, 0x3f, 4)
		fmt.Printf("brcmpcie: C1 silicon detected (QoS fix bits reserved) — AXI outstanding throttled to 4\n")
	} else {
		fmt.Printf("brcmpcie: D0 silicon (QoS fix bits active)\n")
	}
	// VDM-QoS AAN — de Pi 5-DT eist dit voor de RP1-poort (bcm2712-rpi-5-b.dts:
	// brcm,vdm-qos-map = 0xbbaa9888; brcm_pcie_set_tc_qos). De RP1 stúúrt
	// QoS-VDM's (paniek-prioritering) zodra zijn interne FIFO's vollopen —
	// precies het sustained-RX-moment. Met VDM-receptie uit (de oude regel
	// hier) landen die berichten op een dove RC. (Freeze-jacht 2026-07-13,
	// referentie-agent ronde 2, delta #2.)
	rc.wr(miscVDMQoSMapHi, 0xbbaa9888)
	rc.wr(miscVDMQoSMapLo, 0xbbaa9888)
	rc.wr(rcTLVDMCtl1, 0) // match VendorID 0
	rc.mod(rcTLVDMCtl0, 0, 0x70000) // VDM_ENABLED|VDM_IGNORETAG|VDM_IGNOREVNDRID
	rc.mod(miscCtrl1, 0, 1<<5)      // EN_VDM_QOS_CONTROL
	return ok
}

// mdioWrite schrijft een intern PHY-register (pakket: port [19:16],
// regad [15:0], cmd [23:20] met WRITE=0) en wacht tot bit 31 zakt.
func (rc *RC) mdioWrite(port, regad int, data uint16) bool {
	rc.wr(mdioAddr, uint32(port&0xf)<<16|uint32(regad&0xffff))
	_ = rc.rd(mdioAddr)
	rc.wr(mdioWrData, 1<<31|uint32(data))
	for range 1000 {
		if rc.rd(mdioWrData)&(1<<31) == 0 {
			return true
		}
	}
	return false
}

// StartLink lost PERST# en wacht op de link (PCIe CEM: 100ms Tpvperl, dan
// pollen). Geeft de twee statusbits terug; beide waar = link getraind.
func (rc *RC) StartLink() (phy, dl bool) {
	if rc.Gen != 0 {
		// Snelheidsplafond: target-link-speed in LNKCTL2 én de max in de
		// geadverteerde link capability.
		rc.mod(cfgLnkCtl2, 0xf, uint32(rc.Gen))
		rc.mod(cfgLinkCap, 0xf, uint32(rc.Gen))
	}
	// CLKREQ/refclk-override/L1SS allemaal uit vóór de training.
	rc.mod(hardDebug, hdClkreqDebug|hdRefclkOvrdEn|hdRefclkOvrdOut|hdL1SSEnable, 0)

	rc.mod(miscPCIeCtrl, 0, 1<<2) // PERSTB=1 → PERST# lossen
	time.Sleep(100 * time.Millisecond)

	for range 40 { // ≤200ms (Linux: 100ms — ruime marge, dit is een meting)
		phy, dl = rc.LinkStatus()
		if phy && dl {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	return
}

// LinkStatus leest PHYLINKUP en DL_ACTIVE.
func (rc *RC) LinkStatus() (phy, dl bool) {
	st := rc.rd(miscPCIeStatus)
	return st&statusPHYLinkUp != 0, st&statusDLActive != 0
}

// Status geeft het rauwe PCIE_STATUS-register (diagnose).
func (rc *RC) Status() uint32 { return rc.rd(miscPCIeStatus) }

// LinkSpeedWidth leest LNKSTA na de training: (gen, lanes).
func (rc *RC) LinkSpeedWidth() (speed, width int) {
	st := rc.rd(cfgLnkSta) >> 16
	return int(st & 0xf), int(st >> 4 & 0x3f)
}

// CfgRead32/CfgWrite32 doen configruimte-toegang: bus 0 = de RC zelf (direct
// op de basis), dieper via het EXT_CFG-indexvenster. LET OP: benader nooit
// bus ≥ 1 zonder DL_ACTIVE — dat is een bus-abort.
func (rc *RC) CfgRead32(bus, devno, fn int, off uintptr) uint32 {
	return dev.Read32(rc.cfg(bus, devno, fn, off))
}

func (rc *RC) CfgWrite32(bus, devno, fn int, off uintptr, v uint32) {
	dev.Write32(rc.cfg(bus, devno, fn, off), v)
	dev.MB()
}

func (rc *RC) cfg(bus, devno, fn int, off uintptr) uintptr {
	if bus == 0 {
		return rc.Base + off
	}
	rc.wr(extCfgIndex, uint32(bus)<<20|uint32(devno)<<15|uint32(fn)<<12)
	dev.MB()
	return rc.Base + extCfgData + off&0xfff
}

// OpenBridge programmeert de type-1-headervelden van de RC zodat verkeer
// het venster door mag: busnummers (secondary=subordinate=1), het bridge-
// memory-window over het hele outbound-PCIe-bereik, en memory-decode +
// bus-mastering in het command-register.
func (rc *RC) OpenBridge() {
	rc.mod(cfgPrimaryBus, 0xffffff, 0x010100) // pri=0, sec=1, sub=1
	base := uint32(rc.Out.PCIe)
	limit := uint32(rc.Out.PCIe + rc.Out.Size - 1)
	rc.wr(cfgMemBase, limit&0xfff00000|base>>16&0xfff0)
	rc.mod(cfgCommand, 0, 0x6) // mem-decode + master
}

// EPBar is één BAR-toewijzing op de endpoint achter de RC (bus 1, dev 0, fn 0):
// Off = config-offset (0x10, 0x14, …), Val = het te schrijven basisadres. Omdat
// HOP zonder firmware-hulp boot wijst niemand deze BAR's toe — de aanroeper geeft
// de gemeten adressen mee (de RP1-eis "BAR1 → PCIe 0x0" is gewoon een Val=0).
type EPBar struct {
	Off uintptr
	Val uint32
}

// BringConfig parametriseert BringUp met de board-specifieke stukken die géén
// deel zijn van de generieke root-complex-sequence: het gedeelde RESCAL-blok, de
// verwachte endpoint-ID en de BAR-toewijzingen. De RC-instantie zelf (Base,
// SWInit, Gen, in/out-windows) draagt de rest van het adresplan.
type BringConfig struct {
	Rescal uintptr // gedeeld analoog kalibratieblok (0 = overslaan)
	WantID uint32  // verwachte vendor<<16|device van de endpoint op bus 1 (0 = niet checken)
	Bars   []EPBar // BAR-toewijzingen op de endpoint
}

// BringUp voert de volledige root-complex-bring-upsequence uit — Rescal →
// Setup → StartLink → OpenBridge → endpoint-ID controleren → BAR's toewijzen →
// mem-decode/bus-mastering aan — en centraliseert zo de orkestratie die eerst
// in elke board-ProbeNIC herhaald stond. Het board levert alleen nog de RC-
// config + BringConfig; de vaste sequence woont hier, bij de RC-primitieven.
func (rc *RC) BringUp(cfg BringConfig) error {
	if cfg.Rescal != 0 && !Rescal(cfg.Rescal) {
		return fmt.Errorf("brcmpcie: RESCAL-kalibratie bevestigt niet")
	}
	if rcMode, _ := rc.Setup(); !rcMode {
		return fmt.Errorf("brcmpcie: controller strapt niet als root-complex (status %#x)", rc.Status())
	}
	if _, dl := rc.StartLink(); !dl {
		return fmt.Errorf("brcmpcie: PCIe-link traint niet (status %#x)", rc.Status())
	}
	rc.OpenBridge()

	if cfg.WantID != 0 {
		if id := rc.CfgRead32(1, 0, 0, 0); id != cfg.WantID {
			return fmt.Errorf("brcmpcie: endpoint meldt %#x (verwacht %#x)", id, cfg.WantID)
		}
	}
	for _, b := range cfg.Bars {
		rc.CfgWrite32(1, 0, 0, b.Off, b.Val)
	}
	// mem-decode + bus-mastering op de endpoint aanzetten.
	rc.CfgWrite32(1, 0, 0, cfgCommand, rc.CfgRead32(1, 0, 0, cfgCommand)|0x6)
	dev.MB()
	return nil
}

// AssignBARs meet en programmeert de memory-BAR's van één endpoint (bus/devno,
// fn 0) achter deze root-complex: eerst alle groottes opmeten (de
// all-ones-schrijftruc + teruglezen), dan BAR1 → PCIe 0x0 (de RP1-DMA-
// loopback-eis) en de rest aaneengesloten daarachter, elk uitgelijnd op zijn
// eigen grootte, met minstens 16MB tussen BAR1 en de rest. Geeft per BAR-index
// het toegewezen PCIe-adres (^0 = afwezig), de gemeten grootte (0 = afwezig) en
// of het een 64-bit BAR was (die neemt óók de volgende index in).
//
// Eén codepad voor twee aanroepers: de rpi5-board-bring-up (stil — de
// terugwaardes worden genegeerd) en probe6 (die de meting uit de returns
// print). LET OP: dit schrijft per BAR een 64-bit hi-word alleen als de BAR
// zich als 64-bit meldt — zo blijft een aangrenzende 32-bit BAR (bv. RP1's
// buren) ongemoeid, anders dan pcie.SetBAR64 dat altijd 64-bit schrijft.
func (rc *RC) AssignBARs(bus, devno int) (addr, size [6]uint64, is64 [6]bool) {
	for i := range addr {
		addr[i] = ^uint64(0)
	}
	for i := 0; i < 6; i++ {
		off := uintptr(0x10 + i*4)
		rc.CfgWrite32(bus, devno, 0, off, 0xffffffff)
		lo := rc.CfgRead32(bus, devno, 0, off)
		if lo == 0 || lo == 0xffffffff {
			continue
		}
		if lo&0x7 == 0x4 { // 64-bit memory-BAR: neemt ook slot i+1
			rc.CfgWrite32(bus, devno, 0, off+4, 0xffffffff)
			hi := rc.CfgRead32(bus, devno, 0, off+4)
			size[i] = ^(uint64(hi)<<32 | uint64(lo&^0xf)) + 1
			is64[i] = true
		} else {
			size[i] = uint64(^(lo &^ 0xf) + 1)
		}
		if is64[i] {
			i++
		}
	}
	// Toewijzen: BAR1 → 0x0 (DMA-loopback-eis), cursor voor de rest erachter.
	cursor := size[1]
	if cursor < 0x1000000 {
		cursor = 0x1000000 // minstens 16MB vrijhouden na BAR1
	}
	for i := 0; i < 6; i++ {
		if size[i] == 0 {
			continue
		}
		a := uint64(0) // BAR1 blijft op 0x0
		if i != 1 {
			a = (cursor + size[i] - 1) &^ (size[i] - 1) // uitlijnen op eigen grootte
			cursor = a + size[i]
		}
		addr[i] = a
		off := uintptr(0x10 + i*4)
		rc.CfgWrite32(bus, devno, 0, off, uint32(a))
		if is64[i] {
			rc.CfgWrite32(bus, devno, 0, off+4, uint32(a>>32))
		}
		if is64[i] {
			i++ // 64-bit BAR nam ook slot i+1
		}
	}
	return addr, size, is64
}
