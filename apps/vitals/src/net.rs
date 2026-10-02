//! De netwerktests. De kern levert geen netstats per app, dus vitals meet
//! zijn eigen draadverkeer:
//!
//! - rx: download van een externe bron, de doorvoer door de hele keten
//!   (app-stack, switch, NAT, NIC, internet);
//! - tx: client-gedreven, `curl -o /dev/null http://node:poort/blob?mb=64`;
//!   de handler klokt zijn eigen zendkant ([`serve_blob`]);
//! - up: client-gedreven, een reeks PUTs van 1 MiB naar `/sink`
//!   ([`serve_sink`], `perf.sh`), de tegenhanger van tx;
//! - storm: veel korte verbindingen naar de eigen `/ping`;
//! - rtt: kale TCP-handshakes naar de gateway, de vloer van het interne pad.
//!
//! De werkers van rx en storm zijn eigen taken op core 0 (een verbinding
//! per taak, handboek §2); hun tellers staan in een `Local` van dit module
//! ([`RX`], [`STORM`]), die de test voor elke run leegmaakt. Er loopt
//! hooguit één test tegelijk, dus nooit twee runs op dezelfde tellers.

use crate::Shared;
use crate::report::{ERR_CAP, Report, Short, pct, secs};
use crate::run::{self, BOARD, Params, Test, note};
use alloc::string::String;
use alloc::vec::Vec;
use applib::appnet::{self, TcpStream};
use applib::rt::Exec;
use applib::tcp::{Dialer, TcpConn};
use applib::{EXEC, clock};
use core::cell::{Cell, RefCell};
use core::fmt::Write;
use core::time::Duration;
use leanhttp::{Call, Exchange, Header};
use sync::{Either, Local, select};

/// Een publiek bestand van 100 MB over plain http (leanhttp linkt bewust
/// geen TLS). De test leest er standaard 32 MB van; `VITALS_RX_URL` of
/// `?url=` kiest een andere bron, bijvoorbeeld een buur op het LAN.
pub(crate) const DEFAULT_RX_URL: &str = "http://cachefly.cachefly.net/100mb.test";

/// Zolang mag een verbinding stil zijn voor rx hem stilgevallen noemt.
const STALL: Duration = Duration::from_secs(10);

/// De termijn van een dial.
const CONNECT: Duration = Duration::from_secs(5);

/// De leesbuffer per rx-verbinding.
const RX_BUF: usize = 64 << 10;

/// De leesbuffer van `/sink`.
const SINK_BUF: usize = 64 << 10;

/// Het doel van rtt zonder `?addr=`: de kern op het slot-LAN, zijn
/// system-poort (de enige poort die de gateway zeker open heeft).
const RTT_PORT: u16 = applib::sys::ADDRESS.1;

/// Een kopie van `s`, of `None` als de heap nee zei.
fn owned(s: &str) -> Option<String> {
    let mut o = String::new();
    o.try_reserve_exact(s.len()).ok()?;
    o.push_str(s);
    Some(o)
}

/// De tellers van één rx-run.
struct RxState {
    /// Bytes binnen, over alle verbindingen.
    got: Cell<u64>,
    /// De langste wacht op een antwoordkop.
    header_ns: Cell<u64>,
    /// Klaar gemelde verbindingen.
    done: Cell<u64>,
    /// De laatste finish.
    end_ns: Cell<u64>,
    /// De eerste fout.
    err: RefCell<Short<ERR_CAP>>,
}

/// De tellers van rx.
static RX: Local<RxState> = Local::new(RxState {
    got: Cell::new(0),
    header_ns: Cell::new(0),
    done: Cell::new(0),
    end_ns: Cell::new(0),
    err: RefCell::new(Short::new()),
});

/// Zet de eerste fout.
fn first_err(cell: &RefCell<Short<ERR_CAP>>, args: core::fmt::Arguments<'_>) {
    if let Ok(mut e) = cell.try_borrow_mut()
        && e.is_empty()
    {
        let _ = e.write_fmt(args);
    }
}

/// rx: download (een stuk van) een groot bestand en meet de doorvoer.
/// `?n=` parallelle verbindingen, elk een eigen GET van `mb/n`: één socket
/// haalt op een langzaam board de draad niet vol terwijl zijn cores idle
/// staan (het plafond is venster gedeeld door round-trip, per verbinding).
pub(crate) async fn rx(sh: &'static Shared, r: &mut Report, p: &Params) {
    let url = p.url.as_deref().unwrap_or(sh.rx_url);
    let conns = Params::int(p.n, 1, 1, 8);
    let want = Params::int(p.mb, 32, 1, 1024) << 20;
    let share = want / conns;
    let want = share * conns;
    let st = RX.get();
    st.got.set(0);
    st.header_ns.set(0);
    st.done.set(0);
    st.end_ns.set(0);
    if let Ok(mut e) = st.err.try_borrow_mut() {
        *e = Short::new();
    }
    let t0 = clock::now_ns();
    let mut started = 0;
    for _ in 0..conns {
        let Some(u) = owned(url) else { break };
        if sh.exec.spawn(rx_conn(sh.exec, u, share)).is_err() {
            break;
        }
        started += 1;
    }
    if started < conns {
        first_err(
            &st.err,
            format_args!("started {started} of {conns} connections (no room)"),
        );
    }
    while st.done.get() < started {
        note(format_args!(
            "rx {} of {} MB",
            st.got.get() >> 20,
            want >> 20
        ));
        EXEC.after(Duration::from_millis(100)).await;
    }
    let el = secs(st.end_ns.get().saturating_sub(t0));
    let got = st.got.get();
    if let Ok(e) = st.err.try_borrow()
        && !e.is_empty()
    {
        r.fail(format_args!("{}", e.as_str()));
        return;
    }
    if got < want {
        r.fail(format_args!(
            "short benchmark response: read {got} bytes, requested {want}"
        ));
        return;
    }
    r.add("throughput", got as f64 / el / 1e6, "MB/s");
    r.add("read", (got >> 20) as f64, "MB");
    r.add("header", st.header_ns.get() as f64 / 1e6, "ms");
    r.add("conns", conns as f64, "");
    r.line(format_args!(
        "{url} ({conns} connection(s), read {} MB)",
        got >> 20
    ));
}

/// Eén rx-verbinding als taak: de uitkomst in [`RX`].
async fn rx_conn(exec: &'static Exec, url: String, share: u64) {
    let st = RX.get();
    if let Err(e) = rx_one(exec, &url, share).await {
        first_err(&st.err, format_args!("{e}"));
    }
    st.end_ns.set(st.end_ns.get().max(clock::now_ns()));
    st.done.set(st.done.get() + 1);
}

/// Waarom een verbinding niet af kwam.
enum RxError {
    /// HTTP of de verbinding.
    Http(leanhttp::Error),
    /// De server zei geen 200.
    Status(u16),
    /// Te lang niets binnen.
    Stalled(u64),
    /// Geen heap voor de leesbuffer.
    NoBuffer,
}

impl core::fmt::Display for RxError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            RxError::Http(e) => write!(f, "http: {e}"),
            RxError::Status(s) => write!(f, "http status {s} (want 200)"),
            RxError::Stalled(got) => write!(
                f,
                "stalled: nothing for {} s after {got} bytes",
                STALL.as_secs()
            ),
            RxError::NoBuffer => write!(f, "no heap for a {RX_BUF}-byte buffer"),
        }
    }
}

/// Eén GET van `share` bytes.
async fn rx_one(exec: &'static Exec, url: &str, share: u64) -> Result<(), RxError> {
    let st = RX.get();
    let mut buf = Vec::new();
    buf.try_reserve_exact(RX_BUF)
        .map_err(|_| RxError::NoBuffer)?;
    buf.resize(RX_BUF, 0);
    let mut header = Header::new();
    header
        .set("User-Agent", "HopOS-vitals")
        .map_err(RxError::Http)?;
    let h0 = clock::now_ns();
    let call = Call {
        url,
        header,
        header_timeout: Some(STALL),
        ..Call::default()
    };
    let mut d = Dialer {
        exec,
        connect: CONNECT,
    };
    let mut resp = leanhttp::fetch(&mut d, call).await.map_err(RxError::Http)?;
    if resp.status != 200 {
        return Err(RxError::Status(resp.status));
    }
    st.header_ns
        .set(st.header_ns.get().max(clock::now_ns().saturating_sub(h0)));
    let mut got = 0u64;
    while got < share {
        let want = usize::try_from(share - got).unwrap_or(RX_BUF).min(RX_BUF);
        let dst = buf.get_mut(..want).unwrap_or_default();
        match select(resp.read(dst), exec.after(STALL)).await {
            Either::Left(Ok(0)) => break,
            Either::Left(Ok(n)) => {
                got += n as u64;
                st.got.set(st.got.get() + n as u64);
            }
            Either::Left(Err(e)) => return Err(RxError::Http(e)),
            Either::Right(()) => return Err(RxError::Stalled(got)),
        }
    }
    Ok(())
}

/// De tellers van één storm-run.
struct StormState {
    /// Het volgende verzoeknummer.
    next: Cell<u64>,
    /// Mislukte verzoeken.
    errs: Cell<u64>,
    /// Klaar gemelde werkers.
    done: Cell<u64>,
    /// De duur van elk gelukt verzoek in µs, vooraf gereserveerd.
    durs: RefCell<Vec<u32>>,
}

/// De tellers van storm.
static STORM: Local<StormState> = Local::new(StormState {
    next: Cell::new(0),
    errs: Cell::new(0),
    done: Cell::new(0),
    durs: RefCell::new(Vec::new()),
});

/// storm: veel korte GETs op `/ping`, elk een hele verbinding (dial, GET,
/// sluiten). Standaard op het eigen adres en de eigen poort: dan meet hij
/// de app-stack en zijn server, client en server in één slot (een
/// ondergrens). `?addr=ip:poort` of `VITALS_STORM_ADDR` kiest een ander
/// doel, zoals de gepubliceerde poort op het adres van de node (de
/// hairpin door switch en NAT). Zuiver meten is van buitenaf naar `/ping`
/// stormen (tools/netmeter).
pub(crate) async fn storm(sh: &'static Shared, r: &mut Report, p: &Params) {
    let target = match p.addr.as_deref().or(sh.storm) {
        Some(a) => match appnet::parse_addr(a) {
            Some(t) => t,
            None => {
                r.fail(format_args!("addr {a:?} is not ip:port"));
                return;
            }
        },
        None => (sh.ip, sh.port),
    };
    let workers = Params::int(p.n, 8, 1, 16);
    let total = Params::int(p.reqs, 200, 10, 10_000);
    let st = STORM.get();
    st.next.set(0);
    st.errs.set(0);
    st.done.set(0);
    {
        let Ok(mut d) = st.durs.try_borrow_mut() else {
            r.fail(format_args!("storm table busy"));
            return;
        };
        d.clear();
        let need = usize::try_from(total).unwrap_or(0);
        if d.capacity() < need && d.try_reserve_exact(need).is_err() {
            r.fail(format_args!("no heap for {total} samples"));
            return;
        }
    }
    let t0 = clock::now_ns();
    let mut started = 0;
    for _ in 0..workers {
        if sh.exec.spawn(storm_worker(sh.exec, target, total)).is_err() {
            break;
        }
        started += 1;
    }
    while st.done.get() < started {
        EXEC.after(Duration::from_millis(20)).await;
    }
    let el = secs(clock::now_ns().saturating_sub(t0));
    let Ok(mut d) = st.durs.try_borrow_mut() else {
        r.fail(format_args!("storm table busy"));
        return;
    };
    let ok = d.len();
    let ms = |us: u32| f64::from(us) / 1000.0;
    r.add("rate", ok as f64 / el, "conn/s");
    r.add("p50", ms(pct(&mut d, 50)), "ms");
    r.add("p90", ms(pct(&mut d, 90)), "ms");
    r.add("p99", ms(pct(&mut d, 99)), "ms");
    r.add("errors", st.errs.get() as f64, "");
    drop(d);
    let ([a, b, c, dd], port) = target;
    r.line(format_args!(
        "{total} requests, {started} workers, target http://{a}.{b}.{c}.{dd}:{port}/ping"
    ));
    r.line(format_args!(
        "each request is one full connection (dial + GET + close)"
    ));
    if started < workers {
        r.line(format_args!("only {started} of {workers} workers started"));
    }
}

/// Eén storm-werker: verzoeken tot het totaal op is.
async fn storm_worker(exec: &'static Exec, target: ([u8; 4], u16), total: u64) {
    let st = STORM.get();
    loop {
        let k = st.next.get() + 1;
        if k > total {
            break;
        }
        st.next.set(k);
        if k.is_multiple_of(50) {
            note(format_args!("storm {k}/{total}"));
        }
        let t = clock::now_ns();
        if ping(exec, target).await.is_ok() {
            let us = u32::try_from(clock::now_ns().saturating_sub(t) / 1000).unwrap_or(u32::MAX);
            if let Ok(mut d) = st.durs.try_borrow_mut()
                && d.len() < d.capacity()
            {
                d.push(us);
            }
        } else {
            st.errs.set(st.errs.get() + 1);
        }
    }
    st.done.set(st.done.get() + 1);
}

/// Eén verbinding: dial, `GET /ping`, het antwoord lezen, sluiten.
async fn ping(exec: &'static Exec, ([a, b, c, d], port): ([u8; 4], u16)) -> Result<(), ()> {
    let s = TcpStream::connect_timeout([a, b, c, d], port, CONNECT)
        .await
        .map_err(|_| ())?;
    let mut url = Short::<48>::new();
    let _ = write!(url, "http://{a}.{b}.{c}.{d}:{port}/ping");
    let call = Call {
        url: url.as_str(),
        header_timeout: Some(STALL),
        ..Call::default()
    };
    let mut resp = leanhttp::send(TcpConn::new(s, exec), call)
        .await
        .map_err(|_| ())?;
    if resp.status != 200 {
        return Err(());
    }
    let mut buf = [0u8; 16];
    loop {
        match select(resp.read(&mut buf), exec.after(STALL)).await {
            Either::Left(Ok(0)) => break,
            Either::Left(Ok(_)) => {}
            Either::Left(Err(_)) | Either::Right(()) => return Err(()),
        }
    }
    // Sluiten, ook als hij herbruikbaar was: elk verzoek is een verbinding.
    drop(resp.release().await);
    Ok(())
}

/// rtt: kale TCP-handshakes (connect en close, 5 ms uit elkaar) naar de
/// gateway: geen HTTP, geen handler, de vloer van het interne pad. Het doel
/// is de kern op 10.100.0.1 op zijn system-poort, of `?addr=ip:poort`.
pub(crate) async fn rtt(_sh: &'static Shared, r: &mut Report, p: &Params) {
    let (ip, port) = match p.addr.as_deref() {
        Some(a) => match appnet::parse_addr(a) {
            Some(t) => t,
            None => {
                r.fail(format_args!("addr {a:?} is not ip:port"));
                return;
            }
        },
        None => (appnet::HOST, RTT_PORT),
    };
    let count = Params::int(p.n, 100, 10, 2000);
    let mut durs: Vec<u32> = Vec::new();
    if durs
        .try_reserve_exact(usize::try_from(count).unwrap_or(0))
        .is_err()
    {
        r.fail(format_args!("no heap for {count} samples"));
        return;
    }
    let mut errs = 0u64;
    for k in 0..count {
        let t = clock::now_ns();
        match TcpStream::connect_timeout(ip, port, CONNECT).await {
            Ok(s) => {
                let us =
                    u32::try_from(clock::now_ns().saturating_sub(t) / 1000).unwrap_or(u32::MAX);
                if durs.len() < durs.capacity() {
                    durs.push(us);
                }
                let _ = s.close();
            }
            Err(_) => errs += 1,
        }
        if k.is_multiple_of(25) {
            note(format_args!("rtt {k}/{count}"));
        }
        EXEC.after(Duration::from_millis(5)).await;
    }
    let [a, b, c, d] = ip;
    r.add("p50", f64::from(pct(&mut durs, 50)), "us");
    r.add("p99", f64::from(pct(&mut durs, 99)), "us");
    r.add("max", f64::from(pct(&mut durs, 100)), "us");
    r.add("errors", errs as f64, "");
    r.line(format_args!(
        "{count} TCP dials to {a}.{b}.{c}.{d}:{port} (connect + close, 5 ms apart)"
    ));
}

/// `GET /blob?mb=N`: streamt N MB (standaard 32) en klokt zijn eigen
/// zendkant. De TX-tegenhanger van rx, gedreven door een client die je zelf
/// kiest; het resultaat komt als "tx" in de tabel.
pub(crate) async fn serve_blob(
    ex: &mut Exchange<'_, TcpConn>,
    sh: &'static Shared,
) -> leanhttp::Result {
    let mb = Params::int(
        ex.req
            .query("mb")
            .ok()
            .flatten()
            .and_then(|v| v.parse().ok()),
        32,
        1,
        1024,
    );
    let total = mb << 20;
    let mut len = Short::<24>::new();
    let _ = write!(len, "{total}");
    // Content-Length vooraf: dan schrijft leanhttp direct door, zonder
    // chunking, en kan curl de voortgang tonen.
    ex.header_mut()
        .set("Content-Type", "application/octet-stream")?;
    ex.header_mut().set("Content-Length", len.as_str())?;
    let mut r = Report::new(Test::Tx.name(), sh.uptime_ns());
    let t0 = clock::now_ns();
    let mut sent = 0u64;
    let mut failed = None;
    while sent < total {
        let n = usize::try_from(total - sent)
            .unwrap_or(sh.blob.len())
            .min(sh.blob.len());
        match ex.write(sh.blob.get(..n).unwrap_or_default()).await {
            Ok(m) => sent += m as u64,
            Err(e) => {
                failed = Some(e);
                break;
            }
        }
    }
    let el = clock::now_ns().saturating_sub(t0);
    r.duration_ns = el;
    r.add("throughput", sent as f64 / secs(el) / 1e6, "MB/s");
    r.add("sent", (sent >> 20) as f64, "MB");
    r.line(format_args!("{} MB of {mb} MB", sent >> 20));
    if let Some(e) = failed {
        r.fail(format_args!("client went away: {e}"));
    }
    run::store(Test::Tx, r);
    match failed {
        Some(e) => Err(e),
        None => Ok(()),
    }
}

/// Om de zoveel requests van een reeks één regel op de console.
const UP_LOG_EVERY: u64 = 16;

/// `PUT /sink`: ontvangt een body (tot 1 MiB, de grens van leanhttp) en
/// gooit hem weg. De ontvangkant van het netwerkpad; het resultaat komt als
/// "up" in de tabel en beslaat de hele reeks: bytes gedeeld door de
/// wandtijd van de eerste tot de laatste request, dus met de gaten die de
/// client laat vallen.
pub(crate) async fn serve_sink(
    ex: &mut Exchange<'_, TcpConn>,
    sh: &'static Shared,
) -> leanhttp::Result {
    let mut buf = Vec::new();
    if buf.try_reserve_exact(SINK_BUF).is_err() {
        return ex.error(503, "out of memory").await;
    }
    buf.resize(SINK_BUF, 0);
    let t0 = sh.uptime_ns();
    let mut got = 0u64;
    let mut failed = None;
    loop {
        match ex.read_body(&mut buf).await {
            Ok(0) => break,
            Ok(n) => got += n as u64,
            Err(e) => {
                failed = Some(e);
                break;
            }
        }
    }
    let t1 = sh.uptime_ns();
    let burst = {
        let Ok(mut b) = BOARD.get().try_borrow_mut() else {
            return ex.error(503, "busy").await;
        };
        b.up.add(t0, t1, got);
        b.up
    };
    let mut r = Report::new(Test::Up.name(), burst.start_ns);
    r.duration_ns = burst.end_ns.saturating_sub(burst.start_ns);
    r.add(
        "throughput",
        burst.bytes as f64 / secs(r.duration_ns) / 1e6,
        "MB/s",
    );
    r.add("received", burst.bytes as f64 / f64::from(1u32 << 20), "MB");
    r.add("requests", burst.reqs as f64, "");
    r.line(format_args!(
        "{} request(s), {} MB in {:.2}s wall time (bodies of up to 1 MiB, leanhttp's limit)",
        burst.reqs,
        burst.bytes >> 20,
        secs(r.duration_ns)
    ));
    if let Some(e) = failed {
        r.fail(format_args!("client went away: {e}"));
    }
    if burst.reqs == 1 || burst.reqs.is_multiple_of(UP_LOG_EVERY) || failed.is_some() {
        run::store(Test::Up, r);
    } else {
        run::keep(Test::Up, r);
    }
    if let Some(e) = failed {
        return Err(e);
    }
    let mut out = Short::<96>::new();
    let _ = writeln!(
        out,
        "{{\"received\":{got},\"burst_mb\":{},\"burst_seconds\":{:.3}}}",
        burst.bytes >> 20,
        secs(burst.end_ns.saturating_sub(burst.start_ns))
    );
    ex.header_mut().set("Content-Type", "application/json")?;
    ex.write(out.as_str().as_bytes()).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_rtt_target_is_the_system_port_of_the_gateway() {
        assert_eq!(appnet::HOST, [10, 100, 0, 1]);
        assert_eq!(RTT_PORT, 10100);
    }
}
