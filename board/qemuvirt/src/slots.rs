//! Het slot-plan van QEMU virt: waar de kern van dit board zijn kooien,
//! park-mailboxen, switch-code en partities fysiek legt, en waar QEMU een
//! app-image voor de eerste plaatsing neerzet.
//!
//! De adressen zijn die van de Go-kern (`OLD/metal/board/qemuvirt`): BEWUST
//! verschoven ten opzichte van de IPA-constanten van de apps (de kooi-regio
//! op 0xC200_0000 terwijl apps hun staart op 0x5000_0000+ zien, en een pool
//! in twee losse regio's). Op QEMU is identity de valkuil die IPA/PA-
//! verwisselingen verhult; ongelijk gemaakt knalt elke verwisseling in de
//! regressie in plaats van pas op een board. Vereist `-m 3G`.
//!
//! Dit bezit alleen getallen en de ene lezer van de staging. Welk slot
//! waar draait is van de lifecycle-actor van de kern.

use abi::Region;
use abi::layout::{Plan, PlanSpec, Pool};

/// De control-pages van de eigen cores van de kern (node-SMP).
pub const NODE_CTRL_PA: u64 = 0xC000_0000;
/// De kooi-regio: blok 0 draagt de EL2-vectoren van de app-cores, de
/// parkeerlus, de sched-blokken en de switch-code; blok i de stage-2 en de
/// contexten van slot i.
pub const CAGE_PA: u64 = 0xC200_0000;
/// Het venster dat de identity map van de kern als Device mapt: de
/// control-pages en de kooi-regio. De app-cores lezen dit op EL2 met de MMU
/// uit (dus langs elke cache heen), en `cpu::el2` rekent erop dat de
/// park-mailbox coherent is zonder veeg (dispatch.rs `core_state`).
pub const DEVICE_WINDOW: Region = Region::new(0xC000_0000, 0x0400_0000);
/// De boot-scratch: buiten elke pool, zoals in Go (`BootScratchPA`).
pub const BOOT_SCRATCH_PA: u64 = 0xB000_0000;
/// De partitie-pool: het klassieke venster van 1,5 GB en een tweede regio
/// van 240 MB die bewijst dat de pool meer dan één stuk aankan.
pub const POOL: [Region; 2] = [
    Region::new(0x5000_0000, 0x6000_0000),
    Region::new(0xB100_0000, 0x0F00_0000),
];

/// Het woord waarin QEMU de maat van het gestagede image legt
/// (`-device loader,addr=…,data=<maat>,data-len=8` in image/qemu-run.sh).
/// Nul = er is niets gestaged.
pub const STAGE_HDR_PA: u64 = 0xB010_0000;
/// Waar QEMU het image rauw neerlegt (`-device loader,file=…,addr=…,
/// force-raw=on`): tussen de boot-scratch en de tweede pool-regio, dus
/// buiten alles wat de lifecycle uitdeelt of wist.
pub const STAGE_PA: u64 = 0xB020_0000;
/// De grootste staging: tot aan de tweede pool-regio.
pub const STAGE_MAX: u64 = 0xB100_0000 - STAGE_PA;

const _: () = assert!(STAGE_HDR_PA + 8 <= STAGE_PA);
const _: () = assert!(BOOT_SCRATCH_PA + abi::layout::BOOT_SCRATCH_LEN <= STAGE_HDR_PA);
const _: () = assert!(NODE_CTRL_PA >= DEVICE_WINDOW.base && CAGE_PA > NODE_CTRL_PA);

/// Het PA-plan voor een node met `cores` cores (de kern-core meegeteld): elke
/// andere core is een app-core, en elke app-core krijgt één kooi.
pub fn plan(cores: usize) -> abi::Result<Plan> {
    let app_cores = cores.saturating_sub(1).max(1);
    let mut pool = Pool::new();
    for r in POOL {
        pool.push(r).map_err(|_| abi::Error::TooMany {
            what: "pool regions",
            cap: abi::layout::POOL_MAX,
        })?;
    }
    let plan = Plan::new(PlanSpec {
        node_ctrl_pa: NODE_CTRL_PA,
        cage_pa: CAGE_PA,
        boot_scratch_pa: BOOT_SCRATCH_PA,
        pool,
        max_slots: app_cores,
        app_cores,
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

/// Het MPIDR-target van core `core` op virt: `hw/arm/virt.c` legt bij een
/// GICv3 zestien cores per cluster (aff0), daarboven aff1.
#[must_use]
pub const fn mpidr(core: usize) -> u64 {
    ((core % 16) as u64) | (((core / 16) as u64) << 8)
}

/// Het image dat QEMU vóór de boot neerlegde, of `None` als het maatwoord
/// nul of onzin is. Alleen de maat wordt hier getoetst; de inhoud is
/// onvertrouwd en gaat door de ELF-lezer en `abi::place`.
#[must_use]
pub fn staged_image() -> Option<&'static [u8]> {
    imp::stage()
}

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
mod imp {
    use super::{STAGE_HDR_PA, STAGE_MAX, STAGE_PA};
    use dev::Pa;

    pub(super) fn stage() -> Option<&'static [u8]> {
        let size = dev::read64(Pa(STAGE_HDR_PA));
        if size == 0 || size > STAGE_MAX {
            return None;
        }
        let len = usize::try_from(size).ok()?;
        // SAFETY: `[STAGE_PA, STAGE_PA + size)` ligt binnen de staging
        // (`size <= STAGE_MAX`, net getoetst): RAM dat de
        // identity map als Normal mapt, buiten de pool en buiten de
        // kern-RAM, dus niemand schrijft erin nadat QEMU het vulde. Alleen
        // lezen.
        Some(unsafe { core::slice::from_raw_parts(STAGE_PA as usize as *const u8, len) })
    }
}

#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
mod imp {
    //! Host-stub: er is geen QEMU die iets neerlegde.
    pub(super) fn stage() -> Option<&'static [u8]> {
        None
    }
}
