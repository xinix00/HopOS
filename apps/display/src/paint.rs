//! Het tekenwerk op het glas: pixels, vlakken en tekst in het formaat van
//! de grant ([`Glass`]), met het font van de console van de kern.
//!
//! Bezit niets dan de beschrijving van het glas; de pixels zelf zijn van
//! de taak die de [`Painter`] houdt (de tekentaak van main.rs, de enige).
//! Elke schrijf is vluchtig (`dev`): het venster is Normal-NC (of Device
//! als de stage-1-map geweigerd werd), dus zonder cache staat elke store
//! meteen op het glas.

use applib::fb::Glass;
use dev::Pa;

/// De tekenaar op één glas.
///
/// # Invariants
///
/// `g` is door [`Glass::from_env`] getoetst: elke pixel binnen `width` x
/// `height` ligt binnen `[g.base, g.base + g.size())`.
pub(crate) struct Painter {
    g: Glass,
}

impl Painter {
    /// Een tekenaar op `g`.
    pub(crate) fn new(g: Glass) -> Self {
        // INVARIANT: `Glass` bestaat alleen getoetst (`from_env`), of in de
        // tests met een geometrie die in hun buffer past.
        Self { g }
    }

    /// Het glas.
    pub(crate) fn glass(&self) -> Glass {
        self.g
    }

    /// Het adres van pixel `(x, y)`, of `None` buiten het glas.
    fn at(&self, x: u32, y: u32) -> Option<Pa> {
        if x >= self.g.width || y >= self.g.height {
            return None;
        }
        let off = u64::from(y) * u64::from(self.g.stride) + u64::from(x) * u64::from(self.g.bpx());
        Some(Pa(self.g.base + off))
    }

    /// Het rauwe pixelwoord op `(x, y)` (0 buiten het glas).
    pub(crate) fn raw(&self, x: u32, y: u32) -> u32 {
        match (self.at(x, y), self.g.bpp) {
            (Some(pa), 16) => u32::from(dev::read16(pa)),
            (Some(pa), _) => dev::read32(pa),
            (None, _) => 0,
        }
    }

    /// Schrijft een rauw pixelwoord (zoals [`Painter::raw`] het gaf).
    pub(crate) fn put_raw(&self, x: u32, y: u32, v: u32) {
        match (self.at(x, y), self.g.bpp) {
            (Some(pa), 16) => dev::write16(pa, v as u16),
            (Some(pa), _) => dev::write32(pa, v),
            (None, _) => {}
        }
    }

    /// Eén pixel in `rgb` (0x00RRGGBB).
    pub(crate) fn put(&self, x: u32, y: u32, rgb: u32) {
        self.put_raw(x, y, self.g.encode(rgb));
    }

    /// Een rechthoek in `rgb`, geknipt op het glas. Op 32 bpp twee pixels
    /// per store waar de rij het toelaat: een vol 1080p-scherm is dan een
    /// miljoen stores in plaats van twee.
    pub(crate) fn fill(&self, x: u32, y: u32, w: u32, h: u32, rgb: u32) {
        let x1 = x.saturating_add(w).min(self.g.width);
        let y1 = y.saturating_add(h).min(self.g.height);
        let v = self.g.encode(rgb);
        for py in y..y1 {
            let mut px = x;
            while px < x1 {
                if self.g.bpp == 32
                    && px + 1 < x1
                    && let Some(pa) = self.at(px, py).filter(|pa| pa.is_aligned(8))
                {
                    dev::write64(pa, u64::from(v) << 32 | u64::from(v));
                    px += 2;
                    continue;
                }
                self.put_raw(px, py, v);
                px += 1;
            }
        }
    }

    /// Tekst op `(x, y)` in cellen van `8 * scale` pixels, voorgrond `fg`
    /// op achtergrond `bg`; geeft de breedte in pixels. Buiten ASCII een
    /// `?` (het font van de console).
    pub(crate) fn text(&self, x: u32, y: u32, scale: u32, s: &[u8], fg: u32, bg: u32) -> u32 {
        let scale = scale.max(1);
        let (fg, bg) = (self.g.encode(fg), self.g.encode(bg));
        let cell = 8 * scale;
        for (i, &c) in s.iter().enumerate() {
            let cx = x.saturating_add((i as u32).saturating_mul(cell));
            let glyph = driver_fb::glyph(c);
            for (row, bits) in glyph.iter().enumerate() {
                for col in 0..8u32 {
                    let on = bits >> col & 1 != 0;
                    let v = if on { fg } else { bg };
                    for dy in 0..scale {
                        for dx in 0..scale {
                            self.put_raw(cx + col * scale + dx, y + row as u32 * scale + dy, v);
                        }
                    }
                }
            }
        }
        (s.len() as u32).saturating_mul(cell)
    }
}

/// De maat van de cursor in pixels.
pub(crate) const CURSOR: u32 = 13;

/// De cursor met wat er onder hem stond, zodat hij kan bewegen zonder het
/// beeld eronder kapot te maken.
pub(crate) struct Cursor {
    /// Waar hij staat (linksboven), als hij staat.
    at: Option<(u32, u32)>,
    /// Het rauwe beeld onder hem.
    under: [u32; (CURSOR * CURSOR) as usize],
}

impl Default for Cursor {
    fn default() -> Self {
        Self {
            at: None,
            under: [0; (CURSOR * CURSOR) as usize],
        }
    }
}

impl Cursor {
    /// Haalt hem weg: het bewaarde beeld terug.
    pub(crate) fn hide(&mut self, p: &Painter) {
        let Some((x, y)) = self.at.take() else {
            return;
        };
        for (i, v) in self.under.iter().enumerate() {
            let i = i as u32;
            p.put_raw(x + i % CURSOR, y + i / CURSOR, *v);
        }
    }

    /// Zet hem met zijn punt op `(x, y)`: eerst het beeld eronder bewaren,
    /// dan een pijlkop in `rgb` met een donkere rand.
    pub(crate) fn show(&mut self, p: &Painter, x: u32, y: u32, rgb: u32, edge: u32) {
        self.hide(p);
        for (i, v) in self.under.iter_mut().enumerate() {
            let i = i as u32;
            *v = p.raw(x + i % CURSOR, y + i / CURSOR);
        }
        for dy in 0..CURSOR {
            // Een driehoek: rij dy is dy + 1 breed, met een rand.
            for dx in 0..=dy.min(CURSOR - 1) {
                let border = dx == 0 || dx == dy || dy == CURSOR - 1;
                p.put(x + dx, y + dy, if border { edge } else { rgb });
            }
        }
        self.at = Some((x, y));
    }

    /// Waar hij staat.
    #[cfg(test)]
    pub(crate) fn at(&self) -> Option<(u32, u32)> {
        self.at
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::vec;
    use std::vec::Vec;

    /// Een glas in een vector: `dev` schrijft op de host naar gewoon
    /// geheugen.
    fn glass(mem: &[u64], w: u32, h: u32, bpp: u32) -> Glass {
        Glass {
            base: mem.as_ptr() as u64,
            width: w,
            height: h,
            stride: w * bpp / 8,
            bpp,
            swap: false,
        }
    }

    #[test]
    fn fill_and_read_back() {
        let mem: Vec<u64> = vec![0; 64 * 16 * 4 / 8];
        let p = Painter::new(glass(&mem, 64, 16, 32));
        p.fill(3, 2, 5, 4, 0x0011_2233);
        assert_eq!(p.raw(3, 2), 0x0011_2233);
        assert_eq!(p.raw(7, 5), 0x0011_2233);
        assert_eq!(p.raw(8, 5), 0);
        assert_eq!(p.raw(3, 6), 0);
        // Geknipt op het glas, geen schrijf erbuiten.
        p.fill(60, 14, 100, 100, 0x00ff_ffff);
        assert_eq!(p.raw(63, 15), 0x00ff_ffff);
        assert_eq!(p.raw(64, 15), 0);
    }

    #[test]
    fn text_uses_the_console_font() {
        let mem: Vec<u64> = vec![0; 64 * 16 * 4 / 8];
        let p = Painter::new(glass(&mem, 64, 16, 32));
        let w = p.text(0, 0, 1, b"H", 0x00ff_ffff, 0x0000_0001);
        assert_eq!(w, 8);
        let lit = (0..8)
            .flat_map(|y| (0..8).map(move |x| (x, y)))
            .filter(|&(x, y)| p.raw(x, y) == 0x00ff_ffff)
            .count();
        let glyph = driver_fb::glyph(b'H');
        let want: u32 = glyph.iter().map(|r| r.count_ones()).sum();
        assert_eq!(lit as u32, want);
        assert_eq!(p.raw(8, 0), 0, "the cell ends at 8 pixels");
    }

    #[test]
    fn the_cursor_leaves_the_picture_as_it_was() {
        let mem: Vec<u64> = vec![0; 64 * 32 * 2 / 8];
        let p = Painter::new(glass(&mem, 64, 32, 16));
        p.fill(0, 0, 64, 32, 0x0012_3456);
        let before: Vec<u32> = (0..32 * 64).map(|i| p.raw(i % 64, i / 64)).collect();
        let mut c = Cursor::default();
        c.show(&p, 10, 10, 0x00ff_5030, 0);
        assert_ne!(p.raw(10, 10), before[10 * 64 + 10]);
        c.show(&p, 20, 12, 0x00ff_5030, 0);
        assert_eq!(p.raw(10, 10), before[10 * 64 + 10], "old place restored");
        c.hide(&p);
        let after: Vec<u32> = (0..32 * 64).map(|i| p.raw(i % 64, i / 64)).collect();
        assert_eq!(before, after);
        assert_eq!(c.at(), None);
    }
}
