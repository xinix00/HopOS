//! De console over TCP (`kern::conport`; Go: `kern/conport` en
//! `driver/conlog`): een ring van 64 KiB achter de UART-sink, waar
//! `nc node 5555` eerst de bewaarde console uit krijgt en daarna live
//! meeleest. Op een headless board (de Pi 4, de Radxa, de O6N zonder
//! UART aan de Mac) is dit de enige plek waar de kern zijn redenering
//! kwijt kan; Derek noemde hem op 30-09 "killer" toen hij ontbrak.
//!
//! Dit bezit de ring, de tee en de lezersplaatsen; de listener en de
//! lezertaken staan in net.rs (de node-stack), en de aan/uit-keuze komt uit
//! de config (kern::nodecfg: `hopos.console`, anders `hopos.insecure`).
//!
//! Eigendom: de ring is van de OS-core (`LocalCell`, zoals het glas in
//! gui.rs). De tee schrijft alleen op die core; een regel van een andere
//! core gaat naar de UART en niet in de ring. Een lening die al loopt (een
//! noodregel uit exception-context midden in een regel) slaat de ring
//! over, de UART krijgt alles. De lezers lenen de ring per hap
//! (`snapshot`), nooit over een await heen.
//!
//! De tee geeft elke regel ook aan de zwarte doos (`flip::black_box`): een
//! ring op een vaste plek buiten het kernimage die een watchdog-reset
//! overleeft, want deze ring in de BSS doet dat niet. Die schrijft vanaf elke
//! core; de volgende koude boot drukt de staart af (`HOPOS_FLIP_BLACKBOX`).

use core::sync::atomic::{
    AtomicBool, AtomicPtr, AtomicUsize,
    Ordering::{Acquire, Relaxed, Release},
};
use kern::conport::{Readers, Ring};
use sync::LocalCell;

/// De ring: 256 KiB. Een boot is 10 KB tot `HOP_UP` (de Pi 5, 30-09),
/// maar de tik is 300 bytes per seconde, dus 64 KiB was na drie minuten
/// alleen nog tikken en de boot was weg (de Pi 4 op 5555, 30-09). Een
/// kwart megabyte houdt de boot een kwartier vast; .bss, geen heap.
const RING_BYTES: usize = 256 * 1024;

/// De ring zelf; alleen de OS-core raakt hem aan.
static RING: LocalCell<Ring<RING_BYTES>> = LocalCell::cell(Ring::new());
/// De core die de ring bezit (`usize::MAX` = nog geen).
static RING_CORE: AtomicUsize = AtomicUsize::new(usize::MAX);
/// De sink vóór ons (de UART van het board), zoals `kmain` hem zette.
/// Null = nog geen tee.
static PREV: AtomicPtr<()> = AtomicPtr::new(core::ptr::null_mut());
/// Staat de TCP-poort aan (de keuze van kern::nodecfg)?
static ENABLED: AtomicBool = AtomicBool::new(false);
/// De lezersplaatsen: hoogstens `kern::conport::MAX_READERS` tegelijk.
pub(crate) static READERS: Readers = Readers::new();

/// Hangt de ring achter `prev` en zet de tee als console-sink; de core van
/// nu bezit de ring tot [`here`] hem overdraagt.
pub(crate) fn install(prev: fn(&[u8])) {
    PREV.store(prev as *mut (), Release);
    RING_CORE.store(crate::BOARD.this_core(), Release);
    cpu::console::set_sink(tee);
}

/// De OS-core neemt de ring over (na een verhuizing van de kern, vóór het
/// glas): vanaf nu schrijft alleen deze core erin.
pub(crate) fn here() {
    RING_CORE.store(crate::BOARD.this_core(), Release);
}

/// De sink: eerst de zwarte doos, dan de vorige (de UART), dan de ring op
/// de eigen core. De tee van het glas (gui.rs) roept dit als zijn "UART".
///
/// De doos eerst, zoals in Go (`conlog.Route`): hangt de UART-poll, dan
/// staat de regel toch al in DRAM.
pub(crate) fn tee(b: &[u8]) {
    crate::flip::black_box(b);
    let p = PREV.load(Acquire);
    if !p.is_null() {
        // SAFETY: `PREV` wordt alleen door `install` geschreven, met een
        // geldige `fn(&[u8])`; een functiepointer en een datapointer zijn op
        // onze targets even groot, en null is uitgesloten (zelfde vorm als
        // `cpu::console::sink` en de tee in gui.rs).
        let prev = unsafe { core::mem::transmute::<*mut (), fn(&[u8])>(p) };
        prev(b);
    }
    if RING_CORE.load(Acquire) != crate::BOARD.this_core() {
        return;
    }
    if let Ok(mut r) = RING.try_borrow_mut() {
        r.write(b);
    }
}

/// Een hap uit de ring vanaf `seen`, voor `kern::conport::stream`. Een
/// ring die net beschreven wordt geeft niets; de volgende poll wel.
pub(crate) fn snapshot(seen: u64, out: &mut [u8]) -> (usize, u64) {
    match RING.try_borrow() {
        Ok(r) => r.since(seen, out),
        Err(_) => (0, seen),
    }
}

/// Waar een nieuwe lezer begint: het oudste dat nog in de ring staat, niet
/// nul, zodat wie na de boot verbindt de hele geschiedenis krijgt die er is.
pub(crate) fn oldest() -> u64 {
    RING.try_borrow().map_or(0, |r| r.oldest())
}

/// Herhaalt het begin van deze boot op de vorige sink (de UART of de
/// dockchannel), hoogstens `max` bytes, zonder de ring opnieuw te vullen.
/// Voor een board waarvan de lezer pas na de boot aanhaakt (de M4, 30-09:
/// de kis-poort verschijnt pas 30 s na een herstart, en de bootregels zijn
/// dan al weg). Alleen op de core die de ring bezit; geeft het aantal
/// bytes.
pub(crate) fn replay(max: usize) -> usize {
    let p = PREV.load(Acquire);
    if p.is_null() {
        return 0;
    }
    // SAFETY: als in `tee`: `PREV` komt alleen uit `install`, met een
    // geldige `fn(&[u8])`, en null is hierboven uitgesloten.
    let prev = unsafe { core::mem::transmute::<*mut (), fn(&[u8])>(p) };
    let mut seen = oldest();
    let mut total = 0;
    let mut buf = [0u8; 512];
    while total < max {
        let want = (max - total).min(buf.len());
        let (n, next) = snapshot(seen, &mut buf[..want]);
        if n == 0 {
            break;
        }
        prev(&buf[..n]);
        seen = next;
        total += n;
    }
    total
}

/// De keuze van de config: aan of uit. Vóór de listener (net.rs) gezet.
pub(crate) fn enable(on: bool) {
    ENABLED.store(on, Relaxed);
}

/// Staat de poort aan?
pub(crate) fn enabled() -> bool {
    ENABLED.load(Relaxed)
}
