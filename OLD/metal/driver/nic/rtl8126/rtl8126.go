// Package rtl8126 is HopOS' driver voor de Realtek RTL8126A 5GbE-PCIe-NIC
// (10ec:8126, Orion O6) en de RTL8125-familie 2,5GbE (10ec:8125, Orion O6N)
// — de LAN-poorten van de Radxa Orions. Geschreven naar
// de Linux r8169-driver (drivers/net/ethernet/realtek/r8169_main.c, mainline
// 09-09-2026, mac_version RTL_GIGA_MAC_VER_70) als referentie, kruislings
// getoetst aan Realteks eigen r8126-driver waar mainline zwijgt; het
// registerrecept staat regel voor regel in docs/v1/archief/rtl8126-recept.md.
//
// Vorm: polled, één RX- en één TX-ring met de klassieke 16-byte-descriptors
// (opts1/opts2/addr), geen offloads, geen interrupts (IMR 0), geen
// PHY-firmware-blob (mainline draait zonder; het blob zijn PHY-errata-
// patches, geen voorwaarde voor link of verkeer). Zelfde contract als igb/
// gem/dwmac: een netdev.Device (Receive/Transmit op rauwe frames), de
// firmware (UEFI) wees de BAR's al toe en wij lezen alleen uit.
//
// GESCHREVEN VÓÓR HET EERSTE O6N-CONTACT: elk register hieronder komt uit de
// bron, niets is gemeten. cmd/probeuefi meet het op het bord (reset → MAC →
// ringen → link → DHCP) vóór er iets op leunt.
//
// Volgorde voor de gebruiker: Reset (chip-id, hw_init, hw_reset, MAC) →
// Init (ringen + hw_start) → LinkUp (PHY aan, autoneg, wachten). Dat is de
// mainline-volgorde (probe → open → phy_start); de PHY-config hoeft niet
// vóór de MAC-config.
package rtl8126

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// PCI-identiteit.
const (
	VendorID     = 0x10ec
	DeviceID8126 = 0x8126 // RTL8126A 5GbE (Orion O6)
	DeviceID8125 = 0x8125 // RTL8125B/D/CP/BP 2.5GbE (Orion O6N)
)

// Supported meldt of (vendor, device) door deze driver gedreven wordt: de
// RTL8126A en de RTL8125-familie (behalve de oude 8125A: die heeft een lange
// eigen PHY-lijst en zit op geen Orion). Welke variant het is beslist de
// chip-XID bij Reset (variants), niet het PCI-ID.
func Supported(vendor, device uint16) bool {
	return vendor == VendorID && (device == DeviceID8126 || device == DeviceID8125)
}

// variant is wat per mac_version verschilt in het mainline-pad (recept
// §15): een handvol hw-parameters in rtl_hw_start_8125_common, de EPHY-
// tabel, de CLKREQ-bit, de PHY-config en de advertentie. Alles wat hier
// niet staat is voor de hele familie gelijk.
type variant struct {
	name       string
	ephy       []ephyEntry        // rtl_ephy_init-tabel vóór 8125_common (nil = geen)
	rxDescFmt  bool               // W8(0xd8, &^0x02): alleen VER_70/80
	e614       uint16             // mac_ocp 0xe614 mask 0x0700 → deze waarde
	e63e       uint16             // mac_ocp 0xe63e mask 0x0c30 → deze waarde
	ea1cSecond uint16             // tweede 0xea1c-modify: dit masker → 0
	mitigEnd   uintptr            // interrupt-mitigation-tabel wissen tot hier
	intCfg1    bool               // W16(0x7a, 0)
	clkreqCfg2 bool               // CLKREQ uit via Config2 bit 7 (61-66) i.p.v. INT_CFG0 bit 3 (70/80)
	phy        func(n *Net) error // variant-specifieke PHY-tweaks (ná enable_gphy_10m)
	eeePHY     bool               // rtl8168g_config_eee_phy: 0xa432 |= 0x0010 (8125-varianten)
	dash       bool               // rtl8125bp_driver_start (VER_66)
	adv        uint16             // 0xa5d4-advertentie: 0x0180 (2.5G+5G) of 0x0080 (2.5G)
}

type ephyEntry struct{ reg, mask, bits uint16 }

// variants op XID (TxConfig[31:20] & 0xfcf); r8169 rtl_chip_infos.
var variants = map[uint32]*variant{
	0x649: &v8126A, 0x64a: &v8126A,
	0x641: &v8125B,
	0x688: &v8125D, 0x689: &v8125D, 0x68a: &v8125D,
	0x708: &v8125CP,
	0x681: &v8125BP,
}

var (
	v8126A = variant{name: "RTL8126A", rxDescFmt: true, e614: 0x0400, e63e: 0x0020, ea1cSecond: 0x0300,
		mitigEnd: 0xa80, intCfg1: true, adv: 0x0180}
	v8125B = variant{name: "RTL8125B", e614: 0x0200, e63e: 0x0000, ea1cSecond: 0x0004,
		mitigEnd: 0xa80, intCfg1: true, clkreqCfg2: true, eeePHY: true, adv: 0x0080, phy: phy8125B,
		ephy: []ephyEntry{{0x0b, 0xffff, 0xa908}, {0x1e, 0xffff, 0x20eb}, {0x4b, 0xffff, 0xa908},
			{0x5e, 0xffff, 0x20eb}, {0x22, 0x0030, 0x0020}, {0x62, 0x0030, 0x0020}}}
	v8125D = variant{name: "RTL8125D", e614: 0x0300, e63e: 0x0020, ea1cSecond: 0x0004,
		mitigEnd: 0xb00, clkreqCfg2: true, eeePHY: true, adv: 0x0080}
	v8125CP = variant{name: "RTL8125CP", e614: 0x0300, e63e: 0x0020, ea1cSecond: 0x0004,
		mitigEnd: 0xb00, clkreqCfg2: true, eeePHY: true, adv: 0x0080, phy: phy8125CP}
	v8125BP = variant{name: "RTL8125BP", e614: 0x0300, e63e: 0x0020, ea1cSecond: 0x0004,
		mitigEnd: 0xb00, clkreqCfg2: true, eeePHY: true, adv: 0x0080, phy: phy8125BP, dash: true}
)

// MMIO-registers (r8169_main.c enum rtl_registers / rtl8125_registers).
const (
	regMAC0       = 0x00 // MAC-adres [3:0]
	regMAC4       = 0x04 // MAC-adres [5:4]
	regMAR0       = 0x08 // multicast-filter (2×32 bit)
	regTxDescLo   = 0x20
	regTxDescHi   = 0x24
	regIntCfg0    = 0x34 // INT_CFG0_8125
	regChipCmd    = 0x37
	regIntrMask   = 0x38 // IntrMask_8125 (32 bit)
	regIntrStatus = 0x3c // IntrStatus_8125 (32 bit)
	regTxConfig   = 0x40
	regRxConfig   = 0x44
	regCfg9346    = 0x50
	regConfig1    = 0x52
	regConfig2    = 0x53
	regConfig3    = 0x54
	regConfig5    = 0x56
	regPHYstatus  = 0x6c
	regERIDR      = 0x70 // ERI data (DASH-handshake 8125BP)
	regERIAR      = 0x74 // ERI adres/commando
	regIntCfg1    = 0x7a // INT_CFG1_8125
	regEPHYAR     = 0x80 // EPHY-toegang (8125B-tabel)
	regTxPoll     = 0x90 // TxPoll_8125 (16 bit)
	regOCPDR      = 0xb0 // MAC-OCP
	regGPHYOCP    = 0xb8 // PHY-OCP
	regMCU        = 0xd3
	regRxDescFmt  = 0xd8 // bit 1 = nieuw RX-descriptorformaat (VER_70)
	regRxMaxSize  = 0xda // 16 bit
	regCPlusCmd   = 0xe0 // 16 bit
	regIntrMitig  = 0xe2 // 16 bit
	regRxDescLo   = 0xe4
	regRxDescHi   = 0xe8
	regMISC       = 0xf0
	reg1880       = 0x1880
	reg0382       = 0x382
	regMAC0Bkp    = 0x19e0 // MAC0_BKP: het adres uit de EEPROM/eFuse
	regRSSCtrl    = 0x4500
	regQNumCtrl   = 0x4800
	regIntMitigLo = 0xa00 // interrupt-mitigation-tabel 0xa00..0xa7c/0xafc (variant)

	// ChipCmd.
	cmdReset  = 0x10
	cmdRxEnb  = 0x08
	cmdTxEnb  = 0x04
	cmdStopRq = 0x80

	// Cfg9346.
	cfgUnlock = 0xc0
	cfgLock   = 0x00

	// MCU (0xd3).
	mcuNowIsOOB   = 0x80
	mcuRxTxEmpty  = 0x30
	mcuLinkListOK = 0x02

	// MISC.
	miscRxDVGated = 1 << 19

	// RxConfig: RX_FETCH_DFLT_8125 (8<<27) | RX_DMA_BURST (7<<8) |
	// RX_PAUSE_SLOT_ON (1<<11) = 0x40000f00; accept-bits [5:0].
	rxCfgBase       = 0x40000f00
	rxAcceptMask    = 0x3f
	rxAcceptOKMask  = 0x0f
	rxAcceptMyPhys  = 0x02
	rxAcceptMcast   = 0x04
	rxAcceptBcast   = 0x08
	rxAcceptDefault = rxAcceptBcast | rxAcceptMcast | rxAcceptMyPhys

	// TxConfig: TX_DMA_BURST 7<<8 | InterFrameGap 3<<24.
	txCfg = 0x03000700

	// CPlusCmd: CPCMD_MASK (de bits die mainline bewaart).
	cpCmdMask = 0x2063

	// OCP.
	ocpFlag = 0x80000000

	// Descriptor-bits (opts1).
	descOwn   = 1 << 31
	ringEnd   = 1 << 30
	firstFrag = 1 << 29
	lastFrag  = 1 << 28
	rxRES     = 1 << 21 // receive error summary
	rxLenMask = 0x3fff

	// PHY (clause-22 via OCP 0xa400 + 2·reg).
	phyBMCR     = 0xa400
	phyBMSR     = 0xa402
	phyADV      = 0xa408
	phyCTRL1000 = 0xa412
	phyPHYSR    = 0xa434 // MII_RESV2 (0x1a): snelheid/duplex
	phyNBaseT   = 0xa5d4 // 10GBT_CTRL: 2.5G/5G-advertentie

	bmcrReset     = 0x8000
	bmcrANEnable  = 0x1000
	bmcrPowerDown = 0x0800
	bmcrIsolate   = 0x0400
	bmcrANRestart = 0x0200
	bmsrLinkUp    = 0x0004

	ethMinFrame = 60 // ETH_ZLEN: mainline padt in software (rtl_quirk_packet_padto)

	// Ringen. 2KB-buffers (boven 1522 + marge); RX 256 diep voor een 5GbE-
	// poort die tussen twee polls door 512KB moet kunnen bufferen, TX 64.
	bufSize = 2048
	nRx     = 256
	nTx     = 64
	descLen = 16

	// bufOff: frame-buffers in een eigen 2MB-blok, gescheiden van de
	// descriptors in blok 0 — het coherent/streaming-onderscheid dat igb ook
	// maakt, zodat het board de búffers Normal-WB mag mappen (uefi.MapNormal)
	// terwijl de descriptors device blijven.
	bufOff = 2 << 20

	txTimeout = 100 * time.Millisecond
)

// Net is één RTL8126A.
type Net struct {
	Base   uintptr // BAR2-registerblok (door de firmware toegewezen; ≥ 0x5000 mappen)
	BusOff uint64  // DMA-vertaling: busadres = fysiek + BusOff (0 = identiteit)
	MAC    [6]byte // uit MAC0_BKP, door Reset
	XID    uint32  // chip-id uit TxConfig, door Reset
	Name   string  // variantnaam ("RTL8126A", "RTL8125B", ...), door Reset

	v *variant

	rxRing, txRing uintptr
	rxBufs, txBufs uintptr
	rxHead, txHead int
}

func (n *Net) r8(off uintptr) uint8        { return dev.Read8(n.Base + off) }
func (n *Net) r16(off uintptr) uint16      { return dev.Read16(n.Base + off) }
func (n *Net) r32(off uintptr) uint32      { return dev.Read32(n.Base + off) }
func (n *Net) w8(off uintptr, v uint8)     { dev.Write8(n.Base+off, v) }
func (n *Net) w16(off uintptr, v uint16)   { dev.Write16(n.Base+off, v) }
func (n *Net) w32(off uintptr, v uint32)   { dev.Write32(n.Base+off, v) }
func (n *Net) set8(off uintptr, v uint8)   { n.w8(off, n.r8(off)|v) }
func (n *Net) clr8(off uintptr, v uint8)   { n.w8(off, n.r8(off)&^v) }
func (n *Net) clr16(off uintptr, v uint16) { n.w16(off, n.r16(off)&^v) }
func (n *Net) commit()                     { n.r8(regChipCmd) } // rtl_pci_commit: PCI-write posten

// macOCP: de MAC-OCP-ruimte via OCPDR (0xb0), zonder busy-vlag.
func (n *Net) macOCPWrite(reg uint16, v uint16) {
	n.w32(regOCPDR, ocpFlag|uint32(reg)<<15|uint32(v))
}

func (n *Net) macOCPRead(reg uint16) uint16 {
	n.w32(regOCPDR, uint32(reg)<<15)
	return uint16(n.r32(regOCPDR))
}

func (n *Net) macOCPModify(reg, mask, set uint16) {
	n.macOCPWrite(reg, n.macOCPRead(reg)&^mask|set)
}

// phyOCP: de PHY-OCP-ruimte via GPHY_OCP (0xb8), bit 31 = bezig (write:
// wacht tot laag; read: wacht tot hoog). Mainline wacht 25µs×10; wij ruimer.
func (n *Net) phyOCPWrite(reg uint16, v uint16) error {
	n.w32(regGPHYOCP, ocpFlag|uint32(reg)<<15|uint32(v))
	return n.waitFlag(regGPHYOCP, ocpFlag, false, 25*time.Microsecond, 100, "phy-ocp write")
}

func (n *Net) phyOCPRead(reg uint16) (uint16, error) {
	n.w32(regGPHYOCP, uint32(reg)<<15)
	if err := n.waitFlag(regGPHYOCP, ocpFlag, true, 25*time.Microsecond, 100, "phy-ocp read"); err != nil {
		return 0, err
	}
	return uint16(n.r32(regGPHYOCP)), nil
}

func (n *Net) phyOCPModify(reg, mask, set uint16) error {
	v, err := n.phyOCPRead(reg)
	if err != nil {
		return err
	}
	return n.phyOCPWrite(reg, v&^mask|set)
}

// waitFlag pollt een 32-bit-register tot (reg&mask != 0) == high, met
// bounded pogingen (rtl_loop_wait). Mainline logt een time-out en gaat door;
// wij geven hem terug — op ijzer wil je weten wélke stap hing.
func (n *Net) waitFlag(off uintptr, mask uint32, high bool, delay time.Duration, tries int, what string) error {
	for range tries {
		if (n.r32(off)&mask != 0) == high {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("rtl8126: %s timed out (reg %#x=%#x)", what, off, n.r32(off))
}

func (n *Net) wait8(off uintptr, mask, want uint8, delay time.Duration, tries int, what string) error {
	for range tries {
		if n.r8(off)&mask == want {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("rtl8126: %s timed out (reg %#x=%#x)", what, off, n.r8(off))
}

// Reset is het probe-deel van mainline: chip-id, rtl_hw_init_8125,
// rtl_hw_reset en het MAC-adres. Na Reset is het MAC bekend en de MAC-kern
// in rust; de PHY staat nog uit (LinkUp zet hem aan).
func (n *Net) Reset() error {
	tx := n.r32(regTxConfig)
	if tx == 0xffffffff {
		return fmt.Errorf("rtl8126: TxConfig reads all-ones (device off the bus?)")
	}
	n.XID = tx >> 20 & 0xfcf
	n.v = variants[n.XID]
	if n.v == nil {
		return fmt.Errorf("rtl8126: unsupported chip XID %#x (TxConfig=%#x; known: 8126A 0x649/0x64a, 8125B 0x641, 8125D 0x688-0x68a, 8125CP 0x708, 8125BP 0x681)", n.XID, tx)
	}
	n.Name = n.v.name

	// rtl_init_rxcfg + rtl8169_irq_mask_and_ack (probe-volgorde).
	n.w32(regRxConfig, rxCfgBase)
	n.irqMaskAndAck()

	if err := n.hwInit(); err != nil {
		return err
	}
	if err := n.hwReset(); err != nil {
		return err
	}

	// MAC: MAC0_BKP (0x19e0, zes bytes in draadvolgorde), terugval MAC0.
	for i := range n.MAC {
		n.MAC[i] = n.r8(regMAC0Bkp + uintptr(i))
	}
	if !validUnicast(n.MAC) {
		for i := range n.MAC {
			n.MAC[i] = n.r8(regMAC0 + uintptr(i))
		}
	}
	if !validUnicast(n.MAC) {
		return fmt.Errorf("rtl8126: no valid MAC in MAC0_BKP/MAC0 (%x)", n.MAC)
	}
	n.rarSet()
	if n.v.dash {
		n.dashStart()
	}
	return nil
}

// dashStart is rtl8125bp_driver_start: het OOB-handshake dat mainline op de
// 8125BP altijd doet (bij probe én open), via de ERI-OOB-ruimte.
func (n *Net) dashStart() {
	for _, w := range [...]struct{ reg, data uint32 }{{0x14, 0x05}, {0x18, 0x00}, {0x10, 0x01}} {
		n.w32(regERIDR, w.data)
		n.w32(regERIAR, 0x80000000|0x00020000|0x1000|w.reg)
		n.waitFlag(regERIAR, 0x80000000, false, 100*time.Microsecond, 100, "eri write")
	}
}

// ephyWrite/ephyRead: de EPHY-ruimte via EPHYAR (0x80). Registermasker 0x1f
// zoals mainline (EPHYAR_REG_MASK) — de tabellen dragen offsets ≥ 0x20 die
// daarmee op reg&0x1f landen; wij reproduceren mainline letterlijk.
func (n *Net) ephyWrite(reg, v uint16) {
	n.w32(regEPHYAR, 0x80000000|uint32(v)|uint32(reg&0x1f)<<16)
	n.waitFlag(regEPHYAR, 0x80000000, false, 10*time.Microsecond, 100, "ephy write")
	time.Sleep(10 * time.Microsecond)
}

func (n *Net) ephyRead(reg uint16) uint16 {
	n.w32(regEPHYAR, uint32(reg&0x1f)<<16)
	if n.waitFlag(regEPHYAR, 0x80000000, true, 10*time.Microsecond, 100, "ephy read") != nil {
		return 0xffff
	}
	return uint16(n.r32(regEPHYAR))
}

func validUnicast(m [6]byte) bool {
	if m[0]&1 != 0 {
		return false
	}
	var or byte
	for _, b := range m {
		or |= b
	}
	return or != 0
}

// irqMaskAndAck: IMR 0, ISR alles wissen, posten.
func (n *Net) irqMaskAndAck() {
	n.w32(regIntrMask, 0)
	n.w32(regIntrStatus, 0xffffffff)
	n.commit()
}

// enableRxDVGate: RX-datapad dichtzetten en wachten tot de FIFO's leeg zijn
// (rtl_enable_rxdvgate + rtl_wait_txrx_fifo_empty, VER_63+-tak).
func (n *Net) enableRxDVGate() error {
	n.w32(regMISC, n.r32(regMISC)|miscRxDVGated)
	time.Sleep(2 * time.Millisecond)
	n.set8(regChipCmd, cmdStopRq)
	if err := n.wait8(regMCU, mcuRxTxEmpty, mcuRxTxEmpty, 100*time.Microsecond, 42, "rx/tx fifo empty"); err != nil {
		return err
	}
	for range 42 {
		if n.r16(regIntrMitig)&0x0103 == 0x0103 {
			return nil
		}
		time.Sleep(100 * time.Microsecond)
	}
	return fmt.Errorf("rtl8126: fifo-empty (0xe2) timed out (=%#x)", n.r16(regIntrMitig))
}

// hwInit is rtl_hw_init_8125: één keer bij probe, vóór de eerste CmdReset —
// uit OOB-modus, link-list klaar.
func (n *Net) hwInit() error {
	if err := n.enableRxDVGate(); err != nil {
		return err
	}
	n.clr8(regChipCmd, cmdTxEnb|cmdRxEnb)
	time.Sleep(time.Millisecond)
	n.clr8(regMCU, mcuNowIsOOB)
	n.macOCPModify(0xe8de, 1<<14, 0)
	if err := n.wait8(regMCU, mcuLinkListOK, mcuLinkListOK, 100*time.Microsecond, 42, "link list ready (1)"); err != nil {
		return err
	}
	n.macOCPWrite(0xc0aa, 0x07d0)
	n.macOCPWrite(0xc0a6, 0x0150)
	n.macOCPWrite(0xc01e, 0x5555)
	return n.wait8(regMCU, mcuLinkListOK, mcuLinkListOK, 100*time.Microsecond, 42, "link list ready (2)")
}

// hwReset is rtl_hw_reset: CmdReset en wachten tot hij zichzelf wist.
func (n *Net) hwReset() error {
	n.w8(regChipCmd, cmdReset)
	return n.wait8(regChipCmd, cmdReset, 0, 100*time.Microsecond, 100, "chip reset")
}

// rarSet programmeert het MAC-adres (rtl_rar_set): MAC4 vóór MAC0, onder
// Cfg9346-unlock, elk gepost.
func (n *Net) rarSet() {
	n.w8(regCfg9346, cfgUnlock)
	n.w32(regMAC4, uint32(n.MAC[4])|uint32(n.MAC[5])<<8)
	n.commit()
	n.w32(regMAC0, uint32(n.MAC[0])|uint32(n.MAC[1])<<8|uint32(n.MAC[2])<<16|uint32(n.MAC[3])<<24)
	n.commit()
	n.w8(regCfg9346, cfgLock)
}

// Init zet de ringen klaar in de DMA-regio (device-gemapt → ongecachet →
// coherent zonder cache-onderhoud, de HopOS-conventie), doet mainline's
// rtl8169_cleanup en dan rtl_hw_start: de MAC staat daarna aan met RX/TX
// enabled en IMR 0. Reset moet al gedaan zijn.
func (n *Net) Init(dmaBase, dmaSize uintptr) error {
	need := uintptr(bufOff + (nRx+nTx)*bufSize)
	if dmaSize < need {
		return fmt.Errorf("rtl8126: DMA region %#x < %#x", dmaSize, need)
	}
	if dmaBase&(bufOff-1) != 0 {
		return fmt.Errorf("rtl8126: DMA base %#x not 2MB-aligned", dmaBase)
	}
	// Ringen 256-byte-uitgelijnd (eis van het silicium); 4KB-stap houdt ze
	// op eigen pagina's.
	n.rxRing = dmaBase
	n.txRing = dmaBase + 0x1000
	n.rxBufs = dmaBase + bufOff
	n.txBufs = n.rxBufs + nRx*bufSize

	for i := 0; i < nRx; i++ {
		n.armRx(i)
	}
	for i := 0; i < nTx*descLen; i += 4 {
		dev.Write32(n.txRing+uintptr(i), 0)
	}
	dev.Write32(n.txRing+uintptr(nTx-1)*descLen, ringEnd) // laatste TX-descriptor: RingEnd
	dev.MB()

	// rtl8169_cleanup (VER_40+): irq dicht, RX-accept uit, rxdvgate + FIFO
	// leeg, 2ms, CmdReset — mainline doet dit opnieuw vlak vóór hw_start.
	n.irqMaskAndAck()
	n.w32(regRxConfig, n.r32(regRxConfig)&^rxAcceptMask)
	if err := n.enableRxDVGate(); err != nil {
		return err
	}
	time.Sleep(2 * time.Millisecond)
	if err := n.hwReset(); err != nil {
		return err
	}
	return n.hwStart()
}

// hwStart is rtl_hw_start voor VER_70 (rtl_hw_start_8125 → rtl_hw_start_8126a
// → rtl_hw_start_8125_common), zonder ASPM/LTR/EEE-timer/tally (PCIe-
// powermanagement en statistiek: optioneel volgens de bron). De OCP-
// "hw-parameters" van Realtek schrijft mainline onvoorwaardelijk; die nemen
// we integraal over tot bewezen is dat er zonder kan.
func (n *Net) hwStart() error {
	n.w8(regCfg9346, cfgUnlock)

	// rtl_hw_aspm_clkreq_enable(false): ASPM/CLKREQ uit.
	n.macOCPModify(0xe092, 0x00ff, 0)
	if n.v.clkreqCfg2 {
		n.clr8(regConfig2, 1<<7)
	} else {
		n.clr8(regIntCfg0, 1<<3)
	}
	n.clr8(regConfig5, 1<<0)

	n.w16(regCPlusCmd, n.r16(regCPlusCmd)&cpCmdMask) // geen RxChkSum: geen offloads

	// rtl_hw_start_8125: legacy ISR/IMR-layout, interrupt-mitigation uit.
	n.w8(regIntCfg0, 0)
	for off := uintptr(regIntMitigLo); off < n.v.mitigEnd; off += 4 {
		n.w32(off, 0)
	}
	if n.v.intCfg1 {
		n.w16(regIntCfg1, 0)
	}

	// rtl_hw_config: de EPHY-tabel van de variant (8125B), dan 8125_common.
	for _, e := range n.v.ephy {
		n.ephyWrite(e.reg, n.ephyRead(e.reg)&^e.mask|e.bits)
	}

	// rtl_hw_start_8125_common, met de variant-waarden op de vier lijnen
	// die per mac_version verschillen (recept §15c).
	n.clr8(regConfig3, 0x02) // rtl_pcie_state_l2l3_disable
	n.w16(reg0382, 0x221b)
	n.w32(regRSSCtrl, 0)
	n.w16(regQNumCtrl, 0)
	n.macOCPModify(0xd40a, 0x0010, 0) // UPS uit
	n.clr8(regConfig1, 0x10)
	n.macOCPWrite(0xc140, 0xffff)
	n.macOCPWrite(0xc142, 0xffff)
	n.macOCPModify(0xd3e2, 0x0fff, 0x03a9)
	n.macOCPModify(0xd3e4, 0x00ff, 0x0000)
	n.macOCPModify(0xe860, 0x0000, 0x0080)
	n.macOCPModify(0xeb58, 0x0001, 0x0000) // klassiek TX-descriptorformaat (verplicht)
	if n.v.rxDescFmt {
		n.clr8(regRxDescFmt, 0x02) // klassiek RX-descriptorformaat (verplicht, VER_70/80)
	}
	n.macOCPModify(0xe614, 0x0700, n.v.e614)
	n.macOCPModify(0xe63e, 0x0c30, n.v.e63e)
	n.macOCPModify(0xc0b4, 0x0000, 0x000c)
	n.macOCPModify(0xeb6a, 0x00ff, 0x0033)
	n.macOCPModify(0xeb50, 0x03e0, 0x0040)
	n.macOCPModify(0xe056, 0x00f0, 0x0000)
	n.macOCPModify(0xe040, 0x1000, 0x0000)
	n.macOCPModify(0xea1c, 0x0003, 0x0001)
	n.macOCPModify(0xea1c, n.v.ea1cSecond, 0x0000)
	n.macOCPModify(0xe0c0, 0x4f0f, 0x4403)
	n.macOCPModify(0xe052, 0x0080, 0x0068)
	n.macOCPModify(0xd430, 0x0fff, 0x047f)
	n.macOCPModify(0xea1c, 0x0004, 0x0000)
	n.macOCPModify(0xeb54, 0x0000, 0x0001) // TCAM wissen
	time.Sleep(time.Microsecond)
	n.macOCPModify(0xeb54, 0x0001, 0x0000)
	n.clr16(reg1880, 0x0030)
	n.macOCPWrite(0xe098, 0xc302)
	for i := 0; ; i++ {
		if n.macOCPRead(0xe00e)&(1<<13) == 0 {
			break
		}
		if i == 10 {
			return fmt.Errorf("rtl8126: mac-ocp 0xe00e bit 13 stuck (=%#x)", n.macOCPRead(0xe00e))
		}
		time.Sleep(time.Millisecond)
	}
	n.w32(regMISC, n.r32(regMISC)&^miscRxDVGated) // rtl_disable_rxdvgate: RX open (verplicht)

	// Ringen en maten; High vóór Low (mainline-commentaar).
	n.w16(regRxMaxSize, bufSize) // frames groter dan onze buffer weigert de MAC
	rxBus := uint64(n.rxRing) + n.BusOff
	txBus := uint64(n.txRing) + n.BusOff
	n.w32(regTxDescHi, uint32(txBus>>32))
	n.w32(regTxDescLo, uint32(txBus))
	n.w32(regRxDescHi, uint32(rxBus>>32))
	n.w32(regRxDescLo, uint32(rxBus))
	n.w8(regCfg9346, cfgLock)
	n.commit()
	n.w8(regChipCmd, cmdTxEnb|cmdRxEnb)

	n.w32(regRxConfig, rxCfgBase)
	n.w32(regTxConfig, txCfg)
	// rtl_set_rx_mode: alle multicast, plus broadcast + eigen adres.
	n.w32(regMAR0+4, 0xffffffff)
	n.w32(regMAR0, 0xffffffff)
	n.w32(regRxConfig, n.r32(regRxConfig)&^rxAcceptOKMask|rxAcceptDefault)
	n.w32(regIntrMask, 0) // polled
	dev.MB()
	return nil
}

// LinkUp zet de PHY aan (BMCR.PDOWN weg), doet de VER_70-PHY-config van
// mainline (rtl8126a_hw_phy_config zonder firmware-blob), een soft-reset,
// adverteert 10/100/1000 + 2.5G/5G en herstart autonegotiatie; dan wachten op
// BMSR.LinkUp. Geeft (Mbps, full-duplex).
func (n *Net) LinkUp(timeout time.Duration) (speed int, fd bool, err error) {
	// genphy_resume: power-down eraf, 20ms (rtlgen_resume).
	if err := n.phyOCPModify(phyBMCR, bmcrPowerDown, 0); err != nil {
		return 0, false, err
	}
	time.Sleep(20 * time.Millisecond)

	// rtl81xx_hw_phy_config zonder firmware-blob: 10M-gphy aan, de variant-
	// tweaks, legacy force mode (clause 22), ALDPS uit, EEE-PHY-bits uit.
	if err := n.phyOCPModify(0xa442, 0, 1<<11); err != nil { // rtl8168g_enable_gphy_10m
		return 0, false, err
	}
	if n.v.phy != nil {
		if err := n.v.phy(n); err != nil {
			return 0, false, err
		}
	}
	tweaks := []struct{ reg, mask, set uint16 }{
		{0xa5b4, 1 << 15, 0}, // rtl8125_legacy_force_mode
		{0xa430, 1 << 2, 0},  // rtl8168g_disable_aldps
		{0xa6d8, 0x0010, 0},  // rtl8125_common_config_eee_phy ×3
		{0xa428, 0x0080, 0},
		{0xa4a2, 0x0200, 0},
	}
	if n.v.eeePHY {
		tweaks = append(tweaks, struct{ reg, mask, set uint16 }{0xa432, 0, 0x0010}) // rtl8168g_config_eee_phy
	}
	for _, m := range tweaks {
		if err := n.phyOCPModify(m.reg, m.mask, m.set); err != nil {
			return 0, false, err
		}
	}

	// genphy_soft_reset: RESET|ANRESTART, wachten tot bit 15 zakt (≤600ms).
	bmcr, err := n.phyOCPRead(phyBMCR)
	if err != nil {
		return 0, false, err
	}
	if err := n.phyOCPWrite(phyBMCR, bmcr&^bmcrIsolate|bmcrReset|bmcrANRestart); err != nil {
		return 0, false, err
	}
	for i := 0; ; i++ {
		v, err := n.phyOCPRead(phyBMCR)
		if err != nil {
			return 0, false, err
		}
		if v&bmcrReset == 0 {
			break
		}
		if i == 60 {
			return 0, false, fmt.Errorf("rtl8126: PHY soft reset stuck (BMCR=%#x)", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(time.Millisecond)

	// rtl822x_config_aneg + genphy_restart_aneg: alle advertenties expliciet
	// (mainline leunt niet op power-on-defaults), dan AN aan + herstart.
	if err := n.phyOCPModify(phyNBaseT, 0x1180, n.v.adv); err != nil { // 2.5G (+5G op de 8126), geen 10G
		return 0, false, err
	}
	if err := n.phyOCPModify(phyADV, 0x0de0, 0x0de0); err != nil { // 10/100 HD+FD, pause
		return 0, false, err
	}
	if err := n.phyOCPModify(phyCTRL1000, 0x0300, 0x0200); err != nil { // 1000 FD
		return 0, false, err
	}
	bmcr, err = n.phyOCPRead(phyBMCR)
	if err != nil {
		return 0, false, err
	}
	if err := n.phyOCPWrite(phyBMCR, bmcr&^bmcrIsolate|bmcrANEnable|bmcrANRestart); err != nil {
		return 0, false, err
	}

	// Link: BMSR is latched-low, dus twee keer lezen (genphy_update_link).
	deadline := time.Now().Add(timeout)
	for {
		n.phyOCPRead(phyBMSR)
		bmsr, err := n.phyOCPRead(phyBMSR)
		if err != nil {
			return 0, false, err
		}
		if bmsr&bmsrLinkUp != 0 {
			break
		}
		if time.Now().After(deadline) {
			return 0, false, fmt.Errorf("rtl8126: no link within %v (cable? BMSR=%#x PHYstatus=%#x)", timeout, bmsr, n.r32(regPHYstatus))
		}
		time.Sleep(50 * time.Millisecond)
	}
	physr, err := n.phyOCPRead(phyPHYSR)
	if err != nil {
		return 0, false, err
	}
	return decodePHYSR(physr)
}

// decodePHYSR: rtlgen_read_status — bit 3 = full duplex; snelheid uit bits
// [5:4] en [10:9].
func decodePHYSR(v uint16) (int, bool, error) {
	fd := v&(1<<3) != 0
	switch v & 0x0630 {
	case 0x0000:
		return 10, fd, nil
	case 0x0010:
		return 100, fd, nil
	case 0x0020:
		return 1000, fd, nil
	case 0x0210:
		return 2500, fd, nil
	case 0x0220:
		return 5000, fd, nil
	case 0x0200:
		return 10000, fd, nil
	}
	return 0, fd, fmt.Errorf("rtl8126: unknown PHYSR speed code %#x", v)
}

// Interrupt-bits van IntrMask/IntrStatus (32 bit op de 8125-familie; de lage
// 16 zijn de klassieke): wat de RX-lus wekt.
const (
	intRxOK       = 0x0001
	intRxErr      = 0x0002
	intRxOverflow = 0x0010
	intLinkChg    = 0x0020
	intRxFIFOOver = 0x0040
	intRX         = intRxOK | intRxErr | intRxOverflow | intLinkChg | intRxFIFOOver
)

// EnableIRQ laat de NIC zijn INTx-lijn asserteren op RX-werk (en link-
// wissel): IMR = de RX-set. Aanroepen ná Init en nadat de lijn bij de GIC
// scherp staat; zonder EnableIRQ blijft IMR 0 en is de driver puur polled.
func (n *Net) EnableIRQ() {
	n.w32(regIntrStatus, 0xffffffff) // oude latches weg
	n.w32(regIntrMask, intRX)
	n.commit()
}

// AckIRQ laat de lijn los: eerst het masker dicht (IMR = 0), dan ISR in één
// keer schoon (write-1-to-clear van álle bits, rtl8169_irq_mask_and_ack).
// Alleen acken is niet genoeg — dat is wat r8169 in zijn harde IRQ óók doet
// (rtl_irq_disable, dan NAPI, dan rtl_irq_enable): zolang de ring vol staat
// latcht RxDescUnavail direct ná elke ack opnieuw, en op één core kwam de
// pomp dan nooit aan de beurt om te legen (O6N 17/18-09: "INTID 477 keeps
// asserting after 256 acks in one pass"). Met het masker dicht valt INTx hoe
// dan ook; RearmIRQ (vanuit WaitNIC, ná het pompen) zet hem weer open.
//
// Álle bits, niet alleen de gelezen: met "schrijf terug wat je las" bleef op
// de O6N een storm van lege interrupts over (36.000/s, één frame per ~340
// claims, rtt p50 1 ms i.p.v. 160 µs); wat er precies achterbleef is niet
// vastgesteld — de W1C van 0xffffffff maakte het weg (18-09, bundel 44:
// 1.813 claims in 40 s mét een rtt-run, rtt p50 164 µs). Events die zo
// verdwijnen zijn geen verlies: de pomp leest de ring, niet de ISR-bits, en
// de overflow-latches wist Receive zelf zodra de ring leeg is.
func (n *Net) AckIRQ() {
	n.w32(regIntrMask, 0)
	n.w32(regIntrStatus, 0xffffffff)
	n.commit()
}

// RearmIRQ: ISR schoon en dán het masker open (IMR = de RX-set) — dezelfde
// volgorde als EnableIRQ. Eerst stond hier "open zonder ISR aan te raken":
// een bit dat tijdens het gesloten masker latchte zou de lijn bij het openen
// vanzelf trekken. Dat doet deze chip NIET: de interrupt is een flank op het
// zetten van een ISR-bit onder een open masker, en een bit dat al stond
// (frame tijdens de pomp-ronde, door de pomp gewoon gelezen) maakt daarna
// geen flank meer — elk volgend frame wachtte op de failsafe van 10 ms
// (O6N, bundels 47/48, 20-09: cyclus 21 ms, 45 MB/s, tegen 1,5 ms en 118
// MB/s met de rearm vlak ná de ack). Het frame dat tussen deze W1C en het
// openen valt vangt de pomp met zijn extra ronde ná RearmIRQ (hopnet).
func (n *Net) RearmIRQ() {
	n.w32(regIntrStatus, 0xffffffff)
	n.w32(regIntrMask, intRX)
	n.commit()
}

// IRQStatus geeft de ruwe IntrStatus (diagnose: staat er iets te wachten?).
func (n *Net) IRQStatus() uint32 { return n.r32(regIntrStatus) }

// LinkStatus leest de MAC-kant (PHYstatus 0x6c bit 1): goedkoop en zonder
// PHY-OCP-verkeer — voor een statusregel, niet voor de link-wacht.
func (n *Net) LinkStatus() bool { return n.r32(regPHYstatus)&0x02 != 0 }

// BufRegion geeft het frame-bufferbereik (basis, grootte in hele 2MB-blokken)
// ná Init — wat het board desgewenst Normal-WB mapt (uefi.MapNormal). De
// descriptor-ringen vallen er bewust buiten (die blijven device).
func (n *Net) BufRegion() (base, size uintptr) {
	const blk = 2 << 20
	total := uintptr((nRx + nTx) * bufSize)
	return n.rxBufs, (total + blk - 1) &^ (blk - 1)
}

// armRx geeft RX-descriptor i (terug) aan de hardware: adres, opts2 0, en als
// laatste opts1 = Own | RingEnd(laatste) | buffergrootte (rtl8169_mark_to_asic).
func (n *Net) armRx(i int) {
	d := n.rxRing + uintptr(i)*descLen
	bus := uint64(n.rxBufs+uintptr(i)*bufSize) + n.BusOff
	dev.Write32(d+8, uint32(bus))
	dev.Write32(d+12, uint32(bus>>32))
	dev.Write32(d+4, 0)
	dev.MB()
	opts1 := uint32(descOwn | bufSize)
	if i == nRx-1 {
		opts1 |= ringEnd
	}
	dev.Write32(d, opts1)
}

// Receive haalt één frame op (0 = niets) — netdev.Device.
func (n *Net) Receive(buf []byte) (int, error) {
	d := n.rxRing + uintptr(n.rxHead)*descLen
	opts1 := dev.Read32(d)
	if opts1&descOwn != 0 {
		// Polling must also clear RX overflow latches after the ring drains.
		// W1C only observed overflow bits; preserve errors and other events.
		if pending := n.r32(regIntrStatus) & (intRxOverflow | intRxFIFOOver); pending != 0 {
			n.w32(regIntrStatus, pending)
			n.commit()
		}
		return 0, nil
	}
	dev.MB()
	length := 0
	if opts1&rxRES == 0 && opts1&(firstFrag|lastFrag) == firstFrag|lastFrag {
		length = int(opts1&rxLenMask) - 4 // FCS zit in de lengte
		if length < 0 || length > bufSize {
			length = 0 // device length must fit its DMA buffer
		}
	}
	if length > len(buf) {
		length = len(buf)
	}
	if length > 0 {
		src := n.rxBufs + uintptr(n.rxHead)*bufSize
		// Cache-hygiëne vóór de lees (igb-conventie): op een Normal-WB-
		// gemapte buffer maakt dít de read cache-snel; op device onschadelijk.
		dev.CleanInv(src, uintptr(length))
		dev.CopyOut(buf[:length], src)
	}
	n.armRx(n.rxHead) // geen RX-doorbell: de MAC pollt de ownership-bit zelf
	dev.MB()
	n.rxHead = (n.rxHead + 1) % nRx
	return length, nil
}

// Transmit verstuurt één frame (wacht begrensd op een vrije descriptor) —
// netdev.Device. Frames korter dan 60 bytes worden in software gepad
// (mainline: rtl_quirk_packet_padto voor VER_61+).
func (n *Net) Transmit(buf []byte) error {
	if len(buf) > bufSize {
		return fmt.Errorf("rtl8126: frame %d > %d", len(buf), bufSize)
	}
	d := n.txRing + uintptr(n.txHead)*descLen
	deadline := time.Now().Add(txTimeout)
	for dev.Read32(d)&descOwn != 0 {
		if time.Now().After(deadline) {
			// Verloren doorbell ("TxPoll requests are lost when the Tx
			// packets are too close", mainline rtl_tx): nog één keer bellen
			// vóór we opgeven is goedkoper dan een vals "DMA stuck".
			n.w16(regTxPoll, 1)
			if dev.Read32(d)&descOwn != 0 {
				return fmt.Errorf("rtl8126: TX descriptor %d still owned by the NIC after %v", n.txHead, txTimeout)
			}
		}
	}

	dst := n.txBufs + uintptr(n.txHead)*bufSize
	bus := uint64(dst) + n.BusOff
	length := len(buf)
	dev.Copy(dst, buf)
	if length < ethMinFrame {
		dev.Clear(dst+uintptr(length), uint64(ethMinFrame-length))
		length = ethMinFrame
	}
	dev.CleanInv(dst, uintptr(length))
	dev.Write32(d+8, uint32(bus))
	dev.Write32(d+12, uint32(bus>>32))
	dev.Write32(d+4, 0)
	dev.MB()
	opts1 := uint32(descOwn | firstFrag | lastFrag | length)
	if n.txHead == nTx-1 {
		opts1 |= ringEnd
	}
	dev.Write32(d, opts1)
	dev.MB()
	n.txHead = (n.txHead + 1) % nTx
	n.w16(regTxPoll, 1) // doorbell: TxPoll_8125 bit 0 = queue 0
	return nil
}

// phyParamG is r8168g_phy_param: page 0xa43 reg 0x13 (0xa436) = parm, dan
// reg 0x14 (0xa438) modify.
func (n *Net) phyParamG(parm, mask, val uint16) error {
	if err := n.phyOCPWrite(0xa436, parm); err != nil {
		return err
	}
	return n.phyOCPModify(0xa438, mask, val)
}

// phyParam8125 is rtl8125_phy_param: MMD VEND2 0xb87c = parm, 0xb87e modify.
func (n *Net) phyParam8125(parm, mask, val uint16) error {
	if err := n.phyOCPWrite(0xb87c, parm); err != nil {
		return err
	}
	return n.phyOCPModify(0xb87e, mask, val)
}

// phySteps voert een lijst PHY-tweaks uit en stopt bij de eerste fout.
func phySteps(steps []func() error) error {
	for _, f := range steps {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// phy8125B is rtl8125b_hw_phy_config (recept §15f) zonder firmware.
func phy8125B(n *Net) error {
	steps := []func() error{
		func() error { return n.phyOCPModify(0xac46, 0x00f0, 0x0090) },
		func() error { return n.phyOCPModify(0xad30, 0x0003, 0x0001) },
		func() error { return n.phyParam8125(0x80f5, 0xffff, 0x760e) },
		func() error { return n.phyParam8125(0x8107, 0xffff, 0x360e) },
		func() error { return n.phyParam8125(0x8551, 0xff00, 0x0800) },
		func() error { return n.phyOCPModify(0xbf00, 0xe000, 0xa000) },
		func() error { return n.phyOCPModify(0xbf46, 0x0f00, 0x0300) },
	}
	for parm := uint16(0x8044); parm <= 0x807a; parm += 6 {
		p := parm
		steps = append(steps, func() error { return n.phyParamG(p, 0xffff, 0x2417) })
	}
	steps = append(steps,
		func() error { return n.phyOCPModify(0xa4ca, 0, 0x0040) },
		func() error { return n.phyOCPModify(0xbf84, 0xe000, 0xa000) },
	)
	return phySteps(steps)
}

// phy8125CP is rtl8125cp_hw_phy_config zonder firmware.
func phy8125CP(n *Net) error {
	return phySteps([]func() error{
		func() error { return n.phyOCPModify(0xad0e, 0x007f, 0x000b) },
		func() error { return n.phyOCPModify(0xad78, 0, 1<<4) },
		func() error { return n.phyParam8125(0x807f, 0xff00, 0x5300) },
		func() error { return n.phyParamG(0x81b8, 0xffff, 0x00b4) },
		func() error { return n.phyParamG(0x81ba, 0xffff, 0x00e4) },
		func() error { return n.phyParamG(0x81c5, 0xffff, 0x0104) },
		func() error { return n.phyParamG(0x81d0, 0xffff, 0x054d) },
		func() error { return n.phyOCPModify(0xa430, 0, 0x0003) },
		func() error { return n.phyOCPModify(0xa442, 0, 1<<7) },
	})
}

// phy8125BP is rtl8125bp_hw_phy_config zonder firmware.
func phy8125BP(n *Net) error {
	return phySteps([]func() error{
		func() error { return n.phyParamG(0x8010, 0x0800, 0) },
		func() error { return n.phyParam8125(0x8088, 0xff00, 0x9000) },
		func() error { return n.phyParam8125(0x808f, 0xff00, 0x9000) },
		func() error { return n.phyParamG(0x8174, 0x2000, 0x1800) },
	})
}
