//! De heap van een app: een stapel-allocator tussen het einde van het image
//! en de stack, met een plafond en tellers.
//!
//! De executor alloceert één box per `spawn`, en verder alloceert applib
//! niets: het log-pad, de ringen en de system-API-client werken op vaste
//! buffers. Daarom volstaat voorlopig een bump-allocator: vrijgeven lukt
//! alleen voor het laatst uitgegeven blok (een taak die meteen weer eindigt),
//! de rest blijft staan tot het slot herstart. Een app die in een lus taken
//! spawnt, loopt zo tegen het plafond; dat is een luide `SpawnError`, geen
//! stille groei. Een allocator met een vrije lijst komt met `leannet`, die
//! de eerste echte heap-gebruiker is.
//!
//! De tellers zijn de meetlat van het handboek (§6): `used` gaat als
//! geheugen-draw naar de control-page (`CtrlMemSys`), zodat de kern per taak
//! weet wat hij gebruikt naast wat hij mag.

use core::sync::atomic::{
    AtomicU64, AtomicUsize,
    Ordering::{AcqRel, Acquire, Relaxed, Release},
};

/// Een bump-allocator over `[start, end)`.
pub struct Bump {
    start: AtomicUsize,
    next: AtomicUsize,
    end: AtomicUsize,
    /// Geslaagde allocaties.
    pub allocs: AtomicU64,
    /// Geweigerde allocaties (het plafond).
    pub refused: AtomicU64,
}

impl Bump {
    /// Een lege heap: elke allocatie faalt tot [`Bump::init`].
    #[must_use]
    pub const fn new() -> Self {
        Self {
            start: AtomicUsize::new(0),
            next: AtomicUsize::new(0),
            end: AtomicUsize::new(0),
            allocs: AtomicU64::new(0),
            refused: AtomicU64::new(0),
        }
    }

    /// Geeft de heap het bereik `[start, end)`. Door de main-schil, één
    /// keer, vóór de eerste `spawn`.
    pub fn init(&self, start: usize, end: usize) {
        let end = end.max(start);
        self.start.store(start, Relaxed);
        self.end.store(end, Relaxed);
        self.next.store(start, Release);
    }

    /// Reserveert `size` bytes met uitlijning `align` (een macht van twee).
    pub fn reserve(&self, size: usize, align: usize) -> Option<usize> {
        let end = self.end.load(Relaxed);
        let mut cur = self.next.load(Acquire);
        loop {
            let Some(p) = cur.checked_add(align - 1).map(|v| v & !(align - 1)) else {
                break;
            };
            let Some(new) = p.checked_add(size).filter(|&n| n <= end && p != 0) else {
                break;
            };
            match self.next.compare_exchange_weak(cur, new, AcqRel, Acquire) {
                Ok(_) => {
                    self.allocs.fetch_add(1, Relaxed);
                    return Some(p);
                }
                Err(seen) => cur = seen,
            }
        }
        self.refused.fetch_add(1, Relaxed);
        None
    }

    /// Geeft een blok terug. Alleen het laatst uitgegeven blok komt echt
    /// vrij; een ander blok blijft staan (zie de module-doc).
    pub fn release(&self, p: usize, size: usize) {
        if let Some(e) = p.checked_add(size) {
            let _ = self.next.compare_exchange(e, p, AcqRel, Relaxed);
        }
    }

    /// Bytes in gebruik (inclusief wat niet vrij kon komen).
    #[must_use]
    pub fn used(&self) -> u64 {
        let n = self
            .next
            .load(Relaxed)
            .saturating_sub(self.start.load(Relaxed));
        n as u64
    }

    /// De maat van de heap.
    #[must_use]
    pub fn capacity(&self) -> u64 {
        let n = self
            .end
            .load(Relaxed)
            .saturating_sub(self.start.load(Relaxed));
        n as u64
    }
}

impl Default for Bump {
    fn default() -> Self {
        Self::new()
    }
}

/// De heap van dit image, en op het target de global allocator.
#[cfg_attr(all(target_os = "none", not(test)), global_allocator)]
pub static HEAP: Bump = Bump::new();

#[cfg(all(target_os = "none", not(test)))]
mod global {
    use super::Bump;
    use core::alloc::{GlobalAlloc, Layout};

    // SAFETY: `alloc` geeft een blok van minstens `size` bytes op de
    // gevraagde uitlijning binnen `[start, end)`, dat niet overlapt met een
    // ander levend blok (de CAS op `next` geeft elk bereik één keer uit), of
    // null. `dealloc` maakt alleen het laatst uitgegeven blok vrij.
    unsafe impl GlobalAlloc for Bump {
        unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
            match self.reserve(layout.size(), layout.align()) {
                Some(p) => core::ptr::with_exposed_provenance_mut(p),
                None => core::ptr::null_mut(),
            }
        }

        unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
            self.release(ptr.expose_provenance(), layout.size());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn aligns_and_refuses_past_the_ceiling() {
        let h = Bump::new();
        assert_eq!(h.reserve(8, 8), None); // vóór init
        h.init(0x1001, 0x1100);
        assert_eq!(h.reserve(16, 16), Some(0x1010));
        assert_eq!(h.reserve(1, 1), Some(0x1020));
        assert_eq!(h.reserve(0x100, 8), None);
        assert_eq!(h.refused.load(Relaxed), 2);
        assert_eq!(h.allocs.load(Relaxed), 2);
        assert_eq!(h.used(), 0x20);
        assert_eq!(h.capacity(), 0xff);
    }

    #[test]
    fn only_the_last_block_comes_back() {
        let h = Bump::new();
        h.init(0x2000, 0x3000);
        let a = h.reserve(0x100, 8).unwrap();
        let b = h.reserve(0x100, 8).unwrap();
        h.release(a, 0x100); // niet de laatste: blijft staan
        assert_eq!(h.used(), 0x200);
        h.release(b, 0x100);
        assert_eq!(h.used(), 0x100);
        assert_eq!(h.reserve(0x100, 8), Some(b));
    }
}
