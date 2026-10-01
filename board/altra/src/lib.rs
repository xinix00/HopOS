//! De Ampere Altra (en AmpereOne): het UEFI-board (`board-uefi`) plus wat
//! deze machine eigen heeft (`OLD/metal/board/altra/hop`).
//!
//! Alles wat de firmware vertelt, komt langs de universele weg: cores uit
//! de MADT, RAM uit de memory-map (de ~300 GB bulk boven de vlakke 512 GB,
//! gemeten 15-07), PCIe uit de MCFG (tot acht segmenten), de console uit de
//! SPCR (16 TB hoog), CPU_ON via PSCI. Wat hier staat, is de kennis die geen
//! ACPI-tabel draagt:
//!
//! - de NIC: de Intel igb (I210) op deze machines, en QEMU's 82576 waarmee
//!   het pad getest wordt. Gepold, en dat is het profiel: de `_PRT`-INTx
//!   komt aan, maar hem aanzetten doodt de SoC (L83, UART-bewijs), en de
//!   MSI-X/ITS-route die Linux neemt valt buiten de scope. Een 128-core
//!   server is een referentieplatform, geen doel; zijn idle-wekfrequentie is
//!   ruis tegen zijn idle-verbruik;
//! - de schijf: de eerste NVMe, het hele device voor HopOS;
//! - de thermometer: de SMpro via PCC-kanaal 14;
//! - de klok is op servers firmware-domein: geen dvfs-beleid;
//! - de core-klassen: homogeen, alles "big".

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

mod machine;

pub use machine::Altra;

// De slot- en flip-lijm van de binary leest het plan onder deze namen,
// zoals bij de Pi's; op de Altra zijn het die van het UEFI-board.
pub use board_uefi::{DMA, KERN_RAM, KERN_VHE, facts, irq, slots, watchdog};

use board::CoreClass;
use driver_pcie::{Config, Function, find};

/// De schijf die `probe_disk` geeft: de NVMe. De binary noemt hem
/// `vboard::Disk`, zodat de geprobede schijf van de bench naar de opslag gaat
/// zonder dat de binary het type per board kent.
pub type Disk = driver_nvme::Nvme;

/// De naam, voor de bootlog.
pub const NAME: &str = "altra";

/// De klassecode van een NVMe-controller.
pub const CLASS_NVME: u32 = 0x01_08_02;

/// Hoe lang de igb op een link wacht (het Altra-recept sinds 13-07).
pub const LINK_TIMEOUT_NS: u64 = 8_000_000_000;

/// Deze machines zijn homogeen: alles is "big". De UEFI-laag laat de vraag
/// bewust aan het board; een verzonnen indeling maakt jobs onplaatsbaar.
#[must_use]
pub const fn core_class(_core: usize) -> CoreClass {
    CoreClass::Big
}

/// Een gevonden device en het adres van de BAR die de driver wil.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Found {
    /// De functie.
    pub f: Function,
    /// Het BAR-adres (door de firmware toegewezen; op de Altra vaak hoog).
    pub bar: u64,
}

/// De eerste functie over `windows` (config, eerste bus) waarop `is` ja
/// zegt en die een toegewezen memory-BAR `bar` heeft. De firmware wees de
/// BAR's toe; wij lezen alleen (hem overschrijven zette de controller op PA
/// 0: op de Altra een data-abort).
pub fn first<'a, C: Config + 'a>(
    windows: impl IntoIterator<Item = (&'a C, u8)>,
    bar: u8,
    is: impl Fn(&Function) -> bool,
) -> Option<Found> {
    windows.into_iter().find_map(|(c, start)| {
        find(c, start, |f| {
            !f.is_bridge() && is(f) && f.bar_addr(c, bar) != 0
        })
        .map(|f| Found {
            f,
            bar: f.bar_addr(c, bar),
        })
    })
}

/// Is `f` een NIC van dit board?
#[must_use]
pub fn is_nic(f: &Function) -> bool {
    driver_igb::supported(f.vendor, f.device)
}

/// Is `f` een NVMe-controller?
#[must_use]
pub fn is_nvme(f: &Function) -> bool {
    f.class == CLASS_NVME
}

#[cfg(test)]
mod tests {
    use super::*;
    use driver_pcie::Bdf;
    use std::cell::RefCell;
    use std::collections::HashMap;

    #[derive(Default)]
    struct FakeCfg(RefCell<HashMap<Bdf, [u32; 64]>>);

    impl FakeCfg {
        fn add(&self, bdf: Bdf, vendor: u16, device: u16, class: u32, bar0: u64) {
            let mut s = [0u32; 64];
            s[0] = u32::from(vendor) | (u32::from(device) << 16);
            s[2] = class << 8;
            s[4] = (bar0 as u32 & !0xf) | 0x4;
            s[5] = (bar0 >> 32) as u32;
            self.0.borrow_mut().insert(bdf, s);
        }
    }

    impl Config for FakeCfg {
        fn read32(&self, bdf: Bdf, off: u16) -> u32 {
            self.0
                .borrow()
                .get(&bdf)
                .map_or(u32::MAX, |s| s[usize::from(off / 4) % 64])
        }
        fn write32(&self, bdf: Bdf, off: u16, v: u32) {
            if let Some(s) = self.0.borrow_mut().get_mut(&bdf) {
                s[usize::from(off / 4) % 64] = v;
            }
        }
        fn write16(&self, _: Bdf, _: u16, _: u16) {}
    }

    #[test]
    fn igb_and_nvme_are_found_across_segments() {
        let seg0 = FakeCfg::default();
        seg0.add(
            Bdf::new(0, 1, 0).unwrap(),
            0x8086,
            0x1234,
            0x02_00_00,
            0x4000_0000,
        );
        let seg1 = FakeCfg::default();
        seg1.add(
            Bdf::new(0, 2, 0).unwrap(),
            0x8086,
            0x1533,
            0x02_00_00,
            0x3000_0010_0000,
        );
        seg1.add(
            Bdf::new(0, 3, 0).unwrap(),
            0x144d,
            0xa80a,
            CLASS_NVME,
            0x3000_0020_0000,
        );
        let nic = first([(&seg0, 0), (&seg1, 0)], 0, is_nic).unwrap();
        assert_eq!((nic.f.device, nic.bar), (0x1533, 0x3000_0010_0000));
        let disk = first([(&seg0, 0), (&seg1, 0)], 0, is_nvme).unwrap();
        assert_eq!(disk.bar, 0x3000_0020_0000);
        assert_eq!(core_class(127), CoreClass::Big);
    }
}
