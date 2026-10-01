//! De client-rollen in het slot: meten tegen een andere bench op de node
//! (`BENCH_PEER=ip:poort`), over de switch van de kern. Dat is het pad dat
//! netmeter van buitenaf niet ziet: app naar app in één node ("Two Vitals
//! on the same M4 pulling from each other through the switch" in
//! docs/measurements.md).
//!
//! - `BENCH=ping`: [`RTT_N`] round-trips over één open verbinding (dus
//!   zonder handshake in de meting), dan de koude meting: één round-trip na
//!   een seconde stilte, [`COLD_N`] keer (de vorm van de Go-appspike:
//!   `BENCH_RTT` en `BENCH_COLD`).
//! - `BENCH=pull`: `BENCH_BYTES` van de peer (`source`), in MB/s.
//! - `BENCH=push`: `BENCH_BYTES` naar de peer (`sink`), in MB/s, met de
//!   tijd die de peer zelf mat.

use crate::proto::{self, CMD_MAX, PATTERN_LEN, parse_peer, percentile};
use crate::serve::format_into;
use alloc::vec::Vec;
use applib::appnet::{self, NetError, TcpStream};
use applib::{App, EXEC, clock, log};
use core::time::Duration;

/// Round-trips van de warme meting (de Go-appspike: 200).
const RTT_N: usize = 200;

/// Round-trips van de koude meting. De Go-appspike deed er 15; vijf
/// seconden stilte is genoeg om de adaptieve slaap te laten zakken.
const COLD_N: usize = 5;

/// De maat van één ping: 64 bytes, genoeg voor een kop en klein genoeg dat
/// hij één frame is.
const PING_LEN: usize = 64;

/// Zonder `BENCH_BYTES`: 64 MiB, de maat van de Go-tabellen.
const DEFAULT_BYTES: u64 = 64 << 20;

/// De langste wachttijd per op; een peer die zo lang zwijgt, is weg.
const OP_CAP: Duration = Duration::from_secs(30);

/// Hoe vaak de dial het opnieuw probeert: de peer mag nog opkomen (Go: 50
/// keer om de 100 ms).
const DIAL_TRIES: u32 = 50;

/// Wat een client-rol meet.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) enum Role {
    /// Round-trips.
    Ping,
    /// Doorvoer van de peer naar deze app.
    Pull,
    /// Doorvoer van deze app naar de peer.
    Push,
}

/// Draait `role` tegen de peer uit de env en zet de uitkomst op het log.
/// De exitcode van de app: 0 als de meting slaagde.
pub(crate) async fn run(app: &'static App, role: Role) -> u64 {
    let Some((ip, port)) = app.env("BENCH_PEER").and_then(parse_peer) else {
        log!("bench: BENCH_PEER is not ip:port HOPOS_BENCH_FAIL role={role:?}");
        return 1;
    };
    if let Err(e) = appnet::up(app) {
        log!("bench: network stack: {e} HOPOS_BENCH_FAIL role={role:?}");
        return 1;
    }
    let bytes = app
        .env("BENCH_BYTES")
        .and_then(|s| s.parse::<u64>().ok())
        .filter(|n| *n > 0 && *n <= proto::MAX_BYTES)
        .unwrap_or(DEFAULT_BYTES);
    let r = match role {
        Role::Ping => ping(ip, port).await,
        Role::Pull => pull(ip, port, bytes).await,
        Role::Push => push(ip, port, bytes).await,
    };
    match r {
        Ok(()) => 0,
        Err(e) => {
            log!("bench: {role:?} against the peer: {e} HOPOS_BENCH_FAIL role={role:?}");
            1
        }
    }
}

/// Verbindt met de peer; hij mag nog opkomen.
async fn dial(ip: [u8; 4], port: u16) -> Result<TcpStream, NetError> {
    let mut last = NetError::Timeout;
    for _ in 0..DIAL_TRIES {
        match TcpStream::connect_timeout(ip, port, Duration::from_secs(2)).await {
            Ok(s) => return Ok(s),
            Err(e) => last = e,
        }
        EXEC.after(Duration::from_millis(100)).await;
    }
    Err(last)
}

/// Stuurt de commandoregel.
async fn command(s: &mut TcpStream, args: core::fmt::Arguments<'_>) -> Result<(), NetError> {
    let mut line = [0u8; CMD_MAX];
    let n = format_into(&mut line, args);
    s.set_timeout(Some(OP_CAP));
    s.write_all(line.get(..n).unwrap_or_default()).await
}

/// Leest precies `buf.len()` bytes.
async fn read_exact(s: &mut TcpStream, buf: &mut [u8]) -> Result<(), NetError> {
    let mut at = 0;
    while at < buf.len() {
        s.set_timeout(Some(OP_CAP));
        let n = s.read(buf.get_mut(at..).unwrap_or_default()).await?;
        if n == 0 {
            return Err(NetError::Stack(appnet::StackError::Closed));
        }
        at += n;
    }
    Ok(())
}

/// Eén round-trip van [`PING_LEN`] bytes, in microseconden.
async fn round_trip(s: &mut TcpStream, msg: &mut [u8; PING_LEN]) -> Result<u64, NetError> {
    let t0 = clock::now_ns();
    s.set_timeout(Some(OP_CAP));
    s.write_all(msg).await?;
    read_exact(s, msg).await?;
    Ok(clock::now_ns().saturating_sub(t0) / 1000)
}

/// De warme en de koude rtt.
async fn ping(ip: [u8; 4], port: u16) -> Result<(), NetError> {
    let mut s = dial(ip, port).await?;
    command(&mut s, format_args!("echo\n")).await?;
    let mut msg = [0x5au8; PING_LEN];
    let mut rtt: Vec<u64> = Vec::new();
    rtt.try_reserve_exact(RTT_N)
        .map_err(|_| NetError::OutOfMemory { bytes: RTT_N * 8 })?;
    for _ in 0..RTT_N {
        rtt.push(round_trip(&mut s, &mut msg).await?);
    }
    rtt.sort_unstable();
    log!(
        "BENCH_RTT n={RTT_N} min={}us p50={}us p90={}us p99={}us max={}us HOPOS_BENCH_RTT",
        percentile(&rtt, 0),
        percentile(&rtt, 50),
        percentile(&rtt, 90),
        percentile(&rtt, 99),
        percentile(&rtt, 100)
    );
    let mut cold: Vec<u64> = Vec::new();
    cold.try_reserve_exact(COLD_N)
        .map_err(|_| NetError::OutOfMemory { bytes: COLD_N * 8 })?;
    for _ in 0..COLD_N {
        EXEC.after(Duration::from_secs(1)).await;
        cold.push(round_trip(&mut s, &mut msg).await?);
    }
    cold.sort_unstable();
    log!(
        "BENCH_COLD n={COLD_N} min={}us p50={}us max={}us HOPOS_BENCH_COLD",
        percentile(&cold, 0),
        percentile(&cold, 50),
        percentile(&cold, 100)
    );
    s.close()
}

/// `bytes` van de peer, met de patroontoets op elke byte.
async fn pull(ip: [u8; 4], port: u16, bytes: u64) -> Result<(), NetError> {
    let mut pattern = Vec::new();
    pattern
        .try_reserve_exact(PATTERN_LEN * 2)
        .map_err(|_| NetError::OutOfMemory {
            bytes: PATTERN_LEN * 2,
        })?;
    pattern.resize(PATTERN_LEN * 2, 0);
    let (want, buf) = pattern.split_at_mut(PATTERN_LEN);
    proto::fill_pattern(want);
    let mut s = dial(ip, port).await?;
    let t0 = clock::now_ns();
    command(&mut s, format_args!("source {bytes}\n")).await?;
    let (mut got, mut bad) = (0u64, 0u64);
    loop {
        s.set_timeout(Some(OP_CAP));
        let n = s.read(buf).await?;
        if n == 0 {
            break;
        }
        bad += mismatches(want, got, buf.get(..n).unwrap_or_default());
        got += n as u64;
    }
    let ns = clock::now_ns().saturating_sub(t0);
    log!(
        "BENCH_PULL bytes={got} want={bytes} ms={} MBps={:.2} bad={bad} HOPOS_BENCH_PULL",
        ns / 1_000_000,
        proto::mbps(got, ns)
    );
    // De tellers van de stack erbij: hoeveel segmenten de bytes kostten zegt
    // of de zender venster-beperkt was (Pi 4 01-10: 100 MB/s, en de vraag
    // was of het venster ooit boven de vloer van 16 KiB kwam).
    if let Some(net) = appnet::net()
        && let Ok(st) = net.stats()
    {
        log!(
            "BENCH_PULL_STATS segs_in={} bytes_in={} zero_windows={} rx_grown={} rx_grow_refused={} HOPOS_BENCH_PULL_STATS",
            st.tcp_segs_in,
            st.tcp_bytes_in,
            st.tcp_zero_windows,
            st.tcp_rx_grown,
            st.tcp_rx_grow_refused
        );
    }
    if got != bytes || bad != 0 {
        return Err(NetError::Stack(appnet::StackError::Closed));
    }
    Ok(())
}

/// Hoeveel bytes van `data` (vanaf stroompositie `pos`) niet het patroon
/// zijn. Per stuk tot de patroongrens één slice-vergelijking (een memcmp):
/// een lus per byte kostte op QEMU meer dan de overdracht zelf.
pub(crate) fn mismatches(pattern: &[u8], mut pos: u64, mut data: &[u8]) -> u64 {
    let len = pattern.len() as u64;
    let mut bad = 0u64;
    while !data.is_empty() && len > 0 {
        let at = (pos % len) as usize;
        let want = pattern.get(at..).unwrap_or_default();
        let n = want.len().min(data.len());
        let (got, rest) = data.split_at(n);
        let want = want.get(..n).unwrap_or_default();
        if got != want {
            bad += got.iter().zip(want).filter(|(a, b)| a != b).count() as u64;
        }
        data = rest;
        pos += n as u64;
    }
    bad
}

/// `bytes` naar de peer; de peer antwoordt met zijn eigen tijd.
async fn push(ip: [u8; 4], port: u16, bytes: u64) -> Result<(), NetError> {
    let mut block = Vec::new();
    block
        .try_reserve_exact(PATTERN_LEN)
        .map_err(|_| NetError::OutOfMemory { bytes: PATTERN_LEN })?;
    block.resize(PATTERN_LEN, 0);
    proto::fill_pattern(&mut block);
    let mut s = dial(ip, port).await?;
    let t0 = clock::now_ns();
    command(&mut s, format_args!("sink {bytes}\n")).await?;
    let mut left = bytes;
    while left > 0 {
        let n = usize::try_from(left.min(PATTERN_LEN as u64)).unwrap_or(PATTERN_LEN);
        s.set_timeout(Some(OP_CAP));
        s.write_all(block.get(..n).unwrap_or_default()).await?;
        left -= n as u64;
    }
    // Het antwoord van de peer: `sunk <n> <us>`.
    let mut line = [0u8; CMD_MAX];
    let mut at = 0;
    while at < line.len() && !line.get(..at).unwrap_or_default().contains(&b'\n') {
        s.set_timeout(Some(OP_CAP));
        let n = s.read(line.get_mut(at..).unwrap_or_default()).await?;
        if n == 0 {
            break;
        }
        at += n;
    }
    let ns = clock::now_ns().saturating_sub(t0);
    let reply = core::str::from_utf8(line.get(..at).unwrap_or_default())
        .unwrap_or("")
        .trim();
    log!(
        "BENCH_PUSH bytes={bytes} ms={} MBps={:.2} peer=\"{reply}\" HOPOS_BENCH_PUSH",
        ns / 1_000_000,
        proto::mbps(bytes, ns)
    );
    s.close()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mismatches_cross_the_pattern_boundary() {
        let mut p = [0u8; 16];
        proto::fill_pattern(&mut p);
        // Een stroom van 40 bytes vanaf positie 10: drie keer de grens over.
        let stream: Vec<u8> = (10..50).map(|i| p[i % 16]).collect();
        assert_eq!(mismatches(&p, 10, &stream), 0);
        let mut broken = stream.clone();
        broken[7] ^= 1;
        broken[39] ^= 0xff;
        assert_eq!(mismatches(&p, 10, &broken), 2);
        // Een verschoven stroom is fout, en dat zie je.
        assert!(mismatches(&p, 11, &stream) > 0);
    }
}
