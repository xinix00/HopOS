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
// De weg naar de display loopt over het NETWERK — dezelfde POST /input die de
// browser-KVM al gebruikt (zie metal/gui/usbin/deliver.go). Eén invoerweg, of
// de toets nu van een browser of van echt ijzer komt.
package usbin

import (
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/gui/driver/usb/hid"
	"github.com/xinix00/HopOS/metal/gui/driver/usb/xhci"
)

// Sink krijgt elke gebeurtenis. Mag blokkeren noch panieken: hij draait op de
// pollgoroutine van de invoerdienst.
type Sink func(hid.Event)

// pollInterval is hoe vaak we de event-ring bekijken. Een boot-toetsenbord
// meldt zich elke 8-10ms; 4ms is dus ruim binnen de aanslagsnelheid en kost per
// beurt een handvol MMIO-reads.
const pollInterval = 4 * time.Millisecond

// scanInterval is hoe vaak we naar in- en uitpluggen kijken. Een halve seconde
// wachten op een toetsenbord dat je net insteekt is niet merkbaar, en het houdt
// de poortregisters uit de hete lus.
const scanInterval = 500 * time.Millisecond

// port is één bezette roothub-poort met zijn apparaat en decoder.
type port struct {
	dev *xhci.Device
	kb  hid.Keyboard
	ms  hid.Mouse
	buf []byte
}

// Manager bedient nul of meer controllers.
type Manager struct {
	mu    sync.Mutex
	hcs   []*xhci.HC
	ports map[*xhci.HC]map[int]*port
	sink  Sink
	evs   []hid.Event // hergebruikte buffer: het pollpad mag niet allloceren
}

// New maakt een lege invoerdienst.
func New(sink Sink) *Manager {
	return &Manager{ports: map[*xhci.HC]map[int]*port{}, sink: sink}
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

	m.mu.Lock()
	m.hcs = append(m.hcs, hc)
	m.ports[hc] = map[int]*port{}
	m.mu.Unlock()
	return nil
}

// Run draait de scan- en pollus. Blokkeert; start hem als goroutine.
func (m *Manager) Run() {
	next := time.Now()
	for {
		if time.Now().After(next) {
			m.Scan()
			next = time.Now().Add(scanInterval)
		}
		m.Poll()
		time.Sleep(pollInterval)
	}
}

// Scan kijkt welke poorten er bij zijn gekomen en welke leeg zijn geraakt.
// Publiek zodat een probe hem los kan aanroepen.
func (m *Manager) Scan() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, hc := range m.hcs {
		known := m.ports[hc]
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
				if d == nil {
					// Wel iets, maar geen boot-HID. Geen fout: een stick in de
					// poort is gewoon niets voor deze stack.
					fmt.Printf("usb: %s port %d: device is not a boot-HID — ignored\n", hc.Name, p.Num)
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
	m.evs = p.kb.Reset(m.evs[:0])
	m.evs = p.ms.Reset(m.evs)
	m.emit()
	p.dev.Detach()
}

// Poll haalt één ronde rapporten op.
func (m *Manager) Poll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, known := range m.ports {
		for _, p := range known {
			n, ok := p.dev.Report(p.buf)
			if !ok {
				continue
			}
			r := p.buf[:n]
			m.evs = m.evs[:0]
			if p.dev.Proto == xhci.ProtoMouse {
				m.evs = p.ms.Decode(r, m.evs)
			} else {
				m.evs = p.kb.Decode(r, m.evs)
			}
			m.emit()
		}
	}
}

func (m *Manager) emit() {
	if m.sink == nil {
		return
	}
	for _, e := range m.evs {
		m.sink(e)
	}
	m.evs = m.evs[:0]
}

// Devices geeft een momentopname van wat er aan hangt (diagnose/log).
func (m *Manager) Devices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, known := range m.ports {
		for _, p := range known {
			out = append(out, p.dev.String())
		}
	}
	return out
}
