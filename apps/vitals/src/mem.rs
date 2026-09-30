//! De geheugentests en de kleine metingen: bandbreedte (membw), latentie
//! (memlat), het allocatietempo van de heap (alloc) en de overslaap van de
//! timer (timer).
//!
//! Op een nieuw board is het geheugen waar een verkeerde
//! DRAM-controller-config (timing, breedte, frequentie) als eerste
//! zichtbaar wordt: bandbreedte ver onder spec, of een latentietrap die
//! niet bij de cache-hiërarchie past. De timer-test is klein maar
//! board-gevoelig: de timerbron, de event-stream en de slaap van de
//! app-core zitten er allemaal in.
//!
//! Alle buffers komen uit de heap met `try_reserve`: een partitie die te
//! klein is, geeft een fout in het rapport, geen paniek.

use crate::Shared;
use crate::report::{Report, pct};
use crate::run::{Params, note};
use alloc::vec::Vec;
use applib::heap::HEAP;
use applib::{EXEC, clock};
use core::hint::black_box;
use core::time::Duration;
use sync::yield_now;

/// De bovengrens van een bandbreedtebuffer: ruim boven elke L2.
const BW_MAX: usize = 16 << 20;

/// Hoe lang copy en triad elk meten.
const BW_TIME_NS: u64 = 700_000_000;

/// De working-sets van memlat.
const LAT_SIZES: [(usize, &str); 5] = [
    (32 << 10, "ns_32k"),
    (128 << 10, "ns_128k"),
    (512 << 10, "ns_512k"),
    (2 << 20, "ns_2m"),
    (8 << 20, "ns_8m"),
];

/// Zoveel meettijd per working-set.
const LAT_TIME_NS: u64 = 80_000_000;

/// Nanoseconden als seconden, minstens een nanoseconde.
fn secs(ns: u64) -> f64 {
    ns.max(1) as f64 / 1e9
}

/// Een buffer van `n` elementen `v`, of `None` als de heap nee zei.
fn buffer<T: Copy>(n: usize, v: T) -> Option<Vec<T>> {
    let mut b = Vec::new();
    b.try_reserve_exact(n).ok()?;
    b.resize(n, v);
    Some(b)
}

/// De maat van een bandbreedtebuffer: [`BW_MAX`], of een achtste van de
/// partitie als die kleiner is (de triad heeft er drie tegelijk).
fn bw_size(ram: u64) -> usize {
    let eighth = usize::try_from(ram / 8).unwrap_or(BW_MAX);
    if eighth > 0 {
        eighth.min(BW_MAX)
    } else {
        BW_MAX
    }
}

/// membw: STREAM-achtig. Copy (een grote `copy_from_slice`) en triad
/// (`a = b + 3c`), met elke gelezen én geschreven byte geteld, zoals
/// STREAM.
///
/// De triad rekent op `u64` in plaats van `f64`: het target van de apps is
/// `aarch64-unknown-none-softfloat`, en een `f64`-optelling is daar een
/// bibliotheekaanroep. Dan zou de triad de softfloat meten, niet het
/// geheugen. De bytes per element blijven 24, dus het getal staat naast dat
/// van Go.
pub(crate) async fn bandwidth(sh: &'static Shared, r: &mut Report) {
    let size = bw_size(sh.app.ram_size());
    r.add("buffer", (size >> 20) as f64, "MB");
    let (Some(mut src), Some(mut dst)) = (buffer(size, 0u8), buffer(size, 0u8)) else {
        r.fail(format_args!(
            "no heap for two buffers of {} MB (give the job more memory_limit)",
            size >> 20
        ));
        return;
    };
    for (i, b) in src.iter_mut().enumerate() {
        *b = i.to_le_bytes()[0];
    }
    let (mut passes, t0) = (0u64, clock::now_ns());
    while clock::now_ns().saturating_sub(t0) < BW_TIME_NS {
        dst.copy_from_slice(black_box(&src));
        black_box(&mut dst);
        passes += 1;
        yield_now().await;
    }
    let el = secs(clock::now_ns().saturating_sub(t0));
    r.add("copy", passes as f64 * size as f64 * 2.0 / el / 1e9, "GB/s");
    r.line(format_args!(
        "copy: {passes} passes of {} MB in {el:.2}s",
        size >> 20
    ));
    drop((src, dst));

    let n = size / 8;
    let (Some(mut a), Some(mut b), Some(mut c)) =
        (buffer(n, 0u64), buffer(n, 0u64), buffer(n, 0u64))
    else {
        r.fail(format_args!(
            "no heap for three triad arrays of {} MB",
            size >> 20
        ));
        return;
    };
    for (i, (x, y)) in b.iter_mut().zip(c.iter_mut()).enumerate() {
        *x = i as u64;
        *y = (n - i) as u64;
    }
    let (mut passes, t0) = (0u64, clock::now_ns());
    while clock::now_ns().saturating_sub(t0) < BW_TIME_NS {
        triad(&mut a, black_box(&b), black_box(&c));
        passes += 1;
        yield_now().await;
    }
    let el = secs(clock::now_ns().saturating_sub(t0));
    black_box(a.last());
    r.add("triad", passes as f64 * n as f64 * 24.0 / el / 1e9, "GB/s");
    r.line(format_args!(
        "triad (u64): {passes} passes of {n} elements in {el:.2}s"
    ));
}

/// `a[i] = b[i] + 3 * c[i]`: twee lezingen en een schrijf van 8 bytes per
/// element.
#[inline(never)]
fn triad(a: &mut [u64], b: &[u64], c: &[u64]) {
    for ((x, &y), &z) in a.iter_mut().zip(b).zip(c) {
        *x = y.wrapping_add(z.wrapping_mul(3));
    }
}

/// Een Sattolo-permutatie van `0..n`: één cykel door alle elementen, in
/// LCG-volgorde. Deterministisch tussen runs, zonder willekeur.
pub(crate) fn sattolo(p: &mut [u32]) {
    for (i, x) in p.iter_mut().enumerate() {
        *x = u32::try_from(i).unwrap_or(0);
    }
    let mut r: u64 = 88_172_645_463_325_252;
    for i in (1..p.len()).rev() {
        r = r
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        let j = usize::try_from(r % i as u64).unwrap_or(0);
        p.swap(i, j);
    }
}

/// `steps` sprongen door `p` vanaf `cur`: elke lading hangt van de vorige
/// af, dus prefetchers en out-of-order helpen niet.
#[inline(never)]
pub(crate) fn chase(p: &[u32], mut cur: u32, steps: u64) -> u32 {
    for _ in 0..steps {
        cur = p.get(cur as usize).copied().unwrap_or(0);
    }
    black_box(cur)
}

/// memlat: de laadlatentie met een pointer-chase over oplopende
/// working-sets. Ze tekenen de cache-trap: L1, L2, en daarboven DRAM.
///
/// De rekensom van Go blijft staan: een set van `size` heeft `size / 8`
/// woorden van 4 bytes (de helft van het label). Zo liggen de getallen
/// naast die van de Go-generatie.
pub(crate) async fn latency(sh: &'static Shared, r: &mut Report) {
    let max = usize::try_from(sh.app.ram_size() / 8).unwrap_or(usize::MAX);
    let mut furthest = 0.0;
    for (size, name) in LAT_SIZES {
        if max > 0 && size > max {
            break;
        }
        note(format_args!("memlat {} KB", size >> 10));
        let Some(mut p) = buffer(size / 8, 0u32) else {
            r.fail(format_args!(
                "no heap for a working set of {} KB",
                size >> 10
            ));
            return;
        };
        sattolo(&mut p);
        // Het aantal stappen oplopend tot de meting ~80 ms duurt.
        let (mut steps, mut cur) = (1u64 << 16, 0u32);
        let el = loop {
            let t0 = clock::now_ns();
            cur = chase(&p, cur, steps);
            let el = clock::now_ns().saturating_sub(t0);
            if el > LAT_TIME_NS || steps >= 1 << 26 {
                break el;
            }
            steps *= 4;
            yield_now().await;
        };
        black_box(cur);
        let ns = el as f64 / steps as f64;
        furthest = ns;
        r.add(name, ns, "ns/load");
        r.line(format_args!(
            "{:6} KB: {ns:6.1} ns/load ({steps} steps)",
            size >> 10
        ));
        yield_now().await;
    }
    r.add("furthest", furthest, "ns/load");
}

/// De maat van één alloc-blok: 32 KiB, zoals Go's gc-test.
const ALLOC_BLOB: usize = 32 << 10;

/// Zoveel blokken blijven leven (~2 MB); de rest gaat meteen terug.
const ALLOC_LIVE: usize = 64;

/// alloc: het allocatietempo van de heap, de plaats van Go's gc-test. Er
/// is geen collector meer; wat blijft is wat de allocator per seconde
/// aankan met een levende set van ~2 MB, of hij versnippert (de grootste
/// vrije brok na afloop) en of een weigering een weigering blijft.
pub(crate) async fn alloc_rate(r: &mut Report, p: &Params) {
    let secs_want = Params::int(p.secs, 3, 1, 30);
    let before = HEAP.stats();
    let mut live: [Option<Vec<u8>>; ALLOC_LIVE] = [const { None }; ALLOC_LIVE];
    let (mut allocs, mut failed) = (0u64, 0u64);
    let t0 = clock::now_ns();
    let deadline = t0.saturating_add(secs_want * 1_000_000_000);
    while clock::now_ns() < deadline {
        let slot = live.get_mut((allocs as usize) % ALLOC_LIVE);
        match buffer(ALLOC_BLOB, 0u8) {
            Some(mut b) => {
                if let Some(x) = b.first_mut() {
                    *x = allocs.to_le_bytes()[0];
                }
                if let Some(s) = slot {
                    *s = Some(b);
                }
            }
            None => failed += 1,
        }
        allocs += 1;
        if allocs.is_multiple_of(64) {
            yield_now().await;
        }
    }
    let el = secs(clock::now_ns().saturating_sub(t0));
    let during = HEAP.stats();
    drop(live);
    let after = HEAP.stats();
    let largest = HEAP.check().map(|w| w.largest_free).unwrap_or(0);
    r.add(
        "alloc",
        allocs as f64 * ALLOC_BLOB as f64 / el / 1e6,
        "MB/s",
    );
    r.add("allocs_per_s", allocs as f64 / el, "1/s");
    r.add("failed", failed as f64, "");
    r.add("heap_peak", (during.peak >> 10) as f64, "KB");
    r.line(format_args!(
        "{allocs} allocations of {} KB in {el:.2}s, {failed} refused",
        ALLOC_BLOB >> 10
    ));
    r.line(format_args!(
        "heap before {} KB, during {} KB, after {} KB of {} KB; largest free block {} KB",
        before.used >> 10,
        during.used >> 10,
        after.used >> 10,
        after.capacity >> 10,
        largest >> 10
    ));
}

/// De doelen van de timer-test.
const TIMER_TARGETS: [(u64, &str, &str); 3] = [
    (1, "oversleep_1ms_p50", "oversleep_1ms_p99"),
    (5, "oversleep_5ms_p50", "oversleep_5ms_p99"),
    (20, "oversleep_20ms_p50", "oversleep_20ms_p99"),
];

/// Zoveel slaapjes per doel.
const TIMER_ROUNDS: usize = 40;

/// timer: vraag veertig keer een korte slaap en kijk hoeveel te laat je
/// wakker wordt. Dat overschot is de optelsom van timerbron, event-stream,
/// de slaap van de app-core en de executor; op een gedeelde core zit de
/// buurman erin.
pub(crate) async fn timer(r: &mut Report) {
    for (ms, p50, p99) in TIMER_TARGETS {
        let mut over = [0u32; TIMER_ROUNDS];
        for o in &mut over {
            let t0 = clock::now_ns();
            EXEC.after(Duration::from_millis(ms)).await;
            let late = clock::now_ns()
                .saturating_sub(t0)
                .saturating_sub(ms * 1_000_000);
            *o = u32::try_from(late / 1000).unwrap_or(u32::MAX);
        }
        let (a, b, m) = (pct(&mut over, 50), pct(&mut over, 99), pct(&mut over, 100));
        r.line(format_args!(
            "sleep {ms:>2} ms: median +{a} us, p99 +{b} us, max +{m} us"
        ));
        r.add(p50, f64::from(a), "us");
        if ms == 1 {
            r.add(p99, f64::from(b), "us");
        }
    }
    r.line(format_args!(
        "oversleep = actual - requested; includes timer source, idle and executor"
    ));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sattolo_is_one_cycle_through_every_element() {
        let mut p = vec![0u32; 1000];
        sattolo(&mut p);
        let (mut cur, mut seen) = (0u32, vec![false; 1000]);
        for _ in 0..1000 {
            assert!(!seen[cur as usize], "short cycle at {cur}");
            seen[cur as usize] = true;
            cur = p[cur as usize];
        }
        assert_eq!(cur, 0);
        assert!(seen.iter().all(|&s| s));
        // Een volle ronde brengt de chase terug waar hij begon.
        assert_eq!(chase(&p, 0, 1000), 0);
        assert_ne!(chase(&p, 0, 999), 0);
    }

    #[test]
    fn the_triad_is_b_plus_three_c() {
        let mut a = [0u64; 4];
        triad(&mut a, &[1, 2, 3, 4], &[10, 20, 30, 40]);
        assert_eq!(a, [31, 62, 93, 124]);
    }

    #[test]
    fn the_bandwidth_buffer_is_an_eighth_of_the_partition_at_most() {
        assert_eq!(bw_size(128 << 20), 16 << 20);
        assert_eq!(bw_size(1 << 30), 16 << 20);
        assert_eq!(bw_size(64 << 20), 8 << 20);
        assert_eq!(bw_size(0), 16 << 20);
    }
}
