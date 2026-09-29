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
//! (`board_qemuvirt::slots::staged_image`, image/qemu-run.sh) en loopt de
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

extern crate alloc;

#[path = "cage.rs"]
mod cage;

use abi::layout::{ABI_TAIL, CtxState, LINK_BASE, RING_DATA_CAP};
use abi::place::{self, SYM_ABI, SYM_RAM_SIZE, SYM_RAM_START, SYM_SLOT_HINT, Window};
use alloc::vec::Vec;
use board::Board;
use board_qemuvirt::slots as vboard;
use cage::{ArmCage, ArmCores, DevMem, ExecTimer, KernConsole, SlotOutbox};
use core::time::Duration;
use cpu::el2::{self, CoreState};
use cpu::println;
use executor::Executor;
use kern::partmem::{Geometry, PartitionPool};
use kern::pool::{CorePool, Placement};
use kern::slots::{
    Envelope, ImageGrant, Lifecycle, Reply, Request, Response, Servicers, SlotStatus, StartSpec,
    call, servicer_task,
};
use kern::system::{LogTee, SlotLogs};
use kern::{Region, Slot};
use sync::mpsc::Mailbox;

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

/// De buffer van een servicer: één maximaal record (de halve outbox).
const SERVICE_BUF: usize = (RING_DATA_CAP / 2) as usize;

/// Start de slot-machinerie op de executor van core 0: de kooi, de actor,
/// een servicer per slot, en de plaatsing van het gestagede image. Faalt
/// een stap, dan één regel met marker en draait de node door zonder slots:
/// het netwerk en de console hebben er niets mee te maken.
pub(crate) fn start(exec: &'static Executor) {
    let board = &crate::BOARD;
    let plan = match vboard::plan(board.cores()) {
        Ok(p) => p,
        Err(e) => {
            println!("slots: plan refused: {e} HOPOS_SLOT_PLAN");
            return;
        }
    };
    let cage = match ArmCage::new(plan.clone()) {
        Ok(c) => c,
        Err(e) => {
            println!("slots: cage init: {e} HOPOS_CAGE_FAIL");
            return;
        }
    };
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
            println!("slots: core {c} mailbox {:?}", el2::core_state(&plan, core));
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
    let parts = match PartitionPool::new(&pool, Region::default(), own, geo, plan.max_slots()) {
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
    let cores = ArmCores::new(plan.clone());
    let actor = async move {
        let mut lc = Lifecycle::new(
            cage,
            cores,
            ExecTimer(exec),
            KernConsole,
            parts,
            CorePool::new(0),
            SERVICERS,
        );
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
    if let Err(e) = exec.spawn(place_first(exec, plan)) {
        println!("slots: first placement not spawned: {e:?} HOPOS_SLOT_SPAWN");
    }
}

/// De kick van slot `slot` na een schrijf in zijn RX-ring (de `slot_wake`
/// van de switch). Op QEMU virt (nVHE) is de kick een SEV, die elke
/// WFE-slaper wekt, dus het slot kiest geen doel; een board met een
/// gerichte kick (Apple's fast IPI) zoekt hier de core van het slot op.
pub(crate) fn wake(_slot: usize) {
    el2::kick(cage::FLAVOR, 0);
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
async fn place_first(exec: &'static Executor, plan: abi::layout::Plan) {
    let Some(img) = vboard::staged_image() else {
        println!("slots: no staged image, nothing placed HOPOS_SLOT_NONE");
        return;
    };
    for i in 1..=plan.max_slots().min(2) {
        let Some(slot) = Slot::new(i) else { return };
        if !run_once(exec, &plan, slot, img).await {
            return;
        }
    }
}

/// Eén levensduur van `img` in `slot`: plaatsen, volgen tot de exit, en
/// stoppen. Geeft `true` als de stop bevestigd is (core en partitie vrij).
async fn run_once(
    exec: &'static Executor,
    plan: &abi::layout::Plan,
    slot: Slot,
    img: &[u8],
) -> bool {
    let entry = match place(slot, img).await {
        Ok(e) => e,
        Err(e) => {
            println!("slot {slot}: not started: {e} HOPOS_SLOT_FAIL");
            return false;
        }
    };
    let st = status(slot).await;
    let core = st.and_then(|s| s.core).map_or(0, |(c, _)| c.get());
    let part = st.and_then(|s| s.partition).unwrap_or_default();
    println!(
        "HOPOS_SLOT_START slot={slot} core={core} entry={entry:#x} part={:#x}+{:#x} image={}",
        part.base,
        part.size,
        img.len()
    );
    watch_first(exec, plan, slot, core).await
}

/// Plaatst `img` in `slot`: Claim, de segmenten en de patches in de grant,
/// Arm. Geeft de entry.
async fn place(slot: Slot, img: &[u8]) -> Result<u64, PlaceError> {
    let spec = StartSpec::new(
        slot,
        FIRST_MEM,
        Placement {
            cores: 1,
            ..Placement::default()
        },
    );
    let grant = match ask(Request::Claim(spec)).await? {
        Response::Granted(g) => g,
        _ => return Err(PlaceError::Reply),
    };
    match fill(&grant, img) {
        Ok(placement) => {
            let mut grant = grant;
            let mut mem = DevMem;
            let written = write_image(&mut grant, &mut mem, img, &placement);
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
                "slot {slot}: fault vec={} esr={:#x} far={:#x} HOPOS_SLOT_FAULT",
                st.cage.fault_vec - 1,
                st.cage.fault_esr,
                st.cage.fault_far
            );
            // De switcher meldde hem dood en parkeerde de core: de stop
            // geeft partitie en core terug, maar een tweede ronde niet.
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
    let mbox = abi::layout::Core::new(core).and_then(|c| el2::core_state(plan, c).ok());
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
