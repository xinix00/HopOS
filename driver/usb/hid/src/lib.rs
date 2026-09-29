//! USB-HID-BOOT-rapporten naar invoergebeurtenissen.
//!
//! Het boot-protocol (USB HID 1.11 bijlage B) is een vaste rapportvorm die
//! elk toetsenbord en elke muis met bInterfaceSubClass 1 moet spreken: 8
//! bytes respectievelijk 3 bytes, met vaste velden. Het bestaat omdat een
//! BIOS moest kunnen typen zonder een report-descriptor te parsen, en wij
//! zitten in dezelfde positie: dus er staat hier geen parser, alleen een
//! tabel.
//!
//! Wat een toetsenbord stuurt is een TOESTAND ("deze zes toetsen zijn nu
//! ingedrukt") en geen gebeurtenis. Het verschil tussen twee toestanden is
//! de gebeurtenis; dat verschil rekent deze crate uit. Daarom is het
//! stateful per apparaat ([`Keyboard`], [`Mouse`]) en niet één losse
//! functie.
//!
//! Geen registers, geen `unsafe`: de tabel en de toestandslogica draaien in
//! de host-tests. Precies het deel waar een fout stil is: een verkeerde
//! regel in de tabel geeft geen crash maar een verkeerde letter.
//!
//! De uitvoer gaat in een begrensde buffer van de aanroeper ([`Events`]).
//! Eén rapport levert er hoogstens [`MAX_EVENTS`], en een `reset` van een
//! toetsenbord plus een muis ook; wie de buffer per rapport leegt, verliest
//! dus nooit een gebeurtenis.

#![cfg_attr(not(test), no_std)]
#![forbid(unsafe_code)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

use bounded::BoundedVec;

/// Het soort invoergebeurtenis.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Default)]
pub enum Kind {
    /// Een toets gaat omlaag.
    #[default]
    KeyDown,
    /// Een toets komt los.
    KeyUp,
    /// De muis beweegt: [`Event::dx`] en [`Event::dy`] zijn relatief.
    MouseMove,
    /// Een muisknop gaat omlaag.
    MouseDown,
    /// Een muisknop komt los.
    MouseUp,
    /// Het wiel draait: [`Event::dy`] is het aantal klikken.
    MouseWheel,
}

/// Eén invoergebeurtenis, in de vocabulaire van de browser-KVM: `code` is
/// een JavaScript-keyCode voor toetsen en een knopnummer (0 = links) voor
/// de muis.
///
/// Waarom JS-keyCodes en niet HID-usages: de display kent er al één taal,
/// want de KVM-pagina in hop-os-surf stuurt precies dit. Een tweede
/// vocabulaire zou betekenen dat de display twee soorten invoer moet kennen,
/// en dan is een echt toetsenbord iets anders dan een browser, terwijl het
/// dat niet is.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Default)]
pub struct Event {
    /// Wat er gebeurde.
    pub kind: Kind,
    /// De keyCode of het knopnummer.
    pub code: i32,
    /// Relatieve verplaatsing in x ([`Kind::MouseMove`]).
    pub dx: i32,
    /// Relatieve verplaatsing in y, of wielklikken ([`Kind::MouseWheel`]).
    pub dy: i32,
}

impl Event {
    const fn key(kind: Kind, code: i32) -> Self {
        Self {
            kind,
            code,
            dx: 0,
            dy: 0,
        }
    }
}

/// Het meeste dat één decode oplevert: acht modifiers plus zes toetsen los
/// en zes toetsen omlaag.
pub const MAX_EVENTS: usize = 8 + 6 + 6;

/// De uitvoerbuffer van een decode.
pub type Events = BoundedVec<Event, MAX_EVENTS>;

/// Het meeste dat één muisrapport oplevert: drie knoppen, een beweging, het
/// wiel.
const MAX_MOUSE_EVENTS: usize = 5;

// Een muisrapport past, en een reset van toetsenbord plus muis (8 + 6 + 3)
// ook, in één buffer.
const _: () = assert!(MAX_MOUSE_EVENTS <= MAX_EVENTS && 8 + 6 + 3 <= MAX_EVENTS);

/// Zet `e` in `out`. Vol kan alleen als de aanroeper de buffer tussen twee
/// rapporten niet leegde; dan valt de gebeurtenis weg, zoals invoer hier
/// overal "lossy by design" is.
fn put(out: &mut Events, e: Event) {
    let _ = out.push(e);
}

/// HID usage page 7 (keyboard/keypad) naar JavaScript-keyCode. Alleen de
/// toetsen die een boot-toetsenbord kan sturen; de rest levert 0 op en wordt
/// genegeerd. Nul is geen geldige keyCode, dus dat is meteen de check.
static USAGE_TO_KEY_CODE: [u8; 232] = {
    let mut t = [0u8; 232];
    // 0x04..0x1D: a..z naar 'A'..'Z' (JS-keyCodes zijn hoofdletterposities).
    let mut i = 0;
    while i < 26 {
        t[0x04 + i] = 65 + i as u8;
        i += 1;
    }
    // 0x1E..0x26: 1..9 op de bovenrij naar 49..57; 0x27 is de 0.
    let mut i = 0;
    while i < 9 {
        t[0x1E + i] = 49 + i as u8;
        i += 1;
    }
    t[0x27] = 48;

    t[0x28] = 13; // enter
    t[0x29] = 27; // escape
    t[0x2A] = 8; // backspace
    t[0x2B] = 9; // tab
    t[0x2C] = 32; // space

    t[0x2D] = 189; // - _
    t[0x2E] = 187; // = +
    t[0x2F] = 219; // [ {
    t[0x30] = 221; // ] }
    t[0x31] = 220; // \ |
    t[0x32] = 220; // # ~ (non-US hash, dezelfde plek op een ISO-bord)
    t[0x33] = 186; // ; :
    t[0x34] = 222; // ' "
    t[0x35] = 192; // ` ~
    t[0x36] = 188; // , <
    t[0x37] = 190; // . >
    t[0x38] = 191; // / ?
    t[0x39] = 20; // caps lock

    // 0x3A..0x45: F1..F12 naar 112..123.
    let mut i = 0;
    while i < 12 {
        t[0x3A + i] = 112 + i as u8;
        i += 1;
    }

    t[0x46] = 44; // print screen
    t[0x47] = 145; // scroll lock
    t[0x48] = 19; // pause
    t[0x49] = 45; // insert
    t[0x4A] = 36; // home
    t[0x4B] = 33; // page up
    t[0x4C] = 46; // delete
    t[0x4D] = 35; // end
    t[0x4E] = 34; // page down
    t[0x4F] = 39; // right
    t[0x50] = 37; // left
    t[0x51] = 40; // down
    t[0x52] = 38; // up

    t[0x53] = 144; // num lock
    t[0x54] = 111; // keypad /
    t[0x55] = 106; // keypad *
    t[0x56] = 109; // keypad -
    t[0x57] = 107; // keypad +
    t[0x58] = 13; // keypad enter
    // 0x59..0x61: keypad 1..9 naar 97..105.
    let mut i = 0;
    while i < 9 {
        t[0x59 + i] = 97 + i as u8;
        i += 1;
    }
    t[0x62] = 96; // keypad 0
    t[0x63] = 110; // keypad .
    t[0x64] = 226; // non-US backslash (de extra toets links van Z op een ISO-bord)
    t[0x65] = 93; // context menu
    t
};

/// De bitpositie in het modifier-veld (byte 0 van het rapport) naar
/// keyCode. Links en rechts geven dezelfde code, net als een browser doet;
/// alleen de Windows/Command-toetsen verschillen (91/92).
const MOD_KEY_CODE: [i32; 8] = [
    17, // links ctrl
    16, // links shift
    18, // links alt
    91, // links GUI
    17, // rechts ctrl
    16, // rechts shift
    18, // rechts alt (AltGr)
    92, // rechts GUI
];

/// De keyCode van usage `u`, of 0 als het geen toets is die wij kennen.
#[must_use]
pub fn key_code(u: u8) -> i32 {
    USAGE_TO_KEY_CODE
        .get(usize::from(u))
        .map_or(0, |&c| i32::from(c))
}

/// Houdt de vorige toetsenbordtoestand vast om er verschillen uit te halen.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Keyboard {
    mods: u8,
    keys: [u8; 6],
}

impl Keyboard {
    /// Een toetsenbord waarop niets ligt.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            mods: 0,
            keys: [0; 6],
        }
    }

    /// Zet een boot-toetsenbordrapport om in de gebeurtenissen sinds het
    /// vorige rapport. Kortere rapporten worden genegeerd: een apparaat dat
    /// minder dan 8 bytes stuurt spreekt geen boot-protocol.
    ///
    /// De volgorde van de zes usage-bytes is BETEKENISLOOS: een toetsenbord
    /// mag ze herschikken zolang de verzameling klopt. Daarom vergelijken we
    /// verzamelingen en geen posities; wie posities vergelijkt krijgt
    /// fantoomtoetsen zodra er drie vingers tegelijk op liggen.
    pub fn decode(&mut self, r: &[u8], out: &mut Events) {
        let Some(&[mods, _, k0, k1, k2, k3, k4, k5]) = r.get(..8).and_then(|s| s.first_chunk())
        else {
            return;
        };

        // Modifiers: elk bit is één toets.
        for (i, &code) in MOD_KEY_CODE.iter().enumerate() {
            let was = self.mods & (1 << i) != 0;
            let now = mods & (1 << i) != 0;
            if was != now {
                let kind = if now { Kind::KeyDown } else { Kind::KeyUp };
                put(out, Event::key(kind, code));
            }
        }

        let keys = [k0, k1, k2, k3, k4, k5];
        // Een foutrapport (0x01..0x03: rollover of POST-fail) zegt niet dat
        // de toetsen die lagen losgelaten zijn: het toetsenbord meldt alleen
        // dat het niet meer kan bijhouden hoeveel er ligt.
        if keys.iter().any(|&u| (1..=3).contains(&u)) {
            self.mods = mods;
            return;
        }

        // Losgelaten: zat in de oude verzameling, niet in de nieuwe.
        for &u in &self.keys {
            if u != 0 && !keys.contains(&u) {
                let c = key_code(u);
                if c != 0 {
                    put(out, Event::key(Kind::KeyUp, c));
                }
            }
        }
        // Ingedrukt: andersom. Foutcodes (<= 3) zijn hierboven al weg.
        for &u in &keys {
            if u > 3 && !self.keys.contains(&u) {
                let c = key_code(u);
                if c != 0 {
                    put(out, Event::key(Kind::KeyDown, c));
                }
            }
        }

        self.mods = mods;
        self.keys = keys;
    }

    /// Vergeet de toestand en laat alles los wat lag. Nodig bij het
    /// loskoppelen van een apparaat: anders blijft een toets die tijdens het
    /// uittrekken "ingedrukt" was voor altijd ingedrukt voor de display.
    pub fn reset(&mut self, out: &mut Events) {
        self.decode(&[0; 8], out);
        *self = Self::new();
    }
}

/// Houdt de vorige knoppentoestand vast.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Mouse {
    buttons: u8,
}

impl Mouse {
    /// Een muis zonder ingedrukte knoppen.
    #[must_use]
    pub const fn new() -> Self {
        Self { buttons: 0 }
    }

    /// Zet een boot-muisrapport om: byte 0 = knoppen, byte 1/2 = relatieve
    /// verplaatsing als SIGNED bytes, byte 3 = wielklikken (optioneel: het
    /// boot-protocol kent er drie, maar vrijwel elke muis stuurt er vier).
    pub fn decode(&mut self, r: &[u8], out: &mut Events) {
        let Some(&[buttons, x, y]) = r.first_chunk() else {
            return;
        };
        for i in 0..3u8 {
            let was = self.buttons & (1 << i) != 0;
            let now = buttons & (1 << i) != 0;
            if was == now {
                continue;
            }
            let kind = if now { Kind::MouseDown } else { Kind::MouseUp };
            // USB-knopvolgorde is links/rechts/midden; de browser telt
            // links/midden/rechts. Zonder deze omzetting plakt een
            // rechtermuisklik op het middelste knopnummer.
            let code = match i {
                1 => 2,
                2 => 1,
                _ => 0,
            };
            put(out, Event::key(kind, code));
        }
        self.buttons = buttons & 0x7;

        let dx = i32::from(x as i8);
        let dy = i32::from(y as i8);
        if dx != 0 || dy != 0 {
            put(
                out,
                Event {
                    kind: Kind::MouseMove,
                    code: 0,
                    dx,
                    dy,
                },
            );
        }
        if let Some(&w) = r.get(3)
            && w != 0
        {
            put(
                out,
                Event {
                    kind: Kind::MouseWheel,
                    code: 0,
                    dx: 0,
                    dy: i32::from(w as i8),
                },
            );
        }
    }

    /// Laat alle knoppen los (zie [`Keyboard::reset`]).
    pub fn reset(&mut self, out: &mut Events) {
        self.decode(&[0; 3], out);
        *self = Self::new();
    }
}

#[cfg(test)]
mod tests;
