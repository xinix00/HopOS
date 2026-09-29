//! De slaap van een arm64-core: de [`Sleeper`] van de executor, en de klok
//! ([`now`]) die de executor als [`executor::Clock`] krijgt.
//!
//! Dit bezit: de keuze van de event-stream (EVNTI per CNTFRQ), de drie
//! manieren van slapen (WFE met event-stream, WFI op de fysieke timer, de
//! yield naar de EL2-switcher op een gedeelde core) en de tellers die zeggen
//! of een core slaapt of spint. Niet van hier: wat er te doen is. De deuren
//! (IRQ-vlag, ringkoppen) zitten in de executor en komen binnen als het
//! `ready`-predicaat; de governor uit de Go-kern, met zijn `nested`-vlaggen,
//! `RunIdleTimers` en `IdleMayReady`, bestaat niet meer (PORT §4).
//!
//! Waarom slapen hier geen detail is (Derek): jobs staan vooral te idlen, en
//! een slapende core is clock-gated en verbruikt vrijwel niets, op elke
//! kloksnelheid. Een core die "idle" spint is dat niet.

use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use dev::Pa;
use executor::Sleeper;

/// De event-stream mag hooguit zo lang zijn: 1,5 ms. De kleinste periode
/// eronder is op elk bekend board ~1 ms (zie [`event_stream`]).
pub const EVENT_STREAM_MAX_NS: u64 = 1_500_000;

/// De vangrail van de WFI-slaap zonder deadline: 10 ms.
///
/// Een SEV van een andere core wekt WFI niet (alleen WFE), en "geen timer"
/// als oneindig lezen is geen zuinigheid maar een hang (idle.go `wakeAt`):
/// zolang niets anders wekt, pollt de core dus op deze korrel.
pub const WFI_CAP_NS: u64 = 10_000_000;

/// Omrekenen van counter-ticks naar nanoseconden zonder overloop.
///
/// Seconden en rest apart: zelfs een uur maal 1 GHz loopt over als je eerst
/// vermenigvuldigt en dan deelt (idle.go `wakeAt`). `hz == 0` geeft 0.
#[must_use]
pub const fn ticks_to_ns(ticks: u64, hz: u64) -> u64 {
    if hz == 0 {
        return 0;
    }
    let secs = ticks / hz;
    let rest = ticks % hz;
    // `rest < hz <= u64::MAX`, dus `rest * 1e9` past in een u128.
    let frac = (rest as u128 * 1_000_000_000 / hz as u128) as u64;
    secs.saturating_mul(1_000_000_000).saturating_add(frac)
}

/// Omrekenen van nanoseconden naar counter-ticks, zelfde splitsing.
#[must_use]
pub const fn ns_to_ticks(ns: u64, hz: u64) -> u64 {
    let secs = ns / 1_000_000_000;
    let rest = ns % 1_000_000_000;
    let frac = (rest as u128 * hz as u128 / 1_000_000_000) as u64;
    secs.saturating_mul(hz).saturating_add(frac)
}

/// De CNTKCTL_EL1-waarde voor de event-stream: EVNTEN plus de EVNTI-bit
/// met de grootste periode die nog onder `max_period_ns` blijft.
///
/// EVNTI kiest de counterbit waarvan de 0-naar-1-flank het wek-event is;
/// de periode is 2^(EVNTI+1)/CNTFRQ. Bit 15 op de Pi's 54 MHz (1,2 ms) en
/// QEMU's 62,5 MHz (1,05 ms), bit 14 op de Altra's 25 MHz (1,3 ms; een
/// vaste 15 gaf daar 2,6 ms wek-granulariteit).
///
/// EVNTI is 4 bits, dus bit 15 is het plafond, en dat plafond is op een
/// GHz-teller te laag. GEMETEN 29-08 op de Mac mini M4 (CNTFRQ 1 GHz):
/// 2^16 ticks = 65 µs, vijftien keer vaker wakker dan bedoeld. FEAT_ECV
/// (`ecv`, ID_AA64MMFR0_EL1.ECV >= 1) heeft daar een schaalbit voor:
/// CNTKCTL.EVNTIS schuift de gekozen bit 8 posities op (x256), en die
/// zetten we zodra zelfs bit 15 onder een halve milliseconde uitkomt. Op
/// de M4 komt dit uit op EVNTI 11: 2^20 ticks = 1,048 ms = 954 wekken/s,
/// precies wat er gemeten is. Op elk board onder ~131 MHz verandert er
/// niets.
#[must_use]
pub const fn event_stream(hz: u64, ecv: bool, max_period_ns: u64) -> u64 {
    const EVNTEN: u64 = 1 << 2;
    const EVNTIS: u64 = 1 << 17;
    let shift: u32 = if ecv && (1u64 << 16) < hz / 2000 {
        8
    } else {
        0
    };
    let mut i: u32 = 15;
    while i > 4 && (1u128 << (i + 1 + shift)) * 1_000_000_000 > hz as u128 * max_period_ns as u128 {
        i -= 1;
    }
    let mut v = EVNTEN | ((i as u64) << 4);
    if shift != 0 {
        v |= EVNTIS;
    }
    v
}

/// De grens in ticks tussen "de WFE slikte alleen een verschaald event" en
/// "de core heeft echt geslapen": ~2 µs, met 64 ticks als bodem.
///
/// Het getal is TIJD, geen tikken: 64 ticks was 1-2,5 µs op de
/// 25-64 MHz-tellers waarvoor het geschreven werd, maar op de M4's 1 GHz 64
/// nanoseconden, minder dan de WFE zelf kost; de drain-lus hield dan op
/// vóór de core ooit sliep (gemeten 29-08: 3,6M wakes/s bij 33% "slaap").
#[must_use]
pub const fn wfe_min_sleep(hz: u64) -> u64 {
    let t = hz / 500_000;
    if t > 64 { t } else { 64 }
}

/// Monotone nanoseconden sinds het aanzetten van de teller: de
/// [`executor::Clock`] van deze architectuur.
///
/// CNTPCT_EL0 gedeeld door CNTFRQ_EL0. De drop naar EL1 zet CNTVOFF op 0
/// en geeft EL1 toegang tot de fysieke teller (drop.h), dus dit is dezelfde
/// stand als de CNTVCT die de EL2-switcher tegen de wektijden houdt.
#[must_use]
pub fn now() -> u64 {
    ticks_to_ns(arch::counter(), arch::freq())
}

/// De rauwe teller in ticks (CNTPCT_EL0).
#[must_use]
pub fn counter() -> u64 {
    arch::counter()
}

/// De eenheid van de teller: ticks per seconde (CNTFRQ_EL0).
///
/// LET OP QEMU-TCG: WFE is daar een no-op, dus idle-tijd meet er ~0;
/// idle-metingen zijn ijzer-metingen.
#[must_use]
pub fn freq() -> u64 {
    arch::freq()
}

/// Hoe een core slaapt als hij niets te doen heeft.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Mode {
    /// WFE, begrensd door de event-stream (~1 ms): de default, want hij
    /// wekt op elk silicium dat we kennen (behalve de M4, zie `Yield`).
    Wfe,
    /// WFI met de fysieke timer op de deadline. Alleen kiezen waar het board
    /// bewezen heeft dat de timer-PPI de WFI wekt: een slaap die niet wekt
    /// is een hang, en die hoort niet uit een redenering te komen.
    Wfi,
    /// HVC #1 naar de EL2-switcher, met de wektijd in x1. Voor een gedeelde
    /// core, en voor silicium waar een core op EL1 niet kan slapen (de M4,
    /// gemeten 02-09: WFE keert direct terug en geen FIQ wekt een WFI); de
    /// slaap gebeurt dan op EL2, waar hij wél werkt.
    Yield,
}

/// De meetlat van de slaap (handboek §4): zonder deze getallen is "de node
/// slaapt" niet te onderscheiden van "de node spint".
#[derive(Default, Debug)]
pub struct Stats {
    /// Geaccumuleerde idle-TIJD in ticks: de counterstand voor en na elke
    /// slaap, delta erbij. Tijd en geen rondes (herzien 18-07): WFE wekt
    /// ook op SEV's van andere cores en spurious events, en op de drukke
    /// Altra las elke deels-idle app daardoor als "vol idle" (DUTY
    /// 25/50/75 gaf allemaal cpu=0%). Een valse wek telt zo zijn echte
    /// microduur, geen volle tik.
    pub idle_ticks: AtomicU64,
    /// Aantal slaaprondes (de wek-teller van 31-07). Samen met de idle-tijd
    /// is de rekensom rond: tijd per wek = (1 - idle-fractie) / wekken-per-s.
    pub wakes: AtomicU64,
    /// Rondes waarin `ready()` al waar was toen de interrupts dicht waren:
    /// de verloren-wek-race die de deur dichthield.
    pub caught: AtomicU64,
}

/// De [`Sleeper`] van een arm64-core.
///
/// Eén per core, eigendom van de executor-lus van die core (`run` neemt
/// hem als `&mut`). De modus kiest het board, na zijn bewijs op dít
/// silicium; `shared` en de publicatie-adressen zijn de control-page-woorden
/// van een app (HOP's eigen core laat ze leeg).
pub struct ArmSleeper {
    mode: Mode,
    hz: u64,
    min_sleep: u64,
    shared: Option<Pa>,
    publish_idle: Option<Pa>,
    publish_wakes: Option<Pa>,
    /// De meetlat.
    pub stats: Stats,
}

impl ArmSleeper {
    /// Een sleeper in modus `mode`. Zet op deze core de event-stream aan
    /// (CNTKCTL is per core): zonder die stream wekt een WFE op een stille
    /// core nooit, en ook de EL2-switcher slaapt erop.
    #[must_use]
    pub fn new(mode: Mode) -> Self {
        let hz = arch::freq();
        arch::set_cntkctl(event_stream(hz, arch::has_ecv(), EVENT_STREAM_MAX_NS));
        Self {
            mode,
            hz,
            min_sleep: wfe_min_sleep(hz),
            shared: None,
            publish_idle: None,
            publish_wakes: None,
            stats: Stats::default(),
        }
    }

    /// Kijk elke ronde naar dit woord (CtrlShared): niet-nul betekent dat
    /// HOP deze core met een buur liet delen, en dan is idle een yield.
    pub fn watch_shared(&mut self, word: Pa) {
        self.shared = Some(word);
    }

    /// Publiceer de idle-tijd en de wek-teller ook op deze woorden
    /// (CtrlIdle, CtrlWakes op de eigen control-page). Eén 64-bit store per
    /// ronde, in dezelfde ronde; HOP leest ze, of niet.
    pub fn publish(&mut self, idle: Pa, wakes: Pa) {
        self.publish_idle = Some(idle);
        self.publish_wakes = Some(wakes);
    }

    /// Welke modus deze ronde geldt: een gedeelde core yieldt altijd.
    fn round_mode(&self) -> Mode {
        match self.shared {
            Some(w) if dev::read64(w) != 0 => Mode::Yield,
            _ => self.mode,
        }
    }

    /// WFE's tot er écht geslapen is, of tot `ready()`, of tot de deadline.
    ///
    /// De lus is nodig omdat het event-register vrijwel altijd vol zit als
    /// we hier komen: elke exclusive (onze eigen atomics) zet op de N1 een
    /// wek-event, en de eerste WFE keert daardoor per direct terug (GEMETEN
    /// 18-07 op de Altra: 4,7M wakes/s, slaap 0,0 µs). De herhaalde WFE
    /// slaapt wél. Tussen twee pogingen toetsen we `ready()`: een snelle
    /// terugkeer is meestal een verschaald event, maar soms de échte bel
    /// (een SEV van een andere core, een interrupt) en die werd tot 04-09
    /// weggeslikt, waarna de volgende WFE tot de event-stream-tik sliep: 6%
    /// van de system calls 1 ms in plaats van 20 µs. En de deadline: dat was
    /// de milliseconde op élke NIC-interrupt (O6N 18-09: rtt p50 1010 µs
    /// tegen 152 µs gepold).
    fn wfe_sleep(&self, deadline: Option<u64>, ready: &dyn Fn() -> bool) -> u64 {
        let mut slept = 0;
        let mut tries = 0;
        while slept < self.min_sleep && tries < 4 {
            slept += arch::wfe();
            tries += 1;
            if ready() || deadline.is_some_and(|d| arch::counter() >= d) {
                break;
            }
        }
        slept
    }

    /// De yield-wektijd: 0 = nu, anders de counterstand van de deadline,
    /// geklemd op een uur ("nooit" genoeg; een kick komt eerder).
    fn yield_deadline(&self, now_ns: u64, until: Option<u64>) -> u64 {
        const HOUR_NS: u64 = 3_600_000_000_000;
        let Some(u) = until else {
            return arch::counter().saturating_add(ns_to_ticks(HOUR_NS, self.hz));
        };
        let d = u.saturating_sub(now_ns).min(HOUR_NS);
        if d == 0 {
            return 0;
        }
        arch::counter().saturating_add(ns_to_ticks(d, self.hz))
    }

    fn account(&self, slept: u64) {
        let idle = self.stats.idle_ticks.fetch_add(slept, Relaxed) + slept;
        let wakes = self.stats.wakes.fetch_add(1, Relaxed) + 1;
        if let Some(pa) = self.publish_idle {
            dev::write64(pa, idle);
        }
        if let Some(pa) = self.publish_wakes {
            dev::write64(pa, wakes);
        }
    }
}

impl Sleeper for ArmSleeper {
    /// Maskeert, toetst `ready()`, slaapt, en zet de maskers terug zoals ze
    /// stonden.
    ///
    /// Terugzetten, niet blind openen: de IRQ-vector keert terug met I
    /// gemaskeerd zolang de dispatcher zijn lijn nog niet claimde (zie
    /// [`crate::irq`]); die opent I zelf weer.
    ///
    /// WFI wekt op een pending interrupt óók met I gemaskeerd; WFE niet (hij
    /// wekt alleen op een ongemaskeerde). Daarom opent de WFE-modus de
    /// maskers weer vóór de WFE, en is hij toch vrij van de verloren wek:
    /// een interrupt tussen de toets en de WFE eindigt in een exception
    /// return, en die zet het event-register, net als de SEV van
    /// `dev::notify` bij elke `Signal::set`. De eerstvolgende WFE valt er
    /// dan meteen doorheen.
    fn sleep(&mut self, now: u64, until: Option<u64>, ready: &dyn Fn() -> bool) {
        let daif = arch::mask();
        if ready() {
            arch::restore(daif);
            self.stats.caught.fetch_add(1, Relaxed);
            return;
        }
        let slept = match self.round_mode() {
            Mode::Wfe => {
                arch::restore(daif);
                let deadline = until.map(|u| deadline_ticks(now, u, self.hz));
                self.wfe_sleep(deadline, ready)
            }
            Mode::Wfi => {
                let cap = now.saturating_add(WFI_CAP_NS);
                let u = until.map_or(cap, |u| u.min(cap));
                let slept = arch::wfi_until(deadline_ticks(now, u, self.hz));
                arch::restore(daif);
                slept
            }
            Mode::Yield => {
                arch::restore(daif);
                arch::hvc_yield(self.yield_deadline(now, until))
            }
        };
        self.account(slept);
    }
}

/// De counterstand die bij tijdstip `until` (ns) hoort, gerekend vanaf nu.
fn deadline_ticks(now_ns: u64, until_ns: u64, hz: u64) -> u64 {
    arch::counter().saturating_add(ns_to_ticks(until_ns.saturating_sub(now_ns), hz))
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    //! De instructies. Elk blok is één of twee systeeminstructies zonder
    //! geheugeneffect buiten wat er staat.
    use core::arch::asm;

    #[inline]
    pub(super) fn counter() -> u64 {
        let v: u64;
        // SAFETY: CNTPCT_EL0 lezen heeft geen neveneffect; de drop naar EL1
        // zette CNTHCTL.EL1PCTEN, dus hij trapt niet.
        unsafe { asm!("isb", "mrs {}, cntpct_el0", out(reg) v, options(nomem, nostack)) };
        v
    }

    #[inline]
    pub(super) fn freq() -> u64 {
        let v: u64;
        // SAFETY: CNTFRQ_EL0 is een alleen-lezen-register voor EL1.
        unsafe { asm!("mrs {}, cntfrq_el0", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn has_ecv() -> bool {
        let v: u64;
        // SAFETY: een ID-register lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, id_aa64mmfr0_el1", out(reg) v, options(nomem, nostack)) };
        (v >> 60) & 0xF != 0
    }

    pub(super) fn set_cntkctl(v: u64) {
        // SAFETY: CNTKCTL_EL1 regelt alleen EL0-toegang en de event-stream
        // van deze core; de ISB maakt de stream meteen actief.
        unsafe { asm!("msr cntkctl_el1, {}", "isb", in(reg) v, options(nostack)) };
    }

    /// Maskeert I en F en geeft DAIF zoals het stond.
    #[inline]
    pub(super) fn mask() -> u64 {
        let v: u64;
        // SAFETY: DAIF lezen en maskeren raakt alleen PSTATE van deze core.
        // Geen `nomem`: de compiler mag geen geheugentoegang over het
        // maskeren heen schuiven.
        unsafe { asm!("mrs {}, daif", "msr daifset, #3", out(reg) v, options(nostack)) };
        v
    }

    #[inline]
    pub(super) fn restore(daif: u64) {
        // SAFETY: zet de maskers terug die `mask` las; niets anders.
        unsafe { asm!("msr daif, {}", in(reg) daif, options(nostack)) };
    }

    /// Eén WFE, met de counterstand eromheen: de écht in WFE doorgebrachte
    /// tijd in ticks.
    pub(super) fn wfe() -> u64 {
        let (a, b): (u64, u64);
        // SAFETY: WFE wacht op een event en heeft geen ander effect.
        unsafe {
            asm!(
                "mrs {a}, cntpct_el0",
                "wfe",
                "mrs {b}, cntpct_el0",
                a = out(reg) a,
                b = out(reg) b,
                options(nostack),
            );
        }
        b.wrapping_sub(a)
    }

    /// Zet de fysieke EL1-timer op `deadline`, doet één WFI en zet de timer
    /// weer uit (anders blijft hij pending). De interrupt wordt niet genomen
    /// (DAIF dicht) maar wekt WFI wél: dat belooft de architectuur voor
    /// WFI-wake-up events (board/apple wfiTimer, 02-09).
    pub(super) fn wfi_until(deadline: u64) -> u64 {
        let (a, b): (u64, u64);
        // SAFETY: alleen de eigen fysieke timer van deze core en een WFI;
        // de timer staat na afloop weer uit.
        unsafe {
            asm!(
                "mrs {a}, cntpct_el0",
                "msr cntp_cval_el0, {d}",
                "msr cntp_ctl_el0, {one}",
                "isb",
                "wfi",
                "msr cntp_ctl_el0, xzr",
                "isb",
                "mrs {b}, cntpct_el0",
                a = out(reg) a,
                b = out(reg) b,
                d = in(reg) deadline,
                one = in(reg) 1u64,
                options(nostack),
            );
        }
        b.wrapping_sub(a)
    }

    /// HVC #1 naar de EL2-switcher, met de wektijd in x1. Geeft de
    /// idle-wall-tijd (co-resident-runtijd plus slaap) in ticks.
    ///
    /// De switcher bewaart x0..x30 en het EL1-regime, maar GEEN FP: EL2
    /// draait met de MMU uit, en een SIMD-store naar Device faultt op ijzer
    /// (QEMU verhult dat). Op dit target (softfloat) bestaat er geen
    /// FP-staat om te bewaren; een build met FP moet d8..d15 en FPCR hier
    /// zelf om de HVC heen zetten, zoals idle_arm64.s dat deed.
    pub(super) fn hvc_yield(deadline: u64) -> u64 {
        let (a, b): (u64, u64);
        // SAFETY: de switcher hervat ons na de HVC met al onze
        // GP-registers en het EL1-regime intact (switch.rs `yield`); x0 en
        // x1 geven we als klad op.
        unsafe {
            asm!(
                "mrs x0, cntpct_el0",
                "hvc #1",
                "mrs {b}, cntpct_el0",
                "mov {a}, x0",
                a = out(reg) a,
                b = out(reg) b,
                inout("x1") deadline => _,
                out("x0") _,
                options(nostack),
            );
        }
        b.wrapping_sub(a)
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-stubs: geen teller, geen slaap. De rekenkunde hierboven is wat
    //! de host-tests bewijzen; de slaap zelf bewijst het board.
    pub(super) fn counter() -> u64 {
        0
    }
    pub(super) fn freq() -> u64 {
        62_500_000
    }
    pub(super) fn has_ecv() -> bool {
        false
    }
    pub(super) fn set_cntkctl(_v: u64) {}
    pub(super) fn mask() -> u64 {
        0
    }
    pub(super) fn restore(_daif: u64) {}
    pub(super) fn wfe() -> u64 {
        0
    }
    pub(super) fn wfi_until(_deadline: u64) -> u64 {
        0
    }
    pub(super) fn hvc_yield(_deadline: u64) -> u64 {
        0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn evnti(v: u64) -> u64 {
        (v >> 4) & 0xF
    }

    #[test]
    fn event_stream_per_board() {
        // Pi (54 MHz) en QEMU (62,5 MHz): bit 15, geen EVNTIS.
        let pi = event_stream(54_000_000, false, EVENT_STREAM_MAX_NS);
        assert_eq!((evnti(pi), pi & (1 << 17)), (15, 0));
        assert_eq!(
            evnti(event_stream(62_500_000, true, EVENT_STREAM_MAX_NS)),
            15
        );
        // Altra (25 MHz): bit 14.
        assert_eq!(
            evnti(event_stream(25_000_000, false, EVENT_STREAM_MAX_NS)),
            14
        );
        // M4 (1 GHz, FEAT_ECV): EVNTI 11 met EVNTIS = 2^20 ticks = 1,048 ms.
        let m4 = event_stream(1_000_000_000, true, EVENT_STREAM_MAX_NS);
        assert_eq!(evnti(m4), 11);
        assert_ne!(m4 & (1 << 17), 0);
        assert_ne!(m4 & (1 << 2), 0);
        // Zonder ECV blijft de M4 op het plafond steken: bit 15, 65 µs.
        assert_eq!(
            evnti(event_stream(1_000_000_000, false, EVENT_STREAM_MAX_NS)),
            15
        );
    }

    #[test]
    fn min_sleep_is_time_not_ticks() {
        assert_eq!(wfe_min_sleep(54_000_000), 108);
        assert_eq!(wfe_min_sleep(25_000_000), 64);
        assert_eq!(wfe_min_sleep(1_000_000_000), 2000);
    }

    #[test]
    fn conversions_do_not_overflow() {
        let hz = 1_000_000_000;
        let hour = 3_600_000_000_000;
        assert_eq!(ns_to_ticks(hour, hz), hour);
        assert_eq!(ticks_to_ns(ns_to_ticks(hour, 62_500_000), 62_500_000), hour);
        assert_eq!(ticks_to_ns(62_500_000, 62_500_000), 1_000_000_000);
        assert_eq!(ticks_to_ns(1, 62_500_000), 16);
        assert_eq!(ticks_to_ns(u64::MAX, 1), u64::MAX);
        assert_eq!(ticks_to_ns(5, 0), 0);
    }

    #[test]
    fn ready_before_sleep_is_caught() {
        let mut s = ArmSleeper::new(Mode::Wfe);
        s.sleep(0, None, &|| true);
        assert_eq!(s.stats.caught.load(Relaxed), 1);
        assert_eq!(s.stats.wakes.load(Relaxed), 0);
        s.sleep(0, Some(10), &|| false);
        assert_eq!(s.stats.wakes.load(Relaxed), 1);
    }

    #[test]
    fn shared_word_turns_idle_into_yield() {
        let mut word = [0u64; 1];
        let pa = Pa(word.as_mut_ptr() as usize as u64);
        let mut s = ArmSleeper::new(Mode::Wfe);
        s.watch_shared(pa);
        assert_eq!(s.round_mode(), Mode::Wfe);
        dev::write64(pa, 1);
        assert_eq!(s.round_mode(), Mode::Yield);
    }
}
