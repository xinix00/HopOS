//! De OS-core als deelbare core (PORT.md beslissing 2, 30-09): de kern is
//! er de eerste bewoner van, en zijn idle is de rotatie over de andere
//! bewoners (Hop, en de groepen die de kern het toestaat).
//!
//! Dit bezit: de overgang van de kern (EL2) naar een bewoner (EL1 met
//! stage-2) en terug, de rotatie over de bewonerslijst van logische core 0
//! (sched-blok 0 van het plan), de EL2-timer (CNTHP) die de deadline van de
//! executor bewaakt terwijl een bewoner draait, en de meetlat van de
//! overgangen. Niet van hier: wie er bewoner wordt (de plaatsing van de
//! kern) en wat de kern doet als hij terug is (de executor).
//!
//! Waarom de kern hier de rotor is en niet een lid van de switcher van de
//! app-cores: de kern draait zelf op EL2, met zijn eigen vectoren, stack en
//! MMU. De switcher in de plan-regio kent alleen EL1-bewoners en slaapt op
//! EL2 in WFE; een kern daarin hervatten zou zijn hele EL2-regime door die
//! blob laten dragen. Hier is het omgekeerd en klein: de kern roept
//! [`OsCore::run`] als zijn executor niets te doen heeft, de bewoner draait
//! tot hij yieldt, exit doet of faultt, of tot een interrupt (de NIC, de
//! kick van een app-core, de CNTHP op de deadline) naar EL2 trapt; dan keert
//! `run` terug en draait de executor zijn ronde. De ctx-blokken zijn die van
//! de switcher, byte voor byte (zelfde bewaarvolgorde, zelfde staten), zodat
//! alles wat de kern van een bewoner leest (`ctx_state`, `wake_due`, het
//! fault-rapport) hier ook klopt.
//!
//! De verloren-wek-race blijft dicht zoals in `cpu::idle`: de executor
//! toetst `ready()` met I en F gemaskeerd vóór [`OsCore::run`], en een
//! interrupt die daarna binnenkomt staat pending bij de GIC. Met HCR_EL2.IMO
//! op 1 is een fysieke IRQ voor EL1 nooit gemaskeerd, dus hij trapt direct
//! na de ERET weer naar EL2.
//!
//! Wat de kern tijdens de beurt van een bewoner NIET hoort, is een SEV: die
//! wekt een WFE, geen draaiende core. Daarom zet de kern tijdens zo'n beurt
//! (en in zijn WFI-slaap, die een SEV evenmin wekt) in sched-blok 0 de
//! ICC_SGI1R-waarde van zijn kick ([`SCHED_OS_KICK`]); de switcher van een
//! app-core die yieldt of HVC #6 doet, stuurt dan die SGI.
//!
//! Op Apple (`AppleVhe`) is er geen GIC: de kick is de fast IPI
//! (IPI_RR_GLOBAL_EL1, [`Bell::apple`]) en komt aan als FIQ, net als de
//! CNTHP. Met HCR_EL2.FMO op 1 trapt die FIQ tijdens een beurt naar de
//! vectoren hieronder (`fiq/lower-a64`, index 10), en de kern ackt de IPI
//! zelf op EL2 voor hij terugkeert (les 04-09: een ongeackte fast IPI blijft
//! staan en de core komt nooit meer tot slapen).

extern crate alloc;

use super::Error;
use super::dispatch::{
    Flavor, HVC_EXIT, HVC_YIELD, VEC_SYNC_LOWER, context_id, ctx_read, ctx_state, ctx_write, rx_due,
};
use super::layout::{
    CAGE_STRIDE, CTX_CTRL_PA, CTX_GPRS, CTX_KICK_PENDING, CTX_OFF, CTX_REGIME,
    CTX_REGIME_ARM_WORDS, CTX_RESUME, CTX_SP, CTX_STATE, CTX_UNIT_SLOT, CTX_WAKE, CTX_WAKE_NO_PEEK,
    Core, CtxState, Plan, SCHED_CLINT_PA, SCHED_COUNT, SCHED_CURRENT, SCHED_CURSOR, SCHED_LIST,
    SCHED_MSIP_PA, SLOT_CAP,
};
use abi::hopabi::{CTRL_FAULT_ESR, CTRL_FAULT_FAR, CTRL_FAULT_VEC};
use core::sync::atomic::{
    AtomicU64,
    Ordering::{Acquire, Relaxed, Release},
};
use dev::Pa;

/// Het woord in sched-blok 0 (de OS-core) met de ICC_SGI1R_EL1-waarde die
/// de kern wekt; 0 = hij hoort een SEV, niet kicken.
///
/// Op ARM ongebruikt veld van het sched-blok: `SCHED_MSIP_PA` is de
/// RISC-V-kick (de PA van `msip`), en in blok 0 is dat hier dezelfde rol,
/// "het wek-IPI van de kern". Regel 3 van het blok: de kern schrijft, de
/// switcher leest.
pub const SCHED_OS_KICK: u64 = SCHED_MSIP_PA;

/// Het woord in sched-blok 0 met de PA van GICD_SGIR als de kick een
/// MMIO-schrijf is (GICv2, de GIC-400 van de Pi's); 0 = de kick is een
/// ICC_SGI1R-systeemregister (GICv3). Op ARM ongebruikt veld: op RISC-V is
/// het de PA van `mtimecmp`. Eén keer gezet door [`OsCore::new`].
pub const SCHED_OS_KICK_PA: u64 = SCHED_CLINT_PA;

/// SCTLR_EL1 bij een koude start: de RES1-bits, M/C/I/A/WXN uit. Nooit
/// erven (de Pi 5-les van 10-07). Eén bron voor de drop van de switcher en
/// de eerste beurt van een bewoner hier.
pub(super) const SCTLR_EL1_CLEAN: u64 = 0x30d0_0800;
/// SPSR voor de eerste ERET: EL1h, DAIF gemaskeerd.
pub(super) const SPSR_EL1H_MASKED: u64 = 0x3c5;

/// HCR_EL2-bits van een bewoner op de OS-core bovenop die van de kern: VM
/// (stage-2 aan), TSC (een SMC is een ontsnappingspoging), en IMO/FMO/AMO
/// (elke fysieke interrupt landt op EL2, bij de kern; de kern zelf draait
/// al zo, `cpu::boot`). TGE moet uit, anders draait er geen EL1.
const HCR_VM: u64 = 1 << 0;
const HCR_FMO: u64 = 1 << 3;
const HCR_IMO: u64 = 1 << 4;
const HCR_AMO: u64 = 1 << 5;
const HCR_TSC: u64 = 1 << 19;
const HCR_TGE: u64 = 1 << 27;
const HCR_GUEST: u64 = HCR_VM | HCR_FMO | HCR_IMO | HCR_AMO | HCR_TSC;

/// De vectorindexen die niet [`VEC_SYNC_LOWER`] zijn en toch geen fault:
/// IRQ en FIQ uit een lagere EL.
const VEC_IRQ_LOWER: u64 = 9;
const VEC_FIQ_LOWER: u64 = 10;

/// Bit 63 van het kick-woord op Apple: "scherp". Het doel van de fast IPI
/// (core | cluster << 16) is voor E-core 0 in cluster 0 gewoon 0, en 0 is
/// in [`SCHED_OS_KICK`] "niet kicken" (dezelfde val als de brievenbus van
/// Go 31-08, waar E-core 0 nooit werk kon aannemen). De switcher wist het
/// bit voor hij IPI_RR_GLOBAL_EL1 schrijft (bits 29:28 zijn het type, 0 =
/// meteen; bit 63 is er niet).
pub(super) const APPLE_KICK_ARMED: u64 = 1 << 63;

/// Het kick-woord in [`SCHED_OS_KICK`] voor de fast IPI naar de core met
/// affiniteit `mpidr`: het IPI_RR-doel plus [`APPLE_KICK_ARMED`].
#[must_use]
pub(super) const fn apple_kick_word(mpidr: u64) -> u64 {
    APPLE_KICK_ARMED | super::dispatch::apple_ipi_target(mpidr)
}

/// Wat de switcher van een kick-woord in IPI_RR_GLOBAL_EL1 schrijft: zijn
/// masker is `apple_kick_target(u64::MAX)`, als `const` in de assembly
/// (switch.rs), zodat de test hier en de switcher één regel delen.
#[must_use]
#[cfg_attr(
    not(all(target_os = "none", target_arch = "aarch64")),
    allow(dead_code) // op de host is er geen switcher, alleen de test
)]
pub(super) const fn apple_kick_target(word: u64) -> u64 {
    word & !APPLE_KICK_ARMED
}

/// Waar een beurt eindigde, naar de vectorindex waarmee de bewoner
/// terugkwam.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
enum Exit {
    /// Synchroon (index 8): een HVC, of een fault die de ESR beschrijft.
    Sync,
    /// Een IRQ of FIQ uit EL1 (index 9, 10): de kern wil zijn core terug.
    Interrupt,
    /// SError of AArch32 (11..15): altijd een fault.
    Fault,
}

/// De afbeelding van vectorindex naar [`Exit`]. Op elke smaak dezelfde:
/// de GIC levert alles als IRQ, Apple de timer en de kick als FIQ en de AIC
/// als IRQ.
const fn exit_of(vec: u64) -> Exit {
    match vec {
        VEC_SYNC_LOWER => Exit::Sync,
        VEC_IRQ_LOWER | VEC_FIQ_LOWER => Exit::Interrupt,
        _ => Exit::Fault,
    }
}

/// Waardoor een onderbroken beurt terugkwam: de CNTHP als hij afging
/// (ISTATUS), anders de kick als die wacht, anders een device.
const fn interrupted(fired: bool, kick: bool) -> Back {
    if fired {
        Back::Timer
    } else if kick {
        Back::Ipi
    } else {
        Back::Irq
    }
}

/// De vangrail van de zelftest-spinner bovenop twee keer zijn termijn: 50
/// ms.
///
/// Les van 30-09 (de soak van de hoplb-kring, `tools/qemu-soak-hop.sh`,
/// vier QEMU's naast elkaar, load 20 op 14 host-cores): met alleen twee
/// keer de termijn (2 ms bij de timer-toets van 1 ms) gaf de spinner op
/// 2042 us zelf op, vóór de CNTHP van een QEMU-vCPU die even geen host-tijd
/// kreeg, en de boot werd rood (HOPOS_OS_SELFTEST_FAIL, 1 van 25 runs met
/// OSCORE=1). Laat is geen fout: een timer die nooit komt wel. 50 ms is
/// ruim boven elke gemeten vertraging en houdt een board zonder routering
/// nog steeds uit een hangende boot.
const SELFTEST_GRACE_NS: u64 = 50_000_000;

/// De tellerstand waarop de zelftest-spinner zelf opgeeft: `now` plus twee
/// keer `ticks` plus [`SELFTEST_GRACE_NS`] op een teller van `hz`.
fn spin_limit(now: u64, ticks: u64, hz: u64) -> u64 {
    let grace = crate::idle::ns_to_ticks(SELFTEST_GRACE_NS, hz);
    now.wrapping_add(ticks.saturating_mul(2).saturating_add(grace))
}

/// Het ctx-blok in woorden, voor de scratch van de zelftest.
const CTX_WORDS: usize = (super::layout::CTX_LEN / 8) as usize;

/// De langste beurt zonder deadline: 10 ms, dezelfde vangrail als de
/// WFI-slaap (`cpu::idle::WFI_CAP_NS`). "Geen deadline" als oneindig lezen
/// is geen zuinigheid maar een hang.
pub const TURN_CAP_NS: u64 = 10_000_000;

/// Waardoor de kern zijn core terugkreeg.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Back {
    /// De bewoner yieldde (HVC #1, of een andere HVC die geen exit is).
    Yield,
    /// De bewoner deed exit (HVC #0) en is dood.
    Exit,
    /// Een interrupt van een device (de NIC, de UART, de CNTP van de kern).
    Irq,
    /// De kick van een app-core: de SGI van [`Bell`].
    Ipi,
    /// De CNTHP op de deadline van de executor.
    Timer,
    /// Een fault, een getrapte SMC, of een intrekking: de bewoner is dood en
    /// het rapport staat op zijn control-page.
    Fault,
}

impl Back {
    /// Het getal van deze uitkomst in [`Stats::last`] (1..=6; 0 = nog geen
    /// beurt).
    #[must_use]
    pub const fn code(self) -> u64 {
        match self {
            Back::Yield => 1,
            Back::Exit => 2,
            Back::Irq => 3,
            Back::Ipi => 4,
            Back::Timer => 5,
            Back::Fault => 6,
        }
    }

    /// De naam bij [`Back::code`], voor de tik-regel.
    #[must_use]
    pub const fn name_of(code: u64) -> &'static str {
        match code {
            1 => "yield",
            2 => "exit",
            3 => "irq",
            4 => "ipi",
            5 => "timer",
            6 => "fault",
            _ => "-",
        }
    }
}

/// Wat één proef van [`OsCore::selftest`] zag: waardoor de kern terugkwam,
/// na hoeveel ticks, met welke vectorindex, en wat de controller op dat
/// moment als hoogste pending liet zien, vóór de overgang en erna (de peek
/// van [`Bell::pending`], zonder claim).
///
/// Waarom zo veel: op de eerste Pi 5-boot (30-09) gaf de zelftest drie keer
/// `Irq` na 0 us, en de regel zei niet wélke lijn. Een lijn die al vóór de
/// overgang pending stond (een NIC-interrupt uit de boot die nog niemand
/// claimde) is een andere fout dan een lijn die tijdens de beurt komt. Met
/// de INTID erbij is dezelfde klasse fout op de O6N en de Altra in één
/// regel te lezen.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Probe {
    /// Waardoor de kern terugkwam.
    pub back: Back,
    /// Na hoeveel ticks van de teller.
    pub ticks: u64,
    /// De vectorindex van de terugkeer (8 synchroon, 9 IRQ, 10 FIQ).
    pub vec: u64,
    /// De hoogste pending INTID vlak vóór de overgang (1023 = niets; zonder
    /// bel [`Probe::NONE`]).
    pub before: u32,
    /// De hoogste pending INTID direct na de terugkeer.
    pub after: u32,
}

impl Probe {
    /// "Geen peek": een OS-core zonder bel.
    pub const NONE: u32 = u32::MAX;

    /// Stond er vóór de overgang al een lijn pending die niet de kick
    /// `kick` is, dan die INTID. De GIC zegt 1023 (en hoger) voor niets.
    #[must_use]
    pub const fn stale(&self, kick: u32) -> Option<u32> {
        if self.before >= 1020 || self.before == kick {
            None
        } else {
            Some(self.before)
        }
    }
}

/// Wat één beurt van de rotatie deed.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Turn {
    /// Een bewoner draaide; zo kwam de core terug.
    Ran(Back),
    /// Niemand was aan de beurt. `wake` is de vroegste wektijd (CNTPCT) van
    /// een geyielde bewoner, zodat de kern niet langer slaapt dan nodig.
    Idle {
        /// De vroegste wektijd, of `None` als er niemand wacht.
        wake: Option<u64>,
    },
}

/// De kick van de OS-core: wat de switcher van een app-core schrijft om hem
/// te wekken, en hoe de kern na een IRQ ziet of het de kick was. Het board
/// kiest de weg: GICv3 een systeemregister, GICv2 een MMIO-schrijf, Apple
/// de fast IPI ([`Bell::apple`]).
#[derive(Copy, Clone, Debug)]
pub struct Bell {
    /// De SGI naar de OS-core, als woord: met `sgir == 0` de
    /// ICC_SGI1R_EL1-waarde (GICv3, `driver_gicv3::sgi1r`), anders de
    /// 32-bit GICD_SGIR-waarde (GICv2: CPUTargetList in bits 23:16, de
    /// INTID in 3:0). Op Apple het kick-woord van [`apple_kick_word`]. 0 =
    /// geen kick.
    pub sgi1r: u64,
    /// De PA van GICD_SGIR (distributor + 0xF00) op een GICv2, 0 op een
    /// GICv3. De switcher draait met de MMU uit, dus dit is een fysiek adres.
    pub sgir: u64,
    /// De INTID van de kick.
    pub intid: u32,
    /// De hoogste pending INTID (ICC_HPPIR1_EL1), zonder hem te claimen.
    pub pending: fn() -> u32,
}

impl Bell {
    /// De "INTID" van de fast IPI: Apple heeft geen GIC, dus een waarde die
    /// geen GIC-INTID kan zijn. [`Bell::pending`] geeft hem als IPI_SR_EL1
    /// bit 0 staat.
    pub const APPLE_INTID: u32 = u32::MAX;

    /// De kick van Apple silicium naar de OS-core met affiniteit `mpidr`:
    /// de switcher schrijft het doel in IPI_RR_GLOBAL_EL1 (m1n1 `smp.c`,
    /// GEMETEN 02-09 als wek van de switcher), en de kern leest IPI_SR_EL1
    /// bit 0 zonder te acken; de ack doet hij zelf, op EL2, bij de terugkeer
    /// van de beurt (les 04-09).
    #[must_use]
    pub const fn apple(mpidr: u64) -> Bell {
        Bell {
            sgi1r: apple_kick_word(mpidr),
            sgir: 0,
            intid: Self::APPLE_INTID,
            pending: arch::apple_ipi_pending,
        }
    }
}

/// Ackt een wachtende fast IPI op deze Apple-core (IPI_SR_EL1); `true` = er
/// stond er een. Voor de dispatch van een kern zonder switcher eronder: een
/// IPI die niemand ackt, blijft staan en de core slaapt nooit meer (04-09).
pub fn apple_ipi_ack() -> bool {
    let up = arch::apple_ipi_pending() != 0;
    if up {
        arch::apple_ipi_ack();
    }
    up
}

/// De meetlat van de OS-core: overgangen naar een bewoner en waardoor de
/// kern terugkwam. Zonder deze getallen is "Hop krijgt tijd" niet te
/// onderscheiden van "de kern spint".
#[derive(Debug)]
pub struct Stats {
    /// Overgangen naar een bewoner.
    pub entries: AtomicU64,
    /// Terug op een device-interrupt.
    pub irq: AtomicU64,
    /// Terug op de kick van een app-core.
    pub ipi: AtomicU64,
    /// Terug op de CNTHP (de deadline van de executor).
    pub timer: AtomicU64,
    /// Terug op een yield.
    pub yields: AtomicU64,
    /// Terug op een exit.
    pub exits: AtomicU64,
    /// Terug op een fault.
    pub faults: AtomicU64,
    /// De tijd die bewoners kregen, in ticks.
    pub ticks: AtomicU64,
    /// Idle-rondes waarin niemand aan de beurt was.
    pub idle: AtomicU64,
    /// Kicks die de interrupt-dispatch van de kern claimde (ook die de kern
    /// uit zijn WFI haalden, niet alleen uit een beurt). Het board telt.
    pub kicks: AtomicU64,
    /// De laatste beurt: de bewoner in bits 7:0, [`Back::code`] in 15:8.
    /// Staat de console stil, dan zegt dit woord (via de monitor gelezen)
    /// wie de core het laatst had en hoe hij hem teruggaf.
    pub last: AtomicU64,
    /// De langste beurt sinds de vorige lezer hem nulde, in ticks.
    pub longest: AtomicU64,
}

/// De meetlat van deze node (één OS-core).
pub static STATS: Stats = Stats {
    entries: AtomicU64::new(0),
    irq: AtomicU64::new(0),
    ipi: AtomicU64::new(0),
    timer: AtomicU64::new(0),
    yields: AtomicU64::new(0),
    exits: AtomicU64::new(0),
    faults: AtomicU64::new(0),
    ticks: AtomicU64::new(0),
    idle: AtomicU64::new(0),
    kicks: AtomicU64::new(0),
    last: AtomicU64::new(0),
    longest: AtomicU64::new(0),
};

/// De rotatie van de OS-core. Eén, eigendom van de slaap van de executor
/// op die core (`cpu::idle::ArmSleeper::host`).
///
/// De bewonerslijst zelf staat in sched-blok 0 van het plan: de plaatsing
/// schrijft hem ([`host`], [`unhost`]), de rotatie leest hem. Dat is
/// dezelfde tabel als die van een gedeelde app-core, en beide schrijvers
/// draaien op deze ene core tussen twee rondes van de executor.
pub struct OsCore {
    sched: Pa,
    cage: Pa,
    flavor: Flavor,
    hcr: u64,
    bell: Option<Bell>,
}

/// Het laatste fault-rapport van een beurt: ESR, FAR, de PC van de
/// bewoner en de vector (1 synchroon, 2 IRQ, 3 FIQ), voor een proef die
/// geen control-page heeft (de voorproef op Apple, 30-09).
static LAST_FAULT: [core::sync::atomic::AtomicU64; 4] = [
    core::sync::atomic::AtomicU64::new(0),
    core::sync::atomic::AtomicU64::new(0),
    core::sync::atomic::AtomicU64::new(0),
    core::sync::atomic::AtomicU64::new(0),
];

/// Het laatste fault-rapport: `(esr, far, pc, vec)`; alles nul als er
/// nog geen beurt op een fault eindigde.
#[must_use]
pub fn last_fault() -> (u64, u64, u64, u64) {
    (
        LAST_FAULT[0].load(Relaxed),
        LAST_FAULT[1].load(Relaxed),
        LAST_FAULT[2].load(Relaxed),
        LAST_FAULT[3].load(Relaxed),
    )
}

impl OsCore {
    /// De rotatie over het plan `plan`, met de EL2-code van `flavor` en de
    /// kick `bell` (of geen, dan hoort de kern een app-core alleen op zijn
    /// eigen deadline). Zet eenmalig het EL2-regime dat een bewoner nodig
    /// heeft en de kern niet raakt: VTCR, CPTR zonder FP-trap, de
    /// timertoegang van EL1 (CNTHCTL) en CNTVOFF 0.
    ///
    /// Aanroepen op de OS-core zelf. Op Apple (`AppleVhe`) hoort de kick
    /// [`Bell::apple`] te zijn: een GIC-bel zou de switcher daar nooit
    /// sturen (hij kent alleen de fast IPI).
    pub fn new(plan: &Plan, flavor: Flavor, bell: Option<Bell>) -> Result<OsCore, Error> {
        let core0 = Core::new(0).ok_or(Error::BadContextId { id: 0 })?;
        let sched = plan.park_mbox_pa(core0).map_err(Error::Plan)?;
        arch::prepare(flavor);
        dev::write64(sched.add(SCHED_OS_KICK), 0);
        dev::write64(sched.add(SCHED_OS_KICK_PA), bell.map_or(0, |b| b.sgir));
        Ok(OsCore {
            sched,
            cage: plan.vec_base_pa(),
            flavor,
            hcr: (arch::hcr() & !HCR_TGE) | HCR_GUEST,
            bell,
        })
    }

    /// Zet de kick scherp (`true`) of uit: scherp zolang de kern een SEV niet
    /// hoort (in een beurt van een bewoner, en in een WFI-slaap).
    pub fn listen(&self, on: bool) {
        let v = match (on, self.bell) {
            (true, Some(b)) => b.sgi1r,
            _ => 0,
        };
        dev::write64(self.sched.add(SCHED_OS_KICK), v);
        dev::mb();
    }

    /// Eén beurt: de volgende bewoner die aan de beurt is (round-robin vanaf
    /// de cursor: een verse, of een geyielde wiens wektijd verstreek of
    /// wiens RX-ring groeide) draait tot hij de core teruggeeft of tot
    /// `deadline` (CNTPCT). Aanroepen met I en F gemaskeerd, ná de laatste
    /// `ready()`-toets van de executor.
    pub fn run(&mut self, deadline: u64) -> Turn {
        let now = arch::counter();
        let count = (dev::read64(self.sched.add(SCHED_COUNT)) as usize).min(SLOT_CAP);
        let cursor = dev::read64(self.sched.add(SCHED_CURSOR)) as usize;
        let mut earliest: Option<u64> = None;
        for k in 1..=count {
            let i = (cursor + k) % count;
            let id = dev::read8(self.sched.add(SCHED_LIST + i as u64));
            if id == 0 || usize::from(id) > SLOT_CAP {
                continue;
            }
            let ctx = self.ctx(id);
            match ctx_state(ctx) {
                Some(CtxState::BootPending) => {
                    return Turn::Ran(self.turn(i, id, ctx, deadline, true));
                }
                Some(CtxState::Saved) => match due(ctx, now) {
                    None => return Turn::Ran(self.turn(i, id, ctx, deadline, false)),
                    Some(t) => earliest = Some(earliest.map_or(t, |e| e.min(t))),
                },
                _ => {}
            }
        }
        STATS.idle.fetch_add(1, Relaxed);
        Turn::Idle { wake: earliest }
    }

    /// Het ctx-blok van kooi-context `id` (1..=SLOT_CAP).
    fn ctx(&self, id: u8) -> Pa {
        self.cage.add(u64::from(id) * CAGE_STRIDE + CTX_OFF)
    }

    /// De beurt van bewoner `id` op lijstplek `i`.
    fn turn(&mut self, i: usize, id: u8, ctx: Pa, deadline: u64, fresh: bool) -> Back {
        dev::write64(self.sched.add(SCHED_CURSOR), i as u64);
        dev::write64(self.sched.add(SCHED_CURRENT), u64::from(id));
        ctx_write(ctx, CTX_KICK_PENDING, 0);
        ctx_write(ctx, CTX_STATE, CtxState::Running.raw());
        // VTTBR naar de EENHEID van de bewoner (zoals de switcher): tabel en
        // VMID van zijn slot. Geen TLBI bij een hervatting: de entries zijn
        // VMID-getagd. Bij een verse bewoner wel, plus de I-cache: de VMID
        // en de PA's kunnen van een vorige huurder zijn (Altra 15-07).
        let unit = ctx_read(ctx, CTX_UNIT_SLOT) & 0xff;
        let vttbr = self.cage.add(unit * CAGE_STRIDE).0 | (unit << 48);
        arch::set_vttbr(vttbr, fresh);
        arch::timer_arm(deadline);
        self.listen(true);
        STATS.entries.fetch_add(1, Relaxed);
        let t0 = arch::counter();
        let vec = arch::enter(self.flavor, ctx, self.hcr);
        let dt = arch::counter().wrapping_sub(t0);
        STATS.ticks.fetch_add(dt, Relaxed);
        STATS.longest.fetch_max(dt, Relaxed);
        self.listen(false);
        let fired = arch::timer_disarm();
        dev::write64(self.sched.add(SCHED_CURRENT), 0);
        let back = self.settle(ctx, vec, fired);
        STATS.last.store(u64::from(id) | back.code() << 8, Relaxed);
        back
    }

    /// Zet de staat van een bewoner na zijn beurt, en zegt waardoor de kern
    /// terug is.
    fn settle(&self, ctx: Pa, vec: u64, fired: bool) -> Back {
        let back = match exit_of(vec) {
            Exit::Sync => {
                let esr = arch::esr();
                if esr >> 26 != crate::vectors::EC_HVC64 {
                    return self.fault(ctx, vec, esr);
                }
                let imm = esr & 0xffff;
                if imm == HVC_EXIT {
                    ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
                    STATS.exits.fetch_add(1, Relaxed);
                    return Back::Exit;
                }
                // x1 van de bewoner is zijn wektijd (alleen bij HVC #1; een
                // andere HVC, zoals een sibling-wek zonder siblings, is een
                // yield naar nu). Een kick die tijdens de beurt kwam, wint.
                let mut wake = if imm == HVC_YIELD {
                    ctx_read(ctx, CTX_GPRS + 8)
                } else {
                    0
                };
                if ctx_read(ctx, CTX_KICK_PENDING) != 0 {
                    ctx_write(ctx, CTX_KICK_PENDING, 0);
                    wake = 0;
                }
                ctx_write(ctx, CTX_WAKE, wake);
                STATS.yields.fetch_add(1, Relaxed);
                Back::Yield
            }
            Exit::Interrupt => {
                // Onderbroken midden in zijn werk: meteen weer aan de beurt.
                ctx_write(ctx, CTX_WAKE, 0);
                let kick = self.bell.is_some_and(|b| (b.pending)() == b.intid);
                let back = interrupted(fired, kick);
                match back {
                    Back::Timer => STATS.timer.fetch_add(1, Relaxed),
                    Back::Ipi => STATS.ipi.fetch_add(1, Relaxed),
                    _ => STATS.irq.fetch_add(1, Relaxed),
                };
                // Apple: de fast IPI hier acken, op EL2, vóór de kern zijn
                // maskers opent. Een GIC-SGI blijft pending tot de dispatch
                // van de kern hem claimt; een fast IPI die niemand ackt,
                // blijft staan en de core komt nooit meer tot slapen (04-09).
                // Een FIQ die geen van beide is (een timer van de bewoner
                // zelf, een PMC) telt als device: de kern kijkt in zijn
                // eigen dispatch.
                if back == Back::Ipi && self.flavor == Flavor::AppleVhe {
                    arch::apple_ipi_ack();
                }
                back
            }
            Exit::Fault => return self.fault(ctx, vec, arch::esr()),
        };
        ctx_write(ctx, CTX_STATE, CtxState::Saved.raw());
        back
    }

    /// Het fault-rapport op de control-page (vec+1, ESR, FAR, zoals de
    /// switcher), en de bewoner dood. De fault-PC staat al in zijn
    /// resume-woord.
    fn fault(&self, ctx: Pa, vec: u64, esr: u64) -> Back {
        // Ook zonder control-page (de zelftest, de voorproef van een board)
        // blijft het rapport leesbaar: [`last_fault`].
        LAST_FAULT[0].store(esr, Relaxed);
        LAST_FAULT[1].store(arch::far(), Relaxed);
        LAST_FAULT[2].store(ctx_read(ctx, CTX_RESUME), Relaxed);
        LAST_FAULT[3].store(vec + 1, Relaxed);
        let cp = ctx_read(ctx, CTX_CTRL_PA);
        if cp != 0 {
            for (off, v) in [
                (CTRL_FAULT_VEC, vec + 1),
                (CTRL_FAULT_ESR, esr),
                (CTRL_FAULT_FAR, arch::far()),
            ] {
                dev::write64(Pa(cp).add(off), v);
                dev::push(Pa(cp).add(off), 8);
            }
        }
        ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
        STATS.faults.fetch_add(1, Relaxed);
        Back::Fault
    }

    /// De zelftest bij boot: een bewoner zonder kooi (VM=0, MMU uit, een
    /// stub in het kern-image) die spint (`yield_ = false`) of meteen HVC #1
    /// doet, met `kick` vlak vóór de overgang (bijvoorbeeld een SGI naar
    /// deze core, om het IPI-pad te bewijzen), en de CNTHP op `ticks` na
    /// nu. Geeft wat de proef zag ([`Probe`]); `None` als er geen heap was
    /// voor het scratch-ctx-blok.
    pub fn selftest(&mut self, yield_: bool, ticks: u64, kick: &dyn Fn()) -> Option<Probe> {
        let mut block: alloc::vec::Vec<u64> = alloc::vec::Vec::new();
        block.try_reserve_exact(CTX_WORDS).ok()?;
        block.resize(CTX_WORDS, 0);
        let ctx = Pa(block.as_mut_ptr().addr() as u64);
        // De spinner geeft na twee keer de termijn plus een vangrail zelf op
        // (een yield): een board dat de CNTHP of de kick niet naar deze core
        // routeert, geeft zo een rode zelftest in plaats van een hangende
        // boot. Zie [`spin_limit`] voor de vangrail.
        let limit = spin_limit(arch::counter(), ticks, arch::freq());
        prepare(ctx, arch::stub(yield_), limit);
        ctx_write(ctx, CTX_STATE, CtxState::Running.raw());
        let hcr = self.hcr & !HCR_VM;
        let daif = arch::mask();
        let peek = || self.bell.map_or(Probe::NONE, |b| (b.pending)());
        let before = peek();
        arch::timer_arm(arch::counter().wrapping_add(ticks));
        kick();
        let t0 = arch::counter();
        let vec = arch::enter(self.flavor, ctx, hcr);
        let dt = arch::counter().wrapping_sub(t0);
        let after = peek();
        let fired = arch::timer_disarm();
        let back = self.settle(ctx, vec, fired);
        arch::restore(daif);
        // De zelftest telt niet mee in de meetlat van de bewoners.
        let counter = match back {
            Back::Yield => &STATS.yields,
            Back::Exit => &STATS.exits,
            Back::Irq => &STATS.irq,
            Back::Ipi => &STATS.ipi,
            Back::Timer => &STATS.timer,
            Back::Fault => &STATS.faults,
        };
        counter.fetch_sub(1, Relaxed);
        drop(block);
        Some(Probe {
            back,
            ticks: dt,
            vec,
            before,
            after,
        })
    }
}

// ---------------------------------------------------------------------------
// De verhuizing: de oude boot-core wordt een app-core.
// ---------------------------------------------------------------------------

/// De MPIDR van de core die in [`hold`] wacht, met bit 63 gezet (0 = geen:
/// MPIDR 0 is een geldige core).
static HELD: AtomicU64 = AtomicU64::new(0);
/// De parkeerlus van het plan, 0 zolang de kooi-regio er nog niet is.
static HOLD_PARK: AtomicU64 = AtomicU64::new(0);
/// De park-mailbox van de wachtende core: TPIDR_EL2 van de parkeerlus.
static HOLD_MBOX: AtomicU64 = AtomicU64::new(0);

/// Hoe lang [`release_held`] op de parkeerlus wacht: 100 ms. Op QEMU is het
/// een paar microseconden; een core die het in 100 ms niet haalt, is een
/// fout, geen trage core.
const HOLD_GRACE_NS: u64 = 100_000_000;

/// Geeft deze core op nadat de kern naar de OS-core verhuisde (Go:
/// `parkenter`): hij wacht met de maskers dicht tot de kooi-regio er is
/// ([`release_held`]), zet dan zijn EL2-MMU en caches uit, zoals elke
/// app-core na PSCI CPU_ON draait, en springt de parkeerlus van het plan
/// in. Vanaf daar is hij een geparkeerde app-core: een dispatch is mailbox
/// plus SEV, geen CPU_ON (dat gaf op een levende core ALREADY_ON). Geen
/// CPU_OFF: dat was op de Pi 5-stockfirmware een deur zonder terugweg
/// (10-07).
pub fn hold() -> ! {
    HELD.store(arch::mpidr() | (1 << 63), Release);
    dev::notify();
    loop {
        let (mbox, park) = (HOLD_MBOX.load(Acquire), HOLD_PARK.load(Acquire));
        if mbox != 0 && park != 0 {
            arch::park(mbox, park);
        }
        arch::wfe();
    }
}

/// De MPIDR van de core die in [`hold`] wacht, als er een is.
#[must_use]
pub fn held() -> Option<u64> {
    let v = HELD.load(Acquire);
    (v != 0).then_some(v & !(1 << 63))
}

/// Haalt de wachtende boot-core de parkeerlus van `plan` in, als logische
/// app-core `core`: zijn park-mailbox, dan het startschot voor [`hold`], en
/// wachten tot de mailbox "geparkeerd" zegt. Aanroepen na
/// [`super::init_app_cores`] en vóór de eerste dispatch.
pub fn release_held(plan: &Plan, core: Core) -> Result<(), Error> {
    let mbox = plan.park_mbox_pa(core).map_err(Error::Plan)?;
    HOLD_MBOX.store(mbox.0, Release);
    HOLD_PARK.store(plan.park_code_pa().0, Release);
    dev::notify();
    let grace = crate::idle::ns_to_ticks(HOLD_GRACE_NS, arch::freq());
    let t0 = arch::counter();
    loop {
        let w = dev::read64(mbox.add(super::layout::SCHED_MBOX_CTX));
        if w == super::layout::PARK_PARKED {
            return Ok(());
        }
        if arch::counter().wrapping_sub(t0) > grace {
            return Err(Error::UnparkedCore {
                core: core.get(),
                mbox: w,
            });
        }
        core::hint::spin_loop();
    }
}

/// `None` als de geyielde bewoner van `ctx` aan de beurt is op
/// counterstand `now`, anders zijn wektijd. Dezelfde vraag als de switcher
/// stelt: de wektijd (0 = meteen), een kick, of RX voorbij de gewapende
/// drempel (niet bij bit 63, een wachter zonder peek).
fn due(ctx: Pa, now: u64) -> Option<u64> {
    let w = ctx_read(ctx, CTX_WAKE);
    let t = w & !CTX_WAKE_NO_PEEK;
    if t == 0 || now >= t || ctx_read(ctx, CTX_KICK_PENDING) != 0 {
        return None;
    }
    if w & CTX_WAKE_NO_PEEK == 0 && rx_due(ctx) {
        return None;
    }
    Some(t)
}

/// Legt de eerste beurt van een bewoner klaar: x0 = `arg` (de control-page,
/// zoals de trampoline hem doorgeeft), de rest nul, hervatten op `entry` in
/// EL1h met DAIF dicht, en een schoon EL1-regime (SCTLR zonder MMU, de rest
/// nul). Wat de trampoline van een app-core verder zet (VTCR, CPTR, CNTHCTL,
/// CNTVOFF) deed [`OsCore::new`] één keer voor de hele core.
fn prepare(ctx: Pa, entry: u64, arg: u64) {
    for r in 0..31 {
        ctx_write(ctx, CTX_GPRS + 8 * r, 0);
    }
    ctx_write(ctx, CTX_GPRS, arg);
    ctx_write(ctx, CTX_SP, 0);
    ctx_write(ctx, CTX_SP + 8, 0);
    ctx_write(ctx, CTX_RESUME, entry);
    ctx_write(ctx, CTX_RESUME + 8, SPSR_EL1H_MASKED);
    for w in 0..CTX_REGIME_ARM_WORDS {
        ctx_write(ctx, CTX_REGIME + 8 * w, 0);
    }
    ctx_write(ctx, CTX_REGIME, SCTLR_EL1_CLEAN);
    ctx_write(ctx, CTX_WAKE, 0);
    ctx_write(ctx, CTX_KICK_PENDING, 0);
}

/// Sched-blok 0: de bewonerslijst van de OS-core.
fn os_sched(plan: &Plan) -> Result<Pa, Error> {
    let core0 = Core::new(0).ok_or(Error::BadContextId { id: 0 })?;
    plan.park_mbox_pa(core0).map_err(Error::Plan)
}

/// De kooi-context-id van `ctx`, alleen voor een kooi (geen secundaire).
fn cage_id(plan: &Plan, ctx: Pa) -> Result<u8, Error> {
    context_id(plan, ctx)
        .filter(|id| usize::from(*id) <= SLOT_CAP)
        .ok_or(Error::BadContext { pa: ctx.0 })
}

/// De plek van `id` in de bewonerslijst van `sched`.
fn position(sched: Pa, id: u8) -> Option<usize> {
    let count = (dev::read64(sched.add(SCHED_COUNT)) as usize).min(SLOT_CAP);
    (0..count).find(|i| dev::read8(sched.add(SCHED_LIST + *i as u64)) == id)
}

/// Maakt de bewoner met ctx-blok `ctx` bewoner van de OS-core: zijn eerste
/// beurt begint op `entry` met x0 = `arg` (de control-page), en de rotatie
/// neemt hem mee zodra de executor idle is. Het tegenstuk van
/// [`super::dispatch`] voor een app-core, zonder mailbox: de kern ís de
/// core.
///
/// Eerst de ctx, dan de lijst: de rotatie leest de lijst pas in de
/// volgende idle-ronde, op deze zelfde core.
pub fn host(plan: &Plan, ctx: Pa, entry: u64, arg: u64) -> Result<(), Error> {
    cage_id(plan, ctx)?;
    prepare(ctx, entry, arg);
    ctx_write(ctx, CTX_STATE, CtxState::BootPending.raw());
    rehost(plan, ctx).map(|_| ())
}

/// Zet de bewoner met ctx-blok `ctx` terug in de rotatie van de OS-core
/// zonder één woord van zijn ctx te raken: na een kern-flip draagt het blok
/// zijn bewaarde staat nog, en de nieuwe kern hervat hem waar de oude hem
/// liet. Geeft `true` als hij er niet meer in stond.
pub fn rehost(plan: &Plan, ctx: Pa) -> Result<bool, Error> {
    let id = cage_id(plan, ctx)?;
    let sched = os_sched(plan)?;
    if position(sched, id).is_some() {
        return Ok(false);
    }
    let count = (dev::read64(sched.add(SCHED_COUNT)) as usize).min(SLOT_CAP);
    let at = (0..count).find(|i| dev::read8(sched.add(SCHED_LIST + *i as u64)) == 0);
    let i = match at {
        Some(i) => i,
        None if count < SLOT_CAP => {
            dev::write64(sched.add(SCHED_COUNT), count as u64 + 1);
            count
        }
        None => return Err(Error::RosterFull { count }),
    };
    dev::write8(sched.add(SCHED_LIST + i as u64), id);
    dev::mb();
    Ok(true)
}

/// Haalt de bewoner met ctx-blok `ctx` uit de rotatie van de OS-core (een
/// gat in de lijst, zoals bij de switcher). Geeft of hij erin stond. Daarna
/// draait hij hier nooit meer: de kern is de enige die hem de core geeft.
pub fn unhost(plan: &Plan, ctx: Pa) -> Result<bool, Error> {
    let id = cage_id(plan, ctx)?;
    let sched = os_sched(plan)?;
    let Some(i) = position(sched, id) else {
        return Ok(false);
    };
    dev::write8(sched.add(SCHED_LIST + i as u64), 0);
    dev::mb();
    Ok(true)
}

/// Staat de bewoner met ctx-blok `ctx` in de rotatie van de OS-core?
#[must_use]
pub fn hosts(plan: &Plan, ctx: Pa) -> bool {
    match (cage_id(plan, ctx), os_sched(plan)) {
        (Ok(id), Ok(sched)) => position(sched, id).is_some(),
        _ => false,
    }
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    //! De instructies van de overgang: de EL2-kant van de OS-core. Elk blok
    //! raakt alleen registers van deze core, en de enige sprong naar een
    //! lagere EL is [`enter`].
    use super::super::layout::{CTX_GPRS, CTX_REGIME, CTX_RESUME, CTX_SP};
    use super::Flavor;
    use core::arch::{asm, global_asm};
    use dev::Pa;

    /// De teller (CNTPCT_EL0).
    #[inline]
    pub(super) fn counter() -> u64 {
        let v: u64;
        // SAFETY: een lees van de teller heeft geen neveneffect.
        unsafe { asm!("isb", "mrs {}, cntpct_el0", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn mpidr() -> u64 {
        let v: u64;
        // SAFETY: MPIDR_EL1 lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, mpidr_el1", out(reg) v, options(nomem, nostack)) };
        v & 0xff_00ff_ffff
    }

    pub(super) fn freq() -> u64 {
        let v: u64;
        // SAFETY: CNTFRQ_EL0 lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, cntfrq_el0", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn wfe() {
        // SAFETY: WFE wacht op een event; geen geheugeneffect.
        unsafe { asm!("wfe", options(nomem, nostack, preserves_flags)) };
    }

    /// De sprong van [`super::hold`] de parkeerlus in: maskers dicht, de
    /// EL2-MMU en de caches uit (de parkeerlus en de trampoline draaien
    /// zoals op elke app-core met de MMU uit, en de plan-regio is in de map
    /// van de kern XN), de eigen I-cache leeg, TPIDR_EL2 = de mailbox.
    pub(super) fn park(mbox: u64, park: u64) -> ! {
        // SAFETY: deze core draait niets meer van de kern (de kern
        // verhuisde, zijn stack wordt niet meer gelezen); de code hier staat
        // identity-gemapt (VA = PA), dus de fetch na het uitzetten van de
        // MMU leest dezelfde bytes. `park` is de parkeerlus van het plan,
        // `mbox` de park-mailbox van deze core: dat is precies wat de lus
        // van TPIDR_EL2 verwacht.
        unsafe {
            asm!(
                "msr daifset, #0xf",
                "mrs {t}, sctlr_el2",
                "bic {t}, {t}, #1",
                "bic {t}, {t}, #(1 << 2)",
                "bic {t}, {t}, #(1 << 12)",
                "dsb sy",
                "msr sctlr_el2, {t}",
                "isb",
                "ic iallu",
                "dsb sy",
                "isb",
                "msr tpidr_el2, {m}",
                "br {p}",
                t = in(reg) 0u64,
                m = in(reg) mbox,
                p = in(reg) park,
                options(noreturn, nostack),
            )
        }
    }

    /// Maskeert I en F en geeft DAIF zoals het stond.
    pub(super) fn mask() -> u64 {
        let v: u64;
        // SAFETY: DAIF lezen en maskeren raakt alleen PSTATE van deze core;
        // geen `nomem`, zodat er geen toegang over het masker schuift.
        unsafe { asm!("mrs {}, daif", "msr daifset, #3", out(reg) v, options(nostack)) };
        v
    }

    /// Zet DAIF terug zoals `mask` hem las.
    pub(super) fn restore(daif: u64) {
        // SAFETY: zie `mask`.
        unsafe { asm!("msr daif, {}", in(reg) daif, options(nostack)) };
    }

    pub(super) fn hcr() -> u64 {
        let v: u64;
        // SAFETY: HCR_EL2 lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, hcr_el2", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn esr() -> u64 {
        let v: u64;
        // SAFETY: ESR_EL2 lezen heeft geen neveneffect; hij beschrijft de
        // trap waarmee de bewoner net terugkwam (sindsdien geen exception:
        // I en F dicht, en de kern-code ertussen faultt niet).
        unsafe { asm!("mrs {}, esr_el2", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn far() -> u64 {
        let v: u64;
        // SAFETY: zie `esr`.
        unsafe { asm!("mrs {}, far_el2", out(reg) v, options(nomem, nostack)) };
        v
    }

    /// Het EL2-regime dat een bewoner nodig heeft en de kern niet raakt,
    /// eenmalig: VTCR (4 KB-granule, 39-bit IPA, PS = min(PARange, 44 bit),
    /// dezelfde waarde als de trampoline), CPTR zonder FP-trap (de kern is
    /// softfloat), CNTHCTL met de teller- en timertoegang van EL1 in beide
    /// lay-outs, en CNTVOFF 0: de wektijd van een yield is dan dezelfde
    /// stand als CNTPCT.
    pub(super) fn prepare(flavor: Flavor) {
        const VTCR_NO_PS: u64 = 0x8000_3559;
        const PARANGE_44: u64 = 4;
        let cptr: u64 = match flavor {
            Flavor::Nvhe => 0x33FF,
            Flavor::Vhe | Flavor::AppleVhe => 0x30_0000,
        };
        // SAFETY: registers van het EL2-regime die alleen een lagere EL
        // raken (VTCR, CNTVOFF, de EL1-toegang in CNTHCTL) of een trap
        // weghalen die de softfloat-kern nooit raakt (CPTR); geen geheugen.
        unsafe {
            asm!(
                "mrs {t}, id_aa64mmfr0_el1",
                "and {t}, {t}, #0xf",
                "cmp {t}, {p44}",
                "csel {t}, {t}, {p44}, lo",
                "orr {t}, {vtcr}, {t}, lsl #16",
                "msr vtcr_el2, {t}",
                "msr cptr_el2, {cptr}",
                "mrs {t}, cnthctl_el2",
                "orr {t}, {t}, #0x3",
                "orr {t}, {t}, #(0x3 << 10)",
                "msr cnthctl_el2, {t}",
                "msr cntvoff_el2, xzr",
                "isb",
                t = out(reg) _,
                p44 = in(reg) PARANGE_44,
                vtcr = in(reg) VTCR_NO_PS,
                cptr = in(reg) cptr,
                options(nomem, nostack),
            );
        }
    }

    /// VTTBR_EL2 op de eenheid van de bewoner; `fresh`: plus een TLBI van
    /// deze VMID en een lege I-cache (een verse bewoner op hergebruikte PA's
    /// en VMID).
    pub(super) fn set_vttbr(v: u64, fresh: bool) {
        // SAFETY: VTTBR_EL2 vertaalt alleen voor EL1/EL0; de kern op EL2
        // merkt er niets van. De TLBI en IC raken alleen caches van deze
        // core.
        unsafe {
            asm!("msr vttbr_el2, {}", "isb", in(reg) v, options(nostack));
            if fresh {
                asm!(
                    "tlbi vmalls12e1",
                    "dsb nsh",
                    "ic iallu",
                    "dsb nsh",
                    "isb",
                    options(nostack)
                );
            }
        }
    }

    /// De CNTHP (de fysieke EL2-timer, PPI 26) op `deadline`: een bewoner
    /// op EL1 kan hem niet zien of uitzetten, anders dan de CNTP.
    pub(super) fn timer_arm(deadline: u64) {
        // SAFETY: alleen de EL2-timer van deze core.
        unsafe {
            asm!(
                "msr cnthp_cval_el2, {d}",
                "msr cnthp_ctl_el2, {one}",
                "isb",
                d = in(reg) deadline,
                one = in(reg) 1u64,
                options(nomem, nostack),
            );
        }
    }

    /// De CNTHP uit; `true` als hij afging (ISTATUS). Uit laat zijn lijn
    /// vallen, dus de GIC hoeft hem niet te claimen.
    pub(super) fn timer_disarm() -> bool {
        let ctl: u64;
        // SAFETY: alleen de EL2-timer van deze core.
        unsafe {
            asm!(
                "mrs {c}, cnthp_ctl_el2",
                "msr cnthp_ctl_el2, xzr",
                "isb",
                c = out(reg) ctl,
                options(nomem, nostack),
            );
        }
        ctl & (1 << 2) != 0
    }

    /// IPI_SR_EL1 bit 0: wacht er een fast IPI op deze core? Zonder ack
    /// (de peek van [`super::Bell::apple`]); alleen op Apple silicium.
    pub(super) fn apple_ipi_pending() -> u32 {
        let v: u64;
        // SAFETY: IPI_SR_EL1 lezen heeft geen neveneffect; de aanroepers
        // zijn de bel van `Bell::apple` en `apple_ipi_ack`, en die bestaan
        // alleen op een Apple-core, waar het register er is.
        unsafe { asm!("mrs {}, s3_5_c15_c1_1", out(reg) v, options(nomem, nostack)) };
        if v & 1 != 0 {
            super::Bell::APPLE_INTID
        } else {
            0
        }
    }

    /// Ackt de fast IPI van deze core (IPI_SR_EL1, bit 0 is W1C).
    pub(super) fn apple_ipi_ack() {
        // SAFETY: raakt alleen de IPI-status van deze core; alleen geroepen
        // onder `Flavor::AppleVhe` of uit de dispatch van het Apple-board,
        // dus op een Apple-core.
        unsafe { asm!("msr s3_5_c15_c1_1, {}", "isb", in(reg) 1u64, options(nomem, nostack)) };
    }

    unsafe extern "C" {
        fn hopos_os_nvhe_enter(ctx: u64, hcr: u64, vbar: u64) -> u64;
        fn hopos_os_vhe_enter(ctx: u64, hcr: u64, vbar: u64) -> u64;
        safe static hopos_os_nvhe_vectors: u8;
        safe static hopos_os_vhe_vectors: u8;
        safe static hopos_os_stub_spin: u8;
        safe static hopos_os_stub_yield: u8;
    }

    /// Het adres van een label in het kern-image (identity: ook de PA).
    fn addr(p: *const u8) -> u64 {
        p.addr() as u64
    }

    /// De ingang van de zelftest-bewoner.
    pub(super) fn stub(yield_: bool) -> u64 {
        if yield_ {
            addr(&raw const hopos_os_stub_yield)
        } else {
            addr(&raw const hopos_os_stub_spin)
        }
    }

    /// Eén beurt van de bewoner met ctx-blok `ctx`, onder HCR_EL2 = `hcr`.
    /// Geeft de vectorindex waarmee hij terugkwam (8 synchroon, 9 IRQ, 10
    /// FIQ, 11 SError, 12..15 AArch32).
    pub(super) fn enter(flavor: Flavor, ctx: Pa, hcr: u64) -> u64 {
        // Apple is VHE (E2H RES1): dezelfde overgang als de O6N. Het verschil
        // (de kick als FIQ, de ack op EL2) zit in `settle`, niet hier.
        let (f, vbar): (unsafe extern "C" fn(u64, u64, u64) -> u64, u64) = match flavor {
            Flavor::Nvhe => (hopos_os_nvhe_enter, addr(&raw const hopos_os_nvhe_vectors)),
            Flavor::Vhe | Flavor::AppleVhe => {
                (hopos_os_vhe_enter, addr(&raw const hopos_os_vhe_vectors))
            }
        };
        // SAFETY: `ctx` is een ctx-blok van het plan (of de scratch van de
        // zelftest) met een hervatbaar regime, en de aanroeper heeft I en F
        // gemaskeerd. De stub bewaart de callee-saved registers, HCR en
        // VBAR van de kern op zijn eigen stack en zet ze terug vóór hij
        // terugkeert; de bewoner draait op EL1 onder stage-2 (of, bij de
        // zelftest, zonder MMU op een eigen stub in het kern-image) en kan
        // de EL2-stack niet zien. Elke exception uit de bewoner komt op
        // `vbar` en keert via dezelfde frame terug.
        unsafe { f(ctx.0, hcr, vbar) }
    }

    // De overgang en de vectoren, twee smaken uit één bron (zoals de
    // switcher): nVHE (op1 = 0) en VHE (op1 = 5, de _EL12-encoderingen).
    // De VHE-smaak eist dat de kern zelf onder E2H = 1 draait: onder E2H = 0
    // zijn de _EL12-encoderingen UNDEFINED (29-09, QEMU neoverse-n1 met een
    // nVHE-kern: EC 0x0 op `hopos_os_vhe_enter` + 0x40 bij de zelftest).
    // Het board kiest die vorm (board/uefi/src/el2.rs), de binary toetst
    // dat die bij de smaak past (hopos/src/cage.rs).
    //
    // De frame op SP_EL2 (128 bytes): +0..+88 x19..x30, +96 x18 en de ctx,
    // +112 HCR en VBAR van de kern. SP_EL2 verandert niet door een beurt op
    // EL1, dus de exception komt binnen met SP = deze frame.
    //
    // Veiligheid (als gewone regel: clippy weigert een SAFETY-tag op
    // `global_asm!`): deze code draait alleen via `enter` hierboven, met de
    // maskers dicht; de regime-save en -restore zijn die van de switcher
    // (switch.rs), die ze op ijzer bewees.
    global_asm!(
        r#"
    .macro hopos_os_flavor p, op1
    .pushsection .text.hopos_os, "ax"
    .balign 16
    .global \p\()_enter
\p\()_enter:
    sub sp, sp, #128
    stp x19, x20, [sp, #0]
    stp x21, x22, [sp, #16]
    stp x23, x24, [sp, #32]
    stp x25, x26, [sp, #48]
    stp x27, x28, [sp, #64]
    stp x29, x30, [sp, #80]
    stp x18, x0, [sp, #96]
    mrs x9, hcr_el2
    mrs x10, vbar_el2
    stp x9, x10, [sp, #112]
    msr vbar_el2, x2
    msr hcr_el2, x1
    isb
    mov x1, x0
    ldp x2, x3, [x1, #({regime} + 0 * 8)]
    msr s3_\op1\()_c1_c0_0, x2
    msr s3_\op1\()_c2_c0_2, x3
    ldp x2, x3, [x1, #({regime} + 2 * 8)]
    msr s3_\op1\()_c2_c0_0, x2
    msr s3_\op1\()_c2_c0_1, x3
    ldp x2, x3, [x1, #({regime} + 4 * 8)]
    msr s3_\op1\()_c10_c2_0, x2
    msr s3_\op1\()_c10_c3_0, x3
    ldp x2, x3, [x1, #({regime} + 6 * 8)]
    msr s3_\op1\()_c12_c0_0, x2
    msr tpidr_el0, x3
    ldp x2, x3, [x1, #({regime} + 8 * 8)]
    msr tpidrro_el0, x2
    msr tpidr_el1, x3
    ldp x2, x3, [x1, #({regime} + 10 * 8)]
    msr s3_\op1\()_c13_c0_1, x2
    msr s3_\op1\()_c1_c0_2, x3
    ldp x2, x3, [x1, #({regime} + 12 * 8)]
    msr s3_\op1\()_c14_c1_0, x2
    msr csselr_el1, x3
    ldp x2, x3, [x1, #({regime} + 14 * 8)]
    msr par_el1, x2
    msr s3_\op1\()_c4_c0_1, x3
    ldp x2, x3, [x1, #({regime} + 16 * 8)]
    msr s3_\op1\()_c4_c0_0, x2
    msr s3_\op1\()_c5_c2_0, x3
    ldr x2, [x1, #({regime} + 18 * 8)]
    msr s3_\op1\()_c6_c0_0, x2
    ldp x2, x3, [x1, #{ctx_sp}]
    msr sp_el0, x2
    msr sp_el1, x3
    ldp x2, x3, [x1, #{resume}]
    msr elr_el2, x2
    msr spsr_el2, x3
    ldp x4, x5, [x1, #({gprs} + 4 * 8)]
    ldp x6, x7, [x1, #({gprs} + 6 * 8)]
    ldp x8, x9, [x1, #({gprs} + 8 * 8)]
    ldp x10, x11, [x1, #({gprs} + 10 * 8)]
    ldp x12, x13, [x1, #({gprs} + 12 * 8)]
    ldp x14, x15, [x1, #({gprs} + 14 * 8)]
    ldp x16, x17, [x1, #({gprs} + 16 * 8)]
    ldp x18, x19, [x1, #({gprs} + 18 * 8)]
    ldp x20, x21, [x1, #({gprs} + 20 * 8)]
    ldp x22, x23, [x1, #({gprs} + 22 * 8)]
    ldp x24, x25, [x1, #({gprs} + 24 * 8)]
    ldp x26, x27, [x1, #({gprs} + 26 * 8)]
    ldp x28, x29, [x1, #({gprs} + 28 * 8)]
    ldr x30, [x1, #({gprs} + 30 * 8)]
    ldp x2, x3, [x1, #({gprs} + 2 * 8)]
    ldr x0, [x1, #{gprs}]
    ldr x1, [x1, #({gprs} + 1 * 8)]
    isb
    eret

// De terugweg, voor elke exception uit de bewoner. x0 = de vectorindex, de
// x0/x1 van de bewoner staan op [sp] (16 bytes onder de frame).
\p\()_back:
    ldr x1, [sp, #(16 + 104)]
    stp x2, x3, [x1, #({gprs} + 2 * 8)]
    stp x4, x5, [x1, #({gprs} + 4 * 8)]
    stp x6, x7, [x1, #({gprs} + 6 * 8)]
    stp x8, x9, [x1, #({gprs} + 8 * 8)]
    stp x10, x11, [x1, #({gprs} + 10 * 8)]
    stp x12, x13, [x1, #({gprs} + 12 * 8)]
    stp x14, x15, [x1, #({gprs} + 14 * 8)]
    stp x16, x17, [x1, #({gprs} + 16 * 8)]
    stp x18, x19, [x1, #({gprs} + 18 * 8)]
    stp x20, x21, [x1, #({gprs} + 20 * 8)]
    stp x22, x23, [x1, #({gprs} + 22 * 8)]
    stp x24, x25, [x1, #({gprs} + 24 * 8)]
    stp x26, x27, [x1, #({gprs} + 26 * 8)]
    stp x28, x29, [x1, #({gprs} + 28 * 8)]
    str x30, [x1, #({gprs} + 30 * 8)]
    ldp x2, x3, [sp], #16
    stp x2, x3, [x1, #{gprs}]
    mrs x2, elr_el2
    mrs x3, spsr_el2
    stp x2, x3, [x1, #{resume}]
    mrs x2, sp_el0
    mrs x3, sp_el1
    stp x2, x3, [x1, #{ctx_sp}]
    mrs x2, s3_\op1\()_c1_c0_0
    mrs x3, s3_\op1\()_c2_c0_2
    stp x2, x3, [x1, #({regime} + 0 * 8)]
    mrs x2, s3_\op1\()_c2_c0_0
    mrs x3, s3_\op1\()_c2_c0_1
    stp x2, x3, [x1, #({regime} + 2 * 8)]
    mrs x2, s3_\op1\()_c10_c2_0
    mrs x3, s3_\op1\()_c10_c3_0
    stp x2, x3, [x1, #({regime} + 4 * 8)]
    mrs x2, s3_\op1\()_c12_c0_0
    mrs x3, tpidr_el0
    stp x2, x3, [x1, #({regime} + 6 * 8)]
    mrs x2, tpidrro_el0
    mrs x3, tpidr_el1
    stp x2, x3, [x1, #({regime} + 8 * 8)]
    mrs x2, s3_\op1\()_c13_c0_1
    mrs x3, s3_\op1\()_c1_c0_2
    stp x2, x3, [x1, #({regime} + 10 * 8)]
    mrs x2, s3_\op1\()_c14_c1_0
    mrs x3, csselr_el1
    stp x2, x3, [x1, #({regime} + 12 * 8)]
    mrs x2, par_el1
    mrs x3, s3_\op1\()_c4_c0_1
    stp x2, x3, [x1, #({regime} + 14 * 8)]
    mrs x2, s3_\op1\()_c4_c0_0
    mrs x3, s3_\op1\()_c5_c2_0
    stp x2, x3, [x1, #({regime} + 16 * 8)]
    mrs x2, s3_\op1\()_c6_c0_0
    str x2, [x1, #({regime} + 18 * 8)]
    ldp x2, x3, [sp, #112]
    msr hcr_el2, x2
    msr vbar_el2, x3
    isb
    ldp x19, x20, [sp, #0]
    ldp x21, x22, [sp, #16]
    ldp x23, x24, [sp, #32]
    ldp x25, x26, [sp, #48]
    ldp x27, x28, [sp, #64]
    ldp x29, x30, [sp, #80]
    ldr x18, [sp, #96]
    add sp, sp, #128
    ret

// De vectoren tijdens een beurt. De huidige EL (0..7) is de kern zelf: door
// naar zijn eigen tabel (een fout in de kern blijft een fout van de kern).
// Een lagere EL (8..15) is de bewoner: terug naar de kern.
    .balign 2048
    .global \p\()_vectors
\p\()_vectors:
    .irp i, 0, 1, 2, 3, 4, 5, 6, 7
    .balign 0x80
    b __hopos_vectors + \i * 0x80
    .endr
    .irp i, 8, 9, 10, 11, 12, 13, 14, 15
    .balign 0x80
    stp x0, x1, [sp, #-16]!
    mov x0, #\i
    b \p\()_back
    .endr
    .popsection
    .endm

    hopos_os_flavor hopos_os_nvhe, 0
    hopos_os_flavor hopos_os_vhe, 5

// De zelftest-bewoner: EL1 zonder MMU, uit het kern-image. Spinnen tot de
// kern hem onderbreekt (hooguit tot tellerstand x0, dan toch een yield), of
// meteen yielden (x1 = 0: wek nu).
    .pushsection .text.hopos_os, "ax"
    .balign 16
    .global hopos_os_stub_spin
hopos_os_stub_spin:
    isb
    mrs x2, cntvct_el0
    cmp x2, x0
    b.lo hopos_os_stub_spin
    b hopos_os_stub_yield
    .global hopos_os_stub_yield
hopos_os_stub_yield:
    mov x1, #0
    hvc #1
    b hopos_os_stub_yield
    .popsection
"#,
        gprs = const CTX_GPRS,
        regime = const CTX_REGIME,
        resume = const CTX_RESUME,
        ctx_sp = const CTX_SP,
    );
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-stubs: geen EL2. De rotatie en de lijst bewijzen de tests; de
    //! overgang zelf bewijst het board.
    use super::Flavor;
    use dev::Pa;

    pub(super) fn counter() -> u64 {
        0
    }
    pub(super) fn mask() -> u64 {
        0
    }
    pub(super) fn restore(_daif: u64) {}
    pub(super) fn mpidr() -> u64 {
        0
    }
    pub(super) fn freq() -> u64 {
        62_500_000
    }
    pub(super) fn wfe() {
        core::hint::spin_loop();
    }
    pub(super) fn park(_mbox: u64, _park: u64) -> ! {
        loop {
            core::hint::spin_loop();
        }
    }
    pub(super) fn hcr() -> u64 {
        0
    }
    pub(super) fn esr() -> u64 {
        0
    }
    pub(super) fn far() -> u64 {
        0
    }
    pub(super) fn prepare(_flavor: Flavor) {}
    pub(super) fn set_vttbr(_v: u64, _fresh: bool) {}
    pub(super) fn timer_arm(_deadline: u64) {}
    pub(super) fn timer_disarm() -> bool {
        false
    }
    pub(super) fn stub(_yield: bool) -> u64 {
        0
    }
    pub(super) fn apple_ipi_pending() -> u32 {
        0
    }
    pub(super) fn apple_ipi_ack() {}
    /// Op de host komt elke beurt meteen terug op een IRQ (vector 9); de
    /// tests toetsen de rotatie, niet de overgang.
    pub(super) fn enter(_flavor: Flavor, _ctx: Pa, _hcr: u64) -> u64 {
        super::VEC_IRQ_LOWER
    }
}

#[cfg(test)]
mod tests;
