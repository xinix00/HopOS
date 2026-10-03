//! Tijdelijke meetlat (feature `hopcost`, 03-10): wat een hop tussen twee
//! bewoners van de OS-core kost, in vijf fasen op de architectuurteller
//! (CNTPCT op arm64, TIME op riscv64). Een hop van A naar B:
//!
//! - a: de trap van A (zijn kick of yield, de eerste instructie van de
//!   terugweg) tot de kern terug is in Rust, na `settle`;
//! - b: van daar tot de switch een frame in een RX-ring schreef en belde
//!   (`slot_wake`); een hop zonder bel heeft b = 0;
//! - c: van de bel (of van a, zonder bel) tot de rotatie B kiest: de rest
//!   van de executor-ronde, de slaper, [`crate::el2::next`];
//! - d: de wissel naar B: de boekhouding van de beurt, tabellen, TLB en
//!   registers, tot vlak voor de `mret`/`eret`;
//! - e: B zelf, van zijn eerste instructie tot zijn eigen trap terug.
//!
//! Elke beurt is een hop (ook B na B). Een rondreis loopt van een vraag van
//! de klant tot zijn volgende: een vraag is een bel na een hop zonder bel
//! (de klant draaide, rekende en stuurde; een bel vlak na een bel is een
//! antwoord of een ack), en de klant is wie de eerste vraag stelt. Eén regel
//! per rondreis met de vijf fasen opgeteld en het pad (`3>2` met bel, `3~2`
//! zonder), en per partij de mediaan. Daarnaast per soort hop (met en
//! zonder bel) de mediaan van elke fase met de tussenstempels van [`mark`],
//! los van de groepering (`HOPOS_HOPCOST_HOP`). De kern zet de ring pas op
//! de console als het stil is ([`drain`]), zodat de UART niet in de meting
//! valt. Eén OS-core per node
//! en alleen de kern raakt dit aan, dus losse atomics zonder slot. Zonder de
//! feature zijn alle haken leeg.

use core::fmt;

/// De stand van de architectuurteller.
#[must_use]
pub fn stamp() -> u64 {
    #[cfg(all(target_os = "none", target_arch = "aarch64"))]
    return crate::idle::counter();
    #[cfg(all(target_os = "none", target_arch = "riscv64"))]
    return crate::riscv::csr::rdtime();
    #[cfg(not(target_os = "none"))]
    0
}

/// De rotatie koos bewoner `id` (vóór de wissel).
#[inline]
pub fn pick(id: u8) {
    #[cfg(feature = "hopcost")]
    imp::pick(id, stamp());
    #[cfg(not(feature = "hopcost"))]
    let _ = id;
}

/// De beurt van bewoner `id` is voorbij en de kern is terug, na `settle`.
#[inline]
pub fn ran(id: u8) {
    #[cfg(feature = "hopcost")]
    imp::ran(id, stamp());
    #[cfg(not(feature = "hopcost"))]
    let _ = id;
}

/// De switch belde na een schrijf in een RX-ring (`slot_wake`).
#[inline]
pub fn wake() {
    #[cfg(feature = "hopcost")]
    imp::wake(stamp());
}

/// Een tussenstempel in het beurtpad, de laatste telt: [`BACK`] (de
/// asm-overgang is terug in Rust), [`SLEEP`] (de slaper begint), [`RUN`]
/// (de rotatie begint, na de deur en het masker) en [`ENTER`] (vlak vóór de
/// asm-overgang naar de bewoner). Zo splitst de regel a in asm en de rest,
/// c in executor, deur plus masker en `el2::next`, en d in Rust en asm.
#[inline]
pub fn mark(at: usize) {
    #[cfg(feature = "hopcost")]
    imp::mark(at, stamp());
    #[cfg(not(feature = "hopcost"))]
    let _ = at;
}

/// Zie [`mark`].
pub const BACK: usize = 0;
/// Zie [`mark`].
pub const SLEEP: usize = 1;
/// Zie [`mark`].
pub const RUN: usize = 2;
/// Zie [`mark`].
pub const ENTER: usize = 3;

/// Hoeveel hops met een bel er tot nu toe vastgelegd zijn: staat dit
/// getal stil, dan is het stil op het net van de OS-core (een bewoner die
/// alleen op zijn wektijd draait, telt niet).
#[must_use]
pub fn recorded() -> u64 {
    #[cfg(feature = "hopcost")]
    return imp::BELLS.load(core::sync::atomic::Ordering::Relaxed);
    #[cfg(not(feature = "hopcost"))]
    0
}

/// Zet wat er in de ring staat op `out`: één regel per rondreis en daarna
/// de mediaan per fase. Alleen aanroepen als het stil is, buiten een beurt.
pub fn drain(out: &mut dyn FnMut(fmt::Arguments<'_>)) {
    #[cfg(feature = "hopcost")]
    imp::drain(out);
    #[cfg(not(feature = "hopcost"))]
    let _ = out;
}

// De stempels in de assembly van de overgang (riscv `oscore.rs`, arm64
// `el2/oscore.rs`): leeg zonder de feature. `in` vlak voor de sprong naar de
// bewoner, met twee registers die de overgang daarna zelf laadt; `out` als
// eerste werk van de terugweg, met twee registers die al bewaard zijn.
#[cfg(all(feature = "hopcost", target_os = "none", target_arch = "riscv64"))]
macro_rules! rv_in {
    () => {
        "\n    rdtime t0\n    lla t1, hopos_hopcost\n    sd t0, 0(t1)\n"
    };
}
#[cfg(all(feature = "hopcost", target_os = "none", target_arch = "riscv64"))]
macro_rules! rv_out {
    () => {
        "\n    rdtime t1\n    lla t2, hopos_hopcost\n    sd t1, 8(t2)\n"
    };
}
#[cfg(all(not(feature = "hopcost"), target_os = "none", target_arch = "riscv64"))]
macro_rules! rv_in {
    () => {
        ""
    };
}
#[cfg(all(not(feature = "hopcost"), target_os = "none", target_arch = "riscv64"))]
macro_rules! rv_out {
    () => {
        ""
    };
}
#[cfg(all(target_os = "none", target_arch = "riscv64"))]
pub(crate) use {rv_in, rv_out};

#[cfg(all(feature = "hopcost", target_os = "none", target_arch = "aarch64"))]
macro_rules! arm_in {
    () => {
        "\n    mrs x2, cntpct_el0\n    adrp x3, hopos_hopcost\n    str x2, [x3, :lo12:hopos_hopcost]\n"
    };
}
#[cfg(all(feature = "hopcost", target_os = "none", target_arch = "aarch64"))]
macro_rules! arm_out {
    () => {
        "\n    mrs x2, cntpct_el0\n    adrp x3, hopos_hopcost\n    add x3, x3, :lo12:hopos_hopcost\n    str x2, [x3, #8]\n"
    };
}
#[cfg(all(not(feature = "hopcost"), target_os = "none", target_arch = "aarch64"))]
macro_rules! arm_in {
    () => {
        ""
    };
}
#[cfg(all(not(feature = "hopcost"), target_os = "none", target_arch = "aarch64"))]
macro_rules! arm_out {
    () => {
        ""
    };
}
#[cfg(all(target_os = "none", target_arch = "aarch64"))]
pub(crate) use {arm_in, arm_out};

/// Tikken per seconde van [`stamp`].
#[cfg(feature = "hopcost")]
fn hz() -> u64 {
    #[cfg(all(target_os = "none", target_arch = "aarch64"))]
    return crate::idle::freq();
    #[cfg(all(target_os = "none", target_arch = "riscv64"))]
    return crate::riscv::idle::hz();
    #[cfg(not(target_os = "none"))]
    1_000_000_000
}

/// Tikken naar nanoseconden op `hz`.
#[cfg(feature = "hopcost")]
fn ns(ticks: u64, hz: u64) -> u64 {
    crate::idle::ticks_to_ns(ticks, hz)
}

/// De mediaan van `v` (gesorteerd ter plekke); 0 voor een lege.
#[cfg(feature = "hopcost")]
fn median(v: &mut [u64]) -> u64 {
    v.sort_unstable();
    v.get(v.len() / 2).copied().unwrap_or(0)
}

/// Nanoseconden als microseconden met één decimaal.
#[cfg(feature = "hopcost")]
struct Us(u64);

#[cfg(feature = "hopcost")]
impl fmt::Display for Us {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}", self.0 / 1000, self.0 % 1000 / 100)
    }
}

/// De stempels uit de assembly van de overgang, door beide architecturen
/// geschreven: `[0]` vlak voor de sprong naar de bewoner, `[1]` de eerste
/// instructie van zijn trap terug.
#[cfg(feature = "hopcost")]
#[unsafe(export_name = "hopos_hopcost")]
pub static STAMPS: [core::sync::atomic::AtomicU64; 2] =
    [const { core::sync::atomic::AtomicU64::new(0) }; 2];

#[cfg(feature = "hopcost")]
mod imp {
    use super::STAMPS;
    use core::fmt::{self, Write as _};
    use core::sync::atomic::{AtomicU64, Ordering::Relaxed};

    /// Hoeveel hops de ring vasthoudt: de 200 warme rondreizen van `bench
    /// ping` met hun hops.
    const CAP: usize = 2048;
    /// Hoeveel rondreizen één partij voor de mediaan telt.
    const RTTS: usize = 256;
    /// Het bit in `naar` van een hop waarin de switch belde.
    const BELL: u64 = 1 << 8;

    /// De open hop: van wie (`FROM`), naar wie (`TO`), en de stempels t0
    /// (trap), t1 (kern terug), t2 (bel, 0 = geen), t3 (gekozen). `T[1] ==
    /// 0`: geen open hop.
    static FROM: AtomicU64 = AtomicU64::new(0);
    static TO: AtomicU64 = AtomicU64::new(0);
    static T: [AtomicU64; 4] = [const { AtomicU64::new(0) }; 4];
    /// De ring: per hop `[van, naar, a, b, c, d, e, a-asm, c-executor,
    /// c-deur, c-next, d-rust, d-asm]` in tikken (zie [`super::mark`]).
    static RING: [[AtomicU64; W]; CAP] = [const { [const { AtomicU64::new(0) }; W] }; CAP];
    const W: usize = 13;
    /// De tussenstempels van [`super::mark`], en die van de terugweg van de
    /// afzender van de open hop.
    static M: [AtomicU64; 4] = [const { AtomicU64::new(0) }; 4];
    static SRC_BACK: AtomicU64 = AtomicU64::new(0);

    pub(super) fn mark(at: usize, now: u64) {
        if let Some(w) = M.get(at) {
            w.store(now, Relaxed);
        }
    }

    fn m(at: usize) -> u64 {
        M.get(at).map_or(0, |w| w.load(Relaxed))
    }
    static HEAD: AtomicU64 = AtomicU64::new(0);
    pub(super) static BELLS: AtomicU64 = AtomicU64::new(0);
    static TAIL: AtomicU64 = AtomicU64::new(0);

    fn t(i: usize) -> u64 {
        T.get(i).map_or(0, |w| w.load(Relaxed))
    }

    fn set(i: usize, v: u64) {
        if let Some(w) = T.get(i) {
            w.store(v, Relaxed);
        }
    }

    pub(super) fn pick(id: u8, now: u64) {
        TO.store(u64::from(id), Relaxed);
        set(3, now);
    }

    pub(super) fn wake(now: u64) {
        if t(1) != 0 && t(2) == 0 {
            set(2, now);
        }
    }

    pub(super) fn ran(id: u8, now: u64) {
        let t4 = STAMPS[0].load(Relaxed);
        let back = STAMPS[1].load(Relaxed);
        if t(1) != 0 && TO.load(Relaxed) == u64::from(id) {
            let bell = t(2) != 0;
            let t2 = if bell { t(2) } else { t(1) };
            let (sleep, run, enter) = (m(super::SLEEP), m(super::RUN), m(super::ENTER));
            let hop: [u64; W] = [
                FROM.load(Relaxed),
                u64::from(id) | if bell { BELL } else { 0 },
                t(1).wrapping_sub(t(0)),
                t2.wrapping_sub(t(1)),
                t(3).wrapping_sub(t2),
                t4.wrapping_sub(t(3)),
                back.wrapping_sub(t4),
                SRC_BACK.load(Relaxed).saturating_sub(t(0)),
                sleep.saturating_sub(t2),
                run.saturating_sub(sleep.max(t2)),
                t(3).saturating_sub(run.max(sleep).max(t2)),
                enter.saturating_sub(t(3)),
                t4.saturating_sub(enter.max(t(3))),
            ];
            let h = HEAD.load(Relaxed);
            if let Some(slot) = RING.get((h % CAP as u64) as usize) {
                for (w, v) in slot.iter().zip(hop) {
                    w.store(v, Relaxed);
                }
            }
            HEAD.store(h + 1, Relaxed);
            if bell {
                BELLS.fetch_add(1, Relaxed);
            }
        }
        SRC_BACK.store(m(super::BACK), Relaxed);
        FROM.store(u64::from(id), Relaxed);
        set(0, back);
        set(1, now);
        set(2, 0);
    }

    /// Hop `k` van de ring, de fasen in nanoseconden.
    fn read(k: u64, hz: u64) -> [u64; W] {
        let slot = RING.get((k % CAP as u64) as usize);
        core::array::from_fn(|i| {
            let v = slot.and_then(|s| s.get(i)).map_or(0, |w| w.load(Relaxed));
            if i < 2 { v } else { super::ns(v, hz) }
        })
    }

    /// Begint bij hop `k` een vraag: een bel na een hop zonder bel (de
    /// klant draaide, rekende en stuurde; een bel direct na een bel is een
    /// antwoord of een ack), of een bel van de klant op een antwoord dat
    /// zelf op een bel van hem volgde (`3>2 2>3 3>2`: vraag, antwoord met
    /// de ack erin, en de volgende vraag met de ack erin; hop-cost5).
    /// `who`: alleen van die afzender.
    fn opens(k: u64, first: u64, who: Option<u64>, hz: u64) -> bool {
        let h = read(k, hz);
        if h[1] & BELL == 0 || who.is_some_and(|w| h[0] != w) {
            return false;
        }
        if k == first {
            return true;
        }
        let prev = read(k - 1, hz);
        if prev[1] & BELL == 0 {
            return true;
        }
        // Een antwoord van de ander aan deze afzender, op een bel van hem.
        k - 1 > first && prev[0] != h[0] && prev[1] & !BELL == h[0] && {
            let before = read(k - 2, hz);
            before[1] & BELL != 0 && before[0] == h[0]
        }
    }

    /// De rondreis vanaf hop `k`: tot de volgende vraag van `who` (of het
    /// eind). Geeft de fasen opgeteld en waar hij ophoudt.
    fn rtt(k: u64, head: u64, who: u64, hz: u64) -> ([u64; 5], u64) {
        let mut sum = [0u64; 5];
        let mut j = k;
        while j < head {
            let h = read(j, hz);
            if j > k && opens(j, k, Some(who), hz) {
                break;
            }
            for (s, v) in sum.iter_mut().zip(h.iter().skip(2)) {
                *s += v;
            }
            j += 1;
        }
        (sum, j)
    }

    /// Het pad van een rondreis, `3>2 2~3 ...`, afgekapt op de buffer.
    struct Path {
        b: [u8; 64],
        n: usize,
    }

    impl fmt::Write for Path {
        fn write_str(&mut self, s: &str) -> fmt::Result {
            for c in s.bytes() {
                if let Some(p) = self.b.get_mut(self.n) {
                    *p = c;
                    self.n += 1;
                }
            }
            Ok(())
        }
    }

    pub(super) fn drain(out: &mut dyn FnMut(fmt::Arguments<'_>)) {
        let (head, tail) = (HEAD.load(Relaxed), TAIL.load(Relaxed));
        if head == tail {
            return;
        }
        TAIL.store(head, Relaxed);
        let lost = (head - tail).saturating_sub(CAP as u64);
        let hz = super::hz();
        rounds(out, tail + lost, head, lost, hz);
        classes(out, tail + lost, head, hz);
    }

    /// De rondreizen tussen `start` en `head`, en hun mediaan.
    fn rounds(out: &mut dyn FnMut(fmt::Arguments<'_>), start: u64, head: u64, lost: u64, hz: u64) {
        use super::Us;
        // De rondreis begint bij de eerste vraag: wie dan belt, is de klant.
        let mut first = start;
        while first < head && !opens(first, start, None, hz) {
            first += 1;
        }
        let who = if first < head { read(first, hz)[0] } else { 0 };
        let mut n = 0;
        let mut k = first;
        while k < head {
            let (sum, end) = rtt(k, head, who, hz);
            let mut path = Path { b: [0; 64], n: 0 };
            for j in k..end {
                let h = read(j, hz);
                let arrow = if h[1] & BELL != 0 { '>' } else { '~' };
                let _ = write!(path, "{}{arrow}{} ", h[0], h[1] & !BELL);
            }
            out(format_args!(
                "hopcost rtt hops={} a={} b={} c={} d={} e={} sum={} us [{}] HOPOS_HOPCOST",
                end - k,
                Us(sum[0]),
                Us(sum[1]),
                Us(sum[2]),
                Us(sum[3]),
                Us(sum[4]),
                Us(sum.iter().sum()),
                core::str::from_utf8(path.b.get(..path.n).unwrap_or(&[]))
                    .unwrap_or("?")
                    .trim_end(),
            ));
            n += 1;
            k = end;
        }
        if n < 2 {
            return;
        }
        // De mediaan per fase over de partij (de laatste rondreis telt niet:
        // die houdt op waar de ring ophoudt, niet bij een bel).
        let n = (n - 1).min(RTTS);
        let mut p50 = [0u64; 6];
        let mut v = [0u64; RTTS];
        for (f, p) in p50.iter_mut().enumerate() {
            let mut k = first;
            for x in v.iter_mut().take(n) {
                let (sum, end) = rtt(k, head, who, hz);
                *x = sum.get(f).copied().unwrap_or_else(|| sum.iter().sum());
                k = end;
            }
            *p = super::median(v.get_mut(..n).unwrap_or(&mut []));
        }
        out(format_args!(
            "hopcost p50 n={n} lost={lost} a={} b={} c={} d={} e={} rtt={} us HOPOS_HOPCOST_P50",
            Us(p50[0]),
            Us(p50[1]),
            Us(p50[2]),
            Us(p50[3]),
            Us(p50[4]),
            Us(p50[5])
        ));
    }

    /// Hoeveel hops één klasse voor de mediaan telt (de laatste).
    const HOPS: usize = 512;

    /// Per soort hop (met en zonder bel) de mediaan van elke fase en
    /// tussenfase, los van hoe de hops tot rondreizen groeperen: op ijzer
    /// draaien er meer bewoners en interrupts tussendoor.
    fn classes(out: &mut dyn FnMut(fmt::Arguments<'_>), from: u64, head: u64, hz: u64) {
        use super::Us;
        for (bell, name) in [(true, "bell"), (false, "nobell")] {
            let of = |k: &u64| (read(*k, hz)[1] & BELL != 0) == bell;
            let n = (from..head).filter(of).count();
            if n == 0 {
                continue;
            }
            let skip = n.saturating_sub(HOPS);
            let mut p = [0u64; W];
            let mut v = [0u64; HOPS];
            for (f, x) in p.iter_mut().enumerate().skip(2) {
                let mut len = 0;
                for (k, slot) in (from..head).filter(of).skip(skip).zip(v.iter_mut()) {
                    *slot = read(k, hz).get(f).copied().unwrap_or(0);
                    len += 1;
                }
                *x = super::median(v.get_mut(..len).unwrap_or(&mut []));
            }
            out(format_args!(
                "hopcost hop {name} n={n} a={} (asm {}) b={} c={} (executor {} door+mask {} next {}) d={} (rust {} asm {}) e={} us HOPOS_HOPCOST_HOP",
                Us(p[2]),
                Us(p[7]),
                Us(p[3]),
                Us(p[4]),
                Us(p[8]),
                Us(p[9]),
                Us(p[10]),
                Us(p[5]),
                Us(p[11]),
                Us(p[12]),
                Us(p[6]),
            ));
        }
    }
}

#[cfg(all(test, feature = "hopcost"))]
mod tests {
    use super::*;
    use core::sync::atomic::Ordering::Relaxed;

    #[test]
    fn a_round_trip_runs_from_request_to_request() {
        // Het patroon van `bench ping` over TCP (03-10, QEMU): de klant 3
        // vraagt, de dienst 2 ackt, 3 yieldt zonder bel, 2 antwoordt, 3 ackt
        // (een bel vlak na een bel: geen nieuwe vraag), 2 yieldt, en 3
        // vraagt opnieuw. Per hop in us: a 2 (asm 1), d 2 (rust 1), e 8; met
        // bel b 5 en c 4 (executor 1, deur 1, next 2), zonder bel c 6.
        let mut t = 0;
        let mut hop = |id: u8, bell: bool| {
            let k = if bell { 3000 } else { 0 };
            if bell {
                imp::wake(t + 2000 + k);
            }
            imp::mark(SLEEP, t + 3000 + k);
            imp::mark(RUN, t + 4000 + k);
            imp::pick(id, t + 6000 + k);
            imp::mark(ENTER, t + 7000 + k);
            STAMPS[0].store(t + 8000 + k, Relaxed);
            STAMPS[1].store(t + 16000 + k, Relaxed);
            imp::mark(BACK, t + 17000 + k);
            imp::ran(id, t + 18000 + k);
            t += 18000 + k;
        };
        hop(3, false);
        for _ in 0..3 {
            for (id, bell) in [
                (2, true),
                (3, true),
                (2, false),
                (3, true),
                (2, true),
                (3, false),
            ] {
                hop(id, bell);
            }
        }
        hop(2, true);
        let mut lines = Vec::new();
        drain(&mut |a| lines.push(format!("{a}")));
        assert_eq!(lines.len(), 7, "{lines:?}");
        for l in &lines[..3] {
            assert!(l.contains("hops=6 "), "{l}");
            assert!(l.contains("[3>2 2>3 3~2 2>3 3>2 2~3]"), "{l}");
        }
        assert!(lines[3].contains("[3>2]"), "{}", lines[3]);
        assert!(lines[4].contains("HOPOS_HOPCOST_P50"), "{}", lines[4]);
        assert!(lines[4].contains("n=3 "), "{}", lines[4]);
        assert!(
            lines[5].starts_with(
                "hopcost hop bell n=13 a=2.0 (asm 1.0) b=5.0 c=4.0 (executor 1.0 door+mask 1.0 next 2.0) d=2.0 (rust 1.0 asm 1.0) e=8.0 us"
            ),
            "{}",
            lines[5]
        );
        assert!(lines[6].starts_with(
                "hopcost hop nobell n=6 a=2.0 (asm 1.0) b=0.0 c=6.0 (executor 3.0 door+mask 1.0 next 2.0)"
            ), "{}", lines[6]);

        // Met de ack in het antwoord en in de volgende vraag (hop-cost5):
        // twee hops per rondreis, allebei met bel.
        hop(3, false);
        for _ in 0..3 {
            hop(2, true);
            hop(3, true);
        }
        hop(2, true);
        lines.clear();
        drain(&mut |a| lines.push(format!("{a}")));
        assert_eq!(lines.len(), 7, "{lines:?}");
        for l in &lines[..3] {
            assert!(l.contains("hops=2 "), "{l}");
            assert!(l.contains("[3>2 2>3]"), "{l}");
        }
        assert!(lines[4].contains("HOPOS_HOPCOST_P50"), "{}", lines[4]);
    }
}
