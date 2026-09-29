//! De kern-binary van HopOS v3: kiest precies één board, zet de console op,
//! toont de bunny en draait de executor.
//!
//! De bootvolgorde is die van de Go-main (`OLD/metal/cmd/hopos/main.go`)
//! voor zover de lagen eronder er zijn: console, bunny, `runtime`-regel,
//! het privilege-niveau, de firmware-regel, en dan het werk als taken op de
//! executor. Een taak per lus: de tik, de IRQ-dispatch, de NIC-pomp.

#![no_std]
#![no_main]
#![allow(clippy::expect_used)] // boot-code: vóór de agent draait is falen parkeren (handboek §12)

use board::Board;
use board::heap::Heap;
use core::panic::PanicInfo;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use cpu::println;
use executor::Executor;
use netdev::Device;
use sync::{Local, Signal, select};

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

    match board.probe_nic() {
        Ok(Some(nic)) => {
            println!("net: nic up HOPOS_NIC_UP mac={}", nic.mac());
            exec.spawn(nic_pump(exec, nic)).expect("spawn nic");
        }
        Ok(None) => println!("net: no NIC on this board HOPOS_NIC_NONE"),
        Err(e) => println!("net: {e} HOPOS_NIC_FAIL"),
    }

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

/// De vangrail van de RX-pomp als de NIC een lijn heeft. De Go-kern nam
/// 10 ms omdat tamago's IRQ-pad flanken verloor (21-09, L83 p42); hier is
/// de lijn level-triggered en de deur dicht, dus is de vangrail een
/// vangrail, en houdt 100 ms de node ver onder de 100 wekken per seconde.
const RX_GUARD: Duration = Duration::from_millis(100);

/// De microslaap van de pomp zonder lijn (de Go-kern: 300 µs).
const RX_POLL: Duration = Duration::from_micros(300);

/// De RX-pomp: wacht op de bel van de NIC (of de vangrail), haalt alles
/// op, en doet één doorbell per burst. Tot `net` de uplink drijft, meldt
/// hij alleen het eerste frame; een ARP-vraag naar de gateway zorgt dat er
/// een komt (QEMU's user-net stuurt uit zichzelf niets).
async fn nic_pump<N: Device>(exec: &'static Executor, mut nic: N) {
    let arp = arp_who_has(nic.mac().0, GUEST_IP, GATEWAY_IP);
    if let Err(e) = nic.transmit(&arp) {
        println!("net: arp probe not sent: {e}");
    }
    nic.flush();
    let mut buf = [0u8; netdev::MAX_FRAME];
    let mut frames: u64 = 0;
    loop {
        while let Some(n) = nic.receive(&mut buf) {
            frames += 1;
            if frames == 1 {
                println!("net: first frame received HOPOS_RX_FRAME len={n}");
            }
        }
        nic.flush();
        match nic.irq() {
            Some(bell) => {
                let _ = select(bell.wait(), exec.after(RX_GUARD)).await;
            }
            None => exec.after(RX_POLL).await,
        }
    }
}

/// Het adres van de gast op QEMU's user-net (de slirp-default).
const GUEST_IP: [u8; 4] = [10, 0, 2, 15];
/// De gateway van QEMU's user-net.
const GATEWAY_IP: [u8; 4] = [10, 0, 2, 2];

/// Een ARP-vraag "wie heeft `target`?" van `mac`/`ip`, opgevuld tot de
/// Ethernet-minimumlengte van 60 bytes.
fn arp_who_has(mac: [u8; 6], ip: [u8; 4], target: [u8; 4]) -> [u8; 60] {
    let mut f = [0u8; 60];
    let fields: [&[u8]; 11] = [
        &[0xff; 6],    // bestemming: broadcast
        &mac,          // bron
        &[0x08, 0x06], // EtherType ARP
        &[0x00, 0x01], // hardware: Ethernet
        &[0x08, 0x00], // protocol: IPv4
        &[6, 4],       // adreslengtes
        &[0x00, 0x01], // operatie: request
        &mac,          // afzender-MAC
        &ip,           // afzender-IP
        &[0; 6],       // doel-MAC: onbekend
        &target,       // doel-IP
    ];
    let mut at = 0;
    for field in fields {
        if let Some(dst) = f.get_mut(at..at + field.len()) {
            dst.copy_from_slice(field);
        }
        at += field.len();
    }
    f
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
