//! De Go-tests van `gui/driver/usb/hid`, één op één.

use super::*;

fn dec_kb(k: &mut Keyboard, r: &[u8]) -> Events {
    let mut out = Events::new();
    k.decode(r, &mut out);
    out
}

fn dec_ms(m: &mut Mouse, r: &[u8]) -> Events {
    let mut out = Events::new();
    m.decode(r, &mut out);
    out
}

// Go: TestKeyboardPressRelease
#[test]
fn keyboard_press_release() {
    let mut k = Keyboard::new();
    // 'a' ingedrukt.
    let got = dec_kb(&mut k, &[0, 0, 0x04, 0, 0, 0, 0, 0]);
    assert_eq!(got.as_slice(), &[Event::key(Kind::KeyDown, 65)]);
    // Zelfde rapport nog eens: een toetsenbord herhaalt zijn toestand, dat
    // is geen nieuwe aanslag.
    assert!(dec_kb(&mut k, &[0, 0, 0x04, 0, 0, 0, 0, 0]).is_empty());
    // Losgelaten.
    let got = dec_kb(&mut k, &[0; 8]);
    assert_eq!(got.as_slice(), &[Event::key(Kind::KeyUp, 65)]);
}

// Go: TestKeyboardReordering
//
// De zes usage-bytes mogen door het toetsenbord herschikt worden. Wie
// posities vergelijkt in plaats van verzamelingen ziet hier een loslaten plus
// indrukken die niet gebeurd zijn.
#[test]
fn keyboard_reordering() {
    let mut k = Keyboard::new();
    dec_kb(&mut k, &[0, 0, 0x04, 0x05, 0x06, 0, 0, 0]);
    assert!(dec_kb(&mut k, &[0, 0, 0x06, 0x04, 0x05, 0, 0, 0]).is_empty());
}

// Go: TestKeyboardModifiers
#[test]
fn keyboard_modifiers() {
    let mut k = Keyboard::new();
    // Linker shift ingedrukt, daarna 'a' erbij.
    let got = dec_kb(&mut k, &[0x02, 0, 0, 0, 0, 0, 0, 0]);
    assert_eq!(got.as_slice(), &[Event::key(Kind::KeyDown, 16)]);
    let got = dec_kb(&mut k, &[0x02, 0, 0x04, 0, 0, 0, 0, 0]);
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].code, 65);
    // Allebei los in één rapport: twee events.
    assert_eq!(dec_kb(&mut k, &[0; 8]).len(), 2);
}

// Go: TestKeyboardRolloverIsNoKey
//
// Rollover (0x01) is een foutcode en geen toets: het toetsenbord meldt dat er
// meer vingers liggen dan het kan rapporteren.
#[test]
fn keyboard_rollover_is_no_key() {
    let mut k = Keyboard::new();
    assert!(dec_kb(&mut k, &[0, 0, 1, 1, 1, 1, 1, 1]).is_empty());
}

// Go: TestKeyboardResetReleases
//
// Een apparaat dat wordt losgetrokken terwijl er een toets ligt, mag die
// toets niet voor altijd ingedrukt laten bij de display.
#[test]
fn keyboard_reset_releases() {
    let mut k = Keyboard::new();
    dec_kb(&mut k, &[0x01, 0, 0x04, 0, 0, 0, 0, 0]);
    let mut got = Events::new();
    k.reset(&mut got);
    assert_eq!(got.len(), 2);
    assert!(got.iter().all(|e| e.kind == Kind::KeyUp));
    assert!(dec_kb(&mut k, &[0; 8]).is_empty());
}

// Go: TestKeyboardShortReportIgnored
#[test]
fn keyboard_short_report_ignored() {
    let mut k = Keyboard::new();
    assert!(dec_kb(&mut k, &[0, 0, 0x04]).is_empty());
}

// Go: TestMouseButtonOrder
//
// USB telt links/rechts/midden, de browser links/midden/rechts.
#[test]
fn mouse_button_order() {
    let mut m = Mouse::new();
    let got = dec_ms(&mut m, &[0x02, 0, 0]); // rechterknop
    assert_eq!(got.as_slice(), &[Event::key(Kind::MouseDown, 2)]);
    let mut m = Mouse::new();
    let got = dec_ms(&mut m, &[0x04, 0, 0]); // middelste knop
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].code, 1);
}

// Go: TestMouseNegativeMove
#[test]
fn mouse_negative_move() {
    let mut m = Mouse::new();
    let got = dec_ms(&mut m, &[0, 0xFF, 0xFE, 0]);
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].kind, Kind::MouseMove);
    assert_eq!((got[0].dx, got[0].dy), (-1, -2));
}

// Go: TestMouseWheelOptional
#[test]
fn mouse_wheel_optional() {
    let mut m = Mouse::new();
    assert!(dec_ms(&mut m, &[0, 0, 0]).is_empty());
    let got = dec_ms(&mut m, &[0, 0, 0, 0xFF]);
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].kind, Kind::MouseWheel);
    assert_eq!(got[0].dy, -1);
}

// Go: TestKeyCodeTableAnchors
//
// De tabel is het deel dat stil fout kan zijn: een verkeerde regel geeft
// geen crash maar een verkeerde letter. Dit vangt de klassieke verschuiving
// met één.
#[test]
fn key_code_table_anchors() {
    for (usage, want, what) in [
        (0x04, 65, "a"),
        (0x1D, 90, "z"),
        (0x1E, 49, "1"),
        (0x26, 57, "9"),
        (0x27, 48, "0"),
        (0x28, 13, "enter"),
        (0x2C, 32, "spatie"),
        (0x3A, 112, "F1"),
        (0x45, 123, "F12"),
        (0x4F, 39, "rechts"),
        (0x52, 38, "omhoog"),
        (0x59, 97, "keypad 1"),
        (0x61, 105, "keypad 9"),
        (0x00, 0, "geen toets"),
        (0xE0, 0, "modifier hoort niet in de tabel"),
        (0xFF, 0, "buiten de tabel"),
    ] {
        assert_eq!(key_code(usage), want, "usage {usage:#04x} ({what})");
    }
}

// Go: TestRolloverPreservesHeldKeys
#[test]
fn rollover_preserves_held_keys() {
    let mut k = Keyboard::new();
    let key = [0, 0, 4, 0, 0, 0, 0, 0];
    dec_kb(&mut k, &key);
    assert!(dec_kb(&mut k, &[0, 0, 1, 1, 1, 1, 1, 1]).is_empty());
    assert!(dec_kb(&mut k, &key).is_empty());
    let got = dec_kb(&mut k, &[0; 8]);
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].kind, Kind::KeyUp);
}

// De grens van de uitvoerbuffer: het ergste rapport past precies.
#[test]
fn worst_case_report_fits() {
    let mut k = Keyboard::new();
    let got = dec_kb(&mut k, &[0xFF, 0, 4, 5, 6, 7, 8, 9]);
    assert_eq!(got.len(), 8 + 6);
    let got = dec_kb(&mut k, &[0x00, 0, 10, 11, 12, 13, 14, 15]);
    assert_eq!(got.len(), MAX_EVENTS);
}
