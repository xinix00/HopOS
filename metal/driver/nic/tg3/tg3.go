// Package tg3 is de driver voor Broadcom's NetXtreme-familie (tg3), zoals hij
// in élke Mac mini zit: de BCM57762 achter een Apple-PCIe-rootpoort
// (14e4:1682, PCIe x1 Gen1). Referentie is Linux' drivers/net/ethernet/
// broadcom/tg3.c — dat is waar de registerkaart, de resetvolgorde en de
// firmware-handshake vandaan komen; wat hier staat is het kleinste deel
// daarvan dat een node aan het netwerk krijgt.
//
// Wat deze driver NIET doet, en waarom dat mag: geen interrupts (HopOS pollt,
// zoals alle NIC-drivers hier), geen jumbo-frames, geen TSO/checksum-offload,
// geen statistieken-DMA, geen WoL, geen ASF/firmware-management. De jumbo- en
// mini-rings blijven leeg — één standaard-ring van 2KB-buffers dekt 1500-byte
// frames.
//
// DMA-model: de ringen en buffers liggen in één aaneengesloten regio die de
// aanroeper aanwijst (op Apple: de NetDMA-regio uit het PA-plan, buiten élke
// RAM-declaratie en dus device-gemapt, met de DART in bypass — dan is een
// DMA-adres gewoon een fysiek adres). Geen cache-onderhoud nodig; de driver
// doet wél barriers rond elke ring-update, want de NIC leest ze asynchroon.
package tg3

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// Registers (Linux tg3.h). De eerste 0x100 bytes van BAR0 spiegelen de
// PCI-config-space; de device-registers zitten daarachter.
const (
	pciCommand      = 0x0004
	pciMiscHostCtrl = 0x0068 // config-shadow: CHIPREV in bits 31:16
	pciPCIState     = 0x0070
	pciGen15ProdID  = 0x00fc // PRODID_ASICREV voor de 5776x-familie

	// MISC_HOST_CTRL-bits. INDIR_ACCESS is de sleutel tot het hele indirecte
	// venster: staat dat bit uit, dan verdwijnt élke schrijf naar
	// MEM_WIN_BASE/DATA en leest élke lees nul. tg3 schrijft deze waarde als
	// állereerste handeling, vóór welke andere toegang dan ook
	// (tg3_enable_register_access), en opnieuw na elke chip-reset.
	miscMaskPCIInt  = 1 << 1
	miscWordSwap    = 1 << 3
	miscPCIStateRW  = 1 << 4
	miscIndirAccess = 1 << 7
	miscChipRevMask = 0xffff0000

	// PCISTATE zoals tg3_restore_pci_state hem na een reset achterlaat.
	pciStateROMEnable = 1 << 5
	pciStateROMRetry  = 1 << 6

	macMode      = 0x0400
	macStatus    = 0x0404
	macAddr0High = 0x0410
	macAddr0Low  = 0x0414
	macMIComm    = 0x044c
	macMIMode    = 0x0454
	macTxMode    = 0x045c
	macRxMode    = 0x0468
	macMILEDStat = 0x0450 // MAC_MI_STAT
	macLEDCtrl   = 0x040c
	macRxMTUSize = 0x043c
	macTxLengths = 0x0464
	macRcvRule   = 0x0500 // MAC_RCV_RULE_CFG
	macLowWmark  = 0x0504 // MAC_LOW_WMARK_MAX_RX_FRAME

	grcMode    = 0x6800
	grcMiscCfg = 0x6804

	// MAC_MODE-bits
	modeReset        = 1 << 0
	modeHalfDuplex   = 1 << 1
	modePortMaskGMII = 0x0c
	modePortGMII     = 0x08
	modePortMII      = 0x04
	modeRxStatEnab   = 1 << 11
	modeTxStatEnab   = 1 << 14
	// De data-engines. Zonder deze drie doet de MAC niets met de ringen: de
	// link staat en de tellers lopen, maar er komt geen enkel frame binnen
	// (gemeten 29-08: 0 frames in 5s met alleen de STAT-bits).
	modeTDEEnable  = 1 << 21 // transmit data engine
	modeRDEEnable  = 1 << 22 // receive data engine
	modeFHDEEnable = 1 << 23 // frame header data engine

	// MI_COM (MDIO)
	miCmdRead      = 0x08000000
	miCmdWrite     = 0x04000000
	miStart        = 0x20000000
	miBusy         = 0x20000000
	miPhyShift     = 21
	miRegShift     = 16
	miDataMask     = 0xffff
	miModeAutoPoll = 1 << 4
	miModeBase     = 0x000c0000

	grcCoreClkReset = 1 << 0
	grcMiscCfgPCIe  = 1 << 29 // tg3 zet dit bit op PCIe-chips apart vóór de reset

	// De 57765/57766-familie vraagt om een handvol eigen instellingen; tg3 doet
	// ze in tg3_reset_hw vóór de ringen. De namen komen uit tg3.h.
	pciDMARWCtrl      = 0x006c
	dmaRWDisCacheAlgn = 0x00000001
	dmaRWWriteCmd     = 0x7 << 28
	dmaRWReadCmd      = 0x6 << 24

	cpmuLSPD10MBClk   = 0x3604
	cpmuLSPDMACClkMsk = 0x001f0000
	cpmuLSPDMACClk625 = 0x00130000
	cpmuPadRngCtl     = 0x3668
	cpmuPadRngRDIV2   = 0x00040000

	grcModePCIeDLSel  = 0x20000000
	grcModePCIePortMk = 0x60000000
	pcieTLDLPLPort    = 0x7c00
	pcieDLLoFTSMax    = 0x000c
	pcieDLLoFTSMaxMsk = 0x000000ff
	pcieDLLoFTSMaxVal = 0x0000002c

	// GRC_MODE-swapbits. Niet optioneel op een little-endian host: de chip is
	// intern big-endian, en deze bits zetten DMA-data (BSWAP/WSWAP_DATA) en
	// descriptors (WSWAP_NONFRM_DATA) in host-volgorde. tg3 zet ze altijd;
	// alleen BSWAP_NONFRM_DATA is big-endian-only.
	grcSwapData   = 0x10 | 0x20 // BSWAP_DATA | WSWAP_DATA
	grcSwapNonFrm = 0x04        // WSWAP_NONFRM_DATA

	// PHY-adres van de ingebouwde PHY (tg3: TG3_PHY_MII_ADDR).
	phyAddr = 1

	// Standaard MII-registers.
	miiBMCR     = 0x00
	miiBMSR     = 0x01
	miiPHYID1   = 0x02
	miiPHYID2   = 0x03
	miiANAR     = 0x04
	miiCTRL1000 = 0x09
	miiSTAT1000 = 0x0a
	miiAuxStat  = 0x19 // Broadcom: auxiliary status (snelheid/duplex)

	bmcrReset     = 1 << 15
	bmcrANEnable  = 1 << 12
	bmcrANRestart = 1 << 9
	bmsrLinkUp    = 1 << 2
	bmsrANDone    = 1 << 5
)

// Net is één NIC.
type Net struct {
	Base uintptr // BAR0 (registerblok)
	Cfg  uintptr // PCI config space van deze functie (ECAM), 0 = BAR0-spiegel
	mac  net.HardwareAddr
	r    rings

	fwMbox uint32 // laatste waarde uit de firmware-mailbox (diagnose)
}

func (n *Net) rd(off uintptr) uint32    { return dev.Read32(n.Base + off) }
func (n *Net) wr(off uintptr, v uint32) { dev.Write32(n.Base+off, v); dev.MB() }

// New maakt een driver voor het registerblok op base met het meegegeven MAC.
// Het MAC komt van buiten omdat de chip het na een PERST niet meer weet: er
// staat dan Broadcom's default (00:10:18:00:00:00) in MAC_ADDR_0, en het echte
// adres woont in de ADT (Apple) of in NVRAM.
func New(base, cfg uintptr, mac net.HardwareAddr) *Net {
	return &Net{Base: base, Cfg: cfg, mac: mac}
}

// HardwareAddr geeft het MAC van deze NIC.
func (n *Net) HardwareAddr() net.HardwareAddr { return n.mac }

// ChipRev geeft de ASIC-revisie. Meestal staat die in MISC_HOST_CTRL, maar de
// waarde 0xf betekent daar "kijk in het product-ID-register" — en pas dán weet
// je met welke familie je te maken hebt. Op deze mini komt daar 0x57766 uit, en
// dat verschil telt: een 57766 is 57765_CLASS maar géén 5717_PLUS, en dat
// bepaalt onder meer of de standaard-ring een SRAM-adres nodig heeft.
func (n *Net) ChipRev() uint32 {
	rev := n.rd(pciMiscHostCtrl) >> 16
	if rev>>12 == 0xf {
		return n.cfgRead32(pciGen15ProdID) >> 12
	}
	return rev >> 12
}

// cfgRead32/cfgWrite32 praten met de echte PCI-config-space van deze functie.
// Valt terug op de BAR0-spiegel (de eerste 0x100 bytes) als de aanroeper geen
// ECAM-adres meegaf.
func (n *Net) cfgRead32(off uintptr) uint32 {
	if n.Cfg != 0 {
		return dev.Read32(n.Cfg + off)
	}
	return n.rd(off)
}

func (n *Net) cfgWrite32(off uintptr, v uint32) {
	if n.Cfg != 0 {
		dev.Write32(n.Cfg+off, v)
	} else {
		dev.Write32(n.Base+off, v)
	}
	dev.MB()
}

// enableRegAccess is tg3_enable_register_access: MISC_HOST_CTRL zo zetten dat
// register-toegang werkt. Het bit dat telt is INDIR_ACCESS — zonder dat bestaat
// het indirecte venster niet, en dus ook de weg naar NIC-SRAM niet. Dat is waar
// de ring-control-blocks van de send- en return-ring wonen; blijven die leeg,
// dan weet de chip niet waar zijn ringen liggen en doet hij geen enkele DMA.
// De CHIPREV-bits (31:16) zijn read-only maar schrijven we mee terug, precies
// zoals tg3 dat doet.
func (n *Net) enableRegAccess() {
	v := n.cfgRead32(pciMiscHostCtrl)&miscChipRevMask |
		miscMaskPCIInt | miscWordSwap | miscIndirAccess | miscPCIStateRW
	n.cfgWrite32(pciMiscHostCtrl, v)
}

// pollFirmware wacht tot de bootcode van de chip klaar is: die zet MAGIC1 in de
// firmware-mailbox en daarna het complement (tg3_poll_fw). Uitblijven is geen
// fout — niet elke kaart draagt firmware — maar de waarde is wél de eerlijkste
// meting dat het SRAM-venster werkt: een dood venster geeft nul.
func (n *Net) pollFirmware() uint32 {
	deadline := time.Now().Add(time.Second)
	var val uint32
	for time.Now().Before(deadline) {
		if val = n.readMem(sramFirmwareMbox); val == ^uint32(firmwareMboxMagic1) {
			break
		}
	}
	return val
}

// Reset doet de core-clock-reset en brengt de chip terug in een staat waarin
// hij aanspreekbaar is. Dit is tg3_chip_reset teruggebracht tot wat een kale
// bring-up nodig heeft: geen NVRAM-lock, geen APE-lock, geen ASF-handshake
// (management-firmware draait hier niet). Wat er WEL in moet, en wat drie
// avonden kostte om te vinden: de reset wist het indirecte venster én de
// command-bits in de config space. Zonder ze terug te zetten staat de link
// keurig op 1 Gb/s en gebeurt er verder niets.
func (n *Net) Reset() error {
	// Vóór élke andere toegang: het indirecte venster aan.
	n.enableRegAccess()

	// De core-clock-reset wist memory- en bus-master-enable in PCI_COMMAND
	// (tg3_save_pci_state); zonder bus-master doet de chip daarna geen DMA.
	cmd := n.cfgRead32(pciCommand)

	// MAC uit, dan de reset zelf. Op PCIe-chips schrijft tg3 GRC_MISC_CFG in
	// twee stappen: eerst bit 29 alleen, daarna bit 29 mét CORECLK_RESET. Geen
	// read-modify-write — de resetwaarde van het register is wat we willen.
	n.wr(macMode, modeHalfDuplex)
	time.Sleep(time.Millisecond)

	n.wr(grcMiscCfg, grcMiscCfgPCIe)
	n.wr(grcMiscCfg, grcMiscCfgPCIe|grcCoreClkReset)
	time.Sleep(120 * time.Microsecond)
	n.cfgRead32(pciCommand) // posted write eruit duwen; tg3 doet exact dit
	time.Sleep(120 * time.Microsecond)

	// tg3_restore_pci_state: venster terug, retry-gedrag, command-bits terug.
	n.enableRegAccess()
	n.cfgWrite32(pciPCIState, pciStateROMEnable|pciStateROMRetry)
	n.cfgWrite32(pciCommand, cmd)

	// De memory arbiter moet aan voordat er iets uit NIC-SRAM komt: hij regelt
	// het verkeer naar dat geheugen, en zonder hem kan de chip ook zijn eigen
	// ring-control-blocks niet ophalen.
	n.wr(memarbMode, n.rd(memarbMode)|modeEnable)

	// Antwoordt hij weer? De config-shadow op offset 0 draagt vendor/device.
	deadline := time.Now().Add(500 * time.Millisecond)
	ok := false
	for !ok && time.Now().Before(deadline) {
		if n.rd(0)&0xffff == 0x14e4 {
			ok = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		return fmt.Errorf("tg3: chip did not answer after reset (id %#x)", n.rd(0))
	}

	n.fwMbox = n.pollFirmware()
	n.wr(grcMode, grcSwapData|grcSwapNonFrm)
	n.tune57765()
	return nil
}

// tune57765 is wat tg3 extra doet voor de 57765/57766-familie, in dezelfde
// volgorde als tg3_reset_hw. Drie dingen die je niet zelf verzint: een
// pad-ring-deler tegen zend-hangers, een PCIe-DL-instelling voor het aantal
// fast-training-sequences, en de klok voor 10Mb/s. En daarna DMA_RW_CTRL — de
// lees/schrijf-commando's en de cache-uitlijning van de DMA-engines, die tg3
// uit zijn eigen DMA-test haalt en voor deze familie op een vaste waarde zet.
func (n *Net) tune57765() {
	n.wr(cpmuPadRngCtl, n.rd(cpmuPadRngCtl)|cpmuPadRngRDIV2)

	// De lage 1K van het PCIe-DL-blok is alleen zichtbaar via een venster in
	// GRC_MODE; daarna het venster netjes terugzetten.
	grc := n.rd(grcMode)
	n.wr(grcMode, grc&^uint32(grcModePCIePortMk)|grcModePCIeDLSel)
	v := n.rd(pcieTLDLPLPort+pcieDLLoFTSMax) &^ uint32(pcieDLLoFTSMaxMsk)
	n.wr(pcieTLDLPLPort+pcieDLLoFTSMax, v|pcieDLLoFTSMaxVal)
	n.wr(grcMode, grc)

	n.wr(cpmuLSPD10MBClk, n.rd(cpmuLSPD10MBClk)&^uint32(cpmuLSPDMACClkMsk)|cpmuLSPDMACClk625)

	n.wr(pciDMARWCtrl, n.rd(pciDMARWCtrl)&^uint32(dmaRWDisCacheAlgn)|
		dmaRWWriteCmd|dmaRWReadCmd|dmaRWDisCacheAlgn)
}

// SetMAC schrijft het MAC-adres in MAC_ADDR_0 (en daarmee in het RX-filter).
func (n *Net) SetMAC() {
	if len(n.mac) != 6 {
		return
	}
	n.wr(macAddr0High, uint32(n.mac[0])<<8|uint32(n.mac[1]))
	n.wr(macAddr0Low, binary.BigEndian.Uint32(n.mac[2:6]))
}

// mdioSetup zet auto-polling uit: zolang de MAC zelf de PHY pollt, botsen onze
// MI_COM-transacties met de zijne.
func (n *Net) mdioSetup() {
	n.wr(macMIMode, miModeBase)
	time.Sleep(40 * time.Microsecond)
}

// MDIORead leest een PHY-register.
func (n *Net) MDIORead(reg int) (uint16, error) {
	n.wr(macMIComm, miCmdRead|miStart|phyAddr<<miPhyShift|uint32(reg)<<miRegShift)
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		v := n.rd(macMIComm)
		if v&miBusy == 0 {
			return uint16(v & miDataMask), nil
		}
	}
	return 0, fmt.Errorf("tg3: MDIO read reg %d timed out", reg)
}

// MDIOWrite schrijft een PHY-register.
func (n *Net) MDIOWrite(reg int, val uint16) error {
	n.wr(macMIComm, miCmdWrite|miStart|phyAddr<<miPhyShift|uint32(reg)<<miRegShift|uint32(val))
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n.rd(macMIComm)&miBusy == 0 {
			return nil
		}
	}
	return fmt.Errorf("tg3: MDIO write reg %d timed out", reg)
}

// PHYID geeft de PHY-identificatie (OUI + model) — de goedkoopste controle dat
// de MDIO-bus werkt: 0x0000/0xffff betekent "niemand thuis".
func (n *Net) PHYID() (uint32, error) {
	n.mdioSetup()
	hi, err := n.MDIORead(miiPHYID1)
	if err != nil {
		return 0, err
	}
	lo, err := n.MDIORead(miiPHYID2)
	if err != nil {
		return 0, err
	}
	return uint32(hi)<<16 | uint32(lo), nil
}

// LinkUp wacht tot de PHY een link meldt en geeft snelheid (Mb/s) en duplex.
// Autonegotiatie wordt opnieuw gestart als hij nog niet klaar is — de kabel kan
// er net in zijn gegaan.
func (n *Net) LinkUp(timeout time.Duration) (speed int, fullDuplex bool, err error) {
	n.mdioSetup()

	bmcr, err := n.MDIORead(miiBMCR)
	if err != nil {
		return 0, false, err
	}
	if bmcr&bmcrANEnable == 0 {
		if err := n.MDIOWrite(miiBMCR, bmcr|bmcrANEnable|bmcrANRestart); err != nil {
			return 0, false, err
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// BMSR moet twee keer gelezen worden: het link-bit is latching-laag.
		if _, err := n.MDIORead(miiBMSR); err != nil {
			return 0, false, err
		}
		bmsr, err := n.MDIORead(miiBMSR)
		if err != nil {
			return 0, false, err
		}
		if bmsr&bmsrLinkUp != 0 {
			return n.linkSpeed()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, false, fmt.Errorf("tg3: no link within %s", timeout)
}

// linkSpeed leest snelheid en duplex uit Broadcom's auxiliary status (reg 0x19),
// bits 10:8 — dezelfde tabel als tg3_setup_copper_phy gebruikt.
func (n *Net) linkSpeed() (int, bool, error) {
	aux, err := n.MDIORead(miiAuxStat)
	if err != nil {
		return 0, false, err
	}
	switch aux >> 8 & 0x7 {
	case 1:
		return 10, false, nil
	case 2:
		return 10, true, nil
	case 3:
		return 100, false, nil
	case 5:
		return 100, true, nil
	case 6:
		return 1000, false, nil
	case 7:
		return 1000, true, nil
	}
	return 0, false, fmt.Errorf("tg3: unknown link state (aux %#x)", aux)
}

// SetPortMode zet de MAC in de modus die bij de gemeten snelheid hoort.
func (n *Net) SetPortMode(speed int, fullDuplex bool) {
	m := n.rd(macMode) &^ uint32(modePortMaskGMII|modeHalfDuplex)
	if speed == 1000 {
		m |= modePortGMII
	} else {
		m |= modePortMII
	}
	if !fullDuplex {
		m |= modeHalfDuplex
	}
	n.wr(macMode, m|modeRxStatEnab|modeTxStatEnab|modeTDEEnable|modeRDEEnable|modeFHDEEnable)
}

// ── Ringen, RX en TX ────────────────────────────────────────────────────────
//
// Eén standaard-ring van 2KB-buffers (geen jumbo, geen mini), één return-ring
// en één send-ring; alles in de DMA-regio die de aanroeper aanwijst. De NIC
// schrijft zijn voortgang in het status-blok, wij pollen dat — er is geen
// interrupt in het spel, net als bij elke andere NIC-driver hier.
//
// De descriptors staan in host-volgorde (little-endian): GRC_MODE krijgt geen
// swap-bits, precies zoals tg3 op een little-endian host doet.
const (
	rxStdRing = 512  // TG3_RX_STD_MAX_SIZE_5700 — genoeg, en zonder LRG_PROD_RING_CAP
	rxRetRing = 512  // return-ring: even groot als de producer-ring
	txRing    = 512  // TG3_TX_RING_SIZE
	rxBufSize = 2048 // 1536 payload (TG3_RX_STD_DMA_SZ) op een 2K-korrel
	rxDMASize = 1536

	rxDescSize = 32 // struct tg3_rx_buffer_desc
	txDescSize = 16 // struct tg3_tx_buffer_desc

	// Indeling van de DMA-regio.
	offStatus = 0x0000 // status-blok (80 bytes; ruim op een eigen pagina)
	offRxStd  = 0x1000 // producer-ring
	offRxRet  = offRxStd + rxStdRing*rxDescSize
	offTxBD   = offRxRet + rxRetRing*rxDescSize
	offRxBuf  = 0x20000 // pakketbuffers
	offTxBuf  = offRxBuf + rxStdRing*rxBufSize

	// NeedBytes is wat de driver aan DMA-geheugen vraagt (het board reserveert
	// het in zijn PA-plan; op Apple is dat NetDMAPA/NetDMASize).
	NeedBytes = offTxBuf + txRing*rxBufSize

	// Registers voor de ringen (tg3.h).
	// LET OP de twee vensters: 0x78/0x80 is het REGISTER-venster,
	// 0x7c/0x84 het GEHEUGEN-venster. Ze door elkaar halen kostte drie boots:
	// je schrijft de offset in het ene venster en de data in het andere, en
	// leest dan je eigen offset terug (gemeten 29-08).
	memWinBase = 0x007c // TG3PCI_MEM_WIN_BASE_ADDR
	memWinData = 0x0084 // TG3PCI_MEM_WIN_DATA

	hostccMode     = 0x3c00
	hostccRxCol    = 0x3c08
	hostccTxCol    = 0x3c0c
	hostccRxMax    = 0x3c10
	hostccTxMax    = 0x3c14
	hostccStatusHi = 0x3c38
	hostccStatusLo = 0x3c3c

	rcvdbdiJumboBD = 0x2440 // BDINFO: host_addr(8) maxlen_flags(4) nic_addr(4)
	rcvdbdiStdBD   = 0x2450
	rcvdbdiMiniBD  = 0x2460
	rcvbdiStdThr   = 0x2c18
	stdReplenishLW = 0x2d00 // STD_REPLENISH_LWM (57765-plus)

	rcvlpcConfig      = 0x2010
	rcvlpcStatsCtrl   = 0x2014
	rcvlpcStatsEnable = 0x2018
	snddataiStatsCtrl = 0x0c08
	snddataiStatsEnab = 0x0c0c

	// De reserve-control van de read-DMA: op 57765-plus zet tg3 hier een
	// FIFO-overflow-fix in (TG3_RDMA_RSRVCTRL_FIFO_OFLW_FIX).
	rdmaRsrvCtrl   = 0x4900
	rdmaFifoOflwFx = 0x4

	// De memory arbiter: zonder hem is de on-chip SRAM onbereikbaar (het
	// venster leest nul) en kan de chip zijn eigen ring-control-blocks niet
	// ophalen — dus geen DMA en geen frames.
	memarbMode         = 0x4000
	bufmgrMode         = 0x4400
	bufmgrMBRdmaLow    = 0x4410
	bufmgrMBMacRxLow   = 0x4414
	bufmgrMBHighWater  = 0x4418
	bufmgrDMALowWater  = 0x4434
	bufmgrDMAHighWater = 0x4438

	rdmacMode    = 0x4800
	wdmacMode    = 0x4c00
	snddataiMode = 0x0c00
	snddatacMode = 0x1000
	sndbdsMode   = 0x1400
	sndbdiMode   = 0x1800
	sndbdcMode   = 0x1c00
	rcvlpcMode   = 0x2000
	rcvdbdiMode  = 0x2400
	rcvdccMode   = 0x2800
	rcvbdiMode   = 0x2c00
	rcvccMode    = 0x3000
	rcvlscMode   = 0x3400

	modeEnable = 0x2
	modeAttn   = 0x4 // ATTN_ENABLE: hetzelfde bit in bijna elk MODE-register

	// De DMA-engines: alleen ENABLE is niet genoeg. tg3 zet altijd de hele rij
	// foutmeldings-bits erbij (target/master abort, pariteit, adres-overflow,
	// FIFO over/under-run, lange lees) — die horen bij een werkende engine,
	// niet bij diagnose.
	dmacErrEnab       = 0x4 | 0x8 | 0x10 | 0x20 | 0x40 | 0x80 | 0x100 | 0x200
	rdmacFifoLongBrst = 0x00030000 // PCIe
	rdmacJmb2KMMRR    = 0x00800000 // 57766, MTU <= 1500
	rdmacIPv6LSOEn    = 0x10000000 // 57765-plus
	wdmacStatusTagFix = 0x20000000 // 5755-plus

	hostccMode32Byte = 0x100 // 32-byte status-blok (tp->coalesce_mode)
	rcvdbdiInvRingSz = 0x10  // ringgrootte staat in de RCB
	rcvbdiRCBAttn    = 0x4
	rcvlpcClass0Attn = 0x4
	rcvlpcMAPoorAttn = 0x8
	rcvlpcStatOflow  = 0x10
	rcvRuleDefault   = 0x8        // RCV_RULE_CFG_DEFAULT_CLASS
	rcvlpcStatsDack  = 0x00040000 // RCVLPC_STATSENAB_DACK_FIX
	txModeEnable     = 0x2
	txModeMbufFix    = 0x100 // TX_MODE_MBUF_LOCKUP_FIX (5755-plus)
	rxModeEnable     = 0x2
	rxModeIPv6Csum   = 0x01000000 // 5755-plus
	macHashReg0      = 0x0470     // vier woorden: het multicast-filter
	macModeRxStatClr = 0x1000
	macModeTxStatClr = 0x8000
	miStatLnkAttn    = 0x1

	bdinfoDisabled = 0x2
	bdinfoSize     = 0x10
	bdCacheMaxCnt  = 128 // TG3_SRAM_RX_STD_BDCACHE_SIZE_5700

	// NIC-SRAM: de RCB's van de send- en return-ring wonen daar, bereikbaar
	// via het memory-window (tg3_write_mem).
	sramSendRCB   = 0x0100
	sramRcvRetRCB = 0x0200
	sramTxBufDesc = 0x4000
	sramRxBufDesc = 0x6000

	// De bootcode van de chip meldt zich hier: eerst MAGIC1, dan het
	// complement. Meteen de goedkoopste test of het venster überhaupt leeft.
	sramFirmwareMbox   = 0x0b50
	firmwareMboxMagic1 = 0x4b657654

	// Mailboxes (64-bit; wij schrijven de lage helft, offset +4).
	mbRxStdProd = 0x0268 + 4
	mbRxRetCons = 0x0280 + 4
	mbTxProd    = 0x0300 + 4
	mbInterrupt = 0x0200 + 4

	bdinfoMaxlenShift = 16
	txdFlagEnd        = 0x0004
	rxdFlagEnd        = 0x0004 // RXD_FLAG_END in type_flags van élke producer-BD
	rxdFlagError      = 0x0400
	// RXD_ERR_MASK uit tg3.h — let op dat ODD_NIBBLE_RCVD_MII (0x100000) er
	// NIET in zit: dat bit zet de chip ook op frames die verder in orde zijn.
	rxdErrMask      = 0x01ef0000
	sdStatusUpdated = 0x1
)

// ringstaat: waar wij zijn in elke ring. De NIC's kant staat in het status-blok.
type rings struct {
	dma      uintptr // basis van de DMA-regio
	rxStdIdx uint32  // volgende plek in de producer-ring die wij vullen
	rxRetIdx uint32  // onze consumer-index in de return-ring
	txProd   uint32  // onze producer-index in de send-ring
}

// writeMem schrijft een woord in NIC-SRAM via het geheugenvenster
// (tg3_write_mem). Het venster wordt daarna op nul gezet — zo laat tg3 het ook
// achter. Dit werkt alleen als INDIR_ACCESS aanstaat; zie enableRegAccess.
func (n *Net) writeMem(off, val uint32) {
	dev.Write32(n.Base+memWinBase, off)
	dev.Write32(n.Base+memWinData, val)
	dev.Write32(n.Base+memWinBase, 0)
	dev.MB()
}

// readMem leest een woord uit NIC-SRAM via hetzelfde venster als writeMem.
func (n *Net) readMem(off uint32) uint32 {
	dev.Write32(n.Base+memWinBase, off)
	v := dev.Read32(n.Base + memWinData)
	dev.Write32(n.Base+memWinBase, 0)
	return v
}

// SelfTest meet in één regel waar de ringen op staan of vallen: doet het
// SRAM-venster het (via BAR0 zoals tg3, en via de config space als tweede
// mening), staat INDIR_ACCESS aan, staat bus-mastering aan, en wat zei de
// bootcode. De probe drukt hem af vóór Init, zodat een dode ring meteen een
// oorzaak heeft in plaats van een symptoom.
func (n *Net) SelfTest() string {
	const probe = 0x5a5a1234
	const off = sramSendRCB + 8

	n.writeMem(off, probe)
	bar := n.readMem(off)

	cfg := "n/a"
	if n.Cfg != 0 {
		dev.Write32(n.Cfg+memWinBase, off)
		dev.Write32(n.Cfg+memWinData, ^uint32(probe))
		dev.Write32(n.Cfg+memWinBase, off)
		v := dev.Read32(n.Cfg + memWinData)
		dev.Write32(n.Cfg+memWinBase, 0)
		cfg = fmt.Sprintf("%#x", v)
	}

	state := "dead"
	if bar == probe {
		state = "OK"
	}
	return fmt.Sprintf("sram[%#x] bar=%#x cfg=%s (%s)  MISC_HOST_CTRL=%#x PCI_CMD=%#x fw_mbox=%#x",
		off, bar, cfg, state,
		n.cfgRead32(pciMiscHostCtrl), n.cfgRead32(pciCommand)&0xffff, n.fwMbox)
}

// Stats leest de MAC-tellers rechtstreeks uit hun registers, zoals
// tg3_periodic_fetch_stats. Ze staan volledig los van DMA: lopen ze op terwijl
// het status-blok nul blijft, dan ontvángt de MAC wel degelijk en strandt het
// verkeer pas op weg naar het geheugen van de host. Dat is het verschil tussen
// een MAC-probleem en een transport-probleem, en het kost geen enkele buffer.
func (n *Net) Stats() string {
	const (
		txOctets = 0x0800
		txUcast  = 0x086c
		txBcast  = 0x0874
		rxOctets = 0x0880
		rxUcast  = 0x088c
		rxMcast  = 0x0890
		rxBcast  = 0x0894
		rxFCSErr = 0x0898
	)
	return fmt.Sprintf("rx oct=%d ucast=%d mcast=%d bcast=%d fcs_err=%d | tx oct=%d ucast=%d bcast=%d",
		n.rd(rxOctets), n.rd(rxUcast), n.rd(rxMcast), n.rd(rxBcast), n.rd(rxFCSErr),
		n.rd(txOctets), n.rd(txUcast), n.rd(txBcast))
}

// RCBDump leest de twee ring-control-blocks terug uit NIC-SRAM: staat er wat we
// dachten te schrijven, dan weet de chip waar zijn ringen liggen.
func (n *Net) RCBDump() string {
	one := func(base uint32) string {
		return fmt.Sprintf("addr=%08x%08x maxlen=%#x nic=%#x",
			n.readMem(base), n.readMem(base+4), n.readMem(base+8), n.readMem(base+12))
	}
	return "send[" + one(sramSendRCB) + "] ret[" + one(sramRcvRetRCB) + "]"
}

// Counters leest de tellers van de list-placement-eenheid. Die staat precies op
// de plek waar het nu misgaat: de MAC ontvangt, maar er belandt niets in de
// ringen. Elke teller wijst een andere schuldige aan — geen BD beschikbaar,
// door een filter gevallen, of een volle werkrij.
func (n *Net) Counters() string {
	const (
		lpcStatus     = 0x2004
		lpcNonEmpty   = 0x200c
		lpcDropFilter = 0x2240
		lpcWQFull     = 0x2244
		lpcNoRcvBD    = 0x224c
		lpcInDiscards = 0x2250
		lpcInErrors   = 0x2254
		lpcThreshHit  = 0x2258
		dbdiStatus    = 0x2404
		dbdiStdConIdx = 0x2474
		bdiStatus     = 0x2c04
		bdiStdProdIdx = 0x2c0c
	)
	return fmt.Sprintf("lpc status=%#x nonempty=%#x drop_filter=%d wq_full=%d no_rcv_bd=%d "+
		"in_discards=%d in_errors=%d thresh_hit=%d | dbdi status=%#x std_con=%d | bdi status=%#x std_prod=%d",
		n.rd(lpcStatus), n.rd(lpcNonEmpty), n.rd(lpcDropFilter), n.rd(lpcWQFull),
		n.rd(lpcNoRcvBD), n.rd(lpcInDiscards), n.rd(lpcInErrors), n.rd(lpcThreshHit),
		n.rd(dbdiStatus), n.rd(dbdiStdConIdx), n.rd(bdiStatus), n.rd(bdiStdProdIdx))
}

// setBDInfo schrijft een ring-control-block: waar de ring in host-geheugen
// staat, hoe groot hij is, en (alleen voor de send-ring) waar zijn spiegel in
// NIC-SRAM ligt.
func (n *Net) setBDInfo(sram uint32, addr uint64, maxlenFlags, nicAddr uint32) {
	n.writeMem(sram+0, uint32(addr>>32))
	n.writeMem(sram+4, uint32(addr))
	n.writeMem(sram+8, maxlenFlags)
	n.writeMem(sram+12, nicAddr)
}

// STAND 29-08 (zie docs/archief/apple-m4.md): reset, MAC, MDIO, PHY en link
// werken (1000 Mb/s full duplex). De DMA lag stil omdat het SRAM-venster nul
// teruggaf; de oorzaak stond in tg3_enable_register_access en is nu na te lezen
// bij enableRegAccess: MISC_HOST_CTRL_INDIR_ACCESS moet aan vóór de eerste
// indirecte toegang, en de core-clock-reset wist het bit weer. Twee dingen die
// daar naast lagen en dezelfde herkomst hebben: de reset wist ook de
// command-bits in de config space (bus-master weg = geen DMA), en de swap-bits
// in GRC_MODE stonden uit terwijl tg3 ze óók op een little-endian host zet.
//
// Wat hierna nog kan opspelen, in volgorde van waarschijnlijkheid: de
// waterlijnen van de buffer-manager (nu de 57765-waarden uit
// tg3_init_bufmgr_config), en TG3PCI_DMA_RW_CTRL — dat register komt bij tg3
// uit een DMA-test tijdens de probe, en wij laten de resetwaarde staan.

// wrMbox schrijft een mailbox en leest hem terug: dat duwt de posted write de
// PCIe-brug uit, zoals tg3's tw32_mailbox_f.
func (n *Net) wrMbox(off uintptr, v uint32) {
	dev.Write32(n.Base+off, v)
	dev.Read32(n.Base + off)
}

// Init zet de ringen op en brengt de datapaden aan. dmaBase moet minstens
// NeedBytes groot zijn, device-gemapt (ongecached) en fysiek aaneengesloten;
// met de DART in bypass is het DMA-adres gelijk aan dit fysieke adres.
//
// De volgorde is die van tg3_reset_hw en dat is geen toeval: de chip wil zijn
// ringen kennen vóór de engines lopen, en de engines vóór de MAC. Wat hier
// afwijkt van Linux is alleen wat we niet doen — geen jumbo, geen RSS/TSS, geen
// TSO-firmware, geen statistieken-DMA, geen interrupts.
func (n *Net) Init(dmaBase, dmaSize uintptr) error {
	if dmaSize < NeedBytes {
		return fmt.Errorf("tg3: dma region %d bytes, need %d", dmaSize, NeedBytes)
	}
	n.r = rings{dma: dmaBase}
	dev.Clear(dmaBase, uint64(offRxBuf)) // status-blok en ringen leeg

	// GRC: host bezit de send-BD's, pseudo-header-checksum voor TX doen wij
	// niet in hardware, en de swap-bits (zie grcSwapData) horen erbij — die
	// zetten descriptors en DMA-data in host-volgorde.
	const grcHostStackup, grcHostSendBDs, grcNoTxPhdr, grcIRQMacAttn = 0x00010000, 0x00020000, 0x00100000, 0x04000000
	n.wr(grcMode, grcSwapData|grcSwapNonFrm|grcHostStackup|grcHostSendBDs|grcNoTxPhdr|grcIRQMacAttn)

	// Buffer-manager: de waterlijnen voor 57765-plus (tg3_init_bufmgr_config).
	n.wr(bufmgrMBRdmaLow, 0x0)
	n.wr(bufmgrMBMacRxLow, 0x2a)
	n.wr(bufmgrMBHighWater, 0xa0)
	n.wr(bufmgrDMALowWater, 0x5)
	n.wr(bufmgrDMAHighWater, 0xa)
	n.wr(bufmgrMode, modeEnable|modeAttn)
	deadline := time.Now().Add(100 * time.Millisecond)
	for n.rd(bufmgrMode)&modeEnable == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("tg3: buffer manager did not start (mode %#x)", n.rd(bufmgrMode))
		}
	}

	// Wanneer vult de chip zijn eigen BD-cache bij (tg3_setup_rxbd_thresholds).
	n.wr(rcvbdiStdThr, rxStdRing/8)
	n.wr(stdReplenishLW, bdCacheMaxCnt)

	// De producer-ring vullen: elke descriptor wijst naar zijn eigen buffer, en
	// draagt in opaque zijn eigen index — de return-ring geeft die terug, en zo
	// weten we welke buffer een binnengekomen frame draagt.
	for i := 0; i < rxStdRing; i++ {
		d := dmaBase + offRxStd + uintptr(i)*rxDescSize
		buf := uint64(dmaBase) + offRxBuf + uint64(i)*rxBufSize
		dev.Write32(d+0, uint32(buf>>32))
		dev.Write32(d+4, uint32(buf))
		dev.Write32(d+8, rxDMASize)             // idx_len: len in de lage helft
		dev.Write32(d+12, rxdFlagEnd)           // type_flags: END, anders neemt de chip hem niet
		dev.Write32(d+28, uint32(i)|0x00010000) // opaque: index | RING_STD
	}
	dev.MB()

	// De producer-ring aanmelden. Op 57765-plus staat de ringgrootte in de
	// bovenste helft van maxlen_flags en de DMA-lengte ×4 in de lage.
	rxStd := uint64(dmaBase) + offRxStd
	n.wr(rcvdbdiStdBD+0, uint32(rxStd>>32))
	n.wr(rcvdbdiStdBD+4, uint32(rxStd))
	// Waar de chip zijn eigen kopie van de producer-ring bewaart. Dit veld is
	// geen decoratie: laat je het staan, dan zoekt de ontvangsteenheid zijn
	// descriptors op het adres dat er toevallig in stond, leest daar rommel als
	// bufferlengte, en meldt voor élk frame FRM_TOO_BIG. tg3 slaat deze regel
	// alleen over op de 5717-familie; een 57766 is 57765_CLASS maar géén
	// 5717_PLUS, en heeft hem dus nodig (gekost: een avond, gevonden 29-08).
	n.wr(rcvdbdiStdBD+12, sramRxBufDesc)
	n.wr(rcvdbdiJumboBD+8, bdinfoDisabled) // geen jumbo-ring
	n.wr(rcvdbdiStdBD+8, rxStdRing<<bdinfoMaxlenShift|rxDMASize<<2)

	// Alle buffers aanbieden: de producer wijst één voorbij de laatste.
	n.r.rxStdIdx = rxStdRing - 1
	n.wrMbox(mbRxStdProd, n.r.rxStdIdx)

	// tg3_rings_reset. De ongebruikte RCB's expliciet uitzetten — de chip loopt
	// ze anders af en leest rommel: 4 send-ringen en 17 return-ringen op
	// 5717-plus, waarvan wij er van elk één gebruiken.
	// 57765-klasse: 2 send-ringen en 4 return-ringen, waarvan wij er van elk
	// één gebruiken. Verder gaan dan de chip kent is niet onschuldig — dan
	// schrijf je BDINFO-vlaggen over SRAM dat van de bootcode is.
	for i := 1; i < 2; i++ {
		n.writeMem(sramSendRCB+uint32(i)*bdinfoSize+8, bdinfoDisabled)
	}
	for i := 1; i < 4; i++ {
		n.writeMem(sramRcvRetRCB+uint32(i)*bdinfoSize+8, bdinfoDisabled)
	}
	n.wrMbox(mbInterrupt, 1) // interrupts gemaskeerd: wij pollen
	n.wrMbox(mbTxProd, 0)
	n.wrMbox(mbRxRetCons, 0)

	st := uint64(dmaBase) + offStatus
	n.wr(hostccStatusHi, uint32(st>>32))
	n.wr(hostccStatusLo, uint32(st))

	// Return- en send-ring: hun RCB's staan in NIC-SRAM.
	n.setBDInfo(sramSendRCB, uint64(dmaBase)+offTxBD, txRing<<bdinfoMaxlenShift, sramTxBufDesc)
	n.setBDInfo(sramRcvRetRCB, uint64(dmaBase)+offRxRet, rxRetRing<<bdinfoMaxlenShift, 0)

	// MAC-adres, frame-maten en de ontvangstregel. MAC_RX_MTU_SIZE staat na een
	// reset niet vanzelf goed: is hij te klein, dan gooit de MAC élk frame weg.
	n.SetMAC()
	// Het multicast-filter helemaal open. Een node hoort mDNS, ARP-achtigen en
	// IPv6-buurontdekking te zien; een hash bijhouden per aangemeld adres is
	// werk dat pas loont als er iets te winnen valt.
	for i := uintptr(0); i < 4; i++ {
		n.wr(macHashReg0+i*4, 0xffffffff)
	}
	n.wr(macRxMTUSize, 1500+14+4+4) // MTU + ethernet-kop + FCS + VLAN
	n.wr(macTxLengths, 2<<12|6<<8|32)
	n.wr(macRcvRule, rcvRuleDefault)
	n.wr(rcvlpcConfig, 0x0181)

	// De statistieken-tellers. Wij lezen ze niet, maar de blokken willen ze
	// aan hebben staan (tg3 doet dit onvoorwaardelijk).
	n.wr(rcvlpcStatsEnable, n.rd(rcvlpcStatsEnable)&^uint32(rcvlpcStatsDack))
	n.wr(rcvlpcStatsCtrl, 1)
	n.wr(snddataiStatsEnab, 0xffffff)
	n.wr(snddataiStatsCtrl, 0x3) // ENABLE | FASTUPD

	// Host coalescing: eerst uit, dan de drempels, dan aan met een 32-byte
	// status-blok — dat is wat de chip DMA't en waar wij in pollen.
	n.wr(hostccMode, 0)
	deadline = time.Now().Add(50 * time.Millisecond)
	for n.rd(hostccMode)&modeEnable != 0 && time.Now().Before(deadline) {
	}
	n.wr(hostccRxCol, 60)
	n.wr(hostccTxCol, 60)
	n.wr(hostccRxMax, 1) // één frame is genoeg om het blok bij te werken
	n.wr(hostccTxMax, 1)
	n.wr(hostccMode, modeEnable|hostccMode32Byte)

	n.wr(rcvccMode, modeEnable|modeAttn)
	// De attentie-bits erbij (tg3 zet alleen ENABLE): zonder deze latcht
	// RCVLPC_STATUS niet, en dan zegt een weggegooid frame niets over de reden.
	// CLASS0 = frame in de wegwerpklasse, MAPOOR = mbuf-pool op.
	n.wr(rcvlpcMode, modeEnable|rcvlpcClass0Attn|rcvlpcMAPoorAttn|rcvlpcStatOflow)

	// De MAC-data-engines aan. Zonder deze drie staat de link en gebeurt er
	// niets: geen enkel frame komt binnen (gemeten 29-08).
	n.wr(macMode, n.rd(macMode)|modeRxStatEnab|modeTxStatEnab|
		modeTDEEnable|modeRDEEnable|modeFHDEEnable|macModeRxStatClr|macModeTxStatClr)
	time.Sleep(40 * time.Microsecond)

	// De DMA-engines. De FIFO-overflow-fix hoort bij 57765-plus.
	n.wr(rdmaRsrvCtrl, n.rd(rdmaRsrvCtrl)|rdmaFifoOflwFx)
	n.wr(wdmacMode, modeEnable|dmacErrEnab|wdmacStatusTagFix)
	time.Sleep(40 * time.Microsecond)
	n.wr(rdmacMode, modeEnable|dmacErrEnab|rdmacFifoLongBrst|rdmacIPv6LSOEn|rdmacJmb2KMMRR)
	time.Sleep(40 * time.Microsecond)

	// En de blokken erboven, in tg3's volgorde.
	n.wr(rcvdccMode, modeEnable|modeAttn)
	n.wr(snddatacMode, modeEnable)
	n.wr(sndbdcMode, modeEnable|modeAttn)
	n.wr(rcvbdiMode, modeEnable|rcvbdiRCBAttn)
	n.wr(rcvdbdiMode, modeEnable|rcvdbdiInvRingSz)
	n.wr(snddataiMode, modeEnable)
	n.wr(sndbdiMode, modeEnable|modeAttn)
	n.wr(sndbdsMode, modeEnable|modeAttn)

	// Pas nu de MAC zelf.
	n.wr(macTxMode, txModeEnable|txModeMbufFix)
	time.Sleep(100 * time.Microsecond)
	n.wr(macRxMode, rxModeEnable|rxModeIPv6Csum)
	time.Sleep(10 * time.Microsecond)
	n.wr(macMILEDStat, miStatLnkAttn)
	n.wr(macLowWmark, 1) // 57765-klasse: niet droppen bij flow control
	dev.MB()
	return nil
}

// statusWord leest een 32-bit woord uit het status-blok.
func (n *Net) statusWord(off uintptr) uint32 { return dev.Read32(n.r.dma + offStatus + off) }

// rxProducer is de index tot waar de NIC in de return-ring geschreven heeft
// (status-blok, idx[0].rx_producer — de lage helft van het woord op +16).
func (n *Net) rxProducer() uint32 { return n.statusWord(16) & 0xffff }

// txConsumer is hoever de NIC met de send-ring is (idx[0].tx_consumer).
func (n *Net) txConsumer() uint32 { return n.statusWord(16) >> 16 }

// Receive haalt één frame op; 0, nil betekent "niets binnengekomen".
func (n *Net) Receive(buf []byte) (int, error) {
	prod := n.rxProducer()
	if prod == n.r.rxRetIdx {
		return 0, nil
	}
	d := n.r.dma + offRxRet + uintptr(n.r.rxRetIdx)*rxDescSize
	idxLen := dev.Read32(d + 8)
	flags := dev.Read32(d+12) & 0xffff
	errVLAN := dev.Read32(d + 20)
	opaque := dev.Read32(d + 28)
	// De lengte in de descriptor telt de FCS mee; die hoort niet bij het frame
	// (tg3_rx: `len = ... - ETH_FCS_LEN`). Vier bytes te veel doorgeven betekent
	// vier bytes rommel achter élk pakket dat de stack krijgt.
	length := int(idxLen&0xffff) - 4
	src := n.r.dma + offRxBuf + uintptr(opaque&0xffff)*rxBufSize

	n.r.rxRetIdx = (n.r.rxRetIdx + 1) % rxRetRing
	n.wr(mbRxRetCons, n.r.rxRetIdx)

	// De buffer weer aanbieden: zijn descriptor staat al goed (adres en lengte
	// veranderen niet), alleen de producer schuift op.
	n.r.rxStdIdx = (n.r.rxStdIdx + 1) % rxStdRing
	n.wr(mbRxStdProd, n.r.rxStdIdx)

	if flags&rxdFlagError != 0 || errVLAN&rxdErrMask != 0 || length < 14 || length > len(buf) {
		return 0, nil // stuk frame of te groot voor de aanroeper: laten vallen
	}
	dev.CopyOut(buf[:length], src)
	return length, nil
}

// Transmit stuurt één frame. Blokkeert kort als de ring vol is.
func (n *Net) Transmit(buf []byte) error {
	if len(buf) == 0 || len(buf) > rxBufSize {
		return fmt.Errorf("tg3: frame of %d bytes does not fit", len(buf))
	}
	next := (n.r.txProd + 1) % txRing
	deadline := time.Now().Add(100 * time.Millisecond)
	for next == n.txConsumer() {
		if time.Now().After(deadline) {
			return fmt.Errorf("tg3: transmit ring full")
		}
	}

	dst := n.r.dma + offTxBuf + uintptr(n.r.txProd)*rxBufSize
	dev.Copy(dst, buf)
	d := n.r.dma + offTxBD + uintptr(n.r.txProd)*txDescSize
	dev.Write32(d+0, uint32(uint64(dst)>>32))
	dev.Write32(d+4, uint32(dst))
	dev.Write32(d+8, uint32(len(buf))<<16|txdFlagEnd)
	dev.Write32(d+12, 0)
	dev.MB()

	n.r.txProd = next
	n.wr(mbTxProd, n.r.txProd)
	return nil
}

// BufRegion geeft de DMA-regio die deze driver gebruikt (voor diagnose).
func (n *Net) BufRegion() (base uintptr, size uintptr) { return n.r.dma, NeedBytes }
