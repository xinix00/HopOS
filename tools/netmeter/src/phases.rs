//! De fasen: elk een meting tegen `apps/bench` op de node, met één
//! [`Outcome`] als uitkomst. Draden, geen async: dit is de host, en een
//! meetlat met een eigen runtime meet ook die runtime.

use crate::proto::{self, PATTERN_LEN, mbps, parse_sunk, percentile};
use std::io::{self, Read, Write};
use std::net::{SocketAddr, TcpStream, UdpSocket};
use std::thread;
use std::time::{Duration, Instant};

/// De langste stilte per op: een node die zo lang zwijgt, is weg.
const OP_CAP: Duration = Duration::from_secs(30);

/// De maat van één ping (zoals `apps/bench`: één frame).
const PING_LEN: usize = 64;

/// Wat de gebruiker vroeg.
#[derive(Clone, Debug)]
pub(crate) struct Plan {
    /// De node: IP en poort van de bench.
    pub(crate) addr: SocketAddr,
    /// Parallelle verbindingen voor `rtt`, `in` en `out`.
    pub(crate) conns: usize,
    /// Round-trips per verbinding in `rtt`.
    pub(crate) rtt: usize,
    /// Verbindingen in `storm`, één tegelijk.
    pub(crate) storm: usize,
    /// Bytes per verbinding in `in` en `out`.
    pub(crate) bytes: u64,
    /// Datagrammen in `udp`.
    pub(crate) udp: usize,
}

/// De uitkomst van één fase.
#[derive(Clone, Debug, Default)]
pub(crate) struct Outcome {
    /// De naam van de fase.
    pub(crate) phase: &'static str,
    /// Bytes die over de lijn gingen (payload).
    pub(crate) bytes: u64,
    /// De wandtijd van de fase, in milliseconden.
    pub(crate) ms: u64,
    /// De doorvoer in decimale MB/s (0 voor rtt, storm en udp).
    pub(crate) mbps: f64,
    /// Latenties in microseconden: min, p50, p90, p99, max.
    pub(crate) lat_us: Option<[u64; 5]>,
    /// Verbindingen per seconde (storm).
    pub(crate) conn_per_s: Option<f64>,
    /// Verloren datagrammen (udp) of foute bytes (out).
    pub(crate) lost: u64,
    /// Het getal van de node zelf (in: de MB/s die het slot mat).
    pub(crate) node_mbps: Option<f64>,
}

impl Outcome {
    /// De regel in de vorm van de Go-netmeter (`NETMETER phase=...`).
    pub(crate) fn line(&self) -> String {
        let mut s = format!(
            "NETMETER phase={} bytes={} ms={} MBps={:.2}",
            self.phase, self.bytes, self.ms, self.mbps
        );
        if let Some(n) = self.node_mbps {
            s += &format!(" nodeMBps={n:.2}");
        }
        if let Some([min, p50, p90, p99, max]) = self.lat_us {
            s += &format!(" min={min}us p50={p50}us p90={p90}us p99={p99}us max={max}us");
        }
        if let Some(c) = self.conn_per_s {
            s += &format!(" conn_per_s={c:.0}");
        }
        if self.phase == "udp" || self.phase == "out" {
            s += &format!(" lost={}", self.lost);
        }
        s
    }
}

/// Een verbinding met de bench, met de termijnen gezet en Nagle uit (een
/// ping van 64 bytes mag niet op een ACK wachten).
fn connect(addr: SocketAddr) -> io::Result<TcpStream> {
    let s = TcpStream::connect_timeout(&addr, OP_CAP)?;
    s.set_read_timeout(Some(OP_CAP))?;
    s.set_write_timeout(Some(OP_CAP))?;
    s.set_nodelay(true)?;
    Ok(s)
}

/// De latenties als `[min, p50, p90, p99, max]`.
fn spread(mut v: Vec<u64>) -> [u64; 5] {
    v.sort_unstable();
    [
        percentile(&v, 0),
        percentile(&v, 50),
        percentile(&v, 90),
        percentile(&v, 99),
        percentile(&v, 100),
    ]
}

/// Microseconden sinds `t0`.
fn us_since(t0: Instant) -> u64 {
    u64::try_from(t0.elapsed().as_micros()).unwrap_or(u64::MAX)
}

/// Draait `f` op `n` draden tegelijk en verzamelt de uitkomsten.
fn parallel<T: Send + 'static>(
    n: usize,
    f: impl Fn() -> io::Result<T> + Send + Sync + Clone + 'static,
) -> io::Result<Vec<T>> {
    let handles: Vec<_> = (0..n.max(1))
        .map(|_| {
            let f = f.clone();
            thread::spawn(f)
        })
        .collect();
    let mut out = Vec::with_capacity(handles.len());
    for h in handles {
        let r = h
            .join()
            .map_err(|_| io::Error::other("a measuring thread panicked"))?;
        out.push(r?);
    }
    Ok(out)
}

/// `rtt`: per verbinding `plan.rtt` round-trips van 64 bytes over één open
/// verbinding (de handshake zit er niet in), `plan.conns` verbindingen
/// tegelijk; de verdeling over alle samples.
pub(crate) fn rtt(plan: &Plan) -> io::Result<Outcome> {
    let (addr, n) = (plan.addr, plan.rtt);
    let t0 = Instant::now();
    let per = parallel(plan.conns, move || {
        let mut s = connect(addr)?;
        s.write_all(b"echo\n")?;
        let mut msg = [0x5au8; PING_LEN];
        let mut v = Vec::with_capacity(n);
        for _ in 0..n {
            let t = Instant::now();
            s.write_all(&msg)?;
            s.read_exact(&mut msg)?;
            v.push(us_since(t));
        }
        Ok(v)
    })?;
    let all: Vec<u64> = per.into_iter().flatten().collect();
    Ok(Outcome {
        phase: "rtt",
        bytes: (all.len() * PING_LEN * 2) as u64,
        ms: us_since(t0) / 1000,
        lat_us: Some(spread(all)),
        ..Outcome::default()
    })
}

/// `storm`: `plan.storm` verbindingen na elkaar, elk connect, één ping,
/// sluiten: de verbindingscyclus en verbindingen per seconde (de
/// "connection cycle, one at a time" en de storm van measurements.md).
pub(crate) fn storm(plan: &Plan) -> io::Result<Outcome> {
    let t0 = Instant::now();
    let mut v = Vec::with_capacity(plan.storm);
    for _ in 0..plan.storm {
        let t = Instant::now();
        let mut s = connect(plan.addr)?;
        let mut msg = [0x33u8; PING_LEN];
        s.write_all(b"echo\n")?;
        s.write_all(&msg)?;
        s.read_exact(&mut msg)?;
        drop(s);
        v.push(us_since(t));
    }
    let secs = t0.elapsed().as_secs_f64().max(1e-9);
    Ok(Outcome {
        phase: "storm",
        bytes: (v.len() * PING_LEN * 2) as u64,
        ms: us_since(t0) / 1000,
        conn_per_s: Some(v.len() as f64 / secs),
        lat_us: Some(spread(v)),
        ..Outcome::default()
    })
}

/// `in`: de host schrijft `plan.bytes` per verbinding naar `sink`; de
/// node meldt zijn eigen tijd terug. Het getal is de doorvoer DE NODE IN
/// ("Into the board" in measurements.md).
pub(crate) fn inbound(plan: &Plan) -> io::Result<Outcome> {
    let (addr, bytes) = (plan.addr, plan.bytes);
    let block = proto::pattern();
    let t0 = Instant::now();
    let per = parallel(plan.conns, move || {
        let mut s = connect(addr)?;
        s.write_all(format!("sink {bytes}\n").as_bytes())?;
        let mut left = bytes;
        while left > 0 {
            let n = usize::try_from(left.min(PATTERN_LEN as u64)).unwrap_or(PATTERN_LEN);
            s.write_all(&block[..n])?;
            left -= n as u64;
        }
        let mut reply = String::new();
        s.read_to_string(&mut reply)?;
        parse_sunk(&reply).ok_or_else(|| io::Error::other(format!("no sunk reply: {reply:?}")))
    })?;
    let ns = t0.elapsed().as_nanos();
    let total: u64 = per.iter().map(|(n, _)| n).sum();
    // Het getal van het slot: de traagste verbinding bepaalt de wandtijd.
    let node_us = per.iter().map(|(_, us)| *us).max().unwrap_or(0);
    Ok(Outcome {
        phase: "in",
        bytes: total,
        ms: u64::try_from(ns / 1_000_000).unwrap_or(u64::MAX),
        mbps: mbps(total, ns),
        node_mbps: Some(mbps(total, u128::from(node_us) * 1000)),
        ..Outcome::default()
    })
}

/// `out`: de node schrijft `plan.bytes` per verbinding uit `source`; de
/// host toetst elke byte tegen het patroon. De doorvoer DE NODE UIT.
pub(crate) fn outbound(plan: &Plan) -> io::Result<Outcome> {
    let (addr, bytes) = (plan.addr, plan.bytes);
    let t0 = Instant::now();
    let per = parallel(plan.conns, move || {
        let want = proto::pattern();
        let mut s = connect(addr)?;
        s.write_all(format!("source {bytes}\n").as_bytes())?;
        let mut buf = vec![0u8; 256 << 10];
        let (mut got, mut bad) = (0u64, 0u64);
        loop {
            let n = s.read(&mut buf)?;
            if n == 0 {
                break;
            }
            for (i, b) in buf[..n].iter().enumerate() {
                let at = ((got + i as u64) % PATTERN_LEN as u64) as usize;
                if want[at] != *b {
                    bad += 1;
                }
            }
            got += n as u64;
        }
        Ok((got, bad + bytes.saturating_sub(got)))
    })?;
    let ns = t0.elapsed().as_nanos();
    let total: u64 = per.iter().map(|(n, _)| n).sum();
    Ok(Outcome {
        phase: "out",
        bytes: total,
        ms: u64::try_from(ns / 1_000_000).unwrap_or(u64::MAX),
        mbps: mbps(total, ns),
        lost: per.iter().map(|(_, b)| b).sum(),
        ..Outcome::default()
    })
}

/// `udp`: `plan.udp` datagrammen van 64 bytes naar de UDP-echo, één
/// tegelijk; een echo die na een seconde niet terug is, telt als verloren.
pub(crate) fn udp(plan: &Plan) -> io::Result<Outcome> {
    let local: SocketAddr = if plan.addr.is_ipv4() {
        SocketAddr::from(([0, 0, 0, 0], 0))
    } else {
        SocketAddr::from(([0u16; 8], 0))
    };
    let sock = UdpSocket::bind(local)?;
    sock.connect(plan.addr)?;
    sock.set_read_timeout(Some(Duration::from_secs(1)))?;
    let t0 = Instant::now();
    let (mut v, mut lost) = (Vec::with_capacity(plan.udp), 0u64);
    let mut buf = [0u8; 2048];
    for i in 0..plan.udp {
        let mut msg = [0u8; PING_LEN];
        msg[..8].copy_from_slice(&(i as u64).to_le_bytes());
        let t = Instant::now();
        sock.send(&msg)?;
        // Een late echo van een vorig datagram is geen antwoord op dit.
        loop {
            match sock.recv(&mut buf) {
                Ok(n) if n == PING_LEN && buf[..8] == msg[..8] => {
                    v.push(us_since(t));
                    break;
                }
                Ok(_) => {}
                Err(e)
                    if matches!(
                        e.kind(),
                        io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut
                    ) =>
                {
                    lost += 1;
                    break;
                }
                Err(e) => return Err(e),
            }
        }
    }
    let got = v.len() as u64;
    Ok(Outcome {
        phase: "udp",
        bytes: got * PING_LEN as u64 * 2,
        ms: us_since(t0) / 1000,
        lat_us: (!v.is_empty()).then(|| spread(v)),
        lost,
        ..Outcome::default()
    })
}
