// Package igb is HopOS' driver voor de Intel igb-familie gigabit-MACs — op
// de Ampere Altra Dev Kit de I210 (8086:1533), en in QEMU het 82576-model
// (8086:10c9) waarmee dit pad end-to-end getest wordt. Geschreven naar het
// I210-datasheet met de Linux igb-driver als referentie (PLAN.md-werkwijze);
// polled, één RX- en één TX-queue met advanced descriptors (het enige type
// dat Linux gebruikt én dat QEMU's igb-model emuleert) — zelfde vorm als
// metal/driver/nic/gem en metal/driver/nic/virtionet, en net als die drivers een go-net
// NetworkDevice (Receive/Transmit op rauwe frames).
//
// GESCHREVEN VÓÓR HET EERSTE ALTRA-CONTACT: de registerlaag is tegen QEMU's
// igb bewezen (gedeelde 82575+-registerfamilie); de I210-specifieke aannames
// (interne PHY-autoneg na reset, NVM-autoload van het MAC) staan gemarkeerd
// en cmd/probeuefi meet ze op het echte bord vóór er iets op leunt. De
// firmware (UEFI) heeft de BAR's al toegewezen; wij lezen alleen uit
// (pcie.Device.BAR) en zetten bus-mastering aan (pcie.Device.Enable).
package igb

import (
	"fmt"
	"time"

	"hop-os/metal/dev"
)

// Registeroffsets (82575+-familie: 82576 = QEMU, I210/I211 = Altra; Linux
// e1000_regs.h). Queue-registers zijn de queue-0-aliassen op 0x2800/0x3800.
const (
	regCTRL   = 0x0000
	regSTATUS = 0x0008
	regRCTL   = 0x0100
	regTCTL   = 0x0400
	regMDIC   = 0x0020 // MDI control: PHY-registertoegang
	regICR    = 0x1500 // interrupt cause (lezen = wissen)
	regIMC    = 0x150C // interrupt mask clear (we pollen: alles dicht)

	regRDBAL  = 0x2800
	regRDBAH  = 0x2804
	regRDLEN  = 0x2808
	regSRRCTL = 0x280C
	regRDH    = 0x2810
	regRDT    = 0x2818
	regRXDCTL = 0x2828
	regTDBAL  = 0x3800
	regTDBAH  = 0x3804
	regTDLEN  = 0x3808
	regTDH    = 0x3810
	regTDT    = 0x3818
	regTXDCTL = 0x3828
	regRAL0   = 0x5400 // MAC-adres (NVM-autoload na reset)
	regRAH0   = 0x5404

	// CTRL.
	ctrlSLU = 1 << 6  // set link up (verplicht vóór de MAC een link meldt)
	ctrlRST = 1 << 26 // device-reset (zelfwissend)

	// STATUS.
	statusFD         = 1 << 0
	statusLU         = 1 << 1
	statusSpeedShift = 6 // [7:6]: 00=10, 01=100, 1x=1000

	// MDIC: DATA [15:0], REGADD [20:16], PHYADD [25:21], OP [27:26]
	// (01=write, 10=read), READY bit 28, ERROR bit 30. De interne PHY van de
	// igb-familie antwoordt op adres 1 (Linux hw->phy.addr).
	mdicWriteOp = 0b01 << 26
	mdicReadOp  = 0b10 << 26
	mdicReady   = 1 << 28
	mdicError   = 1 << 30
	phyAddr     = 1

	// PHY BMCR (register 0): autonegotiatie aan + herstart — het standaard
	// igb-recept na een device-reset; zonder deze herstart meldt (o.a. het
	// QEMU-model) de link zich niet.
	bmcrAutoNegRestart = 0x1200

	// RCTL.
	rctlEN    = 1 << 1
	rctlBAM   = 1 << 15 // broadcast accepteren (DHCP!)
	rctlSECRC = 1 << 26 // FCS strippen

	// TCTL: enable, pad short packets, en de standaard collision-waarden
	// (CT=15, COLD=0x3F — alleen relevant op half-duplex, maar het zijn de
	// datasheet-defaults die Linux ook schrijft).
	tctlEN   = 1 << 1
	tctlPSP  = 1 << 3
	tctlCT   = 0x0F << 4
	tctlCOLD = 0x3F << 12

	// SRRCTL: buffergrootte in 1KB-eenheden [6:0] + descriptortype [27:25]
	// (001 = advanced, één buffer).
	srrctlBSize2K  = 2
	srrctlDescAdv1 = 0b001 << 25

	// RXDCTL/TXDCTL.
	qEnable = 1 << 25

	// Advanced RX-descriptor writeback (qword 1): status [19:0], lengte
	// [47:32] — als 32-bit woorden: status in w2, lengte in w3 [15:0].
	rxDD  = 1 << 0
	rxEOP = 1 << 1

	// Advanced TX-descriptor (w2 = cmd_type_len): lengte [17:0] + DTYP data
	// (0011<<20) + DCMD: EOP, IFCS (FCS door de MAC), RS (writeback DD),
	// DEXT (advanced). w3 = olinfo: payload-lengte << 14. Writeback: DD in
	// w3 bit 0.
	txDTypeData = 0b0011 << 20
	txEOP       = 1 << 24
	txIFCS      = 1 << 25
	txRS        = 1 << 27
	txDEXT      = 1 << 29
	txPayShift  = 14
	txDD        = 1 << 0

	txTimeout = 100 * time.Millisecond

	bufSize = 2048 // SRRCTL BSIZEPKT-eenheid; ruim boven 1522
	nRx     = 64
	nTx     = 16
)

// supported zijn de igb-familieleden die deze driver aankan: de I210/I211
// (Altra Dev Kit, koper/serdes/fiber/sgmii) en de 82576 (QEMU's igb-model —
// het testpad). Eén tabel; probe én board vragen ernaar via Supported.
var supported = map[uint16]bool{
	0x10c9: true, // 82576 (QEMU)
	0x1533: true, // I210 koper
	0x1536: true, // I210 fiber
	0x1537: true, // I210 serdes
	0x1538: true, // I210 sgmii
	0x1539: true, // I211
}

// Supported meldt of een Intel-device-ID (vendor 0x8086) door deze driver
// gedreven wordt.
func Supported(deviceID uint16) bool { return supported[deviceID] }

// Net is één igb-instantie.
type Net struct {
	Base   uintptr // BAR0-registerblok (door de firmware toegewezen)
	BusOff uint64  // DMA-vertaling: busadres = fysiek + BusOff (Altra/QEMU: 0)
	MAC    [6]byte // gelezen uit RAL0/RAH0 door Reset

	rxRing, txRing uintptr
	rxBufs, txBufs uintptr
	rxHead, txHead int
}

func (n *Net) rd(off uintptr) uint32    { return dev.Read32(n.Base + off) }
func (n *Net) wr(off uintptr, v uint32) { dev.Write32(n.Base+off, v) }

// Reset doet de igb_reset_hw-kern: interrupts dicht, RX/TX uit, CTRL.RST,
// wachten tot hij zichzelf wist, interrupts opnieuw dicht, en het MAC-adres
// uit RAL0/RAH0 (de NVM laadt het daar automatisch na reset — I210-aanname,
// QEMU-bewezen; een leeg register is een meting, geen driver-bug).
func (n *Net) Reset() error {
	n.wr(regIMC, 0xFFFFFFFF)
	n.wr(regRCTL, 0)
	n.wr(regTCTL, tctlPSP)
	dev.MB()

	n.wr(regCTRL, n.rd(regCTRL)|ctrlRST)
	time.Sleep(10 * time.Millisecond)
	for range 100 {
		if n.rd(regCTRL)&ctrlRST == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if n.rd(regCTRL)&ctrlRST != 0 {
		return fmt.Errorf("igb: reset stuck (CTRL=%#x)", n.rd(regCTRL))
	}
	n.wr(regIMC, 0xFFFFFFFF)
	n.rd(regICR) // restjes wissen

	ral, rah := n.rd(regRAL0), n.rd(regRAH0)
	if ral == 0 && rah&0xFFFF == 0 {
		return fmt.Errorf("igb: no MAC in RAL0/RAH0 (empty NVM?)")
	}
	n.MAC = [6]byte{
		byte(ral), byte(ral >> 8), byte(ral >> 16), byte(ral >> 24),
		byte(rah), byte(rah >> 8),
	}
	return nil
}

// MDICRead leest een PHY-register via het MDI-control-register (begrensd
// pollen op READY — een dode MDI-bus mag de boot niet gijzelen).
func (n *Net) MDICRead(reg int) (uint16, error) {
	n.wr(regMDIC, uint32(reg&0x1F)<<16|phyAddr<<21|mdicReadOp)
	return n.mdicWait()
}

// MDICWrite schrijft een PHY-register.
func (n *Net) MDICWrite(reg int, val uint16) error {
	n.wr(regMDIC, uint32(val)|uint32(reg&0x1F)<<16|phyAddr<<21|mdicWriteOp)
	_, err := n.mdicWait()
	return err
}

func (n *Net) mdicWait() (uint16, error) {
	for range 100_000 {
		v := n.rd(regMDIC)
		if v&mdicError != 0 {
			return 0, fmt.Errorf("igb: MDI error (MDIC=%#x)", v)
		}
		if v&mdicReady != 0 {
			return uint16(v), nil
		}
	}
	return 0, fmt.Errorf("igb: MDI stuck busy")
}

// LinkUp zet CTRL.SLU, herstart PHY-autonegotiatie (het igb-recept na een
// reset — de MAC volgt de PHY, geen force-bits) en wacht op een link.
// Geeft (snelheid in Mbps, full-duplex).
func (n *Net) LinkUp(timeout time.Duration) (speed int, fd bool, err error) {
	n.wr(regCTRL, n.rd(regCTRL)|ctrlSLU)
	if err := n.MDICWrite(0, bmcrAutoNegRestart); err != nil {
		return 0, false, err
	}
	deadline := time.Now().Add(timeout)
	for n.rd(regSTATUS)&statusLU == 0 {
		if time.Now().After(deadline) {
			return 0, false, fmt.Errorf("igb: no link within %v (cable? STATUS=%#x)", timeout, n.rd(regSTATUS))
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := n.rd(regSTATUS)
	switch s >> statusSpeedShift & 0b11 {
	case 0:
		speed = 10
	case 1:
		speed = 100
	default:
		speed = 1000
	}
	return speed, s&statusFD != 0, nil
}

// Init zet de ringen klaar in de DMA-regio (device-gemapt → ongecachet →
// coherent zonder cache-onderhoud, de HopOS-conventie) en zet RX/TX aan.
// Reset moet al gedaan zijn (MAC bekend).
func (n *Net) Init(dmaBase, dmaSize uintptr) error {
	need := uintptr(nRx*16 + nTx*16 + (nRx+nTx)*bufSize)
	if dmaSize < need {
		return fmt.Errorf("igb: DMA region %#x < %#x", dmaSize, need)
	}
	n.rxRing = dmaBase
	n.txRing = dmaBase + nRx*16
	n.rxBufs = dmaBase + nRx*16 + nTx*16
	n.txBufs = n.rxBufs + nRx*bufSize

	// RX-ring: advanced read-format = {pkt_addr, hdr_addr(0)} per descriptor.
	for i := 0; i < nRx; i++ {
		n.armRx(i)
	}
	// TX-ring: schoon; eigendom volgt uit TDH/TDT en de DD-writeback.
	for i := 0; i < nTx*16; i += 4 {
		dev.Write32(n.txRing+uintptr(i), 0)
	}
	dev.MB()

	// RX-queue 0: basis/lengte, 2KB-buffers advanced, enable, dán RDT vullen
	// (igb_configure_rx_ring-volgorde).
	rxBus := uint64(n.rxRing) + n.BusOff
	n.wr(regRDBAL, uint32(rxBus))
	n.wr(regRDBAH, uint32(rxBus>>32))
	n.wr(regRDLEN, nRx*16)
	n.wr(regSRRCTL, srrctlBSize2K|srrctlDescAdv1)
	n.wr(regRDH, 0)
	n.wr(regRDT, 0)
	n.wr(regRXDCTL, qEnable)
	for range 1000 {
		if n.rd(regRXDCTL)&qEnable != 0 {
			break
		}
	}
	n.wr(regRCTL, rctlEN|rctlBAM|rctlSECRC)
	n.wr(regRDT, nRx-1) // alle descriptors aan de hardware

	// TX-queue 0.
	txBus := uint64(n.txRing) + n.BusOff
	n.wr(regTDBAL, uint32(txBus))
	n.wr(regTDBAH, uint32(txBus>>32))
	n.wr(regTDLEN, nTx*16)
	n.wr(regTDH, 0)
	n.wr(regTDT, 0)
	n.wr(regTXDCTL, qEnable)
	for range 1000 {
		if n.rd(regTXDCTL)&qEnable != 0 {
			break
		}
	}
	n.wr(regTCTL, tctlEN|tctlPSP|tctlCT|tctlCOLD)
	dev.MB()
	return nil
}

// armRx zet descriptor i terug in read-format met zijn eigen buffer.
func (n *Net) armRx(i int) {
	d := n.rxRing + uintptr(i)*16
	bus := uint64(n.rxBufs+uintptr(i)*bufSize) + n.BusOff
	dev.Write32(d+0, uint32(bus))
	dev.Write32(d+4, uint32(bus>>32))
	dev.Write32(d+8, 0) // hdr_addr laag + writeback-status komt hier terug
	dev.Write32(d+12, 0)
}

// Receive haalt één frame op (0 = niets) — go-net NetworkDevice.
func (n *Net) Receive(buf []byte) (int, error) {
	d := n.rxRing + uintptr(n.rxHead)*16
	status := dev.Read32(d + 8)
	if status&rxDD == 0 {
		return 0, nil
	}
	dev.MB()
	length := int(dev.Read32(d+12) & 0xFFFF)
	if status&rxEOP == 0 {
		// Frame > buffer (kan niet bij 2KB vs MTU 1522) — descriptor weg-
		// gooien en doorschuiven i.p.v. een halve frame afleveren.
		length = 0
	}
	if length > len(buf) {
		length = len(buf)
	}
	if length > 0 {
		dev.CopyOut(buf[:length], n.rxBufs+uintptr(n.rxHead)*bufSize)
	}

	// Descriptor herwapenen en aan de hardware geven (RDT = laatst gevulde).
	n.armRx(n.rxHead)
	dev.MB()
	n.wr(regRDT, uint32(n.rxHead))
	n.rxHead = (n.rxHead + 1) % nRx
	return length, nil
}

// Transmit verstuurt één frame (wacht begrensd op een vrije descriptor) —
// go-net NetworkDevice.
func (n *Net) Transmit(buf []byte) error {
	if len(buf) > bufSize {
		return fmt.Errorf("igb: frame %d > %d", len(buf), bufSize)
	}
	d := n.txRing + uintptr(n.txHead)*16

	// Ring-occupancy éérst (review #3): TDT mag nooit op TDH uitkomen —
	// TDT==TDH betekent voor de 82575/I210-familie "lege ring", en een
	// burst van nTx posts zonder deze rem doet precies dat (waarna de
	// hardware níets meer fetcht). De e1000-invariant: max nTx−1 uitstaand,
	// dus wachten zolang onze volgende TDT-waarde de TDH raakt.
	next := uint32((n.txHead + 1) % nTx)
	deadline := time.Now().Add(txTimeout)
	for n.rd(regTDH)&0xFFFF == next {
		if time.Now().After(deadline) {
			return fmt.Errorf("igb: TX ring full after %v (TDH stuck at %d)", txTimeout, next)
		}
	}

	// En de per-descriptor-status: DD gezet (writeback) of nooit gebruikt
	// (w2==0). Begrensd wachten, gem-conventie: een hangende DMA mag de
	// caller niet eeuwig gijzelen.
	for dev.Read32(d+8) != 0 && dev.Read32(d+12)&txDD == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("igb: TX descriptor %d still busy after %v (DMA stuck?)", n.txHead, txTimeout)
		}
	}

	bus := uint64(n.txBufs+uintptr(n.txHead)*bufSize) + n.BusOff
	dev.Copy(n.txBufs+uintptr(n.txHead)*bufSize, buf)
	dev.Write32(d+0, uint32(bus))
	dev.Write32(d+4, uint32(bus>>32))
	dev.Write32(d+12, uint32(len(buf))<<txPayShift)
	dev.MB()
	dev.Write32(d+8, uint32(len(buf))|txDTypeData|txEOP|txIFCS|txRS|txDEXT)
	dev.MB()
	n.txHead = (n.txHead + 1) % nTx
	n.wr(regTDT, uint32(n.txHead)) // doorbell: hardware haalt t/m TDT-1 op
	return nil
}
