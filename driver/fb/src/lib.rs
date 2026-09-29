//! De tekst-console op een lineaire framebuffer: het beeldkanaal voor
//! iedereen zonder debug-kabel.
//!
//! BEWUST GEEN display-driver. De console zet geen controller op en doet
//! geen mode-setting; hij schrijft pixels in een beeld dat al loopt. Wie dat
//! beeld aanzette, is van het board: de firmware (UEFI GOP, de
//! VideoCore-mailbox van de Pi), QEMU (`ramfb` via fw_cfg), of op de Radxa
//! onze eigen scanout-keten (`gui-rkscan`). Hier komt alleen een [`Desc`]
//! binnen: adres, maat, stride en pixelformaat.
//!
//! Eigendom: één [`Console`], met `&mut self` op elke schrijf. De binary
//! houdt hem in een `LocalCell` op de OS-core en de console-haak leent hem
//! per regel (onder het console-slot van `cpu`, het tweede van de twee
//! sloten uit het handboek §1.3); de meettaak leent hem voor
//! [`Console::header_status`]. De Go-versie was een pakket vol globals en
//! "lock-vrij omdat header en log andere pixels raken"; hier is dat een
//! eigenaar en geen afspraak.
//!
//! Geen allocatie, nergens: een regel is bytes in, pixels uit. De buffer
//! ligt buiten de eigen RAM en is dus Device of (na `memattr::normal_nc`,
//! dat doet de binary) Normal-NC: elke store staat meteen op het glas,
//! zonder cache-onderhoud.
//!
//! De log wrapt: onderaan springt de cursor terug naar de eerste rij onder
//! de kop en veegt de rij die hij gaat beschrijven. Echt scrollen zou elke
//! regel het hele scherm opnieuw tekenen (1080p: twee miljoen stores per
//! regel op ongecached geheugen); de wrap kost één rij (Go deed hetzelfde,
//! 11-07).

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

mod font;

use core::fmt;
use dev::Pa;
use font::FONT8X8;

/// Een lineaire framebuffer: adres en geometrie.
///
/// Het board vult hem uit GOP (UEFI), de mailbox (Pi), fw_cfg (QEMU
/// `ramfb`) of zijn eigen scanout (RK3566).
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub struct Desc {
    /// Het fysieke adres van de eerste pixel.
    pub base: Pa,
    /// De breedte in pixels.
    pub width: u32,
    /// De hoogte in pixels.
    pub height: u32,
    /// Bytes per pixelrij.
    pub stride: u32,
    /// Bits per pixel: 32 (x8r8g8b8) of 16 (r5g6b5).
    pub bpp: u8,
    /// Rood en blauw wisselen: GOP-pixelformaat 0 (RGB) in plaats van de
    /// gangbare BGR. Anders staat alles blauw waar rood hoort.
    pub swap_rb: bool,
}

impl Desc {
    /// De maat van de buffer in bytes (`stride * height`), of `None` bij
    /// een geometrie die overloopt.
    #[must_use]
    pub fn size(&self) -> Option<u64> {
        u64::from(self.stride).checked_mul(u64::from(self.height))
    }

    /// Toetst de geometrie en geeft de bytes per pixel.
    ///
    /// Een lege, onbegrepen of overlopende descriptor is een weigering met
    /// de getallen erin: de console blijft dan uit (geen scherm is geen
    /// fout).
    pub fn check(&self) -> Result<u32> {
        if self.base.0 == 0 || self.width == 0 || self.height == 0 || self.stride == 0 {
            return Err(Error::Empty);
        }
        let bpx = match self.bpp {
            32 => 4,
            16 => 2,
            bpp => return Err(Error::Bpp { bpp }),
        };
        if u64::from(self.width) * u64::from(bpx) > u64::from(self.stride) {
            return Err(Error::Stride {
                width: self.width,
                stride: self.stride,
            });
        }
        let size = self.size().ok_or(Error::Overflow {
            base: self.base.0,
            size: u64::MAX,
        })?;
        if self.base.0.checked_add(size).is_none() || !self.base.is_aligned(u64::from(bpx)) {
            return Err(Error::Overflow {
                base: self.base.0,
                size,
            });
        }
        Ok(bpx)
    }
}

/// Waarom een descriptor geweigerd wordt.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// Adres, breedte, hoogte of stride is nul.
    Empty,
    /// Een pixelformaat dat de console niet tekent.
    Bpp {
        /// De bits per pixel die het board meldde.
        bpp: u8,
    },
    /// Een rij past niet in de stride.
    Stride {
        /// De breedte in pixels.
        width: u32,
        /// De stride in bytes.
        stride: u32,
    },
    /// De buffer loopt over het einde van de adresruimte, of het adres is
    /// niet op een pixel gealigneerd.
    Overflow {
        /// Het beginadres.
        base: u64,
        /// De maat in bytes.
        size: u64,
    },
    /// Het scherm is kleiner dan één tekencel.
    TooSmall,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Empty => f.write_str("framebuffer descriptor is empty"),
            Self::Bpp { bpp } => write!(f, "{bpp} bpp is not drawable (32 or 16)"),
            Self::Stride { width, stride } => {
                write!(f, "width {width} does not fit stride {stride}")
            }
            Self::Overflow { base, size } => {
                write!(
                    f,
                    "framebuffer {base:#x}+{size:#x} overflows or is unaligned"
                )
            }
            Self::TooSmall => f.write_str("framebuffer smaller than one character cell"),
        }
    }
}

/// Het resultaat van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// De voorgrond: wit.
const FG: u32 = 0xFFFF_FFFF;
/// De achtergrond: donker blauwgrijs, zodat "beeld doet het" iets anders is
/// dan een zwart scherm.
const BG: u32 = 0xFF10_1828;
/// De breedte van een statusveld rechts in de kop, in tekens. Dekt
/// "mem 100% (128/128MB)" ruim (Derek, 15-07).
pub const STATUS_WIDTH: usize = 26;
/// Vanaf deze hoogte is het een echt scherm en worden de cellen 16x16:
/// 120x67 tekens op 1080p (Derek, 11-07: 2x beviel). Kleinere buffers
/// (zelftests, `ramfb` op 640x480) krijgen de rauwe 8x8-glyphs.
pub const SCALE2_FROM: u32 = 720;

/// De console op één framebuffer.
///
/// # Invariants
///
/// Is `active`, dan heeft [`Desc::check`] `d` goedgekeurd, is `bpx` wat hij
/// gaf, en liggen `x < cols`, `y < rows`, `top < rows`: elke pixel die de
/// console schrijft, ligt binnen `[d.base, d.base + d.stride * d.height)`.
#[derive(Debug)]
pub struct Console {
    d: Desc,
    bpx: u32,
    scale: u32,
    cols: u32,
    rows: u32,
    x: u32,
    y: u32,
    top: u32,
    active: bool,
}

impl Default for Console {
    fn default() -> Self {
        Self::new()
    }
}

impl Console {
    /// Een console zonder scherm: elke schrijf is een no-op.
    #[must_use]
    pub const fn new() -> Self {
        Console {
            d: Desc {
                base: Pa(0),
                width: 0,
                height: 0,
                stride: 0,
                bpp: 0,
                swap_rb: false,
            },
            bpx: 0,
            scale: 1,
            cols: 0,
            rows: 0,
            x: 0,
            y: 0,
            top: 0,
            active: false,
        }
    }

    /// Neemt een framebuffer in gebruik, veegt hem schoon en zet de console
    /// aan. Een tweede `init` begint opnieuw (de grant geeft het glas zo
    /// terug: schone lei). Bij een weigering blijft de console uit.
    pub fn init(&mut self, d: Desc) -> Result {
        self.active = false;
        let bpx = d.check()?;
        let scale = if d.height >= SCALE2_FROM { 2 } else { 1 };
        let cell = 8 * scale;
        let (cols, rows) = (d.width / cell, d.height / cell);
        if cols == 0 || rows == 0 {
            return Err(Error::TooSmall);
        }
        // INVARIANT: `check` keurde `d` goed, cursor en kop staan op nul.
        *self = Console {
            d,
            bpx,
            scale,
            cols,
            rows,
            x: 0,
            y: 0,
            top: 0,
            active: true,
        };
        for py in 0..d.height {
            self.fill_row(py, BG);
        }
        Ok(())
    }

    /// Staat de console op het glas?
    #[must_use]
    pub fn is_active(&self) -> bool {
        self.active
    }

    /// De descriptor waar de console op tekent (ook als hij uit staat).
    #[must_use]
    pub fn desc(&self) -> Desc {
        self.d
    }

    /// De maat in tekencellen: (kolommen, rijen).
    #[must_use]
    pub fn cells(&self) -> (u32, u32) {
        (self.cols, self.rows)
    }

    /// Haalt de console van het glas: daarna is elke schrijf een no-op tot
    /// een nieuwe [`init`](Self::init). Raakt geen pixel: wie het glas nu
    /// krijgt, tekent er zelf overheen.
    pub fn disable(&mut self) {
        self.active = false;
    }

    /// Tekent vaste regels bovenaan die nooit meescrollen, zoals Linux zijn
    /// logo bovenin laat staan (Dereks bunny, 11-07). De log begint en wrapt
    /// voortaan onder de kop. Hoogstens `rows - 1` regels.
    pub fn header(&mut self, lines: &[&str]) {
        if !self.active {
            return;
        }
        let n = u32::try_from(lines.len())
            .unwrap_or(u32::MAX)
            .min(self.rows - 1);
        for (row, line) in (0..n).zip(lines) {
            for (col, &c) in (0..self.cols).zip(line.as_bytes()) {
                self.glyph(col, row, printable(c));
            }
        }
        self.top = n;
        self.x = 0;
        self.y = n;
    }

    /// Tekent één statusveld rechts op kopregel `line`, rechts uitgelijnd
    /// tegen de schermrand: de live meetregels naast de bunny (Derek, 15-07:
    /// mem, datum, tijd). Het veld is [`STATUS_WIDTH`] breed, zodat een
    /// oudere, langere tekst altijd overschreven wordt. Schrijft alleen in
    /// de kop, nooit door de log heen.
    pub fn header_status(&mut self, line: u32, s: &str) {
        if !self.active || line >= self.top {
            return;
        }
        let s = s.as_bytes();
        let s = s.get(..STATUS_WIDTH).unwrap_or(s);
        let w = STATUS_WIDTH as u32;
        let start = self.cols.saturating_sub(w);
        let pad = STATUS_WIDTH - s.len();
        for j in 0..STATUS_WIDTH {
            let col = start + j as u32;
            if col >= self.cols {
                break;
            }
            let c = j.checked_sub(pad).and_then(|k| s.get(k)).copied();
            self.glyph(col, line, printable(c.unwrap_or(b' ')));
        }
    }

    /// Schrijft bytes naar de log.
    pub fn write(&mut self, bytes: &[u8]) {
        for &c in bytes {
            self.putc(c);
        }
    }

    /// Tekent één byte. UTF-8 wordt gedegradeerd: vervolgbytes vallen weg,
    /// de leadbyte wordt `?`. De UART houdt de volle tekst.
    pub fn putc(&mut self, c: u8) {
        if !self.active {
            return;
        }
        let c = match c {
            b'\n' => return self.newline(),
            b'\r' => {
                self.x = 0;
                return;
            }
            b'\t' => {
                self.putc(b' ');
                return self.putc(b' ');
            }
            0x80..=0xBF => return, // UTF-8-vervolgbyte
            0xC0..=0xFF => b'?',   // UTF-8-leadbyte
            0..=0x1F | 0x7F => return,
            c => c,
        };
        if self.x >= self.cols {
            self.newline();
        }
        self.glyph(self.x, self.y, c);
        self.x += 1;
    }

    /// Schuift de cursor een regel op; onderaan wrapt hij naar de eerste
    /// logrij onder de kop. De doelregel wordt eerst geveegd, zodat oude
    /// tekst nooit door nieuwe heen schemert.
    fn newline(&mut self) {
        self.x = 0;
        self.y += 1;
        if self.y >= self.rows {
            self.y = self.top;
        }
        let cell = 8 * self.scale;
        for py in self.y * cell..(self.y + 1) * cell {
            self.fill_row(py, BG);
        }
    }

    /// Tekent glyph `c` op cel `(cx, cy)`, met `scale x scale` pixels per
    /// fontbit.
    fn glyph(&mut self, cx: u32, cy: u32, c: u8) {
        let Some(rows) = FONT8X8.get(usize::from(c)) else {
            return;
        };
        let cell = 8 * self.scale;
        let (px0, py0) = (cx * cell, cy * cell);
        for (gy, &bits) in (0u32..).zip(rows) {
            for sy in 0..self.scale {
                let py = py0 + gy * self.scale + sy;
                for gx in 0..8 {
                    let col = if bits >> gx & 1 != 0 { FG } else { BG };
                    for sx in 0..self.scale {
                        self.put(px0 + gx * self.scale + sx, py, col);
                    }
                }
            }
        }
    }

    /// Veegt pixelrij `py` in kleur `argb`. Op 32 bpp met een op 8
    /// gealigneerde rij gaan er twee pixels per store (de helft van de
    /// transacties op Device-geheugen).
    fn fill_row(&mut self, py: u32, argb: u32) {
        if py >= self.d.height {
            return;
        }
        let row = self.d.base.add(u64::from(py) * u64::from(self.d.stride));
        if self.bpx == 4 && row.is_aligned(8) {
            let v = self.encode(argb);
            let pair = u64::from(v) << 32 | u64::from(v);
            let pairs = self.d.width / 2;
            for i in 0..pairs {
                dev::write64(row.add(u64::from(i) * 8), pair);
            }
            if self.d.width % 2 == 1 {
                self.put(self.d.width - 1, py, argb);
            }
            return;
        }
        for px in 0..self.d.width {
            self.put(px, py, argb);
        }
    }

    /// Het pixelwoord voor `argb` (0xAARRGGBB) in het formaat van het
    /// scherm.
    fn encode(&self, argb: u32) -> u32 {
        let argb = if self.d.swap_rb {
            argb & 0xFF00_FF00 | (argb & 0xFF) << 16 | (argb >> 16) & 0xFF
        } else {
            argb
        };
        if self.bpx == 2 {
            let (r, g, b) = ((argb >> 16) & 0xFF, (argb >> 8) & 0xFF, argb & 0xFF);
            return (r >> 3) << 11 | (g >> 2) << 5 | (b >> 3);
        }
        argb
    }

    /// Schrijft één pixel. Buiten het scherm is een no-op (de invariant
    /// zegt dat het niet gebeurt; de toets kost niets).
    fn put(&mut self, px: u32, py: u32, argb: u32) {
        if !self.active || px >= self.d.width || py >= self.d.height {
            return;
        }
        let off = u64::from(py) * u64::from(self.d.stride) + u64::from(px) * u64::from(self.bpx);
        let pa = self.d.base.add(off);
        let v = self.encode(argb);
        if self.bpx == 2 {
            dev::write16(pa, v as u16);
        } else {
            dev::write32(pa, v);
        }
    }
}

/// Een teken dat de console kan tekenen, of `?`.
fn printable(c: u8) -> u8 {
    if (0x20..0x80).contains(&c) { c } else { b'?' }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Een nep-scherm op de host: `dev` schrijft vluchtig naar dit geheugen.
    struct Screen {
        mem: Vec<u64>,
        w: u32,
        h: u32,
        stride: u32,
    }

    impl Screen {
        fn new(w: u32, h: u32) -> Screen {
            let stride = w * 4;
            Screen {
                mem: vec![0; (stride * h) as usize / 8 + 1],
                w,
                h,
                stride,
            }
        }
        fn desc(&self) -> Desc {
            Desc {
                base: Pa(self.mem.as_ptr() as u64),
                width: self.w,
                height: self.h,
                stride: self.stride,
                bpp: 32,
                swap_rb: false,
            }
        }
        fn px(&self, x: u32, y: u32) -> u32 {
            dev::read32(self.desc().base.add(u64::from(y * self.stride + x * 4)))
        }
        fn snapshot(&self) -> Vec<u64> {
            self.mem.clone()
        }
        /// Heeft cel `(cx, cy)` (8x8, schaal 1) een voorgrondpixel?
        fn inked(&self, cx: u32, cy: u32) -> bool {
            (0..8).any(|y| (0..8).any(|x| self.px(cx * 8 + x, cy * 8 + y) == FG))
        }
    }

    // Go: TestInvalidFramebufferCannotRemainActive.
    #[test]
    fn invalid_framebuffer_cannot_remain_active() {
        let s = Screen::new(64, 16);
        let mut c = Console::new();
        c.init(s.desc()).unwrap();
        assert!(c.is_active());
        c.header(&["header"]);
        let before = s.snapshot();
        c.header_status(u32::MAX, "bad");
        c.header_status(1, "not a header row");
        assert_eq!(s.snapshot(), before, "a non-header row wrote pixels");
        let bad = [
            Desc::default(),
            Desc {
                base: Pa(8),
                width: 64,
                height: 16,
                stride: 4,
                bpp: 32,
                swap_rb: false,
            },
            Desc {
                base: Pa(u64::MAX - 8),
                width: 64,
                height: 16,
                stride: 256,
                bpp: 32,
                swap_rb: false,
            },
            Desc {
                bpp: 24,
                ..s.desc()
            },
            Desc {
                width: 4,
                height: 4,
                stride: 16,
                ..s.desc()
            },
        ];
        for d in bad {
            assert!(c.init(d).is_err(), "{d:?}");
            assert!(!c.is_active(), "invalid framebuffer active: {d:?}");
        }
        // Uit is uit: niets raakt het glas.
        let before = s.snapshot();
        c.write(b"hello\n");
        assert_eq!(s.snapshot(), before);
    }

    #[test]
    fn init_paints_the_background_and_header_pins_the_log() {
        let s = Screen::new(64, 32); // 8x4 cellen
        let mut c = Console::new();
        c.init(s.desc()).unwrap();
        assert_eq!(c.cells(), (8, 4));
        assert_eq!(s.px(0, 0), BG);
        assert_eq!(s.px(63, 31), BG);
        c.header(&["HopOS"]);
        assert!(s.inked(0, 0), "header drawn");
        c.write(b"ab");
        assert!(
            s.inked(0, 1) && s.inked(1, 1),
            "log starts under the header"
        );
        // Twee regels verder wrapt de log naar rij 1 en veegt die eerst.
        c.write(b"\nc\nd\n");
        assert!(!s.inked(0, 1), "wrapped row cleared");
        assert!(s.inked(0, 0), "the header never scrolls");
        c.write(b"e");
        assert!(s.inked(0, 1));
    }

    #[test]
    fn long_lines_wrap_and_utf8_degrades() {
        let s = Screen::new(32, 24); // 4x3 cellen
        let mut c = Console::new();
        c.init(s.desc()).unwrap();
        c.write("abcdé".as_bytes());
        // Vier tekens, dan wrapt de regel; é wordt één `?`.
        assert!(s.inked(3, 0));
        assert!(s.inked(0, 1), "the lead byte is one `?`");
        assert!(!s.inked(1, 1), "a continuation byte drew a cell");
    }

    #[test]
    fn header_status_is_right_aligned_and_fixed_width() {
        let s = Screen::new(8 * 40, 8 * 4);
        let mut c = Console::new();
        c.init(s.desc()).unwrap();
        c.header(&["bunny", "bunny", "bunny"]);
        c.header_status(0, "12:00:00");
        // Rechts uitgelijnd: de laatste cel is de laatste 0.
        assert!(s.inked(39, 0));
        assert!(!s.inked(39 - 8, 0), "padding must be blank");
        c.header_status(0, "1");
        assert!(!s.inked(38, 0), "a shorter value overwrites the old one");
        assert!(s.inked(39, 0));
        // De log blijft onder de kop.
        c.write(b"x");
        assert!(s.inked(0, 3));
    }

    #[test]
    fn scale_and_pixel_formats() {
        // Een echt scherm krijgt 16x16-cellen.
        let s = Screen::new(1280, 720);
        let mut c = Console::new();
        c.init(s.desc()).unwrap();
        assert_eq!(c.cells(), (80, 45));
        // RGB in plaats van BGR: rood en blauw ruilen.
        let s = Screen::new(16, 8);
        let mut c = Console::new();
        c.init(Desc {
            swap_rb: true,
            ..s.desc()
        })
        .unwrap();
        assert_eq!(s.px(0, 0), 0xFF28_1810);
        // 16 bpp: r5g6b5.
        let s = Screen::new(16, 8);
        let mut c = Console::new();
        c.init(Desc {
            bpp: 16,
            ..s.desc()
        })
        .unwrap();
        let bg565 = (0x10 >> 3) << 11 | (0x18 >> 2) << 5 | (0x28 >> 3);
        assert_eq!(dev::read16(s.desc().base), bg565 as u16);
    }

    #[test]
    fn disable_then_init_gives_a_clean_slate() {
        let s = Screen::new(64, 16);
        let mut c = Console::new();
        c.init(s.desc()).unwrap();
        c.write(b"hi");
        c.disable();
        // Wie het glas kreeg, tekent eroverheen.
        dev::write32(s.desc().base, 0x1234_5678);
        c.write(b"lost");
        assert_eq!(s.px(0, 0), 0x1234_5678);
        c.init(s.desc()).unwrap();
        assert_eq!(s.px(0, 0), BG);
        assert!(!s.inked(0, 0));
    }
}
