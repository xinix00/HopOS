//! De node-cores: meer cores voor de kern zelf, en de park-mailboxen
//! waarmee de kern een core start of herstart.
//!
//! In Go gaf `ConfigureNode` de HOP-runtime meer cores via `goos.Task` en
//! `GOMAXPROCS`: de Go-scheduler vroeg lui een M, en de kern bracht dan de
//! volgende core op via PSCI CPU_ON naar de gedeelde EL2-trampoline. In
//! Rust is er geen scheduler die om threads vraagt: elke HOP-core draait een
//! eigen executor met eigen taken (handboek §4, PORT §5 `GOMAXPROCS`), en
//! [`configure_node`] brengt ze bij boot in één keer op. Tussen cores gaan
//! berichten door een ring met een kick; een `Local` van core 0 raakt een
//! andere core nooit aan.
//!
//! De opgang van één core:
//!
//! 1. Core 0 leest zijn eigen EL2-regime van de levende registers (MAIR,
//!    TCR, TTBR0, HCR, VBAR, SCTLR) en legt het met de stack-top en de
//!    Rust-main in een [`Handoff`]. Van de bron gelezen, niet afgeleid: een
//!    core met een andere view van de map kan de hoge periferie niet
//!    vertalen (Altra, 17-07: UART en SBSA-watchdog op 16 TB, fault,
//!    watchdog-reset).
//! 2. De handoff gaat naar DRAM (`dev::push`), niet alleen naar onze cache:
//!    de nieuwe core leest hem met de MMU UIT, dus langs elke cache heen. Op
//!    QEMU (geen cachemodel) viel dat nooit op; op de M4 las de tweede core
//!    de vorige inhoud: rommel als sp en ttbr0 (02-09).
//! 3. PSCI CPU_ON naar `hopos_smp_entry` met de handoff in x0. De entry zet
//!    het regime, de MMU aan en de stack, en springt naar Rust; Rust telt de
//!    core ([`node_started`]) en roept de main van het board, die de
//!    executor van die core draait.
//!
//! De park-mailboxen ([`dispatch`], [`park_state`]) zijn het
//! ARM-mechanisme voor de levenscyclus van een app-core: HopOS bezit zijn
//! cores, dus een gestopte core gaat niet terug naar de firmware (PSCI
//! CPU_OFF is op de Pi 5-stockfirmware een eenrichtingsdeur, gemeten 10-07)
//! maar parkeert op EL2 in een WFE-lus op zijn mailbox. PSCI CPU_ON is
//! alleen de éérste opgang per core; daarna is dispatch {ctx, doel-PC} in
//! de mailbox plus een SEV.

extern crate alloc;

use crate::psci;
use abi::layout::{PARK_COLD, PARK_DISPATCHED, PARK_PARKED, SCHED_MBOX_CTX, SCHED_MBOX_PC};
use alloc::vec::Vec;
use core::fmt;
use core::sync::atomic::{
    AtomicBool, AtomicUsize,
    Ordering::{AcqRel, Acquire, Release},
};
use dev::Pa;

/// Hoeveel cores de kern hoogstens krijgt, core 0 meegeteld.
pub const MAX_NODE_CORES: usize = 8;

/// De stack van een node-core: 64 KB, gelijk aan de boot-stack van core 0.
pub const NODE_STACK: usize = 64 << 10;

/// De Rust-main van een node-core: draait de executor van die core en keert
/// nooit terug. Het board levert hem; hij krijgt de core-index (1 tot
/// `cores - 1`).
pub type CoreMain = fn(core: usize) -> !;

/// De vertaling core-index naar MPIDR-target; die is per board (de
/// nummering verschilt per cluster, zie [`crate::psci`]). `None` = deze
/// core bestaat niet.
pub type Target = fn(core: usize) -> Option<u64>;

/// Hoeveel node-cores hun Rust-entry bereikten.
static STARTED: AtomicUsize = AtomicUsize::new(0);
/// Hoeveel node-cores PSCI CPU_ON kregen.
static DISPATCHED: AtomicUsize = AtomicUsize::new(0);
/// [`configure_node`] is geweest: één keer per boot.
static CONFIGURED: AtomicBool = AtomicBool::new(false);

/// Wat een node-core bij zijn entry leest, met de MMU uit.
///
/// De offsets staan als constanten in de entry-assembly; de asserties
/// eronder houden ze gelijk. Een cacheline groot en gealigneerd, zodat
/// één `dev::push` hem helemaal naar DRAM brengt.
#[repr(C, align(64))]
pub struct Handoff {
    /// De stack-top, 16-gealigneerd.
    pub sp: u64,
    /// De main van deze core.
    pub main: CoreMain,
    /// De core-index.
    pub core: u64,
    /// MAIR_EL2 van core 0.
    pub mair: u64,
    /// TCR_EL2 van core 0.
    pub tcr: u64,
    /// TTBR0_EL2 van core 0: dezelfde map, dus dezelfde heap.
    pub ttbr0: u64,
    /// HCR_EL2 van core 0.
    pub hcr: u64,
    /// VBAR_EL2 van core 0: dezelfde vectoren.
    pub vbar: u64,
    /// SCTLR_EL2 van core 0 (MMU en caches aan).
    pub sctlr: u64,
}

const OFF_SP: usize = 0;
const OFF_MAIR: usize = 24;
const OFF_TCR: usize = 32;
const OFF_TTBR0: usize = 40;
const OFF_HCR: usize = 48;
const OFF_VBAR: usize = 56;
const OFF_SCTLR: usize = 64;
const _: () = assert!(core::mem::offset_of!(Handoff, sp) == OFF_SP);
const _: () = assert!(core::mem::offset_of!(Handoff, main) == 8);
const _: () = assert!(core::mem::offset_of!(Handoff, core) == 16);
const _: () = assert!(core::mem::offset_of!(Handoff, mair) == OFF_MAIR);
const _: () = assert!(core::mem::offset_of!(Handoff, tcr) == OFF_TCR);
const _: () = assert!(core::mem::offset_of!(Handoff, ttbr0) == OFF_TTBR0);
const _: () = assert!(core::mem::offset_of!(Handoff, hcr) == OFF_HCR);
const _: () = assert!(core::mem::offset_of!(Handoff, vbar) == OFF_VBAR);
const _: () = assert!(core::mem::offset_of!(Handoff, sctlr) == OFF_SCTLR);

/// Het EL2-regime van de dispatchende core, in de volgorde van [`Handoff`].
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Regime {
    /// MAIR_EL2.
    pub mair: u64,
    /// TCR_EL2.
    pub tcr: u64,
    /// TTBR0_EL2.
    pub ttbr0: u64,
    /// HCR_EL2.
    pub hcr: u64,
    /// VBAR_EL2.
    pub vbar: u64,
    /// SCTLR_EL2.
    pub sctlr: u64,
}

impl Handoff {
    /// De handoff voor `core` met stack-top `sp` en het regime van core 0.
    #[must_use]
    pub const fn new(core: usize, sp: u64, main: CoreMain, r: Regime) -> Self {
        Self {
            sp,
            main,
            core: core as u64,
            mair: r.mair,
            tcr: r.tcr,
            ttbr0: r.ttbr0,
            hcr: r.hcr,
            vbar: r.vbar,
            sctlr: r.sctlr,
        }
    }
}

/// Waarom een core niet opkwam of niet gedispatcht werd.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Error {
    /// Meer cores gevraagd dan [`MAX_NODE_CORES`].
    TooMany {
        /// Gevraagd.
        cores: usize,
        /// Het maximum.
        max: usize,
    },
    /// [`configure_node`] is al geroepen.
    AlreadyConfigured,
    /// Het board kent deze core niet.
    NoSuchCore {
        /// De core-index.
        core: usize,
    },
    /// Geen heap voor de stack of de handoff.
    OutOfMemory {
        /// De core-index.
        core: usize,
    },
    /// De firmware weigerde CPU_ON.
    Psci {
        /// Het MPIDR-target.
        target: u64,
        /// De reden.
        err: psci::Error,
    },
    /// De ctx van een dispatch is geen adres (0, 1 en 2 zijn toestanden).
    BadCtx {
        /// De ctx.
        ctx: u64,
    },
    /// De core draait nog: de mailbox staat niet op cold of parked.
    Busy {
        /// Woord 0 van de mailbox.
        word: u64,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::TooMany { cores, max } => {
                write!(f, "smp: {cores} node cores asked, at most {max}")
            }
            Self::AlreadyConfigured => f.write_str("smp: node cores already configured"),
            Self::NoSuchCore { core } => write!(f, "smp: core {core}: no such core on this board"),
            Self::OutOfMemory { core } => {
                write!(f, "smp: core {core}: no heap for its stack or handoff")
            }
            Self::Psci { target, err } => write!(f, "smp: CPU_ON {target:#x}: {err}"),
            Self::BadCtx { ctx } => {
                write!(f, "smp: dispatch ctx {ctx:#x} is a state, not an address")
            }
            Self::Busy { word } => write!(f, "smp: core not parked (mailbox word0 {word:#x})"),
        }
    }
}

/// Het resultaat van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Geeft de kern `cores` cores (core 0 telt mee): cores 1 tot `cores - 1`
/// komen op via PSCI CPU_ON en draaien elk `main`. Geeft het aantal
/// gedispatchte cores.
///
/// No-op bij `cores <= 1`: dan blijft de node single-core, zoals altijd.
/// Aanroepen op core 0, ná de vectoren en de heap en vóór de eerste
/// `spawn`. Eén keer per boot.
pub fn configure_node(cores: usize, target: Target, main: CoreMain) -> Result<usize> {
    if cores <= 1 {
        return Ok(0);
    }
    if cores > MAX_NODE_CORES {
        return Err(Error::TooMany {
            cores,
            max: MAX_NODE_CORES,
        });
    }
    if CONFIGURED.swap(true, AcqRel) {
        return Err(Error::AlreadyConfigured);
    }
    let regime = arch::regime();
    let entry = arch::entry_pa();
    for core in 1..cores {
        let mpidr = target(core).ok_or(Error::NoSuchCore { core })?;
        let sp = new_stack().ok_or(Error::OutOfMemory { core })?;
        let h =
            new_handoff(Handoff::new(core, sp, main, regime)).ok_or(Error::OutOfMemory { core })?;
        let pa = Pa(core::ptr::from_ref(h) as usize as u64);
        dev::push(pa, core::mem::size_of::<Handoff>());
        psci::cpu_on(mpidr, entry, pa.0).map_err(|err| Error::Psci { target: mpidr, err })?;
        DISPATCHED.fetch_add(1, Release);
    }
    Ok(DISPATCHED.load(Acquire))
}

/// Start precies één core op het EL2-regime van deze core, met een verse
/// stack en `main`: de verhuizing van de kern naar de OS-core bij boot
/// (PORT.md beslissing 2, `hopos.oscore`), dezelfde opgang als
/// [`configure_node`] maar los van de node-telling. De aanroeper geeft
/// daarna zijn eigen core op (`cpu::el2::hold`).
pub fn start_one(core: usize, target: u64, main: CoreMain) -> Result {
    let regime = arch::regime();
    let entry = arch::entry_pa();
    let sp = new_stack().ok_or(Error::OutOfMemory { core })?;
    let h = new_handoff(Handoff::new(core, sp, main, regime)).ok_or(Error::OutOfMemory { core })?;
    let pa = Pa(core::ptr::from_ref(h) as usize as u64);
    dev::push(pa, core::mem::size_of::<Handoff>());
    psci::cpu_on(target, entry, pa.0).map_err(|err| Error::Psci { target, err })
}

/// Hoeveel node-cores (naast core 0) hun Rust-entry bereikten: het bewijs
/// dat de extra cores écht draaien, niet alleen gevraagd zijn.
#[must_use]
pub fn node_started() -> usize {
    STARTED.load(Acquire)
}

/// Hoeveel node-cores PSCI CPU_ON kregen.
#[must_use]
pub fn node_dispatched() -> usize {
    DISPATCHED.load(Acquire)
}

/// De core-index uit een MPIDR-woord: aff0 (de core in zijn cluster) plus
/// twee bits van aff1 (het cluster), zodat cores over clusters heen elk
/// een eigen index houden (idle_arm64.go `coreIndex`). Altijd onder 64.
#[must_use]
pub const fn core_index_of(mpidr: u64) -> usize {
    ((mpidr & 0xF) | (((mpidr >> 8) & 0x3) << 4)) as usize
}

/// De index van de core waar dit draait.
#[must_use]
pub fn core_index() -> usize {
    core_index_of(arch::mpidr())
}

/// Een stack van [`NODE_STACK`] bytes van de heap, voor altijd; de top,
/// 16-gealigneerd.
///
/// `try_reserve_exact` eerst: dan alloceert `resize` niet meer, en een
/// volle heap is `None` in plaats van een abort (handboek §6).
fn new_stack() -> Option<u64> {
    let mut v: Vec<u8> = Vec::new();
    v.try_reserve_exact(NODE_STACK).ok()?;
    v.resize(NODE_STACK, 0);
    let s: &'static mut [u8] = v.leak();
    let top = s.as_ptr() as usize as u64 + s.len() as u64;
    Some(top & !15)
}

/// De handoff op de heap, voor altijd: de core leest hem na zijn entry
/// niet meer, maar niemand anders mag de plek krijgen terwijl hij nog
/// onderweg is.
fn new_handoff(h: Handoff) -> Option<&'static Handoff> {
    let mut v: Vec<Handoff> = Vec::new();
    v.try_reserve_exact(1).ok()?;
    v.push(h);
    let s: &'static [Handoff] = v.leak();
    s.first()
}

// ---------------------------------------------------------------------------
// De park-mailboxen.
// ---------------------------------------------------------------------------

/// De toestand van een park-mailbox (woord 0 van het sched-blok).
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Park {
    /// Nooit geparkeerd: de eerste opgang gaat via PSCI CPU_ON.
    Cold,
    /// Geparkeerd in de EL2-WFE-lus, wachtend op dispatch.
    Parked,
    /// De trampoline bevestigde de dispatch.
    Dispatched,
    /// Het startschot staat erin: de ctx die de kern zette.
    Running(u64),
}

impl Park {
    /// De toestand bij woord 0.
    #[must_use]
    pub const fn from_word(w: u64) -> Park {
        match w {
            PARK_COLD => Self::Cold,
            PARK_PARKED => Self::Parked,
            PARK_DISPATCHED => Self::Dispatched,
            ctx => Self::Running(ctx),
        }
    }
}

/// Hoe een dispatch de core bereikte.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Start {
    /// Eerste opgang: PSCI CPU_ON.
    Cold,
    /// Een geparkeerde core, gewekt met een SEV.
    Kicked,
}

/// De toestand van de core achter mailbox `mbox`.
#[must_use]
pub fn park_state(mbox: Pa) -> Park {
    Park::from_word(dev::read64(mbox.add(SCHED_MBOX_CTX)))
}

/// Geeft het startschot: {ctx, doel-PC} in de mailbox, dan de eenmalige
/// PSCI CPU_ON (cold) of een SEV die de parkeerlus de trampoline in laat
/// springen. Woord 0 = ctx maakt de core meteen "running".
///
/// Weigert een core die niet cold of parked is: twee startschoten op één
/// core is twee bewoners op één stack.
pub fn dispatch(mbox: Pa, target: u64, entry: u64, ctx: u64) -> Result<Start> {
    if ctx <= PARK_DISPATCHED {
        return Err(Error::BadCtx { ctx });
    }
    let state = park_state(mbox);
    let cold = match state {
        Park::Cold => true,
        Park::Parked => false,
        Park::Dispatched | Park::Running(_) => {
            return Err(Error::Busy {
                word: dev::read64(mbox.add(SCHED_MBOX_CTX)),
            });
        }
    };
    // Eerst de PC, dan de ctx: de lus kijkt naar woord 0, en mag de PC dan
    // al zien. `push` brengt beide naar DRAM (de parkeerlus draait met de
    // MMU uit) en sluit af met een DSB, zodat ze er staan vóór de SEV of de
    // SMC.
    dev::write64(mbox.add(SCHED_MBOX_PC), entry);
    dev::write64(mbox.add(SCHED_MBOX_CTX), ctx);
    dev::push(mbox, 16);
    if cold {
        psci::cpu_on(target, entry, ctx).map_err(|err| Error::Psci { target, err })?;
        return Ok(Start::Cold);
    }
    dev::notify();
    Ok(Start::Kicked)
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    //! De registers van het regime, MPIDR, en de entry van een node-core.
    use super::{
        Handoff, OFF_HCR, OFF_MAIR, OFF_SCTLR, OFF_SP, OFF_TCR, OFF_TTBR0, OFF_VBAR, Regime,
    };
    use core::arch::asm;
    use core::sync::atomic::Ordering::Release;

    macro_rules! mrs {
        ($reg:literal) => {{
            let v: u64;
            // SAFETY: een systeemregister van het eigen EL2-regime lezen
            // heeft geen neveneffect.
            unsafe { asm!(concat!("mrs {}, ", $reg), out(reg) v, options(nomem, nostack)) };
            v
        }};
    }

    pub(super) fn regime() -> Regime {
        Regime {
            mair: mrs!("mair_el2"),
            tcr: mrs!("tcr_el2"),
            ttbr0: mrs!("ttbr0_el2"),
            hcr: mrs!("hcr_el2"),
            vbar: mrs!("vbar_el2"),
            sctlr: mrs!("sctlr_el2"),
        }
    }

    pub(super) fn mpidr() -> u64 {
        mrs!("mpidr_el1")
    }

    unsafe extern "C" {
        /// De entry hieronder; alleen zijn adres wordt gebruikt.
        fn hopos_smp_entry();
    }

    /// Het fysieke adres van de entry (identity-map: gelijk aan het
    /// virtuele).
    pub(super) fn entry_pa() -> u64 {
        hopos_smp_entry as *const () as usize as u64
    }

    /// De Rust-kant van de entry: tellen, dan de main van het board.
    extern "C" fn node_entry(h: &'static Handoff) -> ! {
        super::STARTED.fetch_add(1, Release);
        (h.main)(h.core as usize)
    }

    // De entry van een node-core, waar PSCI CPU_ON hem neerzet: EL2, MMU
    // uit, x0 = de handoff. Alles wat Rust nog niet kan en niets meer:
    //
    // - Niet op EL2 (een firmware die ons ergens anders aflevert): parkeren.
    //   Stil, want er is nog geen stack om te melden; core 0 ziet het aan
    //   `node_started`.
    // - Het regime van core 0 zetten. MMU uit betekent dat de loads
    //   Device-toegang zijn: gealigneerd, en `dev::push` bracht ze naar DRAM.
    //   HCR_EL2 eerst, met een ISB: een kern onder E2H = 1 (VHE, de O6N)
    //   geeft TCR_EL2 en SCTLR_EL2 in de vorm van TCR_EL1 en SCTLR_EL1, en
    //   E2H bepaalt hoe de core die velden leest (Linux' `init_el2` zet
    //   HCR_EL2 ook als eerste). Onder nVHE maakt de volgorde niets uit:
    //   MMU uit en DAIF dicht (29-09).
    // - Eerst de TLB en de I-cache van deze core schoon, dan SCTLR (MMU en
    //   caches aan), ISB.
    // - De stack, en `bl` naar Rust met x0 ongewijzigd: de handoff is het
    //   eerste argument van `node_entry`.
    //
    // Veiligheid (als gewone regel: clippy weigert een SAFETY-tag op
    // `global_asm!`, er is geen `unsafe`-blok om te dragen): de entry raakt alleen de registers van zijn eigen core en leest
    // de handoff die core 0 voor hem klaarzette en naar DRAM veegde; de
    // stack is een verse heap-allocatie van precies deze core. `node_entry`
    // keert nooit terug; de parkeerlus erachter is de vangrail.
    core::arch::global_asm!(
        r#"
    .section .text.hopos_smp, "ax"
    .global hopos_smp_entry
    .balign 8
hopos_smp_entry:
    mrs x1, CurrentEL
    lsr x1, x1, #2
    and x1, x1, #3
    cmp x1, #2
    b.ne 9f

    ldr x1, [x0, #{hcr}]
    msr hcr_el2, x1
    isb
    ldr x1, [x0, #{mair}]
    msr mair_el2, x1
    ldr x1, [x0, #{tcr}]
    msr tcr_el2, x1
    ldr x1, [x0, #{ttbr0}]
    msr ttbr0_el2, x1
    ldr x1, [x0, #{vbar}]
    msr vbar_el2, x1
    dsb ish
    isb
    tlbi alle2
    ic iallu
    dsb ish
    isb
    ldr x1, [x0, #{sctlr}]
    msr sctlr_el2, x1
    isb

    ldr x1, [x0, #{sp}]
    mov sp, x1
    bl {rust}
9:
    wfe
    b 9b
"#,
        mair = const OFF_MAIR,
        tcr = const OFF_TCR,
        ttbr0 = const OFF_TTBR0,
        hcr = const OFF_HCR,
        vbar = const OFF_VBAR,
        sctlr = const OFF_SCTLR,
        sp = const OFF_SP,
        rust = sym node_entry,
    );
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-stub: geen registers, geen entry. De opgang loopt tot PSCI, en
    //! die zegt op de host NOT_SUPPORTED.
    use super::Regime;

    pub(super) fn regime() -> Regime {
        Regime::default()
    }
    pub(super) fn mpidr() -> u64 {
        0
    }
    pub(super) fn entry_pa() -> u64 {
        0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn main_stub(_core: usize) -> ! {
        loop {
            core::hint::spin_loop();
        }
    }

    fn two_cores(core: usize) -> Option<u64> {
        (core < 2).then_some(core as u64)
    }

    #[test]
    fn core_index_follows_idle_arm64() {
        assert_eq!(core_index_of(0x8000_0000), 0);
        assert_eq!(core_index_of(0x0000_0003), 3);
        // Cluster 1, core 2: index 18.
        assert_eq!(core_index_of(0x0000_0102), 18);
        // Aff1 boven 3 en aff0 boven 15 vallen weg: altijd onder 64.
        assert!(core_index_of(u64::MAX) < 64);
        assert_eq!(core_index(), 0);
    }

    #[test]
    fn configure_node_on_the_host() {
        // Eén core is geen werk; te veel is een weigering.
        assert_eq!(configure_node(1, two_cores, main_stub), Ok(0));
        assert_eq!(
            configure_node(9, two_cores, main_stub),
            Err(Error::TooMany { cores: 9, max: 8 })
        );
        // De host heeft geen firmware: CPU_ON zegt NOT_SUPPORTED, en er is
        // niets gestart.
        assert_eq!(
            configure_node(2, two_cores, main_stub),
            Err(Error::Psci {
                target: 1,
                err: psci::Error::NotSupported
            })
        );
        assert_eq!(node_dispatched(), 0);
        assert_eq!(node_started(), 0);
        // En één keer per boot.
        assert_eq!(
            configure_node(2, two_cores, main_stub),
            Err(Error::AlreadyConfigured)
        );
    }

    #[test]
    fn handoff_carries_the_regime() {
        let r = Regime {
            mair: 1,
            tcr: 2,
            ttbr0: 3,
            hcr: 4,
            vbar: 5,
            sctlr: 6,
        };
        let h = Handoff::new(3, 0x1000, main_stub, r);
        assert_eq!(
            [h.sp, h.core, h.mair, h.tcr, h.ttbr0, h.hcr, h.vbar, h.sctlr],
            [0x1000, 3, 1, 2, 3, 4, 5, 6]
        );
        assert_eq!(core::mem::size_of::<Handoff>(), 128);
        let top = new_stack().unwrap();
        assert_eq!(top % 16, 0);
    }

    fn mailbox() -> (Vec<u64>, Pa) {
        let mut v = vec![0u64; 32];
        let pa = Pa(v.as_mut_ptr() as usize as u64);
        (v, pa)
    }

    #[test]
    fn park_words_decode() {
        assert_eq!(Park::from_word(0), Park::Cold);
        assert_eq!(Park::from_word(1), Park::Parked);
        assert_eq!(Park::from_word(2), Park::Dispatched);
        assert_eq!(Park::from_word(0x4000_1000), Park::Running(0x4000_1000));
    }

    #[test]
    fn dispatch_kicks_a_parked_core() {
        let (_buf, mb) = mailbox();
        dev::write64(mb, PARK_PARKED);
        assert_eq!(dispatch(mb, 5, 0x8000, 0x4000_1000), Ok(Start::Kicked));
        assert_eq!(dev::read64(mb.add(SCHED_MBOX_PC)), 0x8000);
        assert_eq!(park_state(mb), Park::Running(0x4000_1000));
        // Nu draait hij: een tweede startschot is een weigering.
        assert_eq!(
            dispatch(mb, 5, 0x8000, 0x4000_2000),
            Err(Error::Busy { word: 0x4000_1000 })
        );
        dev::write64(mb, PARK_DISPATCHED);
        assert!(matches!(
            dispatch(mb, 5, 0x8000, 0x4000_2000),
            Err(Error::Busy { .. })
        ));
    }

    #[test]
    fn dispatch_of_a_cold_core_goes_through_psci() {
        let (_buf, mb) = mailbox();
        // Op de host weigert de "firmware"; de mailbox draagt het
        // startschot al, precies zoals in Go (cageDispatch schrijft eerst).
        assert_eq!(
            dispatch(mb, 7, 0x8000, 0x4000_1000),
            Err(Error::Psci {
                target: 7,
                err: psci::Error::NotSupported
            })
        );
        assert_eq!(dispatch(mb, 7, 0x8000, 2), Err(Error::BadCtx { ctx: 2 }));
    }
}
