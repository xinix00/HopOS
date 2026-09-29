package nodemac

import "testing"

// Twee eisen die elkaar tegenspreken en waar dit pakket het compromis is:
// stabiel over reboots (anders elke herstart een nieuwe DHCP-lease en een nieuwe
// hopdns-registratie) én uniek per node (anders botsen twee bordjes van
// hetzelfde type op één LAN). Beide zijn hier host-testbaar, en dat hoort ook:
// een MAC-botsing merk je pas op ijzer, met twee bordjes aan.

func TestExpliciteMacHeeftVoorrang(t *testing.T) {
	got := Identity("02:48:4f:50:aa:bb", "genegeerd", "test")
	want := [6]byte{0x02, 0x48, 0x4f, 0x50, 0xaa, 0xbb}
	if got != want {
		t.Errorf("Identity = %x, wil %x", got, want)
	}
}

func TestNaamAfleidingIsStabielEnUniek(t *testing.T) {
	a := Identity("", "radxa-1", "test")
	if b := Identity("", "radxa-1", "test"); a != b {
		t.Errorf("dezelfde naam gaf twee adressen: %x en %x", a, b)
	}
	// Verschillende namen → verschillende adressen. Vier namen die in de praktijk
	// naast elkaar staan; botsen die, dan is de afleiding waardeloos.
	addrs := map[[6]byte]string{}
	for _, n := range []string{"radxa-1", "radxa-2", "radxa-10", "pi5-1", "licheerv-1", "hopos-a1b2c3d4"} {
		m := Identity("", n, "test")
		if other, dup := addrs[m]; dup {
			t.Errorf("%q en %q krijgen hetzelfde adres %x", n, other, m)
		}
		addrs[m] = n
		if [4]byte{m[0], m[1], m[2], m[3]} != Prefix {
			t.Errorf("%q krijgt voorvoegsel %x, wil %x", n, m[:4], Prefix)
		}
	}
}

func TestLokaalBeheerdEnUnicast(t *testing.T) {
	// Bit 1 van het eerste byte = locally administered (verplicht, anders claimen
	// we OUI-ruimte van iemand anders); bit 0 = multicast en moet NUL zijn, want
	// een multicast-bronadres wordt door switches gedropt.
	for _, m := range [][6]byte{Fallback, Identity("", "radxa-1", "test"), Identity("", "x", "test")} {
		if m[0]&0x02 == 0 {
			t.Errorf("%x is niet locally administered", m)
		}
		if m[0]&0x01 != 0 {
			t.Errorf("%x is een multicast-adres", m)
		}
	}
}

func TestGeenMacEnGeenNaamValtTerugOpDeVasteWaarde(t *testing.T) {
	if got := Identity("", "", "test"); got != Fallback {
		t.Errorf("Identity = %x, wil de terugval %x", got, Fallback)
	}
}

func TestParseWeigertRommelInPlaatsVanHalveAdressen(t *testing.T) {
	// Een typefout in de config mag niet stil een half adres opleveren: dan valt
	// de node terug op de naam-afleiding, wat nog altijd uniek is.
	for _, s := range []string{
		"",
		"02:48:4f:50:aa",       // te kort
		"02:48:4f:50:aa:bb:cc", // te lang
		"02-48-4f-50-aa-bb",    // verkeerde scheiding
		"02:48:4f:50:aa:bg",    // geen hex
		"0248.4f50.aabb",       // Cisco-notatie
	} {
		if m, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepteerde het als %x", s, m)
		}
	}
	// En hoofdletters horen er wél in te mogen.
	if m, ok := Parse("02:48:4F:50:AA:BB"); !ok || m[4] != 0xAA {
		t.Errorf("Parse van hoofdletters faalde: %x ok=%v", m, ok)
	}
}
