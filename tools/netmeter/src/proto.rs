//! De host-kant van het meetprotocol van `apps/bench` (zie daar
//! `src/proto.rs` voor de tabel): de commandoregels, het patroon en de
//! rekensommen. De eerste acht patroonbytes zijn aan beide kanten gepind,
//! zodat een wijziging aan één kant niet stil de toets van `out` breekt.

/// De maat van het patroonblok dat `source` herhaalt.
pub(crate) const PATTERN_LEN: usize = 64 << 10;

/// Het zaad: "HPOS", zoals in `apps/bench` en in `serveUp` van de
/// Go-netmeter.
const SEED: u64 = 0x4850_4f53;

/// Het patroonblok.
pub(crate) fn pattern() -> Vec<u8> {
    let mut buf = vec![0u8; PATTERN_LEN];
    let mut x = SEED;
    for b in &mut buf {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        *b = x as u8;
    }
    buf
}

/// Het `p`-de percentiel van een oplopend gesorteerde reeks: index
/// `n * p / 100`, geklemd (zoals `BENCH_RTT` in de Go-appspike en in
/// `apps/bench`).
pub(crate) fn percentile(sorted: &[u64], p: usize) -> u64 {
    let n = sorted.len();
    if n == 0 {
        return 0;
    }
    let i = (n.saturating_mul(p) / 100).min(n - 1);
    sorted[i]
}

/// Decimale MB/s (docs/measurements.md). Let op: de Go-netmeter rekende
/// in MiB/s (`n / (1<<20)`); dat is 4,9 % minder. De tabellen van de
/// Go-generatie in measurements.md zijn decimaal, en daar wordt tegen
/// gelegd.
pub(crate) fn mbps(bytes: u64, ns: u128) -> f64 {
    if ns == 0 {
        return 0.0;
    }
    bytes as f64 * 1000.0 / ns as f64
}

/// Leest `sunk <n> <us>` van de server: de bytes en de tijd die het slot
/// zelf mat.
pub(crate) fn parse_sunk(line: &str) -> Option<(u64, u64)> {
    let mut it = line.split_ascii_whitespace();
    if it.next()? != "sunk" {
        return None;
    }
    Some((it.next()?.parse().ok()?, it.next()?.parse().ok()?))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_pattern_is_pinned() {
        // Dezelfde acht bytes als in apps/bench/src/proto.rs.
        assert_eq!(
            pattern()[..8],
            [0x0d, 0xfb, 0xc6, 0x97, 0x82, 0x31, 0x0f, 0x05]
        );
        assert_eq!(pattern().len(), PATTERN_LEN);
    }

    #[test]
    fn percentiles_and_rates() {
        let v: Vec<u64> = (1..=200).collect();
        assert_eq!(percentile(&v, 50), 101);
        assert_eq!(percentile(&v, 99), 199);
        assert_eq!(percentile(&[], 99), 0);
        assert!((mbps(118_000_000, 1_000_000_000) - 118.0).abs() < 1e-9);
    }

    #[test]
    fn sunk_parses() {
        assert_eq!(parse_sunk("sunk 1048576 2500\n"), Some((1 << 20, 2500)));
        assert_eq!(parse_sunk("sunk x 1"), None);
        assert_eq!(parse_sunk("nope 1 2"), None);
    }
}
