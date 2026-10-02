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

/// Draagt dit venster over naar het nieuwe beeld van een kern-flip
/// (`hopos/src/flip.rs`, `HOPOS_FLIP_CFG`), als dat beeld zelf geen tekst
/// draagt: de nieuwe kern gaat over deze heen, en zonder venster zou de
/// node zijn naam, zijn MAC en de config van Hop kwijt zijn. Een bundel
/// met een eigen config houdt die (`CFG=` van image/flip-bundle.sh).
///
/// `image` is het platte beeld van `len` bytes in de staging, net
/// neergelegd door de flip, die er tot de sprong als enige in schrijft. Het
/// venster ligt in `.data`, dus op een andere plek in elke build: het
/// nieuwe venster is de eerste 8-uitgelijnde plek met onze magic (gelezen
/// uit ons eigen venster, zodat de code de magic geen tweede keer in het
/// image zet; het script vindt hem precies één keer). Geeft of er
/// gekopieerd is.
pub fn carry_config(image: u64, len: u64) -> bool {
    carry(&CFG, image, len)
}

/// [`carry_config`] met `ours` als ons venster (de host-tests geven een
/// eigen venster).
fn carry(ours: &Window, image: u64, len: u64) -> bool {
    let n = match usize::try_from(ours.len.load(Relaxed)) {
        Ok(n) if n > 0 && n <= WINDOW - TEXT_OFF => n,
        _ => return false,
    };
    let (m0, m1) = (ours.magic[0].load(Relaxed), ours.magic[1].load(Relaxed));
    let end = image.saturating_add(len).saturating_sub(WINDOW as u64);
    let mut at = image.next_multiple_of(8);
    while at <= end {
        if dev::read64(dev::Pa(at)) == m0 && dev::read64(dev::Pa(at + 8)) == m1 {
            break;
        }
        at += 8;
    }
    if at > end || dev::read64(dev::Pa(at + 16)) != 0 {
        return false;
    }
    // De tekst eerst, de lengte als laatste: een half venster is geen config.
    let words = n.div_ceil(8);
    for (i, w) in ours.text.iter().take(words).enumerate() {
        dev::write64(
            dev::Pa(at + TEXT_OFF as u64 + 8 * i as u64),
            w.load(Relaxed),
        );
    }
    dev::write64(dev::Pa(at + 16), n as u64);
    true
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

    /// Ons venster in de toets: een eigen static (één toets gebruikt hem),
    /// want een venster van 64 KiB hoort niet op de stack.
    static OURS: Window = Window {
        magic: [
            AtomicU64::new(u64::from_le_bytes(*b"HOPOS.CF")),
            AtomicU64::new(u64::from_le_bytes(*b"G.WINDOW")),
        ],
        len: AtomicU64::new(0),
        text: [const { AtomicU64::new(0) }; WORDS - 3],
    };

    /// Zet `text` in [`OURS`], zoals het script het schrijft.
    fn window(text: &[u8]) -> &'static Window {
        OURS.len.store(text.len() as u64, Relaxed);
        for (i, c) in text.chunks(8).enumerate() {
            let mut b = [0u8; 8];
            b[..c.len()].copy_from_slice(c);
            OURS.text[i].store(u64::from_le_bytes(b), Relaxed);
        }
        &OURS
    }

    #[test]
    fn the_window_goes_along_only_into_an_image_without_one() {
        let ours = window(b"hopos.node=lrv\n");
        // Een beeld met een leeg venster op +0x2008, en ervoor rommel.
        let mut img = std::vec![0x5555u64; 3 * WINDOW / 8];
        let at = 0x2008 / 8;
        img[at] = ours.magic[0].load(Relaxed);
        img[at + 1] = ours.magic[1].load(Relaxed);
        img[at + 2] = 0;
        let base = img.as_mut_ptr() as u64;
        let len = (img.len() * 8) as u64;
        assert!(carry(ours, base, len));
        assert_eq!(img[at + 2], 15);
        let text: std::vec::Vec<u8> = img[at + 3..at + 5]
            .iter()
            .flat_map(|w| w.to_le_bytes())
            .collect();
        assert_eq!(&text[..15], b"hopos.node=lrv\n");
        // Nu draagt het beeld een venster: het houdt het zijne.
        assert!(!carry(window(b"x=1\n"), base, len));
        assert_eq!(img[at + 2], 15);
        // Zonder eigen tekst, of zonder venster in het beeld: niets.
        assert!(!carry(window(b""), base, len));
        img[at] = 0;
        img[at + 2] = 0;
        assert!(!carry(window(b"hopos.node=lrv\n"), base, len));
    }
}
