// Package gem is HopOS' driver voor de Cadence GEM gigabit-MAC — op de
// Raspberry Pi 5 het ethernet-blok in de RP1-southbridge (fase P2), en
// dezelfde IP-core die op veel andere ARM-SoC's zit. Geschreven naar het
// RP1-peripherals-datasheet en de macb-registerlayout (Linux/u-boot als
// referentie, conform PLAN.md-werkwijze); polled, één RX- en één TX-queue —
// zelfde vorm als metal/virtionet, en net als die driver een
// go-net NetworkDevice (Receive/Transmit op rauwe frames).
//
// GESCHREVEN VÓÓR HET EERSTE BOARD-CONTACT: de registeraannames staan
// per stuk gemarkeerd en metal/cmd/probe5 verifieert ze read-only (module-ID,
// MDIO, PHY-ID's, linkstatus) vóórdat deze driver DMA aanzet. BusOff is de
// DMA-adresvertaling (bus = fysiek + BusOff) — op de Pi 5 bepaalt het
// inbound-window van de root-complex die offset; de probe leest hem uit.
package gem

import (
	"fmt"
	"time"

	"hop-os/metal/dev"
	"hop-os/metal/mdio"
)

// Cadence GEM-registeroffsets (macb/u-boot-conventie).
const (
	regNWCtrl    = 0x000 // network control
	regNWCfg     = 0x004 // network config
	regNWStatus  = 0x008 // network status
	regDMACfg    = 0x010 // DMA config
	regTxStatus  = 0x014
	regRxQBase   = 0x018
	regTxQBase   = 0x01C
	regRxStatus  = 0x020
	regIDR       = 0x02C // interrupt disable
	regMAN       = 0x034 // PHY maintenance (MDIO)
	regPBufRxCut = 0x044 // RX partial store&forward (uit = 0)
	regSpAddr1B  = 0x088 // MAC-adres bottom
	regSpAddr1T  = 0x08C // MAC-adres top
	regModuleID  = 0x0FC // module/revisie-ID (leesbaar zonder init)
	regDCFG1     = 0x280 // design config 1: DBWDEF [27:25] = AXI-busbreedte
	regDCFG6     = 0x294 // design config 6: queue-mask [7:0], DAW64 bit 23
	regTBQPH     = 0x4C8 // TX queue base, hoge 32 bits (64-bit DMA)
	regRBQPH     = 0x4D4 // RX queue base, hoge 32 bits

	// NWCTRL-bits.
	ctrlRxEn    = 1 << 2
	ctrlTxEn    = 1 << 3
	ctrlMgmtEn  = 1 << 4 // MDIO-poort aan
	ctrlClrStat = 1 << 5 // statistiekentellers wissen (macb_reset_hw)
	ctrlTxStart = 1 << 9

	// NWCFG-bits.
	cfgSpeed100 = 1 << 0
	cfgFD       = 1 << 1
	cfgGigabit  = 1 << 10
	cfgRxFCS    = 1 << 17 // FCS strippen van RX-frames
	cfgMDCShift = 18      // MDC-divisor (3 bits): pclk/x — 0b100 = /48
	cfgMDCDiv48 = 0b100 << cfgMDCShift
	cfgDBWShift = 21 // databusbreedte [22:21]: 0=32, 1=64, 2=128 — MOET de
	// gesynthetiseerde AXI-breedte (DCFG1.DBWDEF) matchen; GEMETEN 2026-07-10
	// (probe6 run 4): met DBW=0 op de RP1-GEM is álle DMA dood (TGO blijft
	// hangen, nul RX) terwijl MDIO gewoon werkt — Cadence-vereiste, zie
	// macb_dbw() in de Linux-referentie.

	// NWSTATUS-bits.
	statusMDIOIdle = 1 << 2

	// DMACFG-bits.
	dmaBurstIncr16 = 0x10      // AHB/AXI burst
	dmaRxSizeShift = 16        // RX-buffergrootte in 64-byte-eenheden [23:16]
	dmaTxPBufFull  = 1 << 10   // TX packet buffer: volle grootte
	dmaRxPBufFull  = 0b11 << 8 // RX packet buffer: volle grootte
	dmaAddr64      = 1 << 30   // 64-bit descriptors (adres-hi in woord 2)

	// RX-descriptor woord 0.
	rxOwned = 1 << 0 // software mag hem lezen (DMA klaar)
	rxWrap  = 1 << 1
	// RX-descriptor woord 1.
	rxLenMask = 0x1FFF

	// TX-descriptor woord 1.
	txUsed = 1 << 31
	txWrap = 1 << 30
	txLast = 1 << 15

	// txTimeout begrenst de wacht op een vrije TX-descriptor (Transmit).
	txTimeout = 100 * time.Millisecond

	// MDIO-frame (MAN-register).
	manClause22 = 1 << 30
	manRead     = 0b10 << 28
	manWrite    = 0b01 << 28
	manMustBe10 = 0b10 << 16

	mtuBuf = 1536 // buffergrootte per descriptor (64-voud)
	nRx    = 64
	nTx    = 16
)

// Net is één GEM-instantie.
type Net struct {
	Base   uintptr // GEM-registerblok (ARM-zicht)
	BusOff uint64  // DMA-vertaling: busadres = fysiek adres + BusOff
	MAC    [6]byte

	rxRing, txRing uintptr // descriptor-ringen (4 woorden per descriptor)
	rxBufs, txBufs uintptr
	rxHead, txHead int
}

func (n *Net) rd(off uintptr) uint32    { return dev.Read32(n.Base + off) }
func (n *Net) wr(off uintptr, v uint32) { dev.Write32(n.Base+off, v) }

// ModuleID geeft het GEM module/revisie-register — de eerste read-only
// verificatie dat het registerblok echt antwoordt.
func (n *Net) ModuleID() uint32 { return n.rd(regModuleID) }

// DesignCfg geeft (DCFG1, DCFG6): de gesynthetiseerde AXI-busbreedte
// (DBWDEF [27:25]), het queue-mask [7:0] en DAW64 (bit 23) — de
// hardware-eigenschappen waar Init zich naar richt (diagnose/probe).
func (n *Net) DesignCfg() (dcfg1, dcfg6 uint32) { return n.rd(regDCFG1), n.rd(regDCFG6) }

// TxStatus geeft het rauwe TSR (bit 3 = TGO: zender actief; diagnose).
func (n *Net) TxStatus() uint32 { return n.rd(regTxStatus) }

// MDIORead leest een clause-22 PHY-register (blokkeert tot de bus vrij is).
func (n *Net) MDIORead(phy, reg int) uint16 {
	n.mdioWait()
	n.wr(regMAN, manClause22|manRead|uint32(phy&0x1F)<<23|uint32(reg&0x1F)<<18|manMustBe10)
	n.mdioWait()
	return uint16(n.rd(regMAN))
}

// MDIOWrite schrijft een clause-22 PHY-register.
func (n *Net) MDIOWrite(phy, reg int, val uint16) {
	n.mdioWait()
	n.wr(regMAN, manClause22|manWrite|uint32(phy&0x1F)<<23|uint32(reg&0x1F)<<18|manMustBe10|uint32(val))
	n.mdioWait()
}

func (n *Net) mdioWait() {
	for range 100_000 { // enkele µs typisch; ruim begrensd, nooit eeuwig
		if n.rd(regNWStatus)&statusMDIOIdle != 0 {
			return
		}
	}
}

// MDIOEnable zet alleen de management-poort aan (voor de probe: MDIO-scan
// zonder verder iets te initialiseren).
func (n *Net) MDIOEnable() {
	n.wr(regNWCfg, cfgMDCDiv48)
	n.wr(regNWCtrl, ctrlMgmtEn)
}

// PHYScan zoekt PHY's op de MDIO-bus en geeft (adres, id1, id2) van de
// eerste hit; de BCM54213PE meldt zich met OUI 0x600d. Gedeeld met genet:
// zie metal/mdio (zelfde PHY, zelfde clause-22-scan).
func (n *Net) PHYScan() (addr int, id1, id2 uint16, found bool) {
	return mdio.Scan(n)
}

// AutoNeg start autonegotiatie op de PHY en wacht op een link; geeft
// (snelheid in Mbps, full-duplex). Gedeeld met genet via metal/mdio.
func (n *Net) AutoNeg(phy int, timeout time.Duration) (speed int, fd bool, err error) {
	return mdio.AutoNeg(n, phy, timeout)
}

// Init zet ringen en MAC-config klaar in de DMA-regio (device-gemapt →
// ongecachet → coherent met de GEM zonder cache-onderhoud) en zet RX/TX aan.
// speed/fd komen uit AutoNeg; de RP1-CLKGEN volgt de MAC-snelheid vanzelf.
func (n *Net) Init(dmaBase, dmaSize uintptr, speed int, fd bool) error {
	need := uintptr(nRx*16 + nTx*16 + nRx*mtuBuf + nTx*mtuBuf)
	if dmaSize < need {
		return fmt.Errorf("gem: DMA-regio %#x < %#x", dmaSize, need)
	}
	n.rxRing = dmaBase
	n.txRing = dmaBase + nRx*16
	n.rxBufs = dmaBase + nRx*16 + nTx*16
	n.txBufs = n.rxBufs + nRx*mtuBuf

	// Alles uit, interrupts dicht (we pollen), status wissen — het volledige
	// macb_reset_hw-recept (tellers, alle statusbits, RX-cut-through uit).
	n.wr(regNWCtrl, 0)
	n.wr(regNWCtrl, ctrlClrStat)
	n.wr(regIDR, 0xFFFFFFFF)
	n.wr(regTxStatus, 0xFFFFFFFF)
	n.wr(regRxStatus, 0xFFFFFFFF)
	n.wr(regPBufRxCut, 0)

	// RX-ring: descriptors wijzen naar eigen buffers; DMA is eigenaar.
	for i := 0; i < nRx; i++ {
		d := n.rxRing + uintptr(i)*16
		bus := uint64(n.rxBufs+uintptr(i)*mtuBuf) + n.BusOff
		w0 := uint32(bus) &^ 0b11
		if i == nRx-1 {
			w0 |= rxWrap
		}
		dev.Write32(d+0, w0)
		dev.Write32(d+4, 0)
		dev.Write32(d+8, uint32(bus>>32))
		dev.Write32(d+12, 0)
	}
	// TX-ring: alle descriptors aan software (USED).
	for i := 0; i < nTx; i++ {
		d := n.txRing + uintptr(i)*16
		w1 := uint32(txUsed)
		if i == nTx-1 {
			w1 |= txWrap
		}
		dev.Write32(d+0, 0)
		dev.Write32(d+4, w1)
		dev.Write32(d+8, 0)
		dev.Write32(d+12, 0)
	}
	dev.MB()

	// MAC-adres (filter) — BusOff-onafhankelijk.
	n.wr(regSpAddr1B, uint32(n.MAC[0])|uint32(n.MAC[1])<<8|uint32(n.MAC[2])<<16|uint32(n.MAC[3])<<24)
	n.wr(regSpAddr1T, uint32(n.MAC[4])|uint32(n.MAC[5])<<8)

	// Databusbreedte uit de hardware zelf (DCFG1.DBWDEF: 4=128, 2=64, 1=32) —
	// zie de toelichting bij cfgDBWShift.
	var dbw uint32
	switch d := n.rd(regDCFG1) >> 25 & 0x7; {
	case d >= 4:
		dbw = 2 << cfgDBWShift
	case d >= 2:
		dbw = 1 << cfgDBWShift
	}

	cfg := uint32(cfgMDCDiv48|cfgRxFCS) | dbw
	if fd {
		cfg |= cfgFD
	}
	switch speed {
	case 1000:
		cfg |= cfgGigabit
	case 100:
		cfg |= cfgSpeed100
	}
	n.wr(regNWCfg, cfg)

	n.wr(regDMACfg, dmaBurstIncr16|dmaTxPBufFull|dmaRxPBufFull|
		uint32(mtuBuf/64)<<dmaRxSizeShift|dmaAddr64)

	rxBus := uint64(n.rxRing) + n.BusOff
	txBus := uint64(n.txRing) + n.BusOff
	n.wr(regRxQBase, uint32(rxBus))
	n.wr(regRBQPH, uint32(rxBus>>32))
	n.wr(regTxQBase, uint32(txBus))
	n.wr(regTBQPH, uint32(txBus>>32))

	n.wr(regNWCtrl, ctrlMgmtEn|ctrlRxEn|ctrlTxEn)
	return nil
}

// Receive haalt één frame op (0 = niets) — go-net NetworkDevice.
func (n *Net) Receive(buf []byte) (int, error) {
	d := n.rxRing + uintptr(n.rxHead)*16
	w0 := dev.Read32(d + 0)
	if w0&rxOwned == 0 {
		return 0, nil
	}
	dev.MB()
	length := int(dev.Read32(d+4) & rxLenMask)
	if length > len(buf) {
		length = len(buf)
	}
	dev.CopyOut(buf[:length], n.rxBufs+uintptr(n.rxHead)*mtuBuf)

	// Descriptor terug aan de DMA (adres blijft staan; alleen OWNED wissen).
	dev.Write32(d+4, 0)
	dev.MB()
	dev.Write32(d+0, w0&^rxOwned)
	dev.MB()
	n.rxHead = (n.rxHead + 1) % nRx
	return length, nil
}

// Transmit verstuurt één frame (synchroon starten; wacht niet op voltooiing,
// wél op een vrije descriptor) — go-net NetworkDevice.
func (n *Net) Transmit(buf []byte) error {
	if len(buf) > mtuBuf {
		return fmt.Errorf("gem: frame %d > %d", len(buf), mtuBuf)
	}
	d := n.txRing + uintptr(n.txHead)*16
	// Wacht op een vrije descriptor, maar begrensd — net als AutoNeg/virtionet
	// een deadline hebben. Een dode/hangende DMA mag deze goroutine (en dus de
	// caller) niet eeuwig laten busy-waiten.
	deadline := time.Now().Add(txTimeout)
	for dev.Read32(d+4)&txUsed == 0 { // DMA nog bezig met deze descriptor
		if time.Now().After(deadline) {
			return fmt.Errorf("gem: TX-descriptor %d blijft bezig na %v (DMA hangt?)", n.txHead, txTimeout)
		}
	}
	bus := uint64(n.txBufs+uintptr(n.txHead)*mtuBuf) + n.BusOff
	dev.Copy(n.txBufs+uintptr(n.txHead)*mtuBuf, buf)
	dev.Write32(d+0, uint32(bus))
	dev.Write32(d+8, uint32(bus>>32))
	w1 := uint32(len(buf)) | txLast
	if n.txHead == nTx-1 {
		w1 |= txWrap
	}
	dev.MB()
	dev.Write32(d+4, w1)
	dev.MB()
	n.wr(regNWCtrl, ctrlMgmtEn|ctrlRxEn|ctrlTxEn|ctrlTxStart)

	// RP1-eigenaardigheid (Linux-referentie macb_main.c, "TSTART write
	// might get dropped"): over de PCIe-backed AXI kan de TSTART-schrijf
	// verloren gaan terwijl de DMA net stopt. Linux geeft hem via de
	// IRQ-lus opnieuw; wij pollen kort en herhalen zolang de descriptor
	// van de hardware blijft (USED=0) én de zender niet actief is.
	for range 100 {
		if dev.Read32(d+4)&txUsed != 0 { // verstuurd
			break
		}
		if n.rd(regTxStatus)&(1<<3) == 0 { // TGO laag: DMA stilgevallen
			n.wr(regNWCtrl, ctrlMgmtEn|ctrlRxEn|ctrlTxEn|ctrlTxStart)
		}
		time.Sleep(10 * time.Microsecond)
	}
	n.txHead = (n.txHead + 1) % nTx
	return nil
}
