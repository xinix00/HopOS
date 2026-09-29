//! De ABI-toets van HopOS v3: een app die elk contract van de kern aanraakt
//! en per toets een marker op zijn log zet.
//!
//! Wat de Go-appspike over veel rollen verspreidde (READY, heartbeat, logs,
//! net, bestanden, isolatie), doet deze in één doorloop over wat applib nu
//! draagt: de control-page lezen en terugschrijven, de env, een reeks
//! logregels, een frame op de TX-ring, de klok, de heap, de heartbeat, een
//! system-call over de eigen netstack en de bestandscalls in de eigen root
//! (hopfs op de schijf van de kern). Elke
//! toets is één regel `HOPOS_APPSPIKE_<TOETS> ok|FAIL ...` met de getallen
//! erbij; de laatste regel is `HOPOS_APPSPIKE_DONE pass=N fail=M`, en de
//! exitcode is het aantal mislukte toetsen. De soak-scripts greppen erop.
//!
//! Met `HOLD=1` in de env blijft de app daarna leven (de heartbeat loopt
//! door), voor wie de kill-vlag wil toetsen.
//!
//! Canoniek gelinkt (applib/link.ld): de stage-2-map van de kern legt het
//! image op de partitie van elk slot, en de kern patcht RamStart en RamSize
//! bij plaatsing.

#![cfg_attr(target_os = "none", no_std, no_main)]

use applib::appnet::{self, TcpStream};
use applib::net::{self, Nic};
use applib::{App, AppStatus, EXEC, clock, heap::HEAP, log, sys};
use core::time::Duration;
use netdev::Device;

applib::main!(spike);

/// Op de host bestaat dit image niet: daar is dit alleen een lege binary,
/// zodat de host-poort (clippy `--all-targets`) hem kan typechecken zonder
/// allocator en paniekhaak van het slot.
#[cfg(not(target_os = "none"))]
fn main() {}

/// Het resultaat van de doorloop.
#[derive(Default)]
struct Score {
    pass: u32,
    fail: u32,
}

impl Score {
    /// Telt één toets en zet zijn marker op het log.
    fn check(&mut self, name: &str, ok: bool, detail: core::fmt::Arguments<'_>) {
        if ok {
            self.pass += 1;
            log!("HOPOS_APPSPIKE_{name} ok {detail}");
        } else {
            self.fail += 1;
            log!("HOPOS_APPSPIKE_{name} FAIL {detail}");
        }
    }
}

async fn spike(app: &'static App) {
    log!(
        "HOPOS_APPSPIKE_UP slot={} ram={:#x}+{:#x} tail={:#x}",
        app.slot(),
        app.ram_start(),
        app.ram_size(),
        app.tail().base().0
    );
    let mut s = Score::default();
    ctrl_page(app, &mut s);
    env(app, &mut s);
    logs(&mut s).await;
    frame(app, &mut s).await;
    timer(&mut s).await;
    heartbeat(app, &mut s).await;
    let client = network(app, &mut s).await;
    files(client, &mut s).await;
    heap(app, &mut s);

    log!("HOPOS_APPSPIKE_DONE pass={} fail={}", s.pass, s.fail);
    if app.env("HOLD") == Some("1") {
        core::future::pending::<()>().await;
    }
    // Netjes: eerst het net-afscheid (elke FIN bevestigd), dan de exit.
    app.shutdown(u64::from(s.fail)).await;
}

/// De control-page: READY staat er, de RAM-maat is wat de app van zichzelf
/// weet, en een woord dat de app schrijft leest hij terug.
fn ctrl_page(app: &App, s: &mut Score) {
    let c = app.ctrl();
    let status = c.status();
    let ram = c.ram_size();
    c.set_mem_sys(0x5a5a_0001);
    let back = c.mem_sys();
    let ok = status == Some(AppStatus::Ready) && ram == app.ram_size() && back == 0x5a5a_0001;
    s.check(
        "CTRL",
        ok,
        format_args!(
            "status={status:?} ram={ram:#x} readback={back:#x} cores={} shared={} yield={} wall={}",
            c.cores(),
            c.is_shared(),
            c.is_yield_mode(),
            c.wall_offset()
        ),
    );
}

/// De env: gelezen van de pagina. Een lege env is geen fout (de kern geeft
/// er niet altijd een mee), een onleesbare wel.
fn env(app: &App, s: &mut Score) {
    let role = app.env("ROLE").unwrap_or("-");
    let port = app.env("ER_PORT_HTTP").unwrap_or("-");
    s.check("ENV", true, format_args!("ROLE={role} ER_PORT_HTTP={port}"));
}

/// Aantal logregels van de logtoets.
const LOG_LINES: u32 = 64;

/// N logregels op de outbox. Elke 16 regels een milliseconde, zodat de
/// servicer van de kern de ring kan leegtrekken; gedropt is gedropt, en dat
/// telt als mislukt.
async fn logs(s: &mut Score) {
    let dropped = applib::log::DROPPED.load(core::sync::atomic::Ordering::Relaxed);
    for i in 1..=LOG_LINES {
        log!("HOPOS_APPSPIKE_LINE {i}/{LOG_LINES}");
        if i % 16 == 0 {
            EXEC.after(Duration::from_millis(1)).await;
        }
    }
    let lost = applib::log::DROPPED
        .load(core::sync::atomic::Ordering::Relaxed)
        .wrapping_sub(dropped);
    s.check(
        "LOG",
        lost == 0,
        format_args!("lines={LOG_LINES} dropped={lost}"),
    );
}

/// De Ethertype van het toetsframe: 0x88B5, "local experimental" (IEEE
/// 802), dus nooit iets dat een echte stack verkeerd leest.
const ETHERTYPE_LOCAL: [u8; 2] = [0x88, 0xb5];

/// Eén frame op de TX-ring, van dit slot naar de kern.
async fn frame(app: &App, s: &mut Score) {
    let mut nic = match Nic::open(app) {
        Ok(n) => n,
        Err(e) => {
            s.check("FRAME", false, format_args!("open: {e}"));
            return;
        }
    };
    let mut f = [0u8; 64];
    f[0..6].copy_from_slice(&net::mac_of(0).0);
    f[6..12].copy_from_slice(&nic.mac().0);
    f[12..14].copy_from_slice(&ETHERTYPE_LOCAL);
    f[14..28].copy_from_slice(b"HOPOS_APPSPIKE");
    let r = nic.transmit_wait(&f, clock::now_ns).await;
    s.check(
        "FRAME",
        r.is_ok(),
        format_args!(
            "len={} src={} dst={} result={r:?}",
            f.len(),
            nic.mac(),
            net::mac_of(0)
        ),
    );
}

/// De klok en het timerwiel: een slaap van 10 ms duurt minstens 10 ms.
async fn timer(s: &mut Score) {
    let t0 = clock::now_ns();
    EXEC.after(Duration::from_millis(10)).await;
    let dt = clock::now_ns().wrapping_sub(t0);
    s.check(
        "TIMER",
        (10_000_000..1_000_000_000).contains(&dt),
        format_args!("slept_ns={dt}"),
    );
}

/// De heartbeat loopt als taak: na 300 ms moet de teller op de pagina
/// minstens vijf slagen verder staan.
async fn heartbeat(app: &App, s: &mut Score) {
    let before = app.ctrl().heartbeat();
    EXEC.after(Duration::from_millis(300)).await;
    let after = app.ctrl().heartbeat();
    let beats = after.wrapping_sub(before);
    s.check(
        "BEAT",
        beats >= 5,
        format_args!(
            "before={before} after={after} idle_ticks={}",
            app.ctrl().idle_ticks()
        ),
    );
}

/// Hoe lang de NET-toets op de handshake met de kern wacht.
const NET_DIAL: Duration = Duration::from_secs(3);

/// De netstack: `appnet::up`, een TCP-verbinding naar de system-listener van
/// de kern (10.100.0.1:10100) en één `stat` over die verbinding via
/// `sys::Client`. Zonder listener aan de kern-kant faalt hij met de reden
/// (refused, of een timeout als er niemand antwoordt).
async fn network(app: &'static App, s: &mut Score) -> Option<appnet::SystemClient> {
    let n = match appnet::up(app) {
        Ok(n) => n,
        Err(e) => {
            s.check("NET", false, format_args!("up: {e}"));
            return None;
        }
    };
    let [a, b, c, d] = n.ip();
    let (ip, port) = sys::ADDRESS;
    let t0 = clock::now_ns();
    let conn = match TcpStream::connect_timeout(ip, port, NET_DIAL).await {
        Ok(conn) => conn,
        Err(e) => {
            s.check(
                "NET",
                false,
                format_args!("ip={a}.{b}.{c}.{d} connect {ip:?}:{port}: {e}"),
            );
            return None;
        }
    };
    let dial_us = clock::now_ns().wrapping_sub(t0) / 1000;
    let mut client = n.system_client_over(conn);
    // Eén logregel over de system-verbinding (KindLog): listener, admit,
    // servicer en LogTee. De kern zet de regel als `slot N: ...` op zijn
    // console, en dát is het bewijs van buitenaf: app, switch, kern-stack en
    // system-API in één lijn. De bestandscalls volgen over dezelfde
    // verbinding (`files`).
    let t1 = clock::now_ns();
    let r = client
        .log(b"HOPOS_APPSPIKE_NETLOG via the system connection")
        .await;
    let log_us = clock::now_ns().wrapping_sub(t1) / 1000;
    // `log` keert terug zodra de regel in de TCP-zendbuffer staat, niet als
    // hij aankwam (gemeten 29-09: log_us=320, en zonder wachten zag de kern
    // niets). `flush` wacht tot de kern hem bevestigde; zo meet de toets
    // wat hij belooft, en niet een pauze die toevallig lang genoeg is.
    let t2 = clock::now_ns();
    let flushed = match client.conn_mut() {
        Some(conn) => Some(conn.flush().await),
        None => None,
    };
    let flush_us = clock::now_ns().wrapping_sub(t2) / 1000;
    let kicks = net::TX_KICKS.load(core::sync::atomic::Ordering::Relaxed);
    let ip = format_args!("ip={a}.{b}.{c}.{d} dial_us={dial_us} tx_kicks={kicks}");
    match (r, flushed) {
        (Ok(()), Some(Ok(()))) => s.check(
            "NET",
            true,
            format_args!("{ip} log_us={log_us} flush_us={flush_us}"),
        ),
        (Err(e), _) => s.check("NET", false, format_args!("{ip} log: {e}")),
        (Ok(()), Some(Err(e))) => s.check(
            "NET",
            false,
            format_args!("{ip} flush after {flush_us}us: {e}"),
        ),
        (Ok(()), None) => s.check("NET", false, format_args!("{ip} no connection to flush")),
    }
    Some(client)
}

/// Het bestand van de FS-toets, in de eigen root (`/.tasks/slot<N>/`).
const FS_FILE: &str = "hallo.txt";

/// De inhoud: langer dan één hopfs-blok niet nodig, wel met een staart die
/// een truncate zichtbaar zou maken.
const FS_DATA: &[u8] = b"hallo van appspike, via de system-API naar hopfs op de schijf\n";

/// De bestandscalls over dezelfde verbinding: schrijven (truncate plus
/// write), `stat`, teruglezen, de lijst van de eigen root en weer weg. De
/// root is bij elke start leeg (de kern veegt hem), dus de lijst is precies
/// dit ene bestand.
async fn files(client: Option<appnet::SystemClient>, s: &mut Score) {
    let Some(mut c) = client else {
        s.check("FS", false, format_args!("no system connection"));
        return;
    };
    let t0 = clock::now_ns();
    let r = fs_round(&mut c).await;
    let us = clock::now_ns().wrapping_sub(t0) / 1000;
    match r {
        Ok((size, names)) => s.check(
            "FS",
            true,
            format_args!("file={FS_FILE} size={size} list={names} us={us}"),
        ),
        Err(why) => s.check("FS", false, format_args!("{why} after {us} us")),
    }
}

/// Wat er in de FS-toets misging, met de call of het getal erbij.
enum Why {
    /// Een call faalde.
    Sys(&'static str, sys::Error),
    /// Een call gaf iets anders dan verwacht.
    Num(&'static str, u64),
}

impl Why {
    fn sys(what: &'static str, e: sys::Error) -> Why {
        Why::Sys(what, e)
    }
    fn num(what: &'static str, n: u64) -> Why {
        Why::Num(what, n)
    }
}

impl core::fmt::Display for Why {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Why::Sys(what, e) => write!(f, "{what}: {e}"),
            Why::Num(what, n) => write!(f, "{what} {n}"),
        }
    }
}

/// Eén ronde; geeft de maat en het aantal namen, of wat er misging.
async fn fs_round(c: &mut appnet::SystemClient) -> Result<(u64, usize), Why> {
    c.write_file(FS_FILE, FS_DATA)
        .await
        .map_err(|e| Why::sys("write_file", e))?;
    let size = c.stat(FS_FILE).await.map_err(|e| Why::sys("stat", e))?;
    if size != FS_DATA.len() as u64 {
        return Err(Why::num("stat size", size));
    }
    let mut buf = [0u8; 128];
    let n = c
        .read_into(FS_FILE, 0, &mut buf)
        .await
        .map_err(|e| Why::sys("read_into", e))?;
    if buf.get(..n) != Some(FS_DATA) {
        return Err(Why::num("read back bytes", n as u64));
    }
    let mut list = [0u8; 256];
    let n = c
        .list("/", &mut list)
        .await
        .map_err(|e| Why::sys("list", e))?;
    let names = sys::names(list.get(..n).unwrap_or(&[])).count();
    if !sys::names(list.get(..n).unwrap_or(&[])).any(|x| x == FS_FILE) {
        return Err(Why::num("list without the file, names", names as u64));
    }
    c.remove(FS_FILE).await.map_err(|e| Why::sys("remove", e))?;
    match c.stat(FS_FILE).await {
        Err(sys::Error::NotFound { .. }) => Ok((size, names)),
        Ok(n) => Err(Why::num("still there after remove, size", n)),
        Err(e) => Err(Why::sys("stat after remove", e)),
    }
}

/// De heap: de executor alloceerde de taken, dus er is iets in gebruik, het
/// plafond ligt onder de stack, en de wandeling vindt geen gebroken
/// invariant. De piek en de weigeringen gaan mee als meetlat.
fn heap(app: &App, s: &mut Score) {
    let st = HEAP.stats();
    let walk = HEAP.check();
    let (used, cap) = (st.used, st.capacity);
    match walk {
        Ok(w) => s.check(
            "HEAP",
            used > 0 && used <= cap && cap < app.ram_size(),
            format_args!(
                "used={used} peak={} capacity={cap} allocs={} frees={} failed={} blocks={} free_blocks={}",
                st.peak, st.allocs, st.frees, st.failed, w.blocks, w.free_blocks
            ),
        ),
        Err(e) => s.check("HEAP", false, format_args!("used={used} {e}")),
    }
}
