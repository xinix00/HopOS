//! Het meetprotocol tussen `tools/netmeter` (de host) en deze app (het
//! slot): één commandoregel per verbinding, daarna de payload. Pure logica,
//! dus op de host getoetst; `tools/netmeter/src/proto.rs` is de andere kant
//! en pint dezelfde bytes.
//!
//! | Regel | Wat de server doet |
//! | --- | --- |
//! | `echo\n` | Elke byte terug, tot EOF: de rtt-meting. |
//! | `sink <n>\n` | Leest precies `n` bytes, en antwoordt `sunk <n> <us>\n`: de doorvoer de node in. |
//! | `source <n>\n` | Schrijft `n` bytes van het [`fill_pattern`]-patroon en sluit: de doorvoer de node uit. |
//!
//! Waarom `sink` een lengte draagt en geen EOF afwacht: een half-close
//! (`shutdown(Write)`) heeft applib niet, en met de lengte vooraf weet de
//! server zonder FIN wanneer hij klaar is en kan hij nog antwoorden.
//!
//! UDP op dezelfde poort is een kale echo: elk datagram terug naar zijn
//! afzender.

/// De langste commandoregel, inclusief de `\n`.
pub(crate) const CMD_MAX: usize = 64;

/// De maat van het patroonblok: `source` herhaalt dit blok.
pub(crate) const PATTERN_LEN: usize = 64 << 10;

/// Het zaad van het patroon: "HPOS", hetzelfde als `serveUp` in de
/// Go-netmeter, zodat een build die ooit dezelfde bytes serveerde dat nog
/// doet.
pub(crate) const SEED: u64 = 0x4850_4f53;

/// Het grootste `n` dat een `sink` of `source` mag vragen: 64 GiB. Een
/// meting van een uur op 10 Gbit blijft eronder; een getypte nul te veel
/// niet.
pub(crate) const MAX_BYTES: u64 = 64 << 30;

/// Een commando van de client.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) enum Cmd {
    /// Alles terug tot EOF.
    Echo,
    /// Precies zoveel bytes lezen, dan `sunk`.
    Sink(u64),
    /// Zoveel bytes schrijven, dan sluiten.
    Source(u64),
}

/// Leest één commandoregel (zonder de `\n`). `None` bij onzin: de server
/// sluit dan zonder antwoord, luid op zijn eigen log.
pub(crate) fn parse(line: &[u8]) -> Option<Cmd> {
    let line = core::str::from_utf8(line).ok()?.trim();
    let (word, arg) = match line.split_once(' ') {
        Some((w, a)) => (w, Some(a.trim())),
        None => (line, None),
    };
    let n = || {
        arg?.parse::<u64>()
            .ok()
            .filter(|n| *n > 0 && *n <= MAX_BYTES)
    };
    match word {
        "echo" if arg.is_none() => Some(Cmd::Echo),
        "sink" => n().map(Cmd::Sink),
        "source" => n().map(Cmd::Source),
        _ => None,
    }
}

/// Vult `buf` met het patroon: xorshift64 vanaf [`SEED`], één byte per
/// stap (de vorm van `serveUp` in de Go-netmeter).
pub(crate) fn fill_pattern(buf: &mut [u8]) {
    let mut x = SEED;
    for b in buf.iter_mut() {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        *b = x as u8;
    }
}

/// Het `p`-de percentiel van een oplopend gesorteerde reeks: de index
/// `n * p / 100`, geklemd (de vorm van `BENCH_RTT` in de Go-appspike).
pub(crate) fn percentile(sorted: &[u64], p: usize) -> u64 {
    let n = sorted.len();
    if n == 0 {
        return 0;
    }
    let i = (n.saturating_mul(p) / 100).min(n - 1);
    sorted.get(i).copied().unwrap_or(0)
}

/// Megabytes per seconde, decimaal (docs/measurements.md: MB/s is
/// 1.000.000 bytes per seconde).
pub(crate) fn mbps(bytes: u64, ns: u64) -> f64 {
    if ns == 0 {
        return 0.0;
    }
    bytes as f64 * 1000.0 / ns as f64
}

/// Een `ip:poort` uit de env (`BENCH_PEER`).
pub(crate) fn parse_peer(s: &str) -> Option<([u8; 4], u16)> {
    let (ip, port) = s.trim().rsplit_once(':')?;
    let mut out = [0u8; 4];
    let mut parts = ip.split('.');
    for o in &mut out {
        *o = parts.next()?.parse().ok()?;
    }
    if parts.next().is_some() {
        return None;
    }
    let port = port.parse::<u16>().ok().filter(|p| *p != 0)?;
    Some((out, port))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_three_commands_parse() {
        assert_eq!(parse(b"echo"), Some(Cmd::Echo));
        assert_eq!(parse(b"sink 1048576"), Some(Cmd::Sink(1 << 20)));
        assert_eq!(parse(b"source 5\r"), Some(Cmd::Source(5)));
        assert_eq!(parse(b"echo 3"), None);
        assert_eq!(parse(b"sink"), None);
        assert_eq!(parse(b"sink 0"), None);
        assert_eq!(parse(b"source -1"), None);
        assert_eq!(parse(b"source 68719476737"), None);
        assert_eq!(parse(b"GET / HTTP/1.1"), None);
        assert_eq!(parse(&[0xff, 0xfe]), None);
    }

    /// De eerste bytes van het patroon liggen vast: netmeter op de host
    /// toetst tegen dezelfde acht (tools/netmeter/src/proto.rs).
    #[test]
    fn the_pattern_is_pinned() {
        let mut b = [0u8; 8];
        fill_pattern(&mut b);
        assert_eq!(b, PINNED);
    }

    /// Het patroon van `SEED`, eerste acht bytes.
    const PINNED: [u8; 8] = [0x0d, 0xfb, 0xc6, 0x97, 0x82, 0x31, 0x0f, 0x05];

    #[test]
    fn percentiles_clamp() {
        let v: Vec<u64> = (1..=200).collect();
        assert_eq!(percentile(&v, 50), 101);
        assert_eq!(percentile(&v, 99), 199);
        assert_eq!(percentile(&v, 100), 200);
        assert_eq!(percentile(&[], 50), 0);
        assert_eq!(percentile(&[7], 99), 7);
    }

    #[test]
    fn mbps_is_decimal() {
        assert!((mbps(1_000_000, 1_000_000_000) - 1.0).abs() < 1e-9);
        assert_eq!(mbps(5, 0), 0.0);
    }

    #[test]
    fn peers_parse() {
        assert_eq!(parse_peer("10.100.0.3:9000"), Some(([10, 100, 0, 3], 9000)));
        assert_eq!(parse_peer("10.100.0:9000"), None);
        assert_eq!(parse_peer("10.100.0.3.4:9000"), None);
        assert_eq!(parse_peer("10.100.0.3:0"), None);
        assert_eq!(parse_peer("host:80"), None);
    }
}
