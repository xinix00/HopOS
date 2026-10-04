//! De uitkomst van één test ([`Report`]) en hoe hij naar buiten gaat: als
//! JSON voor `/api/state` en als `key=value`-staart achter een marker op de
//! console.
//!
//! Puur: geen klok, geen stack, geen kern. Een rapport alloceert één keer,
//! bij zijn geboorte (de regelbuffer van [`LINES_CAP`]); daarna groeit niets
//! meer, en wat niet past valt weg met een zichtbare afkapping. Zo kan een
//! test die duizend regels wil de heap van de app niet opeten.
//!
//! Dit module bezit de rapporten niet; dat doet de tabel in `run`.

use alloc::vec::Vec;
use applib::text::{Room, json_str};
use bounded::{BoundedVec, Text};
use core::fmt::{self, Write};

/// Zoveel meetwaarden per test; de breedste (memlat) heeft er zes.
pub(crate) const METRICS: usize = 10;

/// Zoveel bytes aan detailregels per test: de burn van tien minuten met
/// een regel per vijf seconden past er ruim in.
pub(crate) const LINES_CAP: usize = 8 << 10;

/// De maat van een foutmelding.
pub(crate) const ERR_CAP: usize = 160;

/// Eén meetwaarde.
#[derive(Clone, Copy, Debug, PartialEq)]
pub(crate) struct Metric {
    /// De naam, ook de sleutel achter de marker (`rate=...`).
    pub(crate) name: &'static str,
    /// De waarde.
    pub(crate) value: f64,
    /// De eenheid, ASCII (`MB/s`, `us`, `C`).
    pub(crate) unit: &'static str,
}

/// Het langste stuk van `b` dat geldige UTF-8 is.
fn valid_prefix(b: &[u8]) -> &str {
    match core::str::from_utf8(b) {
        Ok(s) => s,
        Err(e) => {
            core::str::from_utf8(b.get(..e.valid_up_to()).unwrap_or_default()).unwrap_or_default()
        }
    }
}

/// De detailregels van een rapport: één buffer, vooraf gereserveerd, met
/// `\n` tussen de regels. Een regel die niet meer past, wordt `...`.
pub(crate) struct Lines {
    buf: Vec<u8>,
    cut: bool,
}

impl Lines {
    /// Een lege buffer van [`LINES_CAP`]; zegt de heap nee, dan blijft hij
    /// leeg en telt elke regel als afgekapt.
    fn new() -> Self {
        let mut buf = Vec::new();
        let cut = buf.try_reserve_exact(LINES_CAP).is_err();
        Self { buf, cut }
    }

    /// Voegt één regel toe.
    fn push(&mut self, args: fmt::Arguments<'_>) {
        if self.cut {
            return;
        }
        let mark = self.buf.len();
        let mut w = Room(&mut self.buf);
        let fits = w.write_fmt(args).is_ok() && w.write_str("\n").is_ok();
        if !fits {
            // Deze regel niet half: terug naar de vorige, en één keer
            // zeggen dat er meer was.
            self.buf.truncate(mark);
            self.cut = true;
            let _ = Room(&mut self.buf).write_str("...\n");
        }
    }

    /// De regels.
    pub(crate) fn iter(&self) -> impl Iterator<Item = &str> {
        valid_prefix(&self.buf)
            .split('\n')
            .filter(|l| !l.is_empty())
    }
}

/// De uitkomst van één test-run. Eenmaal in de tabel wordt hij niet meer
/// gewijzigd.
pub(crate) struct Report {
    /// De test.
    pub(crate) test: &'static str,
    /// De start, in ns sinds de start van de app.
    pub(crate) started_ns: u64,
    /// De duur in ns.
    pub(crate) duration_ns: u64,
    metrics: BoundedVec<Metric, METRICS>,
    lines: Lines,
    err: Text<ERR_CAP>,
    skipped: Option<&'static str>,
}

impl Report {
    /// Een leeg rapport voor `test`, begonnen op `started_ns`.
    pub(crate) fn new(test: &'static str, started_ns: u64) -> Self {
        Self {
            test,
            started_ns,
            duration_ns: 0,
            metrics: BoundedVec::new(),
            lines: Lines::new(),
            err: Text::new(),
            skipped: None,
        }
    }

    /// Een meetwaarde erbij; meer dan [`METRICS`] valt weg.
    pub(crate) fn add(&mut self, name: &'static str, value: f64, unit: &'static str) {
        let _ = self.metrics.push(Metric { name, value, unit });
    }

    /// Een detailregel erbij.
    pub(crate) fn line(&mut self, args: fmt::Arguments<'_>) {
        self.lines.push(args);
    }

    /// De test faalde; de eerste fout telt.
    pub(crate) fn fail(&mut self, args: fmt::Arguments<'_>) {
        if self.err.is_empty() {
            let _ = self.err.write_fmt(args);
        }
    }

    /// De test sloeg zichzelf over, met de reden.
    pub(crate) fn skip(&mut self, why: &'static str) {
        self.skipped = Some(why);
    }

    /// De fout, als die er is.
    pub(crate) fn error(&self) -> Option<&str> {
        (!self.err.is_empty()).then(|| self.err.as_str())
    }

    /// De reden van het overslaan, als die er is.
    pub(crate) const fn skipped(&self) -> Option<&'static str> {
        self.skipped
    }

    /// De meetwaarden.
    pub(crate) fn metrics(&self) -> &[Metric] {
        self.metrics.as_slice()
    }

    /// De meetwaarde `name`.
    #[cfg(test)]
    pub(crate) fn metric(&self, name: &str) -> Option<f64> {
        self.metrics()
            .iter()
            .find(|m| m.name == name)
            .map(|m| m.value)
    }

    /// De `key=value`-staart achter de marker: elke meetwaarde, kort
    /// afgerond.
    pub(crate) fn kv(&self) -> Text<160> {
        let mut s = Text::new();
        for m in self.metrics() {
            let _ = write!(s, " {}=", m.name);
            let _ = num(&mut s, m.value);
        }
        s
    }

    /// Het rapport als JSON-object.
    pub(crate) fn json(&self, w: &mut impl Write) -> fmt::Result {
        w.write_str("{\"test\":")?;
        json_str(w, self.test)?;
        write!(
            w,
            ",\"started_s\":{:.3},\"duration_s\":{:.3},\"metrics\":[",
            secs(self.started_ns),
            secs(self.duration_ns)
        )?;
        for (i, m) in self.metrics().iter().enumerate() {
            if i > 0 {
                w.write_str(",")?;
            }
            w.write_str("{\"name\":")?;
            json_str(w, m.name)?;
            w.write_str(",\"value\":")?;
            json_num(w, m.value)?;
            w.write_str(",\"unit\":")?;
            json_str(w, m.unit)?;
            w.write_str("}")?;
        }
        w.write_str("],\"lines\":[")?;
        for (i, l) in self.lines.iter().enumerate() {
            if i > 0 {
                w.write_str(",")?;
            }
            json_str(w, l)?;
        }
        w.write_str("]")?;
        if let Some(e) = self.error() {
            w.write_str(",\"error\":")?;
            json_str(w, e)?;
        }
        if let Some(s) = self.skipped {
            w.write_str(",\"skipped\":")?;
            json_str(w, s)?;
        }
        w.write_str("}")
    }
}

/// Nanoseconden als seconden, minstens een nanoseconde: een deling erdoor
/// gaat nooit door nul.
pub(crate) fn secs(ns: u64) -> f64 {
    ns.max(1) as f64 / 1e9
}

/// Een getal voor de console: drie significante cijfers is genoeg voor een
/// marker; de JSON heeft de rest.
pub(crate) fn num(w: &mut impl Write, v: f64) -> fmt::Result {
    let a = if v < 0.0 { -v } else { v };
    // Een heel getal (cores, fouten, een chunk) zonder decimalen.
    let whole = a < 1e15 && v == (v as i64) as f64;
    if a >= 100.0 || whole {
        write!(w, "{v:.0}")
    } else if a >= 10.0 {
        write!(w, "{v:.1}")
    } else {
        write!(w, "{v:.2}")
    }
}

/// Een getal als JSON: `null` voor NaN en oneindig (JSON kent ze niet).
pub(crate) fn json_num(w: &mut impl Write, v: f64) -> fmt::Result {
    if v.is_finite() {
        write!(w, "{v:.3}")
    } else {
        w.write_str("null")
    }
}

/// Het `p`-de percentiel van `v` (100 is het maximum), op de manier van de
/// Go-vitals: sorteren en de index `len * p / 100`, geklemd. 0 zonder
/// samples. Sorteert `v` ter plekke.
pub(crate) fn pct(v: &mut [u32], p: usize) -> u32 {
    if v.is_empty() {
        return 0;
    }
    v.sort_unstable();
    let i = (v.len() * p / 100).min(v.len() - 1);
    v.get(i).copied().unwrap_or(0)
}

/// Het gemiddelde; 0 zonder samples.
pub(crate) fn avg(v: &[f64]) -> f64 {
    if v.is_empty() {
        return 0.0;
    }
    v.iter().sum::<f64>() / v.len() as f64
}

#[cfg(test)]
mod tests {
    use super::*;
    use alloc::string::String;

    fn json(r: &Report) -> String {
        let mut s = String::new();
        r.json(&mut s).unwrap();
        s
    }

    #[test]
    fn a_report_renders_as_json_with_its_lines_and_error() {
        let mut r = Report::new("cpu", 2_000_000_000);
        r.duration_ns = 5_000_000_000;
        r.add("rate", 812.25, "Msteps/s");
        r.line(format_args!("{} bursts", 7));
        r.line(format_args!("say \"hi\""));
        r.fail(format_args!("first"));
        r.fail(format_args!("second"));
        assert_eq!(
            json(&r),
            "{\"test\":\"cpu\",\"started_s\":2.000,\"duration_s\":5.000,\"metrics\":[{\"name\":\"rate\",\"value\":812.250,\"unit\":\"Msteps/s\"}],\"lines\":[\"7 bursts\",\"say \\\"hi\\\"\"],\"error\":\"first\"}"
        );
        assert_eq!(r.metric("rate"), Some(812.25));
        assert_eq!(r.error(), Some("first"));
    }

    #[test]
    fn the_marker_tail_rounds_to_three_digits() {
        let mut r = Report::new("membw", 0);
        r.add("copy", 4.56789, "GB/s");
        r.add("triad", 12.345, "GB/s");
        r.add("p99", 1234.6, "us");
        r.add("cores", 2.0, "");
        r.add("errors", 0.0, "");
        assert_eq!(
            r.kv().as_str(),
            " copy=4.57 triad=12.3 p99=1235 cores=2 errors=0"
        );
    }

    #[test]
    fn a_number_that_is_not_one_is_null() {
        let mut s = String::new();
        json_num(&mut s, f64::NAN).unwrap();
        assert_eq!(s, "null");
    }

    #[test]
    fn lines_stop_at_the_cap_and_say_so_once() {
        let mut r = Report::new("burn", 0);
        let long = "x".repeat(1000);
        for _ in 0..20 {
            r.line(format_args!("{long}"));
        }
        let lines: Vec<&str> = r.lines.iter().collect();
        assert_eq!(lines.len(), LINES_CAP / 1001 + 1);
        assert_eq!(lines.last(), Some(&"..."));
        assert!(r.lines.buf.len() <= LINES_CAP);
        assert_eq!(r.lines.buf.capacity(), LINES_CAP);
    }

    #[test]
    fn metrics_beyond_the_table_fall_away() {
        let mut r = Report::new("memlat", 0);
        for _ in 0..METRICS + 3 {
            r.add("x", 1.0, "");
        }
        assert_eq!(r.metrics().len(), METRICS);
    }

    #[test]
    fn percentiles_follow_the_go_index() {
        let mut v = [5u32, 1, 4, 2, 3, 10, 9, 8, 7, 6];
        assert_eq!(pct(&mut v, 50), 6);
        assert_eq!(pct(&mut v, 99), 10);
        assert_eq!(pct(&mut v, 100), 10);
        assert_eq!(pct(&mut [], 50), 0);
        assert_eq!(avg(&[1.0, 2.0, 3.0]), 2.0);
        assert_eq!(avg(&[]), 0.0);
    }
}
