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
	"fmt"

	"github.com/xinix00/HopOS/metal/dev"
)

const (
	// Head en tail liggen elk in hun EIGEN cacheline, en dat is een harde eis, geen
	// nette vormgeving: op een architectuur zonder coherente harten (RISC-V, zie
	// dev.Push/Pull) schrijft de producer bij zijn cache-clean de hele line terug —
	// inclusief zijn verouderde kopie van tail, en dus over de voortgang van de
	// consument. Andersom idem. Gemeten 30-07 op de LicheeRV: kleine frames (DNS,
	// TCP-handshake, HTTP-headers) kwamen door, maar zodra beide kanten tegelijk
	// hameren (een download van 5MB) stond de ring binnen één segment stil.
	//
	// 64 bytes uit elkaar kost niets — de ringkop was toch al een aparte pagina —
	// en op ARM verandert er niets: daar zijn deze regio's device-gemapt.
	hdrHead = 0x00
	hdrTail = 0x40
	hdrSize = 0x10
	dataOff = 0x80

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
	// De HELE kop publiceren, niet alleen het maatwoord: head en tail liggen sinds
	// ABI 3 elk in hun eigen cacheline (zie hieronder), dus een Push van 8 bytes
	// laat die twee ongepubliceerd. Op een board waar HOP en het app-hart niet
	// coherent zijn zou de andere kant daar dan lezen wat er toevallig in DRAM
	// stond in plaats van de nul die Clear net schreef.
	dev.Push(base, dataOff)
	dev.MB()
}

// Ring is één kant van een SPSC-ring op fysiek adres base.
type Ring struct {
	base    uintptr
	size    uint64
	corrupt bool   // consumer zag een onmogelijke header; ring is dood
	why     string // de meting van dát moment (CorruptWhy) — anders is een
	// corrupt-verklaring van buiten niet te onderscheiden van een lege ring,
	// en dat onderscheid was precies de jacht van 17-08 (boot 9: slot-TX
	// leest eeuwig leeg terwijl de app schrijft).
}

// markCorrupt zet de vlag met de reden; de eerste reden wint (de vervolgstaat
// van een corrupte ring is geen nieuwe informatie).
func (r *Ring) markCorrupt(why string) {
	if !r.corrupt {
		r.corrupt = true
		r.why = why
	}
}

// CorruptWhy geeft de reden van de corrupt-verklaring ("" = niet corrupt).
func (r *Ring) CorruptWhy() string { return r.why }

// Open koppelt aan een door Init klaargezette ring. Pull vóór het lezen van de
// capaciteit, net als bij de kop-accessors hieronder: Init schreef die van het
// andere hart af. In de praktijk dekte de partitie-brede veeg vóór de dispatch
// dit al, maar dat is een toevallige dekking en geen contract — hier staat het
// expliciet, en het kost één op per ring.
func Open(base uintptr) *Ring {
	dev.Pull(base+hdrSize, 8)
	return &Ring{base: base, size: dev.Read64(base + hdrSize)}
}

// De vier kop-accessors doen het cache-onderhoud van de ABI: Pull vóór een lees,
// Push ná een schrijf. Op ARM zijn dat no-ops (de ringen zijn device-gemapt en dus
// coherent); op een architectuur zonder device-mapping is het precies wat een
// SPSC-ring tussen twee niet-coherente harten nodig heeft. Hier en niet bij de
// aanroepers, want dít zijn de enige plekken waar de kop van een ring gelezen of
// geschreven wordt — één plek, geen discipline.
func (r *Ring) head() uint64 {
	dev.Pull(r.base+hdrHead, 8)
	return dev.Read64(r.base + hdrHead)
}

func (r *Ring) tail() uint64 {
	dev.Pull(r.base+hdrTail, 8)
	return dev.Read64(r.base + hdrTail)
}

func (r *Ring) setHead(v uint64) {
	dev.Write64(r.base+hdrHead, v)
	dev.Push(r.base+hdrHead, 8)
}

func (r *Ring) setTail(v uint64) {
	dev.Write64(r.base+hdrTail, v)
	dev.Push(r.base+hdrTail, 8)
}

// putHdr schrijft één recordheader en publiceert hem meteen. ÉLKE header loopt
// hierlangs — een echt record én een PAD — zodat de cache-stap niet meer van
// discipline per call-site afhangt. Precies dat ging mis: de PAD-header had geen
// Push, en dat is één plek vergeten in code die verder overal klopt. Zelfde
// argument als bij de kop-accessors: één plek, geen discipline.
func (r *Ring) putHdr(off, length uint64, typ uint32) {
	addr := r.base + dataOff + uintptr(off%r.size)
	dev.Write64(addr, length|uint64(typ)<<32)
	dev.Push(addr, recHdr)
}

func (r *Ring) writeRec(off uint64, typ uint32, p []byte) {
	r.putHdr(off, uint64(len(p)), typ)
	addr := r.base + dataOff + uintptr(off%r.size)
	dev.Copy(addr+recHdr, p)
	dev.Push(addr+recHdr, uintptr(align8(uint64(len(p)))))
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
		// Een verzonnen head (de indexen leven in geheugen dat de andere kant
		// kan beschrijven) laat contig-recHdr hieronder underflowen — dan zou de
		// PAD-lengte bijna 2^64 worden en de header zelf voorbij de datarand
		// staan.
		if contig < recHdr {
			return false
		}
		if r.size-(head-tail) < contig+need {
			return false
		}
		// PAD-record over de staart, dan vooraan verder. De Push hoort er net zo
		// hard bij als bij een echt record: zonder blijft deze header in de cache
		// van de producer staan en leest de consument stále bytes op die plek —
		// een verzonnen lengte, waarna zijn validatie de ring corrupt verklaart en
		// de lus voorgoed stopt.
		//
		// Dit was de "download-flakiness" (gemeten 31-07 op de LicheeRV): een PAD
		// wordt ALLEEN bij wraparound geschreven, dus alles onder één ringlengte
		// werkte altijd — DHCP, HTTP-headers, een pagina van 16KB — en een
		// download van 5MB stopte op 926360 van 5305899 bytes, 95% van de
		// 978944-byte RX-ring. Precies de eerste wrap. Dat het soms tóch goed ging
		// was de stale lengte die af en toe plausibel uitviel.
		r.putHdr(head, contig-recHdr, TypePad)
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
			r.markCorrupt(fmt.Sprintf("head-tail>size (head=%#x tail=%#x size=%#x)", head, tail, r.size))
			return 0, 0, false
		}
		// De 8-byte header moet zélf nog vóór de datarand liggen, en dát moet
		// vóór de Read64 vaststaan: de indexen leven in geheugen dat de app kan
		// beschrijven, dus een verzonnen tail (size-7 bijvoorbeeld) zou hieronder
		// 1-7 bytes voorbij de databuffer lezen. Vandaag valt dat nog in de
		// mapped slack van het slot-venster (RingDataCap < RingStride) — dus geluk,
		// geen contract; wordt die slack ooit nul, dan is het een fault op core 0.
		if tail%r.size > r.size-recHdr {
			r.markCorrupt(fmt.Sprintf("header past de datarand niet (tail=%#x size=%#x)", tail, r.size))
			return 0, 0, false
		}
		dev.MB() // index gezien → payload zichtbaar

		addr := r.base + dataOff + uintptr(tail%r.size)
		// Verversen vóór het lezen: de producer schreef dit record uit zíjn cache
		// weg (Push), maar op een niet-coherente architectuur kan er nog een oude
		// regel van ons in de weg staan. Eerst de header, dan (na de
		// lengtevalidatie hieronder) de payload — nooit meer dan gepubliceerd is.
		dev.Pull(addr, recHdr)
		hdr := dev.Read64(addr)
		length, rtyp := uint32(hdr), uint32(hdr>>32)
		need := recHdr + align8(uint64(length))

		if need > head-tail || need > r.size-tail%r.size || uint64(length) > uint64(len(buf)) {
			r.markCorrupt(fmt.Sprintf("onmogelijke header (hdr=%#x len=%d typ=%d head=%#x tail=%#x size=%#x buf=%d)",
				hdr, length, rtyp, head, tail, r.size, len(buf)))
			return 0, 0, false
		}

		if rtyp == TypePad {
			dev.MB() // header gelezen vóór de ruimte vrijgeven
			r.setTail(tail + need)
			continue
		}
		dev.Pull(addr+recHdr, uintptr(length))
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
