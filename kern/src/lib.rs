//! De orchestrator-mechanismen van HopOS: slots en partities, de kooi, de
//! system-API, hopfs, de kern-flip en de console.
//!
//! Deze crate BEZIT de boekhouding van de node: welke partitie van wie is,
//! welke core welke bewoner draagt, welke servicer bij welk slot hoort, de
//! boom van hopfs. Hij bezit GEEN ijzer: alles wat per architectuur of board
//! anders is (een kooi bouwen, een core aanzetten, een blok lezen, een
//! TCP-verbinding) komt binnen als trait, zodat de logica op de
//! ontwikkelmachine test met nep-implementaties en `cpu`, `board` en `net` de
//! echte leveren:
//!
//! | Trait | Wie levert hem | Module |
//! | --- | --- | --- |
//! | [`cage::Cage`] | `cpu` (stage-2 op ARM, PMP op RISC-V) | [`cage`] |
//! | [`cage::Cores`] | het board (PSCI, klassen small/mid/big) | [`cage`] |
//! | [`cage::Timer`] | de executor | [`cage`] |
//! | [`cage::PhysMem`] | `dev` via het board | [`cage`] |
//! | [`hopfs::BlockIo`] | de blok-driver in een `blkdev::Paced` (virtio-blk op QEMU, NVMe op ijzer) | [`hopfs`] |
//! | [`system::Conn`] | `leannet` | [`system`] |
//!
//! De specificatie is de Go-kern in `OLD/metal/kern`; de regels E1 tot E9
//! uit `OLD/docs/lifecycle.md` staan hier als typen ([`partmem`]) en tests.

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
#![forbid(unsafe_code)]

extern crate alloc;

pub mod cage;
#[cfg(feature = "media")]
pub mod codecabi;
pub mod conport;
pub mod deviceabi;
mod error;
pub mod grants;
pub mod hopfs;
pub mod kernflip;
pub mod nodecfg;
pub mod partmem;
pub mod pool;
pub mod rpc;
mod sha256;
pub mod slots;
pub mod stage2;
pub mod store;
pub mod system;
pub mod watchdog;

#[cfg(test)]
mod testutil;

pub use error::{Error, Result};

/// De harde kooi-bovengrens: het hoogste slotnummer dat de kern ooit kent.
///
/// 128 dekt de Ampere Altra (127 app-cores); een board zet zijn eigen,
/// lagere `max_slots` bij boot. Zelfde getal als `layout.SlotCap` in Go.
pub const SLOT_CAP: usize = 128;

/// De hoogste logische core die de kern ooit kent.
pub const CORE_CAP: usize = 256;

/// De blokkorrel van de partities: 2 MiB, omdat de stage-2-partitieblokken
/// 2 MiB zijn.
pub const GRAIN: u64 = 2 << 20;

const _: () = assert!(SLOT_CAP == abi::layout::SLOT_CAP);

/// Een slotnummer (= kooinummer), 1 tot en met [`SLOT_CAP`].
///
/// # Invariants
///
/// `1 <= self.0 <= SLOT_CAP`; alleen [`Slot::new`] maakt er een.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
pub struct Slot(u8);

impl Slot {
    /// Het eerste slot: een geldige beginwaarde voor een vaste tabel.
    // INVARIANT: 1 ligt in 1..=SLOT_CAP.
    pub const FIRST: Slot = Slot(1);

    /// Het slot `i`, of `None` buiten `1..=SLOT_CAP`.
    #[must_use]
    pub const fn new(i: usize) -> Option<Slot> {
        if i >= 1 && i <= SLOT_CAP {
            // INVARIANT: bereik net getoetst; SLOT_CAP past in een u8.
            Some(Slot(i as u8))
        } else {
            None
        }
    }

    /// Het nummer als index in een tabel van `SLOT_CAP + 1` plaatsen.
    #[must_use]
    pub const fn get(self) -> usize {
        self.0 as usize
    }
}

impl core::fmt::Display for Slot {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(f, "{}", self.0)
    }
}

/// Een logisch corenummer (1 tot en met [`CORE_CAP`]); het fysieke nummer
/// komt uit [`cage::Cores::phys`].
///
/// De nummers zijn logisch en aaneengesloten: een afspraak van de kern, geen
/// uitspraak over het silicium (Apple's HOP-core zit midden in de cpu-lijst,
/// een RISC-V-board levert hart-ID's met gaten).
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
pub struct Core(u16);

impl Core {
    /// De OS-core: logische core 0, waar de kern zelf woont en die hij deelt
    /// met Hop en met de sharegroups die hem mogen delen (PORT.md beslissing
    /// 2). Geen app-core: [`Core::new`] geeft hem nooit, zodat een getal van
    /// buiten hem niet kan aanwijzen; alleen de plaatsing kiest hem
    /// ([`crate::pool::CorePool::share_os_core`]).
    pub const OS: Core = Core(0);

    /// Core `c`, of `None` buiten `1..=CORE_CAP`.
    #[must_use]
    pub const fn new(c: usize) -> Option<Core> {
        if c >= 1 && c <= CORE_CAP {
            // INVARIANT: bereik net getoetst; CORE_CAP past in een u16.
            Some(Core(c as u16))
        } else {
            None
        }
    }

    /// Het nummer.
    #[must_use]
    pub const fn get(self) -> usize {
        self.0 as usize
    }

    /// De core-run van een kooi: deze core en de `span - 1` erna. Anders dan
    /// `(c..c + span).filter_map(Core::new)` laat dit [`Core::OS`] niet
    /// weg: een stop die de OS-core oversloeg, zag een bewoner daar meteen
    /// als stil.
    pub fn run(self, span: usize) -> impl Iterator<Item = Core> {
        core::iter::once(self)
            .chain((self.get() + 1..self.get() + span.max(1)).filter_map(Core::new))
    }
}

impl core::fmt::Display for Core {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(f, "{}", self.0)
    }
}

/// Een fysiek bereik `[base, base + size)`.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Region {
    /// Het eerste adres.
    pub base: u64,
    /// De maat in bytes.
    pub size: u64,
}

impl Region {
    /// Een bereik.
    #[must_use]
    pub const fn new(base: u64, size: u64) -> Region {
        Region { base, size }
    }

    /// Het eerste adres erna, of `None` als het bereik de adresruimte omloopt.
    #[must_use]
    pub const fn end(self) -> Option<u64> {
        self.base.checked_add(self.size)
    }

    /// Overlapt dit bereik `o`?
    #[must_use]
    pub fn overlaps(self, o: Region) -> bool {
        let (Some(a), Some(b)) = (self.end(), o.end()) else {
            return true; // Een omlopend bereik overlapt per definitie alles.
        };
        self.base < b && o.base < a
    }
}

/// Rondt `n` op naar de [`GRAIN`], of `None` bij omloop.
#[must_use]
pub const fn align_grain(n: u64) -> Option<u64> {
    match n.checked_add(GRAIN - 1) {
        Some(v) => Some(v & !(GRAIN - 1)),
        None => None,
    }
}
