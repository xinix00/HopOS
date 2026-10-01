//! De SART: Apples adresfilter voor de ANS (de NVMe-coprocessor).
//!
//! Waar een PCIe-device achter een DART hangt (een IOMMU met
//! paginatabellen), staat voor de ANS een eenvoudiger zeef: zestien vensters
//! van (adres, grootte, vlaggen), en wat daarbuiten valt komt er niet door.
//! Een DMA van de coprocessor naar geheugen dat niet in een venster staat,
//! gebeurt gewoon niet, zonder foutmelding: het filter meldt niet wat het
//! tegenhoudt.
//!
//! De vensters die de firmware al zette laten we met rust: die horen bij
//! buffers die iBoot en de ANS-firmware onderling gebruiken, en een venster
//! overschrijven dat nog in gebruik is zet de coprocessor stil. Wij zoeken
//! een lege plek.
//!
//! Registerkaart: m1n1 `src/sart.c`, variant 3; dat is wat de ADT van de
//! mini meldt (`sart-version = 3`, node `sart-ans@85C50000`). De Rust-vorm
//! van `OLD/metal/board/apple/sart.go`.

use core::fmt;
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};

/// Het aantal vensters.
pub const ENTRIES: usize = 16;
/// De variant die deze module kent.
pub const VERSION: u32 = 3;

/// Het registerblok van SART v3.
#[repr(C)]
struct Regs {
    /// Vlaggen per venster; 0 = leeg.
    config: [Reg<u32>; ENTRIES],
    /// Fysiek adres >> 12.
    paddr: [Reg<u32>; ENTRIES],
    /// Grootte >> 12.
    size: [Reg<u32>; ENTRIES],
}

const _: () = {
    assert!(offset_of!(Regs, config) == 0x00);
    assert!(offset_of!(Regs, paddr) == 0x40);
    assert!(offset_of!(Regs, size) == 0x80);
};

/// Hoeveel het board moet mappen.
pub const MMIO_LEN: u64 = size_of::<Regs>() as u64;

const SHIFT: u32 = 12;
const GRAIN: u64 = 1 << SHIFT;
const ALLOW: u32 = 0xff;

/// Waarom er geen venster kwam.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// De ADT meldt een andere variant dan 3.
    Version(u32),
    /// Adres of maat niet op 4 KB, of maat nul.
    Unaligned {
        /// Het adres.
        pa: u64,
        /// De maat.
        size: u64,
    },
    /// Adres of maat past niet in de 32 bits van een register (>> 12).
    TooHigh {
        /// Het adres.
        pa: u64,
        /// De maat.
        size: u64,
    },
    /// Alle zestien vensters zijn bezet.
    Full {
        /// Het adres.
        pa: u64,
        /// De maat.
        size: u64,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Version(v) => write!(f, "sart: version {v}, only {VERSION} is known"),
            Self::Unaligned { pa, size } => {
                write!(f, "sart: window {size:#x} at {pa:#x} not on 4 KB")
            }
            Self::TooHigh { pa, size } => {
                write!(f, "sart: window {size:#x} at {pa:#x} beyond 44 bits")
            }
            Self::Full { pa, size } => write!(
                f,
                "sart: all {ENTRIES} windows taken, none for {size:#x} at {pa:#x}"
            ),
        }
    }
}

/// De SART van één coprocessor.
pub struct Sart {
    base: Pa,
}

impl Sart {
    /// Een SART op `base`, van variant `version` (ADT: `sart-version`).
    ///
    /// # Safety
    ///
    /// `base` is het SART-blok uit de ADT, gemapt als Device voor minstens
    /// [`MMIO_LEN`] bytes en voor altijd.
    pub unsafe fn new(base: Pa, version: u32) -> Result<Self, Error> {
        if version != VERSION {
            return Err(Error::Version(version));
        }
        Ok(Self { base })
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    /// Opent een venster voor de coprocessor op `[pa, pa + size)` en geeft
    /// de index.
    ///
    /// Staat precies dit venster er al (een vorige kern van ons: elke flip
    /// vraagt dezelfde regio opnieuw), dan is dat het antwoord. Anders
    /// vreet elke flip een entry en na zestien staat de opslag stil (gemeten
    /// 03-09, twaalf flips na de power-cycle).
    pub fn allow(&mut self, pa: Pa, size: u64) -> Result<usize, Error> {
        let e = (pa.0, size);
        if size == 0 || !pa.is_aligned(GRAIN) || !size.is_multiple_of(GRAIN) {
            return Err(Error::Unaligned { pa: e.0, size: e.1 });
        }
        let (Ok(p), Ok(s)) = (u32::try_from(pa.0 >> SHIFT), u32::try_from(size >> SHIFT)) else {
            return Err(Error::TooHigh { pa: e.0, size: e.1 });
        };
        let r = self.regs();
        let entries = || r.config.iter().zip(&r.paddr).zip(&r.size).enumerate();
        for (i, ((c, ep), es)) in entries() {
            if c.read() != 0 && ep.read() == p && es.read() == s {
                return Ok(i);
            }
        }
        for (i, ((c, ep), es)) in entries() {
            if c.read() != 0 {
                continue; // Van de firmware, of van een vorige kern.
            }
            ep.write(p);
            es.write(s);
            dev::mb();
            c.write(ALLOW);
            dev::mb();
            return Ok(i);
        }
        Err(Error::Full { pa: e.0, size: e.1 })
    }
}
