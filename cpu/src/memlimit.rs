//! Het geheugenplafond: de grens van de heap, afgeleid uit het RAM-venster
//! waarin deze wereld draait.
//!
//! Niemand hoort hierover na te denken (Derek, 02-08): geen getal per
//! board, geen getal per job, en een nieuw board of een kleinere partitie is
//! vanzelf goed. [`arm`] rekent uit wat de allocator mag; het board geeft
//! het resultaat aan zijn heap (`board::heap::Heap::init`). Deze module
//! bezit alleen de rekensom, niet de allocator.
//!
//! Waarom dit bestaat (gemeten 02-08, LicheeRV): Go's GC verdubbelde zijn
//! heap-doel per ronde en kende geen muur. In een klein vast raam eindigde
//! dat als "fatal error: out of memory", en die haalde alleen een UART, want
//! de TCP-console stierf mee: van buiten een volledige stille node-dood.
//! Drie zwarte dozen vingen hem identiek: alloc ~34,9 MB, Sys 39.780 KB,
//! pas drie GC's op ~56 s. Het was nooit een lek; het was ontbrekende
//! informatie.
//!
//! In Rust is die informatie een harde grens: de heap geeft op het plafond
//! `null`, en elke allocatie die kan falen zegt dat in zijn type (handboek
//! §6). Een wereld die meer wil dan mag, krijgt een `AllocError` in plaats
//! van de dood. De rekensom bleef:
//!
//! ```text
//! muur   = ram_start + ram_size - stack_offset   (de RAM-declaratie)
//! basis  = einde image+BSS                        (waar de heap begint)
//! budget = muur - basis                           (de hele arena)
//! limiet = budget - slack, slack = max(10% van budget, 4 MB)
//! ```
//!
//! **Eén regel voor élke wereld, kern én app**: de marge hoort bij het raam,
//! niet bij wie erin woont (Derek, 11-08). Was de fractie 1/16 (~6%), dan
//! gaf dat op de LicheeRV een limiet van 47 MB op 47 MB bruikbare ruimte:
//! nul marge, en dood.
//!
//! Wat NIET meeging, en waarom:
//!
//! - De GC-thrash-wachter (`watch`, HOPOS_GC_THRASH) en het GOGC-tempo
//!   (25 onder een raam van 128 MB; gemeten op de QEMU-bank: GOGC 100 gaf
//!   een OOM na 151 s, GOGC 25 een piek van 30,6 MB met 15 MB over). Er is
//!   geen GC: geen doodspiraal om luid te maken en geen verdubbel-regel om
//!   af te remmen.
//! - De reden voor de vloer van 4 MB was Go-specifiek: `SetMemoryLimit` is
//!   zacht, en de allocator groeide in stappen die pas ná de toets gezet
//!   werden (gemeten 06-08, per-256KB-hashes: 1 MB marge gaf elf genulde
//!   blokken, 4 MB nul). Hier is de grens hard, dus die reden vervalt. De
//!   regel zelf blijft staan tot een meting zegt dat hij anders moet: hij
//!   is de enige die op elk board gedraaid heeft, en wat onder het plafond
//!   overblijft is de ruimte van de stack en de zwarte doos, niet van de
//!   heap.

use core::fmt;

/// De ondergrens van de marge onder de muur (4 MB).
pub const ARENA_SLACK: u64 = 4 << 20;

/// Het RAM-venster van een wereld, zoals de RAM-declaratie het zegt.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Window {
    /// De eerste byte van het venster.
    pub ram_start: u64,
    /// De maat van het venster.
    pub ram_size: u64,
    /// Het stuk bovenaan dat de stack houdt (de muur ligt eronder).
    pub stack_offset: u64,
    /// Het einde van image plus BSS (`__bss_end`): waar de heap begint.
    pub data_end: u64,
}

/// De uitkomst van [`arm`]: de arena die de allocator krijgt.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Arena {
    /// De eerste heap-byte (= [`Window::data_end`]).
    pub start: u64,
    /// De exclusieve grens: het plafond.
    pub end: u64,
    /// De hele ruimte tussen basis en muur.
    pub budget: u64,
    /// Wat onder de muur vrij blijft.
    pub slack: u64,
    /// Het venster waar dit uit kwam, voor de consoleregel.
    pub window: Window,
}

impl Arena {
    /// Het plafond in bytes.
    #[must_use]
    pub const fn limit(&self) -> u64 {
        self.end - self.start
    }
}

impl fmt::Display for Arena {
    /// Eén regel zelfconfiguratie voor de console-historie: als een node
    /// ooit tóch tegen zijn plafond loopt, zijn dit de getallen die de
    /// operator wil kennen.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "mem: heap limit {}MB (window {}MB, image+bss {}MB, slack {}MB)",
            self.limit() >> 20,
            self.window.ram_size >> 20,
            self.start.saturating_sub(self.window.ram_start) >> 20,
            self.slack >> 20,
        )
    }
}

/// Waarom er geen plafond uit het venster kwam.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Error {
    /// Het venster is leeg of loopt over de adresruimte heen.
    NoWindow(Window),
    /// De heap-basis ligt niet tussen het begin en de muur.
    BaseOutside {
        /// De heap-basis.
        base: u64,
        /// De muur.
        wall: u64,
    },
    /// Na de marge blijft er niets over.
    TooSmall {
        /// Het budget.
        budget: u64,
        /// De marge die eraf moest.
        slack: u64,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoWindow(w) => write!(
                f,
                "memlimit: no usable RAM window (start {:#x}, size {:#x}, stack {:#x})",
                w.ram_start, w.ram_size, w.stack_offset
            ),
            Self::BaseOutside { base, wall } => {
                write!(
                    f,
                    "memlimit: heap base {base:#x} is not below the wall {wall:#x}"
                )
            }
            Self::TooSmall { budget, slack } => write!(
                f,
                "memlimit: budget {budget:#x} does not cover the slack {slack:#x}"
            ),
        }
    }
}

/// Het resultaat van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Leidt de arena van de allocator af uit het RAM-venster, met de marge
/// [`ARENA_SLACK`].
///
/// Faalt de berekening (venster onbekend of absurd klein), dan zegt de fout
/// waarom, met de getallen. De Go-kern deed dan bewust niets ("een
/// verzonnen limiet is een nieuwe manier om stuk te gaan"); hier kiest de
/// aanroeper, en een heap zonder bereik is een heap die weigert.
pub fn arm(window: Window) -> Result<Arena> {
    arm_with(window, ARENA_SLACK)
}

/// De rekensom, met de vloer van de marge als parameter (voor de tests).
fn arm_with(window: Window, min_slack: u64) -> Result<Arena> {
    let wall = window
        .ram_start
        .checked_add(window.ram_size)
        .and_then(|top| top.checked_sub(window.stack_offset))
        .filter(|_| window.ram_size != 0)
        .ok_or(Error::NoWindow(window))?;
    let base = window.data_end;
    if base <= window.ram_start || base >= wall {
        return Err(Error::BaseOutside { base, wall });
    }
    let budget = wall - base;
    // 10% is Go's eigen richtlijn voor headroom onder een memory limit; de
    // vloer dekt wat niet meeschaalt.
    let slack = (budget / 10).max(min_slack);
    if slack >= budget {
        return Err(Error::TooSmall { budget, slack });
    }
    Ok(Arena {
        start: base,
        end: base + (budget - slack),
        budget,
        slack,
        window,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const MB: u64 = 1 << 20;

    fn window(size: u64, image: u64) -> Window {
        Window {
            ram_start: 0x8000_0000,
            ram_size: size,
            stack_offset: MB,
            data_end: 0x8000_0000 + image,
        }
    }

    #[test]
    fn ten_percent_above_the_floor() {
        // 1 GB-raam, 17 MB image+bss: marge 10%.
        let a = arm(window(1024 * MB, 17 * MB)).unwrap();
        let budget = 1024 * MB - MB - 17 * MB;
        assert_eq!(a.budget, budget);
        assert_eq!(a.slack, budget / 10);
        assert_eq!(a.limit(), budget - budget / 10);
        assert_eq!(a.start, 0x8000_0000 + 17 * MB);
        assert_eq!(a.end, a.start + a.limit());
    }

    #[test]
    fn floor_on_small_windows() {
        // Het LicheeRV-geval: een klein raam, de vloer van 4 MB wint.
        let a = arm(window(24 * MB, 6 * MB)).unwrap();
        assert_eq!(a.budget, 17 * MB);
        assert_eq!(a.slack, ARENA_SLACK);
        assert_eq!(a.limit(), 13 * MB);
        assert_eq!(
            a.to_string(),
            "mem: heap limit 13MB (window 24MB, image+bss 6MB, slack 4MB)"
        );
    }

    #[test]
    fn refusals_carry_the_numbers() {
        assert_eq!(arm(window(0, MB)), Err(Error::NoWindow(window(0, MB))));
        // Basis op of boven de muur.
        let w = window(8 * MB, 7 * MB);
        assert_eq!(
            arm(w),
            Err(Error::BaseOutside {
                base: 0x8000_0000 + 7 * MB,
                wall: 0x8000_0000 + 7 * MB
            })
        );
        // Basis op het begin: het image staat er niet in.
        assert!(matches!(
            arm(window(64 * MB, 0)),
            Err(Error::BaseOutside { .. })
        ));
        // Budget kleiner dan de vloer.
        assert_eq!(
            arm(window(8 * MB, 4 * MB)),
            Err(Error::TooSmall {
                budget: 3 * MB,
                slack: ARENA_SLACK
            })
        );
        // Overloop van de adresruimte.
        let mut w = window(64 * MB, MB);
        w.ram_start = u64::MAX - MB;
        assert!(matches!(arm(w), Err(Error::NoWindow(_))));
        // De rekensom met een andere vloer, zoals de Go-test `arm` hem riep.
        assert_eq!(arm_with(window(12 * MB, 6 * MB), MB).unwrap().slack, MB);
    }
}
