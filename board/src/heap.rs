//! De kern-heap hergebruikt vrijgegeven blokken, ook bij interleaved I/O.
//! De allocator wordt gedeeld met applib; alleen de meetlat en core-identiteit
//! verschillen. De kern alloceert uitsluitend op zijn primaire executorcore,
//! nooit in een ISR of vanaf de geparkeerde secundaire cores.
use core::alloc::{GlobalAlloc, Layout};

/// De enige allocerende kerncore.
#[doc(hidden)]
pub struct KernelCore;
impl heap::CoreId for KernelCore {
    fn id() -> u64 {
        0
    }
}

/// De meetlat van de kernheap; bytes in gebruik omvatten blokkoppen.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct HeapStats {
    /// Bytes in gebruik.
    pub used: usize,
    /// Vrije bytes onder het plafond.
    pub free: usize,
    /// Geslaagde allocaties.
    pub allocs: u64,
    /// Vrijgaven.
    pub frees: u64,
    /// Geweigerde allocaties.
    pub refused: u64,
}

/// Een herbruikbare heap over het door het board aangewezen RAM.
pub struct Heap(heap::Heap<KernelCore>);
impl Heap {
    /// Leeg tot `init`.
    #[must_use]
    pub const fn new() -> Self {
        Self(heap::Heap::new())
    }
    /// Legt het eigen gebied vast, eenmaal bij boot.
    ///
    /// # Safety
    /// `[start, end)` is geldig beschrijfbaar RAM dat uitsluitend deze heap
    /// bezit en dat gedurende de hele uitvoering beschikbaar blijft.
    pub unsafe fn init(&self, start: usize, end: usize) {
        // SAFETY: de aanroeper levert het exclusieve, blijvende heapgebied.
        unsafe { self.0.init(start, end) };
    }
    /// De meetlat voor GUI en console.
    #[must_use]
    pub fn stats(&self) -> HeapStats {
        let s = self.0.stats();
        HeapStats {
            used: s.used as usize,
            free: s.ceiling.saturating_sub(s.used) as usize,
            allocs: s.allocs,
            frees: s.frees,
            refused: s.failed,
        }
    }
}
impl Default for Heap {
    fn default() -> Self {
        Self::new()
    }
}
// SAFETY: de gedeelde allocator bewaakt uitlijning, grenzen en exclusief
// blokbezit. De kern gebruikt hem alleen op zijn primaire executorcore.
unsafe impl GlobalAlloc for Heap {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        // SAFETY: hetzelfde GlobalAlloc-contract, rechtstreeks doorgegeven.
        unsafe { self.0.alloc(layout) }
    }
    unsafe fn dealloc(&self, p: *mut u8, layout: Layout) {
        // SAFETY: p/layout kwamen uit deze allocator en worden niet meer gebruikt.
        unsafe { self.0.dealloc(p, layout) }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn interleaved_sync_and_network_buffers_do_not_exhaust_kernel_heap() {
        let mut memory = vec![0u8; 4 << 20];
        let h = Heap::new();
        let start = memory.as_mut_ptr() as usize;
        // SAFETY: memory blijft exclusief van h voor de duur van deze test.
        unsafe { h.init(start, start + memory.len()) };
        let tree = Layout::from_size_align(524288, 16).unwrap();
        let blob = Layout::from_size_align(483328, 16).unwrap();
        let tcp = Layout::from_size_align(65536, 64).unwrap();
        for _ in 0..2000 {
            // SAFETY: ieder blok komt van h, is niet null en wordt precies
            // eenmaal vrijgegeven. Er bestaan geen leningen van de lijven.
            unsafe {
                let a = h.alloc(tree);
                let b = h.alloc(blob);
                let c = h.alloc(tcp);
                assert!(!a.is_null() && !b.is_null() && !c.is_null());
                h.dealloc(a, tree);
                h.dealloc(b, blob);
                h.dealloc(c, tcp);
            }
            assert_eq!(h.stats().used, 0);
        }
        assert_eq!(h.stats().refused, 0);
        assert_eq!(h.stats().frees, 6000);
    }
}
