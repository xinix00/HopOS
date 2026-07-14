// Package genet is HopOS' driver voor de Broadcom GENET v5 gigabit-MAC — de
// geïntegreerde NIC van de Raspberry Pi 4 (BCM2711, ARM-zicht 0xFD580000).
// Géén PCIe zoals de Pi 5/RP1: direct memory-mapped, en de DMA-descriptors
// leven in régisterruimte (on-chip), niet in RAM — alleen de framebuffers
// liggen in DRAM (busadres = fysiek adres, 1:1; geen dma-ranges op de scb-bus).
//
// Recept uit de Linux-referentie bcmgenet.c/bcmmii.c (rpi-6.12.y) en vooral
// U-Boots minimale polled driver (uboot-bcmgenet.c) — die bewijst op de Pi 4
// precies onze vorm: ring 16 (de hardware-default), register-mode (géén
// 64-byte status-blocks), polled, geen HFB. Zelfde interface als metal/driver/nic/gem
// en metal/driver/nic/virtionet: go-net NetworkDevice (Receive/Transmit).
//
// De valkuilen uit de referentie zitten als commentaar bij de code; de twee
// grootste: elke TX-descriptor MOET QTAG 0x3F<<7 dragen (anders eet de
// arbiter het frame), en prod/cons-indices zijn vrijlopende 16-bit-tellers
// (mod 0x10000) náást de descriptor-pointer (mod 256).
package genet

import (
	"fmt"
	"time"

	"hop-os/metal/dev"
	"hop-os/metal/driver/nic/mdio"
)

// Registeroffsets vanaf de GENET-basis (bcmgenet.h, GENET_V5-hw_params).
const (
	sysRevCtrl       = 0x000 // versie-nibble [27:24]: v5 meldt 6
	sysPortCtrl      = 0x004 // 3 = PORT_MODE_EXT_GPHY
	sysRBufFlushCtrl = 0x008 // bit0 = RX-flush, bit1 = umac-sw-reset

	extRGMIIOOBCtrl = 0x08C // RGMII_LINK bit4, OOB_DISABLE bit5, RGMII_MODE_EN bit6, ID_MODE_DIS bit16

	intrl2_0Clear = 0x208
	intrl2_0Set   = 0x210
	intrl2_1Clear = 0x248
	intrl2_1Set   = 0x250

	rbufCtrl         = 0x300 // bit1 = ALIGN_2B (2 pad-bytes vóór elk RX-frame)
	rbufTBufSizeCtrl = 0x3B4

	umacCmd      = 0x808 // TX_EN 0, RX_EN 1, speed [3:2], PROMISC 4, SW_RESET 13, LCL_LOOP_EN 15
	umacMAC0     = 0x80C
	umacMAC1     = 0x810
	umacMaxFrame = 0x814
	umacTxFlush  = 0xB34
	umacMIBCtrl  = 0xD80
	umacMDIOCmd  = 0xE14 // START_BUSY 29, READ_FAIL 28, RD 2<<26, WR 1<<26, phy<<21, reg<<16, data
	umacMDFCtrl  = 0xE50 // filter-enable-bits: filter n = bit 16-n
	umacMDFAddr  = 0xE54 // 17 filters × 2 woorden

	// DMA: 256 descriptors van 3 woorden (12 bytes), dáárna de ringregisters
	// (0x40 per ring, ring 16 = de default), dáárna de gedeelde registers.
	rxBD        = 0x2000
	rdmaRing16  = 0x3000 // WRITE_PTR +0, PROD +8, CONS +C, BUF_SIZE +10, START +14, END +1C, XON +28, READ_PTR +2C
	rdmaRingCfg = 0x3040
	rdmaCtrl    = 0x3044
	rdmaBurst   = 0x304C
	txBD        = 0x4000
	tdmaRing16  = 0x5000 // READ_PTR +0, CONS +8, PROD +C, BUF_SIZE +10, START +14, END +1C, FLOW +28, WRITE_PTR +2C
	tdmaRingCfg = 0x5040
	tdmaCtrl    = 0x5044
	tdmaBurst   = 0x504C

	// LENGTH_STATUS-bits (woord 0 van elke descriptor; lengte in [27:16]).
	dmaEOP = 0x4000
	dmaSOP = 0x2000
	// TX: QTAG [12:7] moet 0x3F zijn (valkuil 1) + CRC laten aanhangen.
	txQTag      = 0x3F << 7
	txAppendCRC = 0x0040
	// RX-foutbits: OV, CRC, RXER, NO, LG.
	rxErrMask = 0x001F

	nBD     = 256  // descriptors per richting (alle aan ring 16, als U-Boot)
	bufSize = 2048 // RX_BUF_LENGTH; ook de TX-korrel

	txTimeout = 100 * time.Millisecond
)

// Net is één GENET-instantie.
type Net struct {
	Base uintptr
	MAC  [6]byte

	rxBufs, txBufs uintptr
	rxCons, txProd uint32 // vrijlopende index, mod 0x10000 (valkuil 3); de
	// descriptor-pointer is int(...)%nBD — nBD deelt 0x10000, dus consistent.
}

func (n *Net) rd(off uintptr) uint32           { return dev.Read32(n.Base + off) }
func (n *Net) wr(off uintptr, v uint32)        { dev.Write32(n.Base+off, v) }
func (n *Net) mod(off uintptr, mask, v uint32) { n.wr(off, n.rd(off)&^mask|v) }

// Rev geeft het rauwe SYS_REV_CTRL-register; nibble [27:24] hoort 6 te zijn
// (zo meldt v5-silicium zich — de eerste read-only verificatie).
func (n *Net) Rev() uint32 { return n.rd(sysRevCtrl) }

// Reset brengt de MAC in een bekende staat: de U-Boot-sequence (sw-reset mét
// tijdelijke local-loopback voor een stabiele rxclk — en SW_RESET daarna
// écht wissen, valkuil 2), MIB-reset, framelengte, RX-alignment, interrupts
// dicht (wij pollen) en de poort-mux naar de externe GPHY. Hierna werkt MDIO.
func (n *Net) Reset() {
	r := n.rd(sysRBufFlushCtrl)
	n.wr(sysRBufFlushCtrl, r|2)
	time.Sleep(10 * time.Microsecond)
	n.wr(sysRBufFlushCtrl, r&^uint32(2))
	time.Sleep(10 * time.Microsecond)
	n.wr(sysRBufFlushCtrl, 0)
	time.Sleep(10 * time.Microsecond)

	n.wr(umacCmd, 0)
	n.wr(umacCmd, 1<<13|1<<15) // SW_RESET | LCL_LOOP_EN
	time.Sleep(2 * time.Microsecond)
	n.wr(umacCmd, 0)

	n.wr(umacMIBCtrl, 7)
	n.wr(umacMIBCtrl, 0)
	n.wr(umacMaxFrame, 1536)
	n.mod(rbufCtrl, 0, 1<<1) // ALIGN_2B; géén 64B-statusblocks (valkuil 7)
	n.wr(rbufTBufSizeCtrl, 1)

	n.wr(intrl2_0Set, 0xFFFFFFFF)
	n.wr(intrl2_0Clear, 0xFFFFFFFF)
	n.wr(intrl2_1Set, 0xFFFFFFFF)
	n.wr(intrl2_1Clear, 0xFFFFFFFF)

	n.wr(sysPortCtrl, 3) // PORT_MODE_EXT_GPHY
}

// MDIORead leest een clause-22 PHY-register via de interne unimac-MDIO.
func (n *Net) MDIORead(phy, reg int) uint16 {
	n.wr(umacMDIOCmd, 2<<26|uint32(phy&0x1F)<<21|uint32(reg&0x1F)<<16)
	if !n.mdioKick() { // START_BUSY bleef staan: transactie niet voltooid
		return 0xFFFF // zelfde "geen PHY / mislukte read"-sentinel als PHYScan filtert
	}
	v := n.rd(umacMDIOCmd)
	if v&(1<<28) != 0 { // READ_FAIL
		return 0xFFFF
	}
	return uint16(v)
}

// MDIOWrite schrijft een clause-22 PHY-register.
func (n *Net) MDIOWrite(phy, reg int, val uint16) {
	n.wr(umacMDIOCmd, 1<<26|uint32(phy&0x1F)<<21|uint32(reg&0x1F)<<16|uint32(val))
	n.mdioKick()
}

// mdioKick zet START_BUSY en wacht (begrensd) tot de transactie klaar is;
// geeft true als START_BUSY binnen de grens wiste, false bij een stall.
func (n *Net) mdioKick() bool {
	n.mod(umacMDIOCmd, 0, 1<<29)
	for range 100_000 { // ~25µs typisch; ruim begrensd, nooit eeuwig
		if n.rd(umacMDIOCmd)&(1<<29) == 0 {
			return true
		}
	}
	return false
}

// PHYScan zoekt PHY's op de MDIO-bus — de BCM54213PE hoort op adres 1
// (DT ethernet-phy@1), id1 0x600d, géén reset-GPIO op dit board. Gedeeld met
// gem via metal/driver/nic/mdio (zelfde PHY-chip als de Pi 5, andere MDIO-master).
func (n *Net) PHYScan() (addr int, id1, id2 uint16, found bool) {
	return mdio.Scan(n)
}

// AutoNeg start autonegotiatie en wacht op een link — zelfde logica als
// metal/driver/nic/gem, gedeeld via metal/driver/nic/mdio (zelfde PHY-chip als de Pi 5).
func (n *Net) AutoNeg(phy int, timeout time.Duration) (speed int, fd bool, err error) {
	return mdio.AutoNeg(n, phy, timeout)
}

// Init zet MAC-adres/filters, ringen en DMA klaar en schakelt de zender en
// ontvanger in. dmaBase moet buiten élke RAM-declaratie liggen (device-
// gemapt → ongecachet → coherent; de GENET is niet cache-coherent, maar zo
// is er niets te onderhouden). speed/fd komen uit AutoNeg. Reset() eerst.
func (n *Net) Init(dmaBase, dmaSize uintptr, speed int, fd bool) error {
	if need := uintptr(2 * nBD * bufSize); dmaSize < need {
		return fmt.Errorf("genet: DMA-regio %#x < %#x", dmaSize, need)
	}
	n.rxBufs = dmaBase
	n.txBufs = dmaBase + nBD*bufSize

	// MAC-adres + MDF-filter: broadcast (filter 0) en het eigen adres
	// (filter 1) — bit 16-n schakelt filter n in; geen promiscuous.
	n.wr(umacMAC0, uint32(n.MAC[0])<<24|uint32(n.MAC[1])<<16|uint32(n.MAC[2])<<8|uint32(n.MAC[3]))
	n.wr(umacMAC1, uint32(n.MAC[4])<<8|uint32(n.MAC[5]))
	n.wr(umacMDFAddr+0, 0xFFFF)
	n.wr(umacMDFAddr+4, 0xFFFFFFFF)
	n.wr(umacMDFAddr+8, uint32(n.MAC[0])<<8|uint32(n.MAC[1]))
	n.wr(umacMDFAddr+12, uint32(n.MAC[2])<<24|uint32(n.MAC[3])<<16|uint32(n.MAC[4])<<8|uint32(n.MAC[5]))
	n.wr(umacMDFCtrl, 1<<16|1<<15)

	// DMA uit + flushen vóór de ringen (Linux-volgorde).
	n.mod(tdmaCtrl, 1, 0)
	n.mod(rdmaCtrl, 1, 0)
	n.wr(umacTxFlush, 1)
	time.Sleep(10 * time.Microsecond)
	n.wr(umacTxFlush, 0)
	r := n.rd(sysRBufFlushCtrl)
	n.wr(sysRBufFlushCtrl, r|1)
	time.Sleep(10 * time.Microsecond)
	n.wr(sysRBufFlushCtrl, r)
	time.Sleep(10 * time.Microsecond)

	// RX-ring 16: elk van de 256 register-descriptors wijst vast naar zijn
	// eigen DRAM-buffer; de hardware schrijft LENGTH_STATUS per pakket.
	for i := 0; i < nBD; i++ {
		bus := uint64(n.rxBufs + uintptr(i)*bufSize)
		n.wr(rxBD+uintptr(i)*12+4, uint32(bus))
		n.wr(rxBD+uintptr(i)*12+8, uint32(bus>>32))
	}
	n.wr(rdmaBurst, 8) // BCM2711: dma_max_burst_length = 8
	n.wr(rdmaRing16+0x14, 0)
	n.wr(rdmaRing16+0x1C, nBD*3-1)
	n.wr(rdmaRing16+0x2C, 0) // READ_PTR
	n.wr(rdmaRing16+0x00, 0) // WRITE_PTR
	// RING-INDEX-INVARIANT: onze software-index (int(rxCons)%nBD) en de registers
	// die de hardware advanceert (PROD +0x08 = HW-producer, CONS +0x0C = wij)
	// MOETEN vanaf één bekende stand starten. De vorige code lijnde uit op een
	// mogelijk-niet-nul leftover-PROD (valkuil 5: bootloader/netboot) terwijl het
	// WRITE_PTR/READ_PTR hierboven op 0 werd geforceerd — als de DMA zijn volgende
	// slot uit WRITE_PTR afleidt i.p.v. PROD%256, desynchroniseert dat bij een
	// netboot/warm-reboot (PROD!=0) de DMA-schrijfpositie en onze index.
	// De DMA staat hier uit (rdmaCtrl-enable boven gewist), dus we forceren PROD
	// én CONS expliciet naar 0 — dezelfde bekende nul-stand als een SD-cold-boot
	// (waar PROD al 0 is: dat pad blijft dus ongewijzigd). Zo kunnen index en
	// register niet uiteenlopen, ongeacht wat de bootloader achterliet.
	// NOG TE VERIFIËREN op een echt netboot/warm-reboot (PROD!=0 vóór onze init).
	n.wr(rdmaRing16+0x08, 0) // PROD := 0 (incl. discard-teller in de bovenste helft, valkuil 4)
	n.wr(rdmaRing16+0x0C, 0) // CONS := 0
	n.rxCons = 0
	n.wr(rdmaRing16+0x10, nBD<<16|bufSize)
	n.wr(rdmaRing16+0x28, 5<<16|nBD>>4) // XON/XOFF
	n.wr(rdmaRingCfg, 1<<16)

	// TX-ring 16. Zelfde ring-index-invariant als RX (zie daar): CONS (+0x08) is
	// hier de HW-consumer, PROD (+0x0C) advanceren wij. Forceer beide én de
	// pointerregisters naar 0 i.p.v. uit te lijnen op een leftover-CONS; DMA
	// staat uit. NOG TE VERIFIËREN op een echt netboot/warm-reboot.
	n.wr(tdmaBurst, 8)
	n.wr(tdmaRing16+0x14, 0)
	n.wr(tdmaRing16+0x1C, nBD*3-1)
	n.wr(tdmaRing16+0x00, 0) // READ_PTR
	n.wr(tdmaRing16+0x2C, 0) // WRITE_PTR
	n.wr(tdmaRing16+0x08, 0) // CONS := 0
	n.wr(tdmaRing16+0x0C, 0) // PROD := 0
	n.txProd = 0
	n.wr(tdmaRing16+0x28, 0)
	n.wr(tdmaRing16+0x10, nBD<<16|bufSize)
	n.wr(tdmaRingCfg, 1<<16)

	// DMA aan: eerst RDMA, dan TDMA (ring 16 = enable-bit 17, valkuil 10).
	n.mod(rdmaCtrl, 0, 1<<17|1)
	n.mod(tdmaCtrl, 0, 1<<17|1)

	// RGMII: U-Boot-variant — ID_MODE_DIS=1 (PHY-strap levert de klokskew,
	// wij programmeren geen skew-registers; valkuil 8) + LINK + MODE_EN.
	n.wr(extRGMIIOOBCtrl, 1<<4|1<<6|1<<16)

	// UMAC: snelheid + duplex + aan.
	cmd := n.rd(umacCmd) &^ uint32(3<<2|1<<10)
	switch speed {
	case 1000:
		cmd |= 2 << 2
	case 100:
		cmd |= 1 << 2
	}
	if !fd {
		cmd |= 1 << 10 // HD_EN
	}
	n.wr(umacCmd, cmd|1|1<<1) // TX_EN | RX_EN
	dev.MB()
	return nil
}

// Receive haalt één frame op (0 = niets) — go-net NetworkDevice.
func (n *Net) Receive(buf []byte) (int, error) {
	if n.rd(rdmaRing16+0x08)&0xFFFF == n.rxCons {
		return 0, nil
	}
	// Ordent de PROD-check vóór het lezen van LENGTH_STATUS/de buffer, net als
	// gem/virtionet; wordt load-bearing zodra de buffers Normal-NC gemapt worden.
	dev.MB()
	i := int(n.rxCons) % nBD // descriptor-pointer, mod nBD (nBD deelt 0x10000)
	ls := n.rd(rxBD + uintptr(i)*12)
	length := int(ls >> 16 & 0xFFF)
	flags := ls & 0xFFFF

	got := 0
	// Alleen complete, foutvrije frames afleveren; ALIGN_2B zet 2 pad-bytes
	// vóór het frame (in de lengte meegeteld, valkuil 6). FCS strippen doet
	// de MAC al (CRC_FWD staat uit).
	if flags&(dmaSOP|dmaEOP) == dmaSOP|dmaEOP && flags&rxErrMask == 0 && length > 2 {
		got = length - 2
		if got > len(buf) {
			got = len(buf)
		}
		dev.CopyOut(buf[:got], n.rxBufs+uintptr(i)*bufSize+2)
	}
	n.rxCons = (n.rxCons + 1) & 0xFFFF
	n.wr(rdmaRing16+0x0C, n.rxCons)
	return got, nil
}

// Transmit verstuurt één frame — go-net NetworkDevice. De kick is een
// schrijf van de opgehoogde producer-index; voltooiing volgt via CONS.
func (n *Net) Transmit(buf []byte) error {
	if len(buf) > bufSize {
		return fmt.Errorf("genet: frame %d > %d", len(buf), bufSize)
	}
	// Ringruimte bewaken (256 in-flight max), begrensd wachten zoals gem.
	deadline := time.Now().Add(txTimeout)
	for (n.txProd-n.rd(tdmaRing16+0x08))&0xFFFF >= nBD {
		if time.Now().After(deadline) {
			return fmt.Errorf("genet: TX-ring blijft vol na %v (DMA hangt?)", txTimeout)
		}
	}
	i := int(n.txProd) % nBD // descriptor-pointer, mod nBD (nBD deelt 0x10000)
	dst := n.txBufs + uintptr(i)*bufSize
	dev.Copy(dst, buf)
	bd := txBD + uintptr(i)*12
	n.wr(bd+4, uint32(uint64(dst)))
	n.wr(bd+8, uint32(uint64(dst)>>32))
	dev.MB()
	n.wr(bd, uint32(len(buf))<<16|txQTag|txAppendCRC|dmaSOP|dmaEOP)
	n.txProd = (n.txProd + 1) & 0xFFFF
	n.wr(tdmaRing16+0x0C, n.txProd) // PROD-schrijf = de kick
	return nil
}
