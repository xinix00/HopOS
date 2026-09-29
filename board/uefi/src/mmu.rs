//! De identity map van het UEFI-board: 48 bits VA, 4 KB-korrel, gebouwd
//! bij boot uit de EFI-memory-map (de Rust-vorm van Go's `mmu48.go`).
//!
//! Waarom 48 bits: serversilicium legt periferie hoog. De Altra-UART woont
//! op 0x1000_0260_0000 (16 TB) en ECAM's en BAR's liggen er boven de
//! 512 GB (gemeten 13-07); QEMU virt met highmem legt de ECAM op 256 GB en
//! het hoge MMIO-venster op 512 GB. Een 39-bit-wereld (tamago's, en die
//! van `cpu::boot`) haalt daar niets van.
//!
//! De kaart, van laag naar hoog in voorrang:
//!
//! - alles onder [`DEVICE_SPAN`] als Device-nGnRnE, in blokken van 1 GB
//!   (Device-geheugen wordt nooit speculatief gelezen, dus een gat mappen
//!   kost niets; het is wat tamago ook deed);
//! - wat het board daarboven expliciet noemt (UART, ECAM, GIC,
//!   EFI-MMIO) als Device;
//! - elke RAM-descriptor als Normal WB, op 4 KB precies: een 2 MB-blok dat
//!   een firmware-gat meeneemt, maakt dat gat speculatief leesbaar, en
//!   daar vallen secure carve-outs onder;
//! - de DMA-regio als Normal non-cacheable, het kooi-venster als Device.
//!
//! De mapper splitst een blok in een tabel als een latere mapping maar een
//! deel ervan raakt, met de attributen van het blok erin. Tabellen komen
//! uit een pool die het board bij boot alloceert, in Normal-WB-geheugen:
//! de table-walker leest cacheable (TCR IRGN/ORGN = WB), dus wat de kern
//! schrijft is voor hem zichtbaar zonder veeg. Go's les (Dereks review #1,
//! 14-07): tabellen in device-gemapt geheugen gaven stale walks op echt
//! silicium, en QEMU is cache-blind.

use dev::Pa;

/// Tot waar alles standaard Device is: 1 TB (twee L0-ingangen).
pub(crate) const DEVICE_SPAN: u64 = 1 << 40;

/// De hoogste VA/PA: 48 bits.
pub(crate) const VA_LIMIT: u64 = 1 << 48;

/// Geldig, tabel (niveau 0 tot 2) of pagina (niveau 3).
const TABLE: u64 = 0b11;
/// Geldig blok (niveau 1 en 2).
const BLOCK: u64 = 0b01;
/// De adresbits van een descriptor (47:12).
const ADDR: u64 = 0x0000_ffff_ffff_f000;

/// De verschuiving van niveau `l` (0 tot 3).
const fn shift(l: u32) -> u32 {
    39 - 9 * l
}

/// Waarom de mapper weigert.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum Error {
    /// De tabelpool is op.
    OutOfTables,
    /// Een bereik boven de 48 bits of niet 4 KB-gealigneerd.
    Range {
        /// Het begin.
        pa: u64,
        /// De maat.
        size: u64,
    },
}

/// De attribuutbits van een blok of pagina met MAIR-index `attr`, zonder
/// adres en type (de encodering van `cpu::boot::block`).
pub(crate) const fn attrs(attr: u64) -> u64 {
    cpu::boot::block(0, attr) & !0b11
}

/// De tabellen van één identity map, uit een pool van pagina's.
///
/// # Invariants
///
/// `[pool, pool + cap * 4096)` is van deze mapper, gemapt en schrijfbaar;
/// de eerste `used` pagina's zijn tabellen (pagina 0 is de L0).
pub(crate) struct Mmu {
    pool: u64,
    cap: u64,
    used: u64,
}

impl Mmu {
    /// Een lege map met zijn tabellen in `[pool, pool + cap * 4096)`.
    ///
    /// # Safety
    ///
    /// Dat bereik is 4 KB-gealigneerd, van niemand anders, en schrijfbaar
    /// zolang de map leeft (en zolang de MMU hem gebruikt).
    pub(crate) unsafe fn new(pool: u64, cap: u64) -> Result<Self, Error> {
        if cap == 0 || !pool.is_multiple_of(4096) {
            return Err(Error::OutOfTables);
        }
        dev::clear(Pa(pool), 4096);
        // INVARIANT: de voorwaarde; pagina 0 is net gewist tot een lege L0.
        Ok(Self { pool, cap, used: 1 })
    }

    /// De levende map terug: de pool met de eerste `used` tabellen in
    /// gebruik (na de boot, om een hoog Device-venster bij te mappen).
    ///
    /// # Safety
    ///
    /// Zoals [`new`](Self::new), en de eerste `used` pagina's zijn de
    /// tabellen van een map die [`new`](Self::new) daar bouwde.
    pub(crate) unsafe fn resume(pool: u64, cap: u64, used: u64) -> Result<Self, Error> {
        if used == 0 || used > cap || !pool.is_multiple_of(4096) {
            return Err(Error::OutOfTables);
        }
        // INVARIANT: de voorwaarde van deze functie.
        Ok(Self { pool, cap, used })
    }

    /// De L0: wat in TTBR0 hoort.
    pub(crate) fn root(&self) -> u64 {
        self.pool
    }

    /// Hoeveel tabellen er in gebruik zijn.
    pub(crate) fn tables(&self) -> u64 {
        self.used
    }

    /// Het bereik van de pool dat in gebruik is: `(basis, bytes)`.
    pub(crate) fn used_range(&self) -> (u64, u64) {
        (self.pool, self.used * 4096)
    }

    /// Een verse, lege tabel.
    fn table(&mut self) -> Result<u64, Error> {
        if self.used >= self.cap {
            return Err(Error::OutOfTables);
        }
        let t = self.pool + self.used * 4096;
        self.used += 1;
        dev::clear(Pa(t), 4096);
        Ok(t)
    }

    /// Mapt `[pa, pa + size)` identiek met de attribuutbits `bits` (zie
    /// [`attrs`]), over wat er stond heen. Grote gealigneerde stukken
    /// worden een blok van 1 GB of 2 MB, de randen pagina's van 4 KB.
    pub(crate) fn map(&mut self, pa: u64, size: u64, bits: u64) -> Result<(), Error> {
        let end = pa.checked_add(size).ok_or(Error::Range { pa, size })?;
        if !pa.is_multiple_of(4096) || !end.is_multiple_of(4096) || end > VA_LIMIT {
            return Err(Error::Range { pa, size });
        }
        if size == 0 {
            return Ok(());
        }
        self.map_level(self.pool, 0, pa, end, bits)
    }

    /// Eén niveau van [`map`](Self::map): elke ingang die het bereik raakt.
    fn map_level(
        &mut self,
        table: u64,
        level: u32,
        pa: u64,
        end: u64,
        bits: u64,
    ) -> Result<(), Error> {
        let sz = 1u64 << shift(level);
        let mut at = pa;
        while at < end {
            let entry_base = at & !(sz - 1);
            let entry_end = entry_base.saturating_add(sz);
            let hi = end.min(entry_end);
            let slot = Pa(table + ((at >> shift(level)) & 0x1ff) * 8);
            let whole = at == entry_base && hi == entry_end;
            if whole && level >= 1 {
                let ty = if level == 3 { TABLE } else { BLOCK };
                dev::write64(slot, entry_base | bits | ty);
            } else {
                let next = self.descend(slot, level, entry_base)?;
                self.map_level(next, level + 1, at, hi, bits)?;
            }
            at = hi;
        }
        Ok(())
    }

    /// De tabel onder `slot`: bestaand, nieuw, of een gesplitst blok.
    fn descend(&mut self, slot: Pa, level: u32, entry_base: u64) -> Result<u64, Error> {
        let e = dev::read64(slot);
        if e & 0b11 == TABLE {
            return Ok(e & ADDR);
        }
        let t = self.table()?;
        if e & 0b11 == BLOCK {
            // Het blok wordt 512 kleinere met dezelfde attributen.
            let bits = e & !ADDR & !0b11;
            let child = level + 1;
            let csz = 1u64 << shift(child);
            let ty = if child == 3 { TABLE } else { BLOCK };
            for i in 0..512 {
                dev::write64(Pa(t + i * 8), (entry_base + i * csz) | bits | ty);
            }
        }
        dev::write64(slot, t | TABLE);
        Ok(t)
    }

    /// Wat er op `pa` gemapt staat: `(niveau, descriptor)`, of `None`.
    #[cfg(test)]
    pub(crate) fn lookup(&self, pa: u64) -> Option<(u32, u64)> {
        let mut table = self.pool;
        for level in 0..4 {
            let e = dev::read64(Pa(table + ((pa >> shift(level)) & 0x1ff) * 8));
            match e & 0b11 {
                TABLE if level < 3 => table = e & ADDR,
                TABLE | BLOCK => return Some((level, e)),
                _ => return None,
            }
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use cpu::boot::{ATTR_DEVICE, ATTR_NORMAL, ATTR_NORMAL_NC};

    /// Een pool van `n` gealigneerde pagina's op de host.
    fn pool(n: usize) -> (std::alloc::Layout, u64) {
        let l = std::alloc::Layout::from_size_align(n * 4096, 4096).unwrap();
        // SAFETY: een niet-lege layout.
        let p = unsafe { std::alloc::alloc(l) };
        assert!(!p.is_null());
        (l, p as usize as u64)
    }

    const GB: u64 = 1 << 30;
    const MB2: u64 = 2 << 20;

    #[test]
    fn devices_blocks_ram_pages_and_splits() {
        let (_l, p) = pool(64);
        // SAFETY: de pool is van deze test.
        let mut m = unsafe { Mmu::new(p, 64) }.unwrap();
        let dev = attrs(ATTR_DEVICE);
        let ram = attrs(ATTR_NORMAL);
        let nc = attrs(ATTR_NORMAL_NC);
        m.map(0, DEVICE_SPAN, dev).unwrap();
        // Twee L1-tabellen voor 1 TB aan 1 GB-blokken.
        assert_eq!(m.tables(), 3);
        assert_eq!(m.lookup(0x0900_0000), Some((1, dev | BLOCK)));
        // RAM van 1 GB + 1 MB + 4 KB: één 1 GB-blok, dan 2 MB... nee: de
        // staart is kleiner dan 2 MB, dus pagina's.
        let ram_base = 0x4000_0000;
        m.map(ram_base, GB + (1 << 20) + 4096, ram).unwrap();
        assert_eq!(m.lookup(ram_base), Some((1, ram_base | ram | BLOCK)));
        assert_eq!(m.lookup(0x8000_0000), Some((3, 0x8000_0000 | ram | TABLE)));
        assert_eq!(m.lookup(0x8010_0000), Some((3, 0x8010_0000 | ram | TABLE)));
        // Voorbij de staart: het gesplitste Device-blok, nu als pagina.
        assert_eq!(m.lookup(0x8010_1000), Some((3, 0x8010_1000 | dev | TABLE)));
        // En een 2 MB verder is nog een Device-blok van 2 MB.
        assert_eq!(m.lookup(0x8020_0000), Some((2, 0x8020_0000 | dev | BLOCK)));
        // De DMA-regio: 2 MB NC midden in het RAM-blok splitst het.
        m.map(0x4f00_0000, MB2, nc).unwrap();
        assert_eq!(m.lookup(0x4f00_0000), Some((2, 0x4f00_0000 | nc | BLOCK)));
        assert_eq!(m.lookup(0x4ee0_0000), Some((2, 0x4ee0_0000 | ram | BLOCK)));
        // Hoog: de Altra-UART op 16 TB.
        m.map(0x1000_0260_0000, 4096, dev).unwrap();
        assert_eq!(
            m.lookup(0x1000_0260_0000),
            Some((3, 0x1000_0260_0000 | dev | TABLE))
        );
        assert_eq!(m.lookup(0x1000_0260_1000), None);
        assert_eq!(m.lookup(DEVICE_SPAN), None);
    }

    #[test]
    fn bad_ranges_and_an_empty_pool_are_refused() {
        let (_l, p) = pool(2);
        // SAFETY: de pool is van deze test.
        let mut m = unsafe { Mmu::new(p, 2) }.unwrap();
        assert_eq!(
            m.map(0x1001, 4096, 0),
            Err(Error::Range {
                pa: 0x1001,
                size: 4096
            })
        );
        assert!(m.map(VA_LIMIT, 4096, 0).is_err());
        // Eén tabel over: een L1, dan is hij op.
        assert_eq!(m.map(0, 4096, 0), Err(Error::OutOfTables));
    }
}
