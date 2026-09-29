//! Het registerblok van de VPU (Go: `hwreg.go`): veertien woorden voor de
//! hele VPU en vijftien per hardware-sessie. Alle codec-kennis zit in de
//! firmware, dus dit is echt alles wat er te programmeren valt.

use core::mem::{offset_of, size_of};
use dev::Reg;

/// Het globale blok plus de zestien sessievensters op 0x200.
#[repr(C)]
pub(crate) struct Block {
    /// 0x000: 0x5664 in de bovenste helft op de Linlon V8 van de O6N.
    pub(crate) hardware_id: Reg<u32>,
    /// 0x004: de hardware-scheduler aan of uit.
    pub(crate) enable: Reg<u32>,
    /// 0x008: hoeveel videocores het blok heeft.
    pub(crate) ncores: Reg<u32>,
    /// 0x00c: hoeveel sessies er tegelijk ingeladen kunnen zijn.
    pub(crate) nlsid: Reg<u32>,
    /// 0x010: welke core op welke sessie staat.
    pub(crate) core_lsid: Reg<u32>,
    /// 0x014: vier wachtende jobs, 8 bits elk.
    pub(crate) job_queue: Reg<u32>,
    /// 0x018: welke sessie de interrupt veroorzaakte.
    pub(crate) irq_ve: Reg<u32>,
    _r0: [u32; 2],
    /// 0x024: klokken forceren (diagnose).
    pub(crate) clk_force: Reg<u32>,
    _r1: [u32; 2],
    /// 0x030: revisie van het blok.
    pub(crate) svn_rev: Reg<u32>,
    /// 0x034: uitgeschakelde codecs.
    pub(crate) fuse: Reg<u32>,
    _r2: [u32; 2],
    /// 0x040: beveiligde modus (het DRM-pad; wij raken het niet aan).
    pub(crate) prot_ctrl: Reg<u32>,
    /// 0x044: burstgedrag op de AXI-bus.
    pub(crate) bus_ctrl: Reg<u32>,
    _r3: [u32; 2],
    /// 0x050: software-reset.
    pub(crate) reset: Reg<u32>,
    _r4: [u32; (0x200 - 0x54) / 4],
    /// 0x200 + i * 0x40: de vensters van de hardware-sessies.
    pub(crate) lsid: [Lsid; MAX_LSID],
}

/// Eén hardware-sessie (LSID).
#[repr(C)]
pub(crate) struct Lsid {
    /// Welke cores deze sessie mag en hoeveel.
    pub(crate) ctrl: Reg<u32>,
    /// De L1-tabel, als volledige PTE.
    pub(crate) mmu_ctrl: Reg<u32>,
    /// Niet-beveiligd (wij: altijd).
    pub(crate) nprot: Reg<u32>,
    /// 0 vrij, 1 gewoon, 2 beveiligd.
    pub(crate) alloc: Reg<u32>,
    /// De MMU-tabellen opnieuw lezen.
    pub(crate) flush_all: Reg<u32>,
    /// Deze sessie mag ingepland worden.
    pub(crate) sched: Reg<u32>,
    /// Afbreken; leest 0 zodra het klaar is.
    pub(crate) terminate: Reg<u32>,
    /// Interrupt naar ons; wissen na afhandeling.
    pub(crate) lirq_ve: Reg<u32>,
    /// Interrupt naar de firmware: de deurbel (kick).
    pub(crate) irq_host: Reg<u32>,
    _r: [u32; 2],
    /// De SMMU-stroom (blijft 0 zolang de SMMU bypast).
    pub(crate) stream_id: Reg<u32>,
    /// Busattributen.
    pub(crate) bus_attr: [Reg<u32>; 4],
}

/// De grootste NLSID die de driver aanneemt (Go: `nlsid > 16` weigert).
pub(crate) const MAX_LSID: usize = 16;

// De offsets uit de datasheet (en `mve_regs.h`), byte voor byte.
const _: () = {
    assert!(offset_of!(Block, hardware_id) == 0x00);
    assert!(offset_of!(Block, enable) == 0x04);
    assert!(offset_of!(Block, ncores) == 0x08);
    assert!(offset_of!(Block, nlsid) == 0x0c);
    assert!(offset_of!(Block, core_lsid) == 0x10);
    assert!(offset_of!(Block, job_queue) == 0x14);
    assert!(offset_of!(Block, irq_ve) == 0x18);
    assert!(offset_of!(Block, clk_force) == 0x24);
    assert!(offset_of!(Block, svn_rev) == 0x30);
    assert!(offset_of!(Block, fuse) == 0x34);
    assert!(offset_of!(Block, prot_ctrl) == 0x40);
    assert!(offset_of!(Block, bus_ctrl) == 0x44);
    assert!(offset_of!(Block, reset) == 0x50);
    assert!(offset_of!(Block, lsid) == LSID_BASE);
    assert!(size_of::<Lsid>() == LSID_STRIDE);
    assert!(offset_of!(Lsid, ctrl) == 0x00);
    assert!(offset_of!(Lsid, mmu_ctrl) == 0x04);
    assert!(offset_of!(Lsid, nprot) == 0x08);
    assert!(offset_of!(Lsid, alloc) == 0x0c);
    assert!(offset_of!(Lsid, flush_all) == 0x10);
    assert!(offset_of!(Lsid, sched) == 0x14);
    assert!(offset_of!(Lsid, terminate) == 0x18);
    assert!(offset_of!(Lsid, lirq_ve) == 0x1c);
    assert!(offset_of!(Lsid, irq_host) == 0x20);
    assert!(offset_of!(Lsid, stream_id) == 0x2c);
    assert!(offset_of!(Lsid, bus_attr) == 0x30);
    assert!(size_of::<Block>() == BLOCK_LEN);
};

/// Het eerste sessievenster.
pub(crate) const LSID_BASE: usize = 0x200;
/// De afstand tussen twee sessievensters.
pub(crate) const LSID_STRIDE: usize = 0x40;
/// De maat van het hele blok zoals dit crate het leest.
pub const BLOCK_LEN: usize = LSID_BASE + MAX_LSID * LSID_STRIDE;

/// De fuse-bits waar `supports` op beslist: een exemplaar met HEVC uitgezet
/// bestaat, en dat willen we weten vóór er firmware geladen wordt.
pub(crate) const FUSE_NO_VPX: u32 = 1 << 2;
/// Idem, HEVC.
pub(crate) const FUSE_NO_HEVC: u32 = 1 << 3;

/// LSID-toewijzing: vrij.
pub(crate) const ALLOC_FREE: u32 = 0;
/// LSID-toewijzing: gewoon (niet beveiligd).
pub(crate) const ALLOC_NON_PROTECTED: u32 = 1;

/// CTRL: 4 bits met het aantal cores dat de sessie tegelijk mag hebben.
pub(crate) const CTRL_MAX_CORES_SHIFT: u32 = 8;

/// De job-queue heeft vier plaatsen van 8 bits.
pub(crate) const JOB_SLOTS: u32 = 4;
/// Een lege plaats: lsid-nibble 0xf.
pub(crate) const JOB_INVALID: u32 = 0xf;
/// Alle vier de plaatsen leeg.
pub(crate) const EMPTY_JOB_QUEUE: u32 = 0x0f0f_0f0f;

/// Het sessienummer uit plaats `i` van de job-queue.
pub(crate) const fn job_slot_lsid(q: u32, i: u32) -> u32 {
    (q >> (i * 8)) & 0xf
}

/// Zet sessie en corecount in plaats `i`.
pub(crate) const fn set_job_slot(q: u32, i: u32, lsid: u32, ncores: u32) -> u32 {
    let job = (lsid & 0xf) | ((ncores & 0xf) << 4);
    (q & !(0xff << (i * 8))) | (job << (i * 8))
}
