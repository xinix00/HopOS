//! De verplaatsende helft van de RISC-V-kooi: een Sv39-tabel die het
//! linkadres van een slot op zijn echte partitie legt.
//!
//! De Rust-vorm van `kern/cage/relocate.go` (`Relocate`). Een kooi doet twee
//! dingen, begrenzen en verplaatsen; op ARM doet één stage-2-tabel beide, hier
//! zijn het twee mechanismen: de PMP-whitelist ([`super::pmp`]) begrenst,
//! deze tabel verplaatst. Waarvoor: één artifact per architectuur. Elk slot
//! ziet zichzelf op hetzelfde linkadres, en de tabel vertaalt dat naar de
//! partitie die het slot werkelijk kreeg.
//!
//! Wat verplaatsen NIET is: de invariant. Een app in S-mode mag zijn eigen
//! `satp` schrijven en zijn adresruimte hertekenen; dat is veilig omdat de
//! hardware-walker zelf aan de whitelist onderworpen is. Gevolg: de tabel
//! mág in de partitie staan (in de ABI-staart), en moet dat zelfs, want een
//! tabel buiten de kooi laat de walk faulten.
//!
//! Blokken van 2 MB: een partitie is tientallen MB's, dus fijner mappen kost
//! alleen tabel. Een wortel plus één pagina per gemapte gigabyte.

use core::fmt;

/// Een pagina.
pub const PAGE: u64 = 4096;
/// Een blok van niveau 1 (megapage): 2 MB.
pub const BLOCK: u64 = 2 << 20;
/// Entries per tabel: 512 × 8 bytes = één pagina.
pub const ENTRIES: usize = 512;
/// Het maximum aantal tabelpagina's: de wortel plus drie gigabytes. Genoeg
/// voor een partitie van 1 GB die een GB-grens kruist plus een grant.
pub const MAX_TABLES: usize = 4;

const V: u64 = 1 << 0;
const R: u64 = 1 << 1;
const W: u64 = 1 << 2;
const X: u64 = 1 << 3;
// De U-bit (1 << 4) staat hier niet: een app draait in S-mode, niet in
// U-mode. De G-bit (1 << 5) evenmin, en dat is geen detail: G belooft dat de
// mapping in ELKE adresruimte hetzelfde is, en elk slot mapt juist hetzelfde
// linkadres naar een andere partitie.
const A: u64 = 1 << 6;
const D: u64 = 1 << 7;
/// T-Head-uitbreiding (MAEE): bufferable. Bestaat niet in de spec.
const THEAD_BUF: u64 = 1 << 61;
/// T-Head-uitbreiding (MAEE): cacheable.
const THEAD_CACHE: u64 = 1 << 62;

/// De modus-waarde bovenin `satp`: Sv39.
pub const SATP_SV39: u64 = 8 << 60;
/// Het positieve canonieke deel van Sv39: een linkadres daarboven is een
/// bug in het PA-plan van een board.
pub const LIMIT: u64 = 1 << 38;

/// Welke PTE-attributen de CPU wil voor normaal RAM.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Attrs {
    /// De spec: bits 63..54 zijn nul (QEMU; een gezet bit zonder Svpbmt is
    /// een page fault).
    Spec,
    /// De C906 met MAEE: RAM krijgt bufferable en cacheable (bit 61/62),
    /// MMIO niet. GEMETEN 31-07: zonder die twee sloeg de app om in "Store/AMO
    /// access fault" (mcause 7) op zijn eigen stack, bij de eerste atomic;
    /// een pagina zonder de bits is device-achtig en weigert atomics.
    Thead,
}

/// Eén verplaatsing: waar de app het ziet (`link`) naar waar het staat
/// (`phys`).
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct MapWindow {
    /// Het linkadres.
    pub link: u64,
    /// Het fysieke adres.
    pub phys: u64,
    /// De maat.
    pub size: u64,
    /// Lezen.
    pub r: bool,
    /// Schrijven.
    pub w: bool,
    /// Uitvoeren.
    pub x: bool,
    /// MMIO of de gedeelde ABI-staart: geen cache-attributen.
    pub device: bool,
}

/// De tabelpagina's zoals ze op de tabelbasis moeten komen: de wortel eerst,
/// dan de niveaus in aanmaakvolgorde.
pub struct Tables {
    /// De pagina's.
    pub pages: [[u64; ENTRIES]; MAX_TABLES],
    /// Hoeveel er gebruikt zijn.
    pub used: usize,
    giga: [u64; MAX_TABLES],
}

impl Default for Tables {
    fn default() -> Self {
        Self::new()
    }
}

impl Tables {
    /// Lege tabellen.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            pages: [[0; ENTRIES]; MAX_TABLES],
            used: 0,
            giga: [u64::MAX; MAX_TABLES],
        }
    }

    /// De bytes die gebruikt zijn.
    #[must_use]
    pub const fn len(&self) -> u64 {
        self.used as u64 * PAGE
    }

    /// Zijn er geen tabellen?
    #[must_use]
    pub const fn is_empty(&self) -> bool {
        self.used == 0
    }
}

/// Waarom een map-plan niet te bouwen is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// De tabelbasis ligt niet op een pagina.
    Base {
        /// De basis.
        base: u64,
    },
    /// Geen vensters: een slot zonder mapping kan niet draaien.
    NoWindows,
    /// Een venster zonder maat of zonder rechten, of W zonder R (een
    /// gereserveerde codering).
    Window {
        /// Het venster.
        index: usize,
    },
    /// Een venster buiten de 2 MB-korrel.
    Align {
        /// Het venster.
        index: usize,
    },
    /// Een linkadres buiten Sv39.
    Range {
        /// Het adres.
        link: u64,
    },
    /// Twee vensters op hetzelfde linkadres.
    Overlap {
        /// Het adres.
        link: u64,
    },
    /// Meer gigabytes dan [`MAX_TABLES`] - 1.
    Full,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Base { base } => write!(f, "cage: table base {base:#x} not page aligned"),
            Self::NoWindows => {
                f.write_str("cage: empty map plan, a slot without a mapping cannot run")
            }
            Self::Window { index } => write!(
                f,
                "cage: map window {index} has zero length, no permissions, or W without R"
            ),
            Self::Align { index } => write!(f, "cage: map window {index} not 2MB aligned"),
            Self::Range { link } => {
                write!(
                    f,
                    "cage: link address {link:#x} outside the Sv39 address space"
                )
            }
            Self::Overlap { link } => {
                write!(
                    f,
                    "cage: link address {link:#x} already mapped, overlapping windows"
                )
            }
            Self::Full => write!(f, "cage: map spans more than {} gigabytes", MAX_TABLES - 1),
        }
    }
}

fn leaf_flags(w: &MapWindow, attrs: Attrs) -> u64 {
    let mut f = V | A | D;
    if w.r {
        f |= R;
    }
    if w.w {
        f |= W;
    }
    if w.x {
        f |= X;
    }
    if !w.device && attrs == Attrs::Thead {
        f |= THEAD_BUF | THEAD_CACHE;
    }
    f
}

/// Bouwt de tabellen voor `windows` met de wortel op `base`, en geeft de
/// `satp`-waarde. Alle validatie zit hier, op de host getest; de switcher
/// schrijft alleen `satp` weg.
///
/// A en D staan vooraf: de C906 fault liever dan dat hij ze zelf bijwerkt,
/// en HopOS heeft geen pagina-vervanging waarvoor ze zouden dienen.
pub fn relocate(
    base: u64,
    windows: &[MapWindow],
    attrs: Attrs,
    out: &mut Tables,
) -> Result<u64, Error> {
    if !base.is_multiple_of(PAGE) {
        return Err(Error::Base { base });
    }
    if windows.is_empty() {
        return Err(Error::NoWindows);
    }
    *out = Tables::new();
    out.used = 1;
    for (i, w) in windows.iter().enumerate() {
        let no_perm = !(w.r || w.w || w.x);
        let w_without_r = w.w && !w.r;
        if w.size == 0 || no_perm || w_without_r {
            return Err(Error::Window { index: i });
        }
        if !w.link.is_multiple_of(BLOCK)
            || !w.phys.is_multiple_of(BLOCK)
            || !w.size.is_multiple_of(BLOCK)
        {
            return Err(Error::Align { index: i });
        }
        let flags = leaf_flags(w, attrs);
        let mut off = 0;
        while off < w.size {
            let link = w.link.saturating_add(off);
            if link >= LIMIT {
                return Err(Error::Range { link });
            }
            let gi = link >> 30;
            let bi = ((link >> 21) & (ENTRIES as u64 - 1)) as usize;
            let page = table_for(out, base, gi)?;
            let slot = out
                .pages
                .get_mut(page)
                .and_then(|p| p.get_mut(bi))
                .ok_or(Error::Full)?;
            if *slot != 0 {
                return Err(Error::Overlap { link });
            }
            *slot = ((w.phys + off) >> 12) << 10 | flags;
            off += BLOCK;
        }
    }
    Ok(SATP_SV39 | (base >> 12))
}

/// De pagina van gigabyte `gi`, aangemaakt als hij er nog niet is. Een
/// verwijzende entry heeft ALLEEN V: R/W/X zou hem tot blad maken.
fn table_for(out: &mut Tables, base: u64, gi: u64) -> Result<usize, Error> {
    if let Some(p) = out.giga.iter().position(|&g| g == gi) {
        return Ok(p + 1);
    }
    let page = out.used;
    if page >= MAX_TABLES {
        return Err(Error::Full);
    }
    out.used += 1;
    if let Some(g) = out.giga.get_mut(page - 1) {
        *g = gi;
    }
    let next = base + page as u64 * PAGE;
    let root = out
        .pages
        .first_mut()
        .and_then(|r| r.get_mut(gi as usize))
        .ok_or(Error::Full)?;
    *root = (next >> 12) << 10 | V;
    Ok(page)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Waar `link` volgens de tabellen naartoe gaat, met de bladvlaggen: een
    /// walk in software.
    fn walk(base: u64, t: &Tables, link: u64) -> Option<(u64, u64)> {
        let root = *t.pages.first()?.get(((link >> 30) & 511) as usize)?;
        if root & V == 0 || root & (R | W | X) != 0 {
            return None;
        }
        let page = (((root >> 10) << 12) - base) / PAGE;
        let leaf = *t
            .pages
            .get(page as usize)?
            .get(((link >> 21) & 511) as usize)?;
        if leaf & V == 0 {
            return None;
        }
        let phys = ((leaf >> 10) & ((1 << 44) - 1)) << 12;
        Some((
            phys + (link & (BLOCK - 1)),
            leaf & 0x3ff | leaf & (THEAD_BUF | THEAD_CACHE),
        ))
    }

    fn slot(link: u64, phys: u64, size: u64) -> [MapWindow; 2] {
        let tail = 2 << 20;
        [
            MapWindow {
                link,
                phys,
                size: size - tail,
                r: true,
                w: true,
                x: true,
                device: false,
            },
            MapWindow {
                link: link + size - tail,
                phys: phys + size - tail,
                size: tail,
                r: true,
                w: true,
                x: false,
                device: true,
            },
        ]
    }

    #[test]
    fn a_slot_lands_on_its_partition() {
        // Go's LicheeRV-bewijs: linkadres 0x8800_0000, partitie 0x81c0_0000.
        let mut t = Tables::new();
        let base = 0x81c0_0000 + (32 << 20);
        let satp = relocate(
            base,
            &slot(0x8800_0000, 0x81c0_0000, 32 << 20),
            Attrs::Thead,
            &mut t,
        )
        .unwrap();
        assert_eq!(satp, SATP_SV39 | (base >> 12));
        assert_eq!(t.used, 2);
        let (pa, f) = walk(base, &t, 0x8800_1234).unwrap();
        assert_eq!(pa, 0x81c0_1234);
        assert_eq!(f & (V | R | W | X | A | D), V | R | W | X | A | D);
        assert_ne!(f & THEAD_CACHE, 0);
        // De staart: device, dus zonder cache-attributen en niet uitvoerbaar.
        let (pa, f) = walk(base, &t, 0x8800_0000 + (31 << 20)).unwrap();
        assert_eq!(pa, 0x81c0_0000 + (31 << 20));
        assert_eq!(f & (X | THEAD_CACHE | THEAD_BUF), 0);
        assert_eq!(walk(base, &t, 0x9000_0000), None);
    }

    #[test]
    fn spec_attrs_leave_the_high_bits_clear() {
        let mut t = Tables::new();
        relocate(
            0x9000_0000,
            &slot(0x8800_0000, 0x9000_0000, 8 << 20),
            Attrs::Spec,
            &mut t,
        )
        .unwrap();
        assert!(t.pages[1].iter().all(|&e| e >> 54 == 0));
        // Geen G, geen U op een slot-PTE.
        assert!(t.pages[1].iter().all(|&e| e & 0x30 == 0));
    }

    #[test]
    fn crossing_a_gigabyte_takes_a_second_level_page() {
        let mut t = Tables::new();
        let w = MapWindow {
            link: 0xbfe0_0000,
            phys: 0x8000_0000,
            size: 4 << 20,
            r: true,
            w: true,
            x: true,
            device: false,
        };
        relocate(0x1000, &[w], Attrs::Spec, &mut t).unwrap();
        assert_eq!(t.used, 3);
        assert_eq!(walk(0x1000, &t, 0xc000_0000).unwrap().0, 0x8020_0000);
    }

    #[test]
    fn refusals() {
        let mut t = Tables::new();
        let ok = slot(0x8800_0000, 0x9000_0000, 8 << 20);
        assert_eq!(
            relocate(0x123, &ok, Attrs::Spec, &mut t),
            Err(Error::Base { base: 0x123 })
        );
        assert_eq!(
            relocate(0x1000, &[], Attrs::Spec, &mut t),
            Err(Error::NoWindows)
        );
        let mut wo = ok[0];
        wo.r = false;
        assert_eq!(
            relocate(0x1000, &[wo], Attrs::Spec, &mut t),
            Err(Error::Window { index: 0 })
        );
        let mut odd = ok[0];
        odd.phys += 0x1000;
        assert_eq!(
            relocate(0x1000, &[odd], Attrs::Spec, &mut t),
            Err(Error::Align { index: 0 })
        );
        assert_eq!(
            relocate(0x1000, &[ok[0], ok[0]], Attrs::Spec, &mut t),
            Err(Error::Overlap { link: 0x8800_0000 })
        );
        let mut far = ok[0];
        far.link = LIMIT;
        assert_eq!(
            relocate(0x1000, &[far], Attrs::Spec, &mut t),
            Err(Error::Range { link: LIMIT })
        );
    }
}
