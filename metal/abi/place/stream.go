package place

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Stream plaatst een image ZONDER hem eerst ergens compleet neer te zetten: elke
// byte gaat meteen naar het adres waar hij hoort te draaien.
//
// Waarom dit bestaat. Tot nu toe kwam een image tweemaal in dezelfde partitie te
// staan — gestaged bovenin door de apploader, en daarna geplaatst op zijn
// vaddr — plus de Go-runtime van die loader ernaast. Een partitie moest dus
// image + image + loader dragen om één app te kunnen draaien; cloudflared kreeg
// 124MB voor een image van 30. Streamend is het één keer, en is een partitie
// weer wat hij hoort te zijn: de app plus zijn heap.
//
// Het plan komt nog steeds uit Build — ongewijzigd, met álle validatie. Dat kan
// omdat Finish hem een io.ReaderAt geeft die bestandsoffsets terugvertaalt naar
// waar de bytes inmiddels staan: binnen een PT_LOAD naar de partitie, daarbuiten
// (de staart met sectieheaders en symboltabel) naar een schrapruimte bovenin het
// app-RAM. Eén bron van waarheid, geen tweede parser die kan divergeren —
// dezelfde reden waarom Build zelf bestaat.
//
// De schrapruimte is exact passend, geen gok: de image-maat is vooraf bekend
// (Content-Length is al verplicht) en zodra de headers binnen zijn is ook het
// einde van het laatste PT_LOAD bekend — de staart is het verschil. Segmenten
// moeten onder de schrapruimte blijven; Finish nult hem, dus wat overblijft is
// alleen de app.
//
// Volgorde is de enige aanname, en die is getoetst op echte tamago-images: de
// PT_LOAD's komen in oplopende bestandsoffset, zonder terugsprong. Een image dat
// dat niet doet valt hieronder om met een duidelijke fout in plaats van stil
// verkeerd geplaatst te worden.
type Stream struct {
	sink    Sink
	imgSize uint64
	cfg     buildCfg
	head    []byte // ELF-header + program headers, tot ze compleet zijn
	segs    []Seg  // routeringstabel, zodra de headers binnen zijn
	segEnd  uint64 // bestandsoffset waar het laatste PT_LOAD eindigt
	scrOff  uint64 // schrapruimte voor de staart: appRAM − staartmaat
	pos     uint64 // hoeveel bytes van de image er door Write heen zijn
	fail    error  // eerste fout; verdere Writes zijn een no-op
	ready   bool   // headers geparsed, routering staat
}

// Sink is het geheugen van één partitie, geadresseerd in offsets vanaf de
// linkbasis. De kern implementeert hem op dev.Copy/CopyOut/Clear; de tests op
// een []byte. Bewust geen io.WriterAt: schrijven naar device-geheugen kan niet
// falen en een genegeerde error is erger dan geen error.
type Sink interface {
	Write(off uint64, p []byte)
	Read(p []byte, off uint64)
	Zero(off, n uint64)
}

// buildCfg zijn de parameters die ongewijzigd doorgaan naar Build.
type buildCfg struct {
	linkBase, appRAM, loOff uint64
	slot                    int
	abi                     uint64
}

// maxHead begrenst wat we mogen bufferen vóór de program headers compleet zijn.
// Een echte Go-ELF heeft ze binnen een kilobyte; dit is ruim en het is de enige
// heap die een streamende plaatsing kost.
const maxHead = 64 << 10

// NewStream opent een plaatsing. imgSize is de aangekondigde image-maat
// (Content-Length) en is verplicht: de schrapruimte en de compleetheids-toets
// rekenen ermee. De overige parameters gaan één op één naar Build.
func NewStream(sink Sink, imgSize int64, linkBase, appRAM, loOff uint64, slot int, abi uint64) *Stream {
	s := &Stream{
		sink:    sink,
		imgSize: uint64(imgSize),
		cfg:     buildCfg{linkBase: linkBase, appRAM: appRAM, loOff: loOff, slot: slot, abi: abi},
	}
	if imgSize <= 0 {
		s.failf("image size unknown (Content-Length required)")
	}
	return s
}

// Write voert het volgende stuk van de image in. Implementeert io.Writer, dus
// io.Copy vanaf een HTTP-body werkt rechtstreeks — de bytes gaan er aan de
// andere kant meteen op hun eindbestemming uit.
func (s *Stream) Write(p []byte) (int, error) {
	if s.fail != nil {
		return 0, s.fail
	}
	if s.pos+uint64(len(p)) > s.imgSize {
		return 0, s.failf("more bytes than the announced %d", s.imgSize)
	}
	n := len(p)
	if !s.ready {
		s.head = append(s.head, p...)
		done, err := s.parseHead()
		if err != nil {
			return 0, err
		}
		if !done {
			// Pas hier begrenzen, niet vóór de parse: één groot eerste blok
			// draagt de headers allang — dat mag geen afkeuring worden.
			if len(s.head) > maxHead {
				return 0, s.failf("program headers not complete within %d bytes", maxHead)
			}
			return n, nil // nog te weinig; alles zit in head
		}
		// Headers staan: de gebufferde kop alsnog routeren, daarna streamt de
		// rest er rechtstreeks doorheen.
		buf := s.head
		s.head = nil
		s.ready = true
		s.route(buf)
		return n, s.fail
	}
	s.route(p)
	return n, s.fail
}

// parseHead kijkt of de program headers al binnen zijn, bouwt de
// routeringstabel en rekent de schrapruimte uit. (false, nil) = nog te weinig.
func (s *Stream) parseHead() (bool, error) {
	const ehsize = 64 // Elf64_Ehdr
	if len(s.head) < ehsize {
		return false, nil
	}
	b := s.head
	if b[0] != 0x7F || b[1] != 'E' || b[2] != 'L' || b[3] != 'F' {
		return false, s.failf("not an ELF image (magic %v)", b[:4])
	}
	if b[4] != 2 || b[5] != 1 {
		return false, s.failf("only little-endian 64-bit ELF is supported")
	}
	phoff := binary.LittleEndian.Uint64(b[0x20:])
	phentsize := uint64(binary.LittleEndian.Uint16(b[0x36:]))
	phnum := uint64(binary.LittleEndian.Uint16(b[0x38:]))
	if phentsize < 56 || phnum == 0 {
		return false, s.failf("bogus program header table (%d × %d bytes)", phnum, phentsize)
	}
	end := phoff + phnum*phentsize
	if end > maxHead {
		return false, s.failf("program headers at %#x+%#x beyond the %d-byte head", phoff, phnum*phentsize, maxHead)
	}
	if uint64(len(b)) < end {
		return false, nil
	}
	// Routering: alleen PT_LOAD, en ze moeten oplopen in bestandsoffset — anders
	// zou een latere byte een eerder geplaatste moeten overschrijven en is
	// streamen niet veilig.
	var last uint64
	for i := uint64(0); i < phnum; i++ {
		ph := b[phoff+i*phentsize:]
		if binary.LittleEndian.Uint32(ph[0:]) != 1 { // PT_LOAD
			continue
		}
		seg := Seg{
			Off:    binary.LittleEndian.Uint64(ph[0x08:]),
			Dst:    binary.LittleEndian.Uint64(ph[0x18:]), // p_paddr, zoals Build
			Filesz: binary.LittleEndian.Uint64(ph[0x20:]),
			Memsz:  binary.LittleEndian.Uint64(ph[0x28:]),
		}
		if seg.Off < last {
			return false, s.failf("PT_LOAD at file offset %#x follows %#x — image is not streamable", seg.Off, last)
		}
		// Grofmazige grenscheck vóór de eerste byte landt; de gezaghebbende
		// validatie doet Build in Finish.
		if seg.Filesz > seg.Memsz || seg.Memsz > s.cfg.appRAM ||
			seg.Dst < s.cfg.linkBase+s.cfg.loOff ||
			seg.Dst > s.cfg.linkBase+s.cfg.appRAM-seg.Memsz {
			return false, s.failf("segment %#x+%#x outside the partition window", seg.Dst, seg.Memsz)
		}
		last = seg.Off + seg.Filesz
		s.segs = append(s.segs, seg)
	}
	if len(s.segs) == 0 {
		return false, s.failf("no PT_LOAD segments")
	}
	s.segEnd = last
	if s.imgSize < s.segEnd {
		return false, s.failf("announced size %d is smaller than the segments (%d bytes of file)", s.imgSize, s.segEnd)
	}
	// De staart (sectieheaders, symboltabel) krijgt een exact passende
	// schrapruimte bovenin het app-RAM; segmenten — inclusief hun BSS — moeten
	// eronder blijven, anders past app + staart simpelweg niet samen in deze
	// partitie en is dat een nette startfout.
	tail := s.imgSize - s.segEnd
	if tail > s.cfg.appRAM {
		return false, s.failf("image tail (%d bytes) larger than the partition", tail)
	}
	s.scrOff = s.cfg.appRAM - tail
	for _, sg := range s.segs {
		if sg.Dst+sg.Memsz > s.cfg.linkBase+s.scrOff {
			return false, s.failf("segments (top %#x) and the %d-byte image tail do not fit the partition together",
				sg.Dst+sg.Memsz, tail)
		}
	}
	return true, nil
}

// route stuurt elke byte naar zijn plek: binnen een PT_LOAD naar de partitie,
// ná het laatste segment naar de schrapruimte, en uitlijngaten ertussen nergens
// heen (het bestand bevat daar nullen).
func (s *Stream) route(p []byte) {
	for len(p) > 0 && s.fail == nil {
		off := s.pos
		seg, ok := s.segAt(off)
		switch {
		case ok:
			n := seg.Off + seg.Filesz - off
			if n > uint64(len(p)) {
				n = uint64(len(p))
			}
			s.sink.Write(seg.Dst-s.cfg.linkBase+(off-seg.Off), p[:n])
			p, s.pos = p[n:], s.pos+n
		default:
			n := s.gapLen(off, uint64(len(p)))
			if off >= s.segEnd {
				s.sink.Write(s.scrOff+(off-s.segEnd), p[:n])
			}
			p, s.pos = p[n:], s.pos+n
		}
	}
}

// segAt geeft het PT_LOAD dat bestandsoffset off draagt.
func (s *Stream) segAt(off uint64) (Seg, bool) {
	for _, sg := range s.segs {
		if off >= sg.Off && off < sg.Off+sg.Filesz {
			return sg, true
		}
	}
	return Seg{}, false
}

// gapLen geeft hoeveel bytes vanaf off buiten elk segment vallen (maximaal max).
func (s *Stream) gapLen(off, max uint64) uint64 {
	next := off + max
	for _, sg := range s.segs {
		if sg.Off > off && sg.Off < next {
			next = sg.Off
		}
	}
	return next - off
}

// Finish sluit de plaatsing af: de stroom moet compleet zijn, Build draait over
// de al geplaatste bytes (zie de pakketkop), de patches gaan erin, de BSS wordt
// genuld en de schrapruimte gewist. Geeft het plan terug — Entry is wat de
// aanroeper op de core zet.
func (s *Stream) Finish() (*Plan, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	if !s.ready {
		return nil, fmt.Errorf("stream ended after %d bytes, before the program headers were complete", s.pos)
	}
	if s.pos != s.imgSize {
		return nil, fmt.Errorf("image incomplete: %d of %d bytes", s.pos, s.imgSize)
	}
	plan, err := Build(scatter{s}, int64(s.imgSize), s.cfg.linkBase, s.cfg.appRAM,
		s.cfg.loOff, s.scrOff, s.cfg.slot, s.cfg.abi)
	if err != nil {
		return nil, err
	}
	// Build zag dezelfde headers, dus dit hoort te kloppen. Toch getoetst: als
	// het ooit uiteenloopt, staan de bytes op een andere plek dan het plan zegt
	// en dat is precies de klasse fouten die stil blijft.
	if len(plan.Segs) != len(s.segs) {
		return nil, fmt.Errorf("placement plan (%d segments) disagrees with the stream routing (%d)",
			len(plan.Segs), len(s.segs))
	}
	for i, sg := range plan.Segs {
		if sg != s.segs[i] {
			return nil, fmt.Errorf("placement plan segment %d %+v disagrees with the stream routing %+v",
				i, sg, s.segs[i])
		}
	}
	for _, sg := range plan.Segs {
		s.sink.Zero(sg.Dst-s.cfg.linkBase+sg.Filesz, sg.Memsz-sg.Filesz)
	}
	for _, p := range plan.Patches {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], p.Val)
		s.sink.Write(p.Addr-s.cfg.linkBase, b[:])
	}
	s.sink.Zero(s.scrOff, s.imgSize-s.segEnd)
	return plan, nil
}

func (s *Stream) failf(f string, a ...any) error {
	if s.fail == nil {
		s.fail = fmt.Errorf("stream placement: "+f, a...)
	}
	return s.fail
}

// scatter is de io.ReaderAt waarmee Build het al geplaatste image terugleest: een
// bestandsoffset wordt het adres waar die byte inmiddels staat.
type scatter struct{ s *Stream }

func (c scatter) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	s := c.s
	got := 0
	for got < len(p) {
		at := uint64(off) + uint64(got)
		if at >= s.pos {
			return got, io.EOF
		}
		want := uint64(len(p) - got)
		if seg, ok := s.segAt(at); ok {
			n := seg.Off + seg.Filesz - at
			if n > want {
				n = want
			}
			s.sink.Read(p[got:got+int(n)], seg.Dst-s.cfg.linkBase+(at-seg.Off))
			got += int(n)
			continue
		}
		n := s.gapLen(at, want)
		if n > s.pos-at {
			n = s.pos - at
		}
		if at >= s.segEnd {
			s.sink.Read(p[got:got+int(n)], s.scrOff+(at-s.segEnd))
		} else {
			for i := range p[got : got+int(n)] {
				p[got+i] = 0 // uitlijngat: nul, precies wat er in het bestand staat
			}
		}
		got += int(n)
	}
	return got, nil
}
