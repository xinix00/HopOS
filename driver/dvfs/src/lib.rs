//! Het klokbeleid van HopOS: een OS-taak, geen Hop-taak. De orchestrator
//! is er volledig blind voor, zoals bij SMP. Vastgelegd met Derek in
//! `docs/archief/plan-p2b-soak.md` (2026-07-11), geport uit
//! `OLD/metal/driver/dvfs`:
//!
//! - het signaal is de idle-teller: idle-tijd in generic-timer-tikken. Een
//!   idle core telt ~CNTFRQ per seconde op, een drukke staat stil. Apps
//!   publiceren hem op hun control-page, de kern-core telt zelf;
//! - de wachter sampelt elke ~10 ms en oordeelt over de laatste 50 ms:
//!   íéts onder tempo, dan de klok vol (~20 ms bij aanhoudende last);
//!   álles ~30 s op tempo, dan de klok laag;
//! - de knop ([`Knob`]) alleen op de flank: op de Pi de firmware-mailbox,
//!   op de O6N het `_CPC`-fastchannel per domein; de firmware-throttle
//!   blijft het vangnet.
//!
//! Dit crate is het beleid als rekenwerk: [`Governor::step`] krijgt de
//! tellers van één sample en zegt of de knop om moet. De taak eromheen is
//! [`run`]: een lus op de slaap van de executor die de tellers leest (de
//! binary geeft ze), de flanken en elke 10 s een meetregel logt. De knop is
//! van het board. Zo is het hele beleid op de host te toetsen, ook de lus.

#![cfg_attr(not(test), no_std)]
#![forbid(unsafe_code)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

use core::fmt;
use core::future::Future;

/// De sample-tijd.
pub const SAMPLE_NS: u64 = 10_000_000;
/// Samples per oordeel (50 ms). Eén sample van 10 ms was het oordeel tot
/// 23-09, en dat hield de O6N voorgoed vol: een app die 99,7% idle is,
/// heeft toch losse samples van 0-60% idle (klusjes van ~1 ms die op de
/// stille klok 3,25 keer langer duren, en een teller die pas bij terugkomst
/// uit de yield bijloopt en dus in klonters binnenkomt). Over 50 ms middelt
/// dat weg; echte aanhoudende last haalt de grens na twee volle samples.
pub const WINDOW: usize = 5;
/// De hysterese omlaag.
pub const COOLDOWN_NS: u64 = 30_000_000_000;
/// Een bron is druk onder 70% van het verwachte tempo: ruim onder de
/// jitter van de event-stream, ruim boven "half werk".
const BUSY_NUM: u64 = 7;
const BUSY_DEN: u64 = 10;
/// Een klonter van een lange slaap telt hoogstens voor vier samples.
const CLAMP: u64 = 4;

/// Eén stand van de knop, voor de logregel ("2600 MHz", "8192 perf").
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Level {
    /// De waarde.
    pub value: u32,
    /// De eenheid.
    pub unit: &'static str,
}

impl fmt::Display for Level {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} {}", self.value, self.unit)
    }
}

/// De klok van één board, teruggebracht tot wat het beleid kent: twee
/// standen. `None` is mislukt: het beleid blijft dan staan waar het stond.
pub trait Knob {
    /// Naar het plafond.
    fn full(&mut self) -> Option<Level>;
    /// Naar de stil-stand.
    fn quiet(&mut self) -> Option<Level>;
}

impl<K: Knob + ?Sized> Knob for &mut K {
    fn full(&mut self) -> Option<Level> {
        (**self).full()
    }
    fn quiet(&mut self) -> Option<Level> {
        (**self).quiet()
    }
}

/// Een console-pin: `clock full|quiet|auto`, zodat één kern beide standen
/// kan meten zonder flip. Geen config-sleutel: een pin hoort niet in een
/// node die draait zonder dat iemand kijkt.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum Hold {
    /// Het beleid beslist.
    #[default]
    Auto,
    /// Vast vol.
    Full,
    /// Vast stil.
    Quiet,
}

/// Eén bron in één sample. Bron 0 is de kern-core; de rest zijn app-slots.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Sample {
    /// Draait er iets (een slot met status Ready en cores > 0)?
    pub live: bool,
    /// De idle-teller (monotoon, in timer-tikken).
    pub idle: u64,
    /// Het aantal cores van de bron: het verwachte tempo schaalt mee.
    pub cores: u64,
    /// Rekent de bron nu (niet geyield)? Een teller die niet steeg, telt
    /// alleen als druk als dit waar is: op een yield-idle-board (O6N) slaapt
    /// een stille app tientallen ms in zijn yield, en zonder deze vraag las
    /// elke slapende app als 100% bezig en zakte de klok nooit.
    pub running: bool,
}

/// Wat de governor deed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Change {
    /// Naar vol (`true`) of stil.
    pub full: bool,
    /// Waarom ("boot", "busy", "idle 30s", "held").
    pub why: &'static str,
    /// Wat de knop meldde; `None` = de knop weigerde, de stand bleef.
    pub level: Option<Level>,
}

/// De laatste drukke bron, voor de console: een governor die niet zakt,
/// moet kunnen zeggen wie hem wakker houdt.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Busy {
    /// De bron (0 = kern-core, n = slot n).
    pub source: usize,
    /// Wanneer.
    pub at_ns: u64,
    /// Idle in promille van het verwachte tempo.
    pub idle_permille: u64,
}

/// Het glijdende venster van één bron: idle-tikken en verwacht tempo van
/// de laatste [`WINDOW`] samples.
#[derive(Clone, Copy, Debug, Default)]
struct History {
    d: [u64; WINDOW],
    want: [u64; WINDOW],
    k: usize,
    full: bool,
}

impl History {
    /// Schuift een sample in het venster en geeft de sommen. Tot het venster
    /// vol is, telt alleen wat er is: een verse app wordt meteen beoordeeld.
    fn add(&mut self, d: u64, want: u64) -> (u64, u64) {
        if let (Some(sd), Some(sw)) = (self.d.get_mut(self.k), self.want.get_mut(self.k)) {
            *sd = d.min(want.saturating_mul(CLAMP));
            *sw = want;
        }
        self.k = (self.k + 1) % WINDOW;
        if self.k == 0 {
            self.full = true;
        }
        let n = if self.full { WINDOW } else { self.k };
        let sd = self.d.iter().take(n).sum();
        let sw = self.want.iter().take(n).sum();
        (sd, sw)
    }
}

/// Het beleid over `N` bronnen (bron 0 is de kern-core).
pub struct Governor<const N: usize> {
    hist: [History; N],
    last: [u64; N],
    seen: [bool; N],
    high: bool,
    quiet_since: u64,
    /// De console-pin.
    pub hold: Hold,
    /// De laatste drukke bron.
    pub busy: Option<Busy>,
}

impl<const N: usize> Default for Governor<N> {
    fn default() -> Self {
        Self::new()
    }
}

impl<const N: usize> Governor<N> {
    /// Een governor die nog niets weet.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            hist: [History {
                d: [0; WINDOW],
                want: [0; WINDOW],
                k: 0,
                full: false,
            }; N],
            last: [0; N],
            seen: [false; N],
            high: false,
            quiet_since: 0,
            hold: Hold::Auto,
            busy: None,
        }
    }

    /// Staat de klok vol?
    #[must_use]
    pub fn is_full(&self) -> bool {
        self.high
    }

    /// De eerste stap: de toestand niet aannemen maar zetten. Gemeten
    /// 2026-07-11: met een `arm_freq_min`-vloer boot de firmware op de vloer,
    /// niet op vol, en de hele P1-acceptatie draaide per ongeluk op 800 MHz.
    /// Boot-werk verdient de volle klok; daarna regeert het beleid.
    pub fn boot(&mut self, now: u64, knob: &mut impl Knob) -> Change {
        let level = knob.full();
        self.high = level.is_some();
        self.quiet_since = now;
        Change {
            full: true,
            why: "boot",
            level,
        }
    }

    /// Eén sample: `expect` is het verwachte idle-tempo per core per sample
    /// (CNTFRQ maal [`SAMPLE_NS`]), `samples[i]` de tellers van bron `i`.
    /// Geeft de flank als de knop om moest.
    pub fn step(
        &mut self,
        now: u64,
        expect: u64,
        samples: &[Sample; N],
        knob: &mut impl Knob,
    ) -> Option<Change> {
        let busy = self.measure(now, expect, samples);
        match self.hold {
            Hold::Auto => {}
            h => {
                // Loslaten begint niet meteen met "idle 30s".
                self.quiet_since = now;
                let want = h == Hold::Full;
                return (want != self.high).then(|| self.set(want, "held", knob));
            }
        }
        if busy {
            // Anders valt de klok één stil sample later alweer terug ("idle
            // 30s" één tel na "busy", gemeten 19-07).
            self.quiet_since = now;
            return (!self.high).then(|| self.set(true, "busy", knob));
        }
        if self.high && now.saturating_sub(self.quiet_since) > COOLDOWN_NS {
            return Some(self.set(false, "idle 30s", knob));
        }
        None
    }

    fn set(&mut self, full: bool, why: &'static str, knob: &mut impl Knob) -> Change {
        let level = if full { knob.full() } else { knob.quiet() };
        if level.is_some() {
            self.high = full;
        }
        Change { full, why, level }
    }

    /// Leest de bronnen in de vensters; `true` als er één druk is.
    fn measure(&mut self, now: u64, expect: u64, samples: &[Sample; N]) -> bool {
        let mut busy = false;
        for (i, s) in samples.iter().enumerate() {
            let (Some(h), Some(last), Some(seen)) = (
                self.hist.get_mut(i),
                self.last.get_mut(i),
                self.seen.get_mut(i),
            ) else {
                continue;
            };
            if !s.live || s.cores == 0 {
                *seen = false;
                continue;
            }
            let want = expect.saturating_mul(s.cores);
            // Het eerste sample na een start ijkt alleen; daarna telt óók een
            // teller die op nul blijft (een app die vanaf seconde één 100%
            // brandt) als druk.
            if !*seen {
                *h = History::default();
            } else {
                let mut d = s.idle.wrapping_sub(*last);
                if !s.running {
                    d = d.max(want); // slaapt in zijn yield: dit sample was idle
                }
                let (sd, sw) = h.add(d, want);
                if sd.saturating_mul(BUSY_DEN) < sw.saturating_mul(BUSY_NUM) {
                    busy = true;
                    self.busy = Some(Busy {
                        source: i,
                        at_ns: now,
                        idle_permille: sd.saturating_mul(1000) / sw.max(1),
                    });
                }
            }
            *seen = true;
            *last = s.idle;
        }
        busy
    }
}

/// Om de hoeveel tijd [`run`] zijn meetregel logt.
pub const REPORT_NS: u64 = 10_000_000_000;

/// Wat [`run`] van de binary nodig heeft buiten de knop: de klok, de slaap
/// en de tellers.
pub trait Host<const N: usize> {
    /// Monotone nanoseconden.
    fn now(&self) -> u64;
    /// Slaapt `ns` nanoseconden (de timer van de executor).
    fn sleep(&self, ns: u64) -> impl Future<Output = ()>;
    /// Het verwachte idle-tempo per core per sample (CNTFRQ maal
    /// [`SAMPLE_NS`]).
    fn expect(&self) -> u64;
    /// Vult de tellers van dit sample.
    fn sample(&mut self, out: &mut [Sample; N]);
    /// De temperatuur voor de meetregel, in milligraden; 0 = geen.
    fn temp_milli_c(&mut self) -> i32;
    /// Eén Engelse logregel.
    fn log(&self, args: fmt::Arguments<'_>);
}

/// De governor als taak: eerst de klok vol ("boot"), dan elke
/// [`SAMPLE_NS`] een sample en een oordeel over het venster van 50 ms, de
/// flank naar stil na [`COOLDOWN_NS`], en elke [`REPORT_NS`] één
/// meetregel (`HOPOS_CLOCK`). `hold` is de pin uit de config
/// (`hopos.clock`). Keert nooit terug.
pub async fn run<const N: usize>(mut knob: impl Knob, hold: Hold, host: &mut impl Host<N>) {
    let mut g: Governor<N> = Governor::new();
    g.hold = hold;
    let c = g.boot(host.now(), &mut knob);
    report_change(host, &c);
    run_governor(g, knob, host, c.level).await;
}

/// Als [`run`], maar de boot-flank is al gezet: de aanroeper zette de klok
/// vol met `Knob::full` vóór het net (de Pi's, hopos/src/telemetry.rs), en
/// `level` is wat de knop toen meldde. Zo boot een geflipte kern zijn NIC
/// op de volle klok, zoals een koude boot, ook als de vorige kern stil
/// stond (30-09: twee flips vanuit 800 MHz met een NIC die nooit meer
/// meldde).
pub async fn run_after_boot<const N: usize>(
    knob: impl Knob,
    hold: Hold,
    host: &mut impl Host<N>,
    level: Option<Level>,
) {
    let mut g: Governor<N> = Governor::new();
    g.hold = hold;
    g.high = level.is_some();
    g.quiet_since = host.now();
    run_governor(g, knob, host, level).await;
}

/// De lus van de governor na de boot-flank.
async fn run_governor<const N: usize>(
    mut g: Governor<N>,
    mut knob: impl Knob,
    host: &mut impl Host<N>,
    level: Option<Level>,
) {
    let mut samples = [Sample::default(); N];
    let mut last_report = host.now();
    let mut level = level;
    loop {
        host.sleep(SAMPLE_NS).await;
        let now = host.now();
        host.sample(&mut samples);
        if let Some(c) = g.step(now, host.expect(), &samples, &mut knob) {
            report_change(host, &c);
            if c.level.is_some() {
                level = c.level;
            }
        }
        if now.saturating_sub(last_report) >= REPORT_NS {
            last_report = now;
            let t = host.temp_milli_c();
            let busy = g.busy.filter(|b| now.saturating_sub(b.at_ns) < REPORT_NS);
            host.log(format_args!(
                "dvfs: clock {} ({}), temp {}.{} C, busy {} HOPOS_CLOCK",
                level.map_or(
                    Level {
                        value: 0,
                        unit: "?"
                    },
                    |l| l
                ),
                if g.is_full() { "full" } else { "quiet" },
                t / 1000,
                (t % 1000).abs() / 100,
                Busy::describe(busy),
            ));
        }
    }
}

fn report_change<const N: usize>(host: &impl Host<N>, c: &Change) {
    match c.level {
        Some(l) => host.log(format_args!(
            "dvfs: -> {l} ({}, {}) HOPOS_CLOCK_EDGE",
            if c.full { "full" } else { "quiet" },
            c.why
        )),
        None => host.log(format_args!(
            "dvfs: clock change to {} ({}) failed, the policy keeps its state",
            if c.full { "full" } else { "quiet" },
            c.why
        )),
    }
}

impl Busy {
    /// "none", of de bron en zijn idle.
    fn describe(b: Option<Busy>) -> BusyText {
        BusyText(b)
    }
}

/// [`Busy`] als tekst voor de meetregel.
struct BusyText(Option<Busy>);

impl fmt::Display for BusyText {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.0 {
            None => f.write_str("none"),
            Some(b) if b.source == 0 => {
                write!(f, "kern core ({} permille idle)", b.idle_permille)
            }
            Some(b) => write!(f, "slot {} ({} permille idle)", b.source, b.idle_permille),
        }
    }
}

/// De pin uit `hopos.clock`: `dvfs` (of niets) is het beleid, `max` vast
/// vol, `quiet` vast stil, `firmware` geen governor (de boot-OPP blijft).
/// `None` = firmware; een onbekende waarde is het beleid (de aanroeper
/// meldt hem).
#[must_use]
pub fn hold_of(v: &str) -> (Option<Hold>, bool) {
    match v {
        "" | "dvfs" | "auto" => (Some(Hold::Auto), true),
        "max" | "full" => (Some(Hold::Full), true),
        "quiet" | "min" => (Some(Hold::Quiet), true),
        "firmware" | "off" => (None, true),
        _ => (Some(Hold::Auto), false),
    }
}

#[cfg(test)]
mod tests;
