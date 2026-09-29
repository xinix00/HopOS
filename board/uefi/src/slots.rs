//! Het slot-plan van het UEFI-board: waar de kern zijn kooien,
//! park-mailboxen en partities legt, en waar de stub een app-image van de
//! ESP neerzette.
//!
//! Dezelfde vorm als `board_qemuvirt::slots` (en de Pi's), zodat de
//! slot-lijm van de binary (`hopos/src/slots.rs`, `cage.rs`, `flip.rs`)
//! hetzelfde pad loopt. De vaste adressen liggen in het kernvenster
//! ([`crate::WINDOW_PA`]); de pool komt uit de EFI-memory-map na de exit:
//! al het vrije DRAM (BootServices en Conventional), op 2 MB afgerond.
//!
//! De staging: de stub leest `hopos-stage.elf` van de ESP-root naar
//! [`STAGE_PA`] en schrijft de maat in [`STAGE_HDR_PA`], zoals QEMU's
//! `-device loader` het op virt doet. De rol komt uit `hopos.cfg`:
//! `hopos.stage=app` (het ABI-bewijs van appspike) of `hopos.stage=hop`
//! (de standaard: Hop, de bevoorrechte bewoner, zoals op de Pi's).

use crate::memmap::{self, Map};
use crate::{ADMIN, LOADER, facts};
use abi::Region;
use abi::layout::{POOL_MAX, Plan, PlanSpec, Pool};
use core::sync::atomic::Ordering::Relaxed;
use dev::Pa;

/// De control-pages van de eigen cores van de kern: het begin van het
/// kooi-venster.
pub const NODE_CTRL_PA: u64 = ADMIN.base.0;
/// De kooi-regio: 1 MB verder, tot (SLOT_CAP + 1) blokken van 64 KB.
pub const CAGE_PA: u64 = ADMIN.base.0 + 0x10_0000;
/// Het Device-venster van de kooi.
pub const DEVICE_WINDOW: Region = Region::new(ADMIN.base.0, ADMIN.size);
/// De boot-scratch: het begin van de loader-regio.
pub const BOOT_SCRATCH_PA: u64 = LOADER.base.0;
/// Het woord met de maat van het gestagede image (0 = niets).
pub const STAGE_HDR_PA: u64 = LOADER.base.0 + 0x10_0000;
/// Het woord met de rol: 0 = app, 1 = Hop.
pub const STAGE_ROLE_PA: u64 = STAGE_HDR_PA + 8;
/// Waar het image staat.
pub const STAGE_PA: u64 = LOADER.base.0 + 0x20_0000;
/// Het grootste gestagede image.
pub const STAGE_MAX: u64 = LOADER.base.0 + LOADER.size - STAGE_PA;

const _: () = {
    assert!(BOOT_SCRATCH_PA + abi::layout::BOOT_SCRATCH_LEN <= STAGE_HDR_PA);
    assert!(STAGE_ROLE_PA + 8 <= STAGE_PA);
    // Node-control (SLOT_CAP + 1 pagina's) vóór de kooi, de kooi binnen het
    // venster, ook bij het volle plafond.
    let blocks = abi::layout::SLOT_CAP as u64 + 1;
    assert!(NODE_CTRL_PA + blocks * abi::layout::CTRL_STRIDE <= CAGE_PA);
    assert!(CAGE_PA + blocks * abi::layout::CAGE_STRIDE <= ADMIN.base.0 + ADMIN.size);
};

/// De memory map zoals hij bij de exit was.
pub(crate) fn final_map() -> Map {
    Map {
        pa: facts::MAP[0].load(Relaxed),
        size: facts::MAP[1].load(Relaxed),
        stride: facts::MAP[2].load(Relaxed),
    }
}

/// De pool: het vrije DRAM uit de kaart van de exit.
fn pool() -> abi::Result<Pool> {
    let (free, _dropped) = memmap::free_regions::<POOL_MAX>(&final_map());
    let mut pool = Pool::new();
    for r in free.iter() {
        pool.push(Region::new(r.base.0, r.size))
            .map_err(|_| abi::Error::TooMany {
                what: "pool regions",
                cap: POOL_MAX,
            })?;
    }
    Ok(pool)
}

/// Het laagste RAM-adres uit de kaart (het meetpunt van `required_ram`).
fn ram_base() -> u64 {
    final_map()
        .iter()
        .filter(memmap::Desc::is_ram)
        .map(|d| d.base)
        .min()
        .unwrap_or(0)
}

/// Het PA-plan voor een node met `cores` cores (de kern-core meegeteld):
/// elke andere core is een app-core met één kooi, en één kooi extra voor
/// de bewoner die de OS-core `os_core` deelt (Hop, PORT.md beslissing 2),
/// zoals op virt.
pub fn plan(cores: usize, os_core: usize) -> abi::Result<Plan> {
    let app_cores = cores.saturating_sub(1).max(1);
    let plan = Plan::new(PlanSpec {
        node_ctrl_pa: NODE_CTRL_PA,
        cage_pa: CAGE_PA,
        boot_scratch_pa: BOOT_SCRATCH_PA,
        net_dma_pa: crate::NET_DMA.base.0,
        pool: pool()?,
        ram_base: ram_base(),
        max_slots: app_cores + 1,
        app_cores,
        os_core,
        ..PlanSpec::default()
    })?;
    let blocks = plan.max_slots() as u64 + 1;
    let cage_end = CAGE_PA + blocks * abi::layout::CAGE_STRIDE;
    if cage_end > DEVICE_WINDOW.base + DEVICE_WINDOW.size {
        return Err(abi::Error::Overlap {
            a: Region::new(CAGE_PA, cage_end - CAGE_PA),
            b: DEVICE_WINDOW,
        });
    }
    Ok(plan)
}

/// Het MPIDR-target van logische core `core`: uit de MADT (core 0 is de
/// boot-core; op servers is MPIDR geen slotnummer, de Altra nummert aff1
/// en aff2).
#[must_use]
pub fn mpidr(core: usize) -> u64 {
    facts::CORE_MPIDR.get(core).map_or(0, |m| m.load(Relaxed))
}

/// De logische core met affiniteit `mpidr` (0 als hij onbekend is).
#[must_use]
pub fn core_of(mpidr: u64) -> usize {
    let aff = mpidr & 0xff_00ff_ffff;
    let n = facts::CORES.load(Relaxed).min(facts::MAX_CORES);
    (0..n).find(|&c| self::mpidr(c) == aff).unwrap_or(0)
}

/// Wat het gestagede image is: welke weg de kern ermee gaat.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum StagedRole {
    /// Een gewone app: de kern plaatst hem zelf, twee keer (het ABI-bewijs).
    App,
    /// Hop: de kern plaatst hem één keer, in slot 1, met de bevoegdheid.
    Hop,
}

/// De rol uit [`STAGE_ROLE_PA`]; een onbekend woord is `None`.
#[must_use]
pub fn staged_role() -> Option<StagedRole> {
    match dev::read64(Pa(STAGE_ROLE_PA)) {
        0 => Some(StagedRole::App),
        1 => Some(StagedRole::Hop),
        _ => None,
    }
}

/// Het image dat de stub van de ESP las, of `None`. Alleen de maat wordt
/// hier getoetst; de inhoud is onvertrouwd en gaat door de ELF-lezer en
/// `abi::place`.
#[must_use]
pub fn staged_image() -> Option<&'static [u8]> {
    let size = dev::read64(Pa(STAGE_HDR_PA));
    if size == 0 || size > STAGE_MAX {
        return None;
    }
    let len = usize::try_from(size).ok()?;
    // SAFETY: `[STAGE_PA, STAGE_PA + size)` ligt in de loader-regio van het
    // kernvenster (`size <= STAGE_MAX`, net getoetst): RAM van ons, Normal
    // gemapt, buiten de pool; na de stub schrijft niemand erin.
    Some(unsafe { core::slice::from_raw_parts(STAGE_PA as usize as *const u8, len) })
}

/// Mapt `[pa, pa + size)` als Device (zie [`crate::map_device`]).
pub(crate) fn map_device(pa: u64, size: u64) -> bool {
    if pa.saturating_add(size) <= crate::mmu::DEVICE_SPAN {
        return true;
    }
    let lo = pa & !((1 << 30) - 1);
    let hi = pa.saturating_add(size).next_multiple_of(1 << 30);
    let used = facts::MMU_TABLES.load(Relaxed);
    // SAFETY: de tabelpool is `crate::TABLES` en de eerste `used` tabellen
    // zijn de levende map (de stub bouwde hem daar); alleen de kern-core
    // schrijft hem, en alleen via deze functie.
    let Ok(mut m) =
        (unsafe { crate::mmu::Mmu::resume(crate::TABLES.base.0, crate::TABLES.size / 4096, used) })
    else {
        return false;
    };
    let ok = m
        .map(
            lo,
            hi.saturating_sub(lo),
            crate::mmu::attrs(cpu::boot::ATTR_DEVICE),
        )
        .is_ok();
    facts::MMU_TABLES.store(m.tables(), Relaxed);
    crate::arch::tlbi_all();
    ok
}
