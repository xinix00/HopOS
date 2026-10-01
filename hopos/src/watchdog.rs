//! De node-watchdog: het beleid van `kern::watchdog` op de hardware van het
//! board, als taak op de OS-core (`OLD/metal/cmd/hopos/watchdog.go`,
//! `nodeCanary`, `armBootGuard`, `requestNodeReset`).
//!
//! Dit bezit de watchdog-taak en het levensbewijs: elke [`PET_EVERY`] kijkt
//! hij of de node gezond is (het net op, en als Hop op deze node woont zijn
//! heartbeat die loopt) en geeft dat aan het beleid, dat dan wel of niet
//! aait. Omdat de pets uit een taak op de executor komen, stopt een
//! bevroren executor ze vanzelf; dat is de helft van het vangnet. De andere
//! helft is het bewijs: een node die draait maar doof is, aait niet meer.
//!
//! Wat de Go-canary deed (een nieuwe TCP-verbinding naar de eigen
//! agent-poort) is hier de heartbeat van Hop op zijn control-page plus het
//! adres van de uplink: de agent is een bewoner in een kooi, en zijn
//! heartbeat is zijn eigen bewijs van leven, zonder een verbinding door de
//! stack van de kern. Beperking, eerlijk genoteerd: een doofheid die alleen
//! in de NAT naar Hop zit, mist dit.
//!
//! `hopos.wd=off` zet alles uit, op elk board dezelfde knop: voor een
//! UART-postmortem moet een bevroren node blijven staan.
//!
//! De hardware per board: de SBSA-watchdog uit de GTDT op de UEFI-boards
//! (`board_uefi::watchdog`; QEMU heeft er geen), de PM-watchdog van de
//! BCM-familie op de Pi's (`board_raspi::watchdog`, direct MMIO; QEMU
//! `raspi4b` modelleert alleen de reset en wordt dus niet gewapend), de
//! primaire van `/arm-io/wdt` op de Mac mini (`board_apple::wdt`), de
//! DesignWare-WDT op de Radxa (`board_rk3566::watchdog`, TOP 15 = 89,5 s
//! zoals Go), en niets op virt.

use core::cell::Cell;
use core::time::Duration;
use cpu::println;
use executor::Executor;
use kern::watchdog::{Event, Phase, Policy};
use sync::Local;

/// De aai-cadans: ruim onder de hardware-timeout (12 s gevraagd; op een
/// teller van 1 GHz loopt WOR vol en wordt het 8,6 s), zodat een trage
/// ronde geen reset is.
const PET_EVERY: Duration = Duration::from_secs(2);
/// Hoe oud de laatste heartbeat van Hop mag zijn voor hij als "stil" telt.
/// Hop slaat elke seconde; vijftien is een hapering, geen hang.
const HOP_STALE_NS: u64 = 15_000_000_000;
/// De wachtregel van fase 1 komt elke ~5 minuten (in aai-rondes).
const LOUD_EVERY: u64 = 300 / PET_EVERY.as_secs();

/// Een gevraagde reset (een verloren adres), van wie hem vroeg naar de
/// watchdog-taak. Beide op de executor van de OS-core.
static RESET: Local<Cell<Option<&'static str>>> = Local::new(Cell::new(None));

/// Vraagt een gecontroleerde herstart via de watchdog (Go:
/// `requestNodeReset`, gehangen aan `hopnet.AddressLost`): de stack kan
/// niet van adres wisselen, dus HOP-leven = node-leven. De taak houdt zijn
/// pets in en het ijzer reset. Zonder gewapende watchdog blijft het bij de
/// melding. De DHCP-keeper in net.rs roept dit bij `HOPOS_DHCP_LOST`.
/// Een aai buiten de watchdog-taak om: vlak vóór de sprong van een flip en
/// meteen na een landing, zodat de teller van de vertrekkende kern (12 s op
/// de Pi) niet afloopt terwijl de nieuwe kern nog boot (30-09).
pub(crate) fn pet_now() {
    hw::pet_now();
}

pub(crate) fn request_reset(reason: &'static str) {
    println!("node: reset requested - {reason} HOPOS_RESET_REQUEST");
    let r = RESET.get();
    if r.get().is_none() {
        r.set(Some(reason));
    }
}

/// Start de watchdog: `hopos.wd=off` zet hem uit (ook een die de vorige
/// kern wapende), een board zonder hardware zegt dat hij onbewaakt is, en
/// anders spawnt de taak. Op een flip-boot wapent de taak meteen de
/// boot-guard met twee minuten blinde gratie (de haak die flip.rs zou
/// roepen: de generatie zegt het ook).
pub(crate) fn start(exec: &'static Executor) {
    if hw::param("hopos.wd") == "off" {
        if hw::off() {
            println!("watchdog: disabled for a post-mortem (the previous kernel armed it)");
        }
        println!(
            "watchdog: not armed (hopos.wd=off) - node liveness is UNGUARDED, a frozen node stays up for a post-mortem HOPOS_WD_OFF"
        );
        return;
    }
    let Some(hw) = hw::hardware() else {
        println!(
            "watchdog: this board wires no hardware watchdog - node liveness is UNGUARDED HOPOS_WD_NONE"
        );
        return;
    };
    let flip = crate::flip::generation() > 1;
    let hop = vboard::slots::staged_role() == Ok(board::stage::StagedRole::Hop);
    if let Err(e) = exec.spawn(run(exec, hw, flip, hop)) {
        println!("watchdog: task not spawned ({e:?}) - node liveness is UNGUARDED HOPOS_WD_NONE");
    }
}

/// De heartbeat van Hop: de laatste waarde en wanneer hij veranderde.
struct Beat {
    last: u64,
    at: u64,
}

impl Beat {
    /// Leest de heartbeat van Hop; `true` als hij binnen [`HOP_STALE_NS`]
    /// bewoog.
    fn fresh(&mut self, now: u64) -> bool {
        let Some(page) = crate::clock::ctrl_page(crate::slots::HOP_SLOT) else {
            return false;
        };
        let at = page.add(abi::hopabi::CTRL_HEARTBEAT);
        dev::pull(at, 8);
        let v = dev::read64(at);
        if v != self.last {
            self.last = v;
            self.at = now;
        }
        v != 0 && now.saturating_sub(self.at) < HOP_STALE_NS
    }
}

/// De watchdog-taak.
async fn run(exec: &'static Executor, mut hw: hw::Hw, flip: bool, hop: bool) {
    let mut p = Policy::new(LOUD_EVERY);
    let e = if flip {
        p.arm_boot_guard(&mut hw, cpu::idle::counter(), cpu::idle::freq())
    } else {
        p.start(&mut hw, cpu::idle::counter())
    };
    say(&e, &hw);
    let mut beat = Beat {
        last: 0,
        at: exec.now(),
    };
    loop {
        exec.after(PET_EVERY).await;
        if p.phase() == Phase::Withheld || p.phase() == Phase::Idle {
            // Niets meer te doen: de hardware reset, of er is geen.
            continue;
        }
        if let Some(r) = RESET.get().get() {
            p.request_reset(r);
        }
        let now = exec.now();
        let net = crate::net::uplink_ip().is_some();
        let alive = net && (!hop || beat.fresh(now));
        if let Some(e) = p.tick(&mut hw, cpu::idle::counter(), alive) {
            say(&e, &hw);
        }
    }
}

/// Eén regel per gebeurtenis, met de marker van de Go-kern.
fn say(e: &Event, hw: &hw::Hw) {
    match e {
        Event::BootGuardArmed => println!(
            "watchdog: armed for the flip boot ({hw}) - blind pets limited to two minutes HOPOS_BOOT_GUARD"
        ),
        Event::Unguarded => {
            println!("watchdog: {hw} - node liveness is UNGUARDED HOPOS_WD_NONE");
        }
        Event::GraceExpired => println!(
            "watchdog: flip boot grace expired before liveness - withholding pets HOPOS_BOOT_GUARD_EXPIRED"
        ),
        Event::Armed => println!(
            "watchdog: hardware reset armed ({hw}) - boot guard: blind pets until the node proves liveness HOPOS_WD_ARMED"
        ),
        Event::Waiting(n) => println!(
            "watchdog: no liveness sign yet ({n} rounds: net up and Hop beating) - boot guard only: a full freeze resets, deafness does not yet"
        ),
        Event::Live => println!(
            "watchdog: liveness proven - pets now require the net up and Hop beating HOPOS_CANARY_LIVE"
        ),
        Event::Miss(n) => println!(
            "watchdog: liveness check failed ({n} in a row) - withholding the pet; hardware reset follows unless the node recovers HOPOS_CANARY_MISS"
        ),
        Event::ResetRequested(r) => println!(
            "watchdog: reset requested ({r}) - withholding pets, hardware reset follows HOPOS_RESET_REQUESTED"
        ),
    }
}

/// De hardware op de UEFI-boards: de SBSA-watchdog uit de GTDT. De
/// LicheeRV geeft dezelfde vier namen (`board_licheerv::watchdog`, de
/// DW-WDT; wapent alleen als de probe van de boot antwoordde, en
/// `hopos.cfg` komt uit het venster in het image), en de Pi's ook
/// (`board_raspi::watchdog`, de PM-watchdog; wapent alleen als zijn teller
/// loopt, en de sleutels staan in cmdline.txt), en de Radxa ook
/// (`board_rk3566::watchdog`, de DW-WDT; de sleutels uit `hopos.cfg` in de
/// initrd, anders de APPEND-regel).
#[cfg(any(
    feature = "board-uefi",
    feature = "board-o6n",
    feature = "board-altra",
    feature = "board-licheerv",
    feature = "board-rpi4",
    feature = "board-rpi5",
    feature = "board-rk3566"
))]
mod hw {
    use core::fmt;
    use vboard::watchdog as wd;

    /// De gevraagde hardware-timeout (Go: 12 s).
    #[cfg(not(feature = "board-rk3566"))]
    const TIMEOUT_MS: u64 = 12_000;
    /// De Radxa: TOP 15, ongeveer 89,5 s, zoals Go (de DW-WDT kent alleen
    /// machten van twee; de gemeten waarde staat in `board_rk3566::watchdog`).
    #[cfg(feature = "board-rk3566")]
    const TIMEOUT_MS: u64 = wd::TIMEOUT_MS;

    /// De SBSA-watchdog; na `arm` zijn beschrijving of de reden van falen.
    pub(super) struct Hw {
        desc: Option<wd::Desc>,
        why: &'static str,
    }

    impl kern::watchdog::Hardware for Hw {
        fn arm(&mut self) -> bool {
            match wd::arm(TIMEOUT_MS) {
                Ok(d) => {
                    self.desc = Some(d);
                    true
                }
                Err(why) => {
                    self.why = why;
                    false
                }
            }
        }
        fn pet(&mut self) {
            wd::pet();
        }
    }

    /// Een aai buiten de taak om: vlak vóór de sprong van een flip, en
    /// meteen na een landing. Dan is deze kern nog niet gewapend en telt de
    /// teller van de vorige door (de Pi 5, 30-09: gereset na 12 s tijdens
    /// 5 s framebuffer-geduld), dus op de Pi's een herlaad van wat de
    /// vorige kern wapende. Op de Radxa is de kick zelf die herlaad (een
    /// DW-WDT laadt bij elke kick zijn TOP opnieuw).
    pub(super) fn pet_now() {
        wd::pet();
        #[cfg(any(feature = "board-rpi4", feature = "board-rpi5"))]
        wd::reload_if_armed(TIMEOUT_MS);
    }

    impl fmt::Display for Hw {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            match &self.desc {
                Some(d) => write!(f, "{d}"),
                None => f.write_str(self.why),
            }
        }
    }

    /// Er is een kandidaat; of hij er werkelijk is, zegt `arm`.
    pub(super) fn hardware() -> Option<Hw> {
        Some(Hw {
            desc: None,
            why: "not armed",
        })
    }

    /// Zet een gewapende watchdog uit (`hopos.wd=off` na een flip).
    pub(super) fn off() -> bool {
        wd::off()
    }

    /// Een sleutel uit `hopos.cfg`.
    #[cfg(not(any(
        feature = "board-rpi4",
        feature = "board-rpi5",
        feature = "board-rk3566"
    )))]
    pub(super) fn param(key: &'static str) -> &'static str {
        fw::bootcfg::first(fw::bootcfg::all(crate::BOARD.config(), key))
    }

    /// Een sleutel uit cmdline.txt (de Pi's: /chosen/bootargs), of op de
    /// Radxa uit `hopos.cfg` in de initrd en anders de APPEND-regel.
    #[cfg(any(
        feature = "board-rpi4",
        feature = "board-rpi5",
        feature = "board-rk3566"
    ))]
    pub(super) fn param(key: &'static str) -> &'static str {
        vboard::boot_param(key)
    }
}

/// De Mac mini: de primaire watchdog van `/arm-io/wdt`, dezelfde die
/// `discover` bij de boot stil zette (iBoot laat er meer dan één gewapend
/// achter; natief resette de node zonder dat op 1:43, 31-08). Dezelfde vorm
/// als de UEFI-module hierboven, met `board_apple::wdt`.
#[cfg(feature = "board-apple")]
mod hw {
    use core::fmt;
    use vboard::wdt;

    /// De primaire van `/arm-io/wdt`; na `arm` zijn beschrijving of de reden
    /// van falen.
    pub(super) struct Hw {
        desc: Option<wdt::Desc>,
        why: &'static str,
    }

    impl kern::watchdog::Hardware for Hw {
        fn arm(&mut self) -> bool {
            // 30 s (Go `wdtTimeout`): de ANS en de SMC blokkeren bij hun
            // opstart tot seconden (RTKit `POWER_TIMEOUT_NS`).
            match wdt::arm(wdt::WDT_TIMEOUT_MS) {
                Ok(d) => {
                    self.desc = Some(d);
                    true
                }
                Err(why) => {
                    self.why = why;
                    false
                }
            }
        }
        fn pet(&mut self) {
            wdt::pet();
        }
    }

    /// Een aai buiten de taak om (rond een flip).
    pub(super) fn pet_now() {
        wdt::pet();
    }

    impl fmt::Display for Hw {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            match &self.desc {
                Some(d) => write!(f, "{d}"),
                None => f.write_str(self.why),
            }
        }
    }

    /// Er is een kandidaat als de boom er een beschrijft; of hij wapent,
    /// zegt `arm`.
    pub(super) fn hardware() -> Option<Hw> {
        Some(Hw {
            desc: None,
            why: "not armed",
        })
    }

    /// Zet een gewapende watchdog uit (`hopos.wd=off`).
    pub(super) fn off() -> bool {
        wdt::off()
    }

    /// Een sleutel uit `hopos.cfg` (het venster in het image, of de loader).
    pub(super) fn param(key: &'static str) -> &'static str {
        fw::bootcfg::first(fw::bootcfg::all(crate::BOARD.config(), key))
    }
}

/// De rest (virt, de riscv64-virt): geen watchdog bedraad.
#[cfg(not(any(
    feature = "board-uefi",
    feature = "board-o6n",
    feature = "board-altra",
    feature = "board-apple",
    feature = "board-licheerv",
    feature = "board-rpi4",
    feature = "board-rpi5",
    feature = "board-rk3566"
)))]
mod hw {
    use core::fmt;

    /// Geen hardware.
    pub(super) struct Hw;

    impl kern::watchdog::Hardware for Hw {
        fn arm(&mut self) -> bool {
            false
        }
        fn pet(&mut self) {}
    }

    /// Geen watchdog: niets te aaien.
    pub(super) fn pet_now() {}

    impl fmt::Display for Hw {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.write_str("no watchdog on this board")
        }
    }

    pub(super) fn hardware() -> Option<Hw> {
        None
    }

    pub(super) fn off() -> bool {
        false
    }

    pub(super) fn param(_key: &'static str) -> &'static str {
        ""
    }
}
