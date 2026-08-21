//go:build gui

// Package xhci is de USB-hostcontroller van HopOS: één driver voor alle drie de
// boards die een toetsenbord moeten kunnen zien.
//
// WAAROM DIT ONDER gui/ STAAT EN ACHTER DE gui-TAG (besluit Derek 06-08): een
// toetsenbord is een GUI-apparaat. Niet uit gemak — een headless node heeft
// niets om in te typen. HopOS kent geen shell, dus de enige consument van een
// toetsaanslag is de SURF-inputweg naar de display. Zonder scherm is deze hele
// stack dode code in een binary die al 13 MB weegt.
//
// De naad die dat ooit kan verschuiven: HID is GUI, maar de CONTROLLER
// misschien niet. Komt er ooit USB-storage, dan is precies dit bestand headless
// nodig en verhuist het naar driver/. Dat is dan een verplaatsing, geen
// herbouw — en tot die dag is vooruitbouwen op een apparaat dat niemand vraagt
// dezelfde fout als de surface-grant van vanochtend.
//
// WAAROM XHCI EN NIET OHCI. Een USB-toetsenbord is low-speed, en OHCI is een
// fractie van het werk van XHCI — dus dat lijkt de goedkope route. Hij is het
// niet, want de Pi's hebben helemaal geen OHCI: op de Pi 4 hangt de USB aan een
// VL805 (XHCI over PCIe) en op de Pi 5 aan de RP1 (XHCI over PCIe). Wie die twee
// wil bedienen schrijft sowieso XHCI. En de RK3566 hééft XHCI-poorten, dus met
// één driver zijn we er op alle drie — in plaats van XHCI voor de Pi's plus
// OHCI/EHCI ernaast voor de Radxa.
//
// Alle moderne XHCI-controllers spreken bovendien zelf met low-speed apparaten:
// de hub-logica zit ín de controller, dus wij zien een poort met een snelheid
// en niet een companion-controller die we apart moeten bedienen. Dat is precies
// de complexiteit die EHCI+OHCI zou toevoegen.
//
// REFERENTIE: xHCI 1.2 (Intel, mei 2019), hoofdstuk 4 (operational model) en 5
// (registers). Waar de spec meerdere kanten op kan, staat de gemaakte keuze in
// het commentaar met het spec-nummer erbij.
//
// WAT DIT BESTAND DOET en waar de grens ligt: de controller vinden, resetten,
// zijn parameters lezen en zijn poorten uitlezen. Dat is de laag die op ijzer
// de eerste vraag beantwoordt — "zit er iets, en op welke snelheid" — zonder
// dat er één ring of TRB aan te pas komt. De transfer-laag (command ring, slots,
// endpoints, interrupt-IN) komt daarbovenop en heeft dit als fundament.
//
// Bewust in die volgorde. Een XHCI-stack die je in één keer schrijft en dan pas
// op ijzer zet, faalt met één symptoom: stilte. Dit stuk maakt van die stilte
// een meting — per laag zichtbaar waar het stopt, dezelfde vorm als het
// GMAC-meetpad en de VOP2-klokketen op de Radxa.
package xhci

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// Capability-registers (xHCI 5.3): de eerste bytes van het venster. Ze zijn
// read-only en vertellen waar de rest ligt — de operational-registers staan op
// een OFFSET die per controller verschilt, dus die moet je lezen en niet
// aannemen.
const (
	capLength  = 0x00 // u8: lengte van het capability-blok = offset naar operational
	capHCIVer  = 0x02 // u16: BCD-versie (0x0100 = 1.0, 0x0110 = 1.1, 0x0120 = 1.2)
	capHCSPar1 = 0x04 // slots/interrupters/poorten
	capHCSPar2 = 0x08 // scratchpad-buffers
	capHCCPar1 = 0x10 // 64-bit adressering, contextgrootte, extended caps
	capDBOff   = 0x14 // offset naar de doorbell-array
	capRTSOff  = 0x18 // offset naar de runtime-registers
	capHCCPar2 = 0x1C
)

// Operational-registers (xHCI 5.4), relatief aan capLength.
const (
	opUSBCmd   = 0x00
	opUSBSts   = 0x04
	opPageSize = 0x08
	opDNCtrl   = 0x14
	opCRCR     = 0x18 // command ring control (64-bit)
	opDCBAAP   = 0x30 // device context base address array pointer (64-bit)
	opConfig   = 0x38

	// Poortregisters: per poort vier woorden vanaf opPortBase.
	opPortBase   = 0x400
	opPortStride = 0x10
	portSC       = 0x00 // status en control
)

// USBCMD-bits (xHCI 5.4.1).
const (
	cmdRun   = 1 << 0 // Run/Stop
	cmdHCRST = 1 << 1 // host controller reset
	cmdINTE  = 1 << 2 // interrupter enable
	cmdHSEE  = 1 << 3 // host system error enable
)

// USBSTS-bits (xHCI 5.4.2).
const (
	stsHCH  = 1 << 0  // HCHalted
	stsHSE  = 1 << 2  // host system error
	stsEINT = 1 << 3  // event interrupt
	stsPCD  = 1 << 4  // port change detect
	stsCNR  = 1 << 11 // controller not ready
)

// PORTSC-bits (xHCI 5.4.8). De schrijf-semantiek van dit register is een val:
// CSC/PEC/PRC/etc zijn write-1-to-clear terwijl PED write-1-to-DISABLE is, en
// ze zitten in hetzelfde woord. Een read-modify-write die de statusbits laat
// staan wist ze dus per ongeluk, en eentje die PED meeschrijft zet de poort uit.
// Daarom schrijft deze driver PORTSC nooit met een kale RMW — zie portWrite.
const (
	pscCCS = 1 << 0  // current connect status: er hangt iets aan
	pscPED = 1 << 1  // port enabled/disabled (write 1 = DISABLE)
	pscOCA = 1 << 3  // overcurrent active
	pscPR  = 1 << 4  // port reset
	pscPP  = 1 << 9  // port power
	pscCSC = 1 << 17 // connect status change (w1c)
	pscPEC = 1 << 18 // port enabled change (w1c)
	pscWRC = 1 << 19 // warm reset change (w1c)
	pscOCC = 1 << 20 // overcurrent change (w1c)
	pscPRC = 1 << 21 // port reset change (w1c)
	pscPLC = 1 << 22 // port link state change (w1c)
	pscCEC = 1 << 23 // config error change (w1c)
	pscWPR = 1 << 31 // warm port reset (write 1 = STARTEN)

	// De w1c-bits bij elkaar: dit masker moet je bij élke PORTSC-schrijfactie
	// uitmaskeren als je ze niet wilt wissen.
	pscChangeMask = pscCSC | pscPEC | pscWRC | pscOCC | pscPRC | pscPLC | pscCEC

	// De bits die een ACTIE starten in plaats van een stand te beschrijven.
	// Ze lezen als de huidige toestand maar betekenen bij het schrijven iets
	// heel anders, dus een read-modify-write moet ze altijd uitmaskeren — en
	// wie ze wél wil zetten doet dat expliciet via portAction.
	pscActionMask = pscPED | pscPR | pscWPR

	pscSpeedShift = 10 // [13:10] port speed
	pscSpeedMask  = 0xF
)

// Speed is de snelheid die de controller aan een poort meldt (xHCI 7.2.2.1.1:
// de default speed-ID's; een controller mág ze via zijn extended capabilities
// anders indelen, maar geen enkele die wij bedienen doet dat).
type Speed uint32

const (
	SpeedNone  Speed = 0
	SpeedFull  Speed = 1 // 12 Mbit
	SpeedLow   Speed = 2 // 1,5 Mbit — waar een toetsenbord doorgaans zit
	SpeedHigh  Speed = 3 // 480 Mbit
	SpeedSuper Speed = 4
)

func (s Speed) String() string {
	switch s {
	case SpeedFull:
		return "full-speed"
	case SpeedLow:
		return "low-speed"
	case SpeedHigh:
		return "high-speed"
	case SpeedSuper:
		return "super-speed"
	}
	return "none"
}

// HC is één hostcontroller.
type HC struct {
	// Base is het begin van het MMIO-venster (de capability-registers). Op de
	// Radxa is dat een vast SoC-adres; op de Pi's een PCIe-BAR.
	Base uintptr

	// Name is wat er in de log verschijnt — een node met drie controllers moet
	// te lezen zijn.
	Name string

	// BusOff is wat je bij een CPU-fysiek adres moet optellen om te krijgen wat
	// de CONTROLLER als adres ziet. Nul op een SoC waar de xHCI direct op de
	// geheugenbus hangt (Radxa); niet nul achter een PCIe-root-complex dat een
	// inbound-window verschuift (dezelfde grootheid als gem.Net.BusOff).
	//
	// Dit staat expliciet in het type en niet stil op nul, omdat een verkeerde
	// waarde hier geen foutmelding geeft maar DMA naar het verkeerde stuk DRAM —
	// het soort fout dat zich als willekeurige corruptie voordoet.
	BusOff uint64

	op    uintptr // operational-registers (Base + CAPLENGTH)
	db    uintptr // doorbell-array
	rt    uintptr // runtime-registers
	slots int     // MaxSlots
	ports int     // MaxPorts
	ctx64 bool    // contextgrootte 64 byte i.p.v. 32
	ac64  bool    // controller kan 64-bit adressen
	ver   uint16

	// Gevuld door Start (zie host.go).
	arena   arena
	dmaBase uintptr // vast board-venster; bewaard voor recovery na HCRST
	dmaSize uintptr
	page    uintptr // paginagrootte van de controller (PAGESIZE-register)
	nSlots  int     // hoeveel slots we daadwerkelijk in CONFIG aanzetten
	ctxSize uintptr // 32 of 64
	scratch int
	dcbaa   uintptr
	res     []*slotRes
	cmd     *ring
	evt     *evring
	pending []event
	dropped int
	running bool

	// poisoned betekent dat hardware- en software-ownership niet meer bewezen
	// gelijk lopen (bv. Disable Slot zonder bevestiging). Nieuwe Enable Slot-
	// opdrachten zijn dan verboden: alleen een geslaagde controllerreset maakt
	// alle hardware-slots aantoonbaar vrij en wist deze toestand. usbin ziet dit
	// via RecoveryNeeded en herbouwt de controller met hetzelfde DMA-venster.
	poisoned error
}

// Probe leest de capability-registers en vult de afgeleide adressen in. Raakt
// de controller verder NIET aan: dit is de goedkoopste manier om te weten of er
// überhaupt een xHCI achter dit adres zit, en het is het eerste dat op ijzer
// stukgaat als een klok of een power-domein niet aanstaat.
//
// Een venster dat niets terugpraat leest als 0x00000000 of 0xFFFFFFFF; allebei
// zijn onmogelijke CAPLENGTH-waarden, dus die vangen we hier af in plaats van
// verderop op een onzinnig offset te gaan schrijven.
func (h *HC) Probe() error {
	raw := dev.Read32(h.Base + capLength)
	length := raw & 0xFF
	h.ver = uint16(raw >> 16)
	if length < 0x20 || length > 0x80 {
		return fmt.Errorf("xhci %s: CAPLENGTH %#x uit het raw-woord %#08x — geen controller op %#x "+
			"(0 = niet geklokt, 0xFF..= dode bus)", h.Name, length, raw, h.Base)
	}
	h.op = h.Base + uintptr(length)

	p1 := dev.Read32(h.Base + capHCSPar1)
	h.slots = int(p1 & 0xFF)
	h.ports = int((p1 >> 24) & 0xFF)

	cp1 := dev.Read32(h.Base + capHCCPar1)
	h.ac64 = cp1&(1<<0) != 0
	h.ctx64 = cp1&(1<<2) != 0

	h.db = h.Base + uintptr(dev.Read32(h.Base+capDBOff)&^0x3)
	h.rt = h.Base + uintptr(dev.Read32(h.Base+capRTSOff)&^0x1F)

	if h.ports == 0 || h.slots == 0 {
		return fmt.Errorf("xhci %s: %d poorten / %d slots — HCSPARAMS1 %#08x is niet plausibel",
			h.Name, h.ports, h.slots, p1)
	}
	return nil
}

// Info geeft wat Probe gevonden heeft, voor één logregel.
func (h *HC) Info() (ver uint16, slots, ports int, ctx64 bool) {
	return h.ver, h.slots, h.ports, h.ctx64
}

// Reset haalt de controller uit welke staat de bootloader hem ook achterliet en
// zet hem gehalteerd klaar.
//
// De volgorde is niet vrij (xHCI 4.2). Eerst STOPPEN en op HCHalted wachten:
// een HCRST terwijl de controller loopt is undefined behaviour, en U-Boot heeft
// hier net nog USB-apparaten gescand — dus hij lóópt. Pas daarna reset, en dan
// wachten tot zowel HCRST als CNR (Controller Not Ready) weg zijn: CNR is het
// bit dat zegt dat de interne staat nog niet bruikbaar is, en erop schrijven
// vóór die tijd wordt genegeerd of hangt de bus.
func (h *HC) Reset() error {
	if h.op == 0 {
		return fmt.Errorf("xhci %s: Reset vóór Probe", h.Name)
	}
	cmd := dev.Read32(h.op + opUSBCmd)
	if cmd&cmdRun != 0 {
		dev.Write32(h.op+opUSBCmd, cmd&^cmdRun)
		if err := h.wait(opUSBSts, stsHCH, stsHCH, 500*time.Millisecond, "halt"); err != nil {
			return err
		}
	}
	dev.Write32(h.op+opUSBCmd, dev.Read32(h.op+opUSBCmd)|cmdHCRST)
	dev.MB()
	// HCRST wist zichzelf; CNR gaat daarna nog een tijd hoog. De spec noemt
	// geen bovengrens, alleen "de driver moet wachten" — een halve seconde is
	// ruim en houdt een dode controller kort.
	if err := h.wait(opUSBCmd, cmdHCRST, 0, 500*time.Millisecond, "HCRST clear"); err != nil {
		return err
	}
	if err := h.wait(opUSBSts, stsCNR, 0, 500*time.Millisecond, "controller ready"); err != nil {
		return err
	}
	h.poisoned = nil
	h.running = false
	return nil
}

// Port is de stand van één poort.
type Port struct {
	Num       int // 1-gebaseerd, zoals de spec ze nummert
	Connected bool
	Enabled   bool
	Speed     Speed
	Raw       uint32
}

// Ports leest alle poorten uit. Puur lezen — geen reset, geen power — zodat dit
// veilig is als eerste ding dat je op ijzer draait.
func (h *HC) Ports() []Port {
	out := make([]Port, 0, h.ports)
	for i := 1; i <= h.ports; i++ {
		v := dev.Read32(h.portReg(i))
		out = append(out, Port{
			Num:       i,
			Connected: v&pscCCS != 0,
			Enabled:   v&pscPED != 0,
			Speed:     Speed((v >> pscSpeedShift) & pscSpeedMask),
			Raw:       v,
		})
	}
	return out
}

// PowerOn zet PP op alle poorten die het nog niet hebben. Sommige controllers
// komen met de poortvoeding uit uit reset, en dan meldt een aangesloten
// toetsenbord zich nooit — CCS blijft 0 en je zoekt op de verkeerde plek.
func (h *HC) PowerOn() {
	for i := 1; i <= h.ports; i++ {
		v := dev.Read32(h.portReg(i))
		if v&pscPP == 0 {
			h.portWrite(i, v|pscPP)
		}
	}
	// De spec eist 20ms tussen poortvoeding en een betrouwbare CCS-lezing
	// (xHCI 4.19.3 verwijst naar USB2 9.1.2: de poort moet debouncen).
	time.Sleep(20 * time.Millisecond)
}

// ClearChanges wist de w1c-statusbits van een poort. Nodig vóór je op een
// verandering gaat wachten: blijft er een oude change-bit staan, dan lees je
// die aan voor de nieuwe.
//
// Wissen doe je door er een 1 náár te schrijven — dus deze functie schrijft de
// change-bits juist WEL mee, precies andersom dan portWrite. Dat is het hele
// verschil tussen de twee, en het is de reden dat ze allebei bestaan: één
// schrijfpad dat de statusbits met rust laat, en één dat ze wist. Wie ze
// samenvoegt krijgt een functie die het ene geval altijd verkeerd doet.
func (h *HC) ClearChanges(port int) {
	v := dev.Read32(h.portReg(port))
	if v&pscChangeMask == 0 {
		return
	}
	dev.Write32(h.portReg(port), v&^uint32(pscActionMask))
	dev.MB()
}

// portAction schrijft PORTSC mét één actiebit erbij (PR of WPR). Alles wat een
// stand beschrijft blijft staan, de w1c-bits en de ándere actiebits gaan eruit.
func (h *HC) portAction(n int, action uint32) {
	v := dev.Read32(h.portReg(n))
	dev.Write32(h.portReg(n), v&^uint32(pscChangeMask|pscActionMask)|action)
	dev.MB()
}

// portReg geeft het PORTSC-adres van poort n (1-gebaseerd).
func (h *HC) portReg(n int) uintptr {
	return h.op + opPortBase + uintptr(n-1)*opPortStride + portSC
}

// portWrite schrijft PORTSC veilig: de write-1-to-clear-statusbits worden
// uitgemaskeerd (anders wist een gewone RMW ze) en PED wordt uitgemaskeerd
// (want een 1 daar ZET de poort uit in plaats van hem aan te houden). Dit is de
// enige plek in deze driver die PORTSC schrijft, precies omdat die twee vallen
// bij een read-modify-write allebei stil zijn.
func (h *HC) portWrite(n int, v uint32) {
	dev.Write32(h.portReg(n), v&^uint32(pscChangeMask|pscActionMask))
	dev.MB()
}

// wait pollt een register tot (waarde & mask) == want.
func (h *HC) wait(off uintptr, mask, want uint32, d time.Duration, what string) error {
	deadline := time.Now().Add(d)
	for {
		v := dev.Read32(h.op + off)
		if v&mask == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("xhci %s: timeout op %s (reg+%#x = %#08x, masker %#x wil %#x)",
				h.Name, what, off, v, mask, want)
		}
		time.Sleep(time.Millisecond)
	}
}
