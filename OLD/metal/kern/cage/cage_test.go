// Host-tests voor de kooi-encodering. De PMP-rekenkunde is precies de plek
// waar een stille fout een lek oplevert (een venster dat één bit te groot is
// dekt de buurpartitie), dus die hoort op de host bewezen — de stub schrijft
// alleen nog weg wat hier is uitgerekend.
package cage

import "testing"

// Het plan van een slot: partitie + control page + granted MMIO. Elk venster is
// TOR, dus twee entries (onder- en bovengrens), afgesloten met de deny-all.
// Precies acht entries — het budget van de C906 is daarmee vol, en dat is een
// eigenschap om te kennen en niet om tegenaan te lopen.
func TestEncodeSlotPlan(t *testing.T) {
	addr, cfg, err := Encode(Plan{Allow: []Window{
		{Base: 0x88000000, Size: 64 << 20, R: true, W: true, X: true}, // app-partitie
		{Base: 0x8FF10000, Size: 4 << 10, R: true, W: true},           // control page
		{Base: 0x04140000, Size: 4 << 10, R: true, W: true},           // UART0 (granted MMIO)
	}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Per paar: een byte 0x00 (A=OFF — dit pmpaddr is alléén de ondergrens van
	// zijn opvolger) en een byte 0x08|rechten (A=TOR). De deny-all sluit af met
	// 0x08: actief, geen rechten. Géén L-bit ergens: locken zou HOP's eigen
	// M-mode meebinden en dan kan de switcher de kooi niet wisselen.
	if cfg != 0x8000b000b000f00 {
		t.Errorf("pmpcfg0 = %#x, wil 0x8000b000b000f00", cfg)
	}
	want := []uint64{
		0x22000000, 0x23000000, // partitie 0x88000000..0x8C000000
		0x23fc4000, 0x23fc4400, // control page
		0x1050000, 0x1050400, // UART0
		0x0, 0x4000000000, // deny-all: nul tot de hele PA-ruimte
	}
	if len(addr) != len(want) {
		t.Fatalf("%d entries, wil %d", len(addr), len(want))
	}
	for i := range want {
		if addr[i] != want[i] {
			t.Errorf("pmpaddr%d = %#x, wil %#x", i, addr[i], want[i])
		}
	}
}

// Dit is waarvoor TOR bestaat: een partitie die géén macht van twee is. Onder
// NAPOT werd 124MB naar 128MB afgerond en die past op de LicheeRV nergens
// uitgelijnd (HOP zit middenin het DRAM), dus strandde zo'n job op een grens die
// niet in het silicium zat maar in onze encodering.
func TestEncodeWillekeurigeMaat(t *testing.T) {
	addr, cfg, err := Encode(Plan{Allow: []Window{
		{Base: 0x88000000, Size: 124 << 20, R: true, W: true, X: true},
	}})
	if err != nil {
		t.Fatalf("Encode 124MB: %v", err)
	}
	if addr[0] != 0x22000000 || addr[1] != 0x23f00000 {
		t.Errorf("grenzen %#x..%#x, wil 0x22000000..0x23f00000", addr[0], addr[1])
	}
	// De bovengrens moet exact op het einde van de partitie liggen: één bit te
	// hoog en de buurpartitie ligt in de kooi.
	if got := addr[1] << 2; got != 0x88000000+124<<20 {
		t.Errorf("bovengrens dekt tot %#x, wil %#x", got, 0x88000000+124<<20)
	}
	if cfg != 0x8000f00 {
		t.Errorf("pmpcfg0 = %#x, wil 0x8000f00", cfg)
	}
}

// De deny-all moet áltijd de laatste actieve entry zijn en géén enkel recht
// hebben: dat is de helft van de invariant (de andere helft is Verify's readback
// op het hart).
func TestDenyAllIsLastAndGrantsNothing(t *testing.T) {
	addr, cfg, err := Encode(Plan{Allow: []Window{
		{Base: 0x88000000, Size: 64 << 20, R: true, W: true, X: true},
	}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	deny := byte(cfg >> 24) // entry 3: het paar ná de partitie
	if deny&cfgATOR == 0 {
		t.Errorf("deny-all is niet actief (A-veld = %#x)", deny)
	}
	if deny&(cfgR|cfgW|cfgX) != 0 {
		t.Errorf("deny-all heeft rechten: %#x", deny)
	}
	if addr[2] != 0 || addr[3] != 1<<40>>2 {
		t.Errorf("deny-all dekt %#x..%#x, wil 0..%#x", addr[2]<<2, addr[3]<<2, uint64(1)<<40)
	}
	// Géén enkele entry gelockt: dat zou HOP's eigen M-mode meebinden.
	if cfg&0x8080808080808080 != 0 {
		t.Errorf("een entry is gelockt (L-bit): %#x", cfg)
	}
}

// Een grens die niet op de PMP-korrel valt moet hard falen, niet stil iets
// anders dekken: bij een korrel > 4 bytes leest het silicium de onderste bits
// van pmpaddr als nul.
func TestEncodeWeigertKrommeGrenzen(t *testing.T) {
	for _, tc := range []struct {
		naam string
		w    Window
	}{
		{"basis niet op de korrel", Window{Base: 0x88000800, Size: 64 << 20}},
		{"maat niet op de korrel", Window{Base: 0x88000000, Size: 0x1800}},
		{"maat nul", Window{Base: 0x88000000, Size: 0}},
	} {
		if _, _, err := Encode(Plan{Allow: []Window{tc.w}}); err == nil {
			t.Errorf("%s: Encode accepteerde het venster", tc.naam)
		}
	}
}

// Wat TOR juist WEL moet accepteren: een bereik dat geen macht van twee is en
// niet natuurlijk uitgelijnd — dat was de hele NAPOT-beperking.
func TestEncodeAccepteertOngealigneerdBereik(t *testing.T) {
	if _, _, err := Encode(Plan{Allow: []Window{
		{Base: 0x88001000, Size: 3 << 20, R: true},
	}}); err != nil {
		t.Errorf("Encode weigerde een geldig TOR-bereik: %v", err)
	}
}

// Het entry-budget is hard: meer vensters dan de C906 heeft = weigeren, niet
// de deny-all eraf laten vallen (dat zou de kooi stil openzetten). Met TOR kost
// elk venster twee entries, dus vier vensters passen al niet meer.
func TestEncodeWeigertTeVeelVensters(t *testing.T) {
	var p Plan
	for i := range 4 {
		p.Allow = append(p.Allow, Window{Base: uint64(0x88000000 + i*0x100000), Size: 4 << 10, R: true})
	}
	if _, _, err := Encode(p); err == nil {
		t.Fatalf("Encode accepteerde 4 vensters + deny-all (%d entries) in %d", 4*2+2, MaxEntries)
	}
}
