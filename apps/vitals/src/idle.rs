//! De idle-meting: passief, altijd aan. Idle-gedrag wil je juist zien als
//! er verder niets gebeurt, dus een test die je start zou zijn eigen meting
//! verpesten.
//!
//! De sampler ([`sample`]) leest elke [`SAMPLE_EVERY`] de twee woorden die
//! de slaper van applib op de control-page publiceert en die verder niemand
//! leest (de kern leest `CTRL_WAKES` bewust niet):
//!
//! - `CTRL_IDLE` (0x48): de geslapen tijd in tellertikken;
//! - `CTRL_WAKES` (0x108): het aantal idle-rondes.
//!
//! Daaruit volgt over een venster ([`window`]) de idle-fractie
//! (ΔIdle / (Δwand · Hz)), het wektempo (ΔWakes / Δwand) en de kosten per
//! wek: (1 - idle-fractie) / wekken per seconde. Onder QEMU-TCG is WFE een
//! no-op; dit zijn ijzer-cijfers.
//!
//! Dit module bezit de ring met samples ([`RING`], een leesbare tabel: de
//! sampler schrijft, `/api/state` leest kort).

use applib::{App, EXEC, clock};
use core::cell::RefCell;
use core::time::Duration;
use sync::Local;

/// Het ritme van de sampler.
pub(crate) const SAMPLE_EVERY: Duration = Duration::from_secs(2);

/// Het venster op de pagina.
pub(crate) const WINDOW: Duration = Duration::from_secs(60);

/// Zoveel samples houdt de ring: ruim twee minuten, meer dan het venster.
pub(crate) const RING: usize = 64;

/// Het kortste venster dat een cijfer oplevert.
const MIN_SPAN_NS: u64 = 4_000_000_000;

/// Eén sample van de control-page.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Sample {
    /// Wanneer, in ns op de klok van de app.
    pub(crate) t_ns: u64,
    /// `CTRL_IDLE`: geslapen tikken.
    pub(crate) idle: u64,
    /// `CTRL_WAKES`: idle-rondes.
    pub(crate) wakes: u64,
    /// `CTRL_CORES`: de cores van het slot (minstens 1).
    pub(crate) cores: u64,
}

/// De ring: de laatste [`RING`] samples, oudste eerst.
pub(crate) struct Ring {
    buf: [Sample; RING],
    len: usize,
    head: usize,
}

impl Ring {
    /// Een lege ring.
    pub(crate) const fn new() -> Self {
        Self {
            buf: [Sample {
                t_ns: 0,
                idle: 0,
                wakes: 0,
                cores: 0,
            }; RING],
            len: 0,
            head: 0,
        }
    }

    /// Een sample erbij; de oudste valt weg als hij vol is.
    pub(crate) fn push(&mut self, s: Sample) {
        if let Some(slot) = self.buf.get_mut(self.head) {
            *slot = s;
        }
        self.head = (self.head + 1) % RING;
        self.len = (self.len + 1).min(RING);
    }

    /// Sample `i`, 0 is de oudste.
    fn get(&self, i: usize) -> Option<Sample> {
        if i >= self.len {
            return None;
        }
        let start = (self.head + RING - self.len) % RING;
        self.buf.get((start + i) % RING).copied()
    }
}

/// De idle-cijfers over een venster.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct Window {
    /// Is er een venster (minstens [`MIN_SPAN_NS`] zonder reset)?
    pub(crate) ok: bool,
    /// De lengte van het venster in s.
    pub(crate) span_s: f64,
    /// Het idle-percentage; `None` als het niet te verdedigen is.
    pub(crate) idle_pct: Option<f64>,
    /// Idle-rondes per seconde.
    pub(crate) wakes_per_s: f64,
    /// De kosten van één wek in µs; `None` zonder idle-percentage of wekken.
    pub(crate) wake_cost_us: Option<f64>,
    /// Waarom er geen idle-percentage is.
    pub(crate) note: Option<&'static str>,
}

/// De cijfers over (hooguit) de laatste `span_ns` van `ring`, op een
/// teller van `hz`.
///
/// Alleen het aaneengesloten stuk telt: een teller die terugloopt of een
/// ander aantal cores (een herstart, een flip) begint een nieuw venster. Bij
/// meer dan één core is de som van de idle-tijd geen percentage van één
/// wandklok; dan het wektempo wel en het percentage niet, met de reden
/// erbij (zoals de Go-vitals).
pub(crate) fn window(ring: &Ring, span_ns: u64, hz: u64) -> Window {
    let Some(last) = ring.len.checked_sub(1).and_then(|i| ring.get(i)) else {
        return Window::default();
    };
    if ring.len < 3 {
        return Window::default();
    }
    let cutoff = last.t_ns.saturating_sub(span_ns);
    let mut base = last;
    for k in (0..ring.len).rev() {
        let Some(cur) = ring.get(k) else { break };
        if cur.t_ns < cutoff {
            break;
        }
        if let Some(next) = ring.get(k + 1)
            && (next.idle < cur.idle
                || next.wakes < cur.wakes
                || next.cores.max(1) != cur.cores.max(1))
        {
            break;
        }
        base = cur;
    }
    let span = last.t_ns.saturating_sub(base.t_ns);
    if span < MIN_SPAN_NS {
        return Window::default();
    }
    let span_s = span as f64 / 1e9;
    let wakes_per_s = last.wakes.saturating_sub(base.wakes) as f64 / span_s;
    let mut w = Window {
        ok: true,
        span_s,
        wakes_per_s,
        ..Window::default()
    };
    if last.cores > 1 {
        w.note = Some(
            "SMP: the idle counter sums the sleep of every core; a total idle percentage cannot be derived",
        );
        return w;
    }
    if hz == 0 {
        w.note = Some("counter frequency unavailable");
        return w;
    }
    let slept = last.idle.saturating_sub(base.idle) as f64;
    let frac = (slept / (span_s * hz as f64)).clamp(0.0, 1.0);
    w.idle_pct = Some(frac * 100.0);
    if wakes_per_s > 0.0 {
        // De rekensom uit abi/layout: tijd per wek = (1 - idle) / wekken.
        w.wake_cost_us = Some((1.0 - frac) / wakes_per_s * 1e6);
    }
    w
}

/// De ring van deze app.
pub(crate) static SAMPLES: Local<RefCell<Ring>> = Local::new(RefCell::new(Ring::new()));

/// Het venster van nu, over [`WINDOW`].
pub(crate) fn now() -> Window {
    let span = u64::try_from(WINDOW.as_nanos()).unwrap_or(u64::MAX);
    SAMPLES
        .get()
        .try_borrow()
        .map(|r| window(&r, span, clock::hz()))
        .unwrap_or_default()
}

/// De sampler: elke [`SAMPLE_EVERY`] één sample in de ring.
pub(crate) async fn sample(app: &'static App) {
    let ctrl = app.ctrl();
    loop {
        let s = Sample {
            t_ns: clock::now_ns(),
            idle: ctrl.idle_ticks(),
            wakes: ctrl.idle_rounds(),
            cores: ctrl.cores(),
        };
        if let Ok(mut r) = SAMPLES.get().try_borrow_mut() {
            r.push(s);
        }
        EXEC.after(SAMPLE_EVERY).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const S: u64 = 1_000_000_000;

    fn ring(samples: &[(u64, u64, u64, u64)]) -> Ring {
        let mut r = Ring::new();
        for &(t, idle, wakes, cores) in samples {
            r.push(Sample {
                t_ns: t * S,
                idle,
                wakes,
                cores,
            });
        }
        r
    }

    #[test]
    fn half_asleep_at_a_hundred_wakes_is_five_ms_per_wake() {
        // 54 MHz (de Pi): 10 s venster, 5 s geslapen, 1000 wekken.
        let r = ring(&[
            (0, 0, 0, 1),
            (5, 135_000_000, 500, 1),
            (10, 270_000_000, 1000, 1),
        ]);
        let w = window(&r, 60 * S, 54_000_000);
        assert!(w.ok);
        assert_eq!(w.span_s, 10.0);
        assert_eq!(w.wakes_per_s, 100.0);
        assert_eq!(w.idle_pct, Some(50.0));
        assert_eq!(w.wake_cost_us, Some(5000.0));
    }

    #[test]
    fn a_reset_starts_a_new_window() {
        // De teller liep terug tussen t=4 en t=6: alleen 6..12 telt.
        let r = ring(&[
            (0, 0, 0, 1),
            (2, 100, 10, 1),
            (4, 200, 20, 1),
            (6, 0, 0, 1),
            (12, 600, 60, 1),
        ]);
        let w = window(&r, 60 * S, 100);
        assert_eq!(w.span_s, 6.0);
        assert_eq!(w.wakes_per_s, 10.0);
        assert_eq!(w.idle_pct, Some(100.0));
    }

    #[test]
    fn smp_gives_wakes_but_no_percentage() {
        let r = ring(&[(0, 0, 0, 2), (5, 10, 50, 2), (10, 20, 100, 2)]);
        let w = window(&r, 60 * S, 1_000_000_000);
        assert!(w.ok);
        assert_eq!(w.idle_pct, None);
        assert_eq!(w.wakes_per_s, 10.0);
        assert!(w.note.is_some());
    }

    #[test]
    fn too_short_or_too_few_is_no_window() {
        assert!(!window(&ring(&[(0, 0, 0, 1), (2, 1, 1, 1)]), 60 * S, 1).ok);
        let r = ring(&[(0, 0, 0, 1), (1, 1, 1, 1), (2, 2, 2, 1)]);
        assert!(!window(&r, 60 * S, 1).ok);
    }

    #[test]
    fn the_ring_keeps_the_newest() {
        let mut r = Ring::new();
        for t in 0..(RING as u64 + 10) {
            r.push(Sample {
                t_ns: t,
                ..Sample::default()
            });
        }
        assert_eq!(r.len, RING);
        assert_eq!(r.get(0).map(|s| s.t_ns), Some(10));
        assert_eq!(r.get(RING - 1).map(|s| s.t_ns), Some(RING as u64 + 9));
        assert_eq!(r.get(RING), None);
    }

    #[test]
    fn the_window_is_limited_to_its_span() {
        let r = ring(&[
            (0, 0, 0, 1),
            (100, 0, 0, 1),
            (110, 0, 100, 1),
            (120, 0, 200, 1),
        ]);
        let w = window(&r, 60 * S, 1);
        assert_eq!(w.span_s, 20.0);
        assert_eq!(w.wakes_per_s, 10.0);
    }
}
