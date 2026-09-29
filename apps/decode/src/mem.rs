//! Buffers op hele pagina's in de eigen partitie: wat de kern als grant
//! aanneemt.
//!
//! De codec-calls dragen aanwijzingen, geen bytes (docs/media.md): een
//! buffer is een afstand vanaf `RamStart` en een lengte, beide op hele
//! pagina's van 4 KB, want de codec-MMU kent niets fijners en een buffer die
//! halverwege een pagina begint, neemt de buren mee. De kern weigert al het
//! andere (`codec_grant`). Een [`PageBuf`] is zo'n stuk uit de heap van de
//! app: één allocatie met een pagina speling, het begin naar boven op een
//! paginagrens. Hij groeit nooit, dus zijn adres ligt vast zolang hij leeft;
//! dat is de belofte die de app de kern doet zolang het ijzer erin schrijft.

use alloc::vec::Vec;

/// De paginamaat van de codec-MMU.
pub(crate) const PAGE: usize = 4096;

/// Een stuk van de eigen partitie op hele pagina's.
///
/// # Invariants
///
/// `at + len <= mem.len()`, `mem` groeit of krimpt nooit (dus het adres
/// ligt vast), en `mem.as_ptr() + at` ligt op een paginagrens.
pub(crate) struct PageBuf {
    mem: Vec<u8>,
    at: usize,
    len: usize,
}

impl PageBuf {
    /// Een buffer van `len` bytes naar boven op hele pagina's, of `None` als
    /// de heap hem niet heeft.
    pub(crate) fn new(len: usize) -> Option<PageBuf> {
        let len = len.checked_next_multiple_of(PAGE)?.max(PAGE);
        let mut mem = Vec::new();
        mem.try_reserve_exact(len.checked_add(PAGE)?).ok()?;
        mem.resize(len + PAGE, 0);
        let at = mem.as_ptr().addr().wrapping_neg() % PAGE;
        // INVARIANT: `at < PAGE`, dus `at + len <= len + PAGE == mem.len()`;
        // `mem` wordt hierna nooit meer van maat veranderd.
        Some(PageBuf { mem, at, len })
    }

    /// De lengte in bytes (hele pagina's).
    pub(crate) fn len(&self) -> u64 {
        self.len as u64
    }

    /// Het fysieke begin gezien vanaf `ram_start`: de `off` van de grant.
    pub(crate) fn off(&self, ram_start: u64) -> u64 {
        (self.mem.as_ptr().addr() + self.at) as u64 - ram_start
    }

    /// De bytes, om bitstream in te schrijven.
    pub(crate) fn bytes_mut(&mut self) -> &mut [u8] {
        self.mem
            .get_mut(self.at..self.at + self.len)
            .unwrap_or_default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_page_buffer_starts_on_a_page_and_is_whole_pages() {
        for want in [1, 4095, 4096, 4097, 3 * 1024 * 1024 + 5] {
            let mut b = PageBuf::new(want).unwrap();
            assert_eq!(b.len() % PAGE as u64, 0);
            assert!(b.len() >= want as u64);
            let base = b.mem.as_ptr().addr() as u64;
            // RamStart is een paginagrens; de afstand dus ook.
            let ram = base & !(PAGE as u64 - 1);
            assert_eq!(b.off(ram) % PAGE as u64, 0);
            assert_eq!(b.bytes_mut().len() as u64, b.len());
            assert_eq!(b.bytes_mut().as_ptr().addr() % PAGE, 0);
        }
    }
}
