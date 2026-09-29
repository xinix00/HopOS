//! `hopos.cfg` op de LicheeRV: een venster in het kern-image dat
//! `image/licheerv-agent.sh` vult (`CFG=pad`), zoals het `embedcfg`-venster
//! van de Go-generatie (`OLD/image/licheerv-agent.sh`, `image/hopcfg`).
//!
//! Waarom in het image: de FSBL geeft geen DTB en geen bootargs, en de kern
//! heeft (nog) geen SD-driver om een bestand naast `fip.bin` te lezen. Het
//! image is het enige dat de FSBL voor ons in het DRAM legt. Het script zoekt
//! de magic in `monitor.bin`, schrijft de lengte en de tekst erachter, en
//! bouwt dan pas de FIP: geen checksum om bij te werken, want `fiptool`
//! rekent over het gepatchte beeld.
//!
//! De indeling, 64 KiB (het getal van Go):
//!
//! ```text
//! +0   16 bytes  "HOPOS.CFG.WINDOW" (alleen het script leest hem)
//! +16  u64 LE    de lengte van de tekst (0 = geen config)
//! +24  ...       de tekst: hopos.cfg, key=value per regel (`fw::bootcfg::all`)
//! ```
//!
//! Een venster zonder tekst is een node met de defaults: het ingebouwde
//! MAC-adres (luid, `HOPOS_MAC_FIXED`) en geen naam.

use core::sync::atomic::{AtomicU64, Ordering::Relaxed};

/// De maat van het venster.
pub const WINDOW: usize = 64 << 10;
/// Waar de tekst begint.
pub const TEXT_OFF: usize = 24;
/// Woorden in het venster.
const WORDS: usize = WINDOW / 8;

/// Het venster, in de indeling van de kop.
#[repr(C)]
struct Window {
    magic: [AtomicU64; 2],
    len: AtomicU64,
    text: [AtomicU64; WORDS - 3],
}

const _: () = assert!(core::mem::size_of::<Window>() == WINDOW);
const _: () = assert!(core::mem::offset_of!(Window, text) == TEXT_OFF);

/// Het venster. `AtomicU64`'s en geen `[u8; N]`: een onveranderlijke static
/// mag de compiler vouwen tot zijn beginwaarde (een lege config), en de
/// waarde verandert hier buiten de compiler om (het script patcht het
/// bestand). In `.data`, zodat het in het platte beeld staat dat de FSBL
/// laadt. De magic staat alleen in deze beginwaarde: de code vergelijkt hem
/// nooit, dus het script vindt hem precies één keer.
#[used]
#[cfg_attr(target_os = "none", unsafe(link_section = ".data.hopcfg"))]
static CFG: Window = Window {
    magic: [
        AtomicU64::new(u64::from_le_bytes(*b"HOPOS.CF")),
        AtomicU64::new(u64::from_le_bytes(*b"G.WINDOW")),
    ],
    len: AtomicU64::new(0),
    text: [const { AtomicU64::new(0) }; WORDS - 3],
};

/// De lengte zoals het script hem schreef, of `None` als hij niet in het
/// venster past (een kapot beeld: dan geen config, niet een halve).
#[must_use]
pub fn len() -> Option<usize> {
    let n = usize::try_from(CFG.len.load(Relaxed)).ok()?;
    (n <= WINDOW - TEXT_OFF).then_some(n)
}

/// De tekst van `hopos.cfg`, of "" (geen config, een te lange lengte, of
/// geen UTF-8).
#[must_use]
pub fn text() -> &'static str {
    let Some(n) = len() else {
        return "";
    };
    // SAFETY: `CFG.text` is een static van `WINDOW - TEXT_OFF` bytes die
    // leeft zolang het programma, en `n` past erin (net getoetst). Niemand schrijft
    // het venster na de boot (het script patchte het bestand, de kern leest
    // alleen), dus een gewone lees door de atomics heen racet met niets.
    let bytes = unsafe { core::slice::from_raw_parts(CFG.text.as_ptr().cast::<u8>(), n) };
    core::str::from_utf8(bytes).unwrap_or("")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_unpatched_window_is_no_config() {
        assert_eq!(len(), Some(0));
        assert_eq!(text(), "");
        let head: [u8; 8] = CFG.magic[0].load(Relaxed).to_le_bytes();
        assert_eq!(&head, b"HOPOS.CF");
        assert_eq!(core::mem::size_of_val(&CFG), WINDOW);
    }
}
