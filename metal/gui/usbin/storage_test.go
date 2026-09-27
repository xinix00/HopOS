//go:build gui

package usbin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/xhci"
)

func newTestBulk(m *Manager, hc *xhci.HC) *Bulk {
	return &Bulk{m: m, hc: hc, gone: make(chan struct{}), max: 4096}
}

// takeReq speelt de eigenaar: één verzoek aannemen zoals Run dat doet.
func takeReq(t *testing.T, m *Manager) *bulkReq {
	t.Helper()
	select {
	case r := <-m.reqs:
		m.enqueue(r)
		return r
	case <-time.After(time.Second):
		t.Fatal("geen verzoek bij de eigenaar aangekomen")
		return nil
	}
}

func TestUitgetrokkenDriveBeantwoordtElkVerzoekMetErrGone(t *testing.T) {
	m := New(nil)
	hc := new(xhci.HC)
	a, b := newTestBulk(m, hc), newTestBulk(m, hc)

	// a heeft een transfer lopen, b wacht erachter op dezelfde controller.
	errA := make(chan error, 1)
	go func() { _, err := a.In(make([]byte, 512)); errA <- err }()
	ra := takeReq(t, m)
	m.queue[hc] = m.queue[hc][:0]
	m.busy[hc] = &inflight{req: ra}
	errB := make(chan error, 1)
	go func() { _, err := b.In(make([]byte, 512)); errB <- err }()
	takeReq(t, m)

	m.dropBulk(a)

	if err := <-errA; !errors.Is(err, ErrGone) {
		t.Fatalf("lopende transfer van a: %v, wil ErrGone", err)
	}
	select {
	case <-a.Gone():
	default:
		t.Fatal("Gone van a bleef open")
	}
	if m.busy[hc] != nil {
		t.Fatal("de controller bleef bezet door een apparaat dat weg is")
	}
	// b is niet van a en moet gewoon blijven wachten op zijn beurt.
	if q := m.queue[hc]; len(q) != 1 || q[0].b != b {
		t.Fatalf("rij na afmelden van a: %+v", q)
	}
	select {
	case err := <-errB:
		t.Fatalf("b kreeg een antwoord terwijl hij nog in de rij stond: %v", err)
	default:
	}

	// Een nieuw verzoek aan a komt niet eens meer bij de eigenaar.
	if _, err := a.In(make([]byte, 512)); !errors.Is(err, ErrGone) {
		t.Fatalf("verzoek na afmelden: %v, wil ErrGone", err)
	}
	m.dropBulk(b)
	if err := <-errB; !errors.Is(err, ErrGone) {
		t.Fatalf("wachtend verzoek van b: %v, wil ErrGone", err)
	}
}

func TestControllerherstelMeldtOpslagAf(t *testing.T) {
	m := New(nil)
	hc := new(xhci.HC)
	b := newTestBulk(m, hc)
	known := map[int]*port{1: {bulk: b}}
	m.forgetController(known)
	select {
	case <-b.Gone():
	default:
		t.Fatal("een controllerreset liet de drive gepubliceerd staan")
	}
}

func TestVerlopenDeadlineBereiktDeEigenaarNiet(t *testing.T) {
	m := New(nil)
	b := newTestBulk(m, new(xhci.HC))
	b.SetCommandDeadline(time.Now().Add(-time.Millisecond))
	// Niemand leest m.reqs: een verzoek dat toch verstuurd werd, zou hier
	// blijven hangen.
	if _, err := b.In(make([]byte, 512)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verlopen deadline: %v", err)
	}
}

func TestLegeTransferIsGeenVerzoek(t *testing.T) {
	m := New(nil)
	b := newTestBulk(m, new(xhci.HC))
	if err := b.Out(nil); err != nil {
		t.Fatal(err)
	}
	if n, err := b.In(nil); n != 0 || err != nil {
		t.Fatalf("In(nil) = %d, %v", n, err)
	}
}
