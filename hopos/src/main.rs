//! De kern-binary van HopOS v3: kiest precies één board, zet de console op,
//! toont de bunny en draait de executor.
//!
//! De bootvolgorde is die van de Go-main (`OLD/metal/cmd/hopos/main.go`)
//! voor zover de lagen eronder er zijn: console, bunny, `runtime`-regel,
//! het privilege-niveau, de firmware-regel, en dan het werk als taken op de
//! executor. Een taak per lus: de tik, de IRQ-dispatch, en het netwerkvlak
//! (net.rs: pomp, switch, poort 0, DHCP, de system-listener).

#![no_std]
#![no_main]
#![allow(clippy::expect_used)] // boot-code: vóór de agent draait is falen parkeren (handboek §12)

mod net;
mod slots;

extern crate alloc;

use board::Board;
use board::heap::Heap;
use core::panic::PanicInfo;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use cpu::println;
use executor::Executor;
use kern::cage::{Console, PhysMem};
use kern::slots::{Envelope, Reply, Servicers};
use kern::system::{Hooks, LogTee, System};
use netdev::Device;
use sync::mpsc::Mailbox;
use sync::{Local, Signal};

#[cfg(not(feature = "board-qemuvirt"))]
compile_error!("kies precies één board: --features board-qemuvirt");

/// Het board van deze binary: één, gekozen door een feature.
#[cfg(feature = "board-qemuvirt")]
type Machine = board_qemuvirt::QemuVirt;

#[cfg(feature = "board-qemuvirt")]
static BOARD: Machine = board_qemuvirt::QemuVirt::new();

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
    r#"              the Go-only OS"#,
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

    let exec: &'static Executor = EXEC.get();
    exec.set_clock(board.clock());

    println!(
        "boot: HopOS v{} on {}, EL{el}, {} cores ({}), {} MB DRAM HOPOS_BOOT",
        env!("CARGO_PKG_VERSION"),
        Machine::NAME,
        board.cores(),
        board.core_class(0),
        board.mem_total() >> 20,
    );

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
                max_slots: board.cores().saturating_sub(1).max(1),
                clock: board.clock(),
                slot_wake: slots::wake,
            };
            if let Err(e) = net::start(exec, nic, params, system_api()) {
                println!("net: {e} HOPOS_NET_FAIL");
            }
        }
        Ok(None) => println!("net: no NIC on this board HOPOS_NIC_NONE"),
        Err(e) => println!("net: {e} HOPOS_NIC_FAIL"),
    }

    // De slots: de kooi-lijm, de lifecycle-actor en de servicers (slots.rs).
    slots::start(exec);

    let mut sleeper = board.sleeper();
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

/// De system-API, gedeeld door alle verbindingstaken (net.rs). Zonder
/// `Privilege`: het token hoort bij het slot van Hop en wordt door de
/// lifecycle geslagen zodra die Hop start.
static SYSTEM: System<'static, 'static, LIFECYCLE_DEPTH> =
    System::new(&LIFECYCLE, &SERVICERS, None, SLOT_MAX).with_logs(&slots::LOGS);

/// De antwoordplekken van de system-API: één per verbindingstaak, zodat
/// een antwoord van de actor nooit bij een andere verbinding landt.
static SYSTEM_REPLIES: [Reply; net::SYSTEM_WORKERS] = [const { Reply::new() }; net::SYSTEM_WORKERS];

/// De haken en de console van de system-API, gedeeld door alle taken.
static SYSTEM_HOOKS: BootHooks = BootHooks;
static SYSTEM_LOG: LogTee<'static, KernConsole> = LogTee::new(KernConsole, &slots::LOGS);

/// De system-API voor de listener.
fn system_api() -> net::SystemApi<LIFECYCLE_DEPTH, DevMem, BootHooks, LogTee<'static, KernConsole>>
{
    net::SystemApi {
        system: &SYSTEM,
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

/// De haken van de system-API zolang er geen wandklok en geen flip-pad is:
/// luid weigeren in plaats van stil slikken.
struct BootHooks;

impl Hooks for BootHooks {
    fn set_clock(&self, unix_ns: u64) {
        println!(
            "system: clock set to {unix_ns} ns ignored, no wall clock yet HOPOS_CLOCK_IGNORED"
        );
    }
    fn flip(&self, bundle: kern::Slot, _sha256: &[u8; 32]) -> kern::Result {
        println!(
            "system: flip to the bundle in slot {bundle} refused, no flip path yet HOPOS_FLIP_REFUSED"
        );
        Err(kern::Error::Kind)
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
        println!(
            "HOPOS_TICK {n} sleeps={} polls={} irq(timer={} nic={} other={})",
            s.sleeps.load(Relaxed),
            s.polls.load(Relaxed),
            IRQS[0].load(Relaxed),
            IRQS[1].load(Relaxed),
            IRQS[2].load(Relaxed),
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
