//! Het glas en de invoer van de display-app: wat een app nodig heeft die
//! de framebuffer-grant van de kern houdt (docs/gui.md, "De display-app").
//!
//! Drie delen, en dit module bezit niets dan de stage-1-tabellen die het
//! zelf legt:
//!
//! - [`Glass`]: de `FB_*`-sleutels uit de env (`FB_BASE`, `FB_WIDTH`,
//!   `FB_HEIGHT`, `FB_STRIDE`, `FB_BPP`, `FB_SWAP`), getoetst voordat er
//!   één pixel geschreven wordt: een stride kleiner dan een rij of een
//!   diepte anders dan 16 of 32 is een weigering, geen scheve streep.
//! - [`map`]: het venster in de eigen stage-1 als **Normal-NC**, op 4 KB.
//!   Een app draait tot hier met de MMU uit, en dan is elke store Device:
//!   een 1080p-frame is een miljoen losse, geordende transacties. Normal-NC
//!   is wat Linux een framebuffer geeft (write-combine): geen cache, dus
//!   de scanout ziet elke store zonder onderhoud, maar het fabric mag
//!   gatheren (Go `cpu/memattr`, 04-08). `FB_BASE` staat niet op 2 MB
//!   (het IPA is `0x2000_0000` plus de offset in het blok), dus de randen
//!   gaan op 4 KB-pagina's.
//! - [`LineReader`] en [`Input`]: de stroom van `INPUT_ADDR`, één JSON-event
//!   per regel (`{"k":"key",..}`, `move`, `btn`, `wheel`), precies wat
//!   `POST /input` van de browser-KVM aanneemt; een lege regel is een
//!   keepalive. De lezer alloceert niet en een te lange regel valt weg in
//!   plaats van de volgende mee te nemen.
//!
//! De stage-1 van [`map`]: een identiteitsmap (VA = IPA) in de eerste 64 KB
//! van het linkvenster, de ruimte die de ABI daarvoor openhoudt
//! (`abi::layout::LINK_TEXT_OFF`). De RAM-declaratie wordt Normal
//! write-back, de ABI-staart Device-nGnRnE (de ringen en de control-page
//! houden de semantiek die ze met de MMU uit hadden), het venster
//! Normal-NC. SCTLR.C blijft uit: elke data-toegang naar Normal-geheugen,
//! ook de tabelwandeling, is dan Non-cacheable, dus de kern en de
//! EL2-switcher (die de app-RAM met de MMU uit lezen) zien alles meteen,
//! net als vóór de map. Wat de MMU hier koopt, is alleen het attribuut van
//! het glas. De switcher bewaart het EL1-regime per bewoner (19 registers,
//! `abi::layout::CTX_REGIME`), dus een yield naar Hop verliest de map niet.

use crate::App;
use core::fmt;

/// De IPA-basis van het glas in de kooi (`kern::stage2::FB_IPA`): het
/// venster staat daar plus de offset in zijn 2 MB-blok.
pub const FB_IPA: u64 = 0x2000_0000;

/// De langste invoerregel die de lezer bewaart. De langste regel van de
/// kern (`gui_usbin::deliver::LINE_MAX`) is 96 bytes.
pub const LINE_CAP: usize = 128;

/// Waarom het glas of de invoer niet te gebruiken is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum FbError {
    /// Een verplichte sleutel ontbreekt: dit slot houdt het glas niet.
    Missing(&'static str),
    /// Een sleutel die geen getal (of adres) is.
    Bad(&'static str),
    /// Een diepte die niet getekend wordt (16 of 32).
    Bpp(u32),
    /// Een rij past niet in de stride.
    Stride {
        /// De breedte in pixels.
        width: u32,
        /// De stride in bytes.
        stride: u32,
        /// Bytes per pixel.
        bpx: u32,
    },
    /// Het venster valt buiten het glasvenster van de kooi of loopt om.
    Window {
        /// `FB_BASE`.
        base: u64,
        /// `FB_STRIDE * FB_HEIGHT`.
        size: u64,
    },
    /// De stage-1 kan hier niet: een ander target, een SMP-app (de andere
    /// cores lopen zonder MMU), of een RAM-declaratie die niet op het
    /// linkvenster begint.
    NoStage1(&'static str),
    /// De tabellen passen niet in de 64 KB die de ABI ervoor openhoudt.
    Tables,
}

impl fmt::Display for FbError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Missing(k) => write!(f, "{k} missing from the env"),
            Self::Bad(k) => write!(f, "{k} is not a number or address"),
            Self::Bpp(b) => write!(f, "FB_BPP={b} is not drawable (16 or 32)"),
            Self::Stride { width, stride, bpx } => write!(
                f,
                "FB_STRIDE={stride} is smaller than FB_WIDTH={width} times {bpx} bytes"
            ),
            Self::Window { base, size } => {
                write!(f, "window {base:#x}+{size:#x} is outside the glass window")
            }
            Self::NoStage1(why) => write!(f, "no stage-1 map: {why}"),
            Self::Tables => write!(f, "stage-1 tables exceed the 64 KB under the image"),
        }
    }
}

/// Het glas zoals de grant het meegaf.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Glass {
    /// Het IPA van de eerste pixel (`FB_BASE`).
    pub base: u64,
    /// De breedte in pixels.
    pub width: u32,
    /// De hoogte in pixels.
    pub height: u32,
    /// Bytes per rij.
    pub stride: u32,
    /// 32 (x8r8g8b8) of 16 (r5g6b5).
    pub bpp: u32,
    /// Rood en blauw ruilen (GOP-formaat RGB).
    pub swap: bool,
}

impl Glass {
    /// Leest en toetst de `FB_*`-sleutels van `env` (in de app:
    /// `|k| app.env(k)`).
    pub fn from_env<'a>(env: impl Fn(&str) -> Option<&'a str>) -> Result<Self, FbError> {
        let num = |k: &'static str| -> Result<u64, FbError> {
            let v = env(k).ok_or(FbError::Missing(k))?;
            parse_u64(v).ok_or(FbError::Bad(k))
        };
        let small = |k: &'static str| -> Result<u32, FbError> {
            u32::try_from(num(k)?).map_err(|_| FbError::Bad(k))
        };
        let g = Glass {
            base: num("FB_BASE")?,
            width: small("FB_WIDTH")?,
            height: small("FB_HEIGHT")?,
            stride: small("FB_STRIDE")?,
            bpp: small("FB_BPP")?,
            swap: env("FB_SWAP") == Some("1"),
        };
        g.check()?;
        Ok(g)
    }

    /// Bytes per pixel.
    #[must_use]
    pub const fn bpx(&self) -> u32 {
        self.bpp / 8
    }

    /// De maat van het venster: `stride * height`.
    #[must_use]
    pub const fn size(&self) -> u64 {
        self.stride as u64 * self.height as u64
    }

    fn check(&self) -> Result<(), FbError> {
        if self.bpp != 16 && self.bpp != 32 {
            return Err(FbError::Bpp(self.bpp));
        }
        if self.width == 0 || self.height == 0 {
            return Err(FbError::Bad("FB_WIDTH/FB_HEIGHT"));
        }
        let row = u64::from(self.width) * u64::from(self.bpx());
        if row > u64::from(self.stride) {
            return Err(FbError::Stride {
                width: self.width,
                stride: self.stride,
                bpx: self.bpx(),
            });
        }
        // Het glas ligt in het venster van de grant: onder de canonieke
        // app-basis, boven FB_IPA, en op een pixel.
        let end = self.base.checked_add(self.size());
        if self.base < FB_IPA
            || end.is_none_or(|e| e > abi::layout::SLOTS_BASE)
            || !self.base.is_multiple_of(u64::from(self.bpx()))
        {
            return Err(FbError::Window {
                base: self.base,
                size: self.size(),
            });
        }
        Ok(())
    }

    /// Het rauwe pixelwoord voor `rgb` (0x00RRGGBB) in het formaat van dit
    /// glas: geruild bij `FB_SWAP`, r5g6b5 bij 16 bpp. Dezelfde regels als
    /// de console van de kern (`driver_fb::Desc::encode`).
    #[must_use]
    pub const fn encode(&self, rgb: u32) -> u32 {
        let rgb = if self.swap {
            rgb & 0xFF00_FF00 | (rgb & 0xFF) << 16 | (rgb >> 16) & 0xFF
        } else {
            rgb
        };
        if self.bpp == 16 {
            let (r, g, b) = ((rgb >> 16) & 0xFF, (rgb >> 8) & 0xFF, rgb & 0xFF);
            return (r >> 3) << 11 | (g >> 2) << 5 | (b >> 3);
        }
        rgb
    }
}

/// Een getal als `123` of `0x1a2b`.
fn parse_u64(s: &str) -> Option<u64> {
    let s = s.trim();
    match s.strip_prefix("0x").or_else(|| s.strip_prefix("0X")) {
        Some(h) => u64::from_str_radix(h, 16).ok(),
        None => s.parse().ok(),
    }
}

/// `INPUT_ADDR` (`10.100.0.1:7879`) als IPv4 en poort. `None` zonder
/// sleutel: dit board heeft geen werkende USB, er valt niets te bellen.
pub fn input_addr<'a>(
    env: impl Fn(&str) -> Option<&'a str>,
) -> Option<Result<([u8; 4], u16), FbError>> {
    let v = env("INPUT_ADDR")?;
    let bad = FbError::Bad("INPUT_ADDR");
    let parsed = v.trim().rsplit_once(':').and_then(|(ip, port)| {
        let ip = crate::appnet::parse_ip4(ip)?;
        let port = port.parse::<u16>().ok().filter(|p| *p != 0)?;
        Some((ip, port))
    });
    Some(parsed.ok_or(bad))
}

/// Hoe het venster op het glas staat na [`map`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Mapped {
    /// Het venster, naar buiten afgerond op 4 KB.
    pub base: u64,
    /// De maat daarvan.
    pub size: u64,
    /// Hoeveel van de 64 KB aan tabellen de map gebruikt.
    pub table_bytes: u64,
}

/// Zet de eigen stage-1 aan met het glas als Normal-NC (zie de moduledoc).
/// Eén keer, vóór de eerste pixel. Een weigering is geen reden om niet te
/// tekenen: zonder map tekent de app via Device, trager maar correct.
pub fn map(app: &App, g: &Glass) -> Result<Mapped, FbError> {
    if app.ctrl().cores() > 1 {
        return Err(FbError::NoStage1(
            "an SMP app: its other cores run without a MMU",
        ));
    }
    let plan = Plan::new(app, g)?;
    stage1::enable(&plan)
}

/// Wat de stage-1 moet dragen: RAM, staart en glas, allemaal
/// identiteit.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Plan {
    /// De RAM-declaratie `[start, end)`: image, heap, stack, en onderin de
    /// tabellen zelf.
    ram: (u64, u64),
    /// De ABI-staart `[start, end)`.
    tail: (u64, u64),
    /// Het glas `[start, end)`, op 4 KB.
    glass: (u64, u64),
}

impl Plan {
    fn new(app: &App, g: &Glass) -> Result<Plan, FbError> {
        let start = app.ram_start();
        if start != abi::layout::LINK_BASE {
            return Err(FbError::NoStage1(
                "the RAM declaration does not start at the link base",
            ));
        }
        let tail = app.tail().base().0;
        let end = start.saturating_add(app.ram_size());
        let lo = g.base & !(PAGE - 1);
        let hi = g.base.saturating_add(g.size()).saturating_add(PAGE - 1) & !(PAGE - 1);
        Ok(Plan {
            ram: (start, end.min(tail)),
            tail: (tail, tail.saturating_add(abi::layout::ABI_TAIL)),
            glass: (lo, hi),
        })
    }
}

/// Een pagina van de stage-1.
const PAGE: u64 = 0x1000;
/// De tabellen van de stage-1: alleen waar ze gebouwd worden (het
/// aarch64-target) en in de tests.
#[cfg(any(test, all(target_os = "none", target_arch = "aarch64")))]
mod tables {
    use super::{FbError, PAGE, Plan};

    /// Een blok van de tweede laag.
    pub(super) const BLOCK: u64 = 0x20_0000;
    /// Een blok van de eerste laag.
    pub(super) const GIB: u64 = 0x4000_0000;

    // De descriptorbits van een stage-1-entry (4 KB-korrel, ARM ARM D8.3).
    pub(super) const DESC_BLOCK: u64 = 0b01;
    pub(super) const DESC_TABLE: u64 = 0b11;
    pub(super) const DESC_PAGE: u64 = 0b11;
    pub(super) const ATTR_SHIFT: u64 = 2;
    /// Inner shareable.
    pub(super) const SH_INNER: u64 = 0b11 << 8;
    /// Access flag: zonder hem faultt de eerste toegang.
    pub(super) const AF: u64 = 1 << 10;
    /// Niet uitvoerbaar op EL1.
    pub(super) const PXN: u64 = 1 << 53;
    /// Niet uitvoerbaar op EL0.
    pub(super) const UXN: u64 = 1 << 54;

    /// De MAIR-indexen: Device-nGnRnE, Normal-NC, Normal write-back (MAIR_EL1
    /// in [`stage1`]).
    pub(super) const IDX_DEVICE: u64 = 0;
    pub(super) const IDX_NC: u64 = 1;
    pub(super) const IDX_WB: u64 = 2;

    /// De attributen per soort: het type-veld vult de laag zelf in.
    pub(super) const ATTR_RAM: u64 = IDX_WB << ATTR_SHIFT | SH_INNER | AF | UXN;
    pub(super) const ATTR_TAIL: u64 = IDX_DEVICE << ATTR_SHIFT | AF | PXN | UXN;
    pub(super) const ATTR_GLASS: u64 = IDX_NC << ATTR_SHIFT | SH_INNER | AF | PXN | UXN;

    /// Hoeveel pagina's tabel er in de 64 KB onder het image passen.
    pub(super) const TABLE_PAGES: usize = (abi::layout::LINK_TEXT_OFF / PAGE) as usize;

    /// Het geheugen waar de tabellen in staan: entry `idx` van tabelpagina
    /// `page`. Op het target de 64 KB onder het image (met de MMU nog uit),
    /// in de tests een vector.
    pub(super) trait TableMem {
        /// Leest een entry.
        fn get(&self, page: usize, idx: usize) -> u64;
        /// Schrijft een entry.
        fn set(&mut self, page: usize, idx: usize, v: u64);
    }

    /// De tabellen: pagina 0 is de eerste laag, de rest wordt uitgedeeld
    /// zoals de map ze vraagt, tot de [`TABLE_PAGES`] op zijn.
    pub(super) struct Tables<M: TableMem> {
        pub(super) mem: M,
        /// Uitgedeeld.
        pub(super) used: usize,
        /// Het IPA van pagina 0.
        pub(super) base: u64,
    }

    impl<M: TableMem> Tables<M> {
        pub(super) fn new(mem: M, base: u64) -> Tables<M> {
            let mut t = Tables { mem, used: 1, base };
            t.clear(0);
            t
        }

        /// Het adres van pagina `i`.
        pub(super) fn pa(&self, i: usize) -> u64 {
            self.base + i as u64 * PAGE
        }

        fn clear(&mut self, page: usize) {
            for i in 0..512 {
                self.mem.set(page, i, 0);
            }
        }

        /// De tabel onder entry `idx` van pagina `page`: bestaand, of vers.
        fn child(&mut self, page: usize, idx: usize) -> Result<usize, FbError> {
            let cur = self.mem.get(page, idx);
            if cur & 0b11 == DESC_TABLE {
                let pa = cur & 0x0000_FFFF_FFFF_F000;
                return usize::try_from((pa - self.base) / PAGE).map_err(|_| FbError::Tables);
            }
            if cur != 0 {
                // Een blok waar een tabel moet: de plannen overlappen.
                return Err(FbError::Tables);
            }
            let new = self.used;
            if new >= TABLE_PAGES {
                return Err(FbError::Tables);
            }
            self.used += 1;
            self.clear(new);
            self.mem.set(page, idx, self.pa(new) | DESC_TABLE);
            Ok(new)
        }

        /// Mapt `[lo, hi)` (4 KB-gealigneerd) op zichzelf met `attr`: hele
        /// 2 MB-blokken als blok, de randen als pagina's.
        pub(super) fn map(&mut self, lo: u64, hi: u64, attr: u64) -> Result<(), FbError> {
            let mut a = lo;
            while a < hi {
                let l1 = usize::try_from(a / GIB)
                    .ok()
                    .filter(|i| *i < 512)
                    .ok_or(FbError::Tables)?;
                let l2page = self.child(0, l1)?;
                let l2 = ((a % GIB) / BLOCK) as usize;
                if a.is_multiple_of(BLOCK) && hi - a >= BLOCK {
                    self.mem.set(l2page, l2, a | attr | DESC_BLOCK);
                    a += BLOCK;
                    continue;
                }
                let l3page = self.child(l2page, l2)?;
                let l3 = ((a % BLOCK) / PAGE) as usize;
                self.mem.set(l3page, l3, a | attr | DESC_PAGE);
                a += PAGE;
            }
            Ok(())
        }

        /// De tabellen van `p` in `mem`.
        pub(super) fn of(mem: M, p: &Plan) -> Result<Tables<M>, FbError> {
            let mut t = Tables::new(mem, p.ram.0);
            t.map(p.ram.0, p.ram.1, ATTR_RAM)?;
            t.map(p.tail.0, p.tail.1, ATTR_TAIL)?;
            t.map(p.glass.0, p.glass.1, ATTR_GLASS)?;
            Ok(t)
        }
    }
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod stage1 {
    //! De hardwarekant: de tabellen in het geheugen en de registers.

    use super::tables::{IDX_DEVICE, IDX_NC, IDX_WB, TableMem, Tables};
    use super::{FbError, Mapped, PAGE, Plan};
    use core::arch::asm;
    use dev::Pa;

    /// MAIR_EL1: Device-nGnRnE, Normal-NC (0x44) en Normal write-back
    /// (0xFF) op hun indexen.
    const MAIR: u64 = 0x00 << (8 * IDX_DEVICE) | 0x44 << (8 * IDX_NC) | 0xFF << (8 * IDX_WB);

    /// TCR_EL1: 39-bit VA vanaf de eerste laag (T0SZ 25), 4 KB-korrel,
    /// tabelwandeling Non-cacheable (IRGN0/ORGN0 0; SCTLR.C blijft uit, zie
    /// de moduledoc), inner shareable, geen TTBR1-wandeling (EPD1, TG1 op
    /// 4 KB), 36-bit IPA (IPS 1: alles van een app ligt onder 4 GB).
    const TCR: u64 = 25 | 0b11 << 12 | 1 << 23 | 0b10 << 30 | 0b001 << 32;

    /// De 64 KB onder het image, rechtstreeks: de MMU staat nog uit, dus
    /// elke store staat meteen in het geheugen.
    struct UnderImage(u64);

    impl UnderImage {
        fn at(&self, page: usize, idx: usize) -> Pa {
            Pa(self.0 + page as u64 * PAGE + idx as u64 * 8)
        }
    }

    impl TableMem for UnderImage {
        fn get(&self, page: usize, idx: usize) -> u64 {
            dev::read64(self.at(page, idx))
        }
        fn set(&mut self, page: usize, idx: usize, v: u64) {
            dev::write64(self.at(page, idx), v);
        }
    }

    /// SCTLR_EL1.M: de MMU aan.
    const SCTLR_M: u64 = 1 << 0;
    /// SCTLR_EL1.C: data-cacheability. Blijft uit (zie de moduledoc).
    const SCTLR_C: u64 = 1 << 2;
    /// SCTLR_EL1.WXN: schrijfbaar is niet uitvoerbaar. Moet uit: het image
    /// en de heap delen hun blokken.
    const SCTLR_WXN: u64 = 1 << 19;

    pub(super) fn enable(p: &Plan) -> Result<Mapped, FbError> {
        let t = Tables::of(UnderImage(p.ram.0), p)?;
        dev::mb();
        // SAFETY: de tabellen staan in de 64 KB onder het image (de ABI
        // houdt ze daarvoor open, `LINK_TEXT_OFF`), dekken de hele
        // RAM-declaratie (code, heap, stack), de staart en het glas als
        // identiteit, en zijn met de MMU uit geschreven (Device: ze staan in
        // het geheugen). De tabelwandeling is Non-cacheable (SCTLR.C uit),
        // dus hij ziet ze. Na `msr sctlr` loopt de code door op hetzelfde
        // adres: VA = IPA. Interrupts staan dicht (een app heeft geen
        // vectoren) en dit is de primaire core van een app met één core.
        unsafe {
            asm!(
                "msr mair_el1, {mair}",
                "msr tcr_el1, {tcr}",
                "msr ttbr0_el1, {ttbr}",
                "isb",
                "tlbi vmalle1",
                "dsb nsh",
                "isb",
                "mrs {s}, sctlr_el1",
                "bic {s}, {s}, {off}",
                "orr {s}, {s}, {m}",
                "msr sctlr_el1, {s}",
                "isb",
                "ic iallu",
                "dsb nsh",
                "isb",
                mair = in(reg) MAIR,
                tcr = in(reg) TCR,
                ttbr = in(reg) t.base,
                s = out(reg) _,
                off = in(reg) SCTLR_C | SCTLR_WXN,
                m = in(reg) SCTLR_M,
                options(nostack),
            );
        }
        Ok(Mapped {
            base: p.glass.0,
            size: p.glass.1 - p.glass.0,
            table_bytes: t.used as u64 * PAGE,
        })
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod stage1 {
    //! Elders geen ARM-stage-1: op RISC-V en op de host tekent de app
    //! zonder map.

    use super::{FbError, Mapped, Plan};

    pub(super) fn enable(_p: &Plan) -> Result<Mapped, FbError> {
        Err(FbError::NoStage1("not an aarch64 target"))
    }
}

/// Eén invoergebeurtenis van de stroom, in de taal van de browser-KVM.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Input {
    /// Een toets: de code van de KVM (een JavaScript-keycode) en neer/op.
    Key {
        /// De code.
        code: i32,
        /// Ingedrukt.
        down: bool,
    },
    /// De cursor staat op `(x, y)` (absoluut, de kern klemt hem op het
    /// scherm).
    Move {
        /// Horizontaal.
        x: i32,
        /// Verticaal.
        y: i32,
    },
    /// Een muisknop op `(x, y)`.
    Button {
        /// 0 links, 1 midden, 2 rechts.
        code: i32,
        /// Ingedrukt.
        down: bool,
        /// Horizontaal.
        x: i32,
        /// Verticaal.
        y: i32,
    },
    /// Het wiel: `v` klikken.
    Wheel {
        /// Klikken, negatief is naar boven.
        v: i32,
        /// Horizontaal.
        x: i32,
        /// Verticaal.
        y: i32,
    },
    /// Een lege regel: de stroom leeft.
    Keepalive,
}

impl Input {
    /// Ontleedt één regel (zonder de newline). `None` voor wat geen van de
    /// vier vormen is: een regel die de app niet begrijpt, slaat hij over.
    #[must_use]
    pub fn parse(line: &[u8]) -> Option<Input> {
        let s = core::str::from_utf8(line).ok()?.trim();
        if s.is_empty() {
            return Some(Input::Keepalive);
        }
        let n = |k: &str| field_num(s, k);
        let (x, y) = (n("x").unwrap_or(0), n("y").unwrap_or(0));
        match field_str(s, "k")? {
            "key" => Some(Input::Key {
                code: n("c")?,
                down: n("v")? != 0,
            }),
            "move" => Some(Input::Move {
                x: n("x")?,
                y: n("y")?,
            }),
            "btn" => Some(Input::Button {
                code: n("c")?,
                down: n("v")? != 0,
                x,
                y,
            }),
            "wheel" => Some(Input::Wheel { v: n("v")?, x, y }),
            _ => None,
        }
    }
}

/// De waarde van `"k":"..."` in een plat JSON-object.
fn field_str<'a>(s: &'a str, key: &str) -> Option<&'a str> {
    let rest = after_key(s, key)?;
    let rest = rest.strip_prefix('"')?;
    rest.split_once('"').map(|(v, _)| v)
}

/// De waarde van `"k":123` in een plat JSON-object.
fn field_num(s: &str, key: &str) -> Option<i32> {
    let rest = after_key(s, key)?;
    let end = rest
        .char_indices()
        .find(|&(i, c)| !(c.is_ascii_digit() || (i == 0 && c == '-')))
        .map_or(rest.len(), |(i, _)| i);
    rest.get(..end)?.parse().ok()
}

/// Wat er na `"key":` komt (spaties overgeslagen).
fn after_key<'a>(s: &'a str, key: &str) -> Option<&'a str> {
    let mut rest = s;
    loop {
        let at = rest.find('"')?;
        rest = rest.get(at + 1..)?;
        let (name, tail) = rest.split_once('"')?;
        let tail = tail.trim_start();
        if name == key
            && let Some(v) = tail.strip_prefix(':')
        {
            return Some(v.trim_start());
        }
        rest = tail;
    }
}

/// Knipt een bytestroom in regels, zonder allocatie. De verbinding leest
/// in [`LineReader::spare`], meldt met [`LineReader::commit`] hoeveel er
/// kwam, en haalt de regels op met [`LineReader::pop`].
///
/// # Invariants
///
/// `len <= LINE_CAP`; `skip` betekent dat de bytes tot de volgende newline
/// bij een te lange regel horen en wegvallen.
pub struct LineReader {
    buf: [u8; LINE_CAP],
    len: usize,
    skip: bool,
    /// Regels die te lang waren en wegvielen.
    pub overlong: u64,
}

impl Default for LineReader {
    fn default() -> Self {
        Self::new()
    }
}

impl LineReader {
    /// Een lege lezer.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            buf: [0; LINE_CAP],
            len: 0,
            skip: false,
            overlong: 0,
        }
    }

    /// De vrije ruimte om in te lezen. Is die op (een regel zonder newline
    /// die de hele buffer vult), dan valt de regel weg en is de buffer weer
    /// leeg.
    pub fn spare(&mut self) -> &mut [u8] {
        if self.len == LINE_CAP {
            // INVARIANT: de regel is te lang; alles tot de volgende newline
            // valt weg.
            self.len = 0;
            self.skip = true;
            self.overlong = self.overlong.wrapping_add(1);
        }
        self.buf.get_mut(self.len..).unwrap_or_default()
    }

    /// `n` bytes zijn in [`LineReader::spare`] gelezen.
    pub fn commit(&mut self, n: usize) {
        self.len = self.len.saturating_add(n).min(LINE_CAP);
    }

    /// De volgende hele regel, zonder newline, gekopieerd naar `out`; geeft
    /// de lengte.
    pub fn pop(&mut self, out: &mut [u8; LINE_CAP]) -> Option<usize> {
        loop {
            let nl = self.buf.get(..self.len)?.iter().position(|&b| b == b'\n')?;
            let line = nl;
            if self.skip {
                self.skip = false;
            } else if let (Some(src), Some(dst)) = (self.buf.get(..line), out.get_mut(..line)) {
                dst.copy_from_slice(src);
                self.consume(nl + 1);
                return Some(line);
            }
            self.consume(nl + 1);
        }
    }

    /// Schuift `n` bytes uit de buffer.
    fn consume(&mut self, n: usize) {
        let n = n.min(self.len);
        self.buf.copy_within(n..self.len, 0);
        self.len -= n;
    }
}

#[cfg(test)]
mod tests {
    use super::tables::*;
    use super::*;
    use std::collections::HashMap;

    fn env(pairs: &[(&'static str, &'static str)]) -> HashMap<&'static str, &'static str> {
        pairs.iter().copied().collect()
    }

    /// De tabelpagina's van de tests.
    struct Pages(std::vec::Vec<[u64; 512]>);

    impl Default for Pages {
        fn default() -> Self {
            Pages(std::vec![[0; 512]; TABLE_PAGES])
        }
    }

    impl TableMem for Pages {
        fn get(&self, page: usize, idx: usize) -> u64 {
            self.0[page][idx]
        }
        fn set(&mut self, page: usize, idx: usize, v: u64) {
            self.0[page][idx] = v;
        }
    }

    const RAMFB: &[(&str, &str)] = &[
        ("FB_BASE", "0x20000000"),
        ("FB_WIDTH", "1280"),
        ("FB_HEIGHT", "800"),
        ("FB_STRIDE", "5120"),
        ("FB_BPP", "32"),
        ("INPUT_ADDR", "10.100.0.1:7879"),
    ];

    #[test]
    fn the_env_of_the_grant_is_the_glass() {
        let e = env(RAMFB);
        let g = Glass::from_env(|k| e.get(k).copied()).unwrap();
        assert_eq!(
            g,
            Glass {
                base: 0x2000_0000,
                width: 1280,
                height: 800,
                stride: 5120,
                bpp: 32,
                swap: false
            }
        );
        assert_eq!(g.size(), 5120 * 800);
        assert_eq!(
            input_addr(|k| e.get(k).copied()),
            Some(Ok(([10, 100, 0, 1], 7879)))
        );
        assert_eq!(input_addr(|_| None), None);
        assert_eq!(
            input_addr(|_| Some("10.100.0.1")),
            Some(Err(FbError::Bad("INPUT_ADDR")))
        );
    }

    #[test]
    fn a_bad_glass_is_refused_before_a_pixel() {
        let mut e = env(RAMFB);
        e.insert("FB_STRIDE", "4000");
        assert!(matches!(
            Glass::from_env(|k| e.get(k).copied()),
            Err(FbError::Stride { .. })
        ));
        let mut e = env(RAMFB);
        e.insert("FB_BPP", "24");
        assert_eq!(
            Glass::from_env(|k| e.get(k).copied()),
            Err(FbError::Bpp(24))
        );
        let mut e = env(RAMFB);
        e.remove("FB_BASE");
        assert_eq!(
            Glass::from_env(|k| e.get(k).copied()),
            Err(FbError::Missing("FB_BASE"))
        );
        let mut e = env(RAMFB);
        e.insert("FB_BASE", "0x50000000");
        assert!(matches!(
            Glass::from_env(|k| e.get(k).copied()),
            Err(FbError::Window { .. })
        ));
    }

    #[test]
    fn encode_follows_the_console() {
        let mut g = Glass {
            base: FB_IPA,
            width: 1,
            height: 1,
            stride: 4,
            bpp: 32,
            swap: false,
        };
        assert_eq!(g.encode(0x0011_2233), 0x0011_2233);
        g.swap = true;
        assert_eq!(g.encode(0x0011_2233), 0x0033_2211);
        g.swap = false;
        g.bpp = 16;
        assert_eq!(g.encode(0x00FF_0000), 0xF800);
    }

    /// De ramfb van QEMU: RAM en glas als blokken, de rand van het glas
    /// als pagina's, en alles in de 64 KB.
    #[test]
    fn the_tables_fit_and_map_identity() {
        let p = Plan {
            ram: (0x5000_0000, 0x5000_0000 + 0x3e0_0000),
            tail: (0x53e0_0000, 0x5400_0000),
            glass: (0x2000_0000, 0x2000_0000 + 0x3e_8000),
        };
        let t = Tables::of(Pages::default(), &p).unwrap();
        // L1, L2 voor GB 0 en GB 1, één L3 voor de rand van het glas.
        assert_eq!(t.used, 4);
        let walk = |va: u64| -> Option<u64> {
            let l1 = t.mem.get(0, (va / GIB) as usize);
            if l1 & 0b11 != DESC_TABLE {
                return None;
            }
            let p2 = ((l1 & 0xFFFF_FFFF_F000) - t.base) / PAGE;
            let l2 = t.mem.get(p2 as usize, ((va % GIB) / BLOCK) as usize);
            match l2 & 0b11 {
                DESC_BLOCK => Some(l2),
                DESC_TABLE => {
                    let p3 = ((l2 & 0xFFFF_FFFF_F000) - t.base) / PAGE;
                    let e = t.mem.get(p3 as usize, ((va % BLOCK) / PAGE) as usize);
                    (e & 0b11 == DESC_PAGE).then_some(e)
                }
                _ => None,
            }
        };
        let ram = walk(0x5001_0000).unwrap();
        assert_eq!(ram & 0xFFFF_FFE0_0000, 0x5000_0000);
        assert_eq!(ram & 0x1c, IDX_WB << ATTR_SHIFT);
        let tail = walk(0x53e0_1000).unwrap();
        assert_eq!(tail & 0x1c, IDX_DEVICE << ATTR_SHIFT);
        assert_ne!(tail & PXN, 0);
        let glass_end = walk(0x2000_0000 + 0x3e_7000).unwrap();
        assert_eq!(glass_end & 0xFFFF_FFFF_F000, 0x2000_0000 + 0x3e_7000);
        assert_eq!(glass_end & 0x1c, IDX_NC << ATTR_SHIFT);
        assert_eq!(walk(0x2000_0000 + 0x3e_8000), None, "past the glass");
        assert_eq!(walk(0x4000_0000), None, "the kern is not ours");
    }

    #[test]
    fn a_plan_that_does_not_fit_is_refused() {
        // Een glas dat over veertien losse blokranden loopt past niet.
        let mut t = Tables::new(Pages::default(), 0x5000_0000);
        let mut r = Ok(());
        for i in 0..20u64 {
            let a = 0x2000_0000 + i * GIB / 2 + PAGE;
            r = t.map(a, a + PAGE, ATTR_GLASS);
            if r.is_err() {
                break;
            }
        }
        assert_eq!(r, Err(FbError::Tables));
    }

    #[test]
    fn the_four_shapes_of_the_kvm() {
        let p = |s: &str| Input::parse(s.as_bytes());
        assert_eq!(
            p(r#"{"k":"key","c":65,"v":1}"#),
            Some(Input::Key {
                code: 65,
                down: true
            })
        );
        assert_eq!(
            p(r#"{"k":"move","x":640,"y":400}"#),
            Some(Input::Move { x: 640, y: 400 })
        );
        assert_eq!(
            p(r#"{"k":"btn","c":2,"v":0,"x":1,"y":2}"#),
            Some(Input::Button {
                code: 2,
                down: false,
                x: 1,
                y: 2
            })
        );
        assert_eq!(
            p(r#"{"k":"wheel","c":0,"v":-3,"x":5,"y":6}"#),
            Some(Input::Wheel { v: -3, x: 5, y: 6 })
        );
        assert_eq!(p(""), Some(Input::Keepalive));
        assert_eq!(p(r#"{"k":"paste"}"#), None);
        assert_eq!(p("not json"), None);
    }

    #[test]
    fn lines_are_cut_and_overlong_ones_dropped() {
        let mut r = LineReader::new();
        let mut out = [0u8; LINE_CAP];
        let feed = |r: &mut LineReader, b: &[u8]| {
            let s = r.spare();
            s[..b.len()].copy_from_slice(b);
            r.commit(b.len());
        };
        feed(&mut r, b"{\"k\":\"key\",\"c\":65,\"v\":1}\n\n{\"k\":\"mo");
        let n = r.pop(&mut out).unwrap();
        assert_eq!(&out[..n], b"{\"k\":\"key\",\"c\":65,\"v\":1}");
        assert_eq!(r.pop(&mut out), Some(0));
        assert_eq!(r.pop(&mut out), None);
        feed(&mut r, b"ve\",\"x\":1,\"y\":2}\n");
        let n = r.pop(&mut out).unwrap();
        assert_eq!(Input::parse(&out[..n]), Some(Input::Move { x: 1, y: 2 }));
        // Een regel langer dan de buffer valt weg, de volgende niet.
        let long = [b'x'; LINE_CAP];
        feed(&mut r, &long);
        assert_eq!(r.pop(&mut out), None);
        feed(&mut r, b"xxx\n{\"k\":\"key\",\"c\":1,\"v\":0}\n");
        let n = r.pop(&mut out).unwrap();
        assert_eq!(&out[..n], b"{\"k\":\"key\",\"c\":1,\"v\":0}");
        assert_eq!(r.overlong, 1);
    }
}
