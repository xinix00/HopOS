package dwmac4

import "testing"

// De bit-arithmetiek van deze driver hoort op de host bewezen te worden en niet
// op een bordje waar één ronde een SD-kaartwissel kost. Deze tests bestaan omdat
// precies dit fout ging bij de VORIGE generatie (driver/nic/dwmac, 30-07): de
// buffergrootte werd door een te smal veld naar nul gemaskeerd en de MAC kreeg
// "elke buffer is 0 bytes" te horen — link stond, TX liep, RX gaf descriptors
// terug zonder één frame. Dezelfde velden bestaan hier, alleen breder.

func TestMacConfigZetDeJuisteSnelheidsbits(t *testing.T) {
	// PS = MII (10/100), FES = 100 i.p.v. 10, geen van beide = GMII (1000).
	// Uit dwmac4_core.c: mac->link.speed10 = PS, speed100 = FES|PS,
	// speed1000 = 0, speed_mask = FES|PS.
	for _, c := range []struct {
		speed                   int
		fd                      bool
		wantPS, wantFES, wantDM bool
	}{
		{10, false, true, false, false},
		{10, true, true, false, true},
		{100, true, true, true, true},
		{1000, true, false, false, true},
		{1000, false, false, false, false},
	} {
		got := macConfig(0, c.speed, c.fd)
		if (got&cfgPS != 0) != c.wantPS {
			t.Errorf("%dMbit: PS=%v, wil %v (cfg %#08x)", c.speed, got&cfgPS != 0, c.wantPS, got)
		}
		if (got&cfgFES != 0) != c.wantFES {
			t.Errorf("%dMbit: FES=%v, wil %v (cfg %#08x)", c.speed, got&cfgFES != 0, c.wantFES, got)
		}
		if (got&cfgDM != 0) != c.wantDM {
			t.Errorf("%dMbit fd=%v: DM=%v, wil %v", c.speed, c.fd, got&cfgDM != 0, c.wantDM)
		}
	}
}

func TestMacConfigWistDeVorigeSnelheidEnLaatJumboUit(t *testing.T) {
	// Van gigabit terug naar 10Mbit: als het masker de FES-bit niet wist, blijft
	// een node na een link-flap op de verkeerde snelheid klokken. En jumbo moet
	// UIT blijven — met JE aan zou de MAC frames tot 9018 bytes accepteren die
	// Receive (die FD|LD in één descriptor eist) stil weggooit.
	prev := macConfig(0, 100, true)
	if prev&cfgFES == 0 {
		t.Fatal("100Mbit zet FES niet — de rest van deze test heeft geen zin")
	}
	got := macConfig(prev|cfgJE, 1000, true)
	if got&cfgFES != 0 || got&cfgPS != 0 {
		t.Errorf("terug naar 1000Mbit laat snelheidsbits staan: %#08x", got)
	}
	if got&cfgJE != 0 {
		t.Errorf("jumbo (JE) blijft aan staan: %#08x", got)
	}
	// Idempotent: nog een keer met dezelfde snelheid geeft dezelfde stand.
	if again := macConfig(got, 1000, true); again != got {
		t.Errorf("niet idempotent: %#08x → %#08x", got, again)
	}
}

func TestMacConfigHoudtDeCoreInitBits(t *testing.T) {
	got := macConfig(0, 1000, true)
	if got&coreInit != coreInit {
		t.Errorf("core-init-bits (JD|BE|DCRS) ontbreken: %#08x", got)
	}
	// ACS (bit 20) mag NIET aan: wij strippen de CRC zelf in rxLen, en met ACS
	// aan zouden we vier bytes te veel aftrekken.
	if got&(1<<20) != 0 {
		t.Errorf("ACS staat aan terwijl rxLen de FCS zelf aftrekt: %#08x", got)
	}
}

func TestRxControlZetEenEchteBuffergrootte(t *testing.T) {
	got := rxControl(0, bufSize)
	// RBSZ is [14:1]: de maat staat er maal twee in.
	if size := (got & rxRBSZMask) >> rxRBSZShift; size != bufSize {
		t.Errorf("RBSZ leest terug als %d, wil %d (rx-control %#08x)", size, bufSize, got)
	}
	if got&(pbl<<rxPBLShift) == 0 {
		t.Errorf("burstlengte ontbreekt: %#08x", got)
	}
	// Een tweede aanroep over de eerste heen mag de maat niet verdubbelen of
	// vervuilen — dat is waarom het veld eerst gewist wordt.
	if again := rxControl(got, bufSize); again != got {
		t.Errorf("niet idempotent: %#08x → %#08x", got, again)
	}
}

func TestRxControlWeigertEenMaatDieNietInHetVeldPast(t *testing.T) {
	for _, size := range []int{0, -8, 1537, 0x8000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("buffergrootte %d werd geaccepteerd", size)
				}
			}()
			rxControl(0, size)
		}()
	}
}

func TestTxDescriptorwoordenDragenDeLengteTweeKeer(t *testing.T) {
	// TDES2 = bufferlengte [13:0], TDES3 = pakketlengte [14:0] + FD|LD|OWN.
	// Twee velden, dezelfde lengte: wie er één vergeet krijgt een MAC die het
	// frame wel pakt maar de verkeerde hoeveelheid bytes verstuurt.
	const n = 1000
	des2, des3 := txDesc2en3(n)
	if des2 != n {
		t.Errorf("TDES2 = %#x, wil %d", des2, n)
	}
	if got := des3 & txPktLenMask; got != n {
		t.Errorf("TDES3-pakketlengte = %d, wil %d", got, n)
	}
	for name, bit := range map[string]uint32{"OWN": txOwn, "FD": txFirst, "LD": txLast} {
		if des3&bit == 0 {
			t.Errorf("TDES3 mist %s: %#08x", name, des3)
		}
	}
}

func TestTxDescriptorwoordenWeigerenOnmogelijkeLengtes(t *testing.T) {
	for _, n := range []int{0, -1, maxFrame + 1, 0x10000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("framelengte %d werd geaccepteerd", n)
				}
			}()
			txDesc2en3(n)
		}()
	}
}

func TestRxLenTrektDeFCSEraf(t *testing.T) {
	// De MAC meldt de lengte MÉT CRC (wij zetten ACS niet), dus moeten er vier
	// bytes af. Vergeet je dat, dan komt élk frame vier bytes te lang de stack in
	// en faalt iedere checksum — een fout die zich als "netwerk werkt niet"
	// voordoet in plaats van als een lengtefout.
	if got := rxLen(rxOwn | rxFirst | rxLast | 1518); got != 1514 {
		t.Errorf("rxLen(1518) = %d, wil 1514", got)
	}
	// En een onzinnig korte melding mag geen negatieve lengte geven: dat zou een
	// slice-panic in Receive worden.
	for _, raw := range []uint32{0, 1, 4} {
		if got := rxLen(raw); got != 0 {
			t.Errorf("rxLen(%d) = %d, wil 0", raw, got)
		}
	}
	// De statusbits boven [14:0] mogen niet in de lengte lekken.
	if got := rxLen(0xFFFF8000 | 100); got != 96 {
		t.Errorf("statusbits lekken in de lengte: %d", got)
	}
}

func TestDeRingenPassenInNeedBytesZonderOverlap(t *testing.T) {
	// initRings verdeelt de regio met dezelfde som; als die twee uit elkaar lopen
	// legt de TX-ring op de RX-buffers (dat gebeurde bij de vorige generatie toen
	// numDesc van 32 naar 64 ging — link stond, DHCP kreeg geen lease meer).
	if descBytes != (numRx+numTx)*descSize {
		t.Errorf("descBytes %d klopt niet met %d descriptors", descBytes, numRx+numTx)
	}
	if NeedBytes != descBytes+bufBytes {
		t.Errorf("NeedBytes %d ≠ descBytes %d + bufBytes %d", NeedBytes, descBytes, bufBytes)
	}
	// De vier gebieden op een rij: rx-desc, tx-desc, rx-buf, tx-buf.
	type span struct {
		name       string
		base, size uintptr
	}
	spans := []span{
		{"rx-desc", 0, numRx * descSize},
		{"tx-desc", numRx * descSize, numTx * descSize},
		{"rx-buf", descBytes, numRx * bufSize},
		{"tx-buf", descBytes + numRx*bufSize, numTx * bufSize},
	}
	for i, a := range spans {
		if a.base+a.size > NeedBytes {
			t.Errorf("%s loopt tot %#x, voorbij NeedBytes %#x", a.name, a.base+a.size, uintptr(NeedBytes))
		}
		for _, b := range spans[i+1:] {
			if a.base < b.base+b.size && b.base < a.base+a.size {
				t.Errorf("%s (%#x+%#x) overlapt %s (%#x+%#x)", a.name, a.base, a.size, b.name, b.base, b.size)
			}
		}
	}
}

func TestBufferPastBovenDeMaximaleFrameMaat(t *testing.T) {
	// De buffer moet een heel frame kunnen dragen: doet hij dat niet, dan komt
	// een MTU-frame over twee descriptors binnen en gooit Receive het weg (die
	// eist FD|LD in één descriptor).
	if bufSize < maxFrame {
		t.Errorf("bufSize %d is kleiner dan maxFrame %d", bufSize, maxFrame)
	}
}

func TestMDIOVeldenStaanOpDeDWMAC4Posities(t *testing.T) {
	// De MDIO-indeling verschilt per DWMAC-generatie, en dit is de reden dat er
	// twee pakketten zijn. Hier de posities uit dwmac4_core.c (dwmac4_setup):
	// addr_shift 21, reg_shift 16, clk_csr_shift 8 — met de commandobits uit
	// stmmac_mdio.c. Een verwisselde shift geeft een bus die stil naar het
	// verkeerde register schrijft.
	const phy, reg = 3, 5
	want := uint32(phy)<<21 | uint32(reg)<<16 | CSR100_150M<<8 | 3<<2 | 1
	got := uint32(phy&0x1F)<<mdioAddrShift | uint32(reg&0x1F)<<mdioRegShift |
		CSR100_150M<<mdioCSRShift | mdioRead | mdioBusy
	if got != want {
		t.Errorf("MDIO-commando %#08x, wil %#08x", got, want)
	}
	if mdioWrite != 1<<2 {
		t.Errorf("MII_GMAC4_WRITE = %#x, wil %#x", mdioWrite, 1<<2)
	}
}
