//! De kern-binary van HopOS v3: kiest precies één board, zet de console op,
//! toont de bunny en draait de executor.
//!
//! De bootvolgorde is die van de Go-main (`OLD/metal/cmd/hopos/main.go`)
//! voor zover de lagen eronder er zijn: console, bunny, `runtime`-regel,
//! het privilege-niveau, de firmware-regel, en dan het werk als taken op de
//! executor. Een taak per lus: de tik, de IRQ-dispatch, de opslag
//! (storage.rs: de schijf, hopfs, de hopfs-actor en de committer) en het
//! netwerkvlak (net.rs: pomp, switch, poort 0, DHCP, de system-listener).

#![no_std]
#![no_main]
#![allow(clippy::expect_used)] // boot-code: vóór de agent draait is falen parkeren (handboek §12)

mod clock;
mod config;
mod flip; // FLIP: de kern-flip (flip.rs)
mod net;
mod slots;
mod storage;

extern crate alloc;

use alloc::boxed::Box;
use board::Board;
use board::heap::Heap;
use core::panic::PanicInfo;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use cpu::println;
use executor::Executor;
use kern::cage::{Console, PhysMem};
use kern::slots::{Envelope, Reply, Servicers};
use kern::system::{Hooks, LogTee, Privilege, System};
use netdev::Device;
use sync::mpsc::Mailbox;
use sync::{Local, Signal};
use vboard::slots::{StagedRole, staged_role};

#[cfg(not(any(
    feature = "board-qemuvirt",
    feature = "board-rpi4",
    feature = "board-rpi5",
    feature = "board-rk3566",
    feature = "board-uefi",
    feature = "board-o6n",
    feature = "board-altra"
)))]
compile_error!(
    "kies precies één board: --features board-qemuvirt, board-rpi4, board-rpi5, board-rk3566, board-uefi, board-o6n of board-altra"
);

// De O6N en de Altra bouwen op het UEFI-board; naast elkaar of naast een
// ander board kan niet.
#[cfg(any(
    all(feature = "board-o6n", feature = "board-altra"),
    all(
        any(feature = "board-o6n", feature = "board-altra"),
        any(
            feature = "board-qemuvirt",
            feature = "board-rpi4",
            feature = "board-rpi5",
            feature = "board-rk3566",
            feature = "board-uefi"
        )
    )
))]
compile_error!("twee boards tegelijk: kies er één");

#[cfg(any(
    all(feature = "board-qemuvirt", feature = "board-rpi4"),
    all(feature = "board-qemuvirt", feature = "board-rpi5"),
    all(feature = "board-rpi4", feature = "board-rpi5"),
    all(feature = "board-rk3566", feature = "board-qemuvirt"),
    all(feature = "board-rk3566", feature = "board-rpi4"),
    all(feature = "board-rk3566", feature = "board-rpi5"),
    all(feature = "board-uefi", feature = "board-qemuvirt"),
    all(feature = "board-uefi", feature = "board-rpi4"),
    all(feature = "board-uefi", feature = "board-rpi5"),
    all(feature = "board-uefi", feature = "board-rk3566")
))]
compile_error!("twee boards tegelijk: kies er één");

/// Het board van deze binary: één, gekozen door een feature.
// Het board onder een neutrale naam: de slot-, kooi- en flip-lijm
// (slots.rs, cage.rs, flip.rs) lezen het plan van het board als `vboard`
// (`slots::plan(cores, os_core)`, `mpidr`, `core_of`, de staging,
// `KERN_RAM`, `DMA`), en elk board levert die namen met zijn eigen getallen.
#[cfg(feature = "board-qemuvirt")]
extern crate board_qemuvirt as vboard;

#[cfg(feature = "board-qemuvirt")]
type Machine = board_qemuvirt::QemuVirt;

#[cfg(feature = "board-qemuvirt")]
static BOARD: Machine = board_qemuvirt::QemuVirt::new();

// De Pi's, onder dezelfde naam `vboard`.
#[cfg(feature = "board-rpi4")]
extern crate board_rpi4 as vboard;

#[cfg(feature = "board-rpi4")]
type Machine = board_rpi4::Rpi4;

#[cfg(feature = "board-rpi4")]
static BOARD: Machine = board_rpi4::Rpi4::new();

#[cfg(feature = "board-rpi5")]
extern crate board_rpi5 as vboard;

#[cfg(feature = "board-rpi5")]
type Machine = board_rpi5::Rpi5;

#[cfg(feature = "board-rpi5")]
static BOARD: Machine = board_rpi5::Rpi5::new();

// De Radxa Zero 3E, onder dezelfde naam en om dezelfde reden als de Pi's.
#[cfg(feature = "board-rk3566")]
extern crate board_rk3566 as vboard;

#[cfg(feature = "board-rk3566")]
type Machine = board_rk3566::Rk3566;

#[cfg(feature = "board-rk3566")]
static BOARD: Machine = board_rk3566::Rk3566::new();

// Het generieke UEFI-board (QEMU onder EDK2; de basis van de O6N en de
// Altra), onder dezelfde naam en om dezelfde reden als de Pi's.
#[cfg(feature = "board-uefi")]
extern crate board_uefi as vboard;

#[cfg(feature = "board-uefi")]
type Machine = board_uefi::Uefi;

#[cfg(feature = "board-uefi")]
static BOARD: Machine = board_uefi::Uefi::new();

// De Radxa Orion O6N en de Ampere Altra: het UEFI-board met hun eigen NIC,
// NVMe en thermometer. Hun crates geven de namen van het UEFI-plan door.
#[cfg(feature = "board-o6n")]
extern crate board_o6n as vboard;

#[cfg(feature = "board-o6n")]
type Machine = board_o6n::O6n;

#[cfg(feature = "board-o6n")]
static BOARD: Machine = board_o6n::O6n::new();

#[cfg(feature = "board-altra")]
extern crate board_altra as vboard;

#[cfg(feature = "board-altra")]
type Machine = board_altra::Altra;

#[cfg(feature = "board-altra")]
static BOARD: Machine = board_altra::Altra::new();

/// De heap: een bump-allocator met een plafond over de kern-RAM.
#[global_allocator]
static HEAP: Heap = Heap::new();

/// De executor van core 0. Alleen de taken op deze core raken hem aan.
static EXEC: Local<Executor> = Local::new(Executor::new());

/// Dereks bunny, het origineel (2026-07-11), letterlijk uit de Go-main:
/// oren netjes boven het snuitje. Bewust geen architectuur in de tagline
/// (ARM64 is het heden, maar AMD64-boardjes liggen al klaar). Op de UART
/// als banner; de lege regel scheidt hem van de log.
const BUNNY: [&str; 5] = [
    r#"   (\(\"#,
    r#"   ( -.-)     HopOS"#,
    r#"   o_(")(")   --------------"#,
    r#"              the Rust-only OS"#,
    "",
];

/// De ingang uit `cpu::boot`: `dtb` is x0 van de firmware, `el` het
/// exception level waarop we binnenkwamen.
#[unsafe(no_mangle)]
extern "C" fn kmain(dtb: u64, el: u64) -> ! {
    let board: &'static Machine = &BOARD;
    cpu::console::set_sink(board.console());

    println!();
    for line in BUNNY {
        println!("{line}");
    }
    println!();

    println!(
        "runtime {} none/aarch64 (HopOS v{})",
        env!("HOPOS_RUSTC"),
        env!("CARGO_PKG_VERSION")
    );

    // Vóór alles: het privilege-niveau. De kooi is een invariant, geen
    // optie; kunnen we hem niet zetten, dan starten we niet.
    let el = u8::try_from(el).unwrap_or(0);
    if let Err(e) = board.privilege(el) {
        println!("FAIL boot: {e}\nHOPOS_BOOT_EL");
        cpu::boot::park();
    }
    println!("{}", board.firmware());

    board.init_heap(&HEAP);
    board.discover(dtb);
    // De OS-core (PORT.md beslissing 2): de kern woont op de core die de
    // bootparameter kiest. Staat hij daar nog niet, dan verhuist hij nu,
    // vóór er één lijn, timer of bewoner aan deze core hangt; deze core
    // wordt dan een app-core.
    move_to_os_core(board, dtb, el);
    boot(board, dtb, el)
}

/// De `dtb` en het EL van de boot, voor de kern na zijn verhuizing.
static BOOT_ARGS: [AtomicU64; 2] = [const { AtomicU64::new(0) }; 2];

/// Verhuist de kern naar de OS-core als hij daar niet draait: die core komt
/// op met hetzelfde EL2-regime en een verse stack en gaat verder in
/// [`boot`], en deze core wacht tot de kooi-regio er is en parkeert dan als
/// app-core (`cpu::el2::hold`; de kooi haalt hem daar op). Keert alleen
/// terug als er niet verhuisd hoeft te worden, of als het niet kan (luid:
/// de kern blijft dan waar hij is).
fn move_to_os_core(board: &'static Machine, dtb: u64, el: u8) {
    let (os, why) = board.os_core();
    let here = board.this_core();
    if let Some(why) = why {
        println!(
            "oscore: hopos.oscore not honoured ({why}), the kern stays on core {here} HOPOS_OSCORE_FALLBACK"
        );
        return;
    }
    if os == here {
        return;
    }
    BOOT_ARGS[0].store(dtb, Relaxed);
    BOOT_ARGS[1].store(u64::from(el), Relaxed);
    let target = vboard::slots::mpidr(os);
    println!(
        "oscore: moving the kern from core {here} to core {os} (mpidr {target:#x}) HOPOS_OSCORE_MOVE"
    );
    match cpu::smp::start_one(os, target, moved) {
        Ok(()) => cpu::el2::hold(),
        Err(e) => println!("oscore: {e}, the kern stays on core {here} HOPOS_OSCORE_FALLBACK"),
    }
}

/// De kern op de OS-core na de verhuizing: verder waar `kmain` was.
fn moved(core: usize) -> ! {
    println!("oscore: the kern runs on core {core} HOPOS_OSCORE_UP");
    let el = u8::try_from(BOOT_ARGS[1].load(Relaxed)).unwrap_or(0);
    boot(&BOARD, BOOT_ARGS[0].load(Relaxed), el)
}

/// De boot vanaf de landing, op de OS-core: de executor en al zijn taken.
fn boot(board: &'static Machine, dtb: u64, el: u8) -> ! {
    // FLIP: de landing, als eerste na de heap. Een overdracht van een
    // vorige kern draagt de bewoners; die adopteren de slots straks.
    let landing = flip::land(dtb);

    let exec: &'static Executor = EXEC.get();
    exec.set_clock(board.clock());

    println!(
        "boot: HopOS v{} on {}, EL{el}, {} cores ({}), {} MB DRAM HOPOS_BOOT gen={} stamp={}",
        env!("CARGO_PKG_VERSION"),
        Machine::NAME,
        board.cores(),
        board.core_class(0),
        board.mem_total() >> 20,
        flip::generation(), // FLIP: 1 koud, N+1 na een flip vanaf N
        env!("HOPOS_STAMP"),
    );

    // De wandklok vóór er een bewoner is: zonder SNTP (nog niet geport) een
    // vaste waarde, luid. Hop stempelt zijn taken ermee (clock.rs).
    let off = clock::set(
        clock::BOOT_WALL_SECS.saturating_mul(1_000_000_000),
        exec.now(),
    );
    println!(
        "clock: no SNTP yet, wall clock fixed at 2026-09-29T00:00:00Z (unix {} s, offset {off} ns) HOPOS_CLOCK_FIXED",
        clock::BOOT_WALL_SECS
    );

    // Wat QEMU stagede, en of er dus een Hop is: alleen dan bestaat het
    // token, en het hoort bij het slot waar de kern Hop plaatst, vóór de
    // system-listener de eerste verbinding ziet (PORT.md beslissing 1).
    let role = staged_role();
    let privilege = match role {
        Some(StagedRole::Hop) => kern::Slot::new(slots::HOP_SLOT).and_then(Privilege::boot),
        _ => None,
    };
    if let Some(p) = &privilege {
        println!(
            "system: privilege minted for slot {} (Hop) HOPOS_PRIVILEGE",
            p.slot()
        );
    }
    // De opslag vóór de system-API: die krijgt de hopfs-actor alleen als
    // er een schijf is (anders weigert elke bestandscall luid).
    let fs = storage::start(exec);
    let system = system(privilege, fs);

    // De IRQ-dispatch spawnt als eerste: hij is de pomp van alle lijnen.
    // Faalt de controller, dan draait de node door op de vangrail van de
    // slaap (interrupts zijn een verbetering, geen voorwaarde).
    match board.start_interrupts() {
        Ok(bell) => exec.spawn(irq_dispatch(board, bell)).expect("spawn irq"),
        Err(e) => println!("irq: {e}, running on the sleep failsafe HOPOS_IRQ_FAIL"),
    }
    exec.spawn(tick(exec)).expect("spawn tick");

    // Het netwerkvlak (net.rs): de pomp op de NIC, de switch, poort 0 met
    // de node-stack, DHCP en de system-listener. Zonder NIC draait de kern
    // door zonder net; dat is een board, geen fout.
    match board.probe_nic() {
        Ok(Some(nic)) => {
            println!("net: nic up HOPOS_NIC_UP mac={}", nic.mac());
            let params = net::Params {
                // Eén slot per app-core plus één voor de OS-core, waar Hop
                // naast de kern woont (het plan van het board, PORT.md
                // beslissing 2): twee cores zijn twee slots.
                max_slots: board.cores().saturating_sub(1).max(1) + 1,
                clock: board.clock(),
                slot_wake: slots::wake,
            };
            if let Err(e) = net::start(exec, nic, params, system_api(system)) {
                println!("net: {e} HOPOS_NET_FAIL");
            }
        }
        Ok(None) => println!("net: no NIC on this board HOPOS_NIC_NONE"),
        Err(e) => println!("net: {e} HOPOS_NIC_FAIL"),
    }

    // De slots: de kooi-lijm, de lifecycle-actor en de servicers (slots.rs).
    // FLIP: na een landing adopteren de slots de bewoners in plaats van
    // Hop opnieuw te plaatsen; de flip-taak en de guard spawnen hier.
    flip::start(exec, landing.is_some());
    slots::start(exec, role, landing.map(|h| h.slots));

    // De OS-core (PORT.md beslissing 2): de idle van de kern is de rotatie
    // over zijn bewoners (Hop). Lukt dat niet, dan houdt de kern zijn core
    // voor zichzelf, luid, en draait Hop nooit.
    let mut sleeper = board.sleeper();
    match slots::os_core() {
        Ok(os) => sleeper.host(os),
        Err(e) => println!("oscore: {e}, the kern keeps its core to itself HOPOS_OS_CORE_FAIL"),
    }
    exec.run(&mut sleeper)
}

/// Meetlat van de IRQ-dispatch: timer-, NIC- en onbekende interrupts.
static IRQS: [AtomicU64; 3] = [const { AtomicU64::new(0) }; 3];

/// De IRQ-dispatch: de vector zette alleen een vlag en luidde de bel, en
/// hier wordt geclaimd, behandeld en afgesloten. Buiten exception-context,
/// dus gewoon code.
async fn irq_dispatch(board: &'static Machine, bell: &'static Signal) {
    loop {
        bell.wait().await;
        let d = board.dispatch_interrupts();
        IRQS[0].fetch_add(u64::from(d.timer), Relaxed);
        IRQS[1].fetch_add(u64::from(d.nic), Relaxed);
        IRQS[2].fetch_add(u64::from(d.other), Relaxed);
    }
}

/// De diepte van de lifecycle-inbox: een verzoek per system-verbinding plus
/// de boot-code, met marge.
pub(crate) const LIFECYCLE_DEPTH: usize = 8;

/// De inbox van de lifecycle-actor. De actor zelf is van het slot-spoor
/// (slots.rs); de system-listener stuurt hier zijn verzoeken heen.
pub(crate) static LIFECYCLE: Mailbox<Envelope<'static>, LIFECYCLE_DEPTH> = Mailbox::new();

/// De servicers van alle slots: de lifecycle-actor schrijft, `admit` leest.
/// Zolang er geen servicer leeft, laat de system-listener niemand toe.
pub(crate) static SERVICERS: Servicers = Servicers::new();

/// De system-API van deze boot, gedeeld door alle verbindingstaken
/// (net.rs), met het token van Hop als die er is.
///
/// Geen `static`: `Privilege::boot` is geen `const` (hij slaat het token
/// precies één keer), dus de API wordt één keer bij boot gebouwd en leeft
/// daarna voor altijd. Boot-code: een heap die dit niet kan geven, is
/// parkeren.
fn system(
    privilege: Option<Privilege>,
    fs: bool,
) -> &'static System<'static, 'static, LIFECYCLE_DEPTH> {
    let mut s = System::new(&LIFECYCLE, &SERVICERS, privilege, SLOT_MAX).with_logs(&slots::LOGS);
    if fs {
        s = s.with_fs(&storage::FS_INBOX);
    }
    Box::leak(Box::new(s))
}

/// De antwoordplekken van de system-API: één per verbindingstaak, zodat
/// een antwoord van de actor nooit bij een andere verbinding landt.
static SYSTEM_REPLIES: [Reply; net::SYSTEM_WORKERS] = [const { Reply::new() }; net::SYSTEM_WORKERS];

/// De haken en de console van de system-API, gedeeld door alle taken.
static SYSTEM_HOOKS: BootHooks = BootHooks;
static SYSTEM_LOG: LogTee<'static, KernConsole> = LogTee::new(KernConsole, &slots::LOGS);

/// De system-API voor de listener.
fn system_api(
    system: &'static System<'static, 'static, LIFECYCLE_DEPTH>,
) -> net::SystemApi<LIFECYCLE_DEPTH, DevMem, BootHooks, LogTee<'static, KernConsole>> {
    net::SystemApi {
        system,
        replies: &SYSTEM_REPLIES,
        mem: DevMem,
        hooks: &SYSTEM_HOOKS,
        log: &SYSTEM_LOG,
    }
}

/// Het hoogste slotnummer dat de system-API kent: het plafond uit de ABI.
/// `admit` vraagt daarbovenop een levende servicer, dus een te ruime grens
/// laat niemand extra binnen.
const SLOT_MAX: usize = abi::layout::SLOT_CAP;

/// Fysiek geheugen woordgewijs via `dev`, voor de image-stream van de
/// system-API. Wat het adres mag zijn, bewaakt de grant van de lifecycle.
/// Een handvat zonder staat: elke verbindingstaak krijgt een kloon.
#[derive(Clone, Copy)]
struct DevMem;

impl PhysMem for DevMem {
    fn read64(&self, pa: u64) -> u64 {
        dev::read64(dev::Pa(pa))
    }
    fn write64(&mut self, pa: u64, v: u64) {
        dev::write64(dev::Pa(pa), v);
    }
    fn clean_inv(&mut self, pa: u64, len: u64) {
        dev::pull(dev::Pa(pa), usize::try_from(len).unwrap_or(0));
    }
}

/// De haken van de system-API: de klok van Hop zet de wandklok van de kern
/// (en daarmee die van elke kooi); de flip toetst de bundel en legt de
/// nieuwe kern klaar (flip.rs), de sprong komt van de flip-taak.
struct BootHooks;

impl Hooks for BootHooks {
    fn set_clock(&self, unix_ns: u64) {
        let off = clock::set(unix_ns, (BOARD.clock())());
        println!("system: wall clock set by Hop to {unix_ns} ns (offset {off} ns) HOPOS_CLOCK_SET");
    }
    fn flip(&self, bundle: &kern::system::FlipBundle, sha256: &[u8; 32]) -> kern::Result {
        flip::prepare(bundle, sha256) // FLIP: toetsen en klaarleggen (flip.rs)
    }
}

/// De console van de kern voor de system-API: kernregels en app-logregels.
struct KernConsole;

impl Console for KernConsole {
    fn log(&self, args: core::fmt::Arguments<'_>) {
        println!("{args}");
    }
    fn app_line(&self, slot: kern::Slot, line: &[u8]) {
        let text = core::str::from_utf8(line).unwrap_or("<not utf-8>");
        println!("slot {slot}: {}", text.trim_end());
    }
}

/// De hartslag: elke seconde één regel met het tiknummer en de meetlat van
/// de executor.
async fn tick(exec: &'static Executor) {
    let start = exec.now();
    let mut n: u64 = 0;
    loop {
        n += 1;
        exec.until(start.saturating_add(n.saturating_mul(1_000_000_000)))
            .await;
        let s = &exec.stats;
        // De OS-core: overgangen naar een bewoner, waardoor de kern terugkwam,
        // en de tijd die de bewoners kregen. Zonder deze getallen is "Hop
        // krijgt tijd" niet te onderscheiden van "de kern spint".
        let o = &cpu::el2::OS_STATS;
        println!(
            "HOPOS_TICK {n} sleeps={} polls={} irq(timer={} nic={} other={}) os(in={} irq={} ipi={} timer={} yield={} exit={} fault={} idle={} res_ms={} kicks={})",
            s.sleeps.load(Relaxed),
            s.polls.load(Relaxed),
            IRQS[0].load(Relaxed),
            IRQS[1].load(Relaxed),
            IRQS[2].load(Relaxed),
            o.entries.load(Relaxed),
            o.irq.load(Relaxed),
            o.ipi.load(Relaxed),
            o.timer.load(Relaxed),
            o.yields.load(Relaxed),
            o.exits.load(Relaxed),
            o.faults.load(Relaxed),
            o.idle.load(Relaxed),
            o.ticks.load(Relaxed) / (cpu::idle::freq() / 1000).max(1),
            o.kicks.load(Relaxed),
        );
    }
}

/// De panic: de reden en een marker op de console, en parkeren. Er is geen
/// herstel (handboek §6); de watchdog is de tweede lijn.
#[panic_handler]
fn panic(info: &PanicInfo<'_>) -> ! {
    cpu::console::emergency(format_args!("panic: {info} HOPOS_PANIC"));
    cpu::boot::park()
}
