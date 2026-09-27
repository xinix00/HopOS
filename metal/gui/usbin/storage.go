//go:build gui

package usbin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/xhci"
)

// Opslag op USB, gezien vanaf de rest van de node. De bus heeft één eigenaar
// (de Run-goroutine van de Manager) en xhci is daarom bewust niet
// goroutine-veilig. Een drive-driver die een sector wil, praat dus niet met
// het apparaat maar met die eigenaar: Bulk stuurt elk verzoek over een kanaal
// en wacht op het antwoord. Geen lock om de controller heen — er is maar één
// goroutine die hem aanraakt, en dat is de hele garantie.

// ErrGone: het apparaat is er niet meer. Uitgetrokken, of de controller moest
// zich herstellen en is daarbij elk apparaat vergeten. Een drive die terugkomt
// is een nieuwe Bulk.
var ErrGone = errors.New("usb: device is gone")

// bulkTimeout is hoe lang één bulk-transfer mag duren als de aanroeper zelf
// geen deadline zet. Een optische drive is traag op een manier die niets met
// de bus te maken heeft: na een disc-wissel spint hij op en kan een READ
// seconden stil liggen. Tien seconden is ruim boven wat een drive nodig heeft.
const bulkTimeout = 10 * time.Second

// Bulk is één opslagapparaat met bulk-only transport, als handvat voor andere
// goroutines. Het voldoet aan optical.Transport.
type Bulk struct {
	VendorID  uint16
	ProductID uint16
	Host      string // naam van de controller
	Port      int    // roothub-poort: samen met Host het fysieke adres

	m    *Manager
	hc   *xhci.HC
	dev  *xhci.Device // alleen de eigenaar raakt dit aan
	max  int
	gone chan struct{}

	// deadline is van de aanroeper en wordt alleen door de aanroeper gelezen
	// (call); driver/optical serialiseert zijn commando's per drive.
	deadline time.Time
}

type bulkOp uint8

const (
	opOut bulkOp = iota
	opIn
	opReset
)

// bulkReq is één verzoek aan de eigenaar. Die beantwoordt élk verzoek dat hij
// aannam precies één keer, ook als het apparaat intussen verdween.
type bulkReq struct {
	b        *Bulk
	op       bulkOp
	buf      []byte
	deadline time.Time
	reply    chan bulkReply
}

type bulkReply struct {
	n   int
	err error
}

// Out stuurt bytes naar de BULK-OUT endpoint.
func (b *Bulk) Out(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	_, err := b.call(opOut, p)
	return err
}

// In haalt tot len(p) bytes van de BULK-IN endpoint; korter mag.
func (b *Bulk) In(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return b.call(opIn, p)
}

// ResetRecovery is de BOT-reset: commandostaat weg, beide endpoints vrij.
func (b *Bulk) ResetRecovery() error {
	_, err := b.call(opReset, nil)
	return err
}

// MaxTransfer is wat één Out of In in één keer kan dragen.
func (b *Bulk) MaxTransfer() int { return b.max }

// SetCommandDeadline geldt voor de resterende fasen van één BOT-commando;
// driver/optical wist hem weer na de hele uitwisseling.
func (b *Bulk) SetCommandDeadline(t time.Time) { b.deadline = t }

// Gone sluit zodra het apparaat weg is. Wie het apparaat ergens publiceert,
// haalt het daar weg als dit kanaal dichtgaat.
func (b *Bulk) Gone() <-chan struct{} { return b.gone }

// call legt één verzoek bij de eigenaar neer en wacht op zijn antwoord.
func (b *Bulk) call(op bulkOp, p []byte) (int, error) {
	dl := b.deadline
	if dl.IsZero() {
		dl = time.Now().Add(bulkTimeout)
	} else if !time.Now().Before(dl) {
		return 0, context.DeadlineExceeded
	}
	r := &bulkReq{b: b, op: op, buf: p, deadline: dl, reply: make(chan bulkReply, 1)}
	select {
	case b.m.reqs <- r:
	case <-b.gone:
		return 0, ErrGone
	}
	rep := <-r.reply
	return rep.n, rep.err
}

// inflight is de ene bulk-transfer die op een controller loopt. Eén per
// controller, want de bouncebuffer is per controller gedeeld: twee drives op
// dezelfde controller wachten dus netjes op elkaar.
type inflight struct {
	req *bulkReq
	td  *xhci.BulkTD
}

// enqueue zet een verzoek achteraan de rij van zijn controller.
func (m *Manager) enqueue(r *bulkReq) {
	m.queue[r.b.hc] = append(m.queue[r.b.hc], r)
}

// serveBulk is het bulk-deel van één ronde van de eigenaar: lopende transfers
// afmaken of laten verlopen, en daarna per vrije controller het volgende
// verzoek starten.
func (m *Manager) serveBulk() {
	for hc, f := range m.busy {
		n, done, err := f.td.Poll()
		if !done {
			if time.Now().Before(f.req.deadline) {
				continue
			}
			// De drive zwijgt. Alleen zijn endpoint gaat terug naar nul; de
			// controller en alles wat er verder aan hangt lopen door.
			if aerr := f.td.Abort(); aerr != nil {
				fmt.Printf("usb: %s port %d: aborting a bulk transfer: %v\n", hc.Name, f.req.b.Port, aerr)
			}
			n, err = 0, fmt.Errorf("usb: bulk transfer timed out: %w", context.DeadlineExceeded)
		}
		delete(m.busy, hc)
		f.req.reply <- bulkReply{n, err}
	}
	for hc, q := range m.queue {
		for len(q) > 0 && m.busy[hc] == nil {
			r := q[0]
			q[0] = nil
			q = q[1:]
			m.start(hc, r)
		}
		if len(q) == 0 {
			delete(m.queue, hc)
		} else {
			m.queue[hc] = q
		}
	}
}

// start voert één verzoek uit. Een BOT-reset is een handvol control transfers
// en commando's met hun eigen korte timeouts en loopt dus meteen door; een
// datatransfer wordt alleen gestart en in latere rondes afgemaakt.
func (m *Manager) start(hc *xhci.HC, r *bulkReq) {
	b := r.b
	select {
	case <-b.gone:
		r.reply <- bulkReply{0, ErrGone}
		return
	default:
	}
	if r.op == opReset {
		r.reply <- bulkReply{0, b.dev.ResetRecovery()}
		return
	}
	td, err := b.dev.StartBulk(r.op == opIn, r.buf)
	if err != nil {
		r.reply <- bulkReply{0, err}
		return
	}
	m.busy[hc] = &inflight{req: r, td: td}
	m.fine = fineRounds
}

// dropBulk meldt een apparaat af: Gone gaat dicht en elk verzoek dat nog op
// hem wachtte krijgt ErrGone. Vóór Detach of een controllerreset, want daarna
// kan er van zijn slot niets meer komen.
func (m *Manager) dropBulk(b *Bulk) {
	close(b.gone)
	if f := m.busy[b.hc]; f != nil && f.req.b == b {
		delete(m.busy, b.hc)
		f.req.reply <- bulkReply{0, ErrGone}
	}
	q := m.queue[b.hc]
	keep := q[:0]
	for _, r := range q {
		if r.b == b {
			r.reply <- bulkReply{0, ErrGone}
			continue
		}
		keep = append(keep, r)
	}
	if len(keep) == 0 {
		delete(m.queue, b.hc)
	} else {
		m.queue[b.hc] = keep
	}
}
