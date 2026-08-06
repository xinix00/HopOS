package mdio

import "testing"

// De RTL8211F-configuratie is een pagina-wissel met twee read-modify-writes
// erin, en dat is precies het soort volgorde dat op ijzer stil misgaat: blijft
// de PHY op pagina 0xd08 staan, dan beantwoordt hij geen enkele normale
// clause-22-vraag meer en lijkt hij verdwenen. Host-testbaar, dus hier.
//
// Deze tests bestaan omdat de ontbrekende TX-delay op 06-08 een gigabit-link
// opleverde waarover geen enkel verzonden frame aankwam — RX foutloos, TX in het
// niets. Eén bit in de PHY.

// pagedPHY registreert alle MDIO-verkeer en houdt per pagina zijn eigen
// registers bij, zodat een vergeten pagina-wissel zichtbaar wordt.
type pagedPHY struct {
	page  uint16
	regs  map[uint16]map[int]uint16 // pagina → reg → waarde
	trace []string
}

func newPagedPHY() *pagedPHY {
	return &pagedPHY{regs: map[uint16]map[int]uint16{}}
}

func (f *pagedPHY) at(reg int) uint16 {
	p, ok := f.regs[f.page]
	if !ok {
		return 0
	}
	return p[reg]
}

func (f *pagedPHY) MDIORead(phy, reg int) uint16 {
	if reg == rtlPageSelect {
		return f.page
	}
	return f.at(reg)
}

func (f *pagedPHY) MDIOWrite(phy, reg int, val uint16) {
	if reg == rtlPageSelect {
		f.page = val
		f.trace = append(f.trace, "page")
		return
	}
	if f.regs[f.page] == nil {
		f.regs[f.page] = map[int]uint16{}
	}
	f.regs[f.page][reg] = val
	f.trace = append(f.trace, "write")
}

func TestRTL8211FZetBeideDelaysVoorRgmiiID(t *testing.T) {
	f := newPagedPHY()
	// Beginstand zoals pin-strapping hem kan achterlaten: rx aan, tx uit —
	// exact het geval dat op 06-08 een link gaf waarover niets aankwam.
	f.regs[rtlPageDelay] = map[int]uint16{rtlRegTXDelay: 0x0000, rtlRegRXDelay: rtlBitRXDelay}

	ConfigureRTL8211F(f, 1, true, true)

	if f.page != rtlPageStd {
		t.Errorf("PHY blijft op pagina %#x staan — normale registers zijn dan onbereikbaar", f.page)
	}
	if f.regs[rtlPageDelay][rtlRegTXDelay]&rtlBitTXDelay == 0 {
		t.Errorf("TX-delay niet gezet: reg 0x11 = %#04x", f.regs[rtlPageDelay][rtlRegTXDelay])
	}
	if f.regs[rtlPageDelay][rtlRegRXDelay]&rtlBitRXDelay == 0 {
		t.Errorf("RX-delay niet gezet: reg 0x15 = %#04x", f.regs[rtlPageDelay][rtlRegRXDelay])
	}
}

func TestRTL8211FWistDelaysVoorKaalRgmii(t *testing.T) {
	f := newPagedPHY()
	f.regs[rtlPageDelay] = map[int]uint16{
		rtlRegTXDelay: rtlBitTXDelay | 0x00FF, // andere bits die moeten blijven
		rtlRegRXDelay: rtlBitRXDelay | 0xFF00,
	}

	ConfigureRTL8211F(f, 1, false, false)

	if got := f.regs[rtlPageDelay][rtlRegTXDelay]; got&rtlBitTXDelay != 0 || got&0x00FF != 0x00FF {
		t.Errorf("TX-reg = %#04x: delay moest weg en de rest moest blijven", got)
	}
	if got := f.regs[rtlPageDelay][rtlRegRXDelay]; got&rtlBitRXDelay != 0 || got&0xFF00 != 0xFF00 {
		t.Errorf("RX-reg = %#04x: delay moest weg en de rest moest blijven", got)
	}
}

func TestRTL8211FSchakeltAltijdTerugNaarPaginaNul(t *testing.T) {
	// De defer moet ook lopen als er onderweg niets te wijzigen valt. Een PHY
	// die op 0xd08 achterblijft ziet er in een scan uit als verdwenen.
	f := newPagedPHY()
	ConfigureRTL8211F(f, 1, true, true)
	if f.page != rtlPageStd {
		t.Fatalf("pagina bleef %#x", f.page)
	}
	tx, rx := RTL8211FDelays(f, 1)
	if !tx || !rx {
		t.Errorf("teruglezen geeft tx=%v rx=%v, wil beide true", tx, rx)
	}
	if f.page != rtlPageStd {
		t.Errorf("RTL8211FDelays liet de pagina op %#x staan", f.page)
	}
}

func TestIsRTL8211FHerkentHetGemetenID(t *testing.T) {
	// Dit is letterlijk het id dat de MDIO-scan op de Radxa las, en dat in de
	// Linux-tabel "RTL8211F Gigabit Ethernet" heet.
	if !IsRTL8211F(0x001C, 0xC916) {
		t.Error("001c:c916 wordt niet herkend")
	}
	if IsRTL8211F(0x0007, 0xC0F0) { // de Broadcom van de Pi 5
		t.Error("een andere PHY wordt als RTL8211F herkend")
	}
}
