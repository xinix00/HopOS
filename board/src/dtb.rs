//! De DTB van deze boot: één keer gevonden in `discover` ([`Dtb::find`], of
//! [`Dtb::keep`] voor de heap-kopie van de Radxa) en bewaard, daarna gelezen
//! voor de bootargs, het framebuffer en de virtio-lijst ([`Dtb::fdt`]).

use crate::Region;
use core::sync::atomic::AtomicUsize;
use core::sync::atomic::Ordering::Relaxed;
use dev::Pa;
use fw::fdt::Fdt;

/// Een blob die blijft (`&'static [u8]`), bewaard in twee atomics: het
/// adres en de lengte, 0 = geen. Eén schrijver, bij de boot.
#[derive(Default)]
pub struct Kept([AtomicUsize; 2]);

impl Kept {
    /// Nog niets bewaard.
    #[must_use]
    pub const fn new() -> Self {
        Self([const { AtomicUsize::new(0) }; 2])
    }

    /// Bewaart `b`.
    pub fn keep(&self, b: &'static [u8]) {
        self.0[1].store(b.len(), Relaxed);
        self.0[0].store(b.as_ptr() as usize, Relaxed);
    }

    /// Wat [`Kept::keep`] bewaarde.
    #[must_use]
    pub fn get(&self) -> Option<&'static [u8]> {
        let (p, n) = (self.0[0].load(Relaxed), self.0[1].load(Relaxed));
        if p == 0 {
            return None;
        }
        // SAFETY: `p` en `n` komen uit een `&'static [u8]` die `keep` kreeg
        // (de enige schrijver, bij de boot vóór elke lezer): die slice leeft
        // en verandert niet zolang het programma draait.
        Some(unsafe { core::slice::from_raw_parts(p as *const u8, n) })
    }
}

/// De DTB van een board.
#[derive(Default)]
pub struct Dtb(Kept);

impl Dtb {
    /// Nog geen DTB.
    #[must_use]
    pub const fn new() -> Self {
        Self(Kept::new())
    }

    /// Bewaart `blob` als het een geldige DTB is.
    pub fn keep(&self, blob: &'static [u8]) -> Option<Fdt<'static>> {
        let f = Fdt::new(blob).ok()?;
        self.0.keep(blob);
        Some(f)
    }

    /// De DTB op `pa`, als er een geldige header staat en de hele blob in
    /// `ram` ligt: gevonden en bewaard. `ram` komt uit het plan van het
    /// board (zoals elk adres naar `dev`): Normal gemapt, en na de boot
    /// schrijft niemand erin (de DTB ligt buiten de heap en de pool).
    pub fn find(&self, pa: u64, ram: Region) -> Option<Fdt<'static>> {
        if pa == 0 || !ram.contains(Pa(pa)) || !pa.is_multiple_of(8) {
            return None;
        }
        let mut head = [0u8; 8];
        dev::copy_out(&mut head, Pa(pa));
        let total = fw::fdt::total_size(&head)?;
        if !ram.contains(Pa(pa).add(total as u64 - 1)) {
            return None;
        }
        // SAFETY: `[pa, pa+total)` ligt in `ram` (hierboven getoetst): RAM
        // dat het board Normal mapt en waar na de boot niemand in schrijft
        // (de voorwaarde van `find`). Alleen lezen.
        let blob = unsafe { core::slice::from_raw_parts(pa as usize as *const u8, total) };
        self.keep(blob)
    }

    /// De bewaarde DTB.
    #[must_use]
    pub fn fdt(&self) -> Option<Fdt<'static>> {
        Fdt::new(self.0.get()?).ok()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::vec::Vec;

    /// Een kale DTB van 64 bytes: het magic, de maat, drie offsets erin.
    fn blob() -> Vec<u8> {
        let mut b = std::vec![0u8; 64];
        for (off, v) in [(0, 0xd00d_feedu32), (4, 64), (8, 40), (12, 60), (16, 48)] {
            b[off..off + 4].copy_from_slice(&v.to_be_bytes());
        }
        b
    }

    #[test]
    fn the_dtb_is_found_once_and_kept() {
        // 256 bytes RAM op 8 bytes uitgelijnd, de blob op +64.
        let mut words = std::vec![0u64; 8];
        words.extend(
            blob()
                .chunks(8)
                .map(|w| u64::from_ne_bytes(w.try_into().unwrap())),
        );
        words.resize(32, 0);
        let at = words.leak().as_ptr() as u64;
        let ram = Region {
            base: Pa(at),
            size: 256,
        };
        let d = Dtb::new();
        assert!(d.fdt().is_none(), "nog niets");
        assert!(d.find(0, ram).is_none());
        assert!(d.find(at + 4, ram).is_none(), "niet op 8 bytes");
        assert!(d.find(at + 256, ram).is_none(), "buiten het RAM");
        assert!(d.find(at, ram).is_none(), "geen magic");
        // Een blob die over het eind van het RAM loopt, telt niet.
        let short = Region {
            base: Pa(at),
            size: 64 + 63,
        };
        assert!(d.find(at + 64, short).is_none());
        assert!(d.fdt().is_none());
        let f = d.find(at + 64, ram).unwrap();
        assert_eq!(f.size(), 64);
        assert_eq!(d.fdt().unwrap().size(), 64);
        // Een kopie (de Radxa) bewaart zich ook; een kromme niet.
        let copy: &'static [u8] = blob().leak();
        assert!(d.keep(&copy[..8]).is_none());
        assert_eq!(d.fdt().unwrap().size(), 64);
        assert!(d.keep(copy).is_some());
        assert_eq!(d.fdt().unwrap().size(), 64);
    }
}
