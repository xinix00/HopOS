//! De slot-kant van de kern-binary: de kooi-lijm ([`cage`]), de
//! lifecycle-actor en de servicers, en het eerste bewijs: appspike in een
//! slot op QEMU.
//!
//! Wat hier gebeurt, is de board-glue van `kern::slots`: het plan van het
//! board wordt een `PartitionPool` en een `CorePool`, de kooi en de cores
//! gaan als waarde de actor in, en per slot draait één servicer-taak uit
//! een vaste pool die de outbox leegtrekt naar de console (`slot N:
//! <regel>`). Er wordt nooit per start gespawnd.
//!
//! De eerste plaatsing leest het image dat QEMU vóór de boot neerlegde
//! (`vboard::slots::staged_image`, image/qemu-run.sh) en loopt de
//! gewone weg: Claim, de bytes in de grant, Arm met de entry uit de ELF,
//! de status pollen tot de app exit meldt, en Stop. Dat twee keer: slot 1
//! op een koude core (PSCI CPU_ON), slot 2 op dezelfde core nu hij
//! geparkeerd staat (mailbox plus SEV). Markers: `HOPOS_SLOT_START`, de
//! regels van de app via de servicer, `HOPOS_SLOT_DONE` bij exit 0 en
//! `HOPOS_SLOT_STOPPED` na de bevestigde stop.
//!
//! Gemeten 29-09 op QEMU 11 (`-cpu cortex-a53 -smp 4`): de switcher, de
//! trampoline en de parkeerlus van `cpu::el2` werkten zonder één fix; van
//! CPU_ON tot READY minder dan de 20 ms van de eerste poll, en van de
//! exit-HVC tot de geparkeerde mailbox (1) binnen één poll.
//!
//! # Hop
//!
//! Draagt de staging de rol Hop (`vboard::slots::staged_role`,
//! image/qemu-run.sh), dan plaatst de kern het image één keer, in slot 1,
//! als de bevoorrechte bewoner (PORT.md beslissing 1 en 2): `Placement::hop`
//! (sharegroup `hop`, die de OS-core met de kern deelt: Hop draait in de
//! idle van de kern, `cpu::el2::OsCore`), 64 MiB, de env met de `HOPOS_*`-keuzes,
//! en het `Privilege`-token hoort bij slot 1 vanaf de boot (`main`). Daarna
//! zet de switch de uplink-poorten van Hop door (8080 agent, 9080 leader)
//! en bewaakt een taak de bewoner: elke wissel één regel, een fault of exit
//! luid, zonder stop (wie Hop herstart is een volgende stap). Appspike
//! plaatst de kern dan niet meer: dat doet Hop zelf, via de system-API.

extern crate alloc;

// De kooi-lijm per architectuur: `cage.rs` (stage-2 en de EL2-switcher)
// of `cage_riscv.rs` (PMP plus Sv39 en de M-mode-switcher), met wat ze
// delen in `glue.rs`. Beide geven de kern dezelfde traits; de lifecycle
// hieronder is architectuur-neutraal.
#[cfg_attr(not(target_arch = "riscv64"), path = "cage.rs")]
#[cfg_attr(target_arch = "riscv64", path = "cage_riscv.rs")]
mod cage;

/// De architectuur-naad van deze lijm: wat op arm64 `cpu::el2` is (de
/// toestand van een core, de kick van een slot, de OS-core), is op riscv64
/// van de kooi-lijm zelf (`cage_riscv.rs`). Op module-niveau, zodat de
/// lijm eronder geen `cfg` kent (handboek §7).
#[cfg(not(target_arch = "riscv64"))]
mod arch {
    use super::cage::FLAVOR;
    use cpu::el2;
    use cpu::println;

    pub(super) use cpu::el2::{OsCore, core_state};

    /// De kick van een slot na een schrijf in zijn RX-ring: op QEMU virt
    /// (nVHE) een SEV, die elke WFE-slaper wekt, dus het slot kiest geen
    /// doel; een board met een gerichte kick (Apple's fast IPI) zoekt hier
    /// de core van het slot op.
    pub(super) fn wake_all() {
        el2::kick(FLAVOR, 0);
    }

    /// Kan de EL2-smaak de OS-core met Hop delen? Apple niet.
    pub(super) const SHARES_OS_CORE: bool = !matches!(FLAVOR, el2::Flavor::AppleVhe);

    /// De rotatie van de OS-core, na de zelftest van de overgang (timer,
    /// yield, kick).
    pub(super) fn os_core(plan: &abi::layout::Plan) -> Result<el2::OsCore, el2::Error> {
        let board = &crate::BOARD;
        let bell = board.os_bell();
        let mut os = el2::OsCore::new(plan, FLAVOR, Some(bell))?;
        let ms = cpu::idle::freq() / 1000;
        let t = probe(&mut os, false, ms, &|| {});
        let y = probe(&mut os, true, 100 * ms, &|| {});
        let k = probe(&mut os, false, 100 * ms, &|| board.kick_self());
        let back = |r: &Tried| r.probe.map(|p| p.back);
        let ok = back(&t) == Some(el2::Back::Timer)
            && back(&y) == Some(el2::Back::Yield)
            && back(&k) == Some(el2::Back::Ipi);
        let show = |r: Tried| Shown {
            tried: r,
            ms,
            kick: bell.intid,
        };
        println!(
            "oscore: cpu {} self-test timer={} yield={} kick={} {}",
            plan.os_core(),
            show(t),
            show(y),
            show(k),
            if ok {
                "HOPOS_OS_SELFTEST ok"
            } else {
                "HOPOS_OS_SELFTEST_FAIL"
            }
        );
        Ok(os)
    }

    /// Hoe vaak één proef het opnieuw doet als een device-lijn hem
    /// onderbrak.
    const PROBE_TRIES: u32 = 3;

    /// Eén proef en hoe vaak hij het deed.
    #[derive(Copy, Clone)]
    struct Tried {
        probe: Option<el2::Probe>,
        tries: u32,
    }

    /// Eén proef van de zelftest, met eerst de interrupts die al wachten
    /// afgehandeld (`crate::drain_interrupts`), en opnieuw als een
    /// device-lijn hem onderbrak, hoogstens [`PROBE_TRIES`] keer.
    ///
    /// Waarom (30-09, de eerste Pi 5-boot: drie keer `Irq` na 0 us): de
    /// zelftest draait in de boot, vóór de executor. Een lijn die al eerder
    /// scherp stond (de NIC, INTID 166, sinds `probe_nic`) en daarna één
    /// keer vuurde, liet de vector de vlag zetten en gemaskeerd terugkeren,
    /// maar de dispatch-taak draait pas als de executor loopt. De lijn
    /// stond dus nog pending bij de GIC, en elke proef kwam op 0 us terug
    /// op een interrupt die niets met de overgang te maken had. Een lijn
    /// die tijdens de proef komt (een frame op het LAN) is net zo min een
    /// oordeel. Komt hij drie keer, dan zegt de regel welke lijn het was.
    fn probe(os: &mut el2::OsCore, yield_: bool, ticks: u64, kick: &dyn Fn()) -> Tried {
        let mut last = Tried {
            probe: None,
            tries: 0,
        };
        for n in 1..=PROBE_TRIES {
            crate::drain_interrupts();
            let probe = os.selftest(yield_, ticks, kick);
            last = Tried { probe, tries: n };
            if probe.is_none_or(|p| p.back != el2::Back::Irq) {
                break;
            }
        }
        last
    }

    /// Een proef voor de zelftest-regel: `(Timer, 1344 us)`, en bij een
    /// onderbreking de vectorindex, de INTID die de controller na de
    /// terugkeer liet zien, een lijn die al vóór de overgang pending stond,
    /// en het aantal pogingen.
    struct Shown {
        tried: Tried,
        ms: u64,
        kick: u32,
    }

    impl core::fmt::Display for Shown {
        fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
            let Some(p) = self.tried.probe else {
                return f.write_str("(none: no heap)");
            };
            let us = p.ticks.saturating_mul(1000) / self.ms.max(1);
            write!(f, "({:?}, {us} us", p.back)?;
            if !matches!(p.back, el2::Back::Timer | el2::Back::Yield | el2::Back::Ipi) {
                write!(f, ", vec {}, INTID {}", p.vec, p.after)?;
            }
            if let Some(i) = p.stale(self.kick) {
                write!(f, ", INTID {i} pending before entry")?;
            }
            if self.tried.tries > 1 {
                write!(f, ", try {}", self.tried.tries)?;
            }
            f.write_str(")")
        }
    }
}

#[cfg(target_arch = "riscv64")]
mod arch {
    pub(super) use super::cage::{OsCore, SHARES_OS_CORE, core_state, os_core, wake_all};
}

/// De koude flip op riscv64 (flip.rs): de app-harts uit het kern-image en,
/// als de sprong niet doorgaat, terug.
#[cfg(target_arch = "riscv64")]
pub(crate) use cage::{Off, is_off, park_for_flip, unpark_after_flip};

use crate::clock::ExecTimer;
use crate::glue::{DevMem, KernConsole, SlotOutbox};
use abi::hopabi::{CTRL_ENV_DATA, CTRL_ENV_LEN, CTRL_ENV_MAX};
use abi::layout::{ABI_CTRL_OFF, ABI_TAIL, CtxState, LINK_BASE, RING_DATA_CAP};
use abi::place::{self, SYM_ABI, SYM_RAM_SIZE, SYM_RAM_START, SYM_SLOT_HINT, Window};
use alloc::boxed::Box;
use alloc::string::String;
use alloc::vec::Vec;
use board::Board;
use board::stage::StagedRole;
#[cfg(not(target_arch = "riscv64"))]
use cage::{ArmCage as SlotCage, ArmCores as SlotCores};
#[cfg(target_arch = "riscv64")]
use cage::{RvCage as SlotCage, RvCores as SlotCores};
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use cpu::el2::{self, CoreState};
use cpu::println;
use executor::Executor;
use kern::partmem::{Geometry, PartitionPool};
use kern::pool::{CorePool, Placement};
use kern::slots::{
    Envelope, ImageGrant, Lifecycle, Mount, Reply, Request, Response, Servicers, SlotStatus,
    StartSpec, call, servicer_task,
};
use kern::system::{LogTee, SlotLogs};
use kern::{Region, Slot};
use sync::mpsc::Mailbox;
use vboard::slots as vslots;

/// De brievenbus van de lifecycle-actor. Hij staat in `main` naast de
/// system-listener, die er zijn verzoeken heen stuurt; de actor die hem
/// leest, woont hier.
static INBOX: &Mailbox<Envelope<'static>, { crate::LIFECYCLE_DEPTH }> = &crate::LIFECYCLE;

/// De servicer-tabel: de actor schrijft, de servicers en `admit` van de
/// system-listener lezen.
static SERVICERS: &Servicers = &crate::SERVICERS;

/// De logringen per slot voor `NEXT_LOG` van de system-API: de servicers
/// schrijven er elke app-regel in (via [`LogTee`]), de listener leest.
pub(crate) static LOGS: SlotLogs = SlotLogs::new();

/// De antwoordplek van de boot-plaatsing (één aanroeper).
static BOOT_REPLY: Reply = Reply::new();

/// De partitie van de eerste plaatsing: 32 MB, staart inbegrepen. Ruim voor
/// appspike (een half MB image, 256 KB stack, een kleine heap).
const FIRST_MEM: u64 = 32 << 20;

/// Hoe lang de boot-plaatsing op een exit wacht.
const FIRST_DEADLINE: Duration = Duration::from_secs(20);

/// Het poll-ritme van de boot-plaatsing.
const FIRST_POLL: Duration = Duration::from_millis(20);

/// Het slot van Hop: de plaatsing bij boot, en het slot dat de bevoegdheid
/// draagt (`main` slaat het token vóór de system-listener start).
pub(crate) const HOP_SLOT: usize = 1;

/// De partitie van Hop: 64 MiB, staart inbegrepen. Ruim: de release-ELF is
/// 594 KiB laadbaar (gemeten 29-09), de rest is heap voor agent, leader,
/// de HTTP-verbindingen en de download-buffer (64 KiB per hap).
const HOP_MEM: u64 = 64 << 20;

/// Het volume van Hop: `/hop` in zijn zicht, `/volumes/hop` op hopfs. Daar
/// bewaart `agentd-hopos` zijn `agent-state.json` en leest hij hem na een
/// herstart terug (`Node::restore`); zijn eigen root (`/.tasks/slot1`) is
/// bij elke start leeg, het volume niet.
const HOP_VOLUME: (&[u8], &[u8]) = (b"/hop", b"/volumes/hop");

/// De agent-poort van Hop; de leader luistert op poort + 1000 (zoals Go).
/// Beide worden op de uplink doorgezet.
const HOP_PORT: u16 = 8080;

/// Hoe lang de plaatsing van Hop op de DHCP-lease wacht voor
/// `HOPOS_NODE_IP`. QEMU's user-net antwoordt binnen een milliseconde; zonder
/// lease start Hop toch, met zijn slot-adres als endpoint (luid).
const UPLINK_WAIT: Duration = Duration::from_secs(10);

/// Het ritme van de bewaking van Hop: een regel per wissel, en om de
/// [`HOP_BEAT_EVERY`] rondes een hartslagregel.
const HOP_WATCH: Duration = Duration::from_secs(1);

/// Om de hoeveel rondes van [`HOP_WATCH`] de bewaking de hartslag toont.
const HOP_BEAT_EVERY: u64 = 30;

/// De buffer van een servicer: één maximaal record (de halve outbox).
const SERVICE_BUF: usize = (RING_DATA_CAP / 2) as usize;

/// Start de slot-machinerie op de executor van core 0: de kooi, de actor,
/// een servicer per slot, en de plaatsing van het gestagede image. Faalt
/// een stap, dan één regel met marker en draait de node door zonder slots:
/// het netwerk en de console hebben er niets mee te maken.
///
/// FLIP: met `adopt` (de bewoners uit het handoff-blob van een kern-flip)
/// neemt de kooi de zittende switch-code over in plaats van hem te
/// installeren, krijgt elke bewoner zijn kooi, ringen en servicer terug
/// zonder dat er één byte van zijn wereld verandert, en plaatst de kern
/// Hop niet opnieuw: Hop draait door (`HOPOS_FLIP_ADOPT`).
///
/// `app_env` is de env van een gestagede app (`hopos.appenv`, [`app_env`]);
/// Hop krijgt de zijne uit `hopos.cfg`.
pub(crate) fn start(
    exec: &'static Executor,
    role: Result<StagedRole, u64>,
    adopt: Option<Vec<kern::slots::SlotState>>,
    app_env: Vec<u8>,
    hop_cfg: String,
) {
    let board = &crate::BOARD;
    let plan = match os_plan() {
        Ok(p) => p,
        Err(e) => {
            println!("slots: plan refused: {e} HOPOS_SLOT_PLAN");
            return;
        }
    };
    // FLIP: een geadopteerde kern schrijft geen byte in de plan-regio: er
    // draaien cores in de switch-code (`cpu::el2::adopt` eist de som).
    let cage = match &adopt {
        Some(_) => SlotCage::adopt(plan.clone()),
        None => SlotCage::new(plan.clone()),
    };
    let mut cage = match cage {
        Ok(c) => c,
        Err(e) => {
            println!("slots: cage init: {e} HOPOS_CAGE_FAIL");
            return;
        }
    };
    if let Some(states) = &adopt {
        for st in states {
            let r = Slot::new(st.slot).map_or(Err(kern::cage::CageError { code: 0 }), |s| {
                // FLIP: de SMP-breedte mee, anders vindt een revoke de
                // secundaires van een SMP-app niet.
                cage.adopt_slot(
                    s,
                    Region::new(st.part_base, st.part_size),
                    st.core,
                    st.cores,
                )
            });
            if let Err(e) = r {
                println!(
                    "slots: slot {} not re-attached: cage code {} HOPOS_FLIP_ADOPT_FAIL",
                    st.slot, e.code
                );
            }
        }
    }
    let sw = cage.installed();
    println!(
        "slots: cage up HOPOS_CAGE_UP region={:#x} switch={:#x}+{:#x} tramp={:#x} hash={:#x} app-cores={} max-slots={}",
        plan.vec_base_pa().0,
        sw.entry.0,
        sw.len,
        sw.tramp.0,
        sw.hash,
        plan.app_cores(),
        plan.max_slots()
    );
    // De eerste meting: na de init moet elke app-core koud staan (mailbox
    // 0). Een ander woord is een core die al iets doet, of een map die de
    // regio niet raakt.
    for c in 1..=plan.app_cores() {
        if let Some(core) = abi::layout::Core::new(c) {
            println!(
                "slots: core {c} mailbox {:?}",
                arch::core_state(&plan, core)
            );
        }
    }

    let mut pool: Vec<Region> = Vec::new();
    if pool.try_reserve_exact(plan.pool().len()).is_err() {
        println!("slots: out of memory for the pool HOPOS_SLOT_PLAN");
        return;
    }
    pool.extend(plan.pool().iter().map(|r| Region::new(r.base, r.size)));
    let kern_ram = board.plan();
    let own = Region::new(
        kern_ram.kern_ram.base.0,
        kern_ram.dma.end().0 - kern_ram.kern_ram.base.0,
    );
    let geo = Geometry {
        link_window: cage::link_window,
        reserve: cage::reserve,
    };
    let parts = match PartitionPool::new(&pool, own, geo, plan.max_slots()) {
        Ok(p) => p,
        Err(e) => {
            println!("slots: partition pool: {e} HOPOS_SLOT_PLAN");
            return;
        }
    };
    println!(
        "slots: pool {} MB in {} regions, largest {} MB",
        parts.capacity() >> 20,
        pool.len(),
        parts.largest() >> 20
    );
    // Wat Hop als pool ziet: de capaciteit min wat de codec straks uit de
    // pool reserveert (hopos/src/codec.rs), anders overschat Hop de node.
    let pool_bytes = parts
        .capacity()
        .saturating_sub(crate::codec::planned_bytes());
    let cores = SlotCores::new(plan.clone());
    // FLIP: de core van een geadopteerde Hop, als hij meeging.
    let hop_core = adopt
        .as_ref()
        .and_then(|v| v.iter().find(|st| st.slot == HOP_SLOT).map(|st| st.core));
    // De actor buiten de future gebouwd (30-09): binnen `async move` stond
    // de verse `Lifecycle` als tijdelijke waarde in het frame van de
    // poll-functie, en dat frame (57 KB) reserveerde elke poll van de
    // lifecycle opnieuw, bovenop de rest van de stack. En in een `Box`
    // (02-10): als waarde in de future stond hij (54 KB) nog een keer in
    // de future van 56 KB, en die twee kopieën in het frame van `setup`
    // brachten de boot-stack met de display-smaak over zijn wachtpagina.
    let mut lc = Box::new(Lifecycle::new(
        cage,
        cores,
        ExecTimer(exec),
        KernConsole,
        parts,
        os_pool(),
        SERVICERS,
        // De device-grants (gui.rs): de framebuffer in de gui-smaak,
        // kaal niets.
        crate::gui::slot_grants(),
    ));
    let actor = async move {
        // FLIP: eerst alle eigendomsclaims terug, dan pas verzoeken.
        if let Some(states) = &adopt {
            match lc.adopt(states) {
                Ok(n) => {
                    println!(
                        "HOPOS_FLIP_ADOPT {n} of {} resident(s) adopted, ownership restored before the first request",
                        states.len()
                    );
                    crate::flip::adopted_ok();
                    // De poorten van de jobspecs: de NAT van deze kern is leeg.
                    lc.republish().await;
                }
                Err(e) => println!("slots: adoption refused: {e} HOPOS_FLIP_ADOPT_FAIL"),
            }
        }
        lc.run(INBOX).await;
    };
    if let Err(e) = exec.spawn(actor) {
        println!("slots: lifecycle not spawned: {e:?} HOPOS_SLOT_SPAWN");
        return;
    }
    for i in 1..=plan.max_slots() {
        let Some(slot) = Slot::new(i) else { continue };
        let Some(ctx) = abi::layout::Slot::new(i).and_then(|s| plan.ctx_pa(s).ok()) else {
            continue;
        };
        if let Err(e) = exec.spawn(servicer(exec, slot, ctx)) {
            println!("slots: servicer {i} not spawned: {e:?} HOPOS_SLOT_SPAWN");
        }
    }
    // FLIP: Hop leeft nog; alleen zijn poorten en de bewaking komen terug.
    if let Some(core) = hop_core {
        if let Err(e) = exec.spawn(resume_hop(exec, plan, core)) {
            println!("slots: Hop watch not spawned: {e:?} HOPOS_SLOT_SPAWN");
        }
        return;
    }
    let spawned = match role {
        Ok(StagedRole::App) => exec.spawn(place_first(exec, plan, app_env)),
        Ok(StagedRole::Hop) => exec.spawn(place_hop(exec, plan, pool_bytes, hop_cfg)),
        Err(word) => {
            println!(
                "slots: staged role word {word:#x} is neither app (0) nor hop (1), nothing placed HOPOS_SLOT_NONE"
            );
            return;
        }
    };
    if let Err(e) = spawned {
        println!("slots: first placement not spawned: {e:?} HOPOS_SLOT_SPAWN");
    }
}

/// De slots waarvan de bewoner de OS-core deelt, één bit per slot: de
/// switch wacht niet op hun RX-ring ([`resident`]). Eén schrijver (de
/// plaatsing van Hop), gelezen door de switch op dezelfde core.
static OS_RESIDENTS: AtomicU64 = AtomicU64::new(0);

/// Deelt de bewoner van `slot` de OS-core (Hop in slot 1)? De
/// `resident` van de switch: zijn consument draait pas als de executor
/// afgeeft, dus wachten op ruimte in zijn ring is stilstand (30-09).
pub(crate) fn resident(slot: usize) -> bool {
    slot < 64 && OS_RESIDENTS.load(Relaxed) & (1 << slot) != 0
}

/// Zet of wist `slot` als bewoner van de OS-core.
fn set_resident(slot: usize, on: bool) {
    if slot < 64 {
        if on {
            OS_RESIDENTS.fetch_or(1 << slot, Relaxed);
        } else {
            OS_RESIDENTS.fetch_and(!(1 << slot), Relaxed);
        }
    }
}

/// De kick van slot `slot` na een schrijf in zijn RX-ring (de `slot_wake`
/// van de switch); zie `arch::wake_all`.
pub(crate) fn wake(_slot: usize) {
    arch::wake_all();
}

/// De servicer-taak van één slot, met zijn eigen recordbuffer.
async fn servicer(exec: &'static Executor, slot: Slot, ctx: dev::Pa) {
    let mut buf: Vec<u8> = Vec::new();
    if buf.try_reserve_exact(SERVICE_BUF).is_err() {
        println!("slot {slot}: no servicer buffer HOPOS_SLOT_SPAWN");
        return;
    }
    buf.resize(SERVICE_BUF, 0);
    let timer = ExecTimer(exec);
    servicer_task(
        slot,
        SERVICERS,
        |part| SlotOutbox::open(part, ctx),
        &timer,
        &LogTee::new(KernConsole, &LOGS),
        INBOX,
        &mut buf,
    )
    .await;
}

/// Waarom de eerste plaatsing niet doorging; één regel met de getallen.
#[derive(Debug)]
enum PlaceError {
    /// De ELF-lezer weigerde.
    Elf(leanelf::Error),
    /// De plaatsingstoets weigerde.
    Place(abi::Error),
    /// De actor weigerde.
    Kern(kern::Error),
    /// De actor gaf een antwoord dat niet bij de vraag hoort.
    Reply,
}

impl core::fmt::Display for PlaceError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::Elf(e) => write!(f, "elf: {e}"),
            Self::Place(e) => write!(f, "place: {e}"),
            Self::Kern(e) => write!(f, "kern: {e}"),
            Self::Reply => write!(f, "unexpected reply from the lifecycle"),
        }
    }
}

/// Stuurt één verzoek naar de actor.
async fn ask(req: Request) -> Result<Response, PlaceError> {
    match call(INBOX, &BOOT_REPLY, req).await {
        Ok(Response::Failed(e)) | Err(e) => Err(PlaceError::Kern(e)),
        Ok(r) => Ok(r),
    }
}

async fn status(slot: Slot) -> Option<SlotStatus> {
    match ask(Request::Status(slot)).await {
        Ok(Response::Status(s)) => Some(s),
        _ => None,
    }
}

/// De eerste plaatsing: het gestagede image in slot 1, wachten op zijn
/// exit en stoppen; daarna hetzelfde image in slot 2. De plaatsing kiest de
/// eerste vrije core, en dat is core 1, die nu in de parkeerlus staat: de
/// tweede ronde bewijst het warme pad (mailbox plus SEV) waar de eerste het
/// koude pad (PSCI CPU_ON) bewees, met een andere kooi, VMID en partitie.
async fn place_first(exec: &'static Executor, plan: abi::layout::Plan, env: Vec<u8>) {
    let Some(img) = vslots::staged_image() else {
        println!("slots: no staged image, nothing placed HOPOS_SLOT_NONE");
        return;
    };
    for i in 1..=plan.max_slots().min(2) {
        let Some(slot) = Slot::new(i) else { return };
        if !run_once(exec, &plan, slot, img, &env).await {
            return;
        }
    }
}

/// De env van een gestagede app uit de bootparameter `hopos.appenv`:
/// `KEY=val`, meer sleutels met komma's (`GUI=display,MODE=x`), want een
/// bootarg kent geen spaties of regels. Zo krijgt appspike op QEMU de env
/// die een jobspec hem zou geven, bijvoorbeeld het glas
/// (tools/qemu-test.sh met `GUI=display`). Te lang voor de control-page
/// is één regel en een lege env: de app draait dan zoals zonder.
pub(crate) fn app_env(param: &str) -> Vec<u8> {
    let mut env = Vec::new();
    if param.is_empty() {
        return env;
    }
    if param.len() >= CTRL_ENV_MAX as usize || env.try_reserve_exact(param.len() + 1).is_err() {
        println!(
            "slots: hopos.appenv of {} bytes refused, the control page holds {CTRL_ENV_MAX} HOPOS_SLOT_ENV",
            param.len()
        );
        return env;
    }
    env.extend(param.bytes().map(|b| if b == b',' { b'\n' } else { b }));
    env.push(b'\n');
    env
}

/// De naam van deze node zonder NIC als `hopos.cfg` geen `hopos.node` zet
/// (Go: `cfg.Node.ID = "hopos-1"`); QEMU zet de zijne in
/// `kern::nodecfg::QEMU_CFG`.
const HOP_NODE: &str = "hopos-1";

/// De naam van deze node in Hop's cluster als de config geen `hopos.node`
/// zet: het board en de staart van de uplink-MAC
/// (`kern::nodecfg::default_node`), zodat elke node op de gedeelde config
/// (image/cfg) een eigen naam heeft. Eén regel op de console als hij
/// gebruikt wordt.
fn default_node(cfg: &kern::nodecfg::NodeCfg<'_>) -> String {
    let Some(mac) = crate::net::uplink_mac() else {
        return HOP_NODE.into();
    };
    let name = kern::nodecfg::default_node(<crate::Machine as Board>::NAME, mac);
    if cfg.one("hopos.node").is_empty() {
        println!(
            "slots: no hopos.node, this node is {name} (board and uplink MAC) HOPOS_NODE_DEFAULT"
        );
    }
    name
}

/// De plaatsing van Hop: het gestagede image één keer in slot 1, met de
/// env van de node, dan de poorten op de uplink en de bewaking.
async fn place_hop(
    exec: &'static Executor,
    plan: abi::layout::Plan,
    pool_bytes: u64,
    hop_cfg: String,
) {
    let Some(img) = vslots::staged_image() else {
        println!("slots: role hop but no staged image, Hop not started HOPOS_HOP_FAIL");
        return;
    };
    let Some(slot) = Slot::new(HOP_SLOT) else {
        return;
    };
    let node_ip = wait_uplink(exec).await;
    // De env van Hop komt uit de config van het board (kern::nodecfg).
    let cfg = kern::nodecfg::NodeCfg::parse(&hop_cfg);
    let node = default_node(&cfg);
    let facts = kern::nodecfg::Facts {
        default_node: &node,
        node_ip,
        dns: crate::net::uplink_dns(),
        port: HOP_PORT,
        app_cores: plan.app_cores(),
        hop_on_os: arch::SHARES_OS_CORE,
        pool_bytes,
        hop_mem: HOP_MEM,
    };
    let env = match crate::config::hop_env(&cfg, &facts) {
        Ok(e) => e,
        Err(e) => {
            println!("slots: Hop env: {e}, Hop not started HOPOS_HOP_FAIL");
            return;
        }
    };
    let at = match hop_placement(&plan) {
        Ok(a) => a,
        Err(e) => {
            println!("slots: Hop placement: {e}, Hop not started HOPOS_HOP_FAIL");
            return;
        }
    };
    let volume = match kern_mounts(&[HOP_VOLUME]) {
        Ok(m) => m,
        Err(e) => {
            println!("slots: Hop volume: {e}, Hop not started HOPOS_HOP_FAIL");
            return;
        }
    };
    // Vóór de plaatsing: zodra de poort aan de switch hangt, mag hij niet
    // meer op Hop's ring wachten. Na de plaatsing volgt de echte core.
    set_resident(slot.get(), arch::SHARES_OS_CORE);
    let entry = match place(slot, img, HOP_MEM, at, env.as_bytes(), volume).await {
        Ok(e) => e,
        Err(e) => {
            println!("slot {slot}: Hop not started: {e} HOPOS_HOP_FAIL");
            return;
        }
    };
    let st = status(slot).await;
    let core = st.and_then(|s| s.core).map_or(0, |(c, _)| c.get());
    set_resident(slot.get(), core == 0);
    let part = st.and_then(|s| s.partition).unwrap_or_default();
    let cpu = abi::layout::Core::new(core).map_or(core, |c| plan.phys_core(c));
    println!(
        "HOPOS_HOP_START slot={slot} core={core} cpu={cpu} entry={entry:#x} part={:#x}+{:#x} image={} env={}",
        part.base,
        part.size,
        img.len(),
        env.len()
    );
    for port in [HOP_PORT, HOP_PORT.saturating_add(1000)] {
        match crate::net::publish(slot.get(), port).await {
            Ok(()) => println!("net: uplink tcp :{port} -> slot {slot} :{port} HOPOS_HOP_PUBLISH"),
            Err(e) => println!(
                "net: uplink tcp :{port} not published to slot {slot}: {e} HOPOS_HOP_PUBLISH_FAIL"
            ),
        }
    }
    watch_hop(exec, &plan, slot, core).await;
}

/// FLIP: Hop kwam mee over een kern-flip. Zijn kooi, ringen en servicer
/// zijn al terug (`start`); de uplink-poorten zijn van de switch van déze
/// kern en gaan opnieuw open, en de bewaking loopt verder.
async fn resume_hop(exec: &'static Executor, plan: abi::layout::Plan, core: usize) {
    let Some(slot) = Slot::new(HOP_SLOT) else {
        return;
    };
    set_resident(slot.get(), core == 0);
    let _ = wait_uplink(exec).await;
    for port in [HOP_PORT, HOP_PORT.saturating_add(1000)] {
        match crate::net::publish(slot.get(), port).await {
            Ok(()) => println!("net: uplink tcp :{port} -> slot {slot} :{port} HOPOS_HOP_PUBLISH"),
            Err(e) => println!(
                "net: uplink tcp :{port} not published to slot {slot}: {e} HOPOS_HOP_PUBLISH_FAIL"
            ),
        }
    }
    println!(
        "slot {slot}: Hop carried over the flip on core {core}, not restarted HOPOS_HOP_RESUMED"
    );
    watch_hop(exec, &plan, slot, core).await;
}

/// Wacht tot de lease er is, hooguit [`UPLINK_WAIT`]; `None` na de termijn.
async fn wait_uplink(exec: &'static Executor) -> Option<core::net::Ipv4Addr> {
    let deadline = exec.now().saturating_add(UPLINK_WAIT.as_nanos() as u64);
    loop {
        if let Some(ip) = crate::net::uplink_ip() {
            return Some(ip);
        }
        if exec.now() >= deadline {
            println!(
                "slots: no uplink lease after {} s, Hop announces its slot address HOPOS_HOP_NO_UPLINK",
                UPLINK_WAIT.as_secs()
            );
            return None;
        }
        exec.after(Duration::from_millis(50)).await;
    }
}

/// `Placement::hop`: sharegroup `hop`, die de OS-core met de kern deelt
/// ([`os_pool`]). De klasse van die core koos de bootparameter
/// (`hopos.oscore`), niet deze plaatsing.
fn hop_placement(plan: &abi::layout::Plan) -> kern::Result<Placement> {
    let at = Placement::hop()?;
    println!(
        "slots: Hop shares the OS core (cpu {}) with the kern, {} app core(s) stay free HOPOS_HOP_OS_CORE",
        plan.os_core(),
        plan.app_cores()
    );
    Ok(at)
}

/// Het plan van deze node: de kern-core is de OS-core, en dat is de core
/// waar dit draait (na de verhuizing bij boot, `main`). Het board-contract
/// hiervoor (`plan(cores, os_core)`, `this_core`, `os_bell`, `kick_self`)
/// volgen alle boards die deze lijm delen.
pub(crate) fn os_plan() -> abi::Result<abi::layout::Plan> {
    let board = &crate::BOARD;
    vslots::plan(board.cores(), board.this_core())
}

/// De core-plaatsing van deze node: Hop's groep deelt de OS-core met de
/// kern (PORT.md beslissing 2). Kan de architectuur dat niet (Apple's
/// EL2-smaak), dan krijgt Hop een app-core zoals vóór 30-09.
fn os_pool() -> CorePool {
    let mut pool = CorePool::new();
    if arch::SHARES_OS_CORE
        && let Err(e) = pool.share_os_core(kern::pool::HOP_GROUP)
    {
        println!("slots: Hop may not share the OS core: {e} HOPOS_OS_CORE_FAIL");
    }
    pool
}

/// De rotatie van de OS-core voor de slaap van de executor: de bewoners van
/// sched-blok 0 en de kick van de app-cores, na de zelftest van de
/// overgang (timer, yield, kick). Aanroepen op de OS-core, ná [`start`]
/// (die de kooi-regio opzette) en ná de interrupts (de zelftest wacht op
/// de CNTHP en de kick-SGI).
pub(crate) fn os_core() -> Result<arch::OsCore, el2::Error> {
    let plan = os_plan().map_err(el2::Error::Plan)?;
    arch::os_core(&plan)
}

/// Het rapport van een exception die de app op EL1 zelf ving (de
/// vectortabel van applib, `CTRL_APP_FAULT_*`), als stuk van een
/// fault-regel; `None` als er geen is. Sinds 30-09: daarvoor sprong zo'n
/// fault naar een lege VBAR_EL1 en zag de kern alleen de instructie-abort
/// op `VBAR + 0x200` (de eerste Pi 5-boot, `esr=0x82000005 far=0x200`).
fn el1_fault(st: &kern::cage::Status) -> Option<alloc::string::String> {
    use core::fmt::Write;
    if st.app_fault_vec == 0 || st.exit_code != abi::hopabi::EXIT_APP_FAULT {
        return None;
    }
    let mut line = alloc::string::String::new();
    let _ = write!(
        line,
        "vec={} esr={:#x} ({}) elr={:#x} far={:#x}",
        st.app_fault_vec - 1,
        st.app_fault_esr,
        kern::cage::esr_class(st.app_fault_esr),
        st.app_fault_elr,
        st.app_fault_far
    );
    Some(line)
}

/// Bewaakt Hop: elke wissel van app-status, core of fault één regel, om de
/// [`HOP_BEAT_EVERY`] seconden de hartslag. Een fault of exit is luid en
/// het einde van de bewaking; een herstart van Hop is een volgende stap
/// (de kern stopt hem niet, zodat zijn staat voor diagnose blijft staan).
async fn watch_hop(exec: &'static Executor, plan: &abi::layout::Plan, slot: Slot, core: usize) {
    let t0 = exec.now();
    let mut last: Option<(u64, u64, bool)> = None;
    let mut round: u64 = 0;
    loop {
        let Some(st) = status(slot).await else {
            println!("slot {slot}: Hop status unavailable HOPOS_HOP_FAIL");
            return;
        };
        let seen = (st.cage.app, st.cage.fault_vec, st.cage.core_on);
        round = round.wrapping_add(1);
        if last != Some(seen) || round.is_multiple_of(HOP_BEAT_EVERY) {
            probe(plan, slot, core, &st, exec.now().saturating_sub(t0));
            last = Some(seen);
        }
        if st.cage.fault_vec != 0 {
            println!(
                "slot {slot}: Hop faulted vec={} esr={:#x} ({}) far={:#x} HOPOS_HOP_FAULT",
                st.cage.fault_vec - 1,
                st.cage.fault_esr,
                kern::cage::esr_class(st.cage.fault_esr),
                st.cage.fault_far
            );
            return;
        }
        if let Some(line) = el1_fault(&st.cage) {
            println!("slot {slot}: Hop faulted at EL1 {line} HOPOS_HOP_FAULT");
            return;
        }
        if st.cage.app == abi::hopabi::AppStatus::Exited as u64 {
            println!(
                "slot {slot}: Hop exited with {} HOPOS_HOP_EXIT",
                st.cage.exit_code
            );
            return;
        }
        exec.after(HOP_WATCH).await;
    }
}

/// Eén levensduur van `img` in `slot`: plaatsen, volgen tot de exit, en
/// stoppen. Geeft `true` als de stop bevestigd is (core en partitie vrij).
async fn run_once(
    exec: &'static Executor,
    plan: &abi::layout::Plan,
    slot: Slot,
    img: &[u8],
    env: &[u8],
) -> bool {
    let at = Placement {
        cores: 1,
        ..Placement::default()
    };
    let entry = match place(slot, img, FIRST_MEM, at, env, Vec::new()).await {
        Ok(e) => e,
        Err(e) => {
            println!("slot {slot}: not started: {e} HOPOS_SLOT_FAIL");
            return false;
        }
    };
    let st = status(slot).await;
    let core = st.and_then(|s| s.core).map_or(0, |(c, _)| c.get());
    // De startregel (`HOPOS_SLOT_START`) zette de kooi-lijm al bij de
    // dispatch; hier alleen wat alleen de boot-plaatsing weet.
    println!(
        "slot {slot}: placed by the kern, image {} bytes, entry {entry:#x}",
        img.len()
    );
    watch_first(exec, plan, slot, core).await
}

/// De volumes als `kern::slots::Mount`, faalbaar gealloceerd.
fn kern_mounts(list: &[(&[u8], &[u8])]) -> kern::Result<Vec<Mount>> {
    let copy = |b: &[u8]| -> kern::Result<Vec<u8>> {
        let mut v = Vec::new();
        v.try_reserve_exact(b.len())
            .map_err(|_| kern::Error::OutOfMemory { bytes: b.len() })?;
        v.extend_from_slice(b);
        Ok(v)
    };
    let mut out = Vec::new();
    out.try_reserve_exact(list.len())
        .map_err(|_| kern::Error::OutOfMemory {
            bytes: list.len() * core::mem::size_of::<Mount>(),
        })?;
    for (local, shared) in list {
        out.push(Mount {
            local: copy(local)?,
            shared: copy(shared)?,
        });
    }
    Ok(out)
}

/// Plaatst `img` in `slot` met `mem` bytes, de core-vraag `at` en de
/// volumes `mounts`: Claim, de segmenten, de patches, de env langs de
/// grant-aanbieder van de actor en dan in de grant, Arm. Geeft de entry.
async fn place(
    slot: Slot,
    img: &[u8],
    mem: u64,
    at: Placement,
    env: &[u8],
    mounts: Vec<Mount>,
) -> Result<u64, PlaceError> {
    let mut spec = StartSpec::new(slot, mem, at);
    spec.mounts = mounts;
    let grant = match ask(Request::Claim(spec)).await? {
        Response::Granted(g) => g,
        _ => return Err(PlaceError::Reply),
    };
    match fill(&grant, img) {
        Ok(placement) => {
            let mut grant = grant;
            let mut mem = DevMem;
            let mut written = write_image(&mut grant, &mut mem, img, &placement);
            if written.is_ok() {
                // De grant-haak, zoals een start van Hop (kern/src/system.rs):
                // de actor laat de aanbieder de env aanvullen.
                written = match grant_env(slot, env).await {
                    Ok(env) => write_env(&mut grant, &mut mem, &env),
                    Err(e) => Err(e),
                };
            }
            if let Err(e) = written {
                let _ = ask(Request::Abort(grant)).await;
                return Err(e);
            }
            let entry = placement.entry;
            ask(Request::Arm { grant, entry }).await?;
            Ok(entry)
        }
        Err(e) => {
            let _ = ask(Request::Abort(grant)).await;
            Err(e)
        }
    }
}

/// De env van een start langs de grant-aanbieder van de actor
/// (`Request::Env`); het antwoord is de complete blob.
async fn grant_env(slot: Slot, env: &[u8]) -> Result<Vec<u8>, PlaceError> {
    let mut own = Vec::new();
    own.try_reserve_exact(env.len())
        .map_err(|_| PlaceError::Kern(kern::Error::OutOfMemory { bytes: env.len() }))?;
    own.extend_from_slice(env);
    match ask(Request::Env { slot, env: own }).await? {
        Response::Env(e) => Ok(e),
        _ => Err(PlaceError::Reply),
    }
}

/// Leest de ELF en bouwt het plaatsingsplan tegen de partitie van de grant
/// (`abi::place`: één bron van waarheid voor kern en apploader).
fn fill(grant: &ImageGrant, img: &[u8]) -> Result<place::Placement, PlaceError> {
    let f = leanelf::File::parse(img).map_err(PlaceError::Elf)?;
    let mut segs = [place::Segment::default(); place::MAX_SEGMENTS];
    let mut n = 0;
    for s in f.segments().filter(|s| s.kind == leanelf::PT_LOAD) {
        let Some(d) = segs.get_mut(n) else {
            return Err(PlaceError::Place(abi::Error::TooMany {
                what: "PT_LOAD segments",
                cap: place::MAX_SEGMENTS,
            }));
        };
        *d = place::Segment {
            paddr: s.paddr,
            off: s.off,
            filesz: s.filesz,
            memsz: s.memsz,
        };
        n += 1;
    }
    let [start, size, hint, stamp] = f
        .lookup([SYM_RAM_START, SYM_RAM_SIZE, SYM_SLOT_HINT, SYM_ABI])
        .map_err(PlaceError::Elf)?;
    // Het stempel is inhoud, geen adres: de waarde op het symbool.
    let abi = match stamp {
        Some(s) => {
            let mut w = [0u8; 8];
            f.read_at_paddr(&mut w, s.value).map_err(PlaceError::Elf)?;
            Some(u64::from_le_bytes(w))
        }
        None => None,
    };
    let app_ram = grant.region().size.saturating_sub(ABI_TAIL);
    let image = place::Image {
        size: img.len() as u64,
        entry: f.entry,
        segments: segs.get(..n).unwrap_or(&[]),
        symbols: place::Symbols {
            ram_start: start.map(|s| s.value),
            ram_size: size.map(|s| s.value),
            slot_hint: hint.map(|s| s.value),
            abi,
        },
    };
    let slot = abi::layout::Slot::new(grant.slot().get()).ok_or(PlaceError::Reply)?;
    place::build(
        &image,
        &Window::canonical(app_ram, 0, app_ram),
        slot,
        Some(abi::ABI_VERSION),
    )
    .map_err(PlaceError::Place)
}

/// Schrijft de segmenten en de patches door de grant. De BSS-staart hoeft
/// niet: de claim wiste de partitie al (E3).
fn write_image(
    grant: &mut ImageGrant,
    mem: &mut DevMem,
    img: &[u8],
    p: &place::Placement,
) -> Result<(), PlaceError> {
    for s in p.segments.iter() {
        let bytes = usize::try_from(s.off)
            .ok()
            .zip(usize::try_from(s.filesz).ok())
            .and_then(|(o, n)| img.get(o..o.checked_add(n)?))
            .ok_or(PlaceError::Place(abi::Error::Segment {
                paddr: s.paddr,
                memsz: s.memsz,
                filesz: s.filesz,
                off: s.off,
            }))?;
        grant
            .write(mem, s.paddr - LINK_BASE, bytes)
            .map_err(PlaceError::Kern)?;
    }
    for pt in p.patches.iter() {
        grant
            .write(mem, pt.addr - LINK_BASE, &pt.val.to_le_bytes())
            .map_err(PlaceError::Kern)?;
    }
    Ok(())
}

/// Legt de env-blob (`key=val\n`) op de control-page, zoals `put_env` van
/// de system-API: de app leest hem bij zijn start (`applib::App::env`).
/// Een lege env schrijft niets; de claim wiste de page al (E3).
fn write_env(grant: &mut ImageGrant, mem: &mut DevMem, env: &[u8]) -> Result<(), PlaceError> {
    if env.is_empty() {
        return Ok(());
    }
    if env.len() as u64 > CTRL_ENV_MAX {
        return Err(PlaceError::Kern(kern::Error::TooLarge {
            len: env.len(),
            max: CTRL_ENV_MAX as usize,
        }));
    }
    let ctrl = grant.region().size.saturating_sub(ABI_TAIL) + ABI_CTRL_OFF;
    grant
        .write(mem, ctrl + CTRL_ENV_DATA, env)
        .and_then(|()| grant.write(mem, ctrl + CTRL_ENV_LEN, &(env.len() as u64).to_le_bytes()))
        .map_err(PlaceError::Kern)
}

/// Volgt een bewoner tot zijn exit: elke statuswissel één regel met de
/// ctx-staat en de mailbox erbij (de meting als het ergens blijft hangen),
/// dan `HOPOS_SLOT_DONE` en de stop die partitie en core teruggeeft. Geeft
/// `true` als die stop bevestigd is.
async fn watch_first(
    exec: &'static Executor,
    plan: &abi::layout::Plan,
    slot: Slot,
    core: usize,
) -> bool {
    let t0 = exec.now();
    let deadline = t0.saturating_add(FIRST_DEADLINE.as_nanos() as u64);
    let mut last: Option<(u64, u64)> = None;
    let mut next_probe = t0;
    loop {
        let Some(st) = status(slot).await else {
            println!("slot {slot}: status unavailable HOPOS_SLOT_FAIL");
            return false;
        };
        let now = exec.now();
        let seen = (st.cage.app, st.cage.fault_vec);
        if last != Some(seen) || now >= next_probe {
            probe(plan, slot, core, &st, now.saturating_sub(t0));
            last = Some(seen);
            next_probe = now.saturating_add(1_000_000_000);
        }
        if st.cage.fault_vec != 0 {
            println!(
                "slot {slot}: fault vec={} esr={:#x} ({}) far={:#x} HOPOS_SLOT_FAULT",
                st.cage.fault_vec - 1,
                st.cage.fault_esr,
                kern::cage::esr_class(st.cage.fault_esr),
                st.cage.fault_far
            );
            // De switcher meldde hem dood en parkeerde de core: de stop
            // geeft partitie en core terug, maar een tweede ronde niet.
            stop(slot).await;
            return false;
        }
        if let Some(line) = el1_fault(&st.cage) {
            println!("slot {slot}: fault at EL1 {line} HOPOS_SLOT_FAULT");
            stop(slot).await;
            return false;
        }
        if st.cage.app == abi::hopabi::AppStatus::Exited as u64 {
            // De servicer trekt de laatste regels nog leeg: even laten.
            exec.after(Duration::from_millis(100)).await;
            if st.cage.exit_code == 0 {
                println!("HOPOS_SLOT_DONE slot={slot} exit=0");
            } else {
                println!(
                    "slot {slot}: exited with {} HOPOS_SLOT_EXIT_FAIL",
                    st.cage.exit_code
                );
            }
            return stop(slot).await;
        }
        if now >= deadline {
            println!(
                "slot {slot}: no exit after {} s HOPOS_SLOT_TIMEOUT",
                FIRST_DEADLINE.as_secs()
            );
            return false;
        }
        exec.after(FIRST_POLL).await;
    }
}

/// Eén meetregel: app-status, ctx-staat, mailbox en heartbeat.
fn probe(plan: &abi::layout::Plan, slot: Slot, core: usize, st: &SlotStatus, dt: u64) {
    let ctx = abi::layout::Slot::new(slot.get())
        .and_then(|s| plan.ctx_pa(s).ok())
        .and_then(el2::ctx_state);
    let mbox = abi::layout::Core::new(core).and_then(|c| arch::core_state(plan, c).ok());
    println!(
        "slot {slot}: +{} ms app={} ctx={:?} mbox={:?} beat={} core_on={}",
        dt / 1_000_000,
        st.cage.app,
        ctx.map(CtxState::raw),
        mbox.map(|m| match m {
            CoreState::Cold => 0,
            CoreState::Parked => 1,
            CoreState::Running(x) => x,
        }),
        st.cage.heartbeat,
        st.cage.core_on
    );
}

/// De stop na een exit: bevestigd stil, dus partitie en core terug. Geeft
/// `true` als de actor de stop bevestigde.
async fn stop(slot: Slot) -> bool {
    match ask(Request::Stop {
        slot,
        timeout: Duration::from_secs(1),
    })
    .await
    {
        Ok(_) => {
            println!("slot {slot}: stopped, partition and core released HOPOS_SLOT_STOPPED");
            true
        }
        Err(e) => {
            println!("slot {slot}: stop: {e} HOPOS_SLOT_STOP_FAIL");
            false
        }
    }
}
