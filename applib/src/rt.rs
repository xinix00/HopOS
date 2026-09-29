//! De main-schil: van `_start` tot de `async fn` van de app.
//!
//! De volgorde is die van `applib.Init` in Go, zonder wat hier niet meer
//! bestaat (memattr, memlimit, SMP, de idle-hooks): de staart uitrekenen,
//! de outbox inhangen (vanaf dan kan de app loggen, ook een paniek), de
//! heap en de klok, READY, en dan twee taken op de executor van de
//! app-core: de heartbeat en de app zelf. Keert de app terug, dan is dat
//! exit 0, na het net-afscheid van [`App::shutdown`].
//!
//! Een app schrijft:
//!
//! ```ignore
//! async fn app_main(app: &'static applib::App) {
//!     applib::log!("hello from slot {}", app.slot());
//! }
//! applib::main!(app_main);
//! ```
//!
//! Op het target zet `_start` (hieronder, `global_asm!`) de stack bovenin de
//! RAM-declaratie, veegt `.bss` en springt naar `__applib_main`, die
//! [`main!`](crate::main!) in de binary definieert. Vergeet een app de macro,
//! dan is dat een linkfout, geen stille lege app.

use crate::app::{App, Beat};
use crate::arch;
use crate::clock;
use crate::heap::HEAP;
use crate::sleep::AppSleeper;
use core::cell::OnceCell;
use core::future::Future;
use core::time::Duration;
use executor::Executor;
use sync::Local;

/// Taken op de app-core: de heartbeat, de app, een RX-pomp en ruimte voor
/// wat de app zelf spawnt.
pub const TASKS: usize = 64;

/// Gelijktijdige timers op de app-core.
pub const TIMERS: usize = 32;

/// De executor van een app-core.
pub type Exec = Executor<TASKS, TIMERS>;

/// De executor van de app-core. Eén core, één executor (handboek §4).
pub static EXEC: Local<Exec> = Local::new(Executor::new());

/// Het App van dit image, gezet door de main-schil vóór de eerste taak.
static APP: Local<OnceCell<App>> = Local::new(OnceCell::new());

/// Het App, als de main-schil het al gezet heeft.
#[must_use]
pub fn app() -> Option<&'static App> {
    APP.get().get()
}

/// De stack onder de top van de RAM-declaratie. Daaronder houdt de heap op.
pub const STACK_SIZE: u64 = 256 << 10;

/// Zoveel bytes onder het einde van de RAM-declaratie begint de stack, zoals
/// `ramStackOffset` in de Go-hopslot.
pub const STACK_TOP_GAP: u64 = 0x100;

/// Het ritme van de heartbeat.
pub const WATCH_PERIOD: Duration = Duration::from_millis(50);

/// De heartbeat als taak: elke 50 ms de teller, de kill-vlag, en om de twee
/// seconden de geheugen-draw.
pub async fn watch(app: &'static App) {
    let exec: &'static Exec = EXEC.get();
    let mut beat: u64 = 0;
    loop {
        beat = beat.wrapping_add(1);
        // Een kill wacht niet op het net-afscheid: de kern vraagt nu.
        if app.beat(beat, HEAP.used()) == Beat::Kill {
            app.exit(0);
        }
        exec.after(WATCH_PERIOD).await;
    }
}

/// De main-schil: zet alles op en draait de executor. Keert nooit terug.
pub fn run<F, Fut>(main: F) -> !
where
    F: FnOnce(&'static App) -> Fut + 'static,
    Fut: Future<Output = ()> + 'static,
{
    let (start, size, slot) = symbols::ram();
    // Zonder staart geen outbox en geen control-page: er is niemand om het
    // aan te vertellen, dus teruggeven. De kern ziet een core die nooit
    // READY werd.
    let Ok(fresh) = App::new(start, size, slot) else {
        arch::park_exit();
    };
    // Vanaf hier kan de app loggen, ook een paniek: `log!` vindt de outbox
    // via dit App.
    let app: &'static App = APP.get().get_or_init(move || fresh);

    let heap_end = start.saturating_add(size).saturating_sub(STACK_SIZE);
    // SAFETY: tussen het einde van het image (`__hopapp_heap_start`, het
    // laatste symbool van applib/link.ld) en de stack onder de top van de
    // RAM-declaratie ligt niets: dat stuk van de eigen partitie is van
    // niemand anders (stage-2 geeft het alleen aan deze app) en leeft zo
    // lang als de app. `init` gebeurt één keer, vóór de eerste allocatie.
    // Op de host is het begin 0 en blijft de heap leeg.
    unsafe {
        HEAP.init(
            symbols::heap_start(),
            usize::try_from(heap_end).unwrap_or(0),
        );
    }

    let exec: &'static Exec = EXEC.get();
    exec.set_clock(clock::now_ns);
    clock::start_event_stream();
    app.announce();

    let spawned = exec.spawn(watch(app)).and_then(|()| {
        exec.spawn(async move {
            main(app).await;
            app.shutdown(0).await;
        })
    });
    if let Err(e) = spawned {
        crate::log!("applib: spawn failed: {e} HOPOS_APP_SPAWN");
        app.exit(1);
    }
    exec.run(&mut AppSleeper::new(app.ctrl()))
}

/// De main-schil van een app: `applib::main!(app_main)`, met
/// `async fn app_main(app: &'static applib::App)`.
#[macro_export]
macro_rules! main {
    ($f:path) => {
        /// De ingang na `_start` (applib/src/rt.rs).
        #[unsafe(no_mangle)]
        pub extern "C" fn __applib_main() -> ! {
            $crate::rt::run($f)
        }
    };
}

/// De namen die `mod symbols` exporteert. `export_name` eist een letterlijke
/// string; deze toets houdt die letterlijke namen gelijk aan wat
/// `abi::place` zoekt, zodat een hernoeming aan één kant niet bouwt.
const SYMBOL_NAMES: [(&str, &str); 4] = [
    (abi::place::SYM_RAM_START, "runtime/goos.RamStart"),
    (abi::place::SYM_RAM_SIZE, "runtime/goos.RamSize"),
    (
        abi::place::SYM_SLOT_HINT,
        "github.com/xinix00/HopOS/metal/v2/board/hopslot.slotHint",
    ),
    (
        abi::place::SYM_ABI,
        "github.com/xinix00/HopOS/metal/v2/app/applib.abiVersion",
    ),
];

const fn same(a: &str, b: &str) -> bool {
    let (a, b) = (a.as_bytes(), b.as_bytes());
    if a.len() != b.len() {
        return false;
    }
    let mut i = 0;
    while i < a.len() {
        if a[i] != b[i] {
            return false;
        }
        i += 1;
    }
    true
}

const _: () = {
    let mut i = 0;
    while i < SYMBOL_NAMES.len() {
        assert!(same(SYMBOL_NAMES[i].0, SYMBOL_NAMES[i].1));
        i += 1;
    }
};

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod symbols {
    //! De woorden die de kern bij plaatsing in het image patcht, en de grens
    //! van het image.
    //!
    //! De namen zijn die van `abi::place` (`SYM_*`): nog de Go-namen, zodat
    //! één plaatsingsweg Go- en Rust-images bedient (PORT.md §1).
    //!
    //! Het zijn `AtomicU64`'s en geen gewone statics: de waarde verandert
    //! buiten de compiler om (de kern schrijft hem vóór de start), en een
    //! onveranderlijke static mag de compiler vouwen tot zijn beginwaarde.
    //! Ze staan in `.data.hopabi`, geïnitialiseerd en met bytes in het
    //! bestand, zodat `_start` ze niet veegt.
    use crate::contract::{ABI_TAIL, ABI_VERSION, SLOT_LINK_BASE};
    use core::sync::atomic::{AtomicU64, Ordering::Relaxed};

    /// RamStart: de partitiebasis zoals de app hem ziet. Buiten de kern om
    /// (een QEMU-dev-boot) is het het linkadres.
    // `export_name` is een unsafe attribuut: de naam moet uniek zijn in het
    // image. Dat is hij; de kern zoekt precies deze.
    #[unsafe(export_name = "runtime/goos.RamStart")]
    #[unsafe(link_section = ".data.hopabi")]
    #[used]
    static RAM_START: AtomicU64 = AtomicU64::new(SLOT_LINK_BASE);

    /// RamSize: de RAM-declaratie (partitie min staart). Buiten de kern om
    /// een partitie van 16 MB.
    // Uniek in het image, zie `RAM_START`.
    #[unsafe(export_name = "runtime/goos.RamSize")]
    #[unsafe(link_section = ".data.hopabi")]
    #[used]
    static RAM_SIZE: AtomicU64 = AtomicU64::new((16 << 20) - ABI_TAIL);

    /// SlotHint: het slotnummer van deze start (0 = niet gepatcht).
    // Uniek in het image, zie `RAM_START`.
    #[unsafe(export_name = "github.com/xinix00/HopOS/metal/v2/board/hopslot.slotHint")]
    #[unsafe(link_section = ".data.hopabi")]
    #[used]
    static SLOT_HINT: AtomicU64 = AtomicU64::new(0);

    /// Het ABI-stempel: de kern leest het bij plaatsing uit het ELF en
    /// weigert een image met een andere versie. Een app die op het verkeerde
    /// adres zijn control-page zoekt is anders een stille misread. `#[used]`
    /// en `KEEP` in het linker-script houden het in het image (in Go veegde
    /// de linker een ongelezen stempel weg, en toen weigerde de kern élk
    /// image).
    // Uniek in het image, zie `RAM_START`.
    #[unsafe(export_name = "github.com/xinix00/HopOS/metal/v2/app/applib.abiVersion")]
    #[unsafe(link_section = ".data.hopabi")]
    #[used]
    static ABI_STAMP: u64 = ABI_VERSION;

    // `__hopapp_heap_start` komt uit applib/link.ld; alleen zijn adres wordt
    // genomen, nooit zijn inhoud.
    unsafe extern "C" {
        static __hopapp_heap_start: u8;
    }

    /// (RamStart, RamSize, slot).
    pub(super) fn ram() -> (u64, u64, u64) {
        (
            RAM_START.load(Relaxed),
            RAM_SIZE.load(Relaxed),
            SLOT_HINT.load(Relaxed),
        )
    }

    /// Het eerste adres na het image: daar begint de heap.
    pub(super) fn heap_start() -> usize {
        (&raw const __hopapp_heap_start).addr()
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod symbols {
    //! Host-stub: een image buiten de kern om, zonder heap.
    use crate::contract::{ABI_TAIL, SLOT_LINK_BASE};

    pub(super) fn ram() -> (u64, u64, u64) {
        (SLOT_LINK_BASE, (16 << 20) - ABI_TAIL, 0)
    }

    pub(super) fn heap_start() -> usize {
        0
    }
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod entry {
    //! `_start` en de paniek.

    // De ingang. De EL2-trampoline van de kern heeft stage-2, de timers en
    // een schone SCTLR al geregeld en ERET't hierheen op EL1; wat rest:
    // interrupts dicht (een app heeft geen vectoren), de stack bovenin de
    // RAM-declaratie, `.bss` vegen, en door naar de Rust-kant. De kern
    // veegde `.bss` bij plaatsing al; het nog eens doen maakt het image
    // onafhankelijk van de lader (QEMU `-kernel`, een apploader).
    core::arch::global_asm!(
        ".section .text._start, \"ax\"",
        ".global _start",
        "_start:",
        "    msr daifset, #0xf",
        // De twee woorden bij naam, tussen aanhalingstekens: de Go-namen
        // van `abi::place` dragen een `/` en een `.`, en een `sym`-operand
        // citeert niet.
        "    adrp x0, \"runtime/goos.RamStart\"",
        "    ldr x0, [x0, :lo12:\"runtime/goos.RamStart\"]",
        "    adrp x1, \"runtime/goos.RamSize\"",
        "    ldr x1, [x1, :lo12:\"runtime/goos.RamSize\"]",
        "    add x0, x0, x1",
        "    sub x0, x0, #{gap}",
        "    and x0, x0, #0xfffffffffffffff0",
        "    mov sp, x0",
        "    adrp x0, __hopapp_bss_start",
        "    add x0, x0, :lo12:__hopapp_bss_start",
        "    adrp x1, __hopapp_bss_end",
        "    add x1, x1, :lo12:__hopapp_bss_end",
        "1:  cmp x0, x1",
        "    b.hs 2f",
        "    str xzr, [x0], #8",
        "    b 1b",
        "2:  bl __applib_main",
        "3:  wfe",
        "    b 3b",
        gap = const super::STACK_TOP_GAP,
    );

    /// De paniek: de reden naar de outbox (elke regel een record, zonder
    /// allocatie), dan exit 2 en de core terug naar de kern. Zonder deze
    /// weg was een paniek in Go een exit-code 2 zonder één regel reden
    /// (gemeten 31-07), of erger, een lijk op een gedeelde core dat zelfs
    /// de stage-2-intrekking niet meer voelde (19-07).
    #[panic_handler]
    fn panic(info: &core::panic::PanicInfo<'_>) -> ! {
        // Alleen de outbox: de stack wordt na dit punt niet meer gepompt.
        crate::log::emit_outbox(format_args!("panic: {info} HOPOS_APP_PANIC"));
        match super::app() {
            Some(app) => app.exit(2),
            None => crate::arch::park_exit(),
        }
    }
}
