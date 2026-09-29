package mdio

import (
	"testing"
	"time"
)

// fakePHY is een clause-22-PHY op de tafel: leest uit een registermap en
// schrijft mee wat de driver aanraakte. Precies genoeg om de twee dingen te
// bewijzen die geen bord nodig hebben: welke registers we schrijven, en hoe we
// de snelheid uit het antwoord van de tegenpartij lezen.
type fakePHY struct {
	reg     map[int]uint16
	written []int
	read    []int
}

func (p *fakePHY) MDIORead(phy, reg int) uint16 {
	p.read = append(p.read, reg)
	return p.reg[reg]
}

func (p *fakePHY) MDIOWrite(phy, reg int, val uint16) {
	p.written = append(p.written, reg)
	p.reg[reg] = val
}

func (p *fakePHY) touched(list []int, reg int) bool {
	for _, r := range list {
		if r == reg {
			return true
		}
	}
	return false
}

// linked geeft een PHY die meteen link + AN-complete meldt.
func linked(lpa, gsta uint16) *fakePHY {
	return &fakePHY{reg: map[int]uint16{
		1:  1<<5 | 1<<2, // BMSR: AN complete + link up
		5:  lpa,         // ANLPAR
		10: gsta,        // GBSR
	}}
}

func TestAutoNegGigabit(t *testing.T) {
	p := linked(1<<8, 1<<11) // tegenpartij kan 100FD én 1000FD
	speed, fd, err := AutoNeg(p, 0, time.Second)
	if err != nil {
		t.Fatalf("AutoNeg: %v", err)
	}
	if speed != 1000 || !fd {
		t.Errorf("speed=%d fd=%v, wil 1000 full duplex", speed, fd)
	}
	if !p.touched(p.written, 9) {
		t.Error("gigabit-PHY: register 9 (GBCR) niet geadverteerd")
	}
}

// De ePHY van de SG2002 kan alleen 10/100 en heeft register 9/10 niet. Deze
// test is de wacht bij die eis: niets schrijven wat er niet is, en een
// (onmogelijke) 1000-melding in GBSR mag de uitkomst niet bepalen.
func TestAutoNegFastLaatGigabitRegistersOngemoeid(t *testing.T) {
	p := linked(1<<8, 1<<11)
	speed, fd, err := AutoNegFast(p, 0, time.Second)
	if err != nil {
		t.Fatalf("AutoNegFast: %v", err)
	}
	if speed != 100 || !fd {
		t.Errorf("speed=%d fd=%v, wil 100 full duplex", speed, fd)
	}
	if p.touched(p.written, 9) {
		t.Error("register 9 (GBCR) geschreven op een PHY zonder gigabit")
	}
	if p.touched(p.read, 10) {
		t.Error("register 10 (GBSR) gelezen op een PHY zonder gigabit")
	}
}

func TestAutoNegSnelheidUitANLPAR(t *testing.T) {
	for _, c := range []struct {
		name  string
		lpa   uint16
		speed int
		fd    bool
	}{
		{"100FD", 1 << 8, 100, true},
		{"100HD", 1 << 7, 100, false},
		{"10FD", 1 << 6, 10, true},
		{"10HD", 0, 10, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			speed, fd, err := AutoNegFast(linked(c.lpa, 0), 0, time.Second)
			if err != nil {
				t.Fatalf("AutoNegFast: %v", err)
			}
			if speed != c.speed || fd != c.fd {
				t.Errorf("speed=%d fd=%v, wil %d/%v", speed, fd, c.speed, c.fd)
			}
		})
	}
}

func TestAutoNegZonderLinkGeeftFout(t *testing.T) {
	p := &fakePHY{reg: map[int]uint16{1: 0}} // BMSR: niets
	if _, _, err := AutoNegFast(p, 0, 10*time.Millisecond); err == nil {
		t.Fatal("geen fout terwijl er geen link is")
	}
}

func TestScanSlaatLegeAdressenOver(t *testing.T) {
	// Adres 0 leest 0xFFFF (niets aanwezig), adres 1 zwijgt met nullen, en de
	// PHY zit op 2 — de scan hoort die te vinden en zijn id's te geven.
	p := &phyBus{ids: map[int][2]uint16{0: {0xFFFF, 0xFFFF}, 1: {0, 0}, 2: {0x0043, 0x5649}}}
	addr, id1, id2, found := Scan(p)
	if !found || addr != 2 || id1 != 0x0043 || id2 != 0x5649 {
		t.Fatalf("Scan = (%d, %04x:%04x, %v), wil (2, 0043:5649, true)", addr, id1, id2, found)
	}
}

// phyBus is een bus met meerdere adressen (Scan-kant).
type phyBus struct {
	ids map[int][2]uint16
}

func (b *phyBus) MDIORead(phy, reg int) uint16 {
	id, ok := b.ids[phy]
	if !ok {
		return 0xFFFF
	}
	if reg == 2 {
		return id[0]
	}
	return id[1]
}

func (b *phyBus) MDIOWrite(phy, reg int, val uint16) {}
