//! De firmware-binary (Go: `fwbin.go`).
//!
//! Elke codec is een eigen binary die wij in de VPU laden: hevcdec,
//! h264dec, av1dec, mpeg2dec, vc1dec, elk zo'n 300 KB. De bitstream-kennis
//! zit in de firmware van Arm/CIX, niet bij ons: wij zetten bytes op de
//! juiste virtuele adressen en praten daarna een berichtenprotocol.
//!
//! Het formaat is een kop van 180 bytes gevolgd door de code. De kop zegt
//! waar de code heen moet, welke bss-pagina's er moeten komen, en welke
//! versie van het host-protocol deze firmware spreekt (de Linlon V8 van de
//! O6N levert major 3).

use crate::arena::Arena;
use crate::mmu::{ACCESS_EXEC, ACCESS_RW, Mmu, PAGE, PAGE_SHIFT};
use driver_codec::{Error, Result};

/// De lengte van de kop.
pub(crate) const HEADER_LEN: usize = 180;
/// Waar de code komt: de nulpagina blijft expres onbemapt.
pub(crate) const TEXT_BASE: u32 = 0x1000;
/// Het magische jump-woord (onderste 16 bits).
pub(crate) const MAGIC: u32 = 0x0000_eb5e;

/// De kop van een `.fwb`-bestand.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub(crate) struct Header {
    pub(crate) rasc_jmp: u32,
    pub(crate) protocol_minor: u8,
    pub(crate) protocol_major: u8,
    pub(crate) part_number: [u8; 8],
    pub(crate) version: [u8; 16],
    pub(crate) text_length: u32,
    pub(crate) bss_start: u32,
    pub(crate) bss_bitmap_size: u32,
    pub(crate) bss_bitmap: [u32; 16],
    pub(crate) master_rw_start: u32,
    pub(crate) master_rw_size: u32,
}

fn le32(b: &[u8], i: usize) -> u32 {
    let mut w = [0u8; 4];
    if let Some(s) = b.get(i..i + 4) {
        w.copy_from_slice(s);
    }
    u32::from_le_bytes(w)
}

/// Leest de kop en toetst hem tegen de werkelijke lengte. Alles wat niet
/// klopt is een fout en geen aanname: een verkeerd geladen firmware hangt de
/// VPU op een manier die alleen een reset oplost, en dat kost op ijzer een
/// hele cyclus.
pub(crate) fn parse(bin: &[u8]) -> Result<Header> {
    if bin.len() < HEADER_LEN {
        return Err(Error::BadFirmware {
            at: 0,
            got: bin.len() as u32,
        });
    }
    let mut h = Header {
        rasc_jmp: le32(bin, 0),
        protocol_minor: bin[4],
        protocol_major: bin[5],
        text_length: le32(bin, 96),
        bss_start: le32(bin, 100),
        bss_bitmap_size: le32(bin, 104),
        master_rw_start: le32(bin, 172),
        master_rw_size: le32(bin, 176),
        ..Header::default()
    };
    h.part_number.copy_from_slice(&bin[64..72]);
    h.version.copy_from_slice(&bin[80..96]);
    for (i, w) in h.bss_bitmap.iter_mut().enumerate() {
        *w = le32(bin, 108 + 4 * i);
    }
    let bad = |at, got| Err(Error::BadFirmware { at, got });
    if h.rasc_jmp & 0xffff != MAGIC & 0xffff {
        return bad(0, h.rasc_jmp);
    }
    if h.text_length as usize > bin.len() {
        return bad(96, h.text_length);
    }
    if h.bss_bitmap_size > 16 * 32 {
        return bad(104, h.bss_bitmap_size);
    }
    if u64::from(h.bss_start) & (PAGE - 1) != 0 {
        return bad(100, h.bss_start);
    }
    if !(2..=3).contains(&h.protocol_major) {
        return bad(5, u32::from(h.protocol_major));
    }
    Ok(h)
}

impl Header {
    /// Het aantal pagina's van het read-only deel.
    pub(crate) fn text_pages(&self) -> u32 {
        self.text_length.div_ceil(PAGE as u32)
    }

    /// Moet pagina `i` van het bss-gebied er komen? De bitmap is dun: een
    /// firmware reserveert veel meer adresruimte dan hij aanraakt.
    pub(crate) fn bss_page(&self, i: u32) -> bool {
        i < self.bss_bitmap_size
            && self
                .bss_bitmap
                .get((i / 32) as usize)
                .is_some_and(|w| w & (1 << (i % 32)) != 0)
    }

    /// Ligt `va` in het master_rw-venster (gedeeld tussen de cores van één
    /// sessie)? Met één core per sessie is dat boekhouding; het telt zodra
    /// een sessie over cores gespreid wordt voor 8K.
    pub(crate) fn shared(&self, va: u32) -> bool {
        self.master_rw_size != 0
            && va >= self.master_rw_start
            && u64::from(va) < u64::from(self.master_rw_start) + u64::from(self.master_rw_size)
    }

    /// Het deelnummer als tekst (`"56648002"`).
    #[cfg(test)]
    pub(crate) fn part(&self) -> &[u8] {
        cstr(&self.part_number)
    }

    /// Draagt de versie `-sum`? Dan wil de firmware het checksum-woord.
    /// Gemeten: alle zestien O6N-blobs zijn `r0p0-sum0005`.
    pub(crate) fn has_sum(&self) -> bool {
        cstr(&self.version).windows(4).any(|w| w == b"-sum")
    }
}

/// De tekst tot de eerste nul.
pub(crate) fn cstr(b: &[u8]) -> &[u8] {
    let n = b.iter().position(|&c| c == 0).unwrap_or(b.len());
    b.get(..n).unwrap_or(b)
}

/// Schrijft een firmware in de arena en hangt hem in de tabel van een
/// sessie: de code op 0x1000 (uitvoerbaar) en de bss-pagina's uit de bitmap
/// (lezen/schrijven, gewist). Geeft het fysieke adres van de code.
///
/// De bss komt als één aaneengesloten blok uit de arena en wordt pagina
/// voor pagina op de dunne adressen gemapt: zo houdt de tabel één stuk vast
/// in plaats van honderden.
///
/// Eén core per sessie. Voor 8K komt de firmware nog eens op instance 1..7
/// met eigen bss; voor 4K Blu-ray is één core ruim.
pub(crate) fn load(m: &mut Mmu, a: &mut Arena, bin: &[u8], h: &Header) -> Result<u64> {
    let text = h.text_pages();
    let pa = m.alloc(a, TEXT_BASE, text, ACCESS_EXEC)?;
    dev::copy_in(
        dev::Pa(pa),
        bin.get(..h.text_length as usize).unwrap_or(&[]),
    );

    let wanted = |i: u32| {
        let va = h.bss_start.wrapping_add(i << PAGE_SHIFT);
        h.bss_page(i) || h.shared(va)
    };
    let n = (0..h.bss_bitmap_size).filter(|&i| wanted(i)).count() as u32;
    if n > 0 {
        let bss = m.alloc_unmapped(a, n)?;
        for (k, i) in (0..h.bss_bitmap_size).filter(|&i| wanted(i)).enumerate() {
            let va = h.bss_start.wrapping_add(i << PAGE_SHIFT);
            m.map_page(a, va, bss + ((k as u64) << PAGE_SHIFT), ACCESS_RW)?;
        }
    }
    Ok(pa)
}
