//! Een GUID-partitietabel lezen: genoeg om één vraag te beantwoorden:
//! welk stuk van deze schijf is niet van iemand anders?
//!
//! Op elk HopOS-board tot de Mac mini was de schijf van ons alleen en was
//! die vraag flauw. Op een machine die we DELEN met het besturingssysteem
//! van de eigenaar (macOS op dezelfde SSD, plus Recovery) is het de enige
//! vraag die telt. Het schrijfvenster van de ANS-NVMe
//! (`driver_nvme::apple`) komt daarom hiervandaan: uit wat de schijf zélf
//! zegt, niet uit een getal dat op één machine klopte.
//!
//! Bewust minimaal: de header en de entries, verder niets. Geen schrijven,
//! geen reparatie, geen backup-header. Wie de tabel wil veranderen, doet dat
//! met het gereedschap van zijn eigen OS (`diskutil apfs resizeContainer`).
//! De lezer kent geen schijf: de aanroeper geeft een leesfunctie per blok.

use crate::bytes::{le16, le32, le64};
use bounded::BoundedVec;
use core::fmt;

/// Zoveel partities houden we bij. De M4 heeft er drie; een tabel met meer
/// gevulde entries dan dit is een [`Error::TooMany`], geen stille afkapping.
pub const MAX_PARTS: usize = 16;
/// Zoveel entries lopen we hoogstens af (de standaard is 128).
pub const MAX_ENTRIES: u32 = 1024;
/// De maat van de naam in een entry: 36 UTF-16-tekens.
pub const NAME_LEN: usize = 36;

/// Waarom de tabel niet te lezen is.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// Het blok op `lba` kwam niet.
    Read {
        /// De LBA.
        lba: u64,
    },
    /// Geen "EFI PART" op LBA 1: ongepartitioneerd, of een andere blokmaat.
    NoSignature,
    /// De entrymaat past niet in een blok.
    EntrySize {
        /// De gemelde entrymaat.
        size: u32,
        /// De blokmaat.
        block: usize,
    },
    /// Een entry eindigt vóór hij begint.
    Backwards {
        /// De index van de entry.
        entry: u32,
        /// Eerste LBA.
        first: u64,
        /// Laatste LBA.
        last: u64,
    },
    /// Meer gevulde entries dan [`MAX_PARTS`], of meer entries dan
    /// [`MAX_ENTRIES`].
    TooMany(u32),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Read { lba } => write!(f, "gpt: block {lba} could not be read"),
            Self::NoSignature => {
                f.write_str("gpt: no signature on LBA 1 (unpartitioned, or a different block size)")
            }
            Self::EntrySize { size, block } => {
                write!(
                    f,
                    "gpt: entry size {size} does not fit a {block}-byte block"
                )
            }
            Self::Backwards { entry, first, last } => {
                write!(
                    f,
                    "gpt: entry {entry} ends ({last}) before it starts ({first})"
                )
            }
            Self::TooMany(n) => write!(f, "gpt: {n} entries, more than we keep"),
        }
    }
}

/// De `Result` van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén partitie: eerste en laatste LBA (inclusief) en zijn naam voor de
/// mens die het logboek leest.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Part {
    /// De eerste LBA.
    pub first: u64,
    /// De laatste LBA, inclusief.
    pub last: u64,
    name: [u8; NAME_LEN],
    name_len: usize,
}

impl Part {
    /// De naam als ASCII; wat daarbuiten valt, is een `?` (dit gaat naar
    /// een console die niet meer kan).
    #[must_use]
    pub fn name(&self) -> &str {
        let b = self.name.get(..self.name_len).unwrap_or_default();
        core::str::from_utf8(b).unwrap_or("?")
    }

    /// Het aantal blokken.
    #[must_use]
    pub fn blocks(&self) -> u64 {
        self.last - self.first + 1
    }
}

/// Wat we van de tabel nodig hebben.
#[derive(Clone, Debug, Default)]
pub struct Table {
    /// De eerste bruikbare LBA.
    pub first_usable: u64,
    /// De laatste bruikbare LBA, inclusief.
    pub last_usable: u64,
    /// De gevulde entries, in tabelvolgorde.
    pub parts: BoundedVec<Part, MAX_PARTS>,
}

/// Leest de tabel van LBA 1. `block` is een buffer van precies één blok;
/// `read(lba, block)` vult hem, `false` = het blok kwam niet.
pub fn read(mut read: impl FnMut(u64, &mut [u8]) -> bool, block: &mut [u8]) -> Result<Table> {
    let bs = block.len();
    if !read(1, block) {
        return Err(Error::Read { lba: 1 });
    }
    if block.get(..8) != Some(b"EFI PART".as_slice()) {
        return Err(Error::NoSignature);
    }
    let (Some(first_usable), Some(last_usable), Some(entry_lba), Some(count), Some(size)) = (
        le64(block, 40),
        le64(block, 48),
        le64(block, 72),
        le32(block, 80),
        le32(block, 84),
    ) else {
        // Een blok dat de header niet draagt, draagt ook geen entry.
        return Err(Error::EntrySize { size: 0, block: bs });
    };
    let mut t = Table {
        first_usable,
        last_usable,
        parts: BoundedVec::new(),
    };
    if size < 128 || size as usize > bs {
        return Err(Error::EntrySize { size, block: bs });
    }
    if count > MAX_ENTRIES {
        return Err(Error::TooMany(count));
    }
    let size = size as usize;
    let per_block = bs / size;
    for i in 0..count {
        let i_us = i as usize;
        if i_us.is_multiple_of(per_block) {
            let lba = entry_lba.saturating_add((i_us / per_block) as u64);
            if !read(lba, block) {
                return Err(Error::Read { lba });
            }
        }
        let off = (i_us % per_block) * size;
        let e = block.get(off..off + size).ok_or(Error::EntrySize {
            size: size as u32,
            block: bs,
        })?;
        if e.get(..16).is_some_and(|g| g.iter().all(|&b| b == 0)) {
            continue; // Een lege sleuf: geen partitie.
        }
        // Een entry is minstens 128 bytes (getoetst), dus deze twee staan erin.
        let (first, last) = (le64(e, 32).unwrap_or(0), le64(e, 40).unwrap_or(0));
        if last < first {
            return Err(Error::Backwards {
                entry: i,
                first,
                last,
            });
        }
        let mut p = Part {
            first,
            last,
            name: [0; NAME_LEN],
            name_len: 0,
        };
        for k in 0..NAME_LEN {
            let c = le16(e, 56 + 2 * k).unwrap_or(0);
            if c == 0 {
                break;
            }
            p.name[k] = if (0x20..=0x7e).contains(&c) {
                c as u8
            } else {
                b'?'
            };
            p.name_len = k + 1;
        }
        t.parts.push(p).map_err(|_| Error::TooMany(i + 1))?;
    }
    Ok(t)
}

/// Het grootste aaneengesloten stuk bruikbare schijf zonder partitie erop:
/// eerste LBA en aantal LBA's; aantal 0 = er is niets vrij.
///
/// Gaten TUSSEN partities tellen mee, en dat is geen detail: wie zijn macOS
/// krimpt, krijgt zijn ruimte precies daar, tussen de container en de
/// Recovery-partitie die erachter blijft staan (gemeten 30-08 op de M4).
#[must_use]
pub fn largest_gap(t: &Table) -> (u64, u64) {
    let (mut first, mut count) = (0, 0);
    let mut pos = t.first_usable;
    while pos <= t.last_usable {
        // Staan we ín een partitie: erlangs.
        if let Some(p) = t.parts.iter().find(|p| p.first <= pos && pos <= p.last) {
            match p.last.checked_add(1) {
                Some(next) => pos = next,
                None => break,
            }
            continue;
        }
        // Anders: tot de dichtstbijzijnde partitie vóór ons, of het einde.
        let end = t
            .parts
            .iter()
            .filter(|p| p.first > pos)
            .map(|p| p.first)
            .min()
            .unwrap_or(t.last_usable.saturating_add(1))
            .min(t.last_usable.saturating_add(1));
        if end - pos > count {
            (first, count) = (pos, end - pos);
        }
        pos = end;
    }
    (first, count)
}

#[cfg(test)]
mod tests {
    use super::*;

    const BS: usize = 4096;

    fn header(first: u64, last: u64) -> Vec<u8> {
        let mut h = vec![0u8; BS];
        h[..8].copy_from_slice(b"EFI PART");
        h[40..48].copy_from_slice(&first.to_le_bytes());
        h[48..56].copy_from_slice(&last.to_le_bytes());
        h[72..80].copy_from_slice(&2u64.to_le_bytes());
        h[80..84].copy_from_slice(&128u32.to_le_bytes());
        h[84..88].copy_from_slice(&128u32.to_le_bytes());
        h
    }

    fn entry(e: &mut [u8], i: usize, first: u64, last: u64, name: &str) {
        let e = &mut e[i * 128..];
        e[0] = 1; // Een niet-lege type-GUID.
        e[32..40].copy_from_slice(&first.to_le_bytes());
        e[40..48].copy_from_slice(&last.to_le_bytes());
        for (j, c) in name.encode_utf16().enumerate() {
            e[56 + 2 * j..58 + 2 * j].copy_from_slice(&c.to_le_bytes());
        }
    }

    fn disk(hdr: Vec<u8>, entries: Vec<u8>) -> impl FnMut(u64, &mut [u8]) -> bool {
        move |lba, p| {
            match lba {
                1 => p.copy_from_slice(&hdr),
                2 => p.copy_from_slice(&entries),
                _ => p.fill(0),
            }
            true
        }
    }

    /// De echte tabel van de Mac mini M4 zoals de node hem 30-08 uitlas:
    /// drie partities met het vrijgemaakte gat ertussen.
    fn m4() -> impl FnMut(u64, &mut [u8]) -> bool {
        let mut e = vec![0u8; BS];
        entry(&mut e, 0, 6, 128_005, "iBootSystemContainer");
        entry(&mut e, 1, 128_006, 19_659_255, "");
        entry(&mut e, 2, 120_827_419, 122_138_127, "RecoveryOSContainer");
        disk(header(6, 122_138_127), e)
    }

    #[test]
    fn the_gap_between_partitions_is_found() {
        let mut buf = vec![0u8; BS];
        let t = read(m4(), &mut buf).unwrap();
        assert_eq!(t.parts.len(), 3);
        assert_eq!(t.parts[0].name(), "iBootSystemContainer");
        assert_eq!(t.parts[2].name(), "RecoveryOSContainer");
        let (first, count) = largest_gap(&t);
        // Het gat ligt TUSSEN de gekrompen macOS-container en RecoveryOS,
        // niet achteraan. Een zoeker die alleen achter de laatste partitie
        // kijkt, vindt hier nul, en dat was precies de val.
        assert_eq!(first, 19_659_256);
        assert_eq!(count, 120_827_419 - 19_659_256);
        let gb = count * 4096 / 1_000_000_000;
        assert!((400..=420).contains(&gb), "gat van {gb} GB, verwacht ~414");
    }

    #[test]
    fn a_full_disk_has_no_gap() {
        let mut e = vec![0u8; BS];
        entry(&mut e, 0, 6, 1000, "all");
        let mut buf = vec![0u8; BS];
        let t = read(disk(header(6, 1000), e), &mut buf).unwrap();
        assert_eq!(largest_gap(&t).1, 0);
    }

    #[test]
    fn no_gpt_is_an_error_not_a_guess() {
        let mut buf = vec![0u8; BS];
        assert_eq!(
            read(
                |_, p: &mut [u8]| {
                    p.fill(0);
                    true
                },
                &mut buf
            )
            .unwrap_err(),
            Error::NoSignature
        );
        assert_eq!(
            read(|_, _: &mut [u8]| false, &mut buf).unwrap_err(),
            Error::Read { lba: 1 }
        );
    }

    #[test]
    fn crooked_entries_are_refused() {
        let mut e = vec![0u8; BS];
        entry(&mut e, 0, 50, 10, "back");
        let mut buf = vec![0u8; BS];
        assert!(matches!(
            read(disk(header(6, 1000), e), &mut buf),
            Err(Error::Backwards { entry: 0, .. })
        ));
        let mut h = header(6, 1000);
        h[84..88].copy_from_slice(&8192u32.to_le_bytes());
        assert!(matches!(
            read(disk(h, vec![0u8; BS]), &mut buf),
            Err(Error::EntrySize { size: 8192, .. })
        ));
        // Meer gevulde entries dan we bijhouden: luid, niet afgekapt.
        let mut e = vec![0u8; BS];
        for i in 0..=MAX_PARTS {
            entry(&mut e, i, 10 + 2 * i as u64, 11 + 2 * i as u64, "p");
        }
        assert!(matches!(
            read(disk(header(6, 1000), e), &mut buf),
            Err(Error::TooMany(_))
        ));
    }

    #[test]
    fn a_disk_without_partitions_is_one_gap() {
        let mut buf = vec![0u8; BS];
        let t = read(disk(header(6, 1000), vec![0u8; BS]), &mut buf).unwrap();
        assert_eq!(largest_gap(&t), (6, 995));
    }
}
