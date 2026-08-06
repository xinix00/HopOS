// Package hid vertaalt USB-HID-BOOT-rapporten naar invoergebeurtenissen.
//
// Het boot-protocol (USB HID 1.11 bijlage B) is een vaste rapportvorm die élk
// toetsenbord en élke muis met bInterfaceSubClass 1 moet spreken: 8 bytes
// respectievelijk 3 bytes, met vaste velden. Het bestaat omdat een BIOS moest
// kunnen typen zonder een report-descriptor te parsen, en wij zitten in
// dezelfde positie — dus er staat hier geen parser, alleen een tabel.
//
// Wat een toetsenbord stuurt is een TOESTAND ("deze zes toetsen zijn nu
// ingedrukt") en geen gebeurtenis. Het verschil tussen twee toestanden is de
// gebeurtenis; dat verschil rekent dit pakket uit. Daarom is het stateful per
// apparaat (Keyboard) en niet één losse functie.
//
// GEEN BUILD-TAG, en dat is met opzet: dit is pure Go zonder MMIO, dus de
// tabel en de toestandslogica draaien in de host-tests. Precies het deel waar
// een fout stil is — een verkeerde regel in de tabel geeft geen crash maar een
// verkeerde letter.
package hid

// Kind is het soort invoergebeurtenis.
type Kind int

const (
	KeyDown Kind = iota
	KeyUp
	MouseMove
	MouseDown
	MouseUp
	MouseWheel
)

// Event is één invoergebeurtenis, in de vocabulaire van de browser-KVM: Code is
// een JavaScript keyCode voor toetsen en een knopnummer (0 = links) voor de
// muis.
//
// Waarom JS-keyCodes en niet HID-usages: de display kent er al één taal, want
// de KVM-pagina in hop-os-surf stuurt precies dit. Een tweede vocabulaire zou
// betekenen dat de display twee soorten invoer moet kennen — en dan is een
// echt toetsenbord iets anders dan een browser, terwijl het dat niet is.
type Event struct {
	Kind   Kind
	Code   int
	DX, DY int // MouseMove: relatieve verplaatsing; MouseWheel: DY = klikken
}

// usageToKeyCode is HID usage page 7 (keyboard/keypad) → JavaScript keyCode.
// Alleen de toetsen die een boot-toetsenbord kan sturen; de rest levert 0 op en
// wordt genegeerd. Nul is geen geldige keyCode, dus dat is meteen de check.
var usageToKeyCode = [232]uint8{
	// 0x04..0x1D: a..z → 'A'..'Z' (JS keyCodes zijn hoofdletterposities)
	0x04: 65, 0x05: 66, 0x06: 67, 0x07: 68, 0x08: 69, 0x09: 70, 0x0A: 71,
	0x0B: 72, 0x0C: 73, 0x0D: 74, 0x0E: 75, 0x0F: 76, 0x10: 77, 0x11: 78,
	0x12: 79, 0x13: 80, 0x14: 81, 0x15: 82, 0x16: 83, 0x17: 84, 0x18: 85,
	0x19: 86, 0x1A: 87, 0x1B: 88, 0x1C: 89, 0x1D: 90,

	// 0x1E..0x27: 1..9 en 0 (op de bovenrij) → 49..57, 48
	0x1E: 49, 0x1F: 50, 0x20: 51, 0x21: 52, 0x22: 53,
	0x23: 54, 0x24: 55, 0x25: 56, 0x26: 57, 0x27: 48,

	0x28: 13, // enter
	0x29: 27, // escape
	0x2A: 8,  // backspace
	0x2B: 9,  // tab
	0x2C: 32, // space

	0x2D: 189, // - _
	0x2E: 187, // = +
	0x2F: 219, // [ {
	0x30: 221, // ] }
	0x31: 220, // \ |
	0x32: 220, // # ~ (non-US hash, dezelfde plek op een ISO-bord)
	0x33: 186, // ; :
	0x34: 222, // ' "
	0x35: 192, // ` ~
	0x36: 188, // , <
	0x37: 190, // . >
	0x38: 191, // / ?
	0x39: 20,  // caps lock

	// 0x3A..0x45: F1..F12 → 112..123
	0x3A: 112, 0x3B: 113, 0x3C: 114, 0x3D: 115, 0x3E: 116, 0x3F: 117,
	0x40: 118, 0x41: 119, 0x42: 120, 0x43: 121, 0x44: 122, 0x45: 123,

	0x46: 44,  // print screen
	0x47: 145, // scroll lock
	0x48: 19,  // pause
	0x49: 45,  // insert
	0x4A: 36,  // home
	0x4B: 33,  // page up
	0x4C: 46,  // delete
	0x4D: 35,  // end
	0x4E: 34,  // page down
	0x4F: 39,  // right
	0x50: 37,  // left
	0x51: 40,  // down
	0x52: 38,  // up

	0x53: 144,                                          // num lock
	0x54: 111,                                          // keypad /
	0x55: 106,                                          // keypad *
	0x56: 109,                                          // keypad -
	0x57: 107,                                          // keypad +
	0x58: 13,                                           // keypad enter
	0x59: 97, 0x5A: 98, 0x5B: 99, 0x5C: 100, 0x5D: 101, // keypad 1..5
	0x5E: 102, 0x5F: 103, 0x60: 104, 0x61: 105, // keypad 6..9
	0x62: 96,  // keypad 0
	0x63: 110, // keypad .
	0x64: 226, // non-US backslash (de extra toets links van Z op een ISO-bord)
	0x65: 93,  // context menu
}

// modKeyCode is de bytepositie in het modifier-veld (byte 0 van het rapport)
// → keyCode. Links en rechts geven dezelfde code, net als een browser doet;
// alleen de Windows/Command-toetsen verschillen (91/92).
var modKeyCode = [8]int{
	17, // links ctrl
	16, // links shift
	18, // links alt
	91, // links GUI
	17, // rechts ctrl
	16, // rechts shift
	18, // rechts alt (AltGr)
	92, // rechts GUI
}

// Keyboard houdt de vorige toetsenbordtoestand vast om er verschillen uit te
// halen.
type Keyboard struct {
	mods uint8
	keys [6]uint8
}

// Decode zet een boot-toetsenbordrapport om in de gebeurtenissen sinds het
// vorige rapport. Kortere rapporten worden genegeerd (0 events) — een apparaat
// dat minder dan 8 bytes stuurt spreekt geen boot-protocol.
//
// De volgorde van de zes usage-bytes is BETEKENISLOOS: een toetsenbord mag ze
// herschikken zolang de verzameling klopt. Daarom vergelijken we verzamelingen
// en geen posities; wie posities vergelijkt krijgt fantoomtoetsen zodra er drie
// vingers tegelijk op liggen.
func (k *Keyboard) Decode(r []byte, out []Event) []Event {
	if len(r) < 8 {
		return out
	}
	mods := r[0]

	// Modifiers: elk bit is één toets.
	for i := 0; i < 8; i++ {
		was, now := k.mods&(1<<uint(i)) != 0, mods&(1<<uint(i)) != 0
		if was == now {
			continue
		}
		kind := KeyUp
		if now {
			kind = KeyDown
		}
		out = append(out, Event{Kind: kind, Code: modKeyCode[i]})
	}

	var keys [6]uint8
	copy(keys[:], r[2:8])

	// Losgelaten: zat in de oude verzameling, niet in de nieuwe.
	for _, u := range k.keys {
		if u == 0 || contains(keys, u) {
			continue
		}
		if c := keyCode(u); c != 0 {
			out = append(out, Event{Kind: KeyUp, Code: c})
		}
	}
	// Ingedrukt: andersom.
	for _, u := range keys {
		if u == 0 || contains(k.keys, u) {
			continue
		}
		// 0x01..0x03 zijn geen toetsen maar foutcodes (rollover/POST-fail): het
		// toetsenbord meldt dat het niet meer kan bijhouden hoeveel er ligt.
		if u <= 3 {
			continue
		}
		if c := keyCode(u); c != 0 {
			out = append(out, Event{Kind: KeyDown, Code: c})
		}
	}

	k.mods, k.keys = mods, keys
	return out
}

// Reset vergeet de toestand. Nodig bij het loskoppelen van een apparaat: anders
// blijft een toets die tijdens het uittrekken "ingedrukt" was voor altijd
// ingedrukt voor de display.
//
// Geeft de losgelaten toetsen terug, zodat de ontvanger geen toets vast houdt
// die er niet meer is.
func (k *Keyboard) Reset(out []Event) []Event {
	out = k.Decode(make([]byte, 8), out)
	*k = Keyboard{}
	return out
}

func contains(set [6]uint8, u uint8) bool {
	for _, v := range set {
		if v == u {
			return true
		}
	}
	return false
}

func keyCode(u uint8) int {
	if int(u) >= len(usageToKeyCode) {
		return 0
	}
	return int(usageToKeyCode[u])
}

// Mouse houdt de vorige knoppentoestand vast.
type Mouse struct {
	buttons uint8
}

// Decode zet een boot-muisrapport om: byte 0 = knoppen, byte 1/2 = relatieve
// verplaatsing als SIGNED bytes, byte 3 = wielklikken (optioneel — het
// boot-protocol kent er drie, maar vrijwel elke muis stuurt er vier).
func (m *Mouse) Decode(r []byte, out []Event) []Event {
	if len(r) < 3 {
		return out
	}
	for i := 0; i < 3; i++ {
		was, now := m.buttons&(1<<uint(i)) != 0, r[0]&(1<<uint(i)) != 0
		if was == now {
			continue
		}
		kind := MouseUp
		if now {
			kind = MouseDown
		}
		// USB-knopvolgorde is links/rechts/midden; de browser telt
		// links/midden/rechts. Zonder deze omzetting plakt een rechtermuisklik
		// op het middelste knopnummer.
		code := 0
		switch i {
		case 1:
			code = 2
		case 2:
			code = 1
		}
		out = append(out, Event{Kind: kind, Code: code})
	}
	m.buttons = r[0] & 0x7

	if dx, dy := int(int8(r[1])), int(int8(r[2])); dx != 0 || dy != 0 {
		out = append(out, Event{Kind: MouseMove, DX: dx, DY: dy})
	}
	if len(r) >= 4 && r[3] != 0 {
		out = append(out, Event{Kind: MouseWheel, DY: int(int8(r[3]))})
	}
	return out
}

// Reset laat alle knoppen los (zie Keyboard.Reset).
func (m *Mouse) Reset(out []Event) []Event {
	out = m.Decode([]byte{0, 0, 0}, out)
	*m = Mouse{}
	return out
}
