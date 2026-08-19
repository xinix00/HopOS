// Package capnp doet precies zoveel Cap'n Proto als de tunnel-registratie
// vraagt: één segment bouwen, één segment lezen, en de stream-omhulling
// eromheen. Geen schema-compiler, geen capability-tabel, geen arena's.
//
// Dat kan omdat de berichtvormen VAST zijn. cloudflared linkt de hele
// capnproto-runtime omdat het generieke code is; wij sturen drie berichten met
// een bekende layout, en dan is de draad-vorm van Cap'n Proto eenvoudig genoeg
// om rechtstreeks te schrijven:
//
//   - alles is little-endian, alles in woorden van acht bytes
//   - een struct is: data-woorden, dan pointer-woorden
//   - een struct-pointer is (offset<<2 | 0), met daarna dataWords en ptrWords
//   - een lijst-pointer is (offset<<2 | 1), met elementtype en aantal
//   - offsets zijn RELATIEF, geteld vanaf het woord NA de pointer zelf
//
// De layouts (dataWords/ptrWords per struct, en welk veld op welke plek) zijn
// niet gegokt: ze komen uit de gegenereerde accessors van de gepinde
// cloudflared (tunnelrpc/proto/tunnelrpc.capnp.go en capnproto2's rpc.capnp.go)
// en staan als constante bij elk bericht hieronder.
package capnp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Builder bouwt één segment op. Structs worden achter elkaar neergezet en
// pointers verwijzen vooruit; dat is genoeg voor berichten die wij vormen (een
// boom, geen graaf met terugverwijzingen).
type Builder struct {
	words []byte // altijd een veelvoud van acht
}

// Struct is een venster op een struct in het segment: waar zijn data begint,
// hoeveel data-woorden en pointer-woorden hij heeft.
type Struct struct {
	b         *Builder
	dataStart int // byte-offset van het eerste data-woord
	dataWords int
	ptrWords  int
}

// NewBuilder maakt een leeg segment.
func NewBuilder() *Builder { return &Builder{} }

// Root zet de wortel-struct neer. Elk bericht heeft er precies één, en de
// wortel-pointer staat in woord 0 van het segment.
func (b *Builder) Root(dataWords, ptrWords int) Struct {
	b.words = make([]byte, 8) // woord 0 = de wortel-pointer
	s := b.alloc(dataWords, ptrWords)
	b.setStructPtr(0, s)
	return s
}

// alloc reserveert ruimte voor een struct en geeft hem terug.
func (b *Builder) alloc(dataWords, ptrWords int) Struct {
	start := len(b.words)
	b.words = append(b.words, make([]byte, (dataWords+ptrWords)*8)...)
	return Struct{b: b, dataStart: start, dataWords: dataWords, ptrWords: ptrWords}
}

// setStructPtr schrijft op byte-offset at een pointer naar s.
func (b *Builder) setStructPtr(at int, s Struct) {
	// Relatieve offset in woorden, geteld vanaf het woord ná de pointer.
	off := (s.dataStart - (at + 8)) / 8
	var p uint64
	p |= uint64(uint32(int32(off))) << 2 & 0xffffffff
	p |= uint64(uint16(s.dataWords)) << 32
	p |= uint64(uint16(s.ptrWords)) << 48
	binary.LittleEndian.PutUint64(b.words[at:], p)
}

// NewStruct zet een nieuwe struct neer in pointer-slot i van s.
func (s Struct) NewStruct(i, dataWords, ptrWords int) Struct {
	child := s.b.alloc(dataWords, ptrWords)
	s.b.setStructPtr(s.ptrOffset(i), child)
	return child
}

func (s Struct) ptrOffset(i int) int {
	if i < 0 || i >= s.ptrWords {
		panic("capnp: pointer index outside the struct")
	}
	return s.dataStart + s.dataWords*8 + i*8
}

// SetUint8/SetUint16/SetUint32/SetInt64/SetBool schrijven in de data-sectie.
// De offsets zijn in bytes respectievelijk bits, precies zoals de gegenereerde
// accessors ze noemen (s.Struct.Uint8(2), s.Struct.Bit(0), ...).
func (s Struct) SetUint8(byteOff int, v uint8) {
	s.b.words[s.dataStart+byteOff] = v
}

func (s Struct) SetUint16(byteOff int, v uint16) {
	binary.LittleEndian.PutUint16(s.b.words[s.dataStart+byteOff:], v)
}

func (s Struct) SetUint32(byteOff int, v uint32) {
	binary.LittleEndian.PutUint32(s.b.words[s.dataStart+byteOff:], v)
}

func (s Struct) SetInt64(byteOff int, v int64) {
	binary.LittleEndian.PutUint64(s.b.words[s.dataStart+byteOff:], uint64(v))
}

// SetUint64 is er voor de interface-id's: die zijn 64-bits hashes en passen
// niet in een int64.
func (s Struct) SetUint64(byteOff int, v uint64) {
	binary.LittleEndian.PutUint64(s.b.words[s.dataStart+byteOff:], v)
}

func (s Struct) SetBool(bitOff int, v bool) {
	if !v {
		return // nul is de default; niets te doen
	}
	i := s.dataStart + bitOff/8
	s.b.words[i] |= 1 << uint(bitOff%8)
}

// SetText zet een Text-veld (UTF-8 mét afsluitende nul) in pointer-slot i.
func (s Struct) SetText(i int, v string) {
	s.setList(i, 2, append([]byte(v), 0))
}

// SetData zet een Data-veld (kale bytes) in pointer-slot i.
func (s Struct) SetData(i int, v []byte) {
	s.setList(i, 2, v)
}

// setList schrijft een lijst-pointer plus de bytes. elemType 2 = één byte per
// element, wat Text en Data allebei zijn.
func (s Struct) setList(i int, elemType uint8, raw []byte) {
	at := s.ptrOffset(i)
	start := len(s.b.words)
	// Op woorden afronden: de vulling hoort bij de lijst en blijft nul.
	padded := (len(raw) + 7) / 8 * 8
	s.b.words = append(s.b.words, make([]byte, padded)...)
	copy(s.b.words[start:], raw)

	off := (start - (at + 8)) / 8
	var p uint64 = 1 // lijst-pointer
	p |= uint64(uint32(int32(off))) << 2 & 0xfffffffc
	p |= uint64(elemType) << 32
	p |= uint64(len(raw)) << 35
	binary.LittleEndian.PutUint64(s.b.words[at:], p)
}

// SetTextList zet een List(Text) in pointer-slot i: een lijst van pointers,
// elk naar zijn eigen letterreeks. ClientInfo.features gebruikt deze vorm.
func (s Struct) SetTextList(i int, values []string) {
	at := s.ptrOffset(i)
	start := len(s.b.words)
	s.b.words = append(s.b.words, make([]byte, len(values)*8)...)

	off := (start - (at + 8)) / 8
	var p uint64 = 1
	p |= uint64(uint32(int32(off))) << 2 & 0xfffffffc
	p |= uint64(6) << 32 // elementtype 6 = pointer
	p |= uint64(len(values)) << 35
	binary.LittleEndian.PutUint64(s.b.words[at:], p)

	for n, v := range values {
		slot := start + n*8
		raw := append([]byte(v), 0)
		textStart := len(s.b.words)
		padded := (len(raw) + 7) / 8 * 8
		s.b.words = append(s.b.words, make([]byte, padded)...)
		copy(s.b.words[textStart:], raw)

		toff := (textStart - (slot + 8)) / 8
		var tp uint64 = 1
		tp |= uint64(uint32(int32(toff))) << 2 & 0xfffffffc
		tp |= uint64(2) << 32
		tp |= uint64(len(raw)) << 35
		binary.LittleEndian.PutUint64(s.b.words[slot:], tp)
	}
}

// SetEmptyList zet een lege lijst (nul elementen) in pointer-slot i. De
// transform-lijst van een promisedAnswer heeft die vorm.
func (s Struct) SetEmptyList(i int) {
	at := s.ptrOffset(i)
	var p uint64 = 1
	p |= uint64(6) << 32
	binary.LittleEndian.PutUint64(s.b.words[at:], p)
}

// Message geeft het segment als bytes.
func (b *Builder) Message() []byte { return b.words }

// WriteMessage schrijft één bericht in de stream-vorm die capnproto's
// "SafeTransport" gebruikt: aantal segmenten minus één (4 bytes), dan per
// segment de lengte in woorden (4 bytes), afgerond op acht bytes, dan de
// segmenten. Wij sturen altijd precies één segment.
func WriteMessage(w io.Writer, segment []byte) error {
	if len(segment)%8 != 0 {
		return errors.New("capnp: segment is not a whole number of words")
	}
	var head [8]byte
	binary.LittleEndian.PutUint32(head[0:], 0) // één segment (aantal - 1)
	binary.LittleEndian.PutUint32(head[4:], uint32(len(segment)/8))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(segment)
	return err
}

// maxMessageWords begrenst wat we van de peer aannemen. Een registratie-
// antwoord is tientallen bytes; dit plafond scheelt een node van 32MB een
// slechte dag als de andere kant onzin stuurt.
const maxMessageWords = 1 << 16 // 512KB

// ReadMessage leest één bericht en geeft het eerste segment terug. Berichten
// met meer segmenten worden geweigerd in plaats van half begrepen: de edge
// stuurt er één, en stil doorgaan op een aanname is hoe je een fout vindt die
// niet in de logs staat.
func ReadMessage(r io.Reader) ([]byte, error) {
	var count [4]byte
	if _, err := io.ReadFull(r, count[:]); err != nil {
		return nil, err
	}
	segments := int(binary.LittleEndian.Uint32(count[:])) + 1
	if segments != 1 {
		return nil, fmt.Errorf("capnp: message has %d segments, this reader handles one", segments)
	}
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	words := int(binary.LittleEndian.Uint32(size[:]))
	if words <= 0 || words > maxMessageWords {
		return nil, fmt.Errorf("capnp: message of %d words refused", words)
	}
	seg := make([]byte, words*8)
	if _, err := io.ReadFull(r, seg); err != nil {
		return nil, err
	}
	return seg, nil
}

// Reader leest een ontvangen segment. Elke opzoeking rekent zijn grenzen na:
// dit zijn bytes van het netwerk.
type Reader struct{ seg []byte }

// NewReader maakt een lezer op een segment.
func NewReader(seg []byte) *Reader { return &Reader{seg: seg} }

// RootStruct volgt de wortel-pointer in woord 0.
func (r *Reader) RootStruct() (View, error) { return r.structAt(0) }

// View is een gelezen struct.
type View struct {
	r         *Reader
	dataStart int
	dataWords int
	ptrWords  int
	null      bool
}

// structAt leest de struct-pointer op byte-offset at.
func (r *Reader) structAt(at int) (View, error) {
	if at+8 > len(r.seg) {
		return View{}, errors.New("capnp: pointer beyond the segment")
	}
	p := binary.LittleEndian.Uint64(r.seg[at:])
	if p == 0 {
		return View{null: true}, nil
	}
	if p&3 != 0 {
		return View{}, fmt.Errorf("capnp: expected a struct pointer, got type %d", p&3)
	}
	off := int(int32(uint32(p&0xffffffff)) >> 2)
	dataWords := int(uint16(p >> 32))
	ptrWords := int(uint16(p >> 48))
	start := at + 8 + off*8
	if start < 0 || start+(dataWords+ptrWords)*8 > len(r.seg) {
		return View{}, errors.New("capnp: struct beyond the segment")
	}
	return View{r: r, dataStart: start, dataWords: dataWords, ptrWords: ptrWords}, nil
}

// IsNull zegt of deze pointer leeg was. Een leeg veld is in Cap'n Proto geen
// fout maar de default, dus dit is een gewone uitkomst.
func (v View) IsNull() bool { return v.null }

// Uint16/Uint32/Int64/Bool lezen uit de data-sectie. Buiten de sectie lezen
// geeft nul: dat is Cap'n Proto's regel voor velden die de afzender (een oudere
// schema-versie) nog niet had.
func (v View) Uint16(byteOff int) uint16 {
	if v.null || byteOff+2 > v.dataWords*8 {
		return 0
	}
	return binary.LittleEndian.Uint16(v.r.seg[v.dataStart+byteOff:])
}

func (v View) Uint32(byteOff int) uint32 {
	if v.null || byteOff+4 > v.dataWords*8 {
		return 0
	}
	return binary.LittleEndian.Uint32(v.r.seg[v.dataStart+byteOff:])
}

func (v View) Int64(byteOff int) int64 {
	if v.null || byteOff+8 > v.dataWords*8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(v.r.seg[v.dataStart+byteOff:]))
}

func (v View) Bool(bitOff int) bool {
	if v.null || bitOff/8 >= v.dataWords*8 {
		return false
	}
	return v.r.seg[v.dataStart+bitOff/8]&(1<<uint(bitOff%8)) != 0
}

// StructPtr volgt pointer-slot i naar een struct.
func (v View) StructPtr(i int) (View, error) {
	if v.null || i >= v.ptrWords {
		return View{null: true}, nil
	}
	return v.r.structAt(v.dataStart + v.dataWords*8 + i*8)
}

// Bytes leest een Data- of Text-veld uit pointer-slot i. Bij Text valt de
// afsluitende nul eraf.
func (v View) Bytes(i int, text bool) ([]byte, error) {
	if v.null || i >= v.ptrWords {
		return nil, nil
	}
	at := v.dataStart + v.dataWords*8 + i*8
	if at+8 > len(v.r.seg) {
		return nil, errors.New("capnp: pointer beyond the segment")
	}
	p := binary.LittleEndian.Uint64(v.r.seg[at:])
	if p == 0 {
		return nil, nil
	}
	if p&3 != 1 {
		return nil, fmt.Errorf("capnp: expected a list pointer, got type %d", p&3)
	}
	off := int(int32(uint32(p&0xffffffff)) >> 2)
	elem := uint8(p >> 32 & 7)
	n := int(p >> 35 & 0x1fffffff)
	if elem != 2 {
		return nil, fmt.Errorf("capnp: expected a byte list, got element type %d", elem)
	}
	start := at + 8 + off*8
	if start < 0 || start+n > len(v.r.seg) || n > math.MaxInt32 {
		return nil, errors.New("capnp: list beyond the segment")
	}
	raw := v.r.seg[start : start+n]
	if text && n > 0 {
		raw = raw[:n-1] // de nul hoort bij de codering, niet bij de waarde
	}
	return raw, nil
}

// Text is Bytes met de Text-conventie.
func (v View) Text(i int) (string, error) {
	raw, err := v.Bytes(i, true)
	return string(raw), err
}
