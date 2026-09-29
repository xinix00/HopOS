//! De server-kant van `tools/netmeter`: TCP met het protocol uit
//! [`crate::proto`] en een UDP-echo op dezelfde poort.
//!
//! De vorm is die van welcome (handboek §2): één taak accepteert en geeft
//! elke verbinding als waarde aan een vrije werker uit een vaste pool van
//! [`WORKERS`]; een werker bezit zijn verbinding en zijn buffer tot hij
//! sluit. Geen taak per verbinding. Per verbinding geen logregel (dat zou
//! een ring-write per meting zijn, en dus de meting); om de
//! [`LOG_EVERY`] verbindingen één regel met de tellers.

use crate::proto::{self, CMD_MAX, Cmd, PATTERN_LEN};
use alloc::vec::Vec;
use applib::appnet::{self, Endpoint, NetError, TcpListener, TcpStream, UdpSocket};
use applib::rt::Exec;
use applib::{App, EXEC, clock, log};
use core::cell::Cell;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use sync::Local;
use sync::spsc::{Channel, Receiver, Sender};

/// De werkers: zoveel verbindingen tegelijk. netmeter meet met hooguit acht
/// parallelle verbindingen (`--conns`); een negende wacht op de eerste die
/// vrijkomt.
pub(crate) const WORKERS: usize = 8;

/// De buffer van één werker: groot genoeg voor een volle TCP-ronde van de
/// stack, klein genoeg dat acht werkers in een app van 32 MiB passen.
const BUF_LEN: usize = 16 << 10;

/// De langste stilte op een verbinding voor de werker hem opgeeft.
const IDLE_CAP: Duration = Duration::from_secs(30);

/// Hoe vaak de acceptor kijkt of er een werker vrij is (het koude pad, zoals
/// in welcome).
const BUSY_POLL: Duration = Duration::from_millis(2);

/// Om de zoveel verbindingen één logregel.
const LOG_EVERY: u64 = 256;

/// De tellers van de server: verbindingen, bytes in en uit, datagrammen.
/// Atomics (handboek §1.3); gelezen door de logregel.
static CONNS: AtomicU64 = AtomicU64::new(0);
static BYTES_IN: AtomicU64 = AtomicU64::new(0);
static BYTES_OUT: AtomicU64 = AtomicU64::new(0);
static DATAGRAMS: AtomicU64 = AtomicU64::new(0);
/// Verbindingen die op een netfout of een termijn eindigden.
static ABORTS: AtomicU64 = AtomicU64::new(0);

/// Zoveel afgebroken verbindingen krijgen een eigen regel; daarna alleen de
/// teller (handboek §6: luid, maar één keer).
const ABORT_LOG: u64 = 3;

/// De rij naar elke werker: één verbinding tegelijk.
static QUEUES: Local<[Channel<TcpStream, 1>; WORKERS]> =
    Local::new([const { Channel::new() }; WORKERS]);

/// Welke werker een verbinding heeft (zoals in welcome: de acceptor zet,
/// de werker wist, nooit over een `.await` geleend).
static BUSY: Local<[Cell<bool>; WORKERS]> = Local::new([const { Cell::new(false) }; WORKERS]);

/// Waarom een verbinding eindigde zonder dat de client klaar was.
#[derive(Debug)]
enum Fail {
    /// De stack weigerde of de deadline verstreek.
    Net(NetError),
    /// Geen geldige commandoregel.
    BadCommand,
}

impl From<NetError> for Fail {
    fn from(e: NetError) -> Self {
        Self::Net(e)
    }
}

/// De server: de netstack op, de listener, de UDP-echo en de werkers, en
/// dan accepteren tot de app stopt.
#[expect(
    clippy::expect_used,
    reason = "de start van de rol: zonder netstack, poort of werkers is er niets te meten, en een luide paniek met reden is het goede einde"
)]
pub(crate) async fn run(app: &'static App, port: u16) {
    let exec: &'static Exec = EXEC.get();
    let net = appnet::up(app).expect("bench: network stack");
    let listener = TcpListener::bind(port).expect("bench: tcp listen");
    let udp = UdpSocket::bind(port).expect("bench: udp bind");
    let mut pattern = Vec::new();
    pattern
        .try_reserve_exact(PATTERN_LEN)
        .expect("bench: pattern block");
    pattern.resize(PATTERN_LEN, 0);
    proto::fill_pattern(&mut pattern);
    let pattern: &'static [u8] = alloc::boxed::Box::leak(pattern.into_boxed_slice());
    let mut senders: Vec<Sender<'static, TcpStream, 1>> = Vec::new();
    senders
        .try_reserve_exact(WORKERS)
        .expect("bench: worker table");
    for (i, q) in QUEUES.get().iter().enumerate() {
        let (tx, rx) = q.split().expect("bench: queue split once");
        senders.push(tx);
        let mut buf = Vec::new();
        buf.try_reserve_exact(BUF_LEN)
            .expect("bench: worker buffer");
        buf.resize(BUF_LEN, 0);
        exec.spawn(worker(i, rx, buf, pattern))
            .expect("bench: spawn worker");
    }
    exec.spawn(udp_echo(udp)).expect("bench: spawn udp echo");
    let [a, b, c, d] = net.ip();
    log!(
        "bench: serving echo, sink and source on tcp {a}.{b}.{c}.{d}:{port}, udp echo on :{port}, {WORKERS} workers HOPOS_BENCH_UP role=serve port={port}"
    );
    accept(listener, &mut senders, exec).await;
}

/// De acceptor: elke verbinding naar de eerste vrije werker.
async fn accept(
    listener: TcpListener,
    senders: &mut [Sender<'static, TcpStream, 1>],
    exec: &'static Exec,
) {
    loop {
        let mut stream = match listener.accept().await {
            Ok(s) => s,
            Err(e) => {
                log!("bench: accept: {e} HOPOS_BENCH_ACCEPT");
                exec.after(Duration::from_millis(100)).await;
                continue;
            }
        };
        while let Some(back) = hand_off(stream, senders) {
            stream = back;
            exec.after(BUSY_POLL).await;
        }
    }
}

/// Geeft `stream` aan een vrije werker; alle werkers bezig is `Some` terug.
fn hand_off(stream: TcpStream, senders: &mut [Sender<'static, TcpStream, 1>]) -> Option<TcpStream> {
    let busy = BUSY.get();
    let free = senders
        .iter_mut()
        .zip(busy.iter())
        .find(|(tx, b)| !b.get() && tx.free() > 0);
    match free {
        Some((tx, b)) => match tx.try_send(stream) {
            Ok(()) => {
                b.set(true);
                None
            }
            Err(sync::Full(back)) => Some(back),
        },
        None => Some(stream),
    }
}

/// Eén werker: wacht op een verbinding, bedient hem, en meldt zich vrij.
async fn worker(
    i: usize,
    mut rx: Receiver<'static, TcpStream, 1>,
    mut buf: Vec<u8>,
    pattern: &'static [u8],
) {
    loop {
        let stream = rx.recv().await;
        match serve(stream, &mut buf, pattern).await {
            Ok(()) => {}
            // Een client die halverwege wegging of een reset stuurde, is
            // een afgebroken meting van de client, geen fout van de node:
            // de eerste paar met de reden, daarna alleen de teller.
            Err(Fail::Net(e)) => {
                if ABORTS.fetch_add(1, Relaxed) < ABORT_LOG {
                    log!("bench: a connection ended early: {e} HOPOS_BENCH_ABORT");
                }
            }
            Err(Fail::BadCommand) => {
                log!("bench: a connection sent no valid command, closed HOPOS_BENCH_BADCMD");
            }
        }
        let n = CONNS.fetch_add(1, Relaxed).wrapping_add(1);
        if n.is_multiple_of(LOG_EVERY) {
            log!(
                "bench: conns={n} in={} out={} udp={} aborts={} HOPOS_BENCH_SERVE",
                BYTES_IN.load(Relaxed),
                BYTES_OUT.load(Relaxed),
                DATAGRAMS.load(Relaxed),
                ABORTS.load(Relaxed)
            );
        }
        if let Some(b) = BUSY.get().get(i) {
            b.set(false);
        }
    }
}

/// Eén verbinding: de commandoregel, dan de rol.
async fn serve(mut s: TcpStream, buf: &mut [u8], pattern: &[u8]) -> Result<(), Fail> {
    let (cmd, rest) = read_command(&mut s, buf).await?;
    match cmd {
        Cmd::Echo => echo(&mut s, buf, rest).await,
        Cmd::Sink(n) => sink(&mut s, buf, rest, n).await,
        Cmd::Source(n) => source(&mut s, pattern, n).await,
    }?;
    // FIN na de gebufferde data; de pomp stuurt hem.
    s.close()?;
    Ok(())
}

/// Leest tot en met de eerste `\n`; geeft het commando en hoeveel bytes
/// payload er al achter in `buf` staan (vanaf index 0 na het schuiven).
async fn read_command(s: &mut TcpStream, buf: &mut [u8]) -> Result<(Cmd, usize), Fail> {
    let mut have = 0;
    loop {
        s.set_timeout(Some(IDLE_CAP));
        let room = buf.get_mut(have..).ok_or(Fail::BadCommand)?;
        let n = s.read(room).await?;
        if n == 0 {
            return Err(Fail::BadCommand);
        }
        have += n;
        let head = buf.get(..have).unwrap_or_default();
        if let Some(nl) = head.iter().position(|b| *b == b'\n') {
            let cmd = head
                .get(..nl)
                .and_then(proto::parse)
                .ok_or(Fail::BadCommand)?;
            let rest = have - nl - 1;
            buf.copy_within(nl + 1..have, 0);
            return Ok((cmd, rest));
        }
        if have >= CMD_MAX {
            return Err(Fail::BadCommand);
        }
    }
}

/// Alles terug tot EOF. `rest` bytes staan al vooraan in `buf`.
async fn echo(s: &mut TcpStream, buf: &mut [u8], rest: usize) -> Result<(), Fail> {
    let mut n = rest;
    loop {
        if n > 0 {
            let data = buf.get(..n).unwrap_or_default();
            s.set_timeout(Some(IDLE_CAP));
            s.write_all(data).await?;
            BYTES_IN.fetch_add(n as u64, Relaxed);
            BYTES_OUT.fetch_add(n as u64, Relaxed);
        }
        s.set_timeout(Some(IDLE_CAP));
        n = s.read(buf).await?;
        if n == 0 {
            return Ok(());
        }
    }
}

/// Leest precies `want` bytes en antwoordt `sunk <n> <us>`: de tijd vanaf
/// de eerste payload-byte tot de laatste, gemeten in het slot.
async fn sink(s: &mut TcpStream, buf: &mut [u8], rest: usize, want: u64) -> Result<(), Fail> {
    let t0 = clock::now_ns();
    let mut got = rest as u64;
    while got < want {
        s.set_timeout(Some(IDLE_CAP));
        let n = s.read(buf).await?;
        if n == 0 {
            break;
        }
        got += n as u64;
    }
    let us = clock::now_ns().saturating_sub(t0) / 1000;
    BYTES_IN.fetch_add(got, Relaxed);
    let mut line = [0u8; CMD_MAX];
    let len = format_into(&mut line, format_args!("sunk {got} {us}\n"));
    s.set_timeout(Some(IDLE_CAP));
    s.write_all(line.get(..len).unwrap_or_default()).await?;
    Ok(())
}

/// Schrijft `want` bytes van het patroon.
async fn source(s: &mut TcpStream, pattern: &[u8], want: u64) -> Result<(), Fail> {
    let mut left = want;
    while left > 0 {
        let n = usize::try_from(left.min(pattern.len() as u64)).unwrap_or(pattern.len());
        s.set_timeout(Some(IDLE_CAP));
        s.write_all(pattern.get(..n).unwrap_or_default()).await?;
        left -= n as u64;
    }
    BYTES_OUT.fetch_add(want, Relaxed);
    Ok(())
}

/// De UDP-echo: elk datagram terug naar zijn afzender.
async fn udp_echo(udp: UdpSocket) {
    let mut buf = [0u8; 2048];
    loop {
        let (n, from): (usize, Endpoint) = match udp.recv_from(&mut buf).await {
            Ok(r) => r,
            Err(e) => {
                log!("bench: udp recv: {e} HOPOS_BENCH_UDP");
                EXEC.after(Duration::from_millis(100)).await;
                continue;
            }
        };
        DATAGRAMS.fetch_add(1, Relaxed);
        // Een volle zendrij is een verloren echo; de client telt dat als
        // verlies, en dat is precies wat hij meten wil.
        let _ = udp.send_to(from, buf.get(..n).unwrap_or_default()).await;
    }
}

/// Schrijft `args` in `out`; de lengte (afgekapt als het niet past).
pub(crate) fn format_into(out: &mut [u8], args: core::fmt::Arguments<'_>) -> usize {
    struct W<'a> {
        out: &'a mut [u8],
        at: usize,
    }
    impl core::fmt::Write for W<'_> {
        fn write_str(&mut self, s: &str) -> core::fmt::Result {
            for b in s.bytes() {
                let slot = self.out.get_mut(self.at).ok_or(core::fmt::Error)?;
                *slot = b;
                self.at += 1;
            }
            Ok(())
        }
    }
    let mut w = W { out, at: 0 };
    let _ = core::fmt::write(&mut w, args);
    w.at
}
