//! De rekentests: cpu (één core), smp (alle cores, met de gedeelde heap)
//! en burn (volgehouden last met de temperatuur).
//!
//! De werklast is de LCG-stap uit de soak van appspike: puur registerwerk,
//! geen geheugendruk, dus wat je meet is de klok van het hart. Elke burst
//! is 2^19 stappen (Go: ~0,3 ms op een A76) en eindigt in een `yield_now`:
//! een rekenlus in een app geeft coöperatief af, en zo blijven heartbeat,
//! de pomp en de pagina lopen terwijl we branden. Dezelfde burst als
//! `apps/bench` (BURN), zodat de getallen naast elkaar kunnen.
//!
//! Over meer cores (handboek §1, "cores onderling"): het werk gaat met
//! `smp::spawn_on` naar de executor van de andere core, de uitkomst komt
//! terug als atomics. Een taak op een secundaire logt niet en raakt het
//! `App` niet aan.
//!
//! Dit module bezit de tellers van smp en burn; ze gelden voor één run
//! tegelijk, en de dispatcher laat er maar één lopen.

use crate::Shared;
use crate::report::{Report, avg};
use crate::run::{Params, note};
use alloc::vec::Vec;
use applib::{EXEC, clock, smp};
use core::hint::black_box;
use core::sync::atomic::{
    AtomicBool, AtomicU64,
    Ordering::{Acquire, Relaxed, Release},
};
use core::time::Duration;
use sync::yield_now;

/// Eén burst: 2^19 LCG-stappen.
pub(crate) const BURST: u64 = 1 << 19;

/// De vermenigvuldiger van Knuth's MMIX-LCG, zoals in de Go-vitals.
const LCG_MUL: u64 = 6_364_136_223_846_793_005;

/// Hoe lang smp op de opgang van de andere cores wacht.
const CORES_WAIT: Duration = Duration::from_secs(5);

/// Hoe lang een run over alle cores hooguit duurt voor hij faalt.
const SMP_LIMIT: Duration = Duration::from_secs(120);

/// Het werk van de smp-vergelijking: 64M stappen, serieel en verdeeld.
const SMP_WORK: u64 = 64 << 20;

/// De woorden per core in de heap-toets.
const HEAP_WORDS: usize = 1 << 16;

/// `n` LCG-stappen vanaf `acc`. Niet inline, zodat de compiler de lus niet
/// in de meetlus vouwt; het resultaat gaat door `black_box`.
#[inline(never)]
pub(crate) fn lcg(mut acc: u64, n: u64) -> u64 {
    for k in 0..n {
        acc = acc.wrapping_mul(LCG_MUL).wrapping_add(k);
    }
    black_box(acc)
}

/// Nanoseconden als seconden, minstens een nanoseconde.
fn secs(ns: u64) -> f64 {
    ns.max(1) as f64 / 1e9
}

/// cpu: LCG-stappen per seconde op één core, met de yield die elke nette
/// app per burst betaalt.
pub(crate) async fn cpu(r: &mut Report, p: &Params) {
    let secs_want = Params::int(p.secs, 5, 1, 60);
    let t0 = clock::now_ns();
    let deadline = t0.saturating_add(secs_want * 1_000_000_000);
    let (mut acc, mut bursts) = (0u64, 0u64);
    while clock::now_ns() < deadline {
        acc = lcg(acc, BURST);
        bursts += 1;
        yield_now().await;
    }
    black_box(acc);
    let el = secs(clock::now_ns().saturating_sub(t0));
    let steps = (bursts * BURST) as f64;
    r.add("rate", steps / el / 1e6, "Msteps/s");
    r.add("burst", el / bursts.max(1) as f64 * 1e6, "us");
    r.line(format_args!(
        "{bursts} bursts of {BURST} LCG steps in {el:.2}s (one yield per burst)"
    ));
}

/// De tellers van een run over alle cores.
struct Tally {
    /// Klaar gemelde cores.
    done: AtomicU64,
    /// De laatste finish, in ns op de gedeelde teller.
    end_ns: AtomicU64,
    /// Bursts over alle cores (burn).
    bursts: AtomicU64,
    /// Stop (burn).
    stop: AtomicBool,
    /// Heap-toets: woorden die klopten en die niet klopten.
    good: AtomicU64,
    bad: AtomicU64,
    /// De uitkomsten, zodat de compiler niets weggooit.
    sink: AtomicU64,
}

/// De tellers; een run zet ze eerst op nul.
static TALLY: Tally = Tally {
    done: AtomicU64::new(0),
    end_ns: AtomicU64::new(0),
    bursts: AtomicU64::new(0),
    stop: AtomicBool::new(false),
    good: AtomicU64::new(0),
    bad: AtomicU64::new(0),
    sink: AtomicU64::new(0),
};

impl Tally {
    /// Alles op nul.
    fn reset(&self) {
        for a in [
            &self.done,
            &self.end_ns,
            &self.bursts,
            &self.good,
            &self.bad,
        ] {
            a.store(0, Relaxed);
        }
        self.stop.store(false, Release);
    }
}

/// Wacht tot alle `n` cores van de app draaien, hooguit [`CORES_WAIT`];
/// het aantal dat er is.
async fn cores_up(n: usize) -> usize {
    let t0 = clock::now_ns();
    let limit = u64::try_from(CORES_WAIT.as_nanos()).unwrap_or(u64::MAX);
    while (!smp::brought_up() || smp::online() < n) && clock::now_ns().saturating_sub(t0) < limit {
        EXEC.after(Duration::from_millis(5)).await;
    }
    smp::online().min(n)
}

/// Zet `fut` op core `k`: core 0 is de eigen executor, de rest via
/// `spawn_on`.
fn spawn_core(k: usize, fut: impl Future<Output = ()> + 'static) -> Result<(), smp::SmpError> {
    if k == 0 {
        EXEC.spawn(fut).map_err(smp::SmpError::Spawn)
    } else {
        smp::spawn_on(k, fut)
    }
}

/// Wacht tot `n` cores `done` meldden, hooguit `limit`; of dat lukte.
async fn all_done(n: u64, limit: Duration) -> bool {
    let t0 = clock::now_ns();
    let limit = u64::try_from(limit.as_nanos()).unwrap_or(u64::MAX);
    while TALLY.done.load(Acquire) < n {
        if clock::now_ns().saturating_sub(t0) > limit {
            return false;
        }
        EXEC.after(Duration::from_millis(1)).await;
    }
    true
}

/// Het deel van de heap-toets op core `k`: een blok vullen met een patroon
/// van deze core, en het blok als waarde naar core 0 sturen, die het leest.
/// Zo bewijst elke core dat wat hij in de gedeelde heap schreef, op een
/// andere core aankomt (dezelfde stage-1, dezelfde cache-orde).
async fn heap_part(k: usize) {
    let mut v: Vec<u32> = Vec::new();
    if v.try_reserve_exact(HEAP_WORDS).is_err() {
        TALLY.bad.fetch_add(HEAP_WORDS as u64, Relaxed);
        TALLY.done.fetch_add(1, Release);
        return;
    }
    let tag = u32::try_from(k).unwrap_or(0) << 24;
    v.extend((0..HEAP_WORDS).map(|i| tag | u32::try_from(i).unwrap_or(0)));
    let check = async move {
        let good = v
            .iter()
            .enumerate()
            .filter(|&(i, &w)| w == tag | u32::try_from(i).unwrap_or(0))
            .count() as u64;
        TALLY.good.fetch_add(good, Relaxed);
        TALLY.bad.fetch_add(HEAP_WORDS as u64 - good, Relaxed);
        TALLY.done.fetch_add(1, Release);
    };
    if smp::spawn_on(0, check).is_err() {
        TALLY.bad.fetch_add(HEAP_WORDS as u64, Relaxed);
        TALLY.done.fetch_add(1, Release);
    }
}

/// Het rekendeel op één core: `steps` stappen in bursts, dan de finish.
async fn work_part(steps: u64) {
    let mut acc = 0u64;
    let mut done = 0u64;
    while done < steps {
        acc = lcg(acc, BURST);
        done += BURST;
        yield_now().await;
    }
    TALLY.sink.fetch_add(acc, Relaxed);
    TALLY.end_ns.fetch_max(clock::now_ns(), Relaxed);
    TALLY.done.fetch_add(1, Release);
}

/// smp: dezelfde 64M stappen serieel op core 0 en verdeeld over alle
/// cores; de speedup is de maat. Eerst de heap-toets: gaat die mis, dan
/// is elk ander getal betekenisloos. Eén core: overslaan.
pub(crate) async fn smp(sh: &'static Shared, r: &mut Report) {
    let want = sh.app.cores();
    r.add("cores", want as f64, "");
    if want < 2 {
        r.skip("only 1 core assigned (give the job cpu_shares >= 2048)");
        return;
    }
    let n = cores_up(want).await;
    if n < want {
        r.fail(format_args!(
            "{n} of {want} cores came up (see HOPOS_APP_SMP_FAIL on the console)"
        ));
        return;
    }

    // De heap: elke core een blok, gelezen op core 0.
    TALLY.reset();
    for k in 0..n {
        if let Err(e) = spawn_core(k, heap_part(k)) {
            r.fail(format_args!("heap check on core {k}: {e}"));
            return;
        }
    }
    if !all_done(n as u64, SMP_LIMIT).await {
        r.fail(format_args!("heap check: not every core reported"));
        return;
    }
    let (good, bad) = (TALLY.good.load(Relaxed), TALLY.bad.load(Relaxed));
    if bad > 0 {
        r.fail(format_args!(
            "shared heap corrupt: {bad} of {} words wrong, SMP is broken on this board",
            good + bad
        ));
        return;
    }
    r.line(format_args!(
        "shared heap verified: {n} blocks of {HEAP_WORDS} words, each written on its own core and read on core 0"
    ));

    // Serieel op core 0.
    note(format_args!("smp serial"));
    let t1 = clock::now_ns();
    let mut acc = 0u64;
    let mut done = 0u64;
    while done < SMP_WORK {
        acc = lcg(acc, BURST);
        done += BURST;
        yield_now().await;
    }
    black_box(acc);
    let serial = clock::now_ns().saturating_sub(t1);

    // Verdeeld over alle cores.
    note(format_args!("smp parallel on {n} cores"));
    TALLY.reset();
    let t2 = clock::now_ns();
    let share = SMP_WORK / n as u64;
    for k in 0..n {
        if let Err(e) = spawn_core(k, work_part(share)) {
            r.fail(format_args!("work on core {k}: {e}"));
            return;
        }
    }
    if !all_done(n as u64, SMP_LIMIT).await {
        r.fail(format_args!("parallel run: not every core finished"));
        return;
    }
    let parallel = TALLY.end_ns.load(Relaxed).saturating_sub(t2);

    r.add("serial", serial as f64 / 1e6, "ms");
    r.add("parallel", parallel as f64 / 1e6, "ms");
    r.add("speedup", serial as f64 / parallel.max(1) as f64, "x");
    r.line(format_args!(
        "{}M LCG steps: serial {} ms, split over {n} cores {} ms",
        SMP_WORK >> 20,
        serial / 1_000_000,
        parallel / 1_000_000
    ));
}

/// De brander op één core: bursts tot de stop, elke burst geteld.
async fn burner() {
    let mut acc = 0u64;
    while !TALLY.stop.load(Acquire) {
        acc = lcg(acc, BURST);
        TALLY.bursts.fetch_add(1, Relaxed);
        yield_now().await;
    }
    TALLY.sink.fetch_add(acc, Relaxed);
    TALLY.done.fetch_add(1, Release);
}

/// Om de zoveel seconden een regel in het rapport en een op de console.
const BURN_LINE_EVERY: u64 = 5;
const BURN_LOG_EVERY: u64 = 10;

/// burn: alle cores branden `secs` lang (standaard 120), met per seconde
/// het tempo en de temperatuur van de kern (`CTRL_TEMP`, elke seconde door
/// de kern op de control-page gezet). Thermal throttling of een
/// dvfs-terugklok is dan een knik in de reeks.
pub(crate) async fn burn(sh: &'static Shared, r: &mut Report, p: &Params) {
    let secs_want = Params::int(p.secs, 120, 10, 600);
    let n = cores_up(sh.app.cores()).await.max(1);
    TALLY.reset();
    for k in 0..n {
        if let Err(e) = spawn_core(k, burner()) {
            TALLY.stop.store(true, Release);
            r.fail(format_args!("burner on core {k}: {e}"));
            return;
        }
    }
    let ctrl = sh.app.ctrl();
    let (mut first, mut last) = (Vec::new(), Vec::new());
    let _ = first.try_reserve_exact(10);
    let _ = last.try_reserve_exact(11);
    let mut max_temp = 0i32;
    let mut prev = TALLY.bursts.load(Relaxed);
    let mut prev_t = clock::now_ns();
    let mut next = prev_t;
    for t in 1..=secs_want {
        next = next.saturating_add(1_000_000_000);
        EXEC.until(next).await;
        let (cur, now) = (TALLY.bursts.load(Relaxed), clock::now_ns());
        let rate =
            cur.saturating_sub(prev) as f64 * BURST as f64 / secs(now.saturating_sub(prev_t)) / 1e6;
        (prev, prev_t) = (cur, now);
        let temp = ctrl.temp_milli_c();
        max_temp = max_temp.max(temp);
        let c = f64::from(temp) / 1000.0;
        if t % BURN_LINE_EVERY == 0 || t == 1 {
            if temp > 0 {
                r.line(format_args!("t={t:>3}s  {rate:8.1} Msteps/s  {c:5.1} C"));
            } else {
                r.line(format_args!("t={t:>3}s  {rate:8.1} Msteps/s"));
            }
        }
        if t % BURN_LOG_EVERY == 0 {
            applib::log!(
                "vitals: burn t={t}s of {secs_want}s on {n} core(s) HOPOS_VITALS_BURN_TICK rate={rate:.1} temp_c={c:.1}"
            );
            note(format_args!("burn {t}/{secs_want}s, {rate:.1} Msteps/s"));
        }
        // t=1 is een halve seconde opstart; de eerste tien daarna tellen.
        if t > 1 && first.len() < 10 && first.len() < first.capacity() {
            first.push(rate);
        }
        if last.len() == 10 {
            last.remove(0);
        }
        if last.len() < last.capacity() {
            last.push(rate);
        }
    }
    TALLY.stop.store(true, Release);
    let stopped = all_done(n as u64, Duration::from_secs(5)).await;
    r.add("cores", n as f64, "");
    r.add("rate_start", avg(&first), "Msteps/s");
    r.add("rate_end", avg(&last), "Msteps/s");
    if avg(&first) > 0.0 {
        r.add("degradation", (1.0 - avg(&last) / avg(&first)) * 100.0, "%");
    }
    if max_temp > 0 {
        r.add("temp_max", f64::from(max_temp) / 1000.0, "C");
    } else {
        r.line(format_args!(
            "no temperature: CTRL_TEMP is 0 (a board without a sensor, or no telemetry yet)"
        ));
    }
    if !stopped {
        r.line(format_args!("not every burner stopped within 5 s"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_lcg_is_the_go_step() {
        // Twee stappen met de hand: acc*a + 0, dan *a + 1.
        let a = LCG_MUL;
        let want = 7u64.wrapping_mul(a).wrapping_mul(a).wrapping_add(1);
        assert_eq!(lcg(7, 2), want);
        assert_eq!(lcg(7, 0), 7);
    }

    #[test]
    fn the_tally_resets_to_zero() {
        TALLY.done.store(3, Relaxed);
        TALLY.stop.store(true, Relaxed);
        TALLY.reset();
        assert_eq!(TALLY.done.load(Relaxed), 0);
        assert!(!TALLY.stop.load(Relaxed));
    }
}
