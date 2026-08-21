//go:build gui

package xhci

import (
	"fmt"
	"math/bits"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// Dit bestand is de weg van "er hangt iets aan poort 3" naar "dit is een
// toetsenbord en zijn rapporten komen binnen". In USB-termen: poortreset,
// slot toewijzen, adresseren, descriptors lezen, de interrupt-endpoint
// configureren en hem armeren.
//
// Wat we NIET doen: HID-report-descriptors parsen. Het boot-protocol (USB HID
// 1.11 §B) legt de rapportvorm vast — 8 bytes toetsenbord, 3 bytes muis — en
// elk apparaat dat bInterfaceSubClass 1 meldt, moet het spreken. Dat is precies
// de reden dat het bestaat: een BIOS moest kunnen typen zonder parser. Wij zijn
// in dezelfde positie.

// USB-standaardrequests en descriptortypes (USB 2.0 §9.4, tabel 9-5).
const (
	reqGetDescriptor = 6
	reqSetConfig     = 9

	descDevice    = 1
	descConfig    = 2
	descInterface = 4
	descEndpoint  = 5

	// HID-class requests (HID 1.11 §7.2), op de interface.
	hidSetIdle     = 0x0A
	hidSetProtocol = 0x0B

	// bInterfaceClass/SubClass van een boot-HID-apparaat.
	classHID     = 3
	subClassBoot = 1
)

// bInterfaceProtocol van een boot-HID-interface (HID 1.11 §4.3). Geëxporteerd
// omdat de aanroeper moet weten welke decoder bij dit apparaat hoort.
const (
	ProtoNone     = 0 // geen boot-HID (of geen interrupt-IN-endpoint)
	ProtoKeyboard = 1
	ProtoMouse    = 2
)

// Endpoint-types in het endpoint-context (xHCI tabel 6-9).
const (
	epTypeControl = 4
	epTypeIntrIn  = 7
)

// hidIface is één boot-HID-interface van een apparaat, met de interrupt-IN
// endpoint die erbij hoort.
type hidIface struct {
	num      int // bInterfaceNumber (nodig voor SET_PROTOCOL/SET_IDLE)
	proto    int // ProtoKeyboard of ProtoMouse
	dci      int
	mps      int
	interval int
	ring     *ring
	bufOff   uintptr // waar in slotRes.buf zijn rapporten landen
	armed    bool
	armTRB   uint64
}

// Device is één geadresseerd USB-apparaat met één of meer boot-HID-interfaces.
//
// MEER DAN ÉÉN, EN DAT IS DE NORMAAL. Een draadloze combo hangt aan één dongle
// die zich als ÉÉN apparaat meldt met twee boot-interfaces: nummer 0 het
// toetsenbord, nummer 1 de muis. Deze driver bond eerst alleen de eerste, en
// dat leek verdedigbaar tot er op 06-08 een Logi Bolt (046d:c548) in een Pi 5
// ging: toetsenbord gevonden, muis stil. De regel is dus niet "één interface
// per apparaat" maar "één endpoint per rol" — en het scheelt in de praktijk
// niets, want beide endpoints hangen aan hetzelfde slot en dezelfde
// Configure-Endpoint-opdracht.
type Device struct {
	hc    *HC
	Slot  int
	Port  int
	Speed Speed

	VendorID  uint16
	ProductID uint16

	mps0    int
	confVal int
	ifaces  []hidIface
	res     *slotRes
	lastErr error
}

// maxHIDIfaces is hoeveel boot-interfaces we van één apparaat bedienen: één
// toetsenbord en één muis. Meer bestaat niet in het boot-protocol — dat kent
// precies die twee rollen — dus dit is de vorm van het probleem en geen
// willekeurige grens.
const maxHIDIfaces = 2

// Protos geeft de rollen die dit apparaat levert.
func (d *Device) Protos() []int {
	out := make([]int, 0, len(d.ifaces))
	for _, f := range d.ifaces {
		out = append(out, f.proto)
	}
	return out
}

// resetRing zet een ring terug op zijn beginstand. Nodig bij hergebruik van een
// slot (herplug): de controller begint na een Address Device weer met cycle 1
// op het adres dat wij in het endpoint-context zetten, dus onze producerkant
// moet daarmee mee terug.
func (r *ring) resetRing() {
	dev.Clear(r.base, uint64(r.n)*16)
	r.enq = 0
	r.cycle = 1
	r.armLink()
	dev.MB()
}

// ctxDW geeft het adres van dword d van context-index i (0 = input control
// context of slot context, afhankelijk van welke context je adresseert).
func (h *HC) ctxDW(base uintptr, i, d int) uintptr {
	return base + uintptr(i)*h.ctxSize + uintptr(d)*4
}

// ResetPort port-reset een poort en wacht tot hij enabled is. Op USB2 zet de
// controller PED pas ná een geslaagde reset; op USB3 gebeurt dat als deel van
// de link-training. In beide gevallen is "PED staat aan" het signaal dat er een
// bruikbaar apparaat aan hangt.
func (h *HC) ResetPort(n int) error {
	h.ClearChanges(n)
	h.portAction(n, pscPR)

	// GEMETEN op een Pi 5 (06-08): 750ms was net niet genoeg voor een Logi
	// Bolt-ontvanger — PORTSC 0x6f1, dus PR nog hoog en PLS op Polling. De scan
	// probeerde het een halve seconde later opnieuw en toen lukte het wél, dus
	// het was een trage poort en geen kapotte. USB2 wil ~50ms; wie meer nodig
	// heeft is een hub of een dongle die zich eerst zelf moet aanmelden.
	deadline := time.Now().Add(2 * time.Second)
	for {
		v := dev.Read32(h.portReg(n))
		if v&pscPRC != 0 {
			h.ClearChanges(n)
			if v&pscPED == 0 {
				return fmt.Errorf("xhci %s: poort %d niet enabled na reset (PORTSC %#08x)", h.Name, n, v)
			}
			return nil
		}
		if v&pscCCS == 0 {
			return fmt.Errorf("xhci %s: poort %d tijdens de reset losgeraakt (PORTSC %#08x)", h.Name, n, v)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("xhci %s: poort %d reset niet klaar (PORTSC %#08x)", h.Name, n, v)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// defaultMPS0 is de control-endpoint-pakketgrootte waarmee we beginnen. Bij
// low-speed is 8 de enige toegestane waarde en bij high/super ligt hij vast; bij
// FULL-speed mag het 8, 16, 32 of 64 zijn en weten we het pas na de eerste acht
// bytes van de device-descriptor. Daarom beginnen we daar met 8 — dat is de enige
// waarde die gegarandeerd werkt — en corrigeren we hem daarna.
func defaultMPS0(s Speed) int {
	switch s {
	case SpeedHigh:
		return 64
	case SpeedSuper:
		return 512
	}
	return 8
}

// disableSlotFn is de kleine testnaad rond het hardwarecommando. De ownership-
// transities eromheen zijn gewone Go-logica en moeten op de host te bewijzen
// zijn zonder MMIO-ringen na te bouwen.
type disableSlotFn func(slot int) error
type clearSlotFn func(slot int)

func (h *HC) disableSlot(slot int) error {
	if slot < 1 || slot > 255 { // Slot ID is het hoge byte van het command-TRB.
		return fmt.Errorf("slot-id %d kan niet in Disable Slot", slot)
	}
	_, err := h.command(0, 0, 0,
		uint32(trbDisableSlot)<<trbTypeShift|uint32(slot)<<24, "disable slot")
	return err
}

func (h *HC) clearSlot(slot int) {
	dev.Write64(h.dcbaa+uintptr(slot)*8, 0)
	dev.MB()
}

// quarantine legt vast dat software niet meer kan bewijzen welke slots de
// controller bezit. Vanaf dit moment mag geen nieuwe Enable Slot meer volgen:
// alleen HCRST maakt alle hardware-state aantoonbaar leeg (Reset wist dit pas
// nadat HCRST én CNR succesvol zijn afgerond).
func (h *HC) quarantine(err error) error {
	if h.poisoned == nil {
		h.poisoned = err
	}
	return fmt.Errorf("xhci %s: slot-ownership onbekend; controllerreset vereist: %w", h.Name, h.poisoned)
}

// claimEnabledSlot boekt een bevestigd Enable Slot-resultaat in. Geeft de
// controller een slot buiten ons CONFIG-bereik terug, dan proberen we precies
// dát gerapporteerde hardware-slot meteen te disablen. Alleen een bevestigde
// disable laat de controller bruikbaar; slot 0, een softwarecollisie of een
// mislukte cleanup quarantaint de hele controller.
func (h *HC) claimEnabledSlot(slot int, disable disableSlotFn) error {
	if slot >= 1 && slot <= h.nSlots {
		if slot >= len(h.res) || h.res[slot] == nil {
			return h.quarantine(fmt.Errorf("Enable Slot gaf slot %d, maar software heeft er geen resources voor", slot))
		}
		r := h.res[slot]
		if r.inUse || r.quarantined {
			return h.quarantine(fmt.Errorf("Enable Slot gaf reeds bezet/quarantined slot %d", slot))
		}
		// Vanaf het bevestigde Enable-resultaat bestaat de hardwarelease, dus nú
		// boeken — niet pas na de descriptor/configuratiefase.
		r.inUse = true
		return nil
	}

	bad := fmt.Errorf("controller gaf na Enable Slot slot %d (CONFIG staat op %d)", slot, h.nSlots)
	if slot < 1 || slot > 255 {
		return h.quarantine(fmt.Errorf("%w; gerapporteerd slot kan niet veilig worden gedisabled", bad))
	}
	if err := disable(slot); err != nil {
		return h.quarantine(fmt.Errorf("%w; cleanup met Disable Slot faalde: %v", bad, err))
	}
	return fmt.Errorf("xhci %s: %w; afwijkend slot is bevestigd gedisabled", h.Name, bad)
}

func (h *HC) abortAttach(slot int, cause error) error {
	if err := h.releaseSlot(slot); err != nil {
		return fmt.Errorf("%v; cleanup na mislukte enumeratie: %w", cause, err)
	}
	return cause
}

// Attach doorloopt de volledige enumeratie van de poort tot een gearmeerde
// interrupt-endpoint. Geeft nil,nil als er wel een apparaat hangt maar het geen
// boot-HID is — een USB-stick in de poort is geen fout, alleen niets voor ons.
func (h *HC) Attach(port int) (*Device, error) {
	if !h.running {
		return nil, fmt.Errorf("xhci %s: Attach vóór Start", h.Name)
	}
	if h.poisoned != nil {
		return nil, fmt.Errorf("xhci %s: geen nieuwe apparaten vóór controllerreset: %w", h.Name, h.poisoned)
	}
	if err := h.ResetPort(port); err != nil {
		return nil, err
	}
	sp := Speed(dev.Read32(h.portReg(port)) >> pscSpeedShift & pscSpeedMask)

	ev, err := h.command(0, 0, 0, uint32(trbEnableSlot)<<trbTypeShift, "enable slot")
	if err != nil {
		return nil, err
	}
	slot := ev.slot
	if err := h.claimEnabledSlot(slot, h.disableSlot); err != nil {
		return nil, err
	}

	d := &Device{hc: h, Slot: slot, Port: port, Speed: sp, res: h.res[slot], mps0: defaultMPS0(sp)}
	if err := d.address(); err != nil {
		return nil, h.abortAttach(slot, err)
	}
	if err := d.readDescriptors(); err != nil {
		return nil, h.abortAttach(slot, err)
	}
	if len(d.ifaces) == 0 {
		if err := h.releaseSlot(slot); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := d.configure(); err != nil {
		return nil, h.abortAttach(slot, err)
	}
	return d, nil
}

// address doet stap 1 t/m 3 van de enumeratie: device context aanhaken, het
// input context vullen met slot + EP0, en Address Device.
func (d *Device) address() error {
	h := d.hc
	r := d.res
	r.ctrl.resetRing()
	dev.Clear(r.devCtx, uint64(h.ctxSize)*32)
	dev.Write64(h.dcbaa+uintptr(d.Slot)*8, uint64(r.devCtx)+h.BusOff)
	dev.MB()

	d.buildInput(1, addSlot|addEP0)
	_, err := h.command(uint32(uint64(r.inCtx)+h.BusOff), uint32((uint64(r.inCtx)+h.BusOff)>>32), 0,
		uint32(trbAddressDev)<<trbTypeShift|uint32(d.Slot)<<24, "address device")
	return err
}

// Add-flags van het input control context (xHCI 6.2.5.1). A0 = slot context,
// A1 = de default control endpoint, An = DCI n.
const (
	addSlot = 1 << 0
	addEP0  = 1 << 1
)

// buildInput vult het input context met het slot context en EP0. entries is de
// hoogste DCI die geldig moet zijn (1 = alleen EP0); add zijn de flags die
// zeggen wélke contexten het commando mag lezen.
//
// De add-flags zijn een parameter en geen afgeleide van entries, en dat is geen
// smaak: een gezette flag naar een leeg endpoint-context is EP Type 0 (Not
// Valid) en dus een parameter error. Wie A0..A(entries) automatisch zou zetten,
// zet flags aan voor de endpoints tússen EP0 en de onze die we nooit invullen.
//
// Het input context wordt élke keer opnieuw opgebouwd in plaats van uit het
// device context gekopieerd. Dat kan omdat wij alles wat erin staat zelf
// bepalen (route 0 — geen hub-diepte, geen TT), en het scheelt de klassieke
// fout waarbij een half gekopieerd context oude endpoints laat staan.
func (d *Device) buildInput(entries int, add uint32) {
	h := d.hc
	in := d.res.inCtx
	dev.Clear(in, uint64(h.ctxSize)*33)
	dev.Write32(h.ctxDW(in, 0, 1), add)

	// Slot context (index 1 in het input context): route string 0 (direct op
	// een roothub-poort), de snelheid en het poortnummer.
	dev.Write32(h.ctxDW(in, 1, 0), uint32(d.Speed)<<20|uint32(entries)<<27)
	dev.Write32(h.ctxDW(in, 1, 1), uint32(d.Port)<<16)

	// EP0 (index 2 = DCI 1): control, 3 retries, onze control ring.
	deq := d.res.ctrl.deqPtr()
	dev.Write32(h.ctxDW(in, 2, 1), 3<<1|uint32(epTypeControl)<<3|uint32(d.mps0)<<16)
	dev.Write32(h.ctxDW(in, 2, 2), uint32(deq))
	dev.Write32(h.ctxDW(in, 2, 3), uint32(deq>>32))
	dev.Write32(h.ctxDW(in, 2, 4), 8) // average TRB length
	dev.MB()
}

// control voert één control transfer uit. Setup-, Data- en Statusfase zijn in
// xHCI drie APARTE TD's (xHCI 4.11.2.2), dus een korte datafase beëindigt alleen
// zijn eigen TD en de statusfase loopt gewoon door — daarom kunnen we op allebei
// een completion vragen en het echte aantal bytes uit de datafase halen.
func (d *Device) control(reqType, req uint8, val, idx, length uint16) (int, error) {
	h := d.hc
	r := d.res.ctrl
	in := reqType&0x80 != 0

	trt := uint32(0) // geen datafase
	switch {
	case length == 0:
	case in:
		trt = 3
	default:
		trt = 2
	}
	r.push(uint32(reqType)|uint32(req)<<8|uint32(val)<<16,
		uint32(idx)|uint32(length)<<16,
		8,
		trbIDT|uint32(trbSetup)<<trbTypeShift|trt<<16)

	var dataTRB uint64
	if length > 0 {
		bus := uint64(d.res.buf+bufCtrl) + h.BusOff
		dir := uint32(0)
		if in {
			dir = 1
		}
		dataTRB = r.push(uint32(bus), uint32(bus>>32), uint32(length),
			trbISP|trbIOC|uint32(trbData)<<trbTypeShift|dir<<16)
	}
	// De statusfase gaat de ANDERE kant op dan de data; zonder data is hij IN.
	sdir := uint32(1)
	if in && length > 0 {
		sdir = 0
	}
	statTRB := r.push(0, 0, 0, trbIOC|uint32(trbStatus)<<trbTypeShift|sdir<<16)
	h.doorbell(d.Slot, 1)

	got := int(length)
	if length > 0 {
		ev, err := h.waitEvent(func(e event) bool {
			return e.kind == trbTransferEvt && e.ptr == dataTRB
		}, 2*time.Second, "control data stage")
		if err != nil {
			return 0, err
		}
		if ev.comp != ccSuccess && ev.comp != ccShortPacket {
			return 0, fmt.Errorf("xhci %s: control %#02x/%d datafase — %s (%d)",
				h.Name, reqType, req, compName(ev.comp), ev.comp)
		}
		got = int(length) - int(ev.rem)
	}
	ev, err := h.waitEvent(func(e event) bool {
		return e.kind == trbTransferEvt && e.ptr == statTRB
	}, 2*time.Second, "control status stage")
	if err != nil {
		return 0, err
	}
	if ev.comp != ccSuccess && ev.comp != ccShortPacket {
		return 0, fmt.Errorf("xhci %s: control %#02x/%d statusfase — %s (%d)",
			h.Name, reqType, req, compName(ev.comp), ev.comp)
	}
	return got, nil
}

// bufBytes leest n bytes uit de control-databuffer van dit slot.
func (d *Device) bufBytes(n int) []byte {
	b := make([]byte, n)
	dev.CopyOut(b, d.res.buf+bufCtrl)
	return b
}

// readDescriptors haalt de device- en configuratiedescriptor op en zoekt de
// boot-HID-interface. Vult Proto/Iface/epDCI/epMPS als hij er een vindt.
func (d *Device) readDescriptors() error {
	h := d.hc

	// Eerst acht bytes: bij full-speed staat de echte EP0-pakketgrootte pas in
	// byte 7, en tot we die weten mogen we niet meer dan één pakket vragen.
	if _, err := d.control(0x80, reqGetDescriptor, descDevice<<8, 0, 8); err != nil {
		return fmt.Errorf("device descriptor (8): %w", err)
	}
	if mps := int(d.bufBytes(8)[7]); mps > 0 && mps != d.mps0 {
		d.mps0 = mps
		// Evaluate Context: alleen EP0 aanpassen, het slot laten staan.
		d.buildInput(1, addEP0)
		if _, err := h.command(uint32(uint64(d.res.inCtx)+h.BusOff), uint32((uint64(d.res.inCtx)+h.BusOff)>>32), 0,
			uint32(trbEvalCtx)<<trbTypeShift|uint32(d.Slot)<<24, "evaluate context (EP0 packet size)"); err != nil {
			return err
		}
	}

	if _, err := d.control(0x80, reqGetDescriptor, descDevice<<8, 0, 18); err != nil {
		return fmt.Errorf("device descriptor: %w", err)
	}
	dd := d.bufBytes(18)
	d.VendorID = uint16(dd[8]) | uint16(dd[9])<<8
	d.ProductID = uint16(dd[10]) | uint16(dd[11])<<8

	// Configuratiedescriptor: eerst de kop voor wTotalLength, dan het geheel.
	if _, err := d.control(0x80, reqGetDescriptor, descConfig<<8, 0, 9); err != nil {
		return fmt.Errorf("config descriptor (9): %w", err)
	}
	cd := d.bufBytes(9)
	total := int(cd[2]) | int(cd[3])<<8
	if total > bufCtrlSize {
		total = bufCtrlSize
	}
	if total < 9 {
		return fmt.Errorf("xhci %s: config descriptor meldt %d bytes", h.Name, total)
	}
	n, err := d.control(0x80, reqGetDescriptor, descConfig<<8, 0, uint16(total))
	if err != nil {
		return fmt.Errorf("config descriptor: %w", err)
	}
	d.parseConfig(d.bufBytes(n))
	return nil
}

// parseConfig loopt de descriptorketen af en verzamelt ELKE boot-HID-interface
// met een interrupt-IN-endpoint — hooguit één per rol.
//
// De keten is plat: een interface-descriptor gevolgd door zijn endpoints, dan
// de volgende interface. We onthouden dus welke interface we net zagen (cur) en
// hangen de eerste bruikbare endpoint daaraan. Een interface die ons niet
// interesseert zet cur op -1, zodat zíjn endpoints niet per ongeluk bij de
// vorige belanden — dat is de val in deze parse.
func (d *Device) parseConfig(b []byte) {
	d.confVal = 0
	if len(b) >= 6 {
		d.confVal = int(b[5])
	}
	cur, curProto := -1, ProtoNone
	for i := 0; i+2 <= len(b); {
		l := int(b[i])
		if l < 2 || i+l > len(b) {
			break
		}
		switch b[i+1] {
		case descInterface:
			cur, curProto = -1, ProtoNone
			if l >= 9 && b[i+5] == classHID && b[i+6] == subClassBoot &&
				(b[i+7] == ProtoKeyboard || b[i+7] == ProtoMouse) &&
				!d.hasProto(int(b[i+7])) && len(d.ifaces) < maxHIDIfaces {
				cur, curProto = int(b[i+2]), int(b[i+7])
			}
		case descEndpoint:
			// bmAttributes[1:0] == 3 = interrupt, bEndpointAddress bit 7 = IN.
			if l >= 7 && cur >= 0 && b[i+3]&0x3 == 3 && b[i+2]&0x80 != 0 {
				ep := int(b[i+2] & 0xF)
				d.ifaces = append(d.ifaces, hidIface{
					num:      cur,
					proto:    curProto,
					dci:      2*ep + 1, // IN-endpoint: DCI = 2N+1
					mps:      (int(b[i+4]) | int(b[i+5])<<8) & 0x7FF,
					interval: int(b[i+6]),
				})
				cur = -1 // deze rol is vervuld; verdere endpoints negeren
			}
		}
		i += l
	}
}

func (d *Device) hasProto(p int) bool {
	for _, f := range d.ifaces {
		if f.proto == p {
			return true
		}
	}
	return false
}

// intervalExponent zet bInterval om naar het exponentveld van het endpoint
// context. xHCI telt in 125µs-microframes als macht van twee; USB telt bij
// low/full-speed in hele frames (1ms) en bij high-speed al in machten van twee.
// De omzetting en de grenzen zijn die van Linux (xhci_get_endpoint_interval).
func intervalExponent(sp Speed, bInterval int) uint32 {
	if bInterval < 1 {
		bInterval = 1
	}
	switch sp {
	case SpeedHigh, SpeedSuper:
		e := bInterval - 1
		if e < 0 {
			e = 0
		}
		if e > 15 {
			e = 15
		}
		return uint32(e)
	}
	// Low/full-speed: bInterval frames = bInterval*8 microframes, naar beneden
	// afgerond op een macht van twee, en geklemd op [3,10] (1ms..128ms).
	e := bits.Len32(uint32(bInterval*8)) - 1
	if e < 3 {
		e = 3
	}
	if e > 10 {
		e = 10
	}
	return uint32(e)
}

// configure zet álle gevonden interrupt-endpoints aan, kiest de configuratie en
// schakelt elke interface naar het boot-protocol. Daarna staan ze gearmeerd.
//
// Eén Configure Endpoint voor allemaal: het input context draagt zoveel
// endpoint-contexten als je wilt, en de add-flags zeggen welke meedoen. Twee
// losse commando's zouden de tweede het werk van de eerste laten overschrijven,
// want elk commando vervangt de héle endpoint-configuratie van het slot.
func (d *Device) configure() error {
	h := d.hc

	add, maxDCI := uint32(addSlot), 0
	for i := range d.ifaces {
		f := &d.ifaces[i]
		f.ring = d.res.intr[i]
		f.bufOff = bufIntr + uintptr(i)*bufIntrSize
		f.ring.resetRing()
		add |= 1 << uint(f.dci)
		if f.dci > maxDCI {
			maxDCI = f.dci
		}
	}
	// A1 blijft UIT: een Configure Endpoint mag de default control endpoint niet
	// aanraken (xHCI 4.6.6) — die is al geregeld door Address Device.
	d.buildInput(maxDCI, add)
	in := d.res.inCtx
	for _, f := range d.ifaces {
		deq := f.ring.deqPtr()
		dev.Write32(h.ctxDW(in, f.dci+1, 0), intervalExponent(d.Speed, f.interval)<<16)
		dev.Write32(h.ctxDW(in, f.dci+1, 1), 3<<1|uint32(epTypeIntrIn)<<3|uint32(f.mps)<<16)
		dev.Write32(h.ctxDW(in, f.dci+1, 2), uint32(deq))
		dev.Write32(h.ctxDW(in, f.dci+1, 3), uint32(deq>>32))
		// Average TRB length en Max ESIT Payload: bij een interrupt-endpoint
		// zonder burst is dat allebei gewoon de pakketgrootte.
		dev.Write32(h.ctxDW(in, f.dci+1, 4), uint32(f.mps)|uint32(f.mps)<<16)
	}
	dev.MB()

	if _, err := h.command(uint32(uint64(in)+h.BusOff), uint32((uint64(in)+h.BusOff)>>32), 0,
		uint32(trbConfigEP)<<trbTypeShift|uint32(d.Slot)<<24, "configure endpoint"); err != nil {
		return err
	}
	if _, err := d.control(0x00, reqSetConfig, uint16(d.confVal), 0, 0); err != nil {
		return fmt.Errorf("set configuration: %w", err)
	}
	for i := range d.ifaces {
		f := &d.ifaces[i]
		// SET_PROTOCOL(0) = boot-protocol, PER INTERFACE. Sommige apparaten
		// stallen hem als ze maar één protocol kennen; dat is geen fout, dan
		// spreken ze al boot.
		if _, err := d.control(0x21, hidSetProtocol, 0, uint16(f.num), 0); err != nil {
			fmt.Printf("usb: %s slot %d iface %d: SET_PROTOCOL(boot) refused (%v) — assuming boot protocol\n",
				h.Name, d.Slot, f.num, err)
		}
		// SET_IDLE(0) = alleen rapporteren bij verandering. Ook optioneel.
		_, _ = d.control(0x21, hidSetIdle, 0, uint16(f.num), 0)
		d.arm(f)
	}
	return nil
}

// arm zet één Normal-TRB op de interrupt-ring van deze interface en belt aan.
// De controller vult de buffer zodra het apparaat iets te melden heeft; tot die
// tijd kost het niets.
func (d *Device) arm(f *hidIface) {
	bus := uint64(d.res.buf+f.bufOff) + d.hc.BusOff
	f.armTRB = f.ring.push(uint32(bus), uint32(bus>>32), uint32(reportLen(f.mps)),
		trbISP|trbIOC|uint32(trbNormal)<<trbTypeShift)
	d.hc.doorbell(d.Slot, f.dci)
	f.armed = true
}

// reportLen is hoeveel we per beurt vragen: de pakketgrootte, begrensd door de
// ruimte die dit slot per interface heeft.
func reportLen(mps int) int {
	if mps > bufIntrSize {
		return bufIntrSize
	}
	return mps
}

// Report haalt één binnengekomen HID-rapport op en armeert die endpoint
// opnieuw. proto zegt van welke rol het kwam — een combo-dongle levert
// toetsenbord én muis op hetzelfde slot, dus de aanroeper moet weten welke
// decoder erbij hoort. Geeft ok=false als er niets klaarstaat; dit is het
// pollpad en het hoort meestal niets te doen.
func (d *Device) Report(buf []byte) (n, proto int, ok bool) {
	h := d.hc
	h.pump()
	for i := range d.ifaces {
		f := &d.ifaces[i]
		if !f.armed {
			continue
		}
		ev, got := h.take(func(e event) bool {
			return e.kind == trbTransferEvt && e.ptr == f.armTRB
		})
		if !got {
			continue
		}
		f.armed = false
		return d.handle(f, ev, buf)
	}
	return 0, 0, false
}

func (d *Device) handle(f *hidIface, ev event, buf []byte) (int, int, bool) {
	h := d.hc
	switch ev.comp {
	case ccSuccess, ccShortPacket:
		n := reportLen(f.mps) - int(ev.rem)
		if n > len(buf) {
			n = len(buf)
		}
		if n > 0 {
			dev.CopyOut(buf[:n], d.res.buf+f.bufOff)
		}
		d.arm(f)
		return n, f.proto, n > 0
	case ccStall:
		// Een gestalde interrupt-endpoint komt niet vanzelf terug: hij moet
		// gereset worden én zijn dequeue-pointer moet opnieuw gezet, anders
		// wijst de controller nog naar het TRB dat de stall veroorzaakte.
		if err := d.recover(f); err != nil {
			d.lastErr = err
			fmt.Printf("usb: %s slot %d iface %d: endpoint recovery failed: %v\n",
				h.Name, d.Slot, f.num, err)
			return 0, 0, false
		}
		d.arm(f)
		return 0, 0, false
	default:
		// Een losgetrokken apparaat geeft transaction errors tot de poort het
		// meldt. Niet opnieuw armeren: de scan ruimt hem op.
		d.lastErr = fmt.Errorf("%s (%d)", compName(ev.comp), ev.comp)
		return 0, 0, false
	}
}

// recover haalt een gestalde endpoint uit halted en zet zijn ring terug op nul.
func (d *Device) recover(f *hidIface) error {
	h := d.hc
	if _, err := h.command(0, 0, 0,
		uint32(trbResetEP)<<trbTypeShift|uint32(f.dci)<<16|uint32(d.Slot)<<24, "reset endpoint"); err != nil {
		return err
	}
	f.ring.resetRing()
	deq := f.ring.deqPtr()
	_, err := h.command(uint32(deq), uint32(deq>>32), 0,
		uint32(trbSetTRDeq)<<trbTypeShift|uint32(f.dci)<<16|uint32(d.Slot)<<24, "set TR dequeue pointer")
	return err
}

// Err geeft de laatste transferfout van dit apparaat (nil zolang alles loopt).
func (d *Device) Err() error { return d.lastErr }

// Detach geeft het slot terug aan de controller. Idempotent. Een fout betekent
// dat de hardware-disable niet bevestigd is; d en zijn slotRes blijven dan
// bewust eigenaar/quarantined en de HC weigert nieuwe allocations tot Reset.
func (d *Device) Detach() error {
	if d.res == nil || !d.res.inUse {
		return nil
	}
	if err := d.hc.releaseSlot(d.Slot); err != nil {
		d.lastErr = err
		return err
	}
	for i := range d.ifaces {
		d.ifaces[i].armed = false
	}
	d.res = nil
	return nil
}

// releaseSlot doet Disable Slot en haalt pas ná de bevestigde completion het
// device context uit de DCBAA. Bij timeout/afwijzing blijft de softwarelease
// staan en gaat het slot in quarantaine; anders zouden volgende Enable Slot-
// retries ongemerkt de eindige hardware-slots kunnen opstapelen.
func (h *HC) releaseSlot(slot int) error {
	return h.releaseSlotWith(slot, h.disableSlot, h.clearSlot)
}

func (h *HC) releaseSlotWith(slot int, disable disableSlotFn, clear clearSlotFn) error {
	if slot < 1 || slot > h.nSlots || slot >= len(h.res) || h.res[slot] == nil {
		return h.quarantine(fmt.Errorf("software probeerde onbekend slot %d vrij te geven", slot))
	}
	r := h.res[slot]
	if !r.inUse && !r.quarantined {
		return nil
	}
	if err := disable(slot); err != nil {
		r.inUse = true
		r.quarantined = true
		return h.quarantine(fmt.Errorf("Disable Slot %d niet bevestigd: %v", slot, err))
	}
	clear(slot)
	r.inUse = false
	r.quarantined = false
	return nil
}

func (d *Device) String() string {
	what := ""
	for _, f := range d.ifaces {
		if what != "" {
			what += "+"
		}
		switch f.proto {
		case ProtoKeyboard:
			what += "keyboard"
		case ProtoMouse:
			what += "mouse"
		default:
			what += "HID"
		}
	}
	if what == "" {
		what = "HID device"
	}
	return fmt.Sprintf("%s %04x:%04x on port %d (%s, slot %d)",
		what, d.VendorID, d.ProductID, d.Port, d.Speed, d.Slot)
}
