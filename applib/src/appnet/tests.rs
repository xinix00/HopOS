//! Twee stacks aan één draad: de "kern" op 10.100.0.1 en een app op slot 1,
//! elk met een eigen pomp-taak over een [`Nic`] op gewone buffers, op één
//! executor met een klok die de test zelf verzet. Zo lopen handshake, data,
//! close en de system-client door precies de code die op het slot draait.

use super::*;
use crate::contract::{HOPABI_HDR_LEN, HOPABI_VERSION, KIND_RESULT, OP_STAT, SYS_HEADER_LEN};
use crate::ring::tests::Backing;
use crate::ring::{Peek, Reader, Writer};
use crate::sys::{check_frame_header, frame_header};
use core::cell::Cell;
use std::boxed::Box;

thread_local! {
    static NOW: Cell<u64> = const { Cell::new(1_000_000_000) };
}

/// De klok van de test: staat stil tot de lus hem naar de volgende timer zet.
fn now() -> u64 {
    NOW.with(Cell::get)
}

fn leak<T>(v: T) -> &'static T {
    Box::leak(Box::new(v))
}

const CAP: u64 = NET_RING_DATA_CAP;
const BUDGET: usize = 4 << 20;
/// Een poll-ronde van tien seconden: de tests lopen dan op de bel en op
/// de wekken van de stack, en een gemiste wek is een test die tien
/// (gesimuleerde) seconden duurt in plaats van een die toevallig slaagt.
const POLL: RxPoll = RxPoll {
    lo: Duration::from_secs(10),
    hi: Duration::from_secs(10),
    hold: 0,
};

/// De deurbel zoals de idle van de app-core hem belt: ligt er RX, dan de
/// bel van die pomp.
type Door = (Peek, &'static Signal);

struct Pair {
    exec: &'static Exec,
    kern: &'static Net,
    app: &'static Net,
    doors: [Door; 2],
}

fn nic(tx: &Backing, rx: &Backing, slot: u64) -> Nic {
    Nic::over(
        Writer::open(tx.pa(), CAP).unwrap(),
        Reader::open(rx.pa(), CAP).unwrap(),
        Peek::new(rx.pa(), CAP),
        mac_of(slot),
    )
}

fn spawn_pump(exec: &'static Exec, net: &'static Net, mut nic: Nic, rx: &Backing) -> Door {
    let bell: &'static Signal = leak(Signal::new());
    let mut buf = frame_buf(net.frame_len()).unwrap();
    exec.spawn(async move { net.pump(&mut nic, &mut buf, bell, POLL).await })
        .unwrap();
    (Peek::new(rx.pa(), CAP), bell)
}

/// De kern en slot 1 aan één draad, met hun pompen al gespawnd.
fn pair() -> Pair {
    let exec: &'static Exec = leak(Exec::new());
    exec.set_clock(now);
    let up = leak(Backing::new(CAP));
    let down = leak(Backing::new(CAP));
    let kern = leak(Net::new(slot_config(0, BUDGET), 7, exec, now).unwrap());
    let app = leak(Net::new(slot_config(1, BUDGET), 9, exec, now).unwrap());
    app.seed_neighbor(host_ip(), mac_of(0).0).unwrap();
    let k = spawn_pump(exec, kern, nic(down, up, 0), up);
    let a = spawn_pump(exec, app, nic(up, down, 1), down);
    Pair {
        exec,
        kern,
        app,
        doors: [k, a],
    }
}

impl Pair {
    /// Draait de executor tot `done`. Heeft geen taak iets te doen, dan
    /// eerst de deurbel (zoals de slaper vóór de WFE), en anders de klok
    /// naar de volgende timer.
    fn run_until(&self, done: impl Fn() -> bool) {
        for _ in 0..1_000_000 {
            if done() {
                return;
            }
            if self.exec.step() {
                continue;
            }
            let mut rang = false;
            for (peek, bell) in &self.doors {
                if peek.head_pending().1 {
                    bell.set();
                    rang = true;
                }
            }
            if !rang {
                let next = self
                    .exec
                    .next_deadline()
                    .expect("niets te doen en geen timer");
                NOW.with(|c| c.set(c.get().max(next)));
            }
        }
        panic!("liep niet af");
    }
}

/// Hoeveel gesimuleerde tijd sinds `t0`; ruim onder de poll-ronde bewijst
/// dat bel en stack de pompen wekten, niet de timer.
fn elapsed(t0: u64) -> u64 {
    now() - t0
}

fn slot<T>() -> &'static RefCell<Option<T>> {
    leak(RefCell::new(None))
}

/// Een kern van één call: leest een request-frame, antwoordt op een stat
/// met maat 4096, en leest dan tot EOF.
async fn fake_kern(l: TcpListener, seen: &'static RefCell<Option<(u8, Vec<u8>, usize)>>) {
    let mut c = l.accept().await.unwrap();
    let mut fh = [0u8; SYS_HEADER_LEN];
    read_exact(&mut c, &mut fh).await;
    let (_, n) = check_frame_header(&fh).unwrap();
    let mut req = vec![0u8; n];
    read_exact(&mut c, &mut req).await;
    let op = req[1];
    let seq = u32::from_le_bytes(req[4..8].try_into().unwrap());
    let mut resp = [0u8; HOPABI_HDR_LEN];
    resp[0] = HOPABI_VERSION;
    resp[1] = op;
    resp[4..8].copy_from_slice(&seq.to_le_bytes());
    resp[8..16].copy_from_slice(&4096u64.to_le_bytes());
    c.write_all(&frame_header(KIND_RESULT, HOPABI_HDR_LEN as u32))
        .await
        .unwrap();
    c.write_all(&resp).await.unwrap();
    // Na de call: de client sluit, en dat is EOF, geen reset.
    let mut rest = [0u8; 16];
    let eof = c.read(&mut rest).await.unwrap();
    *seen.borrow_mut() = Some((op, req[HOPABI_HDR_LEN..].to_vec(), eof));
}

async fn read_exact(c: &mut TcpStream, mut buf: &mut [u8]) {
    while !buf.is_empty() {
        let n = c.read(buf).await.unwrap();
        assert!(n > 0, "EOF midden in een frame");
        buf = &mut buf[n..];
    }
}

#[test]
fn system_stat_over_a_real_tcp_connection() {
    let p = pair();
    let l = p.kern.tcp_listen(sys::ADDRESS.1).unwrap();
    let seen = slot();
    let got = slot();
    p.exec.spawn(fake_kern(l, seen)).unwrap();
    let app = p.app;
    let t0 = now();
    p.exec
        .spawn(async move {
            let mut client = app.system_client();
            let r = client.stat("/data/db.bin").await;
            let connected = client.is_connected();
            drop(client); // FIN
            *got.borrow_mut() = Some((r, connected));
        })
        .unwrap();
    p.run_until(|| seen.borrow().is_some() && got.borrow().is_some());
    assert_eq!(got.borrow_mut().take(), Some((Ok(4096), true)));
    assert!(elapsed(t0) < 1_000_000, "stat duurde {} ns", elapsed(t0));
    let (op, path, eof) = seen.borrow_mut().take().unwrap();
    assert_eq!(
        (op, path.as_slice(), eof),
        (OP_STAT, &b"/data/db.bin"[..], 0)
    );
}

#[test]
fn an_explicit_connection_carries_the_first_call() {
    let p = pair();
    let l = p.kern.tcp_listen(sys::ADDRESS.1).unwrap();
    let seen = slot();
    let got = slot();
    p.exec.spawn(fake_kern(l, seen)).unwrap();
    let app = p.app;
    p.exec
        .spawn(async move {
            let c = app.tcp_connect(HOST, sys::ADDRESS.1).await.unwrap();
            let local = c.local().unwrap();
            let mut client = app.system_client_over(c);
            let r = client.stat("/").await;
            drop(client);
            *got.borrow_mut() = Some((r, local.ip));
        })
        .unwrap();
    p.run_until(|| seen.borrow().is_some() && got.borrow().is_some());
    assert_eq!(got.borrow_mut().take(), Some((Ok(4096), slot_ip(1))));
}

/// 40 × 8 KiB heen en weer door een echo, dan close: de client ziet de
/// echo byte voor byte terug en daarna EOF.
#[test]
fn bulk_both_ways_then_close() {
    const CHUNK: usize = 8 << 10;
    const ROUNDS: usize = 40;
    let p = pair();
    let l = p.kern.tcp_listen(7).unwrap();
    let echoed = leak(Cell::new(0usize));
    let server_done = leak(Cell::new(false));
    let result = slot();
    let t0 = now();
    p.exec
        .spawn(async move {
            let mut c = l.accept().await.unwrap();
            let mut buf = vec![0u8; 4096];
            loop {
                let n = c.read(&mut buf).await.unwrap();
                if n == 0 {
                    break;
                }
                c.write_all(&buf[..n]).await.unwrap();
                echoed.set(echoed.get() + n);
            }
            c.close().unwrap();
            server_done.set(true);
        })
        .unwrap();
    let app = p.app;
    p.exec
        .spawn(async move {
            let mut c = app.tcp_connect([10, 100, 0, 1], 7).await.unwrap();
            let mut back = vec![0u8; CHUNK];
            let mut ok = true;
            for r in 0..ROUNDS {
                let out: Vec<u8> = (0..CHUNK).map(|i| (i * 7 + r) as u8).collect();
                c.write_all(&out).await.unwrap();
                read_exact(&mut c, &mut back).await;
                ok &= back == out;
            }
            // Half dicht is hier heel dicht: close, en de echo sluit ook.
            let h = c.h;
            let net = c.net;
            c.close().unwrap();
            let after = net.with(|st| st.tcp_state(h)).unwrap();
            *result.borrow_mut() = Some((ok, after.is_err()));
        })
        .unwrap();
    p.run_until(|| result.borrow().is_some());
    assert_eq!(result.borrow_mut().take(), Some((true, true)));
    // De echo zag EOF en sloot.
    p.run_until(|| server_done.get());
    assert_eq!(echoed.get(), CHUNK * ROUNDS);
    assert!(
        elapsed(t0) < 1_000_000_000,
        "bulk duurde {} ns",
        elapsed(t0)
    );
    let st = p.app.stats().unwrap();
    assert_eq!(st.refused_no_budget, 0);
}

#[test]
fn a_closed_port_is_refused() {
    let p = pair();
    let got = slot();
    let app = p.app;
    p.exec
        .spawn(async move {
            let r = app.tcp_connect(HOST, 9).await.map(|_| ());
            *got.borrow_mut() = Some(r);
        })
        .unwrap();
    p.run_until(|| got.borrow().is_some());
    assert_eq!(
        got.borrow_mut().take(),
        Some(Err(NetError::Stack(StackError::Refused {
            ip: HOST,
            port: 9
        })))
    );
    assert_eq!(
        conn_error(NetError::Stack(StackError::Refused { ip: HOST, port: 9 })),
        ConnError::Refused
    );
}

#[test]
fn a_silent_peer_times_out_on_the_executor() {
    let p = pair();
    let got = slot();
    let app = p.app;
    let t0 = now();
    p.exec
        .spawn(async move {
            let r = app
                .tcp_connect_timeout([10, 100, 0, 77], 80, Duration::from_millis(200))
                .await
                .map(|_| ());
            *got.borrow_mut() = Some((r, now()));
        })
        .unwrap();
    p.run_until(|| got.borrow().is_some());
    let (r, t) = got.borrow_mut().take().unwrap();
    assert!(
        matches!(
            r,
            Err(NetError::Timeout | NetError::Stack(StackError::DeadlineExceeded))
        ),
        "{r:?}"
    );
    assert!(t - t0 >= 200_000_000 && t - t0 < 300_000_000, "{}", t - t0);
}

#[test]
fn a_read_deadline_fires_and_the_stream_stays_usable() {
    let p = pair();
    let l = p.kern.tcp_listen(7).unwrap();
    let got = slot();
    p.exec
        .spawn(async move {
            let mut c = l.accept().await.unwrap();
            c.set_timeout(Some(Duration::from_millis(50)));
            let mut b = [0u8; 4];
            let first = c.read(&mut b).await.map(|_| ());
            c.set_timeout(None);
            let second = c.read(&mut b).await;
            *got.borrow_mut() = Some((first, second, b));
        })
        .unwrap();
    let app = p.app;
    p.exec
        .spawn(async move {
            let mut c = app.tcp_connect(HOST, 7).await.unwrap();
            app.exec.after(Duration::from_millis(100)).await;
            c.write_all(b"late").await.unwrap();
            app.exec.after(Duration::from_secs(1)).await;
        })
        .unwrap();
    p.run_until(|| got.borrow().is_some());
    assert_eq!(
        got.borrow_mut().take(),
        Some((Err(NetError::Timeout), Ok(4), *b"late"))
    );
}

#[test]
fn udp_round_trip() {
    let p = pair();
    let kern = p.kern;
    let app = p.app;
    let got = slot();
    let server = kern.udp_bind(53).unwrap();
    p.exec
        .spawn(async move {
            let mut buf = [0u8; 64];
            let (n, from) = server.recv_from(&mut buf).await.unwrap();
            buf[..n].reverse();
            server.send_to(from, &buf[..n]).await.unwrap();
        })
        .unwrap();
    p.exec
        .spawn(async move {
            let s = app.udp_bind(0).unwrap();
            let to = Endpoint { ip: HOST, port: 53 };
            s.send_to(to, b"ping").await.unwrap();
            let mut buf = [0u8; 64];
            let (n, from) = s.recv_from(&mut buf).await.unwrap();
            *got.borrow_mut() = Some((buf[..n].to_vec(), from));
        })
        .unwrap();
    p.run_until(|| got.borrow().is_some());
    assert_eq!(
        got.borrow_mut().take(),
        Some((b"gnip".to_vec(), Endpoint { ip: HOST, port: 53 }))
    );
}

#[test]
fn budget_env_and_address_parsing() {
    assert_eq!(budget_for(16 << 20, None), 2 << 20);
    assert_eq!(budget_for(1 << 20, None), BUDGET_MIN);
    assert_eq!(budget_for(1 << 30, None), BUDGET_MAX);
    assert_eq!(budget_for(1 << 30, Some("512k")), 512 << 10);
    assert_eq!(budget_for(1 << 30, Some("3M")), 3 << 20);
    assert_eq!(budget_for(16 << 20, Some("123456")), 123_456);
    assert_eq!(budget_for(16 << 20, Some("junk")), 2 << 20);
    assert_eq!(budget_for(16 << 20, Some("0")), 2 << 20);
    assert_eq!(parse_ip4("10.100.0.1"), Some(HOST));
    assert_eq!(parse_ip4("1.1.1.1"), Some([1, 1, 1, 1]));
    assert_eq!(parse_ip4("1.1.1"), None);
    assert_eq!(parse_ip4("1.1.1.1.1"), None);
    assert_eq!(parse_ip4("1.1.1.256"), None);
    let c = slot_config(3, 2 << 20);
    assert_eq!(
        (c.ip, c.mac, c.gw, c.prefix),
        (slot_ip(3), mac_of(3).0, HOST, 24)
    );
    assert_eq!(c.mtu, NET_MTU);
    assert_eq!(sys::ADDRESS, (HOST, abi::systemapi::PORT));
}

#[test]
fn transport_errors_map_to_what_the_client_retries() {
    assert_eq!(
        conn_error(NetError::Stack(StackError::Reset)),
        ConnError::Reset
    );
    assert_eq!(
        conn_error(NetError::Stack(StackError::Closed)),
        ConnError::Closed
    );
    assert_eq!(conn_error(NetError::Timeout), ConnError::Refused);
    assert_eq!(conn_error(NetError::NotUp), ConnError::Refused);
}

/// De log-verbinding: vóór de dial gaat een regel naar de outbox, daarna
/// als `KindLog`-frame over TCP naar de kern. De enige test die de
/// log-static aanraakt.
#[test]
fn log_lines_go_over_the_system_connection_once_it_is_up() {
    use crate::contract::KIND_LOG;
    let p = pair();
    let l = p.kern.tcp_listen(sys::ADDRESS.1).unwrap();
    let seen = slot();
    p.exec
        .spawn(async move {
            let mut c = l.accept().await.unwrap();
            let mut got = Vec::new();
            for _ in 0..2 {
                let mut fh = [0u8; SYS_HEADER_LEN];
                read_exact(&mut c, &mut fh).await;
                let (kind, n) = check_frame_header(&fh).unwrap();
                let mut line = vec![0u8; n];
                read_exact(&mut c, &mut line).await;
                got.push((kind, line));
            }
            *seen.borrow_mut() = Some(got);
        })
        .unwrap();
    assert!(!try_log(b"too early"), "uit is outbox");
    log_via_system(p.app).unwrap();
    assert_eq!(log_via_system(p.app), Err(NetError::AlreadyUp));
    assert!(!try_log(b"still dialing"));
    p.run_until(|| {
        log_cell()
            .try_borrow()
            .is_ok_and(|l| l.as_ref().is_some_and(|l| l.conn.is_some()))
    });
    let written = crate::log::WRITTEN.load(Relaxed);
    crate::log::emit_via_net(None, format_args!("slot {} up", 1));
    assert!(crate::log::WRITTEN.load(Relaxed) > written);
    assert!(try_log(b"second"));
    p.run_until(|| seen.borrow().is_some());
    assert_eq!(
        seen.borrow_mut().take().unwrap(),
        [
            (KIND_LOG, b"slot 1 up".to_vec()),
            (KIND_LOG, b"second".to_vec())
        ]
    );
}
