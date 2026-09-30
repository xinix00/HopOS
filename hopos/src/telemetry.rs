//! De telemetrie van de node: de thermiek op de tik en op de heartbeat van
//! Hop, en het klokbeleid (`driver_dvfs::run`) als taak op de OS-core.
//!
//! Dit bezit de laatste temperatuurmeting ([`temp_milli_c`]) en de twee
//! taken die hem en de klok bijhouden. De temperatuur gaat elke seconde op
//! de control-page van Hop (`CTRL_TEMP`), zodat Hop hem op zijn heartbeat
//! zet zoals de Go-agent deed (`Temp: board.TempMilliC`, zichtbaar in `hop
//! agents`). De control-page, niet `SLOT_STATUS`: die is van de system-API
//! (een ander spoor), en een woord op de page kost Hop geen call.
//!
//! Per board ([`hw`]): de O6N meet via de SCP (SCMI) en heeft een knop
//! (`_CPC`), de Altra meet via de SMpro (PCC) en laat de klok aan de
//! firmware, de Mac mini meet één keer bij de boot (de SMC) en bewaakt zijn
//! p-states, de Pi's meten en klokken via de VideoCore-mailbox
//! (`board_raspi::clock`), de Radxa meet met de TSADC van de SoC
//! (`board_rk3566::tsadc`), de rest meet (nog) niets. Het beleid leest op de
//! O6N en de Pi's dezelfde tellers ([`counters`]).

use core::sync::atomic::{AtomicI32, Ordering::Relaxed};
use core::time::Duration;
use cpu::println;
use executor::Executor;

/// De laatste meting in milligraden; 0 = geen.
static TEMP: AtomicI32 = AtomicI32::new(0);

/// De laatste temperatuur in milligraden (0 = geen meting), voor de tik.
pub(crate) fn temp_milli_c() -> i32 {
    TEMP.load(Relaxed)
}

/// De temperatuur als tekst voor de tik: `41.5C`, of `-` zonder meting.
pub(crate) struct Temp(pub(crate) i32);

impl core::fmt::Display for Temp {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        if self.0 == 0 {
            return f.write_str("-");
        }
        write!(f, "{}.{}C", self.0 / 1000, (self.0 % 1000).abs() / 100)
    }
}

/// De klok vol vlak vóór de sprong van een flip (flip.rs), op de boards
/// met een knop; de rest doet niets.
pub(crate) fn clock_full_for_flip() {
    hw::full_for_flip();
}

/// Spawnt de thermiek-taak en, als het board een knop heeft, het
/// klokbeleid.
pub(crate) fn start(exec: &'static Executor) {
    hw::open();
    if let Err(e) = exec.spawn(thermal(exec)) {
        println!("hwmon: task not spawned ({e:?}), no temperature");
    }
    hw::governor(exec);
}

/// Elke seconde: meten, bewaren, en op de control-page van Hop zetten.
async fn thermal(exec: &'static Executor) {
    loop {
        let t = hw::temp();
        TEMP.store(t, Relaxed);
        if let Some(page) = crate::clock::ctrl_page(crate::slots::HOP_SLOT) {
            let at = page.add(abi::hopabi::CTRL_TEMP);
            dev::write64(at, i64::from(t) as u64);
            // Hop leest zijn page met de MMU uit: naar DRAM ermee.
            dev::push(at, 8);
        }
        exec.after(Duration::from_secs(1)).await;
    }
}

/// De tellers van het klokbeleid, voor elk board met een knop: de
/// idle-teller en de status van elk slot op zijn control-page, en of zijn
/// bewoner nu rekent. Eén keer, voor de O6N en de Pi's.
#[cfg(any(feature = "board-o6n", feature = "board-rpi4", feature = "board-rpi5"))]
mod counters {
    use core::time::Duration;
    use cpu::println;
    use executor::Executor;
    use vboard::dvfs::{Host, SAMPLE_NS, Sample};

    /// De bronnen van het beleid: de kern-core en zestien slots (de O6N heeft
    /// er twaalf, een Pi vier).
    pub(super) const SOURCES: usize = 17;

    /// De tellers voor het beleid: de idle-teller en de status van elk slot
    /// op zijn control-page, en of zijn bewoner nu rekent (het ctx-blok).
    pub(super) struct SlotHost {
        exec: &'static Executor,
        plan: Option<abi::layout::Plan>,
    }

    impl SlotHost {
        /// De tellers van `exec`, met het slot-plan van de OS-core.
        pub(super) fn new(exec: &'static Executor) -> Self {
            Self {
                exec,
                plan: crate::slots::os_plan().ok(),
            }
        }

        /// Rekent de bewoner van slot `i` nu (niet geyield)? Op de O6N
        /// idlet een app met een yield (HVC), en zonder deze vraag las elke
        /// slapende app als 100% bezig en zakte de klok nooit (23-09).
        fn running(&self, i: usize) -> bool {
            let Some(plan) = &self.plan else {
                return true;
            };
            abi::layout::Slot::new(i)
                .and_then(|s| plan.ctx_pa(s).ok())
                .and_then(cpu::el2::ctx_state)
                .is_none_or(|st| st == abi::layout::CtxState::Running)
        }
    }

    fn word(page: dev::Pa, off: u64) -> u64 {
        dev::pull(page.add(off), 8);
        dev::read64(page.add(off))
    }

    impl Host<SOURCES> for SlotHost {
        fn now(&self) -> u64 {
            self.exec.now()
        }

        fn sleep(&self, ns: u64) -> impl core::future::Future<Output = ()> {
            self.exec.after(Duration::from_nanos(ns))
        }

        fn expect(&self) -> u64 {
            cpu::idle::freq().saturating_mul(SAMPLE_NS) / 1_000_000_000
        }

        /// Bron 0 (de kern-core zelf) telt niet mee: zijn idle-tijd houdt de
        /// slaper van de executor bij en die leest geen taak. Hop, die de
        /// OS-core met de kern deelt, telt wel: als slot, met zijn eigen
        /// teller.
        fn sample(&mut self, out: &mut [Sample; SOURCES]) {
            use abi::hopabi::{AppStatus, CTRL_CORES, CTRL_IDLE, CTRL_STATUS};
            for (i, s) in out.iter_mut().enumerate() {
                *s = Sample::default();
                if i == 0 {
                    continue;
                }
                let Some(page) = crate::clock::ctrl_page(i) else {
                    continue;
                };
                if AppStatus::from_raw(word(page, CTRL_STATUS)) != Some(AppStatus::Ready) {
                    continue;
                }
                *s = Sample {
                    live: true,
                    idle: word(page, CTRL_IDLE),
                    cores: word(page, CTRL_CORES),
                    running: self.running(i),
                };
            }
        }

        fn temp_milli_c(&mut self) -> i32 {
            super::temp_milli_c()
        }

        fn log(&self, args: core::fmt::Arguments<'_>) {
            println!("{args}");
        }
    }
}

/// De O6N: de SCP-sensoren en de `_CPC`-knop.
#[cfg(feature = "board-o6n")]
mod hw {
    /// Nog niets vóór een flip: de `_CPC`-knop blijft van de governor-taak
    /// (of een stille O6N ook een landende kern hindert, is ongemeten).
    pub(super) fn full_for_flip() {}

    use super::counters::{SOURCES, SlotHost};
    use cpu::println;
    use executor::Executor;
    use vboard::dvfs::{self, SAMPLE_NS};

    pub(super) fn open() {}

    pub(super) fn temp() -> i32 {
        crate::BOARD.temp_milli_c()
    }

    fn param(key: &'static str) -> &'static str {
        fw::bootcfg::first(fw::bootcfg::all(crate::BOARD.config(), key))
    }

    /// Het klokbeleid: `hopos.clock` (`dvfs`, `max`, `quiet`, `firmware`)
    /// en `hopos.mhz` (het plafond).
    pub(super) fn governor(exec: &'static Executor) {
        let v = param("hopos.clock");
        let (hold, ok) = dvfs::hold_of(v);
        if !ok {
            println!("dvfs: hopos.clock={v:?} is not dvfs, max, quiet or firmware; following load");
        }
        let Some(hold) = hold else {
            println!(
                "dvfs: hopos.clock=firmware, the boot operating point stays HOPOS_CLOCK_FIRMWARE"
            );
            return;
        };
        let mhz = param("hopos.mhz").parse::<u32>().ok();
        let knob = match crate::BOARD.clock_knob(mhz) {
            Ok(k) => k,
            Err(e) => {
                println!("dvfs: {e}, the clock stays where the firmware left it HOPOS_CLOCK_NONE");
                return;
            }
        };
        println!(
            "dvfs: {} _CPC domains, policy {hold:?}, cap {}, sample {} ms, window 50 ms, cooldown 30 s HOPOS_CLOCK_UP",
            knob.domains().len(),
            mhz.map_or(0, |m| m),
            SAMPLE_NS / 1_000_000
        );
        let task = async move {
            let mut host = SlotHost::new(exec);
            dvfs::run::<SOURCES>(knob, hold, &mut host).await;
        };
        if let Err(e) = exec.spawn(task) {
            println!("dvfs: task not spawned ({e:?}), the clock stays at full");
        }
    }
}

/// De Altra: de SMpro via PCC-kanaal 14; de klok is op servers
/// firmware-domein, dus geen beleid.
#[cfg(feature = "board-altra")]
mod hw {
    use cpu::println;
    use executor::Executor;

    /// Opent de SMpro met de PCCT van de firmware (één proeflees, één regel).
    /// Geen knop hier: niets te doen vóór een flip.
    pub(super) fn full_for_flip() {}

    pub(super) fn open() {
        match crate::BOARD.acpi_table(b"PCCT") {
            Some(pcct) => crate::BOARD.open_hwmon(pcct),
            None => println!("hwmon: no PCCT - temperature telemetry off"),
        }
    }

    pub(super) fn temp() -> i32 {
        crate::BOARD.temp_milli_c()
    }

    pub(super) fn governor(_exec: &'static Executor) {
        println!("dvfs: server clocks are firmware domain on this board HOPOS_CLOCK_NONE");
    }
}

/// De Mac mini: de klok regelt het silicium zelf (de APSC, die
/// `discover` aanzette onder het plafond van `hopos.pstate`), en de
/// wachter meldt elke sprong (Go `PStateWatch`, `board_apple::wdt`). De
/// thermometer is de SMC; die praat bij elke meting een RTKit-coprocessor
/// wakker en weer in slaap (tot seconden), dus hij meet één keer bij de boot
/// (`hopos.smc=1`, in de bootlog) en niet op de tik: een oude waarde op de
/// heartbeat van Hop zou liegen, dus hier 0 (`-`).
#[cfg(feature = "board-apple")]
mod hw {
    use core::time::Duration;
    use cpu::println;
    use executor::Executor;
    use vboard::wdt::PStateWatch;

    /// De cadans van de wachter (Go: twee seconden).
    const WATCH_EVERY: Duration = Duration::from_secs(2);

    pub(super) fn open() {}

    /// Geen knop hier: niets te doen vóór een flip.
    pub(super) fn full_for_flip() {}

    pub(super) fn temp() -> i32 {
        0
    }

    pub(super) fn governor(exec: &'static Executor) {
        let w = PStateWatch::new();
        if w.is_empty() {
            println!("dvfs: no cluster blocks in the ADT, the boot clock stays HOPOS_CLOCK_NONE");
            return;
        }
        println!(
            "dvfs: the APSC governs under the hopos.pstate ceiling; watching the p-states every {} s HOPOS_APPLE_PSTATE_WATCH",
            WATCH_EVERY.as_secs()
        );
        if let Err(e) = exec.spawn(watch(exec, w)) {
            println!("dvfs: p-state watch not spawned ({e:?})");
        }
    }

    /// De wachter: alleen-lezen, één regel per sprong.
    async fn watch(exec: &'static Executor, mut w: PStateWatch) {
        loop {
            w.poll();
            exec.after(WATCH_EVERY).await;
        }
    }
}

/// De Pi's: de SoC-temperatuur via de VideoCore-mailbox
/// (`board_raspi::temp_millic`, dezelfde tag als de bootregel `vcmail:
/// 58.713 C`), en de ARM-klok als knop via dezelfde mailbox
/// (`board_raspi::clock`, Go `StartDVFS`).
#[cfg(any(feature = "board-rpi4", feature = "board-rpi5"))]
mod hw {
    use super::counters::{SOURCES, SlotHost};
    use cpu::println;
    use executor::Executor;
    use vboard::dvfs::{self, Knob, SAMPLE_NS};

    pub(super) fn open() {}

    /// Geen knop hier: niets te doen vóór een flip.
    /// Milligraden uit de mailbox; 0 = geen meting (de mailbox is nog niet
    /// open, of de firmware antwoordde niet).
    pub(super) fn temp() -> i32 {
        vboard::temp_millic().map_or(0, |t| i32::try_from(t).unwrap_or(0))
    }

    /// Het klokbeleid: `hopos.clock` (`dvfs`, `max`, `quiet`, `firmware`)
    /// en `hopos.mhz` (het plafond) uit cmdline.txt, zoals op de O6N.
    pub(super) fn governor(exec: &'static Executor) {
        let v = vboard::boot_param("hopos.clock");
        let (hold, ok) = dvfs::hold_of(v);
        if !ok {
            println!("dvfs: hopos.clock={v:?} is not dvfs, max, quiet or firmware; following load");
        }
        let Some(hold) = hold else {
            println!(
                "dvfs: hopos.clock=firmware, the boot operating point stays HOPOS_CLOCK_FIRMWARE"
            );
            return;
        };
        let mhz = vboard::boot_param("hopos.mhz").parse::<u32>().ok();
        let knob = match crate::BOARD.clock_knob(mhz) {
            Ok(k) => k,
            Err(e) => {
                println!("dvfs: {e}, the clock stays where the firmware left it HOPOS_CLOCK_NONE");
                return;
            }
        };
        let p = knob.plan();
        println!(
            "dvfs: ARM via the mailbox, full {} MHz, quiet {} MHz (firmware min/max {}/{}), policy {hold:?}, cap {}, sample {} ms, window 50 ms, cooldown 30 s HOPOS_CLOCK_UP",
            p.full_hz / 1_000_000,
            p.quiet_hz / 1_000_000,
            p.min_hz / 1_000_000,
            p.max_hz / 1_000_000,
            mhz.map_or(0, |m| m),
            SAMPLE_NS / 1_000_000
        );
        // De boot-flank nu, synchroon en vóór het net (main: telemetry::start
        // gaat vóór net::start): de NIC-init hoort op de volle klok, zoals op
        // een koude boot. Na een flip vanuit een stille kern (800 MHz)
        // initialiseerde de NIC op 800 en sprong de klok er meteen na, en
        // twee keer meldde de NIC daarna nooit meer (30-09, generatie 2).
        let mut knob = knob;
        let level = knob.full();
        match level {
            Some(l) => println!("dvfs: -> {l} (full, boot) HOPOS_CLOCK_EDGE"),
            None => {
                println!("dvfs: clock change to full (boot) failed, the policy keeps its state")
            }
        }
        let task = async move {
            let mut host = SlotHost::new(exec);
            dvfs::run_after_boot::<SOURCES>(knob, hold, &mut host, level).await;
        };
        if let Err(e) = exec.spawn(task) {
            println!("dvfs: task not spawned ({e:?}), the clock stays at full");
        }
    }

    /// De klok vol vlak vóór de sprong van een flip (flip.rs): de
    /// vertrekkende kern laat de nieuwe niet op een stille klok landen. Een
    /// eigen knop op dezelfde mailbox; de governor-taak komt niet meer aan
    /// de beurt.
    pub(super) fn full_for_flip() {
        if let Ok(mut k) = crate::BOARD.clock_knob(None) {
            match k.full() {
                Some(l) => println!("dvfs: -> {l} (full, flip) HOPOS_CLOCK_EDGE"),
                None => println!("dvfs: clock change to full (flip) failed"),
            }
        }
    }
}

/// De Radxa: de TSADC van de SoC (`board_rk3566::tsadc`), het warmste van
/// de twee kanalen (CPU en GPU); geen knop, de klok blijft waar U-Boot hem
/// liet. De sensor gaf in Go nooit een conversie (06-08); `open` zegt in
/// één regel wat hij nu teruggeeft.
#[cfg(feature = "board-rk3566")]
mod hw {
    use cpu::println;
    use executor::Executor;

    /// Brengt de sensor op (busy-waits van ~5 ms, één keer bij de boot) en
    /// meldt de eerste lezing.
    pub(super) fn open() {
        vboard::tsadc::open();
    }

    /// Geen knop hier: niets te doen vóór een flip.
    pub(super) fn full_for_flip() {}

    /// Milligraden; 0 = geen geldige code.
    pub(super) fn temp() -> i32 {
        vboard::tsadc::temp_millic().unwrap_or(0)
    }

    pub(super) fn governor(_exec: &'static Executor) {
        println!(
            "dvfs: no clock knob on this board, the firmware keeps its clock HOPOS_CLOCK_NONE"
        );
    }
}

#[cfg(not(any(
    feature = "board-o6n",
    feature = "board-altra",
    feature = "board-apple",
    feature = "board-rpi4",
    feature = "board-rpi5",
    feature = "board-rk3566"
)))]
mod hw {
    use cpu::println;
    use executor::Executor;

    pub(super) fn open() {}

    /// Geen knop hier: niets te doen vóór een flip.
    pub(super) fn full_for_flip() {}

    pub(super) fn temp() -> i32 {
        0
    }

    pub(super) fn governor(_exec: &'static Executor) {
        println!(
            "dvfs: no clock knob on this board, the firmware keeps its clock HOPOS_CLOCK_NONE"
        );
    }
}
