//! Kooi-spoor: EL2, stage-2 en de switcher.
//!
//! Wat er staat: [`stage2`], het bouwen, intrekken en het venster-grant van
//! de stage-2-tabellen van een kooi (pure geheugenrekenkunde over een blok,
//! op de host getest; de Go-tests uit `OLD/metal/kern/stage2` zijn geport);
//! de switcher zelf (`switch.rs`: de context-wissel op EL2, de rotatie over
//! bewoners op één core, de HVC-handler, de twee trampolines en de
//! parkeerlus, als `global_asm!`); en de Rust-kant daarvan (`dispatch.rs`):
//! de switch-code naar de plan-regio met descriptor en som, de thunks en
//! sched-blokken, [`dispatch`], [`kick`], [`revoke`], [`prepare_smp`],
//! [`adopt`] en de lezers van het ctx-blok; en de OS-core (`oscore.rs`,
//! PORT.md beslissing 2): de kern op EL2 als eerste bewoner van zijn eigen
//! core, die zijn idle aan de andere bewoners geeft ([`OsCore`], [`host`])
//! en ze terugneemt op elke interrupt, kick of deadline.
//!
//! De kern van v3 draait zélf op EL2 (de boot-stub blijft daar), dus een
//! intrekking is geen hypercall meer maar een TLB-invalidatie ter plekke:
//! zie [`arch::hvc_revoke`], dat zijn Go-naam houdt zodat `stage2` leesbaar
//! naast zijn voorganger ligt. Hetzelfde geldt voor de Apple-kick (in Go
//! HVC #3): de kern schrijft het IPI-register zelf.

pub mod chain;
mod dispatch;
mod layout;
mod oscore;
pub mod stage2;
#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod switch;
#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
#[path = "switch_host.rs"]
mod switch;

pub use dispatch::{
    CoreState, Flavor, HVC_DOOR_ACK, HVC_EXIT, HVC_KICK_OS, HVC_WAKE, HVC_YIELD, Installed,
    MAX_BLOB, SW_ENTRY, SW_HASH, SW_HEAD, SW_LEN, SW_SMP, SW_TRAMP, SWITCH_MAGIC, Start, VEC_COUNT,
    VEC_FIQ_LOWER, VEC_STRIDE, VEC_SYNC_LOWER, VEC_TABLE_LEN, adopt, apple_ipi_target, arm_context,
    check_parked, context_id, context_pa, core_state, ctx_read, ctx_state, ctx_write, dispatch,
    image_hash, init_app_cores, install_switch_code, installed_hash, kick, prepare_smp, revoke,
    rx_due, wake_due,
};
pub use oscore::{
    Back, Bell, OsCore, SCHED_OS_KICK, SCHED_OS_KICK_PA, STATS as OS_STATS, Stats as OsStats,
    TURN_CAP_NS, Turn, held, hold, host, hosts, rehost, release_held, unhost,
};

use core::fmt;

/// Waarom een kooi niet gebouwd of niet uitgebreid is. Elke variant draagt
/// de getallen, want "misaligned" zonder adres is niets waard op een
/// headless node.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// IPA-basis, PA-basis of maat is geen veelvoud van het 2 MB-blok.
    Misaligned {
        /// De gevraagde IPA-basis.
        ipa: u64,
        /// De gevraagde PA-basis.
        pa: u64,
        /// De gevraagde maat.
        size: u64,
    },
    /// Het IPA-venster valt buiten het 39-bit-regime of onder de eerste GB.
    IpaWindow {
        /// De gevraagde IPA-basis.
        ipa: u64,
        /// De gevraagde maat.
        size: u64,
    },
    /// Het fysieke bereik (plus de tabelreserve) past niet in de PA-ruimte.
    PaSpace {
        /// De gevraagde PA-basis.
        pa: u64,
        /// De gevraagde maat.
        size: u64,
    },
    /// Het grant-venster is leeg, niet pagina-gealigneerd of te groot.
    Grant {
        /// De fysieke basis van het venster.
        pa: u64,
        /// De maat van het venster.
        size: u64,
    },
    /// Op de GB-ingang van het grant-venster staat al iets anders.
    GrantCollides {
        /// De GB-index in de L1.
        gb: u64,
        /// De descriptor die er al stond.
        entry: u64,
    },
    /// Deze build heeft geen EL2-code (de host).
    NoSwitchCode,
    /// Een blob tussen zijn markers heeft een onmogelijke maat: de linker
    /// trok ze uit elkaar of een marker is verschoven.
    Blob {
        /// Welke blob (0 switcher, 1 trampoline, 2 SMP, 3 parkeerlus).
        index: usize,
        /// De gemeten maat.
        len: usize,
    },
    /// De switch-code-kopie past niet in zijn plek in blok 0.
    SwitchCodeFull {
        /// Hoeveel bytes er nodig zijn.
        need: u64,
        /// Hoeveel er zijn.
        max: u64,
    },
    /// De zittende switch-code is niet de onze: adopteren is geweigerd.
    SwitchCodeMismatch {
        /// De som in de zittende descriptor.
        resident: u64,
        /// De som over onze blobs.
        ours: u64,
    },
    /// Een app-core staat niet in de parkeerlus waar dat beloofd was.
    UnparkedCore {
        /// De core.
        core: usize,
        /// Wat zijn mailbox zegt.
        mbox: u64,
    },
    /// Een dispatch naar een core die al draait.
    CoreRunning {
        /// De core.
        core: usize,
        /// Wat zijn mailbox zegt.
        mbox: u64,
    },
    /// Een adres dat geen ctx-blok van dit plan is.
    BadContext {
        /// Het adres.
        pa: u64,
    },
    /// Een context-id zonder ctx-blok (0, of een secundaire buiten het plan).
    BadContextId {
        /// De id.
        id: u8,
    },
    /// Een mailbox-argument dat de parkeerlus als koud of geparkeerd leest.
    BadArg {
        /// Het argument.
        arg: u64,
    },
    /// Het plan weigerde een index.
    Plan(abi::Error),
    /// Deze EL2-smaak kan de OS-core niet delen (Apple: geen GIC-kick, en
    /// het FIQ-pad van de OS-core is niet geport).
    OsCoreFlavor,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Misaligned { ipa, pa, size } => {
                write!(
                    f,
                    "cage window ipa={ipa:#x} pa={pa:#x} size={size:#x} is not 2 MB aligned"
                )
            }
            Self::IpaWindow { ipa, size } => {
                write!(
                    f,
                    "cage window ipa={ipa:#x} size={size:#x} falls outside the 39-bit IPA regime"
                )
            }
            Self::PaSpace { pa, size } => {
                write!(
                    f,
                    "cage window pa={pa:#x} size={size:#x} does not fit the physical address space"
                )
            }
            Self::Grant { pa, size } => {
                write!(
                    f,
                    "grant window pa={pa:#x} size={size:#x} is empty, unaligned or too large"
                )
            }
            Self::GrantCollides { gb, entry } => {
                write!(
                    f,
                    "grant window collides with L1 entry {gb} (descriptor {entry:#x})"
                )
            }
            Self::NoSwitchCode => write!(f, "this build carries no EL2 switch code"),
            Self::Blob { index, len } => {
                write!(f, "EL2 blob {index} has impossible size {len:#x}")
            }
            Self::SwitchCodeFull { need, max } => {
                write!(f, "switch code needs {need:#x} bytes, block 0 has {max:#x}")
            }
            Self::SwitchCodeMismatch { resident, ours } => {
                write!(
                    f,
                    "resident switch code ({resident:#x}) is not ours ({ours:#x}) HOPOS_FLIP_SWITCHCODE_MISMATCH"
                )
            }
            Self::UnparkedCore { core, mbox } => {
                write!(f, "core {core} is not parked (mailbox {mbox:#x})")
            }
            Self::CoreRunning { core, mbox } => {
                write!(
                    f,
                    "core {core} is running (mailbox {mbox:#x}); dispatch refused"
                )
            }
            Self::BadContext { pa } => write!(f, "{pa:#x} is no context block of this plan"),
            Self::BadContextId { id } => write!(f, "context id {id} has no context block"),
            Self::BadArg { arg } => {
                write!(f, "mailbox argument {arg:#x} reads as cold or parked")
            }
            Self::Plan(e) => write!(f, "plan: {e}"),
            Self::OsCoreFlavor => {
                write!(f, "this EL2 flavor cannot share the OS core with residents")
            }
        }
    }
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    use core::arch::asm;

    /// De intrekking van een kooi in de TLB's. In de Go-generatie was dit
    /// HVC #0 vanuit EL1 naar de revoke-vector op EL2 (vandaar de naam); de
    /// kern van v3 staat zelf op EL2 en doet het ter plekke. De SEV aan het
    /// eind is de les van 19-07: een WFE-slaper doet geen toegangen en
    /// overleefde de intrekking, tot hij gewekt werd.
    pub(super) fn hvc_revoke() {
        // SAFETY: TLB-invalidatie en barrières hebben geen geheugeneffect
        // buiten de ordening; de tabel zelf is al gewist en geveegd.
        unsafe {
            asm!(
                "tlbi alle1is",
                "dsb ish",
                "isb",
                "sev",
                options(nostack, preserves_flags)
            );
        }
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-kant: er is geen EL2 en geen TLB. De tests bewijzen de inhoud
    //! van de tabellen, niet de intrekking zelf (zoals stage2_host.go).
    pub(super) fn hvc_revoke() {}
}
