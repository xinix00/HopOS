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

mod bench; // de meetbanken achter hopos.nvmebench en hopos.idlestat (bench.rs)
mod clock;
mod codec; // het media-vlak (codec.rs); kaal een stub, feature `media`
mod config;
mod conport; // de console over TCP: de ring achter de UART (conport.rs)
mod flip; // FLIP: de kern-flip (flip.rs)
mod glue; // de arch-vrije kooi-lijm, `DevMem` en `KernConsole` (glue.rs)
mod gui; // het gui-vlak (gui.rs); kaal no-ops, feature `gui`
#[cfg(all(
    target_arch = "aarch64",
    target_os = "none",
    not(feature = "board-apple")
))]
mod mem; // memcpy en memcmp met ongealigneerde ldp/stp (mem.rs)
mod net;
#[cfg(feature = "media")]
mod optical;
mod seed; // het zaad van de slots op hun control-page (seed.rs)
mod slots;
mod storage;
mod telemetry; // de thermiek op tik en heartbeat, en het klokbeleid (telemetry.rs)
mod watchdog; // de node-watchdog: kern::watchdog op de hardware (watchdog.rs)

extern crate alloc;

use alloc::boxed::Box;
use board::Board;
use board::heap::Heap;
use core::panic::PanicInfo;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use cpu::println;
use executor::Executor;
use glue::{DevMem, KernConsole};
use kern::slots::{Envelope, Reply, Servicers};
use kern::system::{Hooks, LogTee, Privilege, System};
use netdev::Device;
use sync::mpsc::Mailbox;
use sync::{Local, Signal};
use vboard::slots::{StagedRole, staged_role};

// Precies één board. Twee geven al twee keer `vboard`, geen geeft geen
// `Machine`; deze som zegt het in woorden.
const _: () = assert!(
    cfg!(feature = "board-qemuvirt") as u8
        + cfg!(feature = "board-rpi4") as u8
        + cfg!(feature = "board-rpi5") as u8
        + cfg!(feature = "board-rk3566") as u8
        + cfg!(feature = "board-uefi") as u8
        + cfg!(feature = "board-o6n") as u8
        + cfg!(feature = "board-altra") as u8
        + cfg!(feature = "board-qemuvirt-riscv") as u8
        + cfg!(feature = "board-licheerv") as u8
        + cfg!(feature = "board-apple") as u8
        == 1,
    "kies precies één board: --features board-qemuvirt, board-rpi4, board-rpi5, board-rk3566, board-uefi, board-o6n, board-altra, board-qemuvirt-riscv, board-licheerv of board-apple"
);

// Het board onder een neutrale naam: de slot-, kooi- en flip-lijm
// (slots.rs, cage.rs, flip.rs) lezen het plan van het board als `vboard`
// (`slots::plan(cores, os_core)`, `mpidr`, `core_of`, de staging,
// `KERN_RAM`, `DMA`), en elk board levert die namen met zijn eigen getallen,
// en zichzelf als `Machine`. De O6N en de Altra zijn het UEFI-board met hun
// eigen NIC, NVMe en thermometer; de riscv64-boards (docs/boards-riscv.md)
// bouwen op `--target riscv64gc-unknown-none-elf`.
#[cfg(feature = "board-altra")]
extern crate board_altra as vboard;
#[cfg(feature = "board-apple")]
extern crate board_apple as vboard;
#[cfg(feature = "board-licheerv")]
extern crate board_licheerv as vboard;
#[cfg(feature = "board-o6n")]
extern crate board_o6n as vboard;
#[cfg(feature = "board-qemuvirt")]
extern crate board_qemuvirt as vboard;
#[cfg(feature = "board-qemuvirt-riscv")]
extern crate board_qemuvirt_riscv as vboard;
#[cfg(feature = "board-rk3566")]
extern crate board_rk3566 as vboard;
#[cfg(feature = "board-rpi4")]
extern crate board_rpi4 as vboard;
#[cfg(feature = "board-rpi5")]
extern crate board_rpi5 as vboard;
#[cfg(feature = "board-uefi")]
extern crate board_uefi as vboard;

/// Het board van deze binary: één, gekozen door een feature.
type Machine = vboard::Machine;

static BOARD: Machine = Machine::new();

/// De architectuur van deze binary, voor de runtime-regel.
const ARCH: &str = if cfg!(target_arch = "riscv64") {
    "riscv64"
} else {
    "aarch64"
};

/// De teller waarin de meetlat van de OS-core (`el2::OS_STATS.ticks`)
/// telt: CNTFRQ op arm64, de timebase van de TIME-CSR op riscv64.
const OS_HZ: fn() -> u64 = if cfg!(target_arch = "riscv64") {
    cpu::riscv::idle::hz
} else {
    cpu::idle::freq
};

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
    let uart = board.console();
    cpu::console::set_sink(uart);
    // De ring van de TCP-console achter de UART (conport.rs); het glas komt
    // er in `setup` achter, in dezelfde tee.
    conport::install(uart);

    println!();
    for line in BUNNY {
        println!("{line}");
    }
    println!();

    println!(
        "runtime {} none/{} (HopOS v{})",
        env!("HOPOS_RUSTC"),
        ARCH,
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
    // De ring van 5555 opnieuw aan deze core: op de M4 kent `this_core`
    // de cores pas na de ADT (in `discover`), en daarvóór was dit core 0.
    // Zonder deze regel viel alles tot `conport::here` weg, de landing van
    // een flip en de zwarte doos incluis (01-10).
    conport::here();
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
///
/// Twee helften, en dat is de les van 30-09: alles wat de boot opbouwt
/// (switch, pomp, actoren, de futures vóór hun `Box`) stond in één frame van
/// 125 KB (`sub sp, 0x1e9a0`), en dat frame bleef onder `exec.run` staan
/// zolang de kern leeft. Een plaatsing piekte daarop tot 186 van de 256 KB
/// stack. [`setup`] bouwt alles in een eigen frame dat weg is vóór de
/// executor draait; hier blijft alleen de slaper over.
fn boot(board: &'static Machine, dtb: u64, el: u8) -> ! {
    let exec: &'static Executor = EXEC.get();
    // De deur van de switch om de slaper heen (`net::doorbell`): zonder
    // hem wachtte elk frame van een app op de failsafe van 1 ms.
    let mut sleeper = net::doorbell(setup(board, dtb, el));
    exec.run(&mut sleeper)
}

/// Bouwt de boot op: de landing, de opslag, het net, de slots en de
/// taken. Nooit inline: zijn frame (de grote boot-locals) moet weg zijn
/// voordat [`boot`] de executor start.
#[inline(never)]
fn setup(board: &'static Machine, dtb: u64, el: u8) -> <Machine as Board>::Sleeper {
    // FLIP: de landing, als eerste na de heap. Een overdracht van een
    // vorige kern draagt de bewoners; die adopteren de slots straks.
    let landing = flip::land(dtb);
    // De watchdog van de vertrekkende kern loopt door: meteen herladen, vóór
    // het framebuffer-geduld en de rest van de boot (de Pi 5, 30-09).
    if landing.is_some() {
        watchdog::pet_now();
    }

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

    // De ring van de TCP-console is vanaf hier van de OS-core (na een
    // verhuizing schreef de boot-core hem tot nu), dan de console op het
    // glas, op dezelfde core (gui.rs): de bunny als kop, de log eronder.
    // Zonder framebuffer of kaal gebouwd: één regel of niets.
    conport::here();
    gui::init_framebuffer_console(board);

    // De wandklok vóór er een bewoner is. Na een flip draagt de overdracht
    // de offset van de vorige kern (Hop had hem via SNTP gezet, en de
    // teller liep door de sprong heen door); anders een vaste waarde, luid,
    // tot Hop hem zet. Hop stempelt zijn taken ermee (clock.rs).
    if clock::offset() != 0 {
        println!(
            "clock: wall clock carried over the flip (offset {} ns) HOPOS_CLOCK_CARRIED",
            clock::offset()
        );
    } else {
        let off = clock::set(
            clock::BOOT_WALL_SECS.saturating_mul(1_000_000_000),
            exec.now(),
        );
        println!(
            "clock: no SNTP yet, wall clock fixed at 2026-09-29T00:00:00Z (unix {} s, offset {off} ns) HOPOS_CLOCK_FIXED",
            clock::BOOT_WALL_SECS
        );
    }

    // Het zaad van de slots vóór er een kooi is: de DRBG van de kern is
    // geseed (door het board, anders hier), en de regel zegt wat een slot
    // op zijn control-page krijgt (seed.rs).
    seed::init();

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
    // er een schijf is (anders weigert elke bestandscall luid). De schijf
    // wordt één keer geprobed; de meetbanken (bench.rs, alleen met
    // hopos.nvmebench of hopos.idlestat) lenen hem vóór de opslag hem mount.
    let disk = bench::start(exec, dtb, storage::probe());
    let fs = storage::start(exec, disk);
    // MEDIA: de codec-dienst ná de opslag, want de firmware-blobs staan op
    // het volume (codec.rs); zonder VPU meldt hij luid dat er geen is.
    codec::up(exec);
    let system = system(privilege, fs, slot_count(board));

    // De IRQ-dispatch spawnt als eerste: hij is de pomp van alle lijnen.
    // Faalt de controller, dan draait de node door op de vangrail van de
    // slaap (interrupts zijn een verbetering, geen voorwaarde).
    match board.start_interrupts() {
        Ok(bell) => exec.spawn(irq_dispatch(board, bell)).expect("spawn irq"),
        Err(e) => println!("irq: {e}, running on the sleep failsafe HOPOS_IRQ_FAIL"),
    }
    exec.spawn(tick(exec)).expect("spawn tick");
    // De node-watchdog (op een flip-boot meteen met de boot-guard) en de
    // telemetrie: thermiek en klokbeleid.
    watchdog::start(exec);
    telemetry::start(exec);
    gui::start_screen_status(exec); // de meetregels naast de bunny

    // De config van de node: de tekst van het board (bench::cfg_text,
    // kern::nodecfg), en alleen op QEMU, dat geen bootmedium heeft, voor Hop
    // de vaste bankconfig erachter. Hier al, vóór het net: de console over
    // TCP (conport.rs) kiest uit dezelfde config, en haar listener start
    // zodra de lease er is, ook na een flip waarin Hop niet opnieuw wordt
    // geplaatst.
    let mut hop_cfg = bench::cfg_text(dtb);
    if cfg!(any(
        feature = "board-qemuvirt",
        feature = "board-qemuvirt-riscv"
    )) && role == Some(StagedRole::Hop)
    {
        hop_cfg.push_str(kern::nodecfg::QEMU_CFG);
    }
    let node_cfg = kern::nodecfg::NodeCfg::parse(&hop_cfg);
    conport::enable(kern::nodecfg::console_enabled(&node_cfg));
    REPLAY_AT.store(kern::nodecfg::replay_after(&node_cfg), Relaxed);

    // Het netwerkvlak (net.rs): de pomp op de NIC, de switch, poort 0 met
    // de node-stack, DHCP en de system-listener. Zonder NIC draait de kern
    // door zonder net; dat is een board, geen fout.
    match board.probe_nic() {
        Ok(Some(nic)) => {
            println!("net: nic up HOPOS_NIC_UP mac={}", nic.mac());
            let params = net::Params {
                max_slots: slot_count(board),
                clock: board.clock(),
                slot_wake: slots::wake,
                resident: slots::resident,
            };
            if let Err(e) = net::start(exec, nic, params, system_api(system)) {
                println!("net: {e} HOPOS_NET_FAIL");
            }
        }
        Ok(None) => println!("net: no NIC on this board HOPOS_NIC_NONE"),
        Err(e) => println!("net: {e} HOPOS_NIC_FAIL"),
    }
    // De USB-invoer na het netwerk (gui.rs): de stroom naar de display-app
    // loopt over de switch.
    #[cfg(feature = "media")]
    optical::start(exec);
    gui::start_usb_input(exec);

    // De slots: de kooi-lijm, de lifecycle-actor en de servicers (slots.rs).
    // FLIP: na een landing adopteren de slots de bewoners in plaats van
    // Hop opnieuw te plaatsen; de flip-taak en de guard spawnen hier.
    flip::start(exec, landing.is_some());
    // De env van een gestagede app (appspike op QEMU): `hopos.appenv`.
    let app_env = match role {
        Some(StagedRole::App) => slots::app_env(&bench::bootparam(dtb, "hopos.appenv")),
        _ => alloc::vec::Vec::new(),
    };
    slots::start(exec, role, landing.map(|h| h.slots), app_env, hop_cfg);

    // De OS-core (PORT.md beslissing 2): de idle van de kern is de rotatie
    // over zijn bewoners (Hop). Lukt dat niet, dan houdt de kern zijn core
    // voor zichzelf, luid, en draait Hop nooit.
    let mut sleeper = board.sleeper();
    match slots::os_core() {
        Ok(os) => sleeper.host(os),
        Err(e) => println!("oscore: {e}, the kern keeps its core to itself HOPOS_OS_CORE_FAIL"),
    }
    sleeper
}

/// Meetlat van de IRQ-dispatch: timer-, NIC- en onbekende interrupts.
static IRQS: [AtomicU64; 3] = [const { AtomicU64::new(0) }; 3];

/// De IRQ-dispatch: de vector zette alleen een vlag en luidde de bel, en
/// hier wordt geclaimd, behandeld en afgesloten. Buiten exception-context,
/// dus gewoon code.
async fn irq_dispatch(board: &'static Machine, bell: &'static Signal) {
    loop {
        bell.wait().await;
        dispatch_once(board);
    }
}

/// Eén ronde van de dispatch van het board, geteld in [`IRQS`].
fn dispatch_once(board: &Machine) {
    let d = board.dispatch_interrupts();
    IRQS[0].fetch_add(u64::from(d.timer), Relaxed);
    IRQS[1].fetch_add(u64::from(d.nic), Relaxed);
    IRQS[2].fetch_add(u64::from(d.other), Relaxed);
}

/// Handelt af wat al bij de controller wacht, buiten de dispatch-taak om:
/// voor de zelftest van de OS-core, die in de boot draait, vóór de executor
/// de taak ooit pollt (slots.rs, `probe`). Dezelfde context als de taak
/// (de executor-core van de kern), en dezelfde ronde, dus de signalen die
/// hij zet ziet de taak straks gewoon.
#[cfg_attr(
    target_arch = "riscv64",
    allow(dead_code, reason = "de zelftest van RISC-V staat in cage_riscv.rs")
)]
pub(crate) fn drain_interrupts() {
    dispatch_once(&BOARD);
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

/// De rij van de store-ops (`kern::store`): de apps zetten er hun
/// store-calls in, Hop haalt ze op. Een `static`: acht plaatsen van
/// 10,5 KiB horen in .bss, niet op de heap van de kern.
static STORE: kern::store::StoreQueue = kern::store::StoreQueue::new();

/// De system-API van deze boot, gedeeld door alle verbindingstaken
/// (net.rs), met het token van Hop als die er is.
///
/// Geen `static`: `Privilege::boot` is geen `const` (hij slaat het token
/// precies één keer), dus de API wordt één keer bij boot gebouwd en leeft
/// daarna voor altijd. Boot-code: een heap die dit niet kan geven, is
/// parkeren.
fn system(privilege: Option<Privilege>, fs: bool, max_slots: usize) -> &'static KernSystem {
    let mut s = System::new(&LIFECYCLE, &SERVICERS, privilege, max_slots)
        .with_logs(&slots::LOGS)
        .with_store(&STORE);
    if fs {
        s = s.with_fs(&storage::FS_INBOX);
    }
    #[cfg(feature = "media")]
    {
        s = s.with_devices(&optical::INBOX);
    }
    Box::leak(Box::new(s))
}

/// De antwoordplekken van de system-API: één per verbindingstaak, zodat
/// een antwoord van de actor nooit bij een andere verbinding landt.
static SYSTEM_REPLIES: [Reply; net::SYSTEM_WORKERS] = [const { Reply::new() }; net::SYSTEM_WORKERS];

/// De system-API van de kern: één, bij boot gebouwd ([`system`]).
type KernSystem = System<'static, 'static, LIFECYCLE_DEPTH>;
/// De console van de system-API: de kernconsole met de logrij van de slots.
type SystemLog = LogTee<'static, KernConsole>;

/// De haken en de console van de system-API, gedeeld door alle taken.
static SYSTEM_HOOKS: BootHooks = BootHooks;
static SYSTEM_LOG: SystemLog = LogTee::new(KernConsole, &slots::LOGS);

/// De system-API voor de listener.
fn system_api(system: &'static KernSystem) -> net::SystemApi {
    net::SystemApi {
        system,
        replies: &SYSTEM_REPLIES,
        hooks: &SYSTEM_HOOKS,
        log: &SYSTEM_LOG,
    }
}

/// Het aantal slots van dit board: één per app-core plus één voor de
/// OS-core, waar Hop naast de kern woont (het plan van het board, PORT.md
/// beslissing 2): twee cores zijn twee slots. De switch en de system-API
/// kennen hetzelfde getal. Tot 01-10 kreeg de system-API het plafond uit de
/// ABI (128): een volle Pi 4 zocht dan door tot slot 5, de lifecycle
/// weigerde die met "slot 5 out of range 1..4" in plaats van "full", Hop las
/// daar geen capaciteitstekort in en bleef elke paar seconden opnieuw
/// vragen, en elke vraag was 128 statuscalls door de lifecycle-mailbox: een
/// koude flip vond hem vol ("actor mailbox full").
fn slot_count(board: &Machine) -> usize {
    board.cores().saturating_sub(1).max(1) + 1
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

/// De maat van de kern-stack: `STACK_SIZE` in elk linkscript (hopos/*.ld),
/// boven de wachtpagina (`__stack_guard`) direct boven `.bss`.
const STACK_BYTES: u64 = 0x40000;

/// Hoe ver onder de eigen stackpositie de meter niet wist: een IRQ-frame
/// en de vector die tijdens de meting binnenkomen, landen daar.
const STACK_METER_SLACK: u64 = 16 * 1024;

/// De diepste stack sinds de vorige meting, in bytes, en daarna het stuk
/// daaronder weer op nul (de volgende tik meet opnieuw).
///
/// De meter leest de stack van onderen tot het eerste woord dat niet nul is
/// (QEMU en elke loader geven nul-RAM; na een kern-flip is de eerste meting
/// ruis). Les van 30-09: de plaatsing van een slot (`kern::rpc::mount_table`
/// onder de lifecycle, de publicatieregel) haalde 190 KB van de 256, en een
/// overloop schreef zonder wachtpagina stil in het einde van `.bss` (de
/// switch-tabel, de deuren van de system-API, de heap). Sindsdien: het
/// boot-frame is weg vóór de executor draait ([`boot`]), de lifecycle bouwt
/// zijn actor buiten zijn future, en de boards met `cpu::boot` en de Mac
/// mini zetten een wachtpagina onder de stack (`__stack_guard`; de
/// UEFI-boards nog niet, zie `cpu::boot`). Na de boot meet de tik 16 KB
/// (dat is de meetmarge zelf), tegen 180 KB ervoor. Eén scan van 32K
/// woorden per seconde; de schrijfslag alleen over wat de vorige tik vuil
/// maakte.
fn stack_high_water() -> u64 {
    unsafe extern "C" {
        safe static __bss_end: u8;
        safe static __stack_top: u8;
    }
    let top = (&raw const __stack_top).addr() as u64;
    let bottom = top.saturating_sub(STACK_BYTES);
    if bottom < (&raw const __bss_end).addr() as u64 {
        return 0; // Een linkscript met een andere indeling: niet meten.
    }
    let here = 0u64;
    let sp = (&raw const here).addr() as u64;
    let mut p = bottom;
    while p < top && dev::read64(dev::Pa(p)) == 0 {
        p += 8;
    }
    let used = top - p;
    let mut z = p;
    while z < sp.saturating_sub(STACK_METER_SLACK) {
        dev::write64(dev::Pa(z), 0);
        z += 8;
    }
    used
}

/// De hartslag: elke seconde één regel met het tiknummer en de meetlat van
/// de executor.
/// `hopos.replay=N` (kern::nodecfg): de tik waarop de kern het begin van zijn
/// console herhaalt; 0 = nooit.
static REPLAY_AT: core::sync::atomic::AtomicU64 = core::sync::atomic::AtomicU64::new(0);

async fn tick(exec: &'static Executor) {
    let start = exec.now();
    let mut n: u64 = 0;
    // De slaaptijd van de vorige tik (executor `slept_ns`): de rest van de
    // tik was werk op de OS-core (`busy_ms`).
    let (mut at, mut slept) = (start, 0u64);
    loop {
        n += 1;
        // Een late lezer (de M4 over de dockchannel) krijgt de boot alsnog:
        // elke N tikken de kop, de oudste 16 KiB van de ring en de staart,
        // zodat een lezer die pas na een kabel-herplug aanhaakt er een vangt.
        let replay_at = REPLAY_AT.load(Relaxed);
        if replay_at != 0 && n.is_multiple_of(replay_at) {
            println!(
                "console: replaying the first 16 KiB of this boot for a late reader (hopos.replay={replay_at}) HOPOS_CONSOLE_REPLAY"
            );
            let bytes = conport::replay(16 * 1024);
            println!("console: end of the replay ({bytes} bytes) HOPOS_CONSOLE_REPLAY_END");
        }
        // De RP1-keten op 5 en 30 s (de flip-jacht van 30-09): een koude
        // boot geeft de referentie, een landing het verschil.
        #[cfg(feature = "board-rpi5")]
        if n == 5 || n == 30 {
            vboard::nic_diag();
        }
        let due = start.saturating_add(n.saturating_mul(1_000_000_000));
        exec.until(due).await;
        // Hoe laat deze tik kwam. Een kern die een tijd niets rondmaakte
        // (30-09, tot alpha.14: de periodieke hopfs-commit wachtte synchroon
        // op twee FLUSHes van de schijf, op macOS F_FULLFSYNC's van het
        // image, en de hele OS-core met Hop stond dan stil) haalt zijn
        // gemiste tikken daarna in één salvo in; late_ms zegt dan hoe lang
        // de stilte was.
        let late_ms = exec.now().saturating_sub(due) / 1_000_000;
        let s = &exec.stats;
        let (now, slept_now) = (exec.now(), s.slept_ns.load(Relaxed));
        let busy_ms = now
            .saturating_sub(at)
            .saturating_sub(slept_now.wrapping_sub(slept))
            / 1_000_000;
        (at, slept) = (now, slept_now);
        // De OS-core: overgangen naar een bewoner, waardoor de kern terugkwam,
        // en de tijd die de bewoners kregen. Zonder deze getallen is "Hop
        // krijgt tijd" niet te onderscheiden van "de kern spint".
        let o = &cpu::el2::OS_STATS;
        // De laatste beurt en de langste sinds de vorige tik (30-09: de
        // stilte van de hoplb-kring was niet te lezen zonder te weten wie de
        // OS-core het laatst had), en de switch: werk na de bel tegen werk
        // na de failsafe, volle en gedropte RX, en wat de NAT weggooide.
        let last = o.last.load(Relaxed);
        let long_us = o.longest.swap(0, Relaxed) / (OS_HZ() / 1_000_000).max(1);
        let sw = &net::STATS;
        let stack_kb = stack_high_water() / 1024;
        println!(
            "HOPOS_TICK {n} late_ms={late_ms} busy_ms={busy_ms} sleeps={} polls={} irq(timer={} nic={} other={}) os(in={} irq={} ipi={} timer={} yield={} exit={} fault={} idle={} res_ms={} kicks={}) turn(last={}:{} long_us={long_us}) stack_kb={stack_kb} sw(door={} timer={} rxfull={} rxdrop={} big={} noroute={} txdrop={} flowfull={} natin={} natmiss={}) temp={}",
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
            o.ticks.load(Relaxed) / (OS_HZ() / 1000).max(1),
            o.kicks.load(Relaxed),
            last & 0xff,
            cpu::el2::Back::name_of((last >> 8) & 0xff),
            sw.work_by_door.load(Relaxed),
            sw.work_by_timer.load(Relaxed),
            sw.rx_full.load(Relaxed),
            sw.rx_drops.load(Relaxed),
            sw.nat_oversize.load(Relaxed),
            sw.nat_no_route.load(Relaxed),
            sw.uplink_tx_drops.load(Relaxed),
            sw.nat_flow_full.load(Relaxed),
            sw.nat_reply_in.load(Relaxed),
            sw.nat_in_unmatched.load(Relaxed),
            telemetry::Temp(telemetry::temp_milli_c()),
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
