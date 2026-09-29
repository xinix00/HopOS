//! De page tables van de codec-MMU (Go: `mmu.go`).
//!
//! De MVE heeft een EIGEN MMU, en dat is voor HopOS het belangrijkste feit
//! van dit blok: de VPU ziet 32-bit virtuele adressen die wij vertalen, niet
//! het fysieke geheugen van de node. Wie de tabellen schrijft bepaalt waar
//! de decoder mag lezen en schrijven. Een sessie kan alleen bij zijn eigen
//! firmware, zijn eigen ringen en de buffers die de kern er expliciet in
//! hangt: een buffer van een app die in deze tabel hangt IS de grant, en de
//! tabel is de isolatie.
//!
//! Twee niveaus over pagina's van 4 KB, 1024 entries per tabelpagina:
//!
//! ```text
//!  31        22 21        12 11         0
//! +------------+------------+------------+
//! |  L1 index  |  L2 index  | pageoffset |
//! +------------+------------+------------+
//! ```
//!
//! Een PTE is één 32-bit woord: `attr[31:30] | pa[39:12] << 2 | ap[1:0]`.

use crate::arena::Arena;
use alloc::vec::Vec;
use bounded::BoundedVec;
use dev::Pa;
use driver_codec::{Error, Result};

/// De paginamaat van de codec-MMU.
pub const PAGE: u64 = 1 << PAGE_SHIFT;
/// log2 van [`PAGE`].
pub const PAGE_SHIFT: u32 = 12;
/// Entries per tabelpagina.
pub(crate) const PTES: usize = 1024;
const IDX_SHIFT: u32 = 10;
const IDX_MASK: u32 = PTES as u32 - 1;

pub(crate) const PTE_ATTR_SHIFT: u32 = 30;
pub(crate) const PTE_PA_SHIFT: u32 = 2;
pub(crate) const PTE_PA_MASK: u32 = (1 << 28) - 1;

/// Attribuut: privé voor deze sessie.
pub(crate) const ATTR_PRIVATE: u32 = 0;
/// Attribuut: gedeeld lezen/schrijven.
#[cfg(test)]
pub(crate) const ATTR_SHARED_RW: u32 = 3;

/// Toegang: alleen lezen (de L1-entries: de VPU leest tabellen, schrijft ze
/// nooit).
pub(crate) const ACCESS_RO: u32 = 1;
/// Toegang: uitvoerbaar (de firmwarecode).
pub(crate) const ACCESS_EXEC: u32 = 2;
/// Toegang: lezen en schrijven.
pub(crate) const ACCESS_RW: u32 = 3;

/// Hoeveel losse stukken arena een tabel zelf houdt (L1, code, bss, zeven
/// communicatiepagina's): de L2-pagina's staan apart in hun index.
pub(crate) const MAX_OWNED: usize = 16;

/// Een stuk arena.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) struct Span {
    pub(crate) pa: u64,
    pub(crate) pages: u32,
}

/// De page table van één sessie. Het geheugen van de index wordt één keer
/// bij de probe gereserveerd en per sessie hergebruikt: de kern-heap is een
/// bump-allocator, een allocatie per open zou lekken.
pub(crate) struct Mmu {
    l1: u64,
    /// L1-index naar het fysieke adres van zijn L2-pagina (0 = geen).
    l2: Vec<u64>,
    owned: BoundedVec<Span, MAX_OWNED>,
}

impl Mmu {
    /// Een lege tabel met een gereserveerde index (boot).
    pub(crate) fn reserve() -> Result<Mmu> {
        let mut l2 = Vec::new();
        l2.try_reserve_exact(PTES)
            .map_err(|_| Error::Full { cap: PTES as u32 })?;
        l2.resize(PTES, 0);
        Ok(Mmu {
            l1: 0,
            l2,
            owned: BoundedVec::new(),
        })
    }

    /// Legt de L1-pagina neer: het begin van een sessie.
    pub(crate) fn build(&mut self, a: &mut Arena) -> Result {
        let l1 = a.alloc(1, PAGE_SHIFT as u8)?;
        self.l1 = l1;
        self.own(a, l1, 1)
    }

    /// Het fysieke adres van de L1-pagina: wat in MMU_CTRL gaat.
    pub(crate) fn table(&self) -> u64 {
        self.l1
    }

    fn own(&mut self, a: &mut Arena, pa: u64, pages: u32) -> Result {
        if self.owned.push(Span { pa, pages }).is_err() {
            a.free(pa, pages);
            return Err(Error::Full {
                cap: MAX_OWNED as u32,
            });
        }
        Ok(())
    }

    /// Reserveert `n` pagina's in de arena, mapt ze op `va` en geeft het
    /// fysieke basisadres. Aaneengesloten fysiek: de host schrijft er met
    /// één kopie in en niemand houdt een scatterlijst bij.
    pub(crate) fn alloc(&mut self, a: &mut Arena, va: u32, n: u32, access: u32) -> Result<u64> {
        let pa = a.alloc(n, PAGE_SHIFT as u8)?;
        if let Err(e) = self.map_range(a, va, pa, n, access) {
            a.free(pa, n);
            return Err(e);
        }
        self.own(a, pa, n)?;
        Ok(pa)
    }

    /// Reserveert `n` aaneengesloten pagina's zonder ze te mappen; de
    /// aanroeper mapt ze zelf (de dunne bss van de firmware).
    pub(crate) fn alloc_unmapped(&mut self, a: &mut Arena, n: u32) -> Result<u64> {
        let pa = a.alloc(n, PAGE_SHIFT as u8)?;
        self.own(a, pa, n)?;
        Ok(pa)
    }

    /// Hangt `n` fysieke pagina's vanaf `pa` op de adressen vanaf `va`.
    pub(crate) fn map_range(
        &mut self,
        a: &mut Arena,
        va: u32,
        pa: u64,
        n: u32,
        access: u32,
    ) -> Result {
        for i in 0..n {
            let v = va.wrapping_add(i << PAGE_SHIFT);
            self.map_page(a, v, pa + (u64::from(i) << PAGE_SHIFT), access)?;
        }
        Ok(())
    }

    /// Zet één vertaling. De L2-pagina komt er als hij er nog niet is; de
    /// L1-entry die ernaar wijst is read-only, zoals de firmware verwacht.
    pub(crate) fn map_page(&mut self, a: &mut Arena, va: u32, pa: u64, access: u32) -> Result {
        if pa & (PAGE - 1) != 0 {
            return Err(Error::ArenaRange { at: pa });
        }
        let l1i = (va >> (PAGE_SHIFT + IDX_SHIFT)) & IDX_MASK;
        let l2i = (va >> PAGE_SHIFT) & IDX_MASK;
        let slot = self
            .l2
            .get_mut(l1i as usize)
            .ok_or(Error::ArenaRange { at: u64::from(va) })?;
        if *slot == 0 {
            let p = a.alloc(1, PAGE_SHIFT as u8)?;
            *slot = p;
            dev::write32(
                Pa(self.l1 + u64::from(l1i) * 4),
                pte(ATTR_PRIVATE, p, ACCESS_RO),
            );
        }
        dev::write32(
            Pa(*slot + u64::from(l2i) * 4),
            pte(ATTR_PRIVATE, pa, access),
        );
        Ok(())
    }

    /// Haalt `n` pagina's vanaf `va` uit de tabel. De pagina's zelf blijven
    /// van hun eigenaar: buffers van een app horen niet bij de arena.
    pub(crate) fn unmap_range(&mut self, va: u32, n: u32) {
        for i in 0..n {
            let v = va.wrapping_add(i << PAGE_SHIFT);
            let l1i = (v >> (PAGE_SHIFT + IDX_SHIFT)) & IDX_MASK;
            if let Some(&l2) = self.l2.get(l1i as usize)
                && l2 != 0
            {
                dev::write32(Pa(l2 + u64::from((v >> PAGE_SHIFT) & IDX_MASK) * 4), 0);
            }
        }
    }

    /// Vertaalt terug zoals de VPU het ziet (40 bits fysiek). Alleen voor
    /// tests en diagnose: het normale pad kent zijn adressen uit `alloc`.
    #[cfg(test)]
    pub(crate) fn lookup(&self, va: u32) -> Option<u64> {
        let l1i = (va >> (PAGE_SHIFT + IDX_SHIFT)) & IDX_MASK;
        let l2 = *self.l2.get(l1i as usize)?;
        if l2 == 0 {
            return None;
        }
        let p = dev::read32(Pa(l2 + u64::from((va >> PAGE_SHIFT) & IDX_MASK) * 4));
        if p == 0 {
            return None;
        }
        Some(
            (u64::from((p >> PTE_PA_SHIFT) & PTE_PA_MASK) << PAGE_SHIFT)
                + u64::from(va & (PAGE as u32 - 1)),
        )
    }

    /// Geeft alles terug wat deze tabel in de arena hield; daarna is hij
    /// weer leeg en herbruikbaar voor de volgende sessie op dit slot.
    pub(crate) fn destroy(&mut self, a: &mut Arena) {
        for s in self.owned.iter() {
            a.free(s.pa, s.pages);
        }
        self.owned.clear();
        for l2 in &mut self.l2 {
            if *l2 != 0 {
                a.free(*l2, 1);
                *l2 = 0;
            }
        }
        self.l1 = 0;
    }
}

/// Bouwt één page-table-entry.
pub(crate) const fn pte(attr: u32, pa: u64, access: u32) -> u32 {
    (attr << PTE_ATTR_SHIFT)
        | ((((pa >> PAGE_SHIFT) as u32) & PTE_PA_MASK) << PTE_PA_SHIFT)
        | access
}
