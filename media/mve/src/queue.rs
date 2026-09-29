//! De ringen tussen host en firmware (Go: `queue.go`).
//!
//! Vier ringparen in gedeeld geheugen: één voor berichten en drie voor
//! buffers. Elk paar is twee pagina's: één die alleen de host beschrijft en
//! één die alleen de firmware beschrijft. Geen woord met twee schrijvers,
//! dus geen slot tussen ons en het ijzer: dezelfde vorm als onze eigen
//! net-ringen (handboek §1, de grens ijzer-software).
//!
//! ```text
//! host-pagina: out_rpos u16 | in_wpos u16 | reserved[3] u32 | in_data[1020]
//! mve-pagina:  out_wpos u16 | in_rpos u16 | reserved[3] u32 | out_data[1020]
//! ```
//!
//! Posities tellen in WOORDEN en lopen rond op 1020; een bericht mag over
//! die grens heen breken.

use crate::proto::{BUF_GENERAL, RESP_SWITCHED_IN};
use dev::Pa;
use driver_codec::{Error, Result};

pub(crate) const Q_WORDS: u32 = 1020;
pub(crate) const Q_DATA_OFF: u64 = 16;
pub(crate) const Q_OUT_RPOS: u64 = 0;
pub(crate) const Q_IN_WPOS: u64 = 2;
pub(crate) const Q_OUT_WPOS: u64 = 0;
pub(crate) const Q_IN_RPOS: u64 = 2;
/// Drie woorden; de firmware zet er zijn lopende checksum in.
pub(crate) const Q_RESERVED: u64 = 4;

/// Eén ringpaar: `host` en `mve` zijn de fysieke adressen van de twee
/// pagina's; `sum` is de lopende checksum van alles wat wij stuurden.
///
/// Die checksum is geen luxe: een firmware met `-sum` in zijn versie
/// verwacht achter élke berichtkop een woord met de lopende som. Een driver
/// die dat weglaat schuift de stroom één woord op; de firmware leest dan zijn
/// eigen berichtlengte als data en de sessie is stuk voor hij begint.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub(crate) struct Ring {
    pub(crate) host: u64,
    pub(crate) mve: u64,
    pub(crate) sum: u32,
    pub(crate) csum: bool,
}

fn r16(pa: u64) -> u32 {
    u32::from(dev::read16(Pa(pa)))
}

impl Ring {
    /// Zet één bericht in de ring naar de firmware.
    pub(crate) fn send(&mut self, code: u16, data: &[u8]) -> Result {
        let words = data.len().div_ceil(4) as u32;
        let need = 1 + words + u32::from(self.csum);
        if need > Q_WORDS - 1 {
            return Err(Error::MsgTooBig {
                len: data.len() as u32,
            });
        }
        let mut wpos = r16(self.host + Q_IN_WPOS);
        let rpos = r16(self.mve + Q_IN_RPOS);
        if wpos >= Q_WORDS {
            return Err(Error::QueuePos { pos: wpos });
        }
        if rpos >= Q_WORDS {
            return Err(Error::QueuePos { pos: rpos });
        }
        let mut free = rpos as i32 - wpos as i32;
        if free <= 0 {
            free += Q_WORDS as i32;
        }
        // Eén woord blijft altijd vrij: zonder die marge is een precies volle
        // ring niet van een lege te onderscheiden (beide wpos == rpos), en
        // leest de firmware 1020 woorden oude berichten opnieuw. De
        // Linux-driver rekent zonder marge; wij betalen liever één woord.
        if free - 1 - (need as i32) < 0 {
            return Err(Error::QueueFull);
        }
        let hdr = u32::from(code) | ((data.len() as u32) << 16);
        if self.csum {
            self.sum = self.sum.wrapping_add(hdr).wrapping_add(sum_words(data));
        }
        wpos = self.put(wpos, hdr);
        if self.csum {
            wpos = self.put(wpos, self.sum);
        }
        for i in 0..words as usize {
            wpos = self.put(wpos, word_at(data, i * 4));
        }
        // Alle data staat in het geheugen vóór de firmware de nieuwe
        // schrijfpositie ziet; anders leest hij een kop die naar bytes wijst
        // die er nog niet zijn.
        dev::mb();
        dev::write16(Pa(self.host + Q_IN_WPOS), wpos as u16);
        if self.csum {
            // De firmware vergelijkt zijn som met de laatste drie die wij
            // achterlieten (hij mag één bericht achterlopen).
            let r = self.host + Q_RESERVED;
            dev::write32(Pa(r), dev::read32(Pa(r + 4)));
            dev::write32(Pa(r + 4), dev::read32(Pa(r + 8)));
            dev::write32(Pa(r + 8), self.sum);
        }
        dev::mb();
        Ok(())
    }

    /// Haalt één bericht op in `dst`: `Some((code, n))`, of `None` als er
    /// niets (heel) te lezen is. Een bericht dat niet in `dst` past is een
    /// protocolfout en geen gedeeltelijke lees: een halve descriptor is erger
    /// dan geen. Hetzelfde voor een positie buiten de ring: beide pagina's
    /// zijn beschrijfbaar voor de firmware, en een woord achter de 1020 ligt
    /// al op de volgende fysieke pagina.
    pub(crate) fn recv(&mut self, dst: &mut [u8]) -> Result<Option<(u16, usize)>> {
        let rpos = r16(self.host + Q_OUT_RPOS);
        let wpos = r16(self.mve + Q_OUT_WPOS);
        if rpos >= Q_WORDS {
            return Err(Error::QueuePos { pos: rpos });
        }
        if wpos >= Q_WORDS {
            return Err(Error::QueuePos { pos: wpos });
        }
        if rpos == wpos {
            return Ok(None);
        }
        let mut avail = wpos as i32 - rpos as i32;
        if avail < 0 {
            avail += Q_WORDS as i32;
        }
        let hdr = self.get(rpos);
        let code = hdr as u16;
        let size = (hdr >> 16) as usize;
        let words = size.div_ceil(4);
        if !(RESP_SWITCHED_IN..=BUF_GENERAL).contains(&code) {
            return Err(Error::MsgUnknown { code });
        }
        if (avail as usize) < 1 + words {
            return Ok(None); // De firmware is nog aan het schrijven.
        }
        if size > dst.len() {
            return Err(Error::MsgTooBig { len: size as u32 });
        }
        let mut pos = (rpos + 1) % Q_WORDS;
        for i in 0..words {
            let w = self.get(pos).to_le_bytes();
            pos = (pos + 1) % Q_WORDS;
            let end = (i * 4 + 4).min(size);
            if let (Some(d), Some(s)) = (dst.get_mut(i * 4..end), w.get(..end - i * 4)) {
                d.copy_from_slice(s);
            }
        }
        dev::mb();
        dev::write16(Pa(self.host + Q_OUT_RPOS), pos as u16);
        Ok(Some((code, size)))
    }

    fn put(&self, pos: u32, v: u32) -> u32 {
        dev::write32(Pa(self.host + Q_DATA_OFF + u64::from(pos) * 4), v);
        (pos + 1) % Q_WORDS
    }

    fn get(&self, pos: u32) -> u32 {
        dev::read32(Pa(self.mve + Q_DATA_OFF + u64::from(pos) * 4))
    }
}

/// Telt de woorden van een bericht op zoals de firmware: de staart met
/// nullen aangevuld tot een heel woord.
pub(crate) fn sum_words(data: &[u8]) -> u32 {
    (0..data.len())
        .step_by(4)
        .fold(0u32, |s, i| s.wrapping_add(word_at(data, i)))
}

/// Een 32-bit woord uit `data` op `off`, met nullen aangevuld.
pub(crate) fn word_at(data: &[u8], off: usize) -> u32 {
    let mut b = [0u8; 4];
    let tail = data.get(off..).unwrap_or(&[]);
    let n = tail.len().min(4);
    b[..n].copy_from_slice(&tail[..n]);
    u32::from_le_bytes(b)
}
