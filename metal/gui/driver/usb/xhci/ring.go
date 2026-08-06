//go:build gui

package xhci

import "github.com/xinix00/HopOS/metal/dev"

// Dit bestand is de datastructuur waar xHCI helemaal op draait: de ring van
// 16-byte TRB's (Transfer Request Blocks) met één eigendomsbit. Software en
// controller lopen allebei rond in dezelfde ring; de CYCLE-bit zegt van wie een
// plek is. Software schrijft de bit die de controller verwacht → de plek is van
// de controller. Bij elke omloop klapt de verwachte waarde om, en dát is hoe
// beide kanten zien waar de ander is zonder een enkele gedeelde teller.
//
// De laatste plek van elk segment is een LINK-TRB terug naar het begin, met de
// TOGGLE-CYCLE-bit. Daarom is de bruikbare capaciteit n-1 en niet n.

// TRB-types (xHCI tabel 6-91). Alleen wat deze driver produceert of consumeert.
const (
	trbNormal = 1
	trbSetup  = 2
	trbData   = 3
	trbStatus = 4
	trbLink   = 6

	trbEnableSlot  = 9
	trbDisableSlot = 10
	trbAddressDev  = 11
	trbConfigEP    = 12
	trbEvalCtx     = 13
	trbResetEP     = 14
	trbSetTRDeq    = 16

	trbTransferEvt = 32
	trbCmdCompEvt  = 33
	trbPortStatEvt = 34
)

// Vlaggen in het derde dword van een TRB (xHCI 6.4.1).
const (
	trbCycle = 1 << 0
	trbTC    = 1 << 1 // alleen op een Link-TRB: toggle cycle
	trbISP   = 1 << 2 // interrupt on short packet
	trbChain = 1 << 4
	trbIOC   = 1 << 5 // interrupt on completion
	trbIDT   = 1 << 6 // immediate data (het TRB zélf is de payload)

	trbTypeShift = 10
)

// Completion codes (xHCI tabel 6-90). Alleen de codes waar deze driver een
// beslissing op neemt; de rest gaat als getal de logregel in.
const (
	ccSuccess     = 1
	ccStall       = 6
	ccShortPacket = 13
)

func compName(c uint32) string {
	switch c {
	case ccSuccess:
		return "success"
	case 2:
		return "data buffer error"
	case 3:
		return "babble"
	case 4:
		return "USB transaction error"
	case 5:
		return "TRB error"
	case ccStall:
		return "stall"
	case 7:
		return "resource error"
	case 8:
		return "bandwidth error"
	case 9:
		return "no slots available"
	case 11:
		return "no ping response"
	case ccShortPacket:
		return "short packet"
	case 19:
		return "missed service error"
	case 21:
		return "parameter error"
	}
	return "completion code"
}

// ring is een producer-ring: wij schrijven, de controller leest. Command rings
// en transfer rings zijn allebei dit.
type ring struct {
	base  uintptr // fysiek adres van het segment
	bus   uint64  // wat de CONTROLLER als adres ziet (base + HC.BusOff)
	n     int     // TRB-plaatsen inclusief de link-TRB op de laatste plek
	enq   int     // waar wij het volgende TRB schrijven
	cycle uint32  // de bit die "van de controller" betekent
}

// newRing legt een leeg segment aan met zijn link-TRB. bytes moet een veelvoud
// van 16 zijn en het segment mag geen 64KB-grens kruisen (xHCI 4.11.5.1) —
// beide gelden vanzelf omdat de arena in pagina's uitdeelt.
func newRing(base uintptr, bus uint64, bytes int) *ring {
	r := &ring{base: base, bus: bus, n: bytes / 16, cycle: 1}
	dev.Clear(base, uint64(bytes))
	r.armLink()
	return r
}

// armLink (her)schrijft de link-TRB met de cycle-bit die de controller NU
// verwacht. Wordt bij elke omloop herhaald: de bit klapt om, dus de link moet
// mee.
func (r *ring) armLink() {
	l := r.base + uintptr(r.n-1)*16
	dev.Write32(l+0, uint32(r.bus))
	dev.Write32(l+4, uint32(r.bus>>32))
	dev.Write32(l+8, 0)
	dev.Write32(l+12, uint32(trbLink)<<trbTypeShift|trbTC|r.cycle)
}

// deqPtr geeft de huidige schrijfpositie als dequeue-pointer mét DCS-bit —
// precies wat er in een endpoint-context hoort. De HUIDIGE positie en niet de
// basis: een control-ring die al descriptors verstuurd heeft staat niet meer
// op nul, en een endpoint-context met de basis erin zou de controller terug
// laten lopen over TRB's die al af zijn.
func (r *ring) deqPtr() uint64 {
	p := r.bus + uint64(r.enq)*16
	if r.cycle != 0 {
		p |= 1
	}
	return p
}

// push schrijft één TRB en geeft het BUS-adres ervan terug — dat is waar een
// transfer- of command-completion-event straks naar wijst, en dus onze enige
// manier om een antwoord aan een vraag te koppelen.
//
// De cycle-bit gaat als laatste mee in het controlwoord: pas dáármee draagt het
// TRB over aan de controller, dus de payload moet er al staan. Dit geheugen is
// device-gemapt (nGnRnE), dus de stores landen in programmavolgorde — de
// barrière eronder is voor de doorbell die erop volgt.
func (r *ring) push(p0, p1, p2, ctrl uint32) uint64 {
	a := r.base + uintptr(r.enq)*16
	at := r.bus + uint64(r.enq)*16
	dev.Write32(a+0, p0)
	dev.Write32(a+4, p1)
	dev.Write32(a+8, p2)
	dev.Write32(a+12, ctrl&^uint32(trbCycle)|r.cycle)

	r.enq++
	if r.enq == r.n-1 {
		// De link-TRB krijgt de OUDE cycle (hij is nu van de controller), pas
		// daarna klapt onze verwachting om.
		r.armLink()
		r.enq = 0
		r.cycle ^= 1
	}
	dev.MB()
	return at
}

// Interrupter-registerset 0 (xHCI 5.5.2), relatief aan de runtime-basis.
const (
	rtIR0 = 0x20

	irIMAN   = 0x00
	irIMOD   = 0x04
	irERSTSZ = 0x08
	irERSTBA = 0x10 // 64-bit
	irERDP   = 0x18 // 64-bit; bit 3 = EHB (event handler busy, write-1-to-clear)

	erdpEHB = 1 << 3
)

// evring is de consumer-kant: de controller schrijft, wij lezen. Eén segment,
// geen link-TRB — een event-ring wikkelt op zijn ERST-grens en toggelt daar zelf
// de cycle.
type evring struct {
	base  uintptr
	bus   uint64
	n     int
	deq   int
	cycle uint32
	ir    uintptr // interrupter-registerset (voor ERDP)
}

// event is één gelezen event-TRB, ontdaan van bitgefrommel.
type event struct {
	kind uint32 // trbTransferEvt / trbCmdCompEvt / trbPortStatEvt
	ptr  uint64 // transfer/command: het bus-adres van het TRB dat dit veroorzaakte
	comp uint32 // completion code
	rem  uint32 // transfer: RESTERENDE bytes, niet de overgedragen
	slot int
	port int // alleen bij een port status change event
}

// poll leest één event als er een klaarstaat. De cycle-bit is het eigendomsbit:
// komt hij niet overeen met wat wij verwachten, dan is dit nog een oude plek en
// staat er niets nieuws.
func (e *evring) poll() (event, bool) {
	a := e.base + uintptr(e.deq)*16
	ctrl := dev.Read32(a + 12)
	if ctrl&trbCycle != e.cycle {
		return event{}, false
	}
	p0 := dev.Read32(a + 0)
	p1 := dev.Read32(a + 4)
	p2 := dev.Read32(a + 8)

	ev := event{
		kind: ctrl >> trbTypeShift & 0x3F,
		ptr:  uint64(p0)&^0xF | uint64(p1)<<32,
		comp: p2 >> 24,
		rem:  p2 & 0xFFFFFF,
		slot: int(ctrl >> 24 & 0xFF),
	}
	if ev.kind == trbPortStatEvt {
		// Port Status Change: het poortnummer zit in [31:24] van dword 0.
		ev.port = int(p0 >> 24 & 0xFF)
	}

	e.deq++
	if e.deq == e.n {
		e.deq = 0
		e.cycle ^= 1
	}
	return ev, true
}

// advance publiceert onze leespositie. EHB moet mee als 1 (write-1-to-clear),
// anders blijft de controller denken dat we nog bezig zijn en levert hij geen
// nieuwe interrupt af.
func (e *evring) advance() {
	p := e.bus + uint64(e.deq)*16
	dev.Write32(e.ir+irERDP+4, uint32(p>>32))
	dev.Write32(e.ir+irERDP, uint32(p)|erdpEHB)
	dev.MB()
}
