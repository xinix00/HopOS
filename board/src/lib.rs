//! Het board-contract: wat de kern van een machine vraagt.
//!
//! De Go-kern had één `board.Board`-interface met een register (`Use`,
//! `Current`) dat bij runtime gevuld werd. Hier is het een trait met
//! statische dispatch: de binary kiest precies één board via een feature,
//! en een gemiste methode is een compilefout in plaats van een verrassing
//! op het bord (handboek §7).
//!
//! De trait is met opzet klein. Wat de Go-interface verder droeg (de
//! wandklok, PCIe, de framebuffer, het netplan, de core-start via PSCI)
//! komt erbij zodra een laag erom vraagt; een methode zonder gebruiker is
//! een methode die niemand test.
//!
//! Naast de trait staat [`heap`]: de allocator met een plafond die het
//! board over zijn kern-RAM legt.

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

pub mod heap;

/// De framebuffer-beschrijving van [`Board::framebuffer`], zodat een board
/// hem noemt zonder eigen dependency.
pub use driver_fb as fb;

use core::fmt;
use dev::Pa;
use sync::Signal;

/// Een fysiek geheugenbereik.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Region {
    /// Het beginadres.
    pub base: Pa,
    /// De grootte in bytes.
    pub size: u64,
}

impl Region {
    /// Het eerste adres na het bereik.
    #[must_use]
    pub const fn end(&self) -> Pa {
        self.base.add(self.size)
    }

    /// Ligt `pa` in het bereik?
    #[must_use]
    pub const fn contains(&self, pa: Pa) -> bool {
        pa.0 >= self.base.0 && pa.0 < self.base.0 + self.size
    }
}

/// Het PA-plan van een board: waar de kern woont en waar de DMA-regio's
/// liggen. De tegenhanger van `layout.Plan` uit de Go-kern, voor zover de
/// kern het nu gebruikt.
#[derive(Copy, Clone, Debug)]
pub struct Plan {
    /// De kern-RAM: image, stack en heap. Normal, gecached.
    pub kern_ram: Region,
    /// De DMA-regio van alle drivers samen, buiten de kern-RAM en niet
    /// gecached gemapt.
    pub dma: Region,
    /// Het deel van `dma` dat de NIC krijgt.
    pub net_dma: Region,
}

/// De clusterklasse van een core ("small", "mid", "big"). HOP's plaatsing
/// doet exact-match op klasse.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum CoreClass {
    /// Een zuinige core.
    Small,
    /// Het middensegment.
    Mid,
    /// De beste (of enige) klasse.
    Big,
}

impl fmt::Display for CoreClass {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Small => "small",
            Self::Mid => "mid",
            Self::Big => "big",
        })
    }
}

/// Waarom een board iets weigert.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// Het privilege-niveau kan geen kooi dragen (EL2 op ARM).
    Privilege {
        /// Het niveau waarop we booten.
        el: u8,
    },
    /// De NIC is gevonden maar zijn initialisatie faalde.
    Nic(&'static str),
    /// De schijf is gevonden maar haar initialisatie faalde.
    Disk(&'static str),
    /// De interruptcontroller kwam niet op.
    Irq(&'static str),
    /// Een methode die maar één keer mag, werd twee keer aangeroepen.
    Twice(&'static str),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Privilege { el } => write!(
                f,
                "booted at EL{el}: HopOS requires EL2 (QEMU: virtualization=on)"
            ),
            Self::Nic(why) => write!(f, "nic init: {why}"),
            Self::Disk(why) => write!(f, "disk init: {why}"),
            Self::Irq(why) => write!(f, "interrupt controller: {why}"),
            Self::Twice(what) => write!(f, "{what} called twice"),
        }
    }
}

/// De eis van ARM: HopOS draait op EL2. De Go-zin `board.RequireEL2`.
pub fn require_el2(el: u8) -> Result<(), Error> {
    if el < 2 {
        return Err(Error::Privilege { el });
    }
    Ok(())
}

/// Wat de interrupt-dispatch in één ronde deed; de meetlat van de
/// IRQ-taak.
#[derive(Copy, Clone, Default, Debug, PartialEq, Eq)]
pub struct Dispatched {
    /// Timer-interrupts.
    pub timer: u32,
    /// NIC-interrupts.
    pub nic: u32,
    /// Lijnen die niemand kent (gemeld en afgesloten).
    pub other: u32,
}

/// Het board-contract. Alle methodes draaien op HOP's core; de binary
/// houdt het board in een `static` en geeft `&'static` door.
pub trait Board: Sync {
    /// De NIC-driver van dit board.
    type Nic: netdev::Device;
    /// De slaap van de executor op dit board.
    type Sleeper: executor::Sleeper;

    /// De naam, voor de bootlog.
    const NAME: &'static str;

    /// Eén keer, als eerste: de UART op. Geeft de console-haak.
    fn console(&self) -> fn(&[u8]);

    /// Het exception level waarop we booten, zoals de stub het las.
    fn privilege(&self, el: u8) -> Result<(), Error> {
        require_el2(el)
    }

    /// Eén consoleregel over wie ons bootte en wat die kan. Diagnose, geen
    /// contract.
    fn firmware(&self) -> &'static str;

    /// Geeft `heap` het deel van de kern-RAM dat na image en stack over is.
    /// Eén keer, vóór de eerste allocatie.
    fn init_heap(&self, heap: &heap::Heap);

    /// Leest de firmware-beschrijving (op ARM de FDT uit x0 of een vaste
    /// plek) en onthoudt wat de kern later vraagt. `dtb` is x0 bij boot.
    fn discover(&self, dtb: u64);

    /// De klok: monotone nanoseconden sinds boot.
    fn clock(&self) -> executor::Clock;

    /// De slaap van de executor.
    fn sleeper(&self) -> Self::Sleeper;

    /// DRAM in bytes zoals de firmware het meldt; 0 = onbekend.
    fn mem_total(&self) -> u64;

    /// Het aantal cores, HOP's eigen core meegeteld.
    fn cores(&self) -> usize;

    /// De clusterklasse van `core`.
    fn core_class(&self, core: usize) -> CoreClass;

    /// Het PA-plan.
    fn plan(&self) -> Plan;

    /// Zet de interruptcontroller op HOP's core en de timer-interrupt aan.
    /// Geeft de bel die de IRQ-deur luidt: een taak wacht erop en roept
    /// dan [`dispatch_interrupts`](Board::dispatch_interrupts).
    fn start_interrupts(&self) -> Result<&'static Signal, Error>;

    /// Claimt, behandelt en sluit elke wachtende interrupt. Draait in een
    /// taak, nooit in exception-context.
    fn dispatch_interrupts(&self) -> Dispatched;

    /// Vindt en initialiseert de NIC. `Ok(None)` = geen NIC; één keer.
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error>;

    /// De lineaire framebuffer van dit board, als er een beeld loopt (GOP,
    /// de VideoCore-mailbox, `ramfb`, of de eigen scanout van de RK3566).
    /// `None` = headless, en dat is geen fout. Alleen met de feature `gui`
    /// levert een board er een (docs/gui.md).
    fn framebuffer(&self) -> Option<fb::Desc> {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn el2_is_required() {
        assert_eq!(require_el2(1), Err(Error::Privilege { el: 1 }));
        assert!(require_el2(2).is_ok());
        assert_eq!(
            Error::Privilege { el: 1 }.to_string(),
            "booted at EL1: HopOS requires EL2 (QEMU: virtualization=on)"
        );
    }

    #[test]
    fn region_bounds() {
        let r = Region {
            base: Pa(0x1000),
            size: 0x1000,
        };
        assert!(r.contains(Pa(0x1000)));
        assert!(r.contains(Pa(0x1fff)));
        assert!(!r.contains(Pa(0x2000)));
        assert_eq!(r.end(), Pa(0x2000));
        assert_eq!(CoreClass::Big.to_string(), "big");
    }
}
