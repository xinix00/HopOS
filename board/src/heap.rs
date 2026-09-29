//! De heap van de kern: een bump-allocator met een plafond.
//!
//! Heap is voor boot en koude paden (handboek §6): taken spawnen bij boot,
//! verder alloceert niemand. Daarom is dit bewust de kleinste allocator die
//! klopt: een wijzer die alleen oploopt, een plafond dat `null` geeft in
//! plaats van door te lopen, en een `dealloc` die alleen de laatste
//! allocatie terugneemt. Wat terugkomt uit het midden lekt, en de teller
//! [`Heap::stats`] maakt dat zichtbaar in plaats van een belofte.
//!
//! Eén core alloceert; een ISR alloceert nooit. De CAS-lus is er zodat de
//! allocator ook correct blijft als dat ooit verandert, niet omdat het nu
//! nodig is.

use core::alloc::{GlobalAlloc, Layout};
use core::sync::atomic::{
    AtomicU64, AtomicUsize,
    Ordering::{AcqRel, Acquire, Relaxed, Release},
};

/// Een bump-heap over `[start, end)`; leeg tot [`Heap::init`].
pub struct Heap {
    start: AtomicUsize,
    next: AtomicUsize,
    end: AtomicUsize,
    allocs: AtomicU64,
    frees: AtomicU64,
    leaked: AtomicU64,
    refused: AtomicU64,
}

/// De meetlat van de heap.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct HeapStats {
    /// Bytes in gebruik (inclusief gelekte).
    pub used: usize,
    /// Bytes vrij tot het plafond.
    pub free: usize,
    /// Geslaagde allocaties.
    pub allocs: u64,
    /// Teruggenomen allocaties (de laatste in de rij).
    pub frees: u64,
    /// Vrijgaves uit het midden: blijven bezet.
    pub leaked: u64,
    /// Geweigerd op het plafond.
    pub refused: u64,
}

impl Heap {
    /// Een heap zonder geheugen: elke allocatie faalt tot [`init`](Self::init).
    #[must_use]
    pub const fn new() -> Self {
        Self {
            start: AtomicUsize::new(0),
            next: AtomicUsize::new(0),
            end: AtomicUsize::new(0),
            allocs: AtomicU64::new(0),
            frees: AtomicU64::new(0),
            leaked: AtomicU64::new(0),
            refused: AtomicU64::new(0),
        }
    }

    /// Geeft de heap het bereik `[start, end)`. Eén keer, bij boot, vóór
    /// de eerste allocatie.
    ///
    /// # Safety
    ///
    /// Het bereik is gemapt, schrijfbaar geheugen dat niemand anders ooit
    /// gebruikt, en het blijft zo zolang het programma draait.
    pub unsafe fn init(&self, start: usize, end: usize) {
        self.start.store(start, Release);
        self.end.store(end, Release);
        self.next.store(start, Release);
    }

    /// De meetlat.
    #[must_use]
    pub fn stats(&self) -> HeapStats {
        let next = self.next.load(Relaxed);
        let end = self.end.load(Relaxed);
        HeapStats {
            used: next.saturating_sub(self.start.load(Relaxed)),
            free: end.saturating_sub(next),
            allocs: self.allocs.load(Relaxed),
            frees: self.frees.load(Relaxed),
            leaked: self.leaked.load(Relaxed),
            refused: self.refused.load(Relaxed),
        }
    }
}

impl Default for Heap {
    fn default() -> Self {
        Self::new()
    }
}

// SAFETY: `alloc` geeft alleen blokken uit het bereik dat `init` gaf, elk
// blok hoogstens één keer (de wijzer loopt monotoon op, behalve bij het
// terugnemen van precies het laatste blok), met de gevraagde uitlijning;
// op het plafond geeft hij null.
unsafe impl GlobalAlloc for Heap {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let end = self.end.load(Acquire);
        let mut cur = self.next.load(Acquire);
        loop {
            if cur == 0 {
                self.refused.fetch_add(1, Relaxed);
                return core::ptr::null_mut();
            }
            let Some(start) = cur.checked_next_multiple_of(layout.align()) else {
                self.refused.fetch_add(1, Relaxed);
                return core::ptr::null_mut();
            };
            let new = match start.checked_add(layout.size()) {
                Some(n) if n <= end => n,
                _ => {
                    self.refused.fetch_add(1, Relaxed);
                    return core::ptr::null_mut();
                }
            };
            match self.next.compare_exchange(cur, new, AcqRel, Acquire) {
                Ok(_) => {
                    self.allocs.fetch_add(1, Relaxed);
                    return start as *mut u8;
                }
                Err(seen) => cur = seen,
            }
        }
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        let start = ptr as usize;
        let top = start.wrapping_add(layout.size());
        if self
            .next
            .compare_exchange(top, start, AcqRel, Acquire)
            .is_ok()
        {
            self.frees.fetch_add(1, Relaxed);
        } else {
            self.leaked.fetch_add(1, Relaxed);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bumps_aligns_refuses_and_takes_back_the_last() {
        let mut mem = vec![0u8; 512];
        let base = (mem.as_mut_ptr() as usize).next_multiple_of(16);
        let h = Heap::new();
        let l16 = Layout::from_size_align(16, 16).unwrap();
        let l3 = Layout::from_size_align(3, 1).unwrap();
        // SAFETY: zonder init faalt alles.
        assert!(unsafe { h.alloc(l16) }.is_null());
        // SAFETY: de vector leeft de hele test en niemand anders raakt hem.
        unsafe { h.init(base, base + 256) };
        // SAFETY: GlobalAlloc-aanroepen op een geïnitialiseerde heap; elk
        // blok gaat terug met zijn eigen layout.
        unsafe {
            let a = h.alloc(l16);
            let b = h.alloc(l16);
            assert_eq!((a as usize, b as usize), (base, base + 16));
            // Van boven af komen ze allebei terug.
            h.dealloc(b, l16);
            h.dealloc(a, l16);
            assert_eq!(h.stats().frees, 2);
            assert_eq!(h.stats().used, 0);
            // Uitlijning laat een gat: `c` ligt achter opvulling, dus na
            // het terugnemen van `c` ligt `d` niet bovenaan en lekt hij.
            let d = h.alloc(l3);
            let c = h.alloc(l16);
            assert_eq!(c as usize % 16, 0);
            h.dealloc(c, l16);
            h.dealloc(d, l3);
            assert_eq!((h.stats().frees, h.stats().leaked), (3, 1));
            // Te groot: null, geen overloop.
            assert!(h.alloc(Layout::from_size_align(512, 8).unwrap()).is_null());
        }
        assert_eq!(h.stats().refused, 2);
        drop(mem);
    }
}
