//go:build tamago && arm64

package apple

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De PCIe-kant van dit board: wat m1n1 op t8132 niet doet, en wat op elk ander
// HopOS-board de firmware al gedaan had.
//
// GEMETEN 29-08 (alles via de m1n1-proxy, zie docs/archief/apple-m4.md): m1n1
// brengt de controller en de poorten op — klok aan, poort READY — maar de LINK
// komt niet up. Twee stappen ontbreken, en allebei staan ze in Linux'
// pcie-apple.c: **PERST# loopt op dit silicium over een AP-GPIO** (de ADT zegt
// het letterlijk: function-perst = GPIO 165 voor de ethernet-poort) en de
// link-training moet expliciet gestart worden met PORT_LTSSMCTL. Met die twee
// erbij is de link binnen 50ms up en verschijnt 02:00.0 14e4:1682 — de
// Broadcom 57762.
//
// Wat daarna nog van ons is: busnummers en BAR's toewijzen (niemand deed dat),
// en de DART van deze poort in bypass zetten zodat DMA-adressen gewoon fysieke
// adressen zijn. Dat laatste is dezelfde vertrouwensafspraak als op elk ander
// HopOS-board: de NIC is HOP's device (apps praten via de interne switch, nooit
// rechtstreeks), dus een IOMMU tussen HOP en zijn eigen NIC koopt niets wat de
// stage-2-kooi niet al levert — en de Pi's en de Radxa hebben er helemaal geen.
const (
	// ECAM en de MMIO-vensters uit de ADT-ranges van /arm-io/apcie. Let op het
	// verschil: het 32-bit venster vertaalt (PCI 0x8000_0000 ↔ CPU
	// 0xB_8000_0000), het 64-bit prefetch-venster is 1-op-1. Wij wijzen uit het
	// tweede toe, dan is een BAR-waarde meteen het CPU-adres.
	ECAMBase = 0x1cb0000000
	MMIOBase = 0xB_C000_0000 // 64-bit prefetch, 512MB, PCI-adres == CPU-adres

	// De ethernet-poort (ADT /arm-io/apcie/pci-bridge2, "lan-1gb"): poortregs
	// (reg[7+8*2] van de apcie-node), zijn PERST-GPIO en zijn DART.
	ethPortBase = 0x492028000
	EthPortDev  = 2   // device-nummer van de rootpoort op bus 0
	ethPerstPin = 165 // ADT function-perst
	ethDART     = 0x492000000

	// GPIO-controller /arm-io/gpio0 (gpio,t8101): één 32-bit register per pin.
	// Layout uit Linux' pinctrl-apple-gpio.c: DATA bit 0, MODE bits 3:1 (1 =
	// output), PERIPH bits 6:5, GRP bits 18:16. Alleen de onderste vier bits
	// aanraken — de rest is de pinconfiguratie die iBoot zette, en die wissen
	// maakt de pin dood (gemeten: 0x74a02 → 0x2 en de pin nam geen waarde meer
	// aan tot de oude inhoud teruggeschreven werd).
	gpioBase = 0x39a000000

	// Poortregisters (Linux pcie-apple.c; m1n1 gebruikt dezelfde offsets).
	portLTSSMCTL = 0x080 // bit 0 = START (link training)
	portLINKSTS  = 0x208 // bit 0 = UP, bit 2 = BUSY
	portAPPCLK   = 0x800 // bit 0 = EN
	portSTATUS   = 0x804 // bit 0 = READY
	portPERST    = 0x814 // bit 0 = PERST_OFF

	// DART (dart,t8110) — Linux apple-dart.c: per stream een TCR.
	dartTCR        = 0x1000 // + 4*sid
	dartBypassDART = 1 << 1
	// De tweede bypass-bit. Achter de DART zit nóg een zeef: de DAPF, Apple's
	// adres-filter (m1n1 src/dapf.c). Die laat alleen de vensters door die de
	// firmware in zijn tabel zette, dropt de rest zónder in het DART-foutregister
	// te landen — en ons RAM op 1TiB staat daar niet in. m1n1 zet beide bits
	// samen als hij een DART uitzet (dart.c: tcr_disabled).
	dartBypassDAPF = 1 << 2
	dartStreams    = 16
	dartError      = 0x100
	dartErrAddrLo  = 0x170
	dartErrAddrHi  = 0x174

	// RID2SID: welke DART-stream hoort bij welke PCI-RID. Op t602x/t8132 ligt
	// die tabel op 0x3000 (Linux: PORT_T602X_RID2SID), niet op de 0x828 van de
	// oudere chips. m1n1 nult hem voor t8132 en zet er nooit iets in — zonder
	// mapping weet de poort niet welke stream de NIC's DMA moet dragen.
	portRID2SID     = 0x3000 // + 4*idx
	rid2sidValid    = 1 << 31
	rid2sidSIDShift = 16
)

// gpioSet zet een pin als output op waarde v (read-modify-write: alleen DATA en
// MODE, de rest van de pinconfig blijft staan).
func gpioSet(pin int, v int) {
	a := uintptr(gpioBase) + uintptr(pin)*4
	cur := dev.Read32(a)
	set := cur&^0xF | 1<<1 // MODE = output
	if v != 0 {
		set |= 1
	}
	dev.Write32(a, set)
	dev.MB()
}

// LinkUp brengt de link van de ethernet-poort op en meldt of dat lukte. De
// volgorde is die van Linux' apple_pcie_setup_port; de tijden komen uit de ADT
// (t-refclk-to-perst en perst-to-config, beide 100ms).
func LinkUp(timeout time.Duration) error {
	b := uintptr(ethPortBase)
	if dev.Read32(b+portLINKSTS)&1 != 0 {
		return nil // al up (herhaalde aanroep)
	}
	dev.Write32(b+portAPPCLK, dev.Read32(b+portAPPCLK)|1) // referentieklok aan
	dev.MB()
	gpioSet(ethPerstPin, 0) // PERST# asserted (actief laag)
	time.Sleep(10 * time.Millisecond)
	dev.Write32(b+portPERST, dev.Read32(b+portPERST)|1) // register-PERST vrij
	dev.MB()
	gpioSet(ethPerstPin, 1) // PERST# released
	time.Sleep(100 * time.Millisecond)

	if dev.Read32(b+portSTATUS)&1 == 0 {
		return fmt.Errorf("apcie: port not ready (STATUS %#x)", dev.Read32(b+portSTATUS))
	}
	dev.Write32(b+portLTSSMCTL, 1) // link training starten
	dev.MB()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dev.Read32(b+portLINKSTS)&1 != 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("apcie: link did not come up (LINKSTS %#x LTSSM %#x)",
		dev.Read32(b+portLINKSTS), dev.Read32(b+portLTSSMCTL))
}

// DARTBypass zet alle streams van de ethernet-DART volledig op bypass — DART
// én DAPF, want half bypassen is niet bypassen: de DAPF houdt DMA tegen die
// buiten zijn vensters valt. DMA-adressen zijn daarna fysieke adressen. Alle streams, want welke stream-ID de poort aan de RID
// van de NIC hangt is niet gegarandeerd (PORT_RID2SID kan per firmware-versie
// anders liggen) — en bypass op een ongebruikte stream kost niets.
func DARTBypass() {
	for sid := 0; sid < dartStreams; sid++ {
		a := uintptr(ethDART) + dartTCR + uintptr(sid)*4
		dev.Write32(a, dartBypassDART|dartBypassDAPF)
	}
	dev.MB()
}

// MapRID koppelt een PCI-RID (bus/dev/fn) aan een DART-stream. Zonder deze
// mapping komt de DMA van het endpoint nergens aan.
func MapRID(idx, bus, devno, fn, sid int) {
	v := uint32(rid2sidValid) | uint32(sid)<<rid2sidSIDShift |
		uint32(bus)<<8 | uint32(devno)<<3 | uint32(fn)
	dev.Write32(uintptr(ethPortBase)+portRID2SID+uintptr(idx)*4, v)
	dev.MB()
}

// DARTError geeft het foutregister van de ethernet-DART plus het adres dat de
// fout veroorzaakte: 0 = geen enkele DMA werd geweigerd (en als er dan ook geen
// data binnenkomt, bereikte de DMA de DART niet eens).
func DARTError() (uint32, uint64) {
	return dev.Read32(uintptr(ethDART) + dartError),
		uint64(dev.Read32(uintptr(ethDART)+dartErrAddrHi))<<32 | uint64(dev.Read32(uintptr(ethDART)+dartErrAddrLo))
}

// CfgRead32/CfgWrite32 zijn config-space-toegang voor de hop-helft (die de
// enumeratie doet); ze staan hier omdat het ECAM-plan board-kennis is.
func CfgRead32(a uintptr) uint32     { return dev.Read32(a) }
func CfgWrite32(a uintptr, v uint32) { dev.Write32(a, v); dev.MB() }
