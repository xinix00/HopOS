//! Het slot-plan van de Pi's: waar de kern zijn kooien, park-mailboxen en
//! partities fysiek legt, en waar de firmware het image van Hop neerzette.
//!
//! Dezelfde vorm als `board_qemuvirt::slots`, zodat de slot-lijm van de
//! binary (`hopos/src/slots.rs`, `cage.rs`) hetzelfde pad loopt; alleen
//! `mpidr` verschilt per SoC en staat daarom in het board-crate.
//!
//! De staging: config.txt laadt het image als `initramfs` op
//! [`STAGE_PA`](crate::map::STAGE_PA) en de firmware schrijft het bereik in
//! `/chosen/linux,initrd-*`. De rol komt uit de cmdline:
//! `hopos.stage=app` (het ABI-bewijs van appspike) of `hopos.stage=hop`
//! (de standaard: Hop, de bevoorrechte bewoner).

use crate::map;
use abi::Region;
use abi::layout::{Plan, PlanSpec, Pool};
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use sync::LocalCell;

pub use crate::map::{
    BOOT_SCRATCH_PA, CAGE_PA, DEVICE_WINDOW, NODE_CTRL_PA, STAGE_HDR_PA, STAGE_MAX, STAGE_PA,
    STAGE_ROLE_PA,
};

/// De pool uit de DTB; `discover` vult hem, `plan` leest hem.
pub(crate) static POOL: LocalCell<Pool> = LocalCell::cell(Pool::new());

/// Het gestagede image: begin en lengte (0 = geen), gezet door `discover`.
pub(crate) static STAGE: [AtomicU64; 2] = [AtomicU64::new(0), AtomicU64::new(0)];

/// De rol uit de cmdline: 0 = app, 1 = Hop, anders onbekend.
pub(crate) static ROLE: AtomicU64 = AtomicU64::new(1);

/// Het PA-plan voor een node met `cores` cores (de kern-core meegeteld):
/// elke andere core is een app-core, en elke app-core krijgt één kooi; één
/// kooi extra voor de bewoner die de OS-core `os_core` deelt (Hop, PORT.md
/// beslissing 2), zoals op QEMU virt.
pub fn plan(cores: usize, os_core: usize) -> abi::Result<Plan> {
    let app_cores = cores.saturating_sub(1).max(1);
    let pool = POOL.borrow().clone();
    let plan = Plan::new(PlanSpec {
        node_ctrl_pa: NODE_CTRL_PA,
        cage_pa: CAGE_PA,
        boot_scratch_pa: BOOT_SCRATCH_PA,
        net_dma_pa: map::NET_DMA.base,
        pool,
        ram_base: 0,
        max_slots: app_cores + 1,
        app_cores,
        os_core,
        ..PlanSpec::default()
    })?;
    // De kooi-regio moet helemaal in het Device-venster vallen: een
    // gecachte park-mailbox is op ijzer een verloren startschot.
    let blocks = plan.max_slots() as u64 + 1;
    let cage_end = CAGE_PA + blocks * abi::layout::CAGE_STRIDE;
    let window_end = DEVICE_WINDOW.base + DEVICE_WINDOW.size;
    if cage_end > window_end {
        return Err(abi::Error::Overlap {
            a: Region::new(CAGE_PA, cage_end - CAGE_PA),
            b: DEVICE_WINDOW,
        });
    }
    Ok(plan)
}

/// Wat het gestagede image is: welke weg de kern ermee gaat.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum StagedRole {
    /// Een gewone app: de kern plaatst hem zelf, twee keer (het ABI-bewijs).
    App,
    /// Hop: de kern plaatst hem één keer, in slot 1, met de bevoegdheid.
    Hop,
}

/// De rol van de staging; een onbekende `hopos.stage` is `None` (de kern
/// plaatst dan niets en zegt dat luid).
#[must_use]
pub fn staged_role() -> Option<StagedRole> {
    match ROLE.load(Relaxed) {
        0 => Some(StagedRole::App),
        1 => Some(StagedRole::Hop),
        _ => None,
    }
}

/// De rolcode van een `hopos.stage`-waarde: leeg of `hop` is Hop.
#[must_use]
pub fn role_code(v: &str) -> u64 {
    match v {
        "" | "hop" => 1,
        "app" => 0,
        _ => 2,
    }
}

/// Het image dat de firmware vóór de boot neerlegde, of `None`. Alleen de
/// maat is getoetst (in `discover`: binnen het laadvenster); de inhoud is
/// onvertrouwd en gaat door de ELF-lezer en `abi::place`.
#[must_use]
pub fn staged_image() -> Option<&'static [u8]> {
    let start = STAGE[0].load(Relaxed);
    let len = STAGE[1].load(Relaxed);
    if start == 0 || len == 0 {
        return None;
    }
    crate::arch::stage_slice(start, len)
}

/// Toetst een initrd-bereik: binnen het laadvenster, niet leeg, niet te
/// groot. Geeft begin en lengte.
#[must_use]
pub fn check_stage(start: u64, end: u64) -> Option<(u64, u64)> {
    let len = end.checked_sub(start)?;
    let lo = map::LOADER.base;
    let hi = map::LOADER.base + map::LOADER.size;
    (len > 0 && len <= STAGE_MAX + (STAGE_PA - lo) && start >= lo && end <= hi)
        .then_some((start, len))
}
