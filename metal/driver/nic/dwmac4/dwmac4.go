// Package dwmac4 is HopOS' driver voor de Synopsys DesignWare MAC van de
// 4.x/5.x-generatie (DWMAC4/EQOS) — op de Radxa Zero 3E het GMAC1-blok van de
// RK3566, dat zich als VERSION 0x3051 meldt (snpsver 5.10; gemeten 05-08, en de
// DTS noemt hem "snps,dwmac-4.20a").
//
// Waarom een NIEUW pakket naast metal/driver/nic/dwmac: dat is de
// DWMAC1000-generatie (3.x), en die deelt met deze alleen de naam en de
// leverancier. Hier zit de MDIO op 0x200/0x204 in plaats van 0x10/0x14, het
// MAC-adres op 0x300, de DMA per kanaal op 0x1100+n*0x80 met een MTL-laag
// ertussen, en de descriptors hebben vier woorden met een compleet ander
// bitformaat — inclusief een tail-pointer in plaats van een poll-demand-register.
// Eén pakket met twee generaties zou élke constante een vertakking geven; twee
// pakketten met dezelfde vorm (mdio.MDIO, go-net NetworkDevice) niet.
//
// REFERENTIE (opgehaald 05-08, narekend — niet uit het hoofd): Linux v6.13
// drivers/net/ethernet/stmicro/stmmac — dwmac4.h en dwmac4_dma.h (registers),
// dwmac4_descs.h (descriptorbits), dwmac4_core.c (MDIO-velden, GMAC_CORE_INIT,
// de snelheidsbits), dwmac4_dma.c en dwmac4_lib.c (de init-volgorde, MTL,
// ringlengte en tail-pointer) en stmmac_main.c voor de ringsemantiek. Bij elke
// constante staat waar hij vandaan komt.
//
// De SoC-glue zit NIET hier maar in board/rk3566: pinmux, GRF (RGMII-mode +
// delays), klokgates, de snelheidsdeler en de PHY-reset zijn board-kennis. Dit
// pakket is de IP-core, zoals gem, genet en dwmac dat ook zijn; de
// clause-22-PHY-logica komt uit driver/nic/mdio, gedeeld met die drie.
//
// CACHE-COHERENTIE: net als bij gem en genet ligt de DMA-regio buiten élke
// RAM-declaratie (Plan.NetDMAPA) en is daarmee device-gemapt en dus ongecachet.
// Daarom staan de descriptors hier gewoon 16 bytes uit elkaar en is er geen
// cache-onderhoud — het cacheline-probleem dat op de C906 DSL=12 afdwingt
// (zie driver/nic/dwmac) bestaat op deze ARM-boards niet.
//
// Alleen voor GOOS=tamago (MMIO via metal/dev).
package dwmac4

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/mdio"
)

// MAC-registers vanaf de basis (dwmac4.h).
const (
	regMACConfig    = 0x0000 // GMAC_CONFIG
	regPacketFilter = 0x0008 // GMAC_PACKET_FILTER
	regRxQCtrl0     = 0x00A0 // GMAC_RXQ_CTRL0: welke RX-queue's aan staan
	regVersion      = 0x0110 // GMAC_VERSION: snpsver [7:0], userver [15:8]
	regHWFeature0   = 0x011C // GMAC_HW_FEATURE0
	regHWFeature1   = 0x0120 // GMAC_HW_FEATURE1: fifo-maten
	regMDIOAddr     = 0x0200 // GMAC_MDIO_ADDR
	regMDIOData     = 0x0204 // GMAC_MDIO_DATA
	regAddr0Hi      = 0x0300 // GMAC_ADDR_HIGH(0)
	regAddr0Lo      = 0x0304 // GMAC_ADDR_LOW(0)
)

// GMAC_CONFIG-bits (dwmac4.h).
const (
	cfgJD   = 1 << 17 // jabber disable
	cfgJE   = 1 << 16 // jumbo enable — bewust UIT, zie coreInit
	cfgPS   = 1 << 15 // port select: MII/RMII (10/100) i.p.v. GMII (1000)
	cfgFES  = 1 << 14 // fast ethernet speed: 100 i.p.v. 10
	cfgDM   = 1 << 13 // duplex mode: full
	cfgDCRS = 1 << 9  // carrier sense negeren tijdens TX (half duplex)
	cfgBE   = 1 << 18 // packet burst enable (half duplex)
	cfgTE   = 1 << 1  // transmitter aan
	cfgRE   = 1 << 0  // receiver aan

	// GMAC_CORE_INIT uit dwmac4.h is JD|PS|BE|DCRS|JE. Wij laten JE (jumbo)
	// eruit en dat is een BEWUSTE afwijking, geen weglating: onze RX-buffers zijn
	// bufSize groot, en met jumbo aan zou de MAC frames tot 9018 bytes
	// accepteren die dan over meerdere descriptors verdeeld binnenkomen. Receive
	// hieronder verwerkt geen gesplitste frames (het eist FD|LD in één
	// descriptor), dus zou zo'n frame een stille verliespost zijn in plaats van
	// een afgekeurd frame. PS zit hier ook niet in — die hoort bij de snelheid
	// (speedBits) en wordt daar gezet.
	coreInit = cfgJD | cfgBE | cfgDCRS

	// De snelheidsbits, en het masker eromheen (dwmac4_core.c: mac->link.*):
	//	10Mbit   = PS
	//	100Mbit  = PS|FES
	//	1000Mbit = 0
	speedMask = cfgPS | cfgFES
)

// GMAC_RXQ_CTRL0: twee bits per queue. DCB-modus voor queue 0 = bit 1
// (GMAC_RX_DCB_QUEUE_ENABLE(0)).
const rxQ0DCBEnable = 1 << 1

// MTL-laag (dwmac4.h). Eén queue, dus alleen kanaal 0.
const (
	regMTLTxOpMode = 0x0D00 // MTL_CHAN_TX_OP_MODE(0)
	regMTLRxOpMode = 0x0D30 // MTL_CHAN_RX_OP_MODE(0)
	regMTLTxDebug  = 0x0D08 // MTL_CHAN_TX_DEBUG(0) — alleen voor Diag
	regMTLRxDebug  = 0x0D38 // MTL_CHAN_RX_DEBUG(0)

	mtlTSF      = 1 << 1 // TX store-and-forward
	mtlTxQEn    = 1 << 3 // TXQEN (niet-AVB)
	mtlRSF      = 1 << 5 // RX store-and-forward
	mtlTQSShift = 16     // [24:16] TX-queue-maat in eenheden van 256B, minus 1
	mtlRQSShift = 20     // [29:20] idem voor RX
)

// DMA-registers: één globaal blok plus één per kanaal (dwmac4_dma.h).
const (
	regDMABusMode    = 0x1000
	regDMASysBusMode = 0x1004
	regDMAStatus     = 0x1008
	regDMADebug0     = 0x100C

	dmaChanBase = 0x1100 // kanaal 0; +0x80 per kanaal

	chanControl   = 0x00 // DMA_CHAN_CONTROL
	chanTxControl = 0x04
	chanRxControl = 0x08
	chanTxBaseHi  = 0x10
	chanTxBase    = 0x14
	chanRxBaseHi  = 0x18
	chanRxBase    = 0x1C
	chanTxEnd     = 0x20 // tail-pointer TX
	chanRxEnd     = 0x28 // tail-pointer RX
	chanTxRingLen = 0x2C
	chanRxRingLen = 0x30
	chanIntrEna   = 0x34
	chanCurTxDesc = 0x44
	chanCurRxDesc = 0x4C
	chanStatus    = 0x60

	busSoftReset = 1 << 0 // DMA_BUS_MODE_SFT_RESET

	// DMA_SYS_BUS_MODE is óók het AXI-configregister: burstmodus, toegestane
	// burstlengtes en het aantal openstaande lees/schrijfverzoeken.
	//
	// De waarden komen ÉÉN OP ÉÉN uit de DTS van dit silicium
	// (rk356x-base.dtsi, gmac1_stmmac_axi_setup + de gmac1-node):
	//
	//	snps,mixed-burst           → MB
	//	snps,blen = <0 0 0 0 16 8 4> → BLEN16 | BLEN8 | BLEN4
	//	snps,rd_osr_lmt = <8>      → RD_OSR_LMT
	//	snps,wr_osr_lmt = <4>      → WR_OSR_LMT
	//
	// En wat er NIET staat is even belangrijk: geen snps,fixed-burst en geen
	// snps,aal. FB zetten zou hier actief schadelijk zijn, want de driver zegt
	// het zelf — "mixed burst has no effect when fb is set" — dus zou een
	// welgemeende extra bit de instelling die de vendor wél vraagt uitschakelen.
	sysBusMB      = 1 << 14 // mixed burst
	sysBusBLEN16  = 1 << 3
	sysBusBLEN8   = 1 << 2
	sysBusBLEN4   = 1 << 1
	sysBusRDOSR   = 8 << 16 // [19:16]
	sysBusWROSR   = 4 << 24 // [27:24]
	sysBusOSRMask = 0xF<<16 | 0xF<<24

	sysBusMode = sysBusMB | sysBusBLEN16 | sysBusBLEN8 | sysBusBLEN4 |
		sysBusRDOSR | sysBusWROSR

	chanPBLx8   = 1 << 16 // DMA_BUS_MODE_PBL: PBL maal 8
	chanOSP     = 1 << 4  // operate on second packet
	chanStart   = 1 << 0  // ST (TX) / SR (RX) — zelfde bit, ander register
	txPBLShift  = 16      // DMA_BUS_MODE_PBL_SHIFT
	rxPBLShift  = 16      // DMA_BUS_MODE_RPBL_SHIFT
	rxRBSZShift = 1       // DMA_RBSZ_SHIFT
	rxRBSZMask  = 0x7FFE  // DMA_RBSZ_MASK: veld [14:1], dus de maat maal twee
	pbl         = 8       // burstlengte; met PBLx8 effectief 64 beats
)

// MDIO-veldindeling (dwmac4_core.c dwmac4_setup + stmmac_mdio.c).
const (
	mdioBusy      = 1 << 0
	mdioWrite     = 1 << 2 // MII_GMAC4_WRITE
	mdioRead      = 3 << 2 // MII_GMAC4_READ
	mdioAddrShift = 21     // [25:21]
	mdioRegShift  = 16     // [20:16]
	mdioCSRShift  = 8      // [11:8]
	mdioDataMask  = 0xFFFF

	// CSR-klokrange voor de MDC-deler (include/linux/stmmac.h). De
	// "stmmaceth"-klok van dit board is SCLK_GMAC1 en die staat op 125MHz
	// (clk-rk3568.c: CLK_MAC1_2TOP ← cpll_125m), dus valt hij in 100-150MHz.
	CSR100_150M = 0x1
)

// Descriptorbits (dwmac4_descs.h). Vier woorden van 32 bits.
const (
	descSize = 16 // vier woorden; ook de stap tussen twee descriptors

	// TX: TDES0/1 = bufferadres lo/hi, TDES2 = bufferlengte, TDES3 = pakket.
	txBufLenMask = 0x3FFF  // TDES2_BUFFER1_SIZE_MASK [13:0]
	txPktLenMask = 0x7FFF  // TDES3_PACKET_SIZE_MASK [14:0]
	txLast       = 1 << 28 // TDES3_LAST_DESCRIPTOR
	txFirst      = 1 << 29 // TDES3_FIRST_DESCRIPTOR
	txOwn        = 1 << 31 // TDES3_OWN

	// RX: RDES0/1 = bufferadres lo/hi, RDES3 = eigendom + status/lengte.
	rxPktLenMask = 0x7FFF  // RDES3_PACKET_SIZE_MASK [14:0]
	rxErrSummary = 1 << 15 // RDES3_ERROR_SUMMARY
	rxLast       = 1 << 28 // RDES3_LAST_DESCRIPTOR
	rxFirst      = 1 << 29 // RDES3_FIRST_DESCRIPTOR
	rxBuf1Valid  = 1 << 24 // RDES3_BUFFER1_VALID_ADDR (bij teruggeven)
	rxOwn        = 1 << 31 // RDES3_OWN

	fcsLen = 4 // de MAC meldt de lengte MÉT CRC, zie rxLen
)

// Ringmaten. Zelfde afweging als bij dwmac op de LicheeRV, maar hier op
// gigabit: 64 × 1536B is ~98KB, bij 1Gbit ongeveer 0,8ms buffering. Dat is
// krap tegen een logregel over de UART, maar de ring is niet de enige rem —
// hopnet leest in een lus zonder te printen, en de MAC laat bij een volle ring
// pauseframes weg in plaats van stil te verliezen (RBU in Diag maakt het
// zichtbaar).
const (
	numRx   = 64
	numTx   = 32
	bufSize = 1536 // vier-voud, ruim boven de 1518 van een MTU-1500-frame

	// maxFrame is wat wij versturen; de TX-lengtevelden zijn hier ruim (14/15
	// bits), dus dit is een MTU-grens en geen veldgrens zoals RBS1 op de
	// DWMAC1000.
	maxFrame = 1518

	descBytes = (numRx + numTx) * descSize
	bufBytes  = (numRx + numTx) * bufSize

	// NeedBytes is de DMA-regio die deze driver nodig heeft. Boards reserveren
	// dit in hun plan (Plan.NetDMAPA / NetDMASize).
	NeedBytes = descBytes + bufBytes
)

// Net is één DWMAC4-instantie. Base, CSR en MAC zet het board vóór de eerste
// aanroep; CSR 0 betekent de default-klokrange.
type Net struct {
	Base uintptr // MAC-registerblok (0xFE010000 op de RK3566)
	CSR  uint32  // klokrange-code voor de MDC-deler (0 = CSR100_150M)
	MAC  [6]byte

	rxDesc, txDesc uintptr
	rxBuf, txBuf   uintptr
	rxCur, txCur   int

	rxFrames  uint64 // diagnose: zie Diag
	rxErrors  uint64
	rxLastErr uint32 // rauwe RDES3 van het laatst afgekeurde frame
	txFrames  uint64
}

// Conformiteit compile-time: de gedeelde PHY-logica praat via deze twee
// methodes, net als bij gem, genet en dwmac.
var _ mdio.MDIO = (*Net)(nil)

func (n *Net) rd(off uintptr) uint32    { return dev.Read32(n.Base + off) }
func (n *Net) wr(off uintptr, v uint32) { dev.Write32(n.Base+off, v) }

func (n *Net) chrd(off uintptr) uint32    { return n.rd(dmaChanBase + off) }
func (n *Net) chwr(off uintptr, v uint32) { n.wr(dmaChanBase+off, v) }

// Version geeft de rauwe VERSION-register-inhoud: snpsver in [7:0]. Het eerste
// dat een bring-up hoort te lezen — een blok waarvan de APB-klok dicht staat
// antwoordt hier niet (of houdt de bus vast).
func (n *Net) Version() uint32 { return n.rd(regVersion) }

// FIFOSizes leest de fifo-maten uit de hardware zelf (HW_FEATURE1: TXFIFOSIZE
// [10:6], RXFIFOSIZE [4:0], elk als 128<<n). Uit de hardware en niet uit een
// constante, want de MTL-queue-maat (TQS/RQS) moet erop kloppen en die verschilt
// per silicium — een verkeerde TQS is een MAC die frames in de FIFO laat staan.
func (n *Net) FIFOSizes() (tx, rx int) {
	f := n.rd(regHWFeature1)
	return 128 << (f >> 6 & 0x1F), 128 << (f & 0x1F)
}

// MDIOState geeft het rauwe MDIO_ADDR-register, voor het meetinstrument: blijft
// de BUSY-bit staan, dan wacht de machine op een klok of op een bus die niemand
// terugtrekt — en dat is een ander probleem dan "er zit geen PHY".
func (n *Net) MDIOState() uint32 { return n.rd(regMDIOAddr) }

// HWFeatures geeft de twee feature-registers rauw terug, voor het
// meetinstrument.
func (n *Net) HWFeatures() (f0, f1 uint32) {
	return n.rd(regHWFeature0), n.rd(regHWFeature1)
}

func (n *Net) csr() uint32 {
	if n.CSR == 0 {
		return CSR100_150M
	}
	return n.CSR
}

// --- MDIO ----------------------------------------------------------------

// mdioWait wacht (begrensd) tot de MDIO-machine vrij is; false bij een stall.
// Begrensd en niet eeuwig: een PHY die niet reageert mag de boot niet ophouden —
// dezelfde regel als in de gem-, genet- en dwmac-drivers.
func (n *Net) mdioWait() bool {
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n.rd(regMDIOAddr)&mdioBusy == 0 {
			return true
		}
	}
	return false
}

// MDIORead leest een clause-22 PHY-register. 0xFFFF is de "geen PHY / mislukte
// read"-sentinel die mdio.Scan en mdio.AutoNeg al filteren.
func (n *Net) MDIORead(phy, reg int) uint16 {
	if !n.mdioWait() {
		return 0xFFFF
	}
	n.wr(regMDIOAddr, uint32(phy&0x1F)<<mdioAddrShift|
		uint32(reg&0x1F)<<mdioRegShift|
		n.csr()<<mdioCSRShift|mdioRead|mdioBusy)
	if !n.mdioWait() {
		return 0xFFFF
	}
	return uint16(n.rd(regMDIOData) & mdioDataMask)
}

// MDIOWrite schrijft een clause-22 PHY-register.
func (n *Net) MDIOWrite(phy, reg int, val uint16) {
	if !n.mdioWait() {
		return
	}
	n.wr(regMDIOData, uint32(val))
	n.wr(regMDIOAddr, uint32(phy&0x1F)<<mdioAddrShift|
		uint32(reg&0x1F)<<mdioRegShift|
		n.csr()<<mdioCSRShift|mdioWrite|mdioBusy)
	n.mdioWait()
}

// PHYScan zoekt PHY's op de MDIO-bus en geeft (adres, id1, id2) van de eerste
// hit. Gedeeld met de andere NIC's via metal/driver/nic/mdio.
func (n *Net) PHYScan() (addr int, id1, id2 uint16, found bool) {
	return mdio.Scan(n)
}

// AutoNeg start autonegotiatie en wacht op een link; geeft (snelheid in Mbps,
// full-duplex). Ook gedeeld via metal/driver/nic/mdio.
func (n *Net) AutoNeg(phy int, timeout time.Duration) (speed int, fd bool, err error) {
	return mdio.AutoNeg(n, phy, timeout)
}

// --- bring-up -------------------------------------------------------------

// Reset doet de DMA-softreset. Apart van Init omdat het meetinstrument hem
// vóór een MDIO-scan wil kunnen doen zonder ringen te bouwen: de reset zet ook
// de MDIO-machine schoon, en een blok dat hier hangt vertelt iets over de
// klokken en niets over de descriptors.
func (n *Net) Reset() error {
	n.wr(regDMABusMode, n.rd(regDMABusMode)|busSoftReset)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n.rd(regDMABusMode)&busSoftReset == 0 {
			return nil
		}
	}
	return fmt.Errorf("dwmac4: DMA soft reset does not clear (bus mode %#08x)", n.rd(regDMABusMode))
}

// Init legt de ringen in de DMA-regio en zet MAC, MTL en DMA aan. Aanroepen ná
// de board-glue (pinmux, GRF, klokken, PHY-reset) en ná de autonegotiatie —
// speed en fd komen daaruit, en het board zet de RGMII-klokdeler op dezelfde
// snelheid.
func (n *Net) Init(dmaPA, dmaSize uintptr, speed int, fd bool) error {
	if dmaSize < NeedBytes {
		return fmt.Errorf("dwmac4: DMA region %d KB, need at least %d KB",
			dmaSize>>10, uintptr(NeedBytes)>>10)
	}
	if err := n.Reset(); err != nil {
		return err
	}

	// 1. De AXI-kant, met de waarden uit de DTS van dit silicium (zie
	//    sysBusMode). De OSR-velden eerst wissen: die staan na reset niet
	//    noodzakelijk op nul, en ze er blind bij OR-en geeft een groter getal dan
	//    de vendor toestaat.
	n.wr(regDMASysBusMode, n.rd(regDMASysBusMode)&^sysBusOSRMask|sysBusMode)

	// 2. Ringen bouwen vóórdat de DMA er iets van weet.
	n.initRings(dmaPA)

	// 3. Kanaal 0: PBL, geen interrupts (wij pollen), ring-adressen en -lengtes.
	//    De ringlengte is het AANTAL MIN ÉÉN (stmmac_main.c geeft dma_*_size-1).
	n.chwr(chanControl, n.chrd(chanControl)|chanPBLx8)
	n.chwr(chanIntrEna, 0)

	n.chwr(chanTxControl, n.chrd(chanTxControl)|pbl<<txPBLShift|chanOSP)
	n.chwr(chanTxBaseHi, uint32(uint64(n.txDesc)>>32))
	n.chwr(chanTxBase, uint32(n.txDesc))
	n.chwr(chanTxRingLen, numTx-1)

	n.chwr(chanRxControl, rxControl(n.chrd(chanRxControl), bufSize))
	n.chwr(chanRxBaseHi, uint32(uint64(n.rxDesc)>>32))
	n.chwr(chanRxBase, uint32(n.rxDesc))
	n.chwr(chanRxRingLen, numRx-1)

	// Tail-pointers. Er is geen poll-demand-register in deze generatie: de DMA
	// werkt tot aan de tail en stopt. RX staat vol, dus de tail is één descriptor
	// voorbij het einde; TX is leeg, dus de tail is de basis zelf.
	n.chwr(chanRxEnd, uint32(n.rxDesc+numRx*descSize))
	n.chwr(chanTxEnd, uint32(n.txDesc))

	// 4. MTL: store-and-forward beide kanten, en de queue-maten uit de hardware
	//    (TQS/RQS = fifo/256 - 1, dwmac4_dma.c). Met één queue krijgt die de
	//    hele FIFO.
	txFIFO, rxFIFO := n.FIFOSizes()
	if txFIFO < 256 || rxFIFO < 256 {
		// Kan alleen als HW_FEATURE1 nul leest, en dan praten we tegen een blok
		// dat niet geklokt is. Zonder deze controle wordt fifo/256-1 negatief,
		// loopt hij als uint32 om en zetten we TQS/RQS op onzin — een MAC die
		// frames in zijn FIFO laat staan, wat als "geen netwerk" oogt.
		return fmt.Errorf("dwmac4: implausible FIFO sizes tx=%dB rx=%dB (hw-feature1 %#08x) — is the block clocked?",
			txFIFO, rxFIFO, n.rd(regHWFeature1))
	}
	n.wr(regMTLTxOpMode, mtlTSF|mtlTxQEn|uint32(txFIFO/256-1)<<mtlTQSShift)
	n.wr(regMTLRxOpMode, mtlRSF|uint32(rxFIFO/256-1)<<mtlRQSShift)

	// 5. MAC-adres in de perfect-filter (Addr0), vóór RX aan gaat — anders
	//    draait de MAC even met een 00:00:00:00:00:00-filter.
	n.wr(regAddr0Hi, uint32(n.MAC[5])<<8|uint32(n.MAC[4]))
	n.wr(regAddr0Lo, uint32(n.MAC[3])<<24|uint32(n.MAC[2])<<16|
		uint32(n.MAC[1])<<8|uint32(n.MAC[0]))
	// Filter op nul = perfect match + broadcast. Geen promiscuous: wat wij niet
	// moeten hebben hoort de ring niet te vullen.
	n.wr(regPacketFilter, 0)
	// RX-queue 0 aan in DCB-modus; zonder dit komt er geen frame uit de MTL.
	n.wr(regRxQCtrl0, rxQ0DCBEnable)

	// 6. MAC-config: de core-init-bits plus de snelheid en het duplex.
	n.wr(regMACConfig, macConfig(n.rd(regMACConfig), speed, fd))

	// 7. En dan pas lopen: eerst de DMA-kanalen, dan de MAC-kant.
	n.wr(regDMAStatus, n.rd(regDMAStatus)) // sticky bits van vóór de reset wissen
	n.chwr(chanStatus, n.chrd(chanStatus))
	n.chwr(chanTxControl, n.chrd(chanTxControl)|chanStart)
	n.chwr(chanRxControl, n.chrd(chanRxControl)|chanStart)
	n.wr(regMACConfig, n.rd(regMACConfig)|cfgTE|cfgRE)
	dev.MB()
	return nil
}

// giveRx zet één RX-descriptor in de leesvorm: bufferadres, geen tweede buffer,
// en OWN|BUF1V zodat de DMA hem mag vullen. Eén plek, zodat initRings en Receive
// niet uit elkaar kunnen lopen.
func (n *Net) giveRx(d uintptr, i int) {
	b := uint64(n.rxBuf + uintptr(i)*bufSize)
	dev.Write32(d+0, uint32(b))
	dev.Write32(d+4, uint32(b>>32))
	dev.Write32(d+8, 0) // geen tweede buffer
	dev.MB()
	dev.Write32(d+12, rxOwn|rxBuf1Valid) // OWN als laatste
}

// initRings legt beide ringen en hun buffers in de DMA-regio. Geen chaining en
// geen ring-end-bit zoals bij de DWMAC1000: deze generatie kent de ringlengte
// uit een register en wrapt zelf.
func (n *Net) initRings(dmaPA uintptr) {
	n.rxDesc = dmaPA
	n.txDesc = n.rxDesc + numRx*descSize
	n.rxBuf = dmaPA + descBytes
	n.txBuf = n.rxBuf + numRx*bufSize
	n.rxCur, n.txCur = 0, 0

	for i := 0; i < numRx; i++ {
		n.giveRx(n.rxDesc+uintptr(i)*descSize, i) // meteen van de DMA
	}
	for i := 0; i < numTx; i++ {
		d := n.txDesc + uintptr(i)*descSize
		dev.Write32(d+0, 0)
		dev.Write32(d+4, 0)
		dev.Write32(d+8, 0)
		dev.Write32(d+12, 0) // van ons
	}
	dev.MB()
}

// --- de rekenstukken, apart zodat ze op de host bewijsbaar zijn ------------
//
// Deze vier functies zijn de bit-arithmetiek van de driver, en dat is precies
// waar de vorige generatie op ijzer op struikelde (dwmac, 30-07: een
// buffergrootte die door een te smal veld naar nul werd geveegd, en een MAC die
// vervolgens "elke buffer is 0 bytes" te horen kreeg). Op dit bordje kost een
// ronde een kaartwissel, dus horen ze in een host-test en niet in een boot.

// macConfig bouwt het GMAC_CONFIG-woord: de bestaande inhoud met de
// snelheids-, duplex- en jumbobits gewist, dan de core-init-bits en de snelheid
// erin. Idempotent, zodat een tweede Init dezelfde stand oplevert.
func macConfig(cur uint32, speed int, fd bool) uint32 {
	cfg := cur&^(speedMask|cfgDM|cfgJE) | coreInit
	switch speed {
	case 10:
		cfg |= cfgPS
	case 100:
		cfg |= cfgPS | cfgFES
	default: // 1000: PS en FES beide uit — GMII in plaats van MII
	}
	if fd {
		cfg |= cfgDM
	}
	return cfg
}

// rxControl bouwt het RX_CONTROL-woord: de bestaande inhoud met het RBSZ-veld
// gewist, dan de burstlengte en de buffergrootte erin. RBSZ is [14:1] — de maat
// staat er dus maal twee in, en een buffergrootte die niet in dat veld past zou
// er stil afgekapt in landen. Vandaar de panic: liever een build die valt bij
// het verdiepen van de ringen dan een MAC die halve frames aflevert.
func rxControl(cur uint32, size int) uint32 {
	if size <= 0 || size&1 != 0 || size<<rxRBSZShift&^rxRBSZMask != 0 {
		panic("dwmac4: bufSize past niet in RBSZ [14:1]")
	}
	return cur&^rxRBSZMask | pbl<<rxPBLShift | uint32(size)<<rxRBSZShift
}

// txDesc2en3 geeft de twee TX-descriptorwoorden voor één frame: TDES2 is de
// bufferlengte (veld [13:0]) en TDES3 het pakket — eerste én laatste descriptor
// (wij versturen nooit gefragmenteerd), de pakketlengte in [14:0] en OWN.
func txDesc2en3(length int) (des2, des3 uint32) {
	if length <= 0 || length > maxFrame {
		panic("dwmac4: framelengte buiten de descriptorvelden")
	}
	return uint32(length) & txBufLenMask,
		txOwn | txFirst | txLast | uint32(length)&txPktLenMask
}

// --- polled RX/TX ---------------------------------------------------------

// rxLen haalt de framelengte uit RDES3. De MAC meldt de lengte MÉT de CRC
// erbij, want wij zetten ACS (bit 20 van GMAC_CONFIG: automatic pad/CRC strip)
// niet — precies zoals
// stmmac, dat de vier bytes voor deze generatie ook zelf aftrekt. Vergeet je dat,
// dan komt elk frame vier bytes te lang de stack in en faalt élke checksum.
func rxLen(rdes3 uint32) int {
	l := int(rdes3&rxPktLenMask) - fcsLen
	if l < 0 {
		return 0
	}
	return l
}

// Receive haalt één frame op; 0 = niets beschikbaar (non-blocking, zoals de
// andere drivers — hopnet's rxLoop is de enige aanroeper).
func (n *Net) Receive(buf []byte) (int, error) {
	d := n.rxDesc + uintptr(n.rxCur)*descSize
	dev.MB()
	sts := dev.Read32(d + 12)
	if sts&rxOwn != 0 {
		return 0, nil // nog van de DMA
	}

	length := 0
	switch {
	case sts&rxErrSummary != 0, sts&(rxFirst|rxLast) != rxFirst|rxLast:
		// Foutframe, of een frame over meerdere descriptors. Dat laatste kan
		// alleen als er een frame groter dan bufSize binnenkwam, en dat hebben we
		// met jumbo-uit uitgesloten — maar het rauwe statuswoord bewaren we toch,
		// want zonder dat is "afgekeurd" op ijzer niet te ontleden (zie Diag).
		n.rxErrors++
		n.rxLastErr = sts
	default:
		length = rxLen(sts)
		if length > len(buf) {
			length = len(buf)
		}
		dev.CopyOut(buf[:length], n.rxBuf+uintptr(n.rxCur)*bufSize)
		n.rxFrames++
	}

	// Descriptor teruggeven — en dat is HIER meer werk dan bij de vorige
	// generatie: de DWMAC4-descriptor heeft een leesvorm en een schrijfvorm, en de
	// DMA schrijft bij afronding ALLE VIER de woorden met status vol. Het
	// bufferadres in RDES0/RDES1 is dus weg en moet er opnieuw in, anders leest de
	// DMA na één ronde door de ring in een adres dat zojuist statusbits was.
	// (Bij de DWMAC1000 staat het bufferadres in RDES2 en blijft het staan —
	// vandaar dat driver/nic/dwmac hier niets van doet.)
	n.giveRx(d, n.rxCur)
	dev.MB()
	n.rxCur = (n.rxCur + 1) % numRx
	n.chwr(chanRxEnd, uint32(n.rxDesc+uintptr(n.rxCur)*descSize))

	return length, nil
}

// Transmit verstuurt één frame en wacht tot de descriptor vrij is (blocking,
// zoals gem/genet/dwmac: de stack serialiseert TX).
func (n *Net) Transmit(buf []byte) error {
	if len(buf) == 0 || len(buf) > maxFrame {
		return fmt.Errorf("dwmac4: frame of %d bytes does not fit (max %d)", len(buf), maxFrame)
	}

	d := n.txDesc + uintptr(n.txCur)*descSize
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		dev.MB()
		if dev.Read32(d+12)&txOwn == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dwmac4: TX ring does not drain (chan status %#08x)", n.chrd(chanStatus))
		}
	}

	des2, des3 := txDesc2en3(len(buf))
	b := uint64(n.txBuf + uintptr(n.txCur)*bufSize)
	dev.Copy(uintptr(b), buf)
	// Zelfde reden als bij giveRx: de afgeronde descriptor draagt status in alle
	// vier de woorden, dus het bufferadres moet er per frame opnieuw in. Dit gaat
	// pas fout ná één ronde door de ring — precies het soort bug dat een korte
	// test overleeft en een download niet.
	dev.Write32(d+0, uint32(b))
	dev.Write32(d+4, uint32(b>>32))
	dev.Write32(d+8, des2)
	dev.MB()
	// OWN als laatste: pas dan mag de DMA hem zien, en de rest van de descriptor
	// staat er dan al.
	dev.Write32(d+12, des3)
	dev.MB()

	n.txFrames++
	n.txCur = (n.txCur + 1) % numTx
	n.chwr(chanTxEnd, uint32(n.txDesc+uintptr(n.txCur)*descSize))
	return nil
}

// Diag is het meetinstrument voor een mislukte bring-up: één regel die zegt of
// de DMA liep, waar beide ringen staan en wat de MAC ervan vond. Het board hangt
// hem aan de fout als DHCP niets oplevert — dan is één boot genoeg om te weten
// of TX de deur uit ging, of RX niets binnenkreeg, of geen van beide.
func (n *Net) Diag() string {
	rx := n.rxDesc + uintptr(n.rxCur)*descSize
	tx := n.txDesc + uintptr(n.txCur)*descSize
	dev.MB()
	return fmt.Sprintf("chan-status %#08x dma-status %#08x dbg %#08x mtl tx %#08x rx %#08x "+
		"mac-cfg %#08x rx=%d/%d(err) last-err %#08x tx=%d "+
		"rxdesc[%d] %#08x hw-rx %#08x txdesc[%d] %#08x hw-tx %#08x",
		n.chrd(chanStatus), n.rd(regDMAStatus), n.rd(regDMADebug0),
		n.rd(regMTLTxDebug), n.rd(regMTLRxDebug), n.rd(regMACConfig),
		n.rxFrames, n.rxErrors, n.rxLastErr, n.txFrames,
		n.rxCur, dev.Read32(rx+12), n.chrd(chanCurRxDesc),
		n.txCur, dev.Read32(tx+12), n.chrd(chanCurTxDesc))
}
