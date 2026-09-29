//! Wat de O6N op zijn PCIe-bussen zoekt: de Realtek-NIC (de eerste
//! ondersteunde poort, geen twee-poorts-aggregatie) en de NVMe (klasse
//! 01:08:02, het hele device voor HopOS). Generiek over een
//! [`Config`]-venster, zodat de zoektocht op de host te toetsen is; welke
//! vensters er zijn, zegt de MCFG via `board-uefi`.
//!
//! De firmware (UEFI) configureerde de hiërarchie en wees de BAR's toe: we
//! lezen alleen (Go: "hem overschrijven zou de controller op PA 0 zetten").

use driver_pcie::{Config, Function, find};

/// De klassecode van een NVMe-controller: mass storage, NVM, NVMe.
pub const CLASS_NVME: u32 = 0x01_08_02;

/// Een gevonden device: de functie, de BAR die de driver wil, en de eerste
/// bus van het ECAM-venster (op de O6N heeft elke root-poort zijn eigen
/// venster; de INTx-lijn hangt eraan).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Found {
    /// De functie.
    pub f: Function,
    /// Het adres van de BAR.
    pub bar: u64,
    /// De eerste bus van het venster.
    pub root_bus: u8,
}

/// Zoekt over `windows` (config, eerste bus) de eerste functie waarop `is`
/// ja zegt, met een toegewezen memory-BAR `bar`. Een functie waarvan de BAR
/// nul is, telt niet: de firmware wees hem niet toe, en hem zelf toewijzen
/// doen we op een UEFI-machine niet.
pub fn first<'a, C: Config + 'a>(
    windows: impl IntoIterator<Item = (&'a C, u8)>,
    bar: u8,
    is: impl Fn(&Function) -> bool,
) -> Option<Found> {
    for (c, start) in windows {
        let hit = find(c, start, |f| {
            !f.is_bridge() && is(f) && f.bar_addr(c, bar) != 0
        });
        if let Some(f) = hit {
            return Some(Found {
                f,
                bar: f.bar_addr(c, bar),
                root_bus: start,
            });
        }
    }
    None
}

/// De NIC-interrupt: INTx van de root-poort waar de NIC achter hangt, als
/// GIC-INTID. Bron: de mainline-DT (sky1.dtsi interrupt-map, GIC_SPI n is
/// INTID n+32): x1_0 (bus 0x00, LAN 1) SPI 436, x1_1 (0x30, LAN 2) 445, x2
/// (0x60) 427, x4 (0x90) 417, x8 (0xc0) 407; alle INTA, level-high. De
/// Cix-DSDT noemt dezelfde lijnen in haar `_PRT`. GEMETEN (L80): 477 voor de
/// NIC achter bus 0x30, één interrupt per ontvangen frame, rtt p50 156-201
/// µs tegen 160-168 gepold.
#[must_use]
pub fn nic_intid(root_bus: u8) -> Option<u32> {
    let spi = match root_bus {
        0x00 => 436,
        0x30 => 445,
        0x60 => 427,
        0x90 => 417,
        0xc0 => 407,
        _ => return None,
    };
    Some(spi + 32)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use driver_pcie::Bdf;
    use std::cell::RefCell;
    use std::collections::HashMap;

    /// Een config-space in RAM: per functie 64 dwords.
    #[derive(Default)]
    pub(crate) struct FakeCfg {
        pub(crate) f: RefCell<HashMap<Bdf, [u32; 64]>>,
    }

    impl FakeCfg {
        pub(crate) fn add(&self, bdf: Bdf, vendor: u16, device: u16, class: u32, bar2: u64) {
            let mut s = [0u32; 64];
            s[0] = u32::from(vendor) | (u32::from(device) << 16);
            s[2] = class << 8;
            s[4] = 0x0000_0004; // BAR0: 64-bit mem, niet toegewezen
            s[6] = (bar2 as u32 & !0xf) | 0x4; // BAR2: 64-bit
            s[7] = (bar2 >> 32) as u32;
            self.f.borrow_mut().insert(bdf, s);
        }

        pub(crate) fn bridge(&self, bdf: Bdf, secondary: u8) {
            let mut s = [0u32; 64];
            s[0] = 0x8000_1def;
            s[2] = 0x06_04_00 << 8;
            s[3] = 1 << 16;
            s[6] = u32::from(secondary) << 8;
            self.f.borrow_mut().insert(bdf, s);
        }
    }

    impl Config for FakeCfg {
        fn read32(&self, bdf: Bdf, off: u16) -> u32 {
            self.f
                .borrow()
                .get(&bdf)
                .map_or(u32::MAX, |s| s[usize::from(off / 4) % 64])
        }
        fn write32(&self, bdf: Bdf, off: u16, v: u32) {
            if let Some(s) = self.f.borrow_mut().get_mut(&bdf) {
                s[usize::from(off / 4) % 64] = v;
            }
        }
        fn write16(&self, bdf: Bdf, off: u16, v: u16) {
            if let Some(s) = self.f.borrow_mut().get_mut(&bdf) {
                let w = &mut s[usize::from(off / 4) % 64];
                let sh = (off & 2) * 8;
                *w = (*w & !(0xffff << sh)) | (u32::from(v) << sh);
            }
        }
    }

    fn bdf(bus: u8, dev: u8) -> Bdf {
        Bdf::new(bus, dev, 0).unwrap()
    }

    #[test]
    fn the_first_realtek_behind_a_root_port_is_found_with_its_line() {
        // Twee vensters zoals op de O6N: een lege x1_0 en x1_1 met de NIC.
        let a = FakeCfg::default();
        a.bridge(bdf(0x00, 0), 0x01);
        let b = FakeCfg::default();
        b.bridge(bdf(0x30, 0), 0x31);
        b.add(
            bdf(0x31, 0),
            0x10ec,
            0x8125,
            0x02_00_00,
            0x0000_0060_0000_0000,
        );
        let hit = first([(&a, 0x00), (&b, 0x30)], 2, |f| {
            driver_rtl8126::supported(f.vendor, f.device)
        })
        .unwrap();
        assert_eq!(
            (hit.f.bdf, hit.bar, hit.root_bus),
            (bdf(0x31, 0), 0x60_0000_0000, 0x30)
        );
        assert_eq!(nic_intid(hit.root_bus), Some(477));
        assert_eq!(nic_intid(0x10), None);
    }

    #[test]
    fn an_unassigned_bar_or_the_wrong_class_is_skipped() {
        let c = FakeCfg::default();
        c.add(bdf(0, 1), 0x10ec, 0x8126, 0x02_00_00, 0); // BAR2 niet toegewezen
        c.add(bdf(0, 2), 0x144d, 0xa808, CLASS_NVME, 0x5000_0000);
        assert!(
            first([(&c, 0)], 2, |f| driver_rtl8126::supported(
                f.vendor, f.device
            ))
            .is_none()
        );
        let nvme = first([(&c, 0)], 2, |f| f.class == CLASS_NVME).unwrap();
        assert_eq!(nvme.f.device, 0xa808);
    }
}
