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
//! zonder), en per partij de mediaan. De kern zet de ring pas op de console als het stil is
//! ([`drain`]), zodat de UART niet in de meting valt. Eén OS-core per node
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
    /// De ring: per hop `[van, naar, a, b, c, d, e]` in tikken.
    static RING: [[AtomicU64; 7]; CAP] = [const { [const { AtomicU64::new(0) }; 7] }; CAP];
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
            let hop = [
                FROM.load(Relaxed),
                u64::from(id) | if bell { BELL } else { 0 },
                t(1).wrapping_sub(t(0)),
                t2.wrapping_sub(t(1)),
                t(3).wrapping_sub(t2),
                t4.wrapping_sub(t(3)),
                back.wrapping_sub(t4),
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
        FROM.store(u64::from(id), Relaxed);
        set(0, back);
        set(1, now);
        set(2, 0);
    }

    /// Hop `k` van de ring, de fasen in nanoseconden.
    fn read(k: u64, hz: u64) -> [u64; 7] {
        let slot = RING.get((k % CAP as u64) as usize);
        core::array::from_fn(|i| {
            let v = slot.and_then(|s| s.get(i)).map_or(0, |w| w.load(Relaxed));
            if i < 2 { v } else { super::ns(v, hz) }
        })
    }

    /// Begint bij hop `k` een vraag: een bel na een hop zonder bel (de
    /// klant draaide, rekende en stuurde; een bel direct na een bel is een
    /// antwoord of een ack). `who`: alleen van die afzender.
    fn opens(k: u64, first: u64, who: Option<u64>, hz: u64) -> bool {
        let h = read(k, hz);
        h[1] & BELL != 0
            && who.is_none_or(|w| h[0] == w)
            && (k == first || read(k - 1, hz)[1] & BELL == 0)
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
        use super::Us;
        let (head, tail) = (HEAD.load(Relaxed), TAIL.load(Relaxed));
        if head == tail {
            return;
        }
        TAIL.store(head, Relaxed);
        let lost = (head - tail).saturating_sub(CAP as u64);
        let hz = super::hz();
        // De rondreis begint bij de eerste vraag: wie dan belt, is de klant.
        let start = tail + lost;
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
}

#[cfg(all(test, feature = "hopcost"))]
mod tests {
    use super::*;
    use core::sync::atomic::Ordering::Relaxed;

    /// Eén beurt van `id`: gekozen op `pick`, de sprong op `jump`, de trap
    /// op `trap`, de kern terug op `back`.
    fn turn(id: u8, pick: u64, jump: u64, trap: u64, back: u64) {
        imp::pick(id, pick);
        STAMPS[0].store(jump, Relaxed);
        STAMPS[1].store(trap, Relaxed);
        imp::ran(id, back);
    }

    #[test]
    fn a_round_trip_runs_from_request_to_request() {
        // Het patroon van `bench ping` over TCP (03-10, QEMU): de klant 3
        // vraagt, de dienst 2 ackt, 3 yieldt zonder bel, 2 antwoordt, 3 ackt
        // (een bel vlak na een bel: geen nieuwe vraag), 2 yieldt, en 3
        // vraagt opnieuw.
        let mut t = 0;
        let mut hop = |id: u8, bell: bool| {
            if bell {
                imp::wake(t + 5);
            }
            turn(id, t + 10, t + 11, t + 20, t + 21);
            t += 100;
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
        assert_eq!(lines.len(), 5, "{lines:?}");
        for l in &lines[..3] {
            assert!(l.contains("hops=6 "), "{l}");
            assert!(l.contains("[3>2 2>3 3~2 2>3 3>2 2~3]"), "{l}");
        }
        assert!(lines[3].contains("[3>2]"), "{}", lines[3]);
        assert!(lines[4].contains("HOPOS_HOPCOST_P50"), "{}", lines[4]);
        assert!(lines[4].contains("n=3 "), "{}", lines[4]);
    }
}
