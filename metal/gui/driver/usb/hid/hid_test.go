package hid

import "testing"

func codes(evs []Event) []Event { return evs }

func TestKeyboardPressRelease(t *testing.T) {
	var k Keyboard
	// 'a' ingedrukt.
	got := k.Decode([]byte{0, 0, 0x04, 0, 0, 0, 0, 0}, nil)
	if len(got) != 1 || got[0].Kind != KeyDown || got[0].Code != 65 {
		t.Fatalf("indrukken: %+v", got)
	}
	// Zelfde rapport nog eens: een toetsenbord herhaalt zijn toestand, dat is
	// geen nieuwe aanslag.
	if got := k.Decode([]byte{0, 0, 0x04, 0, 0, 0, 0, 0}, nil); len(got) != 0 {
		t.Fatalf("herhaald rapport gaf events: %+v", got)
	}
	// Losgelaten.
	got = k.Decode(make([]byte, 8), nil)
	if len(got) != 1 || got[0].Kind != KeyUp || got[0].Code != 65 {
		t.Fatalf("loslaten: %+v", got)
	}
}

// De zes usage-bytes mogen door het toetsenbord herschikt worden. Wie posities
// vergelijkt in plaats van verzamelingen ziet hier een loslaten + indrukken die
// niet gebeurd zijn.
func TestKeyboardReordering(t *testing.T) {
	var k Keyboard
	k.Decode([]byte{0, 0, 0x04, 0x05, 0x06, 0, 0, 0}, nil)
	if got := k.Decode([]byte{0, 0, 0x06, 0x04, 0x05, 0, 0, 0}, nil); len(got) != 0 {
		t.Fatalf("herschikking gaf events: %+v", got)
	}
}

func TestKeyboardModifiers(t *testing.T) {
	var k Keyboard
	// Linker shift ingedrukt, daarna 'a' erbij.
	got := k.Decode([]byte{0x02, 0, 0, 0, 0, 0, 0, 0}, nil)
	if len(got) != 1 || got[0].Kind != KeyDown || got[0].Code != 16 {
		t.Fatalf("shift: %+v", got)
	}
	got = k.Decode([]byte{0x02, 0, 0x04, 0, 0, 0, 0, 0}, nil)
	if len(got) != 1 || got[0].Code != 65 {
		t.Fatalf("shift+a: %+v", got)
	}
	// Allebei los in één rapport: twee events.
	got = k.Decode(make([]byte, 8), nil)
	if len(got) != 2 {
		t.Fatalf("beide los: %+v", got)
	}
}

// Rollover (0x01) is een foutcode en geen toets: het toetsenbord meldt dat er
// meer vingers liggen dan het kan rapporteren.
func TestKeyboardRolloverIsNoKey(t *testing.T) {
	var k Keyboard
	if got := k.Decode([]byte{0, 0, 1, 1, 1, 1, 1, 1}, nil); len(got) != 0 {
		t.Fatalf("rollover gaf events: %+v", got)
	}
}

// Een apparaat dat wordt losgetrokken terwijl er een toets ligt, mag die toets
// niet voor altijd ingedrukt laten bij de display.
func TestKeyboardResetReleases(t *testing.T) {
	var k Keyboard
	k.Decode([]byte{0x01, 0, 0x04, 0, 0, 0, 0, 0}, nil)
	got := k.Reset(nil)
	if len(got) != 2 {
		t.Fatalf("reset: %+v", got)
	}
	for _, e := range got {
		if e.Kind != KeyUp {
			t.Fatalf("reset gaf %v", e.Kind)
		}
	}
	if got := k.Decode(make([]byte, 8), nil); len(got) != 0 {
		t.Fatalf("na reset nog toestand: %+v", got)
	}
}

func TestKeyboardShortReportIgnored(t *testing.T) {
	var k Keyboard
	if got := k.Decode([]byte{0, 0, 0x04}, nil); len(got) != 0 {
		t.Fatalf("kort rapport gaf events: %+v", got)
	}
}

// USB telt links/rechts/midden, de browser links/midden/rechts.
func TestMouseButtonOrder(t *testing.T) {
	var m Mouse
	got := m.Decode([]byte{0x02, 0, 0}, nil) // rechterknop
	if len(got) != 1 || got[0].Kind != MouseDown || got[0].Code != 2 {
		t.Fatalf("rechts: %+v", got)
	}
	m = Mouse{}
	got = m.Decode([]byte{0x04, 0, 0}, nil) // middelste knop
	if len(got) != 1 || got[0].Code != 1 {
		t.Fatalf("midden: %+v", got)
	}
}

func TestMouseNegativeMove(t *testing.T) {
	var m Mouse
	got := codes(m.Decode([]byte{0, 0xFF, 0xFE, 0}, nil))
	if len(got) != 1 || got[0].Kind != MouseMove || got[0].DX != -1 || got[0].DY != -2 {
		t.Fatalf("beweging: %+v", got)
	}
}

func TestMouseWheelOptional(t *testing.T) {
	var m Mouse
	if got := m.Decode([]byte{0, 0, 0}, nil); len(got) != 0 {
		t.Fatalf("stil 3-byte rapport gaf events: %+v", got)
	}
	got := m.Decode([]byte{0, 0, 0, 0xFF}, nil)
	if len(got) != 1 || got[0].Kind != MouseWheel || got[0].DY != -1 {
		t.Fatalf("wiel: %+v", got)
	}
}

// De tabel is het deel dat stil fout kan zijn: een verkeerde regel geeft geen
// crash maar een verkeerde letter. Dit vangt de klassieke verschuiving met één.
func TestKeyCodeTableAnchors(t *testing.T) {
	for _, c := range []struct {
		usage uint8
		want  int
		what  string
	}{
		{0x04, 65, "a"}, {0x1D, 90, "z"},
		{0x1E, 49, "1"}, {0x26, 57, "9"}, {0x27, 48, "0"},
		{0x28, 13, "enter"}, {0x2C, 32, "spatie"},
		{0x3A, 112, "F1"}, {0x45, 123, "F12"},
		{0x4F, 39, "rechts"}, {0x52, 38, "omhoog"},
		{0x00, 0, "geen toets"}, {0xE0, 0, "modifier hoort niet in de tabel"},
	} {
		if got := keyCode(c.usage); got != c.want {
			t.Errorf("usage %#02x (%s) = %d, wil %d", c.usage, c.what, got, c.want)
		}
	}
}
