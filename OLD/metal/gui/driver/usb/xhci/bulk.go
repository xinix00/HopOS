//go:build gui

package xhci

// Bulk-endpoints: de tweede soort apparaat die deze controller bedient, naast
// boot-HID. Een optische drive (Blu-ray, DVD) meldt zich als mass storage met
// bulk-only transport: één BULK-OUT en één BULK-IN, en daarover een SCSI/MMC-
// gesprek. De commando's zelf staan bewust NIET hier maar in media/driver/optical —
// dit bestand levert alleen de twee pijpen.
//
// Waarom dat mag in dezelfde driver: een bulk-endpoint is voor de controller
// eenvoudiger dan een interrupt-endpoint (geen interval, geen ESIT-payload) en
// hij gebruikt exact dezelfde ring, doorbell en transfer events. De hele winst
// van HID-endpoints — resetRing, recover, de event-matcher — geldt hier één op
// één.
//
// Eén apparaat is in onze wereld óf HID óf opslag, dus de twee ringen die een
// slot voor zijn boot-interfaces heeft worden hier hergebruikt. Dat kost geen
// byte extra DMA en houdt slotRes zoals hij was.

import (
	"errors"
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Mass storage, bulk-only transport (USB Mass Storage Class, Bulk-Only
// Transport 1.0). SubClass 6 is "SCSI transparent command set": een optische
// drive spreekt daarbinnen MMC.
const (
	classMassStorage = 0x08
	subClassSCSI     = 0x06
	protoBulkOnly    = 0x50

	// Endpoint-types in het endpoint-context (xHCI tabel 6-9), bulk-helft.
	epTypeBulkOut = 2
	epTypeBulkIn  = 6

	// CLEAR_FEATURE(ENDPOINT_HALT) op de endpoint (USB 2.0 §9.4.1).
	reqClearFeature  = 1
	featEndpointHalt = 0
)

// bulkIface is de mass-storage-interface van een apparaat: twee endpoints die
// altijd samen horen.
type bulkIface struct {
	num     int // bInterfaceNumber (nodig voor de reset-request van BOT)
	inDCI   int
	outDCI  int
	inMPS   int
	outMPS  int
	inRing  *ring
	outRing *ring
}

// MassStorage zegt of dit apparaat een bulk-only mass-storage-interface heeft.
func (d *Device) MassStorage() bool { return d.bulk != nil }

// HostName is de naam van de controller waar dit apparaat aan hangt: samen
// met Port het fysieke adres, zodat `disc0` in een jobspec terug te vinden is
// op een poort aan de achterkant.
func (d *Device) HostName() string { return d.hc.Name }

// MaxTransfer is hoeveel bytes één Out of In in één keer kan dragen: de
// gedeelde bouncebuffer van deze controller. De aanroeper knipt zijn lees- en
// schrijfopdrachten hierop.
func (d *Device) MaxTransfer() int { return int(d.hc.bulkSize) }

// parseBulk zoekt de bulk-only interface in de configuratiedescriptor. Wordt
// alleen aangeroepen als er geen boot-HID-interface gevonden is.
func (d *Device) parseBulk(b []byte) {
	cur := -1
	var found bulkIface
	for i := 0; i+2 <= len(b); {
		l := int(b[i])
		if l < 2 || i+l > len(b) {
			break
		}
		switch b[i+1] {
		case descInterface:
			cur = -1
			// bAlternateSetting 0, en de drie bytes die bulk-only maken.
			if l >= 9 && b[i+3] == 0 && b[i+5] == classMassStorage && b[i+6] == subClassSCSI && b[i+7] == protoBulkOnly {
				cur = int(b[i+2])
				found = bulkIface{num: cur}
			}
		case descEndpoint:
			// bmAttributes[1:0] == 2 = bulk; bit 7 van het adres = IN.
			if l >= 7 && cur >= 0 && b[i+3]&0x3 == 2 && b[i+2]&0xF != 0 {
				ep := int(b[i+2] & 0xF)
				mps := (int(b[i+4]) | int(b[i+5])<<8) & 0x7FF
				if mps == 0 {
					break
				}
				if b[i+2]&0x80 != 0 {
					if found.inDCI == 0 {
						found.inDCI, found.inMPS = 2*ep+1, mps // IN: DCI = 2N+1
					}
				} else if found.outDCI == 0 {
					found.outDCI, found.outMPS = 2*ep, mps // OUT: DCI = 2N
				}
			}
		}
		i += l
	}
	if found.inDCI != 0 && found.outDCI != 0 {
		d.bulk = &found
	}
}

// configureBulk programmeert de twee endpoints en zet de configuratie. De
// tegenhanger van configure() voor een opslagapparaat: geen SET_PROTOCOL, geen
// armeren, want bulk werkt op verzoek en niet uit zichzelf.
func (d *Device) configureBulk() error {
	h := d.hc
	f := d.bulk
	f.inRing, f.outRing = d.res.intr[0], d.res.intr[1]
	f.inRing.resetRing()
	f.outRing.resetRing()

	maxDCI := f.inDCI
	if f.outDCI > maxDCI {
		maxDCI = f.outDCI
	}
	d.buildInput(maxDCI, uint32(addSlot)|1<<uint(f.inDCI)|1<<uint(f.outDCI))
	in := d.res.inCtx
	set := func(dci, epType, mps int, r *ring) {
		deq := r.deqPtr()
		// DW0 is nul: een bulk-endpoint heeft geen interval, geen mult en
		// geen streams. Verder dezelfde velden als bij interrupt: CErr 3,
		// het type, de pakketgrootte, en de dequeue-pointer met cycle 1.
		dev.Write32(h.ctxDW(in, dci+1, 0), 0)
		dev.Write32(h.ctxDW(in, dci+1, 1), 3<<1|uint32(epType)<<3|uint32(mps)<<16)
		dev.Write32(h.ctxDW(in, dci+1, 2), uint32(deq))
		dev.Write32(h.ctxDW(in, dci+1, 3), uint32(deq>>32))
		// Average TRB Length is een hint voor de scheduler; Max ESIT Payload
		// blijft nul want dat veld is alleen voor periodiek verkeer.
		dev.Write32(h.ctxDW(in, dci+1, 4), uint32(mps))
	}
	set(f.inDCI, epTypeBulkIn, f.inMPS, f.inRing)
	set(f.outDCI, epTypeBulkOut, f.outMPS, f.outRing)
	dev.MB()

	if _, err := h.command(uint32(uint64(in)+h.BusOff), uint32((uint64(in)+h.BusOff)>>32), 0,
		uint32(trbConfigEP)<<trbTypeShift|uint32(d.Slot)<<24, "configure endpoint"); err != nil {
		return err
	}
	if _, err := d.control(0x00, reqSetConfig, uint16(d.confVal), 0, 0); err != nil {
		return fmt.Errorf("set configuration: %w", err)
	}
	return nil
}

// trbMax is hoeveel bytes er in ÉÉN Normal-TRB passen. Het lengteveld is 17
// bits, en een TRB-buffer mag bovendien geen 64KB-grens kruisen (xHCI 4.11.7.1).
// Grotere overdrachten worden dus geketend: alle TRB's op één na krijgen de
// chain-bit, en alleen de laatste vraagt een event aan.
//
// GEMETEN 22-09, en het kostte twee flips: met 140KB in één TRB antwoordde de
// controller niet meer en las de share nul bytes. De 64KB-grens is hard.
const trbMax = 64 << 10

// BulkTD is één lopende bulk-transfer. Er loopt er hoogstens één per
// controller tegelijk, want de bouncebuffer is gedeeld: de eigenaar van de
// controller (usbin) zorgt daarvoor, en ook voor de deadline — deze laag weet
// niet hoe lang een drive mag nadenken.
type BulkTD struct {
	d   *Device
	in  bool
	n   int
	dst []byte // IN: waar de bytes heen gaan zodra hij klaar is
	dci int
	r   *ring
	trb uint64 // het laatste TRB van de TD, het enige met IOC
}

// StartBulk zet één transfer op de ring en belt aan; wachten doet hij niet.
// OUT kopieert buf meteen naar de bouncebuffer, IN leest er pas in als Poll
// de transfer klaar ziet — tot dan moet buf blijven staan.
func (d *Device) StartBulk(in bool, buf []byte) (*BulkTD, error) {
	h := d.hc
	if d.bulk == nil {
		return nil, errors.New("usb: this device has no bulk endpoints")
	}
	if d.res == nil {
		return nil, errDetached
	}
	if h.poisoned != nil {
		return nil, h.poisoned
	}
	if len(buf) == 0 {
		return nil, errors.New("usb: empty bulk transfer")
	}
	if len(buf) > int(h.bulkSize) {
		return nil, fmt.Errorf("usb: %d bytes exceed the %d-byte transfer buffer", len(buf), h.bulkSize)
	}
	td := &BulkTD{d: d, in: in, n: len(buf), dci: d.bulk.outDCI, r: d.bulk.outRing}
	mps := d.bulk.outMPS
	if in {
		td.dst, td.dci, td.r, mps = buf, d.bulk.inDCI, d.bulk.inRing, d.bulk.inMPS
	} else {
		dev.Copy(h.bulkBuf, buf)
		dev.Push(h.bulkBuf, uintptr(len(buf)))
	}
	base := uint64(h.bulkBuf) + h.BusOff
	n := len(buf)
	for off := 0; off < n; off += trbMax {
		length := n - off
		if length > trbMax {
			length = trbMax
		}
		last := off+length >= n
		flags := uint32(trbNormal) << trbTypeShift
		if last {
			flags |= trbISP | trbIOC
		} else {
			flags |= trbChain
		}
		// TD Size: hoeveel pakketten er ná dit TRB nog komen, afgetopt op 31.
		// De controller plant zijn bursts ermee; op het laatste TRB is hij nul
		// (xHCI 4.11.2.4, dezelfde rekensom als xhci_td_remainder in Linux).
		tds := uint32(0)
		if !last && mps > 0 {
			rest := (n - off - length + mps - 1) / mps
			if rest > 31 {
				rest = 31
			}
			tds = uint32(rest)
		}
		bus := base + uint64(off)
		td.trb = td.r.pushTRB(uint32(bus), uint32(bus>>32), uint32(length)|tds<<17, flags, !last)
	}
	h.doorbell(d.Slot, td.dci)
	return td, nil
}

// Poll kijkt of de transfer klaar is. done=false betekent: nog onderweg, vraag
// het straks weer. n is wat er werkelijk overkwam; een korte IN is normaal en
// geen fout, de drive mag minder geven dan gevraagd.
func (td *BulkTD) Poll() (n int, done bool, err error) {
	d, h := td.d, td.d.hc
	h.pump()
	ev, ok := h.take(func(e event) bool {
		return e.kind == trbTransferEvt && e.ptr == td.trb
	})
	if !ok {
		if h.poisoned != nil {
			// Deze controller moet eerst gereset worden; daar komt deze TD
			// nooit meer uit.
			return 0, true, h.poisoned
		}
		return 0, false, nil
	}
	switch ev.comp {
	case ccSuccess, ccShortPacket:
		if ev.rem > uint32(td.n) {
			return 0, true, h.quarantine(errors.New("bulk completion exceeds transfer length"))
		}
		got := td.n - int(ev.rem)
		if td.in && got > 0 {
			dev.Pull(h.bulkBuf, uintptr(got))
			dev.CopyOut(td.dst[:got], h.bulkBuf)
		}
		return got, true, nil
	case ccStall:
		// Een gestalde bulk-endpoint is bij mass storage geen ongeluk maar een
		// gespreksvorm: de drive stalt de datafase als hij een commando niet
		// aankan en verwacht dat de host de endpoint vrijmaakt en de status
		// ophaalt. Dus herstellen en de aanroeper laten beslissen.
		if err := d.clearHalt(td.dci, td.r, td.in); err != nil {
			return 0, true, err
		}
		return 0, true, errStalled
	default:
		return 0, true, fmt.Errorf("bulk %s (%d)", compName(ev.comp), ev.comp)
	}
}

// Abort breekt een transfer af die zijn deadline haalde: die ene endpoint
// stoppen en op een verse ring zetten, zoals Linux een verlopen URB
// annuleert. De controller blijft gewoon draaien — dat een drive niet
// antwoordt zegt niets over de bus, en het toetsenbord ernaast heeft er geen
// last van. De BOT-laag doet daarna zelf zijn reset.
func (td *BulkTD) Abort() error { return td.d.resetEP(td.dci, td.r) }

// errStalled meldt een gestalde endpoint die weer vrij is. Alleen de BOT-laag
// weet wat dat betekent: status ophalen en de fout van de drive zelf lezen —
// en dat doet die bij élke fout in de datafase, dus niemand hoeft hem te
// herkennen.
var errStalled = errors.New("usb: endpoint stalled")

// clearHalt haalt een endpoint uit halted aan beide kanten: eerst de
// controller (resetEP), dan het apparaat zelf (CLEAR_FEATURE op de endpoint,
// dat ook zijn data-toggle terugzet). Die volgorde is die van de USB-spec en
// van Linux.
func (d *Device) clearHalt(dci int, r *ring, in bool) error {
	if err := d.resetEP(dci, r); err != nil {
		return err
	}
	ep := uint16(dci / 2)
	if in {
		ep |= 0x80
	}
	_, err := d.control(0x02, reqClearFeature, featEndpointHalt, ep, 0)
	return err
}

// ResetRecovery is de mass-storage-reset van BOT: het apparaat gooit zijn
// commandostaat weg en beide endpoints komen uit halted. De uitweg als host en
// drive het spoor bijster zijn.
func (d *Device) ResetRecovery() error {
	if d.bulk == nil {
		return errors.New("usb: this device has no bulk endpoints")
	}
	// Class-specific request 0xFF op de interface (BOT 1.0 §3.1).
	if _, err := d.control(0x21, 0xFF, 0, uint16(d.bulk.num), 0); err != nil {
		return fmt.Errorf("bulk-only mass storage reset: %w", err)
	}
	if err := d.clearHalt(d.bulk.inDCI, d.bulk.inRing, true); err != nil {
		return err
	}
	return d.clearHalt(d.bulk.outDCI, d.bulk.outRing, false)
}
