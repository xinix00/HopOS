//! vitals: de board-dokter. Eén app die de vitale functies van een node
//! meet en benchmarkt: idle-gedrag, rekenen (ook volgehouden, met de
//! temperatuur ernaast), geheugen, netwerk, schijf en timers. Voor bring-up
//! en diagnose van een nieuw board: plaats vitals, open de pagina, druk op
//! **Run all**, en vergelijk het rapport met een gezond board.
//!
//! De port van `OLD/apps/vitals` (Go). De tabel van tests staat in
//! [`run::Test`] en in de README; wat wegviel (sqlite, gc, ramp, push,
//! syscall) staat daar met het waarom.
//!
//! Eén HTTP-server (leanhttp over `appnet::TcpListener`) op de poort uit
//! `ER_PORT_HTTP` (standaard 8080):
//!
//! ```text
//! GET  /               de pagina: Run all, per test een knop, de cijfers
//! GET  /api/state      alles als JSON (node, idle, temperatuur, resultaten)
//! GET  /api/run?test=  een test starten, of test=all; 409 als er een loopt
//! GET  /ping           "pong": het doelwit van de storm-test
//! GET  /blob?mb=N      N MB naar de client, de zendkant ("tx")
//! PUT  /sink           een body tot 1 MiB, de ontvangkant ("up")
//! GET  /health         "ok"
//! ```
//!
//! De vorm is die van welcome (handboek §2): één taak accepteert en geeft
//! elke verbinding als waarde aan een vrije werker uit een vaste pool van
//! [`WORKERS`]. Een test draait als eigen taak ([`run::start`]), één
//! tegelijk; de rekentests over meer cores zetten hun werk met
//! `smp::spawn_on` op de andere cores van de app.
//!
//! Markers: `HOPOS_VITALS_UP port=<n>` als de listener staat, en per test
//! `HOPOS_VITALS_<TEST>` met de meetwaarden als `key=value` (docs/measurements.md,
//! tabel Vitals).

#![cfg_attr(target_os = "none", no_std, no_main)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

extern crate alloc;

mod cpu;
mod disk;
mod idle;
mod mem;
mod net;
mod page;
mod report;
mod run;

use alloc::vec::Vec;
use applib::app::port_of;
use applib::appnet::{self, Net, TcpListener, TcpStream};
use applib::rt::Exec;
use applib::tcp::TcpConn;
use applib::{App, EXEC, clock, log};
use core::time::Duration;
use leanhttp::{Exchange, Found, Mux};
use sync::{Doors, select};

applib::main!(vitals);

/// Op de host bestaat dit image niet: daar is dit een lege binary, zodat de
/// host-poort de rekenkant kan toetsen en clippy de rest kan lezen.
#[cfg(not(target_os = "none"))]
fn main() {}

/// De poort zonder `ER_PORT_HTTP`, zoals de Go-vitals.
const DEFAULT_PORT: u16 = 8080;

/// De werkers. Vier voor een browser, en ruimte voor de storm-test, die
/// zijn eigen poort vanuit hetzelfde slot bestormt (client én server hier).
const WORKERS: usize = 8;

/// De langste stilte op een keep-alive-verbinding: met een vaste pool houdt
/// een stille browser anders een werker een minuut vast.
const READ_CAP: Duration = Duration::from_secs(5);

/// De maat van het patroonblok dat `/blob` en de disk-test herhalen.
const BLOB: usize = 256 << 10;

/// De versie van dit image (die van de werkruimte).
const VERSION: &str = env!("CARGO_PKG_VERSION");

/// De architectuur, voor de pagina.
const ARCH: &str = if cfg!(target_arch = "riscv64") {
    "riscv64"
} else {
    "arm64"
};

/// De deuren van de werkers (zie welcome): met alle werkers bezet wacht
/// de acceptor op de eerste die vrijkomt.
static DOORS: Doors<TcpStream, WORKERS> = Doors::new();

/// De paden van deze server.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
enum Route {
    /// `GET /`.
    Page,
    /// `GET /health`.
    Health,
    /// `GET /api/state`.
    State,
    /// `GET /api/run`.
    Run,
    /// `GET /ping`.
    Ping,
    /// `GET /blob`.
    Blob,
    /// `PUT /sink`, `POST /sink`.
    Sink,
}

/// De routetabel.
const ROUTES: [(&str, Route); 9] = [
    ("GET /", Route::Page),
    ("GET /health", Route::Health),
    ("GET /api/state", Route::State),
    ("GET /api/run", Route::Run),
    ("GET /ping", Route::Ping),
    ("GET /blob", Route::Blob),
    ("PUT /sink", Route::Sink),
    ("POST /sink", Route::Sink),
    ("GET /sink", Route::Sink),
];

/// Wat elke werker en elke test deelt: de feiten van de start. Gezet in
/// [`vitals`] vóór de eerste spawn en daarna alleen gelezen.
pub(crate) struct Shared {
    /// De executor van core 0.
    pub(crate) exec: &'static Exec,
    /// Het handvat van de app.
    pub(crate) app: &'static App,
    /// De netstack.
    pub(crate) net: &'static Net,
    /// De naam van de node (`ER_ATTR_NODE_ID` of `HOPOS_NODE`).
    pub(crate) node: &'static str,
    /// Het slot-IP.
    pub(crate) ip: [u8; 4],
    /// De gepubliceerde poort.
    pub(crate) port: u16,
    /// De bron van de rx-test.
    pub(crate) rx_url: &'static str,
    /// Het doel van de storm-test als `ip:poort`, of `None` voor de eigen
    /// poort op het eigen adres.
    pub(crate) storm: Option<&'static str>,
    /// De start van de app, in ns.
    pub(crate) started_ns: u64,
    /// Het patroonblok van [`BLOB`] bytes.
    pub(crate) blob: &'static [u8],
    mux: Mux<Route>,
}

impl Shared {
    /// Nanoseconden sinds de start van de app.
    pub(crate) fn uptime_ns(&self) -> u64 {
        clock::now_ns().saturating_sub(self.started_ns)
    }
}

#[expect(
    clippy::expect_used,
    reason = "de start van de bin: zonder netstack, poort of patroonblok is er niets te meten, en een luide paniek met reden is het goede einde"
)]
async fn vitals(app: &'static App) {
    let exec: &'static Exec = EXEC.get();
    let port = port_of(app.env("ER_PORT_HTTP"), DEFAULT_PORT);
    let net = appnet::up(app).expect("vitals: network stack");
    let listener = TcpListener::bind(port).expect("vitals: listen on ER_PORT_HTTP");
    let mut mux = Mux::default();
    for (pattern, route) in ROUTES {
        mux.handle(pattern, route).expect("vitals: route");
    }
    let node = app
        .env("ER_ATTR_NODE_ID")
        .or_else(|| app.env("HOPOS_NODE"))
        .unwrap_or("this node");
    let mut blob = Vec::new();
    blob.try_reserve_exact(BLOB).expect("vitals: pattern block");
    blob.resize(BLOB, 0);
    fill_pattern(&mut blob);
    // Eén keer bij de start, voor de levensduur van de app: de werkers en
    // de tests lenen het, niemand schrijft het nog.
    let shared: &'static Shared = alloc::boxed::Box::leak(alloc::boxed::Box::new(Shared {
        exec,
        app,
        net,
        node,
        ip: net.ip(),
        port,
        rx_url: app
            .env("VITALS_RX_URL")
            .filter(|u| !u.is_empty())
            .unwrap_or(net::DEFAULT_RX_URL),
        storm: app.env("VITALS_STORM_ADDR").filter(|a| !a.is_empty()),
        started_ns: clock::now_ns(),
        blob: alloc::boxed::Box::leak(blob.into_boxed_slice()),
        mux,
    }));
    exec.spawn(idle::sample(app))
        .expect("vitals: spawn idle sampler");
    for i in 0..WORKERS {
        exec.spawn(worker(i, shared)).expect("vitals: spawn worker");
    }
    let [a, b, c, d] = shared.ip;
    log!(
        "vitals {VERSION}: serving http on {a}.{b}.{c}.{d}:{port} for node {node}, slot {}, {ARCH}, {} core(s) HOPOS_VITALS_UP port={port}",
        app.slot(),
        app.cores()
    );
    // Tot de kern vraagt te stoppen; de main-schil doet dan het
    // net-afscheid (elke verbinding dicht) en de exit.
    select(app.stopped(), accept(listener, exec)).await;
}

/// Vult `buf` met een xorshift-patroon: de inhoud doet er niet toe (niemand
/// pakt hem uit), maar hij comprimeert niet en een verschoven lees valt op.
fn fill_pattern(buf: &mut [u8]) {
    let mut r: u64 = 2_463_534_242;
    for b in buf {
        r ^= r << 13;
        r ^= r >> 7;
        r ^= r << 17;
        *b = r.to_le_bytes()[0];
    }
}

/// De acceptor: elke verbinding naar de eerste vrije werker.
async fn accept(listener: TcpListener, exec: &'static Exec) {
    loop {
        match listener.accept().await {
            Ok(stream) => DOORS.place(stream).await,
            Err(e) => {
                log!("vitals: accept: {e} HOPOS_VITALS_ACCEPT");
                exec.after(Duration::from_millis(100)).await;
            }
        }
    }
}

/// Eén werker: wacht op een verbinding, bedient hem met leanhttp tot hij
/// sluit, en meldt zich weer vrij.
async fn worker(i: usize, shared: &'static Shared) {
    loop {
        let stream = DOORS.take(i).await;
        let conn = TcpConn::new(stream, shared.exec).with_read_cap(READ_CAP);
        // Een verbinding die eindigt met een termijn of een reset is een
        // client die wegging; dat is geen logregel waard.
        let _ = leanhttp::serve(conn, async |ex: &mut Exchange<'_, TcpConn>| {
            handle(ex, shared).await
        })
        .await;
        DOORS.free(i);
    }
}

/// Eén verzoek.
async fn handle(ex: &mut Exchange<'_, TcpConn>, shared: &'static Shared) -> leanhttp::Result {
    match shared.mux.find(&mut ex.req)? {
        Found::Route(Route::Page) if ex.req.path == "/" => {
            ex.header_mut()
                .set("Content-Type", "text/html; charset=utf-8")?;
            ex.header_mut().set("Cache-Control", "no-store")?;
            ex.write(page::HTML.as_bytes()).await?;
            Ok(())
        }
        Found::Route(Route::Health) => text(ex, "ok\n").await,
        Found::Route(Route::Ping) => text(ex, "pong\n").await,
        Found::Route(Route::State) => state(ex, shared).await,
        Found::Route(Route::Run) => start(ex, shared).await,
        Found::Route(Route::Blob) => net::serve_blob(ex, shared).await,
        Found::Route(Route::Sink) if ex.req.method != "GET" && ex.req.method != "HEAD" => {
            net::serve_sink(ex, shared).await
        }
        Found::Route(Route::Sink) => {
            ex.header_mut().set("Allow", "PUT, POST")?;
            ex.error(
                405,
                "send bodies of up to 1 MiB here: apps/vitals/perf.sh, or PUT /sink",
            )
            .await
        }
        Found::Route(Route::Page) | Found::NotFound => ex.error(404, "not found").await,
        Found::MethodNotAllowed(allow) => {
            ex.header_mut().set("Allow", &allow)?;
            ex.error(405, "method not allowed").await
        }
    }
}

/// Een kort antwoord in platte tekst.
async fn text(ex: &mut Exchange<'_, TcpConn>, body: &str) -> leanhttp::Result {
    ex.header_mut()
        .set("Content-Type", "text/plain; charset=utf-8")?;
    ex.write(body.as_bytes()).await?;
    Ok(())
}

/// `GET /api/state`: alles als JSON.
async fn state(ex: &mut Exchange<'_, TcpConn>, shared: &'static Shared) -> leanhttp::Result {
    let Some(body) = run::state_json(shared) else {
        return ex.error(503, "state does not fit").await;
    };
    ex.header_mut().set("Content-Type", "application/json")?;
    ex.header_mut().set("Cache-Control", "no-store")?;
    ex.write(&body).await?;
    Ok(())
}

/// `GET /api/run?test=<naam>|all`: start een test, één tegelijk.
async fn start(ex: &mut Exchange<'_, TcpConn>, shared: &'static Shared) -> leanhttp::Result {
    let params = run::Params::parse(|k| ex.req.query(k).ok().flatten());
    let name = ex.req.query("test").ok().flatten().unwrap_or_default();
    match run::start(shared, &name, params) {
        Ok(started) => {
            ex.header_mut().set("Content-Type", "application/json")?;
            let mut out = bounded::Text::<64>::new();
            let _ = core::fmt::Write::write_fmt(
                &mut out,
                format_args!("{{\"started\":\"{started}\"}}\n"),
            );
            ex.write(out.as_str().as_bytes()).await?;
            Ok(())
        }
        Err(run::StartError::Unknown) => ex.error(404, "unknown test").await,
        Err(run::StartError::Busy(running)) => {
            let mut msg = bounded::Text::<64>::new();
            let _ = core::fmt::Write::write_fmt(
                &mut msg,
                format_args!("test {running:?} is still running"),
            );
            ex.error(409, msg.as_str()).await
        }
        Err(run::StartError::Spawn) => ex.error(503, "no room for the test task").await,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_mux_routes_every_path() {
        let mut mux = Mux::default();
        for (pattern, route) in ROUTES {
            mux.handle(pattern, route).unwrap();
        }
        for (path, want) in [
            ("/", Route::Page),
            ("/api/state", Route::State),
            ("/api/run", Route::Run),
            ("/ping", Route::Ping),
            ("/blob", Route::Blob),
            ("/health", Route::Health),
        ] {
            let mut req = leanhttp::Request::new("GET", path).unwrap();
            assert_eq!(mux.find(&mut req).unwrap(), Found::Route(&want), "{path}");
        }
        let mut req = leanhttp::Request::new("PUT", "/sink").unwrap();
        assert_eq!(mux.find(&mut req).unwrap(), Found::Route(&Route::Sink));
        let mut req = leanhttp::Request::new("DELETE", "/api/state").unwrap();
        assert!(matches!(
            mux.find(&mut req).unwrap(),
            Found::MethodNotAllowed(_)
        ));
    }

    #[test]
    fn the_pattern_does_not_repeat_within_a_block() {
        let mut b = vec![0u8; 4096];
        fill_pattern(&mut b);
        assert_ne!(b[..2048], b[2048..]);
        assert!(b.iter().any(|&x| x != 0));
    }
}
