//go:build gui

package usbin

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/gui/driver/usb/hid"
	"github.com/xinix00/HopOS/metal/gui/fbgrant"
)

// Dit bestand is de weg van HOP naar het scherm: een stroom van dezelfde
// JSON-events die de browser-KVM al post, over één verbinding.
//
// DE APP BELT, HOP LUISTERT — en het adres reist mee in de fb-grant, naast de
// FB_*-velden (besluit Derek 06-08: "we kunnen hem ook meesturen met de grant
// van de GUI, want dat is TOCH wat nodig is voor keyboard en muis"). Het glas,
// het toetsenbord en de muis zijn één zitplaats, dus ze worden in één keer
// overgedragen.
//
// Dat is niet alleen netter, het is minder. Toen HOP nog naar de app belde, moest
// hij hem eerst vínden: het slot uit de grant, het IP uit het netwerkvlak, de
// poort uit de env van de job — en omdat gui het netwerkvlak niet mag lezen,
// werd die knoop in cmd gelegd. Al dat opzoeken is weg: de app weet waar HOP is,
// want HOP heeft het hem verteld.
//
// WAT ER OVERBLIJFT AAN WANTROUWEN: op accept controleert HOP dat de beller het
// slot ís dat het glas vasthoudt. Sinds de switch-harding van vandaag draagt dat
// gewicht — een slot kan alleen zijn eigen MAC gebruiken en de interne buren
// staan statisch, dus een opgezette TCP-verbinding kan niet van een ander slot
// komen dan zijn bron-IP zegt.
//
// GEEN TWEEDE VOCABULAIRE: exact het object dat /input al aanneemt, één per
// regel. De display ontleedt het met dezelfde code als een browserklik, dus een
// echt toetsenbord blijft niet te onderscheiden van de KVM-pagina.

// inputPort is de poort op het interne gateway-adres (10.100.0.1). Naast 7878
// (SURF) omdat het de andere helft van hetzelfde kanaal is. Het nummer hoeft
// niet beroemd te zijn — het reist mee in de grant.
const inputPort = 7879

// queueDepth: invoer is LOSSY BY DESIGN — dezelfde afspraak als de input-pomp
// in de display zelf. Een display die even niet leest mag de USB-pollus niet
// stilzetten, dus vol = weggooien.
const queueDepth = 256

// De KVM-pagina stuurt ABSOLUTE coördinaten (een canvas kent geen deltas), een
// USB-muis relatieve. Deze laag houdt dus de cursor bij en clamped hem op de
// schermmaat — dezelfde plek waar de display hem tekent.
type cursor struct {
	x, y, w, h int
}

func (c *cursor) move(dx, dy int) {
	c.x, c.y = clamp(c.x+dx, c.w-1), clamp(c.y+dy, c.h-1)
}

func clamp(v, max int) int {
	switch {
	case v < 0:
		return 0
	case max > 0 && v > max:
		return max
	}
	return v
}

// deliverer serveert de invoerstroom.
type deliverer struct {
	q   chan hid.Event
	cur cursor

	l    net.Listener
	mu   sync.Mutex
	conn net.Conn // de huidige display-verbinding (nil = niemand kijkt)
}

// close ruimt de luisterpost op. Nodig als Start alsnog afhaakt (geen enkele
// werkende controller): dan hoort er geen poort open te blijven staan waar
// niemand ooit iets uit krijgt.
func (d *deliverer) close() {
	d.l.Close()
	close(d.q)
}

// listen opent de luisterpost op het interne gateway-adres en geeft het adres
// terug dat in de grant mee moet. Alleen dát adres: het is gebonden aan HOP's
// interne NIC, dus van buiten de node is er niets te bereiken.
func listen() (*deliverer, string, error) {
	addr := fmt.Sprintf("%s:%d", layout.IP4Str(layout.HostIP4()), inputPort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	d := &deliverer{q: make(chan hid.Event, queueDepth), l: l}
	if fb, ok := board.Current().Framebuffer(); ok {
		d.cur.w, d.cur.h = fb.Width, fb.Height
		d.cur.x, d.cur.y = fb.Width/2, fb.Height/2
	}
	go d.accept(l)
	go d.run()
	return d, addr, nil
}

// accept laat alleen de houder van de fb-grant binnen. Een nieuwe verbinding
// verdringt de oude: een display die herstart moet het toetsenbord terugkrijgen
// zonder dat iemand de dode verbinding hoeft op te ruimen.
func (d *deliverer) accept(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			fmt.Printf("usb: input listener stopped: %v\n", err)
			return
		}
		if !d.allowed(c.RemoteAddr()) {
			fmt.Printf("usb: input connection from %v refused — not the framebuffer holder\n", c.RemoteAddr())
			c.Close()
			continue
		}
		d.mu.Lock()
		old := d.conn
		d.conn = c
		d.mu.Unlock()
		if old != nil {
			old.Close()
		}
		fmt.Printf("usb: input stream attached to %v\n", c.RemoteAddr())
	}
}

// allowed vergelijkt de beller met het slot dat het glas vasthoudt. Het
// slot-IP is een deterministische functie van het slotnummer (layout.SlotIP4),
// dus hier is geen tabel en geen netwerkvlak voor nodig.
func (d *deliverer) allowed(a net.Addr) bool {
	slot := fbgrant.Holder()
	if slot == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return false
	}
	return host == layout.IP4Str(layout.SlotIP4(slot))
}

// Sink is de kant die de pollus aanroept: nooit blokkeren.
func (d *deliverer) Sink(e hid.Event) {
	select {
	case d.q <- e:
	default:
	}
}

func (d *deliverer) run() {
	for e := range d.q {
		if e.Kind != hid.MouseMove {
			d.send(d.body(e))
			continue
		}
		// Bewegingen samenvoegen: een muis meldt zich 125x per seconde en
		// alleen de eindpositie is interessant. Zonder dit staat er per
		// tussenstap een bericht in de weg van de volgende toetsaanslag.
		d.cur.move(e.DX, e.DY)
		next, more := d.drainMoves()
		d.send(d.body(hid.Event{Kind: hid.MouseMove}))
		if more {
			d.send(d.body(next))
		}
	}
}

// drainMoves telt alle direct wachtende bewegingen bij de cursor op en geeft
// het eerste event terug dat géén beweging was — dat mag niet verdwijnen, want
// een klik hoort bij de plek waar hij gebeurde.
func (d *deliverer) drainMoves() (hid.Event, bool) {
	for {
		select {
		case e := <-d.q:
			if e.Kind == hid.MouseMove {
				d.cur.move(e.DX, e.DY)
				continue
			}
			return e, true
		default:
			return hid.Event{}, false
		}
	}
}

// body maakt het JSON-event dat /input verwacht (surfserve, inputMsg). Met de
// hand in elkaar gezet en niet via encoding/json: vier velden, en dit pad loopt
// per toetsaanslag.
func (d *deliverer) body(e hid.Event) string {
	switch e.Kind {
	case hid.KeyDown, hid.KeyUp:
		v := 0
		if e.Kind == hid.KeyDown {
			v = 1
		}
		return fmt.Sprintf(`{"k":"key","c":%d,"v":%d}`+"\n", e.Code, v)
	case hid.MouseMove:
		return fmt.Sprintf(`{"k":"move","x":%d,"y":%d}`+"\n", d.cur.x, d.cur.y)
	case hid.MouseDown, hid.MouseUp:
		v := 0
		if e.Kind == hid.MouseDown {
			v = 1
		}
		return fmt.Sprintf(`{"k":"btn","c":%d,"v":%d,"x":%d,"y":%d}`+"\n", e.Code, v, d.cur.x, d.cur.y)
	case hid.MouseWheel:
		return fmt.Sprintf(`{"k":"wheel","c":0,"v":%d,"x":%d,"y":%d}`+"\n", e.DY, d.cur.x, d.cur.y)
	}
	return ""
}

// send schrijft één regel. Geen verbinding = weggooien: er kijkt dan niemand,
// en invoer bewaren voor later levert alleen een lawine bij het aansluiten.
//
// De deadline is er zodat een display die vastloopt de USB-pollus niet
// meesleept: schrijven mag hooguit even duren, daarna gaat de verbinding dicht
// en wacht HOP op een nieuwe.
func (d *deliverer) send(line string) {
	if line == "" {
		return
	}
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c == nil {
		return
	}
	c.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte(line)); err != nil {
		fmt.Printf("usb: input stream lost (%v) — waiting for the display to reconnect\n", err)
		d.mu.Lock()
		if d.conn == c {
			d.conn = nil
		}
		d.mu.Unlock()
		c.Close()
	}
}
