//! Het slot-plan van QEMU virt (riscv64): waar de kern zijn kooi-regio,
//! control-pages, partities en de staging van een app-image legt. Vereist
//! `-m 1G` (DRAM 0x8000_0000 tot 0xC000_0000).
//!
//! In machine mode is er geen vertaling: elk adres hier is fysiek en
//! cachebaar, en QEMU is coherent. De kooi-regio hoeft dus niet in een
//! Device-venster zoals op arm64; op de C906 is hij gecachet en gaat elk
//! woord dat twee harts delen door `dev::push`/`pull` (de regel van de
//! schrijvers per cacheline, `abi::layout::SchedBlock`).
//!
//! De bovenkant van het DRAM blijft vrij: QEMU legt de DTB in de laatste
//! 2 MB-korrel onder het einde van het RAM.

use abi::Region;
use abi::layout::{Plan, PlanSpec, Pool};

/// De control-pages van de eigen harts van de kern.
pub const NODE_CTRL_PA: u64 = 0xB000_0000;
/// De kooi-regio: blok 0 de sched-blokken en de switch-code, blok i het
/// ctx-blok van slot i.
pub const CAGE_PA: u64 = 0xB200_0000;
/// De boot-scratch, buiten elke pool.
pub const BOOT_SCRATCH_PA: u64 = 0xA800_0000;
/// De partitie-pool: 384 MB. Het eerste adres is ook het linkadres van elk
/// riscv64-app-image (`cage_riscv.rs`: de Sv39-tabel legt het op de echte
/// partitie van het slot, zoals `cageLinkBase` in Go).
pub const POOL: [Region; 1] = [Region::new(0x9000_0000, 0x1800_0000)];
/// Het maatwoord van de staging (`image/qemu-riscv-run.sh`).
pub const STAGE_HDR_PA: u64 = 0xA810_0000;
/// Het rolwoord van de staging (0 = app, 1 = Hop).
pub const STAGE_ROLE_PA: u64 = STAGE_HDR_PA + 8;
/// Waar QEMU het image rauw neerlegt.
pub const STAGE_PA: u64 = 0xA820_0000;
/// De grootste staging: 14 MB, zoals op arm64.
pub const STAGE_MAX: u64 = 0xA900_0000 - STAGE_PA;

const _: () = assert!(STAGE_ROLE_PA + 8 <= STAGE_PA);
const _: () = assert!(BOOT_SCRATCH_PA + abi::layout::BOOT_SCRATCH_LEN <= STAGE_HDR_PA);
const _: () = assert!(POOL[0].base + POOL[0].size <= BOOT_SCRATCH_PA);
const _: () = assert!(CAGE_PA > NODE_CTRL_PA);

/// Het PA-plan: hart `os_core` is de kern, elk ander hart een app-hart met
/// één kooi, plus één kooi voor Hop.
pub fn plan(cores: usize, os_core: usize) -> abi::Result<Plan> {
    let app_cores = cores.saturating_sub(1).max(1);
    let mut pool = Pool::new();
    for r in POOL {
        pool.push(r).map_err(|_| abi::Error::TooMany {
            what: "pool regions",
            cap: abi::layout::POOL_MAX,
        })?;
    }
    Plan::new(PlanSpec {
        node_ctrl_pa: NODE_CTRL_PA,
        cage_pa: CAGE_PA,
        boot_scratch_pa: BOOT_SCRATCH_PA,
        pool,
        ram_base: 0x8000_0000,
        max_slots: app_cores + 1,
        app_cores,
        os_core,
        ..PlanSpec::default()
    })
}

/// Het "MPIDR" van een hart op riscv: het hart-id zelf. De gedeelde lijm
/// van `hopos` rekent met die naam.
#[must_use]
pub const fn mpidr(core: usize) -> u64 {
    core as u64
}

/// Het hart bij een id: de inverse van [`mpidr`].
#[must_use]
pub const fn core_of(mpidr: u64) -> usize {
    mpidr as usize
}

/// Het image dat QEMU neerlegde, of `None`.
#[must_use]
pub fn staged_image() -> Option<&'static [u8]> {
    imp::stage()
}

/// Wat het gestagede image is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum StagedRole {
    /// Een gewone app (het ABI-bewijs).
    App,
    /// Hop.
    Hop,
}

/// De rol uit [`STAGE_ROLE_PA`].
#[must_use]
pub fn staged_role() -> Option<StagedRole> {
    match imp::role() {
        0 => Some(StagedRole::App),
        1 => Some(StagedRole::Hop),
        _ => None,
    }
}

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
mod imp {
    use super::{STAGE_HDR_PA, STAGE_MAX, STAGE_PA, STAGE_ROLE_PA};
    use dev::Pa;

    pub(super) fn role() -> u64 {
        dev::read64(Pa(STAGE_ROLE_PA))
    }

    pub(super) fn stage() -> Option<&'static [u8]> {
        let size = dev::read64(Pa(STAGE_HDR_PA));
        if size == 0 || size > STAGE_MAX {
            return None;
        }
        let len = usize::try_from(size).ok()?;
        // SAFETY: `[STAGE_PA, STAGE_PA + size)` ligt binnen de staging
        // (net getoetst): DRAM buiten de pool en de kern-RAM, dat niemand
        // beschrijft nadat QEMU het vulde. Alleen lezen.
        Some(unsafe { core::slice::from_raw_parts(STAGE_PA as usize as *const u8, len) })
    }
}

#[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
mod imp {
    //! Host-stub: er is geen QEMU die iets neerlegde.
    pub(super) fn stage() -> Option<&'static [u8]> {
        None
    }
    pub(super) fn role() -> u64 {
        0
    }
}

// --- De kern-flip (hopos/src/flip.rs, docs/flip.md) ---------------------
//
// Alleen getallen, zoals op arm64 (board/qemuvirt/src/slots.rs). De flip is
// op riscv64 niet bewezen (docs/boards-riscv.md): de sprong zelf
// (`cpu::el2::chain`) is arm64. Deze namen laten de gedeelde lijm bouwen.

/// Het koude linkadres (`hopos/link-riscv.ld`, `KERN_BASE`).
pub const FLIP_LINK_BASE: u64 = 0x8000_0000;
/// Geen PIE.
pub const FLIP_PIE: bool = false;
/// Het einde van het beeld: het begin van de DMA-regio.
pub const FLIP_IMAGE_END: u64 = 0x8f00_0000;
/// De staging van het platte beeld.
pub const FLIP_STAGE_PA: u64 = STAGE_PA;
/// De grootste staging.
pub const FLIP_STAGE_MAX: u64 = STAGE_MAX;
/// De vluchtrecorder.
pub const FLIP_RECORDER_PA: u64 = BOOT_SCRATCH_PA + 0x1000;
/// De trampoline van de sprong.
pub const FLIP_TRAMP_PA: u64 = BOOT_SCRATCH_PA + 0x2000;
/// De maat van het handoff-blob.
pub const FLIP_HANDOFF_LEN: u64 = 0x4_0000;
/// Het handoff-blob: direct onder het staging-maatwoord.
pub const FLIP_HANDOFF_PA: u64 = STAGE_HDR_PA - FLIP_HANDOFF_LEN;

const _: () = {
    assert!(FLIP_RECORDER_PA >= BOOT_SCRATCH_PA + abi::layout::BOOT_SCRATCH_LEN);
    assert!(FLIP_TRAMP_PA + 0x1000 <= FLIP_HANDOFF_PA);
    assert!(FLIP_HANDOFF_PA + FLIP_HANDOFF_LEN <= STAGE_HDR_PA);
};

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_plan_holds_for_two_harts() {
        let p = plan(2, 0).unwrap();
        assert_eq!(p.app_cores(), 1);
        assert_eq!(p.max_slots(), 2);
        assert_eq!(core_of(mpidr(1)), 1);
    }
}
