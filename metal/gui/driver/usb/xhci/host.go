//go:build gui

package xhci

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Dit bestand brengt de controller van "gereset" naar "draaiend": de
// datastructuren die xHCI verplicht in DRAM wil zien (device-context-array,
// scratchpad, command ring, event ring), en daarna de event-pomp waar al het
// antwoordverkeer doorheen komt.
//
// GEEN INTERRUPTS. Deze driver pollt, net als elke andere driver in HopOS: er
// is geen GIC-pad en een toetsenbord is met 8ms-intervallen ruim binnen wat
// pollen aankan. De interrupter wordt wél opgezet (de event-ring hángt eraan),
// alleen staat IE uit en leest niemand IMAN.

// maxDevices is hoeveel apparaten we tegelijk geadresseerd kunnen hebben. De
// controller kan er meestal 32 of 64; wij zetten CONFIG op dit getal en leggen
// per slot een vaste set structuren aan.
//
// Waarom een cap en niet h.slots: de per-slot structuren worden ÉÉN keer
// aangelegd en daarna hergebruikt, zodat in- en uitpluggen niets lekt. Dat
// vooraf doen voor 64 slots is 1,3MB voor een node met één toetsenbord. 8 is
// ruim voor wat er fysiek in een Pi of een Radxa past — dit is een
// invoerapparaat-pad, geen USB-hub-farm.
const maxDevices = 8

// scratchMax is de bovengrens die we aan de scratchpad-eis van de controller
// stellen. Echte hardware vraagt er een handvol; een controller die er honderden
// vraagt is een verkeerd gelezen register en geen apparaat dat we willen
// bedienen — dan liever hier stoppen dan de DMA-regio stil oplopen.
const scratchMax = 64

// pendingCap is de diepte van de event-wachtrij. Ruim: er staan er hooguit een
// paar tegelijk (één commando plus één interrupt-transfer per apparaat), en
// alleen een apparaat dat niemand meer ophaalt vult hem.
const pendingCap = 64

// arena is de bump-allocator over de DMA-regio van het board. Geen free: alles
// wat hier uitkomt leeft zo lang de node leeft (zie maxDevices).
//
// ALLES WORDT OP PAGINAGRENS UITGEDEELD, en dat is geen luiheid. xHCI stelt per
// structuur twee eisen (tabel 6-1): een alignment én een BOUNDARY die hij niet
// mag kruisen. Die tweede is de stille: een ringsegment van 4KB op een
// 64-byte-grens voldoet aan de alignment en kruist tóch een 64KB-grens zodra
// het toevallig hoog in een blok valt. De controller loopt dan van de ring af.
// Eén regel alignment koopt die hele klasse fouten af — en dat kost hier ruimte
// die we hebben, want deze regio is 2MB voor een toetsenbord.
type arena struct{ cur, end uintptr }

func (a *arena) alloc(n, align uintptr) (uintptr, error) {
	p := (a.cur + align - 1) &^ (align - 1)
	if p+n > a.end {
		return 0, fmt.Errorf("xhci: DMA-regio vol (%d bytes gevraagd, %d over)", n, a.end-a.cur)
	}
	a.cur = p + n
	dev.Clear(p, uint64(n))
	return p, nil
}

// slotRes is de vaste set structuren van één device-slot. Vooraf aangelegd,
// hergebruikt bij herplug.
type slotRes struct {
	devCtx uintptr             // door de controller beschreven device context
	inCtx  uintptr             // input context: wat wij de controller vertellen
	ctrl   *ring               // EP0, control transfers
	intr   [maxHIDIfaces]*ring // één per boot-interface (toetsenbord én muis
	// kunnen op ÉÉN apparaat zitten — een draadloze combo op één dongle)
	buf         uintptr // 4KB werkgeheugen (zie bufCtrl/bufIntr)
	inUse       bool    // Enable Slot bevestigd, Disable Slot nog niet bevestigd
	quarantined bool    // Disable Slot faalde: ownership onbekend, reset vereist
}

// Verdeling van de 4KB werkbuffer per slot. Control-data en het HID-rapport
// mogen elkaar niet raken: de interrupt-endpoint staat ARMED terwijl wij een
// control transfer doen, dus de controller kan op elk moment in het
// rapportvenster schrijven.
const (
	bufCtrl     = 0    // descriptors e.d.
	bufCtrlSize = 1024 // een config-descriptor van een HID-apparaat is < 100 bytes
	bufIntr     = 2048 // eerste interface; de volgende op +bufIntrSize
	bufIntrSize = 64
)

// Start bouwt de datastructuren op in de meegegeven DMA-regio en zet de
// controller aan. De regio moet buiten élke RAM-declaratie van de runtime
// liggen (layout.USBDMAPA): dan is hij device-gemapt en dus coherent met de
// controller zonder cache-onderhoud — dezelfde eis als de NIC-ringen.
//
// Volgorde is dwingend (xHCI 4.2): eerst alles in DRAM, dan de pointers in de
// registers, dan pas RUN. Een controller die loopt terwijl DCBAAP nog naar nul
// wijst, leest device-contexten op adres 0.
func (h *HC) Start(dmaBase, dmaSize uintptr) error {
	if h.op == 0 {
		return fmt.Errorf("xhci %s: Start vóór Probe", h.Name)
	}
	if h.poisoned != nil {
		return fmt.Errorf("xhci %s: Start vereist eerst een geslaagde controllerreset: %w", h.Name, h.poisoned)
	}
	if dmaSize == 0 {
		return fmt.Errorf("xhci %s: lege DMA-regio", h.Name)
	}
	// Het board-venster is vast voor de levensduur van de node. Na een poisoned
	// Disable Slot doet Recover HCRST en bouwt exact in dit venster alle tabellen
	// opnieuw op; oude pointers worden pas overschreven nadat hardware gereset is.
	h.dmaBase, h.dmaSize = dmaBase, dmaSize
	h.arena = arena{cur: dmaBase, end: dmaBase + dmaSize}

	// Paginagrootte van de controller: bit n → 2^(n+12). Vrijwel altijd 4KB,
	// maar de scratchpad-buffers moeten er exact op passen dus we lezen hem.
	ps := dev.Read32(h.op + opPageSize)
	h.page = 4096
	for i := 0; i < 16; i++ {
		if ps&(1<<uint(i)) != 0 {
			h.page = uintptr(1) << uint(i+12)
			break
		}
	}
	if h.page > 65536 {
		return fmt.Errorf("xhci %s: paginagrootte %d — buiten wat deze driver plant", h.Name, h.page)
	}

	// 64-bit adressering: als de controller die niet kan, mag geen enkel adres
	// dat we programmeren boven de 4GB liggen. Alle boards die wij bedienen
	// planten hun USB-regio laag, dus dit is een assertie en geen fallback.
	if !h.ac64 && uint64(dmaBase+dmaSize)+h.BusOff > 1<<32 {
		return fmt.Errorf("xhci %s: controller is 32-bit maar de DMA-regio eindigt op %#x",
			h.Name, uint64(dmaBase+dmaSize)+h.BusOff)
	}

	h.nSlots = h.slots
	if h.nSlots > maxDevices {
		h.nSlots = maxDevices
	}
	// De wachtrij vooraf op maat: pump gebruikt cap() als grens, dus een nil
	// slice zou meteen als "vol" lezen.
	h.pending = make([]event, 0, pendingCap)

	// Device Context Base Address Array: DCBAA[0] is de scratchpad, [i] het
	// device context van slot i.
	dcbaa, err := h.arena.alloc(uintptr(h.nSlots+1)*8, h.page)
	if err != nil {
		return err
	}
	h.dcbaa = dcbaa

	if err := h.setupScratchpad(); err != nil {
		return err
	}
	if err := h.setupSlots(); err != nil {
		return err
	}

	// Command ring: één segment van een pagina (255 bruikbare TRB's).
	cr, err := h.arena.alloc(h.page, h.page)
	if err != nil {
		return err
	}
	h.cmd = newRing(cr, uint64(cr)+h.BusOff, int(h.page))

	// Event ring: één segment plus de ERST die ernaar wijst.
	er, err := h.arena.alloc(h.page, h.page)
	if err != nil {
		return err
	}
	erst, err := h.arena.alloc(64, h.page)
	if err != nil {
		return err
	}
	h.evt = &evring{
		base:  er,
		bus:   uint64(er) + h.BusOff,
		n:     int(h.page / 16),
		cycle: 1,
		ir:    h.rt + rtIR0,
	}
	dev.Write32(erst+0, uint32(h.evt.bus))
	dev.Write32(erst+4, uint32(h.evt.bus>>32))
	dev.Write32(erst+8, uint32(h.evt.n)) // segmentgrootte in TRB's
	dev.Write32(erst+12, 0)

	// Nu de registers. CONFIG eerst: hoeveel slots we gaan gebruiken.
	dev.Write32(h.op+opConfig, uint32(h.nSlots))
	h.write64(h.op+opDCBAAP, uint64(h.dcbaa)+h.BusOff)

	// CRCR: het lage dword draagt RCS (Ring Cycle State) — dat moet 1 zijn,
	// want newRing begint met cycle 1. Hoog eerst, dan laag: de controller
	// latcht op het lage woord.
	h.write64(h.op+opCRCR, uint64(h.cmd.bus)|1)

	// De interrupter: ERSTSZ vóór ERSTBA (het schrijven van ERSTBA latcht de
	// tabel), en ERDP ertussen zodat de leespositie klopt vanaf het eerste
	// event. IMAN blijft dicht — wij pollen.
	dev.Write32(h.evt.ir+irERSTSZ, 1)
	h.write64(h.evt.ir+irERDP, h.evt.bus|erdpEHB)
	h.write64(h.evt.ir+irERSTBA, uint64(erst)+h.BusOff)
	dev.Write32(h.evt.ir+irIMOD, 0)

	dev.MB()
	dev.Write32(h.op+opUSBCmd, dev.Read32(h.op+opUSBCmd)|cmdRun)
	if err := h.wait(opUSBSts, stsHCH, 0, 500*time.Millisecond, "run"); err != nil {
		return err
	}
	h.running = true
	return nil
}

// RecoveryNeeded geeft de ownership-fout die een volledige controllerreset
// vereist, of nil als Attach veilig verder mag. usbin controleert dit vóór elke
// scanronde; zo is quarantine een bereikbaar herstelpad en geen reboot-slot.
func (h *HC) RecoveryNeeded() error { return h.poisoned }

// Recover wist alle hardware-slotstate met HCRST, bouwt command/event/device-
// tabellen opnieuw in het bij Start bewaarde DMA-venster en zet de poorten weer
// aan. De manager moet vóór deze call zijn oude Device-handles vergeten: HCRST
// maakt die per definitie ongeldig.
func (h *HC) Recover() error {
	return h.recoverWith(h.Reset, h.Start, h.PowerOn)
}

// recoverWith is de host-testbare toestand rond de drie hardwarestappen.
func (h *HC) recoverWith(reset func() error, start func(uintptr, uintptr) error, power func()) error {
	if h.poisoned == nil {
		return nil
	}
	if h.dmaSize == 0 {
		return fmt.Errorf("xhci %s: recovery heeft geen bewaard DMA-venster", h.Name)
	}
	if err := reset(); err != nil {
		h.poisoned = fmt.Errorf("recovery reset: %w", err)
		return fmt.Errorf("xhci %s: %w", h.Name, h.poisoned)
	}
	// Reset heeft de oude poison na bevestigde HCRST gewist; een Start-fout
	// moet hem opnieuw zetten, anders zou de volgende scan een half opgebouwde
	// controller als gezond behandelen.
	h.poisoned = nil
	if err := start(h.dmaBase, h.dmaSize); err != nil {
		h.running = false
		h.poisoned = fmt.Errorf("recovery start: %w", err)
		return fmt.Errorf("xhci %s: %w", h.Name, h.poisoned)
	}
	power()
	h.poisoned = nil
	return nil
}

// setupScratchpad geeft de controller het krabbelgeheugen dat hij in
// HCSPARAMS2 opeist. Nul buffers is normaal (dan slaan we het over); DCBAA[0]
// blijft dan nul, zoals de spec voorschrijft.
func (h *HC) setupScratchpad() error {
	p2 := dev.Read32(h.Base + capHCSPar2)
	n := int(p2>>27&0x1F | (p2 >> 21 & 0x1F << 5))
	h.scratch = n
	if n == 0 {
		return nil
	}
	if n > scratchMax {
		return fmt.Errorf("xhci %s: %d scratchpad-buffers gevraagd (HCSPARAMS2 %#08x) — boven de grens van %d",
			h.Name, n, p2, scratchMax)
	}
	arr, err := h.arena.alloc(uintptr(n)*8, h.page)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		b, err := h.arena.alloc(h.page, h.page)
		if err != nil {
			return err
		}
		dev.Write64(arr+uintptr(i)*8, uint64(b)+h.BusOff)
	}
	dev.Write64(h.dcbaa, uint64(arr)+h.BusOff)
	return nil
}

// setupSlots legt de vaste structuren van alle slots vooraf aan. Contexten zijn
// 32 of 64 byte per stuk (HCCPARAMS1.CSZ) en een device context heeft er 32,
// een input context 33 — vandaar het verschil in maat.
func (h *HC) setupSlots() error {
	csz := uintptr(32)
	if h.ctx64 {
		csz = 64
	}
	h.ctxSize = csz
	h.res = make([]*slotRes, h.nSlots+1) // 1-gebaseerd, [0] blijft nil
	for i := 1; i <= h.nSlots; i++ {
		r := &slotRes{}
		var err error
		if r.devCtx, err = h.arena.alloc(csz*32, h.page); err != nil {
			return err
		}
		if r.inCtx, err = h.arena.alloc(csz*33, h.page); err != nil {
			return err
		}
		cr, err := h.arena.alloc(h.page, h.page)
		if err != nil {
			return err
		}
		r.ctrl = newRing(cr, uint64(cr)+h.BusOff, int(h.page))
		for k := range r.intr {
			ir, err := h.arena.alloc(h.page, h.page)
			if err != nil {
				return err
			}
			r.intr[k] = newRing(ir, uint64(ir)+h.BusOff, int(h.page))
		}
		if r.buf, err = h.arena.alloc(4096, h.page); err != nil {
			return err
		}
		h.res[i] = r
	}
	return nil
}

// write64 schrijft een 64-bit registerpaar hoog-eerst. Dat is niet cosmetisch:
// bij CRCR, ERSTBA en ERDP draagt het LAGE woord het bit dat de controller laat
// latchen (RCS, respectievelijk de tabel/leespositie). Laag eerst zou hem laten
// latchen op een adres waarvan de bovenhelft nog oud is.
func (h *HC) write64(addr uintptr, v uint64) {
	dev.Write32(addr+4, uint32(v>>32))
	dev.Write32(addr+0, uint32(v))
}

// doorbell wekt de controller voor een ring. Slot 0 target 0 = de command ring;
// slot n target d = endpoint met DCI d van dat device.
func (h *HC) doorbell(slot, target int) {
	dev.Write32(h.db+uintptr(slot)*4, uint32(target))
	dev.MB()
}

// pump haalt alles wat er in de event-ring klaarstaat op en zet het in de
// wachtrij. Dit is de ENIGE plek die de event-ring leest: één consument, dus er
// is geen slot nodig — en dat is meteen de reden dat alle wachtfuncties hier
// doorheen gaan in plaats van zelf te pollen.
func (h *HC) pump() {
	got := false
	for {
		ev, ok := h.evt.poll()
		if !ok {
			break
		}
		got = true
		if len(h.pending) > 0 && len(h.pending) >= cap(h.pending) {
			// Overloop kan alleen als niemand meer wacht op wat er binnenkomt
			// (een losgetrokken apparaat waarvan de transfers blijven falen).
			// De oudste laten vallen houdt het pad levend; de teller maakt het
			// zichtbaar in plaats van stil.
			h.dropped++
			copy(h.pending, h.pending[1:])
			h.pending = h.pending[:len(h.pending)-1]
		}
		h.pending = append(h.pending, ev)
	}
	if got {
		h.evt.advance()
	}
}

// take pakt het eerste event uit de wachtrij waarvoor match waar is.
func (h *HC) take(match func(event) bool) (event, bool) {
	for i, ev := range h.pending {
		if match(ev) {
			h.pending = append(h.pending[:i], h.pending[i+1:]...)
			return ev, true
		}
	}
	return event{}, false
}

// waitEvent pompt tot er een event langskomt dat aan match voldoet. Andere
// events blijven in de wachtrij staan — een toetsaanslag die binnenkomt terwijl
// we op een commando wachten mag niet verdwijnen.
func (h *HC) waitEvent(match func(event) bool, d time.Duration, what string) (event, error) {
	deadline := time.Now().Add(d)
	for {
		h.pump()
		if ev, ok := h.take(match); ok {
			return ev, nil
		}
		if time.Now().After(deadline) {
			return event{}, fmt.Errorf("xhci %s: geen antwoord op %s binnen %v (USBSTS %#08x)",
				h.Name, what, d, dev.Read32(h.op+opUSBSts))
		}
		time.Sleep(time.Millisecond)
	}
}

// command zet één commando op de command ring, belt aan en wacht op het
// Command Completion Event dat naar precies dít TRB terugwijst. Sequentieel:
// er staat er nooit meer dan één uit.
func (h *HC) command(p0, p1, p2, ctrl uint32, what string) (event, error) {
	trb := h.cmd.push(p0, p1, p2, ctrl)
	h.doorbell(0, 0)
	ev, err := h.waitEvent(func(e event) bool {
		return e.kind == trbCmdCompEvt && e.ptr == trb
	}, time.Second, what)
	if err != nil {
		return ev, err
	}
	if ev.comp != ccSuccess {
		return ev, fmt.Errorf("xhci %s: %s afgewezen — %s (%d)", h.Name, what, compName(ev.comp), ev.comp)
	}
	return ev, nil
}

// Stop halteert de controller. Alleen nodig als HOP het glas teruggeeft of bij
// een herstart van de stack; de datastructuren blijven staan.
func (h *HC) Stop() {
	if !h.running {
		return
	}
	dev.Write32(h.op+opUSBCmd, dev.Read32(h.op+opUSBCmd)&^uint32(cmdRun))
	_ = h.wait(opUSBSts, stsHCH, stsHCH, 500*time.Millisecond, "halt")
	h.running = false
}

// Dropped geeft hoeveel events er zijn weggevallen omdat niemand ze ophaalde —
// nul hoort het te zijn, en anders is het een meting en geen ruis.
func (h *HC) Dropped() int { return h.dropped }
