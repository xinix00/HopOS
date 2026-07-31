// Package dwmac is HopOS' driver voor de Synopsys DesignWare GMAC
// (DWMAC1000) — de derde MAC in de boom naast gem (Cadence GEM, Pi 5) en
// genet (Broadcom GENET, Pi 4), en de eerste op RISC-V: op de Sophgo SG2002
// (LicheeRV Nano) hangt de 100M-poort via RMII aan de interne ePHY.
//
// Geschreven naar de vendor U-Boot-driver (designware.c, bindt
// "cvitek,ethernet") en de Linux stmmac-glue (dwmac-cvitek.c); polled, één
// RX- en één TX-ring, en net als de andere twee drivers een go-net
// NetworkDevice (Receive/Transmit op rauwe frames).
//
// De SoC-glue zit NIET hier maar in board/licheerv/hop: klokgates, ePHY-
// power-on en de analoge kalibratie zijn board-kennis. Dit pakket is de
// IP-core — zoals gem en genet dat ook zijn — en de clause-22-PHY-logica
// (scan, autonegotiatie) komt uit driver/nic/mdio, gedeeld met die twee.
//
// Read-only bevestigd op ijzer (probe 30-07, vóór de RJ45 gesoldeerd was):
// klokgates open, versie-register 0x1037 op basis 0x04070000, en de interne
// ePHY antwoordt op MDIO-adres 0 met id 0043:5649 — precies het id dat de
// ePHY-init zelf in de PHY schrijft, dus die keten liep. Wat dáármee nog niet
// bewezen was, is alles onder deze regel: DMA.
//
// CACHE-COHERENTIE — het echte verschil met de ARM-boards. Daar ligt de
// DMA-regio buiten élke RAM-declaratie én ongecachet; op de C906 in M-mode is
// er geen tweede laag die dat kan afdwingen (geen MMU; geheugenattributen
// komen uit de sysmap van de core), dus de regio is gewoon cachebaar DRAM en
// élke overdracht loopt door dev.CleanInv + dev.MB. En daarom staan de
// descriptors 64 bytes uit elkaar (DSL=12 in DMA_BUS_MODE, zie busDSL64): één
// descriptor per cacheline. Aaneengesloten 16B-descriptors zoals de vendor ze
// gebruikt zetten er vier in één line, en dan overschrijft onze write-back van
// descriptor i de updates die de DMA net in i+1..i+3 deed. Gemeten 30-07: DHCP
// lukte nog (twee frames, ver uit elkaar), maar ping verloor de helft van de
// pakketten en een TLS-handshake liep nooit af. U-Boot komt met DSL=0 weg omdat
// het één frame per keer doet; een netstack niet.
package dwmac

import (
	"fmt"
	"time"

	"hop-os/metal/dev"
	"hop-os/metal/driver/nic/mdio"
)

// DWMAC1000-registeroffsets (designware.h-conventie).
const (
	// MAC
	regConf     = 0x0000
	regFilter   = 0x0004
	regGMIIAddr = 0x0010
	regGMIIData = 0x0014
	regVersion  = 0x0020
	regAddr0Hi  = 0x0040
	regAddr0Lo  = 0x0044

	// MAC_CONFIG-bits
	confPortMII  = 1 << 15 // PS: MII/RMII (10/100) i.p.v. GMII
	confFES100   = 1 << 14 // FES: 100Mbit
	confDisRxOwn = 1 << 13 // DO: eigen frames niet terugontvangen (half duplex)
	confDuplex   = 1 << 11 // DM: full duplex
	confTxEn     = 1 << 3
	confRxEn     = 1 << 2

	// DMA (basis + 0x1000)
	dmaBusMode   = 0x1000
	dmaTxPoll    = 0x1004
	dmaRxPoll    = 0x1008
	dmaRxList    = 0x100c
	dmaTxList    = 0x1010
	dmaStatus    = 0x1014
	dmaOpMode    = 0x1018
	dmaHWFeature = 0x1058
	dmaCurTxDesc = 0x1048
	dmaCurRxDesc = 0x104c

	// DMA_BUS_MODE-bits
	busSWReset    = 1 << 0
	busPBL8       = 8 << 8
	busFixedBurst = 1 << 16

	// DSL (Descriptor Skip Length, bits [6:2]): hoeveel 32-bit words de DMA
	// tussen twee descriptors overslaat. Wij zetten 12 → 48 bytes skip → met een
	// 16B-descriptor precies 64 bytes stride, dus ÉÉN DESCRIPTOR PER CACHELINE.
	//
	// Dat is geen optimalisatie maar een correctheidseis op dit board. Zonder DSL
	// staan vier descriptors in één 64B-line: als wij er één beschrijven (OWN
	// teruggeven) is die hele line dirty, en de write-back overschrijft dan de
	// updates die de DMA net in de BUURdescriptors zette. Gemeten 30-07: DHCP
	// (twee frames) lukte, maar ping verloor de helft en een TLS-handshake liep
	// nooit af. De vendor-U-Boot komt hier weg met DSL=0 omdat die één frame per
	// keer doet en tussendoor niets anders; een netstack doet dat niet.
	busDSL64 = 12 << 2

	// DMA_OP_MODE-bits
	opStoreForward = 1 << 21 // TX pas versturen als het frame compleet in de FIFO staat
	opFlushTxFIFO  = 1 << 20
	opTxStart      = 1 << 13
	opRxStart      = 1 << 1

	// MDIO (GMII_ADDR)
	miiBusy      = 1 << 0
	miiWrite     = 1 << 1
	miiRegShift  = 6
	miiAddrShift = 11
	// eth_csrclk staat op dit board vast op 250MHz (dts) → CSR-klokrange
	// 250-300MHz = divisor 0b0101, en die zit in bits [4:2].
	miiClkRange = 0x14
)

// Descriptorvelden (normal format, 16B — géén ALTDESCRIPTOR: bit 7 van
// DMA_BUS_MODE blijft 0, net als bij de vendor).
const (
	descStatus = 0 // RDES0/TDES0
	descCntl   = 4 // RDES1/TDES1
	descBuf    = 8 // RDES2/TDES2 (bufferadres)
	descNext   = 12
	descSize   = 16 // de descriptor zelf
	descStride = 64 // afstand tussen twee descriptors: één cacheline (zie busDSL64)

	descOwn = 1 << 31 // OWN: 1 = van de DMA

	rxLenShift = 16
	rxLenMask  = 0x3fff
	rxStsError = 1 << 15 // ES: samengevatte fout
	rxStsFirst = 1 << 9  // FS
	rxStsLast  = 1 << 8  // LS

	txCntlLast    = 1 << 30 // LS
	txCntlFirst   = 1 << 29 // FS
	ringEnd       = 1 << 25 // RER/TER: laatste descriptor van de ring
	cntlSize1Mask = 0x7ff   // RBS1/TBS1 — ELF bits, zie maxFrame

	// Ringdiepte: 64 descriptors, en dat is een tijdsbudget, niet een smaak. Bij
	// 100Mbit is 16 × 1600B = 25KB precies 2 MILLISECONDEN buffering — minder dan
	// één logregel over een 115200-UART kost (~9ms voor 80 tekens). HOP hoeft dus
	// maar één keer te printen terwijl er data binnenkomt en de ring loopt over;
	// daarna zit TCP in een retransmit-put en komt een download van 5MB nooit af.
	// Gemeten 30-07: stagen lukte één keer in ~50s en daarna vijf minuten niets —
	// puur timing. Met 64 descriptors is het budget ~8ms, en past de hele set nog
	// steeds in de 256KB-plan-regio (bufSize omlaag naar 1664).
	numDesc = 64
	bufSize = 1664 // 26 × 64B, net boven maxFrame — 2 × 64 × 1664 = 208KB

	// maxFrame is wat we de MAC als buffergrootte MELDEN, en dat is niet
	// hetzelfde als bufSize. Het RBS1-veld is 11 bits (cntlSize1Mask), dus
	// 2048 past er níet in: dat maskeert naar nul en dan denkt de MAC dat elke
	// buffer nul bytes groot is. Gemeten gevolg (30-07, eerste DMA-boot): de
	// ring gaf 128 descriptors terug zonder één bruikbaar frame — link stond,
	// TX liep, RX kapot. De vendor programmeert hier 1600 (designware.h,
	// MAC_MAX_FRAME_SZ) terwijl zijn buffers óók 2048 zijn; dat is precies dit
	// onderscheid. rxCntl bewaakt het nu met een panic i.p.v. een stil masker.
	maxFrame = 1600

	// descOff/bufOff verdelen de plan-regio: descriptors vooraan (RX-ring,
	// dan TX-ring, elk descStride uit elkaar), buffers vanaf de tweede pagina.
	// NeedBytes is wat de aanroeper minimaal moet reserveren.
	descOff = 0
	// Afgeleid, niet met de hand: de twee ringen nemen samen 2×numDesc×descStride
	// in, en de buffers beginnen daar precies achter. Met een hardgecodeerde
	// 0x1000 lag de TX-ring bovenop de eerste RX-buffers zodra numDesc boven 32
	// kwam — gemeten 30-07 bij het verdiepen naar 64: link stond, RX/TX bewogen,
	// en DHCP kreeg geen lease meer.
	bufOff = 2 * numDesc * descStride

	// NeedBytes is de DMA-regio die deze driver nodig heeft (descriptors +
	// 2×16 frame-buffers). Boards reserveren dit in hun plan (Plan.NetDMAPA).
	NeedBytes = bufOff + 2*numDesc*bufSize
)

// Net is één DWMAC1000. Base en MAC zet het board vóór de eerste aanroep.
type Net struct {
	Base uintptr // 0x04070000 op de SG2002
	MAC  [6]byte // locally administered; het board bepaalt hem

	rxDesc uintptr // fysiek adres van de RX-ring
	txDesc uintptr
	rxBuf  uintptr
	txBuf  uintptr
	rxCur  int
	txCur  int

	rxFrames  uint64 // diagnose: wat de ring ons gaf (zie Diag)
	rxErrors  uint64
	rxLastErr uint32 // rauwe RDES0 van het laatst afgekeurde frame
	txFrames  uint64
}

// rxCntl is het RDES1-woord van een RX-descriptor: de buffergrootte die we de
// MAC melden, plus de ring-end-bit op de laatste. Aparte functie omdat dit het
// woord is waar de eerste DMA-boot op stukliep — nu host-getest, en een maat
// die niet in het veld past is een panic bij het bouwen van de ring in plaats
// van een MAC die stil met nul-byte buffers werkt.
func rxCntl(last bool) uint32 {
	if maxFrame&^cntlSize1Mask != 0 {
		panic("dwmac: maxFrame past niet in RBS1 (11 bits)")
	}
	c := uint32(maxFrame)
	if last {
		c |= ringEnd
	}
	return c
}

// txCntl is het TDES1-woord voor één frame: eerste én laatste descriptor van
// het frame (wij versturen nooit gefragmenteerd), de lengte, en de
// ring-end-bit op de laatste descriptor van de ring.
func txCntl(length int, last bool) uint32 {
	if length <= 0 || length > maxFrame {
		panic("dwmac: framelengte buiten TBS1")
	}
	c := uint32(txCntlFirst|txCntlLast) | uint32(length)
	if last {
		c |= ringEnd
	}
	return c
}

// Conformiteit compile-time: de PHY-logica in driver/nic/mdio praat via deze
// twee methodes, net als bij gem en genet.
var _ mdio.MDIO = (*Net)(nil)

func (n *Net) rd(off uintptr) uint32    { return dev.Read32(n.Base + off) }
func (n *Net) wr(off uintptr, v uint32) { dev.Write32(n.Base+off, v) }

// Version geeft het snps-versieregister (read-only; 0x1037 op dit silicium).
func (n *Net) Version() uint32 { return n.rd(regVersion) }

// HWFeature geeft het DMA-feature-register (read-only, diagnose).
func (n *Net) HWFeature() uint32 { return n.rd(dmaHWFeature) }

// --- MDIO ----------------------------------------------------------------

// mdioWait wacht tot de busy-bit valt. Geen fout maar een bool: de aanroepers
// hierboven geven 0xffff terug, wat voor clause-22 "geen PHY" betekent — en
// dat is precies hoe mdio.Scan een leeg adres herkent.
func (n *Net) mdioWait() bool {
	for t := 0; t < 10000; t++ {
		if n.rd(regGMIIAddr)&miiBusy == 0 {
			return true
		}
		time.Sleep(10 * time.Microsecond)
	}
	return false
}

// MDIORead leest één clause-22-register van een PHY op de bus.
func (n *Net) MDIORead(phy, reg int) uint16 {
	n.wr(regGMIIAddr, uint32(phy)<<miiAddrShift|uint32(reg)<<miiRegShift|miiClkRange|miiBusy)
	if !n.mdioWait() {
		return 0xffff
	}
	return uint16(n.rd(regGMIIData))
}

// MDIOWrite schrijft één clause-22-register.
func (n *Net) MDIOWrite(phy, reg int, val uint16) {
	n.wr(regGMIIData, uint32(val))
	n.wr(regGMIIAddr, uint32(phy)<<miiAddrShift|uint32(reg)<<miiRegShift|miiClkRange|miiWrite|miiBusy)
	n.mdioWait()
}

// --- bring-up -------------------------------------------------------------

// Init reset de DMA, legt de ringen in de plan-regio en zet MAC en DMA aan.
// Aanroepen ná de ePHY-init en de autonegotiatie (speed/fd komen daaruit).
func (n *Net) Init(dmaPA, dmaSize uintptr, speed int, fd bool) error {
	if dmaSize < NeedBytes {
		return fmt.Errorf("dwmac: DMA-regio %d KB, minimaal %d KB nodig", dmaSize>>10, uintptr(NeedBytes)>>10)
	}

	// Soft reset. Die hangt als de klokgates dicht staan — het board opent ze
	// vóór deze aanroep, dus een timeout hier betekent iets anders (en de
	// melding zegt dat, in plaats van te gokken).
	n.wr(dmaBusMode, n.rd(dmaBusMode)|busSWReset)
	for t := 0; ; t++ {
		if n.rd(dmaBusMode)&busSWReset == 0 {
			break
		}
		if t > 100000 {
			return fmt.Errorf("dwmac: DMA soft reset klaart niet (bus mode %#08x)", n.rd(dmaBusMode))
		}
		time.Sleep(10 * time.Microsecond)
	}

	n.initRings(dmaPA)

	// MAC-adres in de perfect-filter (Addr0). Vóór RX aan, anders zou de MAC
	// even met een 00:00:00:00:00:00-filter draaien.
	n.wr(regAddr0Hi, uint32(n.MAC[5])<<8|uint32(n.MAC[4]))
	n.wr(regAddr0Lo, uint32(n.MAC[3])<<24|uint32(n.MAC[2])<<16|uint32(n.MAC[1])<<8|uint32(n.MAC[0]))
	// Filter op nul = perfect match + broadcast (DBF=0). Geen promiscuous:
	// wat wij niet moeten hebben hoort de ring niet te vullen.
	n.wr(regFilter, 0)

	n.wr(dmaBusMode, busFixedBurst|busPBL8|busDSL64)
	n.wr(dmaRxList, uint32(n.rxDesc))
	n.wr(dmaTxList, uint32(n.txDesc))
	n.wr(dmaOpMode, opStoreForward|opFlushTxFIFO)
	n.wr(dmaStatus, n.rd(dmaStatus)) // sticky bits van vóór de reset wissen

	conf := uint32(confPortMII | confDisRxOwn | confTxEn | confRxEn)
	if speed == 100 {
		conf |= confFES100
	}
	if fd {
		conf |= confDuplex
	}
	n.wr(regConf, conf)

	n.wr(dmaOpMode, n.rd(dmaOpMode)|opTxStart|opRxStart)
	return nil
}

// initRings legt beide ringen en hun buffers in de plan-regio. Ring-mode
// (geen chaining): de DMA loopt de descriptors aaneengesloten af en springt
// terug bij de descriptor met de ring-end-bit.
func (n *Net) initRings(dmaPA uintptr) {
	n.rxDesc = dmaPA + descOff
	n.txDesc = n.rxDesc + numDesc*descStride
	n.rxBuf = dmaPA + bufOff
	n.txBuf = n.rxBuf + numDesc*bufSize
	n.rxCur, n.txCur = 0, 0

	for i := 0; i < numDesc; i++ {
		rx := n.rxDesc + uintptr(i)*descStride
		dev.Write32(rx+descStatus, descOwn) // meteen van de DMA
		dev.Write32(rx+descCntl, rxCntl(i == numDesc-1))
		dev.Write32(rx+descBuf, uint32(n.rxBuf+uintptr(i)*bufSize))
		dev.Write32(rx+descNext, 0)

		tx := n.txDesc + uintptr(i)*descStride
		dev.Write32(tx+descStatus, 0) // van ons
		dev.Write32(tx+descCntl, 0)
		dev.Write32(tx+descBuf, uint32(n.txBuf+uintptr(i)*bufSize))
		dev.Write32(tx+descNext, 0)
	}

	dev.MB()
	dev.CleanInv(n.rxDesc, 2*numDesc*descStride)
	dev.CleanInv(n.rxBuf, 2*numDesc*bufSize)
}

// --- polled RX/TX ---------------------------------------------------------

// Receive haalt één frame op; 0 = niets beschikbaar (non-blocking, zoals de
// andere twee drivers — hopnet's rxLoop is de enige aanroeper).
func (n *Net) Receive(buf []byte) (int, error) {
	d := n.rxDesc + uintptr(n.rxCur)*descStride

	dev.CleanInv(d, descSize)
	dev.MB()
	sts := dev.Read32(d + descStatus)
	if sts&descOwn != 0 {
		return 0, nil // nog van de DMA
	}

	length := 0
	switch {
	case sts&rxStsError != 0, sts&(rxStsFirst|rxStsLast) != rxStsFirst|rxStsLast:
		// Foutframe of een frame over meerdere descriptors (kan niet: wat we
		// als buffergrootte melden ligt boven de MTU). Descriptor teruggeven,
		// frame weg — en het rauwe statuswoord bewaren, want zonder dat is
		// "afgekeurd" op ijzer niet te ontleden (zie Diag).
		n.rxErrors++
		n.rxLastErr = sts
	default:
		length = int(sts>>rxLenShift&rxLenMask) - 4 // FCS eraf
		if length < 0 {
			length = 0
		}
		if length > len(buf) {
			length = len(buf)
		}
		src := n.rxBuf + uintptr(n.rxCur)*bufSize
		dev.CleanInv(src, uintptr(length))
		dev.MB()
		dev.CopyOut(buf[:length], src)
		n.rxFrames++
	}

	// Descriptor teruggeven aan de DMA en hem porren. De cntl-woorden
	// (buffergrootte, ring-end) staan er nog van initRings.
	dev.Write32(d+descStatus, descOwn)
	dev.MB()
	dev.CleanInv(d, descSize)
	n.wr(dmaRxPoll, 1)

	n.rxCur = (n.rxCur + 1) % numDesc
	return length, nil
}

// Transmit verstuurt één frame en wacht tot de descriptor vrij is (blocking,
// zoals gem/genet: de stack serialiseert TX).
func (n *Net) Transmit(buf []byte) error {
	if len(buf) == 0 || len(buf) > maxFrame {
		return fmt.Errorf("dwmac: frame van %d bytes past niet in TBS1 (max %d)", len(buf), maxFrame)
	}

	d := n.txDesc + uintptr(n.txCur)*descStride
	for t := 0; ; t++ {
		dev.CleanInv(d, descSize)
		dev.MB()
		if dev.Read32(d+descStatus)&descOwn == 0 {
			break
		}
		if t > 100000 {
			return fmt.Errorf("dwmac: TX-ring loopt niet leeg (DMA status %#08x)", n.rd(dmaStatus))
		}
		time.Sleep(10 * time.Microsecond)
	}

	dst := n.txBuf + uintptr(n.txCur)*bufSize
	dev.Copy(dst, buf)
	dev.MB()
	dev.CleanInv(dst, uintptr(len(buf)))

	dev.Write32(d+descCntl, txCntl(len(buf), n.txCur == numDesc-1))
	dev.MB()
	dev.Write32(d+descStatus, descOwn) // pas hierna is hij van de DMA
	dev.MB()
	dev.CleanInv(d, descSize)
	n.wr(dmaTxPoll, 1)

	n.txFrames++
	n.txCur = (n.txCur + 1) % numDesc
	return nil
}

// Diag is het meetinstrument voor een mislukte bring-up: één regel die zegt
// of de DMA liep, waar beide ringen staan en wat de MAC ervan vond. Het board
// hangt hem aan de fout als DHCP niets oplevert — dan is één boot genoeg om te
// weten of TX de deur uit ging, of RX niets binnenkreeg, of geen van beide.
func (n *Net) Diag() string {
	rx := n.rxDesc + uintptr(n.rxCur)*descStride
	tx := n.txDesc + uintptr(n.txCur)*descStride
	dev.CleanInv(rx, descSize)
	dev.CleanInv(tx, descSize)
	dev.MB()
	return fmt.Sprintf("dma-status %#08x op-mode %#08x rx=%d/%d(err) last-err %#08x tx=%d "+
		"rxdesc[%d] %#08x hw-rx %#08x txdesc[%d] %#08x hw-tx %#08x",
		n.rd(dmaStatus), n.rd(dmaOpMode), n.rxFrames, n.rxErrors, n.rxLastErr, n.txFrames,
		n.rxCur, dev.Read32(rx+descStatus), n.rd(dmaCurRxDesc),
		n.txCur, dev.Read32(tx+descStatus), n.rd(dmaCurTxDesc))
}
