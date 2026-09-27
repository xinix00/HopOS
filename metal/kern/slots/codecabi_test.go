//go:build media

package slots

import (
	"strings"
	"sync"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// De grant is de enige plek waar een adres van buiten HOP binnenkomt, en wat
// er doorheen komt wordt daarna door een DMA-motor beschreven. Die vraagt niet
// nog eens of het mocht, dus alle argwaan zit hier — en hoort dus getest.
func TestCodecGrantLaatAlleenDeEigenPartitieDoor(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 0x4000000}}) // 64MB
	base, size, err := partAlloc(1, 32<<20)
	if err != nil {
		t.Fatal(err)
	}

	// Binnen de partitie: goed, en het fysieke adres is basis + afstand.
	b, err := codecGrant(1, 4096, 8192)
	if err != nil {
		t.Fatalf("een buffer binnen de partitie werd geweigerd: %v", err)
	}
	if uint64(b.PA) != base+4096 || b.Size != 8192 {
		t.Errorf("grant gaf %#x+%d, verwacht %#x+%d", b.PA, b.Size, base+4096, 8192)
	}

	// Precies tot de rand mag.
	if _, err := codecGrant(1, size-4096, 4096); err != nil {
		t.Errorf("de laatste pagina van de partitie werd geweigerd: %v", err)
	}

	for _, c := range []struct {
		naam   string
		off, n uint64
		wil    string
	}{
		{"één byte voorbij het einde", size - 4096, 8192, "outside"},
		{"begint voorbij het einde", size, 4096, "outside"},
		{"leeg", 0, 0, "zero"},
		{"niet op een pagina", 512, 4096, "whole number"},
		{"lengte niet op een pagina", 0, 4097, "whole number"},
		// De optelling mag niet omlopen: off+n wordt dan weer klein en zou de
		// grenstoets halen terwijl de buffer overal ligt behalve waar het mag.
		{"omloop", 1 << 63, 1 << 63, "outside"},
	} {
		_, err := codecGrant(1, c.off, c.n)
		if err == nil {
			t.Errorf("%s: doorgelaten", c.naam)
			continue
		}
		if !strings.Contains(err.Error(), c.wil) {
			t.Errorf("%s: %v (verwachtte iets over %q)", c.naam, err, c.wil)
		}
	}

	// Een slot zonder partitie krijgt niets, wat het ook vraagt.
	if _, err := codecGrant(2, 0, 4096); err == nil {
		t.Error("een slot zonder partitie kreeg een grant")
	}
}

// fakeCodec is een codec-blok zonder ijzer: sessies houden bij of ze dicht
// zijn en geven de events terug die de test klaarzet.
type fakeCodec struct {
	mu     sync.Mutex
	opened []*fakeSession
	onOpen func() // draait midden in Open, zoals een firmware-lading
}

func (e *fakeCodec) Describe() string                           { return "fake" }
func (e *fakeCodec) Supports(codec.Codec, codec.Direction) bool { return true }
func (e *fakeCodec) Open(codec.Config) (codec.Session, error) {
	if e.onOpen != nil {
		e.onOpen()
	}
	s := &fakeSession{}
	e.mu.Lock()
	e.opened = append(e.opened, s)
	e.mu.Unlock()
	return s, nil
}

type fakeSession struct {
	mu     sync.Mutex
	closed bool
	events []codec.Event
}

func (s *fakeSession) Feed(codec.Buffer, uint64, codec.Flag, uint64) error { return nil }
func (s *fakeSession) Offer(codec.Buffer) error                            { return nil }
func (s *fakeSession) Poll() (codec.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return codec.Event{}, false
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, true
}
func (s *fakeSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *fakeSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// codecFixture geeft slot 1 een partitie en de node een nep-codec.
func codecFixture(t *testing.T) (*fakeCodec, uint64) {
	t.Helper()
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 0x4000000}})
	base, _, err := partAlloc(1, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeCodec{}
	codecMu.Lock()
	oldEngine, oldOwner := codecEngine, codecOwner
	codecMu.Unlock()
	UseCodec(e)
	t.Cleanup(func() {
		codecMu.Lock()
		codecEngine, codecOwner = oldEngine, oldOwner
		codecMu.Unlock()
	})
	return e, base
}

func codecCall(t *testing.T, s *servicer, req hopabi.Req) hopabi.Resp {
	t.Helper()
	resp, err := hopabi.DecodeResp(s.codecServe(req))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func codecOpenReq() hopabi.Req {
	return hopabi.Req{Op: hopabi.OpCodecOpen, Data: hopabi.EncodeOpen(hopabi.OpenArgs{Codec: 1})}
}

func codecPollReq(h uint32) hopabi.Req {
	return hopabi.Req{Op: hopabi.OpCodecPoll, Data: hopabi.EncodeBuf(hopabi.BufArgs{Handle: h})}
}

func codecOwned(s codec.Session) bool {
	codecMu.Lock()
	defer codecMu.Unlock()
	_, ok := codecOwner[s]
	return ok
}

// Een verzoek van de vorige huurder dat nog in de lucht is als het slot wordt
// opgeruimd (evictServicer wacht niet op serveSystemConn) mag geen sessietabel
// laten herrijzen. Vroeger maakte een misser er een verse aan onder het
// slotnummer, en dan stuurde handvat 1 van de volgende huurder de sessie van
// de vorige.
func TestCodecTabelOverleeftDeHuurderNiet(t *testing.T) {
	e, _ := codecFixture(t)
	old := &servicer{slot: 1, stop: make(chan struct{}), done: make(chan struct{}), cancel: func() {}}
	close(old.done)
	svcMu.Lock()
	servicers[1] = old
	svcMu.Unlock()
	t.Cleanup(func() {
		svcMu.Lock()
		delete(servicers, 1)
		svcMu.Unlock()
	})

	if r := codecCall(t, old, codecOpenReq()); r.Status != hopabi.StatusOK || r.Size != 1 {
		t.Fatalf("open: status %d handvat %d", r.Status, r.Size)
	}
	ses := e.opened[0]

	evictServicer(1)
	if !ses.isClosed() {
		t.Fatal("de sessie van de opgeruimde huurder staat nog open")
	}

	// Het late verzoek van de oude verbinding: geen sessie meer, en ook geen
	// nieuwe tabel waar een Open in kan landen.
	if r := codecCall(t, old, codecPollReq(1)); r.Status != hopabi.StatusNoEnt {
		t.Fatalf("laat verzoek na opruimen: status %d, verwacht NoEnt", r.Status)
	}
	if r := codecCall(t, old, codecOpenReq()); r.Status == hopabi.StatusOK {
		t.Fatal("een opgeruimde huurder kon nog een sessie openen")
	}
	late := e.opened[1]
	if !late.isClosed() || codecOwned(late) {
		t.Fatal("de sessie van een Open na het opruimen lekt")
	}

	// De opvolger begint met een lege tabel: zijn handvat 1 is niet dat van
	// de vorige.
	next := &servicer{slot: 1}
	if r := codecCall(t, next, codecPollReq(1)); r.Status != hopabi.StatusNoEnt {
		t.Fatalf("opvolger zag handvat 1 van zijn voorganger: status %d", r.Status)
	}
}

// Een Open laadt firmware en duurt dus even. Wordt de taak juist dan
// opgeruimd, dan mag de sessie die daarna klaarkomt niet blijven hangen: er
// zijn er maar een handvol op de hele node.
func TestCodecOpenTijdensOpruimenSluitZichZelf(t *testing.T) {
	e, _ := codecFixture(t)
	s := &servicer{slot: 1}
	e.onOpen = s.codec.shut
	if r := codecCall(t, s, codecOpenReq()); r.Status == hopabi.StatusOK {
		t.Fatal("Open slaagde terwijl de taak halverwege werd opgeruimd")
	}
	if ses := e.opened[0]; !ses.isClosed() || codecOwned(ses) {
		t.Fatal("de hardware-sessie van een onderbroken Open lekt")
	}
}

// Close geeft de buffers van een sessie terug aan de app; hun boekhouding
// hoort dan mee te gaan, anders groeit held met elke gesloten sessie.
func TestCodecCloseVergeetZijnBuffers(t *testing.T) {
	codecFixture(t)
	s := &servicer{slot: 1}
	for i := 0; i < 2; i++ {
		codecCall(t, s, codecOpenReq())
	}
	feed := func(h uint32, off uint64) {
		r := codecCall(t, s, hopabi.Req{Op: hopabi.OpCodecFeed, Off: off, N: 4096,
			Data: hopabi.EncodeFeed(hopabi.FeedArgs{Handle: h, Filled: 1})})
		if r.Status != hopabi.StatusOK {
			t.Fatalf("feed: status %d %s", r.Status, r.Data)
		}
	}
	feed(1, 0)
	feed(1, 4096)
	feed(2, 8192)
	codecCall(t, s, hopabi.Req{Op: hopabi.OpCodecClose, Data: hopabi.EncodeBuf(hopabi.BufArgs{Handle: 1})})
	s.codec.mu.Lock()
	defer s.codec.mu.Unlock()
	if len(s.codec.held) != 1 {
		t.Fatalf("na Close nog %d buffers in de boekhouding, verwacht alleen die van sessie 2", len(s.codec.held))
	}
	for _, b := range s.codec.held {
		if b.handle != 2 {
			t.Fatalf("buffer van sessie %d overleefde zijn Close", b.handle)
		}
	}
}

// Een event dat niet te vertalen is, is al uit de sessie gehaald. Het mag de
// rest van de poll niet meenemen: het gaat als Fault mee, en wat erna kwam
// komt gewoon aan.
func TestCodecPollVerliestGeenEvents(t *testing.T) {
	e, base := codecFixture(t)
	s := &servicer{slot: 1}
	codecCall(t, s, codecOpenReq())
	e.opened[0].events = []codec.Event{
		{Kind: codec.Consumed, Tag: 7, Buf: codec.Buffer{PA: uintptr(base - 4096), Size: 4096}},
		{Kind: codec.Consumed, Tag: 8, Buf: codec.Buffer{PA: uintptr(base + 4096), Size: 4096}},
	}
	r := codecCall(t, s, codecPollReq(1))
	if r.Status != hopabi.StatusOK || r.Size != 2 {
		t.Fatalf("poll: status %d, %d events (%s)", r.Status, r.Size, r.Data)
	}
	first, _ := hopabi.DecodeEvent(r.Data)
	second, _ := hopabi.DecodeEvent(r.Data[hopabi.EventLen:])
	if first.Kind != hopabi.EventFault || first.Tag != 7 {
		t.Errorf("onvertaalbaar event: %+v, verwacht een Fault met tag 7", first)
	}
	if second.Kind != hopabi.EventConsumed || second.Tag != 8 || second.Off != 4096 {
		t.Errorf("volgend event: %+v", second)
	}
}
