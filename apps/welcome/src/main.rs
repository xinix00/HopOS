//! welcome: de pagina die een node een gezicht geeft, de eerste Rust-app
//! van HopOS v3.
//!
//! Eén HTTP-server (leanhttp over `appnet::TcpListener`) op de poort uit
//! `ER_PORT_HTTP` (standaard 80). `GET /` geeft per verzoek één pagina
//! ([`page`]): de bunny in tekst, de naam van de node en het slot, de
//! uptime, het aantal verzoeken en de heap; `GET /health` geeft 200 met
//! `ok`. De kern zet de poort van de jobspec op de uplink door naar dit slot
//! (DNAT), dus een browser op het LAN ziet de pagina op het adres van de
//! node.
//!
//! De vorm (handboek §2): één taak accepteert en geeft elke verbinding als
//! waarde aan een vrije werker uit een vaste pool van [`WORKERS`]; een
//! werker bezit zijn verbinding tot hij sluit. Geen taak per verbinding.
//!
//! Markers: `HOPOS_WELCOME_UP port=<n>` als de listener staat, en om de
//! [`LOG_EVERY`] verzoeken één regel `HOPOS_WELCOME_REQUESTS`. Per verzoek
//! niets: logs over de outbox zijn goedkoop, maar een pagina die elke
//! seconde herladen wordt, hoort de console niet te vullen.

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

mod page;

use alloc::vec::Vec;
use applib::app::port_of;
use applib::appnet::{self, TcpListener, TcpStream};
use applib::rt::Exec;
use applib::tcp::TcpConn;
use applib::{App, EXEC, clock, heap::HEAP, log};
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use leanhttp::{Exchange, Found, Mux};
use sync::Doors;

applib::main!(welcome);

/// Op de host bestaat dit image niet: daar is dit een lege binary, zodat de
/// host-poort de pagina kan toetsen en clippy de rest kan lezen.
#[cfg(not(target_os = "none"))]
fn main() {}

/// De poort zonder `ER_PORT_HTTP` (een image buiten Hop om).
const DEFAULT_PORT: u16 = 80;

/// De werkers: zoveel verbindingen tegelijk. Een browser opent er twee tot
/// zes naar één host; vier dekt één lezer ruim, en de rest wacht kort op de
/// eerste die vrijkomt.
const WORKERS: usize = 4;

/// De langste stilte op een keep-alive-verbinding (zie
/// [`TcpConn::with_read_cap`]): met een vaste pool van [`WORKERS`] houdt een
/// stille browser anders een werker een minuut vast.
const READ_CAP: Duration = Duration::from_secs(5);

/// Om de zoveel verzoeken één logregel.
const LOG_EVERY: u64 = 100;

/// De versie van dit image (die van de werkruimte).
const VERSION: &str = env!("CARGO_PKG_VERSION");

/// Verzoeken sinds de start, alle paden. Een teller, dus een atomic
/// (handboek §1.3).
static REQUESTS: AtomicU64 = AtomicU64::new(0);

/// De deuren van de werkers: de acceptor zet een verbinding achter de
/// eerste vrije, de werker meldt zich vrij als hij dicht is. Zijn ze alle
/// bezet, dan wacht de vijfde in de acceptor op de eerste die vrijkomt.
static DOORS: Doors<TcpStream, WORKERS> = Doors::new();

/// De paden van deze server.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
enum Route {
    /// `GET /health`: 200, `ok`.
    Health,
    /// `GET /`: de pagina. Een subtree in de Mux; een ander pad is 404.
    Page,
}

/// Wat elke werker deelt: de feiten van de start en de routetabel. Gezet
/// in [`welcome`] vóór de eerste spawn en daarna alleen gelezen.
struct Shared {
    exec: &'static Exec,
    node: &'static str,
    slot: u64,
    ip: [u8; 4],
    port: u16,
    ram: u64,
    started_ns: u64,
    mux: Mux<Route>,
}

#[expect(
    clippy::expect_used,
    reason = "de start van de bin: zonder netstack of poort is er niets te serveren, en een luide paniek met reden is het goede einde"
)]
async fn welcome(app: &'static App) {
    let exec: &'static Exec = EXEC.get();
    let port = port_of(app.env("ER_PORT_HTTP"), DEFAULT_PORT);
    let net = appnet::up(app).expect("welcome: network stack");
    let listener = TcpListener::bind(port).expect("welcome: listen on ER_PORT_HTTP");
    let mut mux = Mux::default();
    mux.handle("GET /health", Route::Health)
        .expect("welcome: route /health");
    mux.handle("GET /", Route::Page).expect("welcome: route /");
    let node = app
        .env("ER_ATTR_NODE_ID")
        .or_else(|| app.env("HOPOS_NODE"))
        .unwrap_or("this node");
    // Eén keer bij de start, en voor de levensduur van de app: de werkers
    // lenen het, niemand schrijft het nog.
    let shared: &'static Shared = alloc::boxed::Box::leak(alloc::boxed::Box::new(Shared {
        exec,
        node,
        slot: app.slot(),
        ip: net.ip(),
        port,
        ram: app.ram_size(),
        started_ns: clock::now_ns(),
        mux,
    }));
    for i in 0..WORKERS {
        exec.spawn(worker(i, shared))
            .expect("welcome: spawn worker");
    }
    let [a, b, c, d] = shared.ip;
    log!(
        "welcome: serving http on {a}.{b}.{c}.{d}:{port} for node {node}, slot {}, {WORKERS} workers HOPOS_WELCOME_UP port={port}",
        shared.slot
    );
    accept(listener, exec).await;
}

/// De acceptor: elke verbinding naar de eerste vrije werker.
async fn accept(listener: TcpListener, exec: &'static Exec) {
    loop {
        match listener.accept().await {
            Ok(stream) => DOORS.place(stream).await,
            Err(e) => {
                log!("welcome: accept: {e} HOPOS_WELCOME_ACCEPT");
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
        // browser die wegging; dat is geen logregel waard.
        let _ = leanhttp::serve(conn, async |ex: &mut Exchange<'_, TcpConn>| {
            handle(ex, shared).await
        })
        .await;
        DOORS.free(i);
    }
}

/// Eén verzoek.
async fn handle(ex: &mut Exchange<'_, TcpConn>, shared: &Shared) -> leanhttp::Result {
    let n = REQUESTS.fetch_add(1, Relaxed).wrapping_add(1);
    if n.is_multiple_of(LOG_EVERY) {
        let st = HEAP.stats();
        log!(
            "welcome: requests={n} heap_used={} heap_peak={} HOPOS_WELCOME_REQUESTS",
            st.used,
            st.peak
        );
    }
    match shared.mux.find(&mut ex.req)? {
        Found::Route(Route::Health) => {
            ex.header_mut()
                .set("Content-Type", "text/plain; charset=utf-8")?;
            ex.write(b"ok\n").await?;
            Ok(())
        }
        Found::Route(Route::Page) if ex.req.path == "/" => serve_page(ex, shared, n).await,
        Found::Route(Route::Page) | Found::NotFound => ex.error(404, "not found").await,
        Found::MethodNotAllowed(allow) => {
            ex.header_mut().set("Allow", &allow)?;
            ex.error(405, "method not allowed").await
        }
    }
}

/// De pagina, met de getallen van dit moment.
async fn serve_page(
    ex: &mut Exchange<'_, TcpConn>,
    shared: &Shared,
    requests: u64,
) -> leanhttp::Result {
    let mut out = Vec::new();
    if out.try_reserve_exact(page::PAGE_CAP).is_err() {
        return ex.error(503, "out of memory").await;
    }
    let st = HEAP.stats();
    let facts = page::Facts {
        node: shared.node,
        slot: shared.slot,
        ip: shared.ip,
        port: shared.port,
        ram: shared.ram,
        version: VERSION,
    };
    let live = page::Live {
        host: ex.req.header.get("Host").unwrap_or(""),
        uptime_ns: clock::now_ns().wrapping_sub(shared.started_ns),
        requests,
        heap_used: st.used,
        heap_peak: st.peak,
        heap_capacity: st.capacity,
        allocs: st.allocs,
    };
    if page::render(&facts, &live, &mut out).is_err() {
        return ex.error(500, "page does not fit").await;
    }
    ex.header_mut()
        .set("Content-Type", "text/html; charset=utf-8")?;
    ex.header_mut().set("Cache-Control", "no-store")?;
    ex.write(&out).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_mux_routes_health_and_the_page() {
        let mut mux = Mux::default();
        mux.handle("GET /health", Route::Health).unwrap();
        mux.handle("GET /", Route::Page).unwrap();
        let mut req = leanhttp::Request::new("GET", "/health").unwrap();
        assert_eq!(mux.find(&mut req).unwrap(), Found::Route(&Route::Health));
        let mut req = leanhttp::Request::new("GET", "/").unwrap();
        assert_eq!(mux.find(&mut req).unwrap(), Found::Route(&Route::Page));
        let mut req = leanhttp::Request::new("HEAD", "/").unwrap();
        assert_eq!(mux.find(&mut req).unwrap(), Found::Route(&Route::Page));
        let mut req = leanhttp::Request::new("POST", "/").unwrap();
        assert!(matches!(
            mux.find(&mut req).unwrap(),
            Found::MethodNotAllowed(_)
        ));
    }
}
