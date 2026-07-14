// Package ring implementeert de SPSC-ringbuffer (single producer, single
// consumer) van de hop-ABI over device-gemapt shared memory. Eén schrijver
// (de app-core) en één lezer (de HOP-kern) per richting — lock-vrij met
// monotone indexen, precies "software in de vorm van de machine".
//
// Geheugenindeling (alle velden 64-bit, gealigneerd):
//
//	+0x00 head   producer-index (bytes, monotoon oplopend)
//	+0x08 tail   consumer-index
//	+0x10 size   datacapaciteit in bytes (door HOP gezet bij slot-start)
//	+0x40 data   [size]byte, circulair
//
// Records: 8-byte header {len uint32, typ uint32} + payload, opgevuld tot een
// 8-voud. Een record wrapt nooit: past hij niet meer aaneengesloten, dan vult
// een PAD-record de staart en begint het record vooraan.
package ring

import (
	"hop-os/metal/dev"
)

const (
	hdrHead = 0x00
	hdrTail = 0x08
	hdrSize = 0x10
	dataOff = 0x40

	recHdr = 8

	// TypePad markeert opvulling tot het einde van de databuffer.
	TypePad = 0

	// hop-ABI recordtypes.
	TypeLog     = 1 // app → HOP (outbox): logregel
	TypeRPCReq  = 3 // app → HOP (outbox): hop-ABI-request (zie metal/abi/hopabi)
	TypeRPCResp = 4 // HOP → app (inbox): hop-ABI-response
	TypeFrame   = 5 // frame-ringen: één rauw Ethernet-frame (metal/net/hopswitch)
)

func align8(n uint64) uint64 { return (n + 7) &^ 7 }

// Init maakt een lege ring met de gegeven datacapaciteit klaar op base
// (aanroepen door HOP vóór CPU_ON; capaciteit moet een 8-voud zijn).
func Init(base uintptr, size uint64) {
	dev.Clear(base, dataOff)
	dev.Write64(base+hdrSize, size)
	dev.MB()
}

// Ring is één kant van een SPSC-ring op fysiek adres base.
type Ring struct {
	base    uintptr
	size    uint64
	corrupt bool // consumer zag een onmogelijke header; ring is dood
}

// Open koppelt aan een door Init klaargezette ring.
func Open(base uintptr) *Ring {
	return &Ring{base: base, size: dev.Read64(base + hdrSize)}
}

func (r *Ring) head() uint64     { return dev.Read64(r.base + hdrHead) }
func (r *Ring) tail() uint64     { return dev.Read64(r.base + hdrTail) }
func (r *Ring) setHead(v uint64) { dev.Write64(r.base+hdrHead, v) }
func (r *Ring) setTail(v uint64) { dev.Write64(r.base+hdrTail, v) }

func (r *Ring) writeRec(off uint64, typ uint32, p []byte) {
	addr := r.base + dataOff + uintptr(off%r.size)
	dev.Write64(addr, uint64(len(p))|uint64(typ)<<32)
	dev.Copy(addr+recHdr, p)
}

// Write plaatst een record; false als de ring vol is (aanroeper beslist:
// droppen of opnieuw proberen). Alleen door de producer aan te roepen.
func (r *Ring) Write(typ uint32, p []byte) bool {
	need := recHdr + align8(uint64(len(p)))
	if need > r.size/2 {
		return false // onredelijk groot record
	}
	head, tail := r.head(), r.tail()
	if head-tail > r.size {
		return false // onmogelijke indexen (malafide consument): niets schrijven
	}

	// Past het record nog aaneengesloten tot het einde van de buffer?
	if contig := r.size - head%r.size; need > contig {
		if r.size-(head-tail) < contig+need {
			return false
		}
		// PAD-record over de staart, dan vooraan verder.
		dev.Write64(r.base+dataOff+uintptr(head%r.size),
			uint64(contig-recHdr)|uint64(TypePad)<<32)
		head += contig
	}
	if r.size-(head-tail) < need {
		return false
	}

	r.writeRec(head, typ, p)
	dev.MB() // payload publiceren vóór de index
	r.setHead(head + need)
	return true
}

// ReadInto haalt het volgende record op en kopieert de payload in buf (door
// de consument hergebruikt — geen allocatie per record). n is de
// payloadlengte; ok=false als de ring leeg is. Alleen door de consumer aan
// te roepen; PAD-records worden intern overgeslagen.
//
// De ringinhoud komt van de producer en is onvertrouwd: een header die niet
// binnen de gepubliceerde bytes, de bufferrand óf buf past — of een head die
// meer dan size vóórloopt op tail — markeert de ring als corrupt en ReadInto
// levert definitief niets meer (zie Corrupt). Een producer mag de consument
// nooit tot een reuzenkopie of een eindeloze PAD-skip kunnen verleiden. buf
// moet minstens één maximaal record kunnen bevatten.
func (r *Ring) ReadInto(buf []byte) (typ uint32, n int, ok bool) {
	if r.corrupt {
		return 0, 0, false
	}
	for {
		head, tail := r.head(), r.tail()
		if head == tail {
			return 0, 0, false
		}
		// Meer gepubliceerd dan de buffer groot is kan alleen met een verzonnen
		// head — en een reusachtige head boven louter PAD-records zou de
		// skip-lus hieronder miljarden ronden gunnen (livelock op de HOP-core).
		if head-tail > r.size {
			r.corrupt = true
			return 0, 0, false
		}
		dev.MB() // index gezien → payload zichtbaar

		addr := r.base + dataOff + uintptr(tail%r.size)
		hdr := dev.Read64(addr)
		length, rtyp := uint32(hdr), uint32(hdr>>32)
		need := recHdr + align8(uint64(length))

		if need > head-tail || need > r.size-tail%r.size || uint64(length) > uint64(len(buf)) {
			r.corrupt = true
			return 0, 0, false
		}

		if rtyp == TypePad {
			dev.MB() // header gelezen vóór de ruimte vrijgeven
			r.setTail(tail + need)
			continue
		}
		dev.CopyOut(buf[:length], addr+recHdr)
		dev.MB() // payload gekopieerd vóór de ruimte vrijgeven
		r.setTail(tail + need)
		return rtyp, int(length), true
	}
}

// Corrupt meldt of de consumer de ring als corrupt heeft gemarkeerd; de enige
// uitweg is een verse Init door HOP (slot-herstart).
func (r *Ring) Corrupt() bool { return r.corrupt }

// Fits meldt of een record met payload-lengte n ooit in deze ring past. Write
// weigert records groter dan de helft van de buffer blijvend (geen "vol maar
// straks weer ruimte"), dus een aanroeper die eeuwig herprobeert tot Write
// lukt, moet dit eerst checken — anders spint hij oneindig.
func (r *Ring) Fits(n int) bool {
	return recHdr+align8(uint64(n)) <= r.size/2
}
