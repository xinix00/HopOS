//go:build gui

// Package usbin is de invoerdienst van HopOS: het bezit de USB-controllers,
// houdt bij wat er in- en uitgeplugd wordt, en levert toetsaanslagen en
// muisbewegingen af bij één ontvanger.
//
// WAAROM HOP DE CONTROLLER BEZIT EN NIET EEN APP. Het gui-ontwerp had hier een
// DeviceGrant staan (§P5/P6): de app krijgt het registerblok in zijn kooi en
// bedient zijn eigen apparaat. Voor GPIO of I²C is dat prima. Voor xHCI niet,
// en het verschil is DMA: een xHCI-controller is een bus-master die descriptors
// leest en schrijft op adressen die HIJ krijgt aangereikt. De stage-2 begrenst
// wat de CPU van een app mag zien, maar niet wat een apparaat namens die app
// doet — daar is een IOMMU voor nodig, en op dit silicium staat die aantoonbaar
// uit (we zetten de VOP-IOMMU zelf uit om de scanout aan de praat te krijgen).
// Een DeviceGrant op een DMA-capabel blok is dus effectief het hele geheugen.
//
// Daarom: HOP leest de rapporten en stuurt de gebeurtenissen door. DeviceGrant
// blijft bestaan voor apparaten die niet kunnen DMA'en.
//
// De weg naar de display loopt over het NETWERK, en het adres reist mee in de
// framebuffer-grant (zie deliver.go): dezelfde JSON-events die de browser-KVM
// al post, over één verbinding. Eén invoerweg, of de toets nu van een browser
// of van echt ijzer komt.
package usbin

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/hid"
	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/xhci"
)

// Sink krijgt elke gebeurtenis. Mag blokkeren noch panieken: hij draait op de
// goroutine die de bus bezit.
type Sink func(hid.Event)

// pollInterval is hoe vaak we de event-ring bekijken. Een boot-toetsenbord
// meldt zich elke 8-10ms; 4ms is dus ruim binnen de aanslagsnelheid en kost per
// beurt een handvol MMIO-reads.
const pollInterval = 4 * time.Millisecond

// scanInterval is hoe vaak we naar in- en uitpluggen kijken. Een halve seconde
// wachten op een toetsenbord dat je net insteekt is niet merkbaar, en het houdt
// de poortregisters uit de hete lus.
const scanInterval = 500 * time.Millisecond

// fineRounds is hoe vaak de eigenaar na het starten van een bulk-transfer op
// 100µs-afstand kijkt voor hij terugvalt op een milliseconde. Een toetsenbord
// kan een milliseconde missen, bulk-opslag niet: één SCSI-opdracht is drie
// transfers, en bij 64KB per opdracht kost een vaste milliseconde per wacht al
// meer dan de drive zelf. GEMETEN 22-09: een Blu-ray door de share kwam zo op
// 2,7 MB/s. Twintig rondes van 100µs dekken het normale geval; wat daarna nog
// wacht is een trage drive, en dáár is een milliseconde precies goed.
const fineRounds = 20

// maxPerPoll begrenst hoeveel rapporten we per beurt van één apparaat
// ophalen. Eén apparaat kan twee endpoints hebben (een combo-dongle levert
// toetsenbord én muis), dus één per beurt zou de muis halveren; ongebrensd zou
// een ratelend apparaat de andere poorten kunnen uithongeren.
const maxPerPoll = 4

// port is één bezette roothub-poort met zijn apparaat en decoder.
type port struct {
	dev  *xhci.Device
	bulk *Bulk // opslag: het handvat dat de rest van de node van dit apparaat heeft
	kb   hid.Keyboard
	ms   hid.Mouse
	buf  []byte
}

// Storage krijgt elk opslagapparaat dat de scanner vindt. Gezet door cmd/hopos
// vóór Start; nil = deze node doet niets met opslag op USB en het apparaat
// blijft simpelweg hangen. Eén haak en geen lijst: er is één eigenaar van de
// bus, en die deelt het apparaat uit aan wie het hebben wil. De haak draait op
// de goroutine van die eigenaar en mag dus zelf niets met de Bulk doen: elk
// verzoek zou wachten op de goroutine die hem aanroept.
var Storage func(*Bulk)

// Manager bedient nul of meer controllers. Alles hierin is van één goroutine
// (Run); Add hoort vóór Run, en wie daarna iets van de bus wil gaat via reqs.
type Manager struct {
	hcs   []*xhci.HC
	ports map[*xhci.HC]map[int]*port
	sink  Sink
	evs   []hid.Event // hergebruikte buffer: het pollpad mag niet allloceren

	reqs  chan *bulkReq           // verzoeken van Bulk-handvatten
	queue map[*xhci.HC][]*bulkReq // wachtend, per controller
	busy  map[*xhci.HC]*inflight  // de ene lopende transfer per controller
	fine  int                     // resterende fijnmazige rondes (fineRounds)
}

// New maakt een lege invoerdienst.
func New(sink Sink) *Manager {
	return &Manager{
		ports: map[*xhci.HC]map[int]*port{},
		sink:  sink,
		reqs:  make(chan *bulkReq),
		queue: map[*xhci.HC][]*bulkReq{},
		busy:  map[*xhci.HC]*inflight{},
	}
}

// Add neemt een controller in beheer: probe, reset, structuren opzetten,
// poortvoeding aan. Een controller die niet antwoordt is geen fatale fout —
// een board mag meer controllers aanbieden dan er fysiek bedraad zijn, en de
// melding is dan de meting.
func (m *Manager) Add(hc *xhci.HC, dmaBase, dmaSize uintptr) error {
	if err := hc.Probe(); err != nil {
		return err
	}
	ver, slots, ports, ctx64 := hc.Info()
	fmt.Printf("usb: %s xHCI %x.%x — %d slots, %d ports, %d-byte contexts\n",
		hc.Name, ver>>8, ver&0xFF, slots, ports, map[bool]int{false: 32, true: 64}[ctx64])

	if err := hc.Reset(); err != nil {
		return err
	}
	if err := hc.Start(dmaBase, dmaSize); err != nil {
		return err
	}
	hc.PowerOn()

	// De rauwe poortstand, één regel. Dit is de meting die op ijzer telt: een
	// controller die netjes opkomt maar op géén poort CCS meldt, is een
	// controller die niet aan de fysieke connector hangt — en dat is iets
	// heel anders dan een driver die stukgaat. Zonder deze regel lijken die
	// twee identiek, namelijk stil.
	var st string
	for _, p := range hc.Ports() {
		st += fmt.Sprintf(" %d:%08x", p.Num, p.Raw)
		if p.Connected {
			st += "(" + p.Speed.String() + ")"
		}
	}
	fmt.Printf("usb: %s PORTSC%s\n", hc.Name, st)

	m.hcs = append(m.hcs, hc)
	m.ports[hc] = map[int]*port{}
	return nil
}

// Run is de eigenaar van de bus: scannen, HID-rapporten ophalen en de
// verzoeken van opslagapparaten bedienen, allemaal op deze ene goroutine.
// Blokkeert; start hem als goroutine.
//
// Hij slaapt tot er een verzoek binnenkomt of er iets te doen is: de volgende
// pollronde, of — zolang er een bulk-transfer loopt — de volgende blik op de
// event-ring.
func (m *Manager) Run() {
	now := time.Now()
	nextScan, nextPoll := now, now
	wake := time.NewTimer(0)
	for {
		select {
		case r := <-m.reqs:
			m.enqueue(r)
		case <-wake.C:
		}
		if !time.Now().Before(nextScan) {
			m.Scan()
			nextScan = time.Now().Add(scanInterval)
		}
		if !time.Now().Before(nextPoll) {
			m.Poll()
			nextPoll = time.Now().Add(pollInterval)
		}
		m.serveBulk()
		wait := time.Until(nextPoll)
		if len(m.busy) > 0 {
			step := time.Millisecond
			if m.fine > 0 {
				m.fine--
				step = 100 * time.Microsecond
			}
			wait = min(wait, step)
		}
		wake.Reset(wait)
	}
}

// Scan kijkt welke poorten er bij zijn gekomen en welke leeg zijn geraakt.
func (m *Manager) Scan() {
	for _, hc := range m.hcs {
		known := m.ports[hc]
		if cause := hc.RecoveryNeeded(); cause != nil {
			// HCRST maakt elk bestaand Device-handle ongeldig, ook als maar
			// één slot de oorspronkelijke fout gaf. Vergeet ze daarom zonder
			// Disable Slot te sturen, maar laat eerst alle lokaal onthouden
			// toetsen en muisknoppen los. Na Recover ziet de normale scan
			// aangesloten apparaten in dezelfde ronde opnieuw.
			m.forgetController(known)
			if err := hc.Recover(); err != nil {
				fmt.Printf("usb: %s: controllerherstel na %v mislukt: %v\n", hc.Name, cause, err)
				continue
			}
			fmt.Printf("usb: %s: controller hersteld na %v; poorten worden opnieuw gescand\n", hc.Name, cause)
		}
		for _, p := range hc.Ports() {
			cur, have := known[p.Num]
			switch {
			case p.Connected && !have:
				// Een net ingeplugd apparaat heeft tijd nodig voor zijn
				// voeding stabiel is; de reset erna wacht op de poort zelf.
				d, err := hc.Attach(p.Num)
				if err != nil {
					fmt.Printf("usb: %s port %d: %v\n", hc.Name, p.Num, err)
					hc.ClearChanges(p.Num)
					continue
				}
				if d != nil && d.MassStorage() {
					// Opslag hoort niet bij invoer, maar de bus heeft één eigenaar en
					// dat is deze scanner. Het apparaat blijft dus hier in de
					// boekhouding, zodat uittrekken gezien wordt, en wie er wél iets
					// mee doet krijgt een Bulk via Storage. Niemand geregistreerd?
					// Dan is het precies zoals eerst: een apparaat dat er hangt.
					fmt.Printf("usb: %s port %d: mass storage %04x:%04x\n", hc.Name, p.Num, d.VendorID, d.ProductID)
					b := &Bulk{
						VendorID: d.VendorID, ProductID: d.ProductID, Host: hc.Name, Port: p.Num,
						m: m, hc: hc, dev: d, max: d.MaxTransfer(), gone: make(chan struct{}),
					}
					known[p.Num] = &port{dev: d, bulk: b}
					hc.ClearChanges(p.Num)
					if Storage != nil {
						Storage(b)
					}
					continue
				}
				if d == nil {
					// Wel iets, maar geen boot-HID en geen opslag. Geen fout: een
					// dongle in de poort is gewoon niets voor deze stack.
					fmt.Printf("usb: %s port %d: device is not a boot-HID — ignored\n", hc.Name, p.Num)
					// Onthouden tot uittrekken of controllerherstel: telkens opnieuw
					// enumereren levert geen invoer op.
					known[p.Num] = &port{}
					hc.ClearChanges(p.Num)
					continue
				}
				fmt.Printf("usb: %s port %d: %v\n", hc.Name, p.Num, d)
				known[p.Num] = &port{dev: d, buf: make([]byte, 16)}
			case !p.Connected && have:
				fmt.Printf("usb: %s port %d: %v unplugged\n", hc.Name, p.Num, cur.dev)
				m.release(cur)
				delete(known, p.Num)
			}
			hc.ClearChanges(p.Num)
		}
	}
}

// release laat alles los wat dit apparaat vast hield. De Reset van de decoders
// is geen opruimwerk maar een correctie: een toets die tijdens het uittrekken
// ingedrukt was, moet bij de display worden losgelaten — anders blijft hij daar
// voor altijd staan.
func (m *Manager) release(p *port) {
	if p.dev == nil {
		return // bekend niet-HID-apparaat; Attach gaf zijn hardwareslot al terug
	}
	if p.bulk != nil {
		m.dropBulk(p.bulk)
	}
	m.evs = m.evs[:0]
	m.appendReset(p)
	m.emit()
	if err := p.dev.Detach(); err != nil {
		fmt.Printf("usb: detach kon controller-slot niet bevestigen: %v\n", err)
	}
}

// forgetController laat decoderstate los en vergeet alle handles na een
// ownership-fout. Het roept bewust geen Detach aan: de daaropvolgende HCRST is
// juist de hardware-operatie die alle slots atomair vrijmaakt, en oude Device-
// handles zijn daarna niet meer geldig.
func (m *Manager) forgetController(known map[int]*port) {
	m.evs = m.evs[:0]
	for _, p := range known {
		m.appendReset(p)
		if p.bulk != nil {
			m.dropBulk(p.bulk)
		}
	}
	m.emit()
	clear(known)
}

func (m *Manager) appendReset(p *port) {
	m.evs = p.kb.Reset(m.evs)
	m.evs = p.ms.Reset(m.evs)
}

// Poll haalt één ronde rapporten op.
func (m *Manager) Poll() {
	for hc, known := range m.ports {
		// Een Disable Slot zonder completion maakt ook de command/event-state
		// verdacht. Poll daarom geen enkel oud Device-handle meer tussen die
		// fout en de controllerreset in de volgende scanronde.
		if hc.RecoveryNeeded() != nil {
			continue
		}
		for _, p := range known {
			if p.dev == nil {
				continue // bekend niet-HID-apparaat
			}
			// Meerdere keren per beurt: één apparaat kan twee endpoints hebben
			// (toetsenbord én muis op één dongle) en Report levert er één per
			// aanroep. Begrensd op maxPerPoll zodat een druk apparaat de
			// andere poorten niet uithongert.
			for k := 0; k < maxPerPoll; k++ {
				if p.dev.MassStorage() {
					break // geen invoer: deze poort levert bytes op verzoek
				}
				n, proto, ok := p.dev.Report(p.buf)
				if !ok {
					break
				}
				r := p.buf[:n]
				m.evs = m.evs[:0]
				if proto == xhci.ProtoMouse {
					m.evs = p.ms.Decode(r, m.evs)
				} else {
					m.evs = p.kb.Decode(r, m.evs)
				}
				m.emit()
			}
		}
	}
}

func (m *Manager) emit() {
	if m.sink != nil {
		for _, e := range m.evs {
			m.sink(e)
		}
	}
	m.evs = m.evs[:0]
}
