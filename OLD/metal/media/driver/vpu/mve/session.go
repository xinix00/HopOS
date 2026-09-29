package mve

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// De descriptor-formaten van de firmware. Byte-exact: de firmware leest deze
// structuren rechtstreeks uit de ring, dus een verschoven veld is geen
// foutmelding maar een decoder die in het wilde weg DMA't.
const (
	// mve_buffer_frame, 72 bytes.
	bfHostHandle    = 0
	bfUserTag       = 8
	bfFlags         = 16
	bfVisibleHeight = 20
	bfVisibleWidth  = 22
	bfFormat        = 24
	bfPlaneTop      = 28 // 3 × uint32
	bfPlaneBot      = 40 // 3 × uint32
	bfStride        = 52 // 3 × int32
	bfMaxWidth      = 64
	bfMaxHeight     = 66
	bfSize          = 72

	// mve_buffer_bitstream, 40 bytes.
	bsHostHandle = 0
	bsUserTag    = 8
	bsFlags      = 16
	bsAllocBytes = 20
	bsOffset     = 24
	bsFilledLen  = 28
	bsBufAddr    = 32
	bsFrameType  = 36
	bsSize       = 40

	// mve_rpc_communication_area.
	rpcState       = 0
	rpcCallID      = 4
	rpcSize        = 8
	rpcParams      = 12
	rpcStateFree   = 0
	rpcStateParam  = 1
	rpcStateReturn = 2

	rpcPrintf = 1
	rpcAlloc  = 2
	rpcResize = 3
	rpcFree   = 4

	rpcRegionProtected = 0
	rpcRegionFrameBuf  = 1
)

// held is een buffer die bij de firmware ligt. id is het nummer waaronder de
// firmware hem kent (host_handle); die geeft hij ongewijzigd terug, en zo
// vinden wij de buffer terug zonder op adressen te hoeven zoeken.
type held struct {
	id    uint64
	buf   codec.Buffer
	va    uint32
	pages uint32
	tag   uint64
	input bool
}

// owned is geheugen dat de firmware zelf vroeg (referentieframes en interne
// werkruimte). Bij 4K HEVC is dit het leeuwendeel van de arena — vandaar dat
// het bij de sessie hoort en niet bij de aanroeper: sluit de sessie, dan is
// het in één keer weg.
//
// Eén blok kan uit meer fysieke stukken bestaan: een resize hangt een nieuw
// stuk arena achter de pagina's die er al stonden. Virtueel is het één blok,
// fysiek zijn het losse toewijzingen die elk alleen hun eigen pagina's
// teruggeven.
type owned struct {
	va    uint32
	span  uint32 // gereserveerde virtuele ruimte (max_size)
	pages uint32 // gemapte pagina's, alle stukken samen
	parts []span // de fysieke stukken, in virtuele volgorde
}

// free geeft alle fysieke stukken van een blok terug aan de arena.
func (o *owned) free(a *Arena) {
	for _, p := range o.parts {
		a.Free(p.pa, p.pages)
	}
	o.parts = nil
}

type session struct {
	d     *Device
	cfg   codec.Config
	lsid  int
	pixel uint16

	mu   sync.Mutex
	mmu  *mmu
	msg  ring
	bin  ring
	bout ring
	rpc  uintptr

	reg     regions // de adresindeling van DEZE firmwareversie
	vaFrame vaRegion
	vaProt  vaRegion
	placed  map[uintptr]*placedBuf // fysieke buffer → zijn vaste plek
	bufs    map[uint64]*held
	rpcMem  []owned
	handle  uint64

	events []codec.Event
	layout codec.Layout
	seq    struct {
		known      bool
		chroma     uint8
		bitdepth   uint8
		minBuffers int
	}
	alloc struct {
		known         bool
		width, height uint16
		cropX, cropY  uint16
	}
	closed  bool
	failed  error
	jobLive bool // draait er een beurt bij de firmware?

	// Na SEQUENCE_PARAMETERS zet de firmware zijn UITVOERPOORT stil en komt
	// er pas weer uit na een output-flush: hij geeft dan alle uitvoerbuffers
	// terug, meldt OUTPUT_FLUSHED, en pas daarna mag de host opnieuw
	// aanbieden (mve_protocol_def.h, boven mve_response_sequence_parameters).
	// Wie dat overslaat ziet een decoder die de stream keurig herkent en
	// daarna niets meer doet — de uitvoerring wordt letterlijk niet gelezen.
	outHold bool           // de poort staat stil, wij wachten op de flush
	outAck  bool           // OUTPUT_FLUSHED gezien; buffers mogen weer
	outPend []codec.Buffer // wat de aanroeper intussen aanbood

	// Tellers voor de bring-up. Een beeld dat onderweg verdwijnt is aan de
	// buitenkant niet van een beeld dat er nooit was te onderscheiden; deze
	// vier maken het verschil zichtbaar zonder een tracelogboek.
	stat struct {
		decodeOnly    int // gedecodeerd maar niet om te tonen
		corrupt       int
		rejected      int // buffer was te klein
		unknown       int // handvat dat wij niet (meer) kenden
		refFrame      int
		streamCorrupt int // de firmware klaagde over de bitstream zelf
		flushes       int // hoe vaak de uitvoerpoort stilgezet werd
		flushBack     int // buffers die de flush terugbracht
		eos           int // beelden met de EOS-vlag
		rpcAllocs     int // geheugenverzoeken van de firmware
		rpcPages      uint32
		offers        int // buffers die wij aanboden
		returns       int // buffers die terugkwamen
	}
}

// start bouwt de adresruimte van een sessie en zet hem op een hardware-slot.
func (s *session) start(bin []byte, h fwHeader) error {
	m, err := newMMU(s.d.arena)
	if err != nil {
		return err
	}
	s.mmu = m
	s.reg = regionsFor(h.ProtocolMajor)
	s.vaFrame = vaRegion{beg: s.reg.frameBeg, end: s.reg.frameEnd, next: s.reg.frameBeg}
	s.vaProt = vaRegion{beg: s.reg.protBeg, end: s.reg.protEnd, next: s.reg.protBeg}
	s.placed = map[uintptr]*placedBuf{}

	if _, err := loadFW(m, bin, h); err != nil {
		return err
	}

	// De zeven communicatiepagina's op hun vaste adressen. De firmware is
	// hierop gelinkt; deze nummers zijn geen keuze van ons.
	var comm [7]uintptr
	for i, va := range [7]uint32{vaMsgInQ, vaMsgOutQ, vaBufInQ, vaBufInRQ, vaBufOutQ, vaBufOutRQ, vaRPC} {
		pa, err := m.alloc(va, 1, accessRW)
		if err != nil {
			return err
		}
		comm[i] = pa
	}

	// Deze firmware wil het checksum-woord: alle O6N-blobs dragen "-sum" in
	// hun versiestring. We lezen dat uit de binary in plaats van het aan te
	// nemen, zodat een toekomstige blob zonder die eis ook werkt.
	csum := hasSum(h.Version)
	s.msg = ring{host: comm[0], mve: comm[1], csum: csum}
	s.bin = ring{host: comm[2], mve: comm[3], csum: csum}
	s.bout = ring{host: comm[4], mve: comm[5], csum: csum}
	s.rpc = comm[6]

	if err := s.mapLSID(); err != nil {
		return err
	}
	// GO zet de sessie in de werkende staat, maar dat alleen laat de firmware
	// nog niets doen: hij wacht op een JOB die zegt met hoeveel cores hij mag
	// werken en hoeveel beelden hij deze beurt mag afmaken. Nul beelden
	// betekent "tot ik je stop". Gemeten op ijzer 22-09: zonder dit bericht
	// leest de firmware wel zijn berichtenring maar raakt hij de
	// invoerbuffers niet aan.
	if err := s.msg.send(reqGo, nil); err != nil {
		return err
	}
	s.startJob()
	s.schedule()
	return nil
}

// startJob geeft de firmware een beurt, als er nog geen loopt. Een JOB is
// eenmalig: na afloop meldt de firmware JOB_DEQUEUED of IDLE en doet hij niets
// meer tot er een nieuwe komt. Wie dat niet weet ziet een decoder die keurig
// de stream herkent en daarna zwijgt — gemeten op ijzer 22-09.
func (s *session) startJob() {
	if s.jobLive {
		return
	}
	if err := s.msg.send(reqJob, jobRequest(1, 0)); err != nil {
		return
	}
	s.jobLive = true
}

// jobRequest bouwt een mve_request_job: hoeveel cores en hoeveel beelden.
func jobRequest(cores, frames uint16) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint16(b[0:], cores)
	binary.LittleEndian.PutUint16(b[2:], frames)
	binary.LittleEndian.PutUint32(b[4:], 0) // geen vlaggen
	return b[:]
}

// mapLSID zet de sessie op zijn hardware-slot. De volgorde is die van het
// ijzer: eerst claimen en afbreken wat er stond, dan de tabel aanwijzen, en
// pas als alles staat het inplannen aanzetten.
func (s *session) mapLSID() error {
	r := s.d.r
	id := s.lsid
	r.writeLSID(id, lsAlloc, allocNonProtected)
	r.writeLSID(id, lsTerminate, 1)
	for i := 0; ; i++ {
		if r.readLSID(id, lsTerminate) == 0 {
			break
		}
		if i > 100000 {
			return fmt.Errorf("mve: session slot %d will not terminate", id)
		}
		// Het ijzer wist dit bit zelf; wachten hoort niet een hele core te
		// kosten. Op ijzer duurt het microseconden, dus dit is gratis.
		runtime.Gosched()
	}
	// Eén core per sessie: geen enkele core verboden, maximaal één tegelijk.
	// Meer cores zijn er voor 8K; voor 4K Blu-ray is één ruim, en spreiden
	// kost een tweede firmware-kopie per extra core.
	r.writeLSID(id, lsCtrl, 1<<ctrlMaxCoresShift)
	// MMU_CTRL krijgt een VOLLEDIGE page-table-entry, niet het paginanummer:
	// hetzelfde formaat als elke andere entry, inclusief attribuut en
	// toegangsrecht. Alleen het nummer schrijven laat de VPU op een adres
	// kijken dat vier keer te klein is, met toegangsrecht nul — hij start dan
	// netjes op en zwijgt vervolgens, want hij kan zijn eigen code niet
	// vinden. Gemeten op ijzer 22-09: precies dat beeld.
	r.writeLSID(id, lsMMUCtrl, pte(attrPrivate, s.mmu.table(), accessRW))
	r.writeLSID(id, lsFlushAll, 0)
	r.writeLSID(id, lsNProt, 1)
	r.writeLSID(id, lsStreamID, 0)
	for i := uintptr(0); i < 4; i++ {
		r.writeLSID(id, lsBusAttr0+i*4, 0)
	}
	r.writeLSID(id, lsLIRQVE, 0)
	r.writeLSID(id, lsIRQHost, 0)
	dev.MB()
	r.writeLSID(id, lsSched, 1)
	return nil
}

// schedule zet deze sessie in de job-queue van de hardware-scheduler en tikt
// de firmware aan. Het inplannen gaat uit terwijl we de queue aanpassen: dat
// is wat het ijzer wil, en het duurt drie registerschrijfacties.
func (s *session) schedule() {
	r := s.d.r
	s.d.mu.Lock()
	r.write(regEnable, 0)
	q := r.read(regJobQueue)

	// Altijd opnieuw inschrijven, nooit "staat er al" concluderen. De
	// hardware laat een opgepakte job achter als 0x00 — lsid 0 met nul cores —
	// en dat is voor een sessie op slot 0 niet te onderscheiden van een job
	// die nog wacht. Wie daarop vertrouwt schrijft nooit meer een tweede
	// beurt, en de decoder zwijgt na zijn eerste (gemeten 22-09).
	slot := -1
	for i := 0; i < jobSlots; i++ {
		if jobSlotLSID(q, i) == uint32(s.lsid) {
			slot = i
			break
		}
	}
	if slot < 0 {
		for i := 0; i < jobSlots; i++ {
			if jobSlotLSID(q, i) == jobInvalid {
				slot = i
				break
			}
		}
	}
	if slot >= 0 {
		r.write(regJobQueue, setJobSlot(q, slot, uint32(s.lsid), 1))
	}
	r.write(regEnable, 1)
	s.d.mu.Unlock()
	s.kick()
}

// kick wekt de firmware — hetzelfde als send_irq in de Linux-driver. Elk
// bericht dat we in een ring leggen moet met zo'n tik komen; zonder tik ligt
// het er tot de firmware toevallig zelf kijkt.
func (s *session) kick() {
	dev.MB()
	s.d.r.writeLSID(s.lsid, lsIRQHost, 1)
}

// unschedule haalt de sessie uit de job-queue.
func (s *session) unschedule() {
	r := s.d.r
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	r.write(regEnable, 0)
	q := r.read(regJobQueue)
	out := uint32(emptyJobQueue)
	j := 0
	for i := 0; i < jobSlots; i++ {
		if jobSlotLSID(q, i) != uint32(s.lsid) {
			out = out&^(0xff<<(j*8)) | ((q>>(i*8))&0xff)<<(j*8)
			j++
		}
	}
	r.write(regJobQueue, out)
	r.write(regEnable, 1)
}

// Feed voert bitstream (decode) of een frame (encode) in.
func (s *session) Feed(b codec.Buffer, n uint64, f codec.Flag, tag uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	h, err := s.attach(b, tag, true)
	if err != nil {
		return err
	}
	var desc []byte
	if s.cfg.Dir == codec.Decode {
		desc = s.bitstreamDesc(h, n, f)
	} else {
		desc = s.frameDesc(h, f, true)
	}
	code := uint16(bufBitstream)
	if s.cfg.Dir == codec.Encode {
		code = bufFrame
	}
	if err := s.bin.send(code, desc); err != nil {
		s.detach(h)
		return err
	}
	// Geen nieuwe JOB hier. Er loopt er één met frames=0 ("oneindig") en de
	// volgende komt pas als de firmware deze teruggeeft (JOB_DEQUEUED). Een
	// job per buffer loopt zijn wachtrij vol en eindigt in "no space in job
	// queue" — gemeten op ijzer 22-09, mét frames eruit die daarvoor al goed
	// gingen. Buffers aanbieden en aantikken is genoeg.
	s.schedule()
	return nil
}

// Offer biedt een lege buffer aan voor het resultaat.
func (s *session) Offer(b codec.Buffer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if s.outHold {
		// De firmware kijkt nu niet in de uitvoerring. Vasthouden en
		// aanbieden zodra de flush rond is; de aanroeper merkt er niets van
		// behalve dat zijn buffer even later pas bij het ijzer ligt.
		s.outPend = append(s.outPend, b)
		return nil
	}
	return s.sendOutput(b)
}

// sendOutput hangt één lege buffer in de uitvoerring van de firmware.
func (s *session) sendOutput(b codec.Buffer) error {
	h, err := s.attach(b, 0, false)
	if err != nil {
		return err
	}
	var desc []byte
	code := uint16(bufFrame)
	if s.cfg.Dir == codec.Decode {
		desc = s.frameDesc(h, 0, false)
	} else {
		desc = s.bitstreamDesc(h, 0, 0)
		code = bufBitstream
	}
	if err := s.bout.send(code, desc); err != nil {
		s.detach(h)
		return err
	}
	s.schedule()
	return nil
}

// Poll verwerkt wat de firmware achterliet en geeft het volgende event. Na
// Close doet hij niets meer: de ringen en de RPC-pagina zijn dan terug in de
// arena en misschien al van een andere sessie, en het hardware-slot is weg.
func (s *session) Poll() (codec.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return codec.Event{}, false
	}
	s.pump()
	if len(s.events) == 0 {
		return codec.Event{}, false
	}
	e := s.events[0]
	s.events = s.events[1:]
	return e, true
}

// Close breekt de sessie af en geeft alles terug.
func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = s.msg.send(reqStop, nil)
	s.unschedule()
	s.release()
	return nil
}

// release geeft het hardware-slot en al het geheugen van deze sessie terug.
// Ook het pad voor een half opgestarte sessie, dus alles is nil-bestendig.
func (s *session) release() {
	r := s.d.r
	if s.lsid >= 0 {
		r.writeLSID(s.lsid, lsSched, 0)
		r.writeLSID(s.lsid, lsTerminate, 1)
		r.writeLSID(s.lsid, lsAlloc, allocFree)
		s.d.release(s.lsid)
		s.lsid = -1
	}
	// Eerst wat de firmware zelf vroeg: die pagina's komen rechtstreeks uit de
	// arena en staan dus niet in de boekhouding van de page table. Bij 4K is
	// dit het leeuwendeel van de arena — vergeten betekent dat de tweede film
	// nergens meer geheugen vindt.
	for i := range s.rpcMem {
		s.rpcMem[i].free(s.d.arena)
	}
	s.rpcMem = nil
	if s.mmu != nil {
		s.mmu.destroy()
		s.mmu = nil
	}
	s.bufs = nil
}

// usable meldt of de sessie nog aanvragen aanneemt.
func (s *session) usable() error {
	if s.closed {
		return codec.ErrClosed
	}
	if s.failed != nil {
		return s.failed
	}
	return nil
}

// attach hangt een buffer van de aanroeper in de adresruimte van de firmware.
func (s *session) attach(b codec.Buffer, tag uint64, input bool) (*held, error) {
	if b.PA == 0 || b.Size == 0 || uint64(b.PA)&(pageSize-1) != 0 {
		return nil, fmt.Errorf("mve: buffer %#x+%d is not a whole page", b.PA, b.Size)
	}
	// De firmware ziet 32-bit adressen. Groter past nergens, en het
	// paginatal zou in 32 bits afgekapt worden tot iets wat wél lijkt te passen.
	if b.Size > 1<<32-pageSize {
		return nil, fmt.Errorf("mve: buffer %#x+%d is larger than the firmware address space", b.PA, b.Size)
	}
	pages := uint32((b.Size + pageSize - 1) / pageSize)
	va, err := s.place(b.PA, pages, s.carriesBitstream(input))
	if err != nil {
		return nil, err
	}
	s.handle++
	s.stat.offers++
	h := &held{id: s.handle, buf: b, va: va, pages: pages, tag: tag, input: input}
	s.bufs[h.id] = h
	return h, nil
}

// detach meldt dat het ijzer klaar is met deze buffer. De MAPPING blijft
// staan: zie place().
func (s *session) detach(h *held) {
	if p, ok := s.placed[h.buf.PA]; ok {
		p.live = false
	}
	delete(s.bufs, h.id)
}

// placedBuf is de vaste plek van één fysieke buffer in de adresruimte van de
// firmware.
type placedBuf struct {
	va        uint32
	pages     uint32
	protected bool
	live      bool // ligt hij nu bij het ijzer?
}

// place geeft een fysieke buffer zijn plek in de adresruimte van de firmware,
// en dezelfde buffer krijgt altijd DEZELFDE plek.
//
// Dat is geen optimalisatie maar een eis. De aanroeper draait rond op een
// pool van een handvol buffers; wie daar een roterende toewijzer op zet,
// vult per 4K-beeld 6075 page-table-entries opnieuw én is na tachtig beelden
// door zijn regio heen. Wat er dán gebeurt is erger dan traag: de toewijzer
// begint vooraan, deelt adressen uit die nog bij een levende buffer horen, en
// de eerstvolgende teruggave haalt de mapping onder het ijzer vandaan.
// Gemeten op ijzer 22-09: MMU ABORT na precies 83 beelden op 4K.
func (s *session) place(pa uintptr, pages uint32, protected bool) (uint32, error) {
	if p, ok := s.placed[pa]; ok {
		if p.pages == pages && p.protected == protected {
			p.live = true
			return p.va, nil
		}
		s.unplace(pa, p) // van maat of kant gewisseld: opnieuw beginnen
	}
	r := s.region(protected)
	va, ok := r.alloc(pages)
	if !ok {
		// Alleen wat NIET bij het ijzer ligt mag wijken.
		s.evict(protected)
		if va, ok = r.alloc(pages); !ok {
			return 0, fmt.Errorf("mve: %d pages do not fit in the firmware address space", pages)
		}
	}
	if err := s.mmu.mapRange(va, pa, pages, accessRW); err != nil {
		r.put(va, pages)
		return 0, err
	}
	s.placed[pa] = &placedBuf{va: va, pages: pages, protected: protected, live: true}
	return va, nil
}

// unplace haalt één plek weg en geeft de ruimte terug.
func (s *session) unplace(pa uintptr, p *placedBuf) {
	s.mmu.unmapRange(p.va, p.pages)
	s.region(p.protected).put(p.va, p.pages)
	delete(s.placed, pa)
}

// evict ruimt de plekken op die nergens meer bij het ijzer liggen.
func (s *session) evict(protected bool) {
	for pa, p := range s.placed {
		if !p.live && p.protected == protected {
			s.unplace(pa, p)
		}
	}
}

// region geeft de toewijzer van de gevraagde regio.
func (s *session) region(protected bool) *vaRegion {
	if protected {
		return &s.vaProt
	}
	return &s.vaFrame
}

// carriesBitstream meldt of de buffers aan deze kant van de sessie
// bitstream-bytes dragen in plaats van pixels. Bij decode is dat de invoer,
// bij encode de uitvoer — en dát bepaalt in welke van de twee firmware-regio's
// de buffer gemapt moet worden. De firmware controleert dat: pixels die in de
// protected-regio staan negeert hij zonder een woord te zeggen.
func (s *session) carriesBitstream(input bool) bool {
	return (s.cfg.Dir == codec.Decode) == input
}

// vaRegion deelt de virtuele ruimte van één firmware-regio uit: eerst uit wat
// er vrijkwam, daarna verse ruimte. Hergebruik gaat op EXACTE maat, want de
// aanroeper werkt met een pool van gelijke buffers — zo kan deze lijst niet
// fragmenteren en hoeft er niets samengevoegd te worden.
type vaRegion struct {
	beg, end uint32
	next     uint32
	free     []vaHole
}

type vaHole struct{ va, pages uint32 }

func (r *vaRegion) alloc(pages uint32) (uint32, bool) {
	for i, h := range r.free {
		if h.pages == pages {
			r.free = append(r.free[:i], r.free[i+1:]...)
			return h.va, true
		}
	}
	// In 64 bits: pages*pageSize loopt in 32 bits over, en dan past een
	// aanvraag van 4GB "makkelijk" in de paar pagina's die de rest overlaat.
	size := uint64(pages) * pageSize
	if size == 0 || uint64(r.next)+size > uint64(r.end) {
		return 0, false
	}
	va := r.next
	r.next += uint32(size)
	return va, true
}

func (r *vaRegion) put(va, pages uint32) {
	r.free = append(r.free, vaHole{va, pages})
}

// takeVA snijdt verse ruimte uit een regio. Alleen voor het geheugen dat de
// FIRMWARE zelf opvraagt (referentieframes, werkruimte): dat leeft zolang de
// sessie leeft en komt nooit in de plekken-boekhouding van de aanroeper.
func (s *session) takeVA(pages uint32, protected bool) (uint32, error) {
	va, ok := s.region(protected).alloc(pages)
	if !ok {
		return 0, fmt.Errorf("mve: %d pages do not fit in the firmware address space", pages)
	}
	return va, nil
}

// frameDesc bouwt een mve_buffer_frame. De invulling verschilt per richting,
// en dat is geen detail:
//
//   - bij INVOER (encode) vertelt visible_frame_* hoe groot het beeld is dat
//     erin staat;
//   - bij UITVOER (decode) blijven die velden NUL. De decoder bepaalt zelf wat
//     er zichtbaar wordt en schrijft het terug in deze velden. Wie ze vooraf
//     invult zegt iets wat hij niet weet.
//
// max_frame_* is niet de beeldmaat maar de CAPACITEIT van de buffer: hoeveel
// pixels breed en hoeveel regels er in het lumavlak passen.
func (s *session) frameDesc(h *held, f codec.Flag, input bool) []byte {
	d := make([]byte, bfSize)
	le := binary.LittleEndian
	le.PutUint64(d[bfHostHandle:], h.id)
	le.PutUint64(d[bfUserTag:], h.tag)
	var flags uint32
	if f&codec.EOS != 0 {
		flags |= frFlagEOS
	}
	if f&codec.KeyFrame != 0 {
		flags |= frFlagForceIDR
	}
	le.PutUint32(d[bfFlags:], flags)

	w, hgt := s.frameGeometry()
	stride := s.stride(w)
	if input {
		le.PutUint16(d[bfVisibleWidth:], uint16(w))
		le.PutUint16(d[bfVisibleHeight:], uint16(hgt))
	}
	le.PutUint16(d[bfFormat:], s.pixel)

	// Hoeveel regels passen er werkelijk in het lumavlak van deze buffer? Een
	// regel luma kost bij 4:2:0 anderhalve stride (het chroma erbij), bij Y8
	// precies één: daar is geen chroma-vlak.
	rows := hgt
	perRow := stride * 3 / 2
	if s.cfg.Pixel == codec.Y8 {
		perRow = stride
	}
	if perRow > 0 {
		if n := int(h.buf.Size) / perRow; n > 0 && n < rows {
			rows = n
		}
	}
	le.PutUint16(d[bfMaxWidth:], uint16(w))
	le.PutUint16(d[bfMaxHeight:], uint16(rows))

	// Y op het begin van de buffer, chroma erachter. Drievlaks (I420) splitst
	// het chroma-deel nog eens in tweeën.
	luma := uint32(stride * rows)
	le.PutUint32(d[bfPlaneTop:], h.va)
	le.PutUint32(d[bfStride:], uint32(stride))
	switch s.cfg.Pixel {
	case codec.I420:
		le.PutUint32(d[bfPlaneTop+4:], h.va+luma)
		le.PutUint32(d[bfPlaneTop+8:], h.va+luma+luma/4)
		le.PutUint32(d[bfStride+4:], uint32(stride/2))
		le.PutUint32(d[bfStride+8:], uint32(stride/2))
	case codec.Y8:
	default:
		le.PutUint32(d[bfPlaneTop+4:], h.va+luma)
		le.PutUint32(d[bfStride+4:], uint32(stride))
	}
	return d
}

// bitstreamDesc bouwt een mve_buffer_bitstream.
func (s *session) bitstreamDesc(h *held, filled uint64, f codec.Flag) []byte {
	d := make([]byte, bsSize)
	le := binary.LittleEndian
	le.PutUint64(d[bsHostHandle:], h.id)
	le.PutUint64(d[bsUserTag:], h.tag)
	var flags uint32
	if f&codec.EOS != 0 {
		flags |= bsFlagEOS
	}
	if f&codec.Headers != 0 {
		flags |= bsFlagCodecConfig
	}
	if filled > 0 {
		flags |= bsFlagEndOfFrame
	}
	le.PutUint32(d[bsFlags:], flags)
	le.PutUint32(d[bsAllocBytes:], uint32(h.pages*pageSize))
	le.PutUint32(d[bsOffset:], 0)
	le.PutUint32(d[bsFilledLen:], uint32(filled))
	le.PutUint32(d[bsBufAddr:], h.va)
	return d
}

// frameGeometry geeft de maat waarop we buffers beschrijven: wat de firmware
// vroeg zodra hij de stream kent, anders wat de aanroeper opgaf.
func (s *session) frameGeometry() (int, int) {
	if s.alloc.known {
		return int(s.alloc.width), int(s.alloc.height)
	}
	return s.cfg.Width, s.cfg.Height
}

// stride geeft de regelafstand in bytes voor het gekozen pixelformaat.
func (s *session) stride(width int) int {
	if s.cfg.Pixel == codec.P010 {
		return width * 2
	}
	return width
}

// hasSum meldt of deze firmware het checksum-woord verwacht.
func hasSum(version string) bool {
	for i := 0; i+4 <= len(version); i++ {
		if version[i:i+4] == "-sum" {
			return true
		}
	}
	return false
}

var _ codec.Session = (*session)(nil)

// ringState laat zien of de firmware al iets heeft teruggeschreven: de
// posities die HIJ bijhoudt. Blijven die op nul terwijl wij wel geschreven
// hebben, dan heeft hij onze berichten nooit gelezen.
func (s *session) ringState() string {
	rd := func(r ring) string {
		return fmt.Sprintf("h(w=%d r=%d sum=%#x/%#x) m(w=%d r=%d sum=%#x)",
			dev.Read16(r.host+qInWPos), dev.Read16(r.host+qOutRPos),
			r.sum, dev.Read32(r.host+qReserved+8),
			dev.Read16(r.mve+qOutWPos), dev.Read16(r.mve+qInRPos),
			dev.Read32(r.mve+qReserved+8))
	}
	return fmt.Sprintf(" msg[%s] in[%s] out[%s] rpc=%#x frames[decodeonly=%d corrupt=%d rejected=%d ref=%d unknown=%d streamcorrupt=%d flushes=%d flushback=%d eos=%d]",
		rd(s.msg), rd(s.bin), rd(s.bout), dev.Read32(s.rpc+rpcState),
		s.stat.decodeOnly, s.stat.corrupt, s.stat.rejected, s.stat.refFrame,
		s.stat.unknown, s.stat.streamCorrupt,
		s.stat.flushes, s.stat.flushBack, s.stat.eos) +
		fmt.Sprintf(" rpc[allocs=%d %dMB] bufs[offered=%d back=%d held=%d] va[frame=%#x prot=%#x]",
			s.stat.rpcAllocs, s.stat.rpcPages*pageSize>>20,
			s.stat.offers, s.stat.returns, len(s.bufs), s.vaFrame.next, s.vaProt.next)
}
