//! De PCIe-zoektocht van een UEFI-machine met eigen drivers (de O6N, de
//! Altra): de ECAM-vensters uit de MCFG, de eerste functie met een
//! toegewezen BAR, en de NVMe. Generiek over een [`Config`]-venster, zodat
//! de zoektocht op de host te toetsen is.
//!
//! De firmware configureerde de hiërarchie en wees de BAR's toe: we lezen
//! alleen (Go: "hem overschrijven zou de controller op PA 0 zetten"; op de
//! Altra een data-abort).

use crate::BLK_DMA;
use board::Error;
use bounded::BoundedVec;
use dev::Pa;
use driver_nvme::Nvme;
use driver_pcie::{Config, Ecam, Function, find};

/// Zoveel ECAM-vensters: de Altra heeft er tot acht, de O6N vijf.
pub const MAX_SEGMENTS: usize = 8;

/// De klassecode van een NVMe-controller: mass storage, NVM, NVMe.
pub const CLASS_NVME: u32 = 0x01_08_02;

/// De ECAM-vensters met hun eerste bus.
pub type Segments = BoundedVec<(Ecam, u8), MAX_SEGMENTS>;

/// De ECAM-vensters uit de MCFG (de eerste [`MAX_SEGMENTS`]).
#[must_use]
pub fn segments() -> Segments {
    let mut v = BoundedVec::new();
    for (e, _seg, start) in crate::ecams() {
        if v.push((e, start)).is_err() {
            break;
        }
    }
    v
}

/// Een gevonden device: de functie, de BAR die de driver wil, het venster
/// en zijn eerste bus (op de O6N heeft elke root-poort zijn eigen venster;
/// de INTx-lijn hangt eraan).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Found {
    /// De functie.
    pub f: Function,
    /// Het adres van de BAR.
    pub bar: u64,
    /// De index van het venster waarin hij gevonden is.
    pub win: usize,
    /// De eerste bus van dat venster.
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
    windows
        .into_iter()
        .enumerate()
        .find_map(|(win, (c, start))| {
            find(c, start, |f| {
                !f.is_bridge() && is(f) && f.bar_addr(c, bar) != 0
            })
            .map(|f| Found {
                f,
                bar: f.bar_addr(c, bar),
                win,
                root_bus: start,
            })
        })
}

/// De eerste functie over `segs` (zie [`first`]).
#[must_use]
pub fn first_in(segs: &Segments, bar: u8, is: impl Fn(&Function) -> bool) -> Option<Found> {
    first(segs.as_slice().iter().map(|(e, s)| (e, *s)), bar, is)
}

/// Vindt en initialiseert de eerste NVMe (het hele device) in [`BLK_DMA`].
/// `Ok(None)` = geen NVMe. Eén keer: de aanroeper bewaakt dat.
pub fn probe_nvme() -> Result<Option<Nvme>, Error> {
    let segs = segments();
    let Some(hit) = first_in(&segs, 0, |f| f.class == CLASS_NVME) else {
        return Ok(None);
    };
    // Een hoge BAR (boven 1 TB, de Altra) moet eerst in de identity map.
    if !crate::map_device(hit.bar, driver_nvme::MMIO_LEN) {
        return Err(Error::Disk("NVMe BAR0 unreachable"));
    }
    if let Some((e, _)) = segs.get(hit.win) {
        hit.f.enable(e);
    }
    // SAFETY: BAR0 is door de firmware toegewezen en nu Device-gemapt
    // (`map_device`); memory-decode en bus-mastering staan aan. BLK_DMA is
    // van deze driver alleen, Normal-NC gemapt door de stub.
    let disk = unsafe { Nvme::new(Pa(hit.bar), BLK_DMA.base, BLK_DMA.size, cpu::idle::now) }
        .map_err(|e| {
            cpu::println!("disk: {e} HOPOS_NVME_FAIL");
            Error::Disk("nvme init failed")
        })?;
    let (major, minor) = disk.version();
    cpu::println!(
        "disk: nvme {} at {} bar0 {:#x}, NVMe {major}.{minor}, {} blocks of {} bytes, max transfer {} HOPOS_NVME_UP",
        disk.model(),
        hit.f.bdf,
        hit.bar,
        disk.blocks(),
        disk.block_size(),
        disk.max_transfer()
    );
    Ok(Some(disk))
}
