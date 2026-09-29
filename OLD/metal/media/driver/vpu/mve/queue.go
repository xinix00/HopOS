package mve

import (
	"encoding/binary"
	"errors"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// De host en de firmware praten over vier ringparen in gedeeld geheugen: één
// voor berichten (start, stop, opties, foutmeldingen) en drie voor buffers
// (invoer erheen, invoer terug, uitvoer heen en terug). Elk paar is twee
// pagina's: eentje die alleen de host beschrijft en eentje die alleen de
// firmware beschrijft. Zo is er geen enkel woord met twee schrijvers en dus
// ook geen lock tussen ons en het ijzer — dezelfde vorm als onze eigen
// net-ringen.
//
//	host-pagina: out_rpos u16 | in_wpos u16 | reserved[3] u32 | in_data[1020]
//	mve-pagina:  out_wpos u16 | in_rpos u16 | reserved[3] u32 | out_data[1020]
//
// Posities tellen in WOORDEN, niet in bytes, en lopen rond op 1020. Een
// bericht mag over die grens heen breken: elk woord wordt los geschreven.
const (
	qWords    = 1020
	qDataOff  = 16
	qOutRPos  = 0 // host schrijft
	qInWPos   = 2 // host schrijft
	qOutWPos  = 0 // firmware schrijft
	qInRPos   = 2 // firmware schrijft
	qReserved = 4 // 3 woorden; de firmware zet er zijn lopende checksum in
)

var (
	errQueueFull  = errors.New("mve: firmware queue full")
	errMsgTooBig  = errors.New("mve: message larger than the queue")
	errMsgUnknown = errors.New("mve: message code out of range")
	errQueuePos   = errors.New("mve: queue position out of range")
)

// ring is één ringpaar. host en mve zijn de fysieke adressen van de twee
// pagina's; sum is de lopende checksum van alles wat wij die kant op stuurden.
//
// Die checksum is geen luxe: de firmware van de O6N draagt "-sum" in zijn
// versiestring (gemeten: alle zestien blobs zijn "r0p0-sum0005") en verwacht
// dan achter élke berichtkop een woord met de lopende som. Een driver die dat
// woord weglaat schuift de hele stroom één woord op — de firmware leest dan
// zijn eigen berichtlengte als data en de sessie is stuk voor hij begint.
type ring struct {
	host uintptr
	mve  uintptr
	sum  uint32
	csum bool // deze firmware wil het checksum-woord
}

// send zet één bericht in de ring naar de firmware.
func (r *ring) send(code uint16, data []byte) error {
	words := (len(data) + 3) / 4
	need := 1 + words // kop
	if r.csum {
		need++
	}
	if need > qWords-1 {
		return errMsgTooBig
	}
	wpos := uint32(dev.Read16(r.host + qInWPos))
	rpos := uint32(dev.Read16(r.mve + qInRPos))
	if wpos >= qWords || rpos >= qWords {
		return errQueuePos
	}
	free := int(rpos) - int(wpos)
	if free <= 0 {
		free += qWords
	}
	// Eén woord blijft altijd vrij. Zonder die marge wordt een precies volle
	// ring niet van een lege te onderscheiden (beide: wpos == rpos), en dan
	// leest de firmware 1020 woorden oude berichten opnieuw. De Linux-driver
	// rekent hier zonder marge; wij betalen liever één woord.
	if free-1-need < 0 {
		return errQueueFull
	}

	hdr := uint32(code) | uint32(len(data))<<16
	if r.csum {
		r.sum += hdr + sumWords(data)
	}
	wpos = r.put(wpos, hdr)
	if r.csum {
		wpos = r.put(wpos, r.sum)
	}
	for i := 0; i < words; i++ {
		wpos = r.put(wpos, wordAt(data, i*4))
	}

	// Alle data staat in het geheugen vóór de firmware de nieuwe schrijfpositie
	// ziet; anders leest hij een kop die naar bytes wijst die er nog niet zijn.
	dev.MB()
	dev.Write16(r.host+qInWPos, uint16(wpos))
	if r.csum {
		// De firmware vergelijkt zijn eigen som met de laatste drie die wij
		// achterlieten (hij mag één bericht achterlopen).
		dev.Write32(r.host+qReserved, dev.Read32(r.host+qReserved+4))
		dev.Write32(r.host+qReserved+4, dev.Read32(r.host+qReserved+8))
		dev.Write32(r.host+qReserved+8, r.sum)
	}
	dev.MB()
	return nil
}

// recv haalt één bericht op. ok=false betekent: niets te lezen. De data komt
// in dst; een bericht dat daar niet in past is een protocolfout en geen
// gedeeltelijke lees, want een halve buffer-descriptor is erger dan geen.
// Hetzelfde voor een positie buiten de ring: beide pagina's staan beschrijfbaar
// in de adresruimte van de firmware, en een woord achter de 1020 ligt al op
// de volgende fysieke pagina.
func (r *ring) recv(dst []byte) (code uint16, n int, ok bool, err error) {
	rpos := uint32(dev.Read16(r.host + qOutRPos))
	wpos := uint32(dev.Read16(r.mve + qOutWPos))
	if rpos >= qWords || wpos >= qWords {
		return 0, 0, false, errQueuePos
	}
	if rpos == wpos {
		return 0, 0, false, nil
	}
	avail := int(wpos) - int(rpos)
	if avail < 0 {
		avail += qWords
	}

	hdr := r.get(rpos)
	code = uint16(hdr)
	size := int(hdr >> 16)
	words := (size + 3) / 4
	if code < respSwitchedIn || code > bufGeneral {
		return 0, 0, false, errMsgUnknown
	}
	if avail < 1+words {
		return 0, 0, false, nil // de firmware is nog aan het schrijven
	}
	if size > len(dst) {
		return code, 0, false, errMsgTooBig
	}

	pos := (rpos + 1) % qWords
	var w [4]byte
	for i := 0; i < words; i++ {
		binary.LittleEndian.PutUint32(w[:], r.get(pos))
		pos = (pos + 1) % qWords
		copy(dst[i*4:size], w[:])
	}
	dev.MB()
	dev.Write16(r.host+qOutRPos, uint16(pos))
	return code, size, true, nil
}

// put schrijft één woord op de ringpositie en geeft de volgende positie.
func (r *ring) put(pos uint32, v uint32) uint32 {
	dev.Write32(r.host+qDataOff+uintptr(pos*4), v)
	return (pos + 1) % qWords
}

// get leest één woord uit de ring van de firmware.
func (r *ring) get(pos uint32) uint32 {
	return dev.Read32(r.mve + qDataOff + uintptr(pos*4))
}

// sumWords telt de woorden van een bericht op zoals de firmware het doet: de
// staart wordt met nullen aangevuld tot een heel woord.
func sumWords(data []byte) uint32 {
	var sum uint32
	for i := 0; i < len(data); i += 4 {
		sum += wordAt(data, i)
	}
	return sum
}

// wordAt leest een 32-bit woord uit data en vult met nullen aan het eind.
func wordAt(data []byte, off int) uint32 {
	var b [4]byte
	copy(b[:], data[off:])
	return binary.LittleEndian.Uint32(b[:])
}
