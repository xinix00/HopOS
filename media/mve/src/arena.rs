//! De arena: het fysieke geheugen dat de VPU mag zien (Go: `arena.go`).
//!
//! Page tables, firmware, de communicatiepagina's en alles wat de firmware
//! onderweg zelf opvraagt (referentieframes: bij 4K HEVC honderden MB's).
//! Eén regio, door de kern aangewezen buiten elke partitie, en ongecached
//! gemapt door het board: de VPU van de O6N is niet coherent (`_CCA = 0`).
//!
//! De allocator is een bitmap over pagina's van 4 KB met first-fit. Bewust
//! het domste dat werkt: de aanvragen zijn groot en zeldzaam, en wat hij wél
//! moet kunnen is uitlijning (de firmware vraagt 2^n-grenzen). Eigendom: de
//! arena is van de [`crate::Device`] en daarmee van één taak; de lock van Go
//! (`arena.mu`) bestaat hier niet.

use crate::mmu::{PAGE, PAGE_SHIFT};
use alloc::vec::Vec;
use driver_codec::{Error, Result};

/// De grootste uitlijning die ergens op slaat: de VPU ziet 32-bit adressen,
/// dus een grens van 2^32 of meer bestaat voor hem niet. Daarboven liep de
/// stapgrootte van `alloc` in Go over (nul: een eindeloze lus).
pub(crate) const MAX_LOG2_ALIGN: u8 = 31;

/// Het fysieke geheugen van de VPU.
#[derive(Debug)]
pub struct Arena {
    base: u64,
    pages: u32,
    used: Vec<u64>,
    free: u32,
}

impl Arena {
    /// Neemt `[base, base + size)` in beheer, naar binnen afgerond op
    /// pagina's. Allocatie van de bitmap is de enige heap die de arena ooit
    /// kost, en die gebeurt hier, bij boot.
    pub fn new(base: u64, size: u64) -> Result<Arena> {
        let start = base.checked_add(PAGE - 1).map_or(0, |v| v & !(PAGE - 1));
        let end = base.saturating_add(size) & !(PAGE - 1);
        if end <= start {
            return Ok(Arena {
                base: 0,
                pages: 0,
                used: Vec::new(),
                free: 0,
            });
        }
        let pages =
            u32::try_from((end - start) / PAGE).map_err(|_| Error::ArenaRange { at: size })?;
        let words = pages.div_ceil(64) as usize;
        let mut used = Vec::new();
        used.try_reserve_exact(words)
            .map_err(|_| Error::Full { cap: pages })?;
        used.resize(words, 0);
        Ok(Arena {
            base: start,
            pages,
            used,
            free: pages,
        })
    }

    /// De capaciteit en wat daarvan vrij is, in pagina's.
    #[must_use]
    pub fn pages(&self) -> (u32, u32) {
        (self.pages, self.free)
    }

    /// Het fysieke basisadres.
    #[must_use]
    pub fn base(&self) -> u64 {
        self.base
    }

    /// Reserveert `n` aaneengesloten pagina's op een 2^`log2align`-grens en
    /// geeft hun basisadres. De pagina's zijn gewist: de firmware verwacht
    /// nullen in zijn bss, en een halve vorige sessie in een referentieframe
    /// is het soort bug dat zich als willekeurige artefacten voordoet.
    pub fn alloc(&mut self, n: u32, log2align: u8) -> Result<u64> {
        if n == 0 || log2align > MAX_LOG2_ALIGN {
            return Err(Error::ArenaRange {
                at: u64::from(log2align),
            });
        }
        let step: u64 = if u32::from(log2align) > PAGE_SHIFT {
            1 << (u32::from(log2align) - PAGE_SHIFT)
        } else {
            1
        };
        // De basis zelf hoeft niet op de grens te liggen: de stap telt vanaf
        // de eerste pagina die hem wél haalt.
        let align = 1u64 << log2align.max(PAGE_SHIFT as u8);
        let first = ((self.base.wrapping_add(align - 1) & !(align - 1)) - self.base) / PAGE;
        let mut i = first;
        while i + u64::from(n) <= u64::from(self.pages) {
            let at = i as u32;
            if self.span_free(at, n) {
                self.mark(at, n, true);
                self.free -= n;
                let pa = self.base + i * PAGE;
                dev::clear(dev::Pa(pa), n as usize * PAGE as usize);
                return Ok(pa);
            }
            i += step;
        }
        Err(Error::ArenaFull {
            pages: u64::from(n),
        })
    }

    /// Geeft `n` pagina's vanaf `pa` terug. Een adres buiten de arena wordt
    /// genegeerd, niet gepanikeerd: dit pad loopt ook bij het opruimen van
    /// een gecrashte sessie. Alleen wat werkelijk in gebruik was telt: een
    /// dubbele free mag de teller niet opblazen, anders belooft `pages`
    /// ruimte die er niet is.
    pub fn free(&mut self, pa: u64, n: u32) {
        let Some(i) = self.index(pa) else { return };
        if u64::from(i) + u64::from(n) > u64::from(self.pages) {
            return;
        }
        self.free += self.mark(i, n, false);
    }

    fn index(&self, pa: u64) -> Option<u32> {
        let off = pa.checked_sub(self.base)?;
        if !off.is_multiple_of(PAGE) || off / PAGE >= u64::from(self.pages) {
            return None;
        }
        u32::try_from(off / PAGE).ok()
    }

    fn bit(&self, j: u32) -> bool {
        self.used
            .get((j / 64) as usize)
            .is_some_and(|w| w & (1 << (j % 64)) != 0)
    }

    fn span_free(&self, i: u32, n: u32) -> bool {
        (i..i + n).all(|j| !self.bit(j))
    }

    /// Zet of wist `[i, i + n)` en geeft hoeveel bits er werkelijk omgingen.
    fn mark(&mut self, i: u32, n: u32, set: bool) -> u32 {
        let mut changed = 0;
        for j in i..i + n {
            if self.bit(j) != set
                && let Some(w) = self.used.get_mut((j / 64) as usize)
            {
                *w ^= 1 << (j % 64);
                changed += 1;
            }
        }
        changed
    }
}
