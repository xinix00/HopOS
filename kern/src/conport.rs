//! De console over TCP: een ring met de laatste bytes van de console, en een
//! lezer-taak die hem vanaf het begin afspeelt en daarna volgt.
//!
//! De ring is geheugen, geen UART: wie verbindt, krijgt eerst de replay (wat
//! er nog in de ring staat) en dan de rest zodra het komt. Een trage lezer
//! verliest de oudste bytes, nooit de node: de schrijver wacht op niemand.
//! Hooguit [`MAX_READERS`] lezers tegelijk; elke lezer houdt een verbinding
//! vast op HOP's eigen stack, en geeft zijn plaats terug zodra de peer weg
//! is ([`Gone`]).

use crate::cage::Timer;
use crate::system::Conn;
use core::fmt;
use core::sync::atomic::{
    AtomicU8, AtomicU64,
    Ordering::{AcqRel, Relaxed},
};
use core::time::Duration;
use sync::{Either, select};

/// Hooguit zoveel lezers tegelijk.
pub const MAX_READERS: u8 = 4;
/// Hoe vaak een lezer naar nieuwe bytes kijkt.
pub const POLL: Duration = Duration::from_millis(100);
/// Zo lang mag data uitstaan zonder dat de peer iets bevestigt; daarna is
/// hij weg. Zonder deze grens hield een lezer die verdween zonder RST zijn
/// plaats tot de hertransmissieladder van de stack opgaf (12 pogingen tot
/// 60 s RTO: minuten), en een peer in een nulvenster voorgoed. Op de O6N en
/// de Radxa (02-10) liep zo de console vol na een dag meetrondes met
/// `nc | head` die abrupt sluiten. Een levende lezer bevestigt binnen een
/// RTT; tien seconden is ook op wifi ruim.
pub const STALL: Duration = Duration::from_secs(10);
/// Elke zoveelste vrijgegeven plaats geeft een regel met de tellers.
pub const FREED_LINE: u64 = 100;
/// Wat een client hoort als alle plaatsen bezet zijn: één regel vóór de
/// close, in plaats van een stille weigering die op een dode node lijkt.
pub const FULL: &[u8] = &full();

/// Bouwt [`FULL`] met [`MAX_READERS`] erin.
const fn full() -> [u8; 25] {
    const { assert!(MAX_READERS < 10) };
    let mut m = *b"console: full, 0 readers\n";
    m[15] += MAX_READERS;
    m
}

/// De ring: `N` bytes, met een monotone schrijfpositie.
///
/// # Invariants
///
/// De bytes `[head - min(head, N), head)` staan in `buf`, byte `p` op
/// `p % N`.
pub struct Ring<const N: usize> {
    buf: [u8; N],
    head: u64,
}

impl<const N: usize> Ring<N> {
    /// Een lege ring.
    #[must_use]
    pub const fn new() -> Self {
        const { assert!(N > 0) };
        Ring {
            buf: [0; N],
            head: 0,
        }
    }

    /// Schrijft bytes; de oudste vallen eruit.
    pub fn write(&mut self, b: &[u8]) {
        for x in b {
            if let Some(slot) = self.buf.get_mut((self.head % N as u64) as usize) {
                *slot = *x;
            }
            self.head = self.head.wrapping_add(1);
        }
    }

    /// De schrijfpositie: alles ervoor is geschreven.
    #[must_use]
    pub fn head(&self) -> u64 {
        self.head
    }

    /// De oudste positie die nog in de ring staat: waar een nieuwe lezer
    /// begint (de replay).
    #[must_use]
    pub fn oldest(&self) -> u64 {
        self.head.saturating_sub(N as u64)
    }

    /// Kopieert de bytes vanaf positie `seen` naar `out`; geeft het aantal
    /// en de volgende positie. Wie te ver achterliep, begint bij de oudste.
    pub fn since(&self, seen: u64, out: &mut [u8]) -> (usize, u64) {
        let from = seen.max(self.oldest());
        let n = ((self.head - from.min(self.head)) as usize).min(out.len());
        for (i, o) in out.iter_mut().take(n).enumerate() {
            let p = from + i as u64;
            *o = self.buf.get((p % N as u64) as usize).copied().unwrap_or(0);
        }
        (n, from + n as u64)
    }
}

impl<const N: usize> Default for Ring<N> {
    fn default() -> Self {
        Self::new()
    }
}

/// De UART als lezer van de ring: tot waar de ring op de draad staat.
///
/// De schrijver van een regel wacht niet op de baudrate: hij zet de regel
/// in de ring, en [`Uart::pump`] geeft de UART wat er nu in zijn zend-FIFO
/// past; de rest volgt bij de volgende pomp. Afgekeken van Linux: de
/// `xmit`-ring van een `uart_port` die de TX-interrupt leegt, en de
/// printer-thread van een nbcon-console (6.12), waar `printk` alleen nog
/// in de ring schrijft. Valt de UART een hele ring achter, dan verliest hij
/// de oudste bytes, en hij telt ze ([`Uart::take_dropped`]); de ring en de
/// TCP-lezers houden ze.
#[derive(Default)]
pub struct Uart {
    /// De eerste positie die nog niet op de draad staat.
    seen: u64,
    /// Bytes die de ring overschreef voor de UART ze had.
    dropped: u64,
}

impl Uart {
    /// Een UART die nog niets van de ring heeft.
    #[must_use]
    pub const fn new() -> Self {
        Uart {
            seen: 0,
            dropped: 0,
        }
    }

    /// Alles in `ring` staat al op de draad: de schrijver schreef het zelf,
    /// wachtend.
    pub fn caught_up<const N: usize>(&mut self, ring: &Ring<N>) {
        self.seen = ring.head();
    }

    /// Zoveel bytes wachten nog op de UART.
    #[must_use]
    pub fn behind<const N: usize>(&self, ring: &Ring<N>) -> u64 {
        ring.head().saturating_sub(self.seen.max(ring.oldest()))
    }

    /// De positie in de ring tot waar de UART hem heeft: loopt alleen op.
    #[must_use]
    pub fn at(&self) -> u64 {
        self.seen
    }

    /// Geeft `put` de bytes die nog niet op de draad staan, in stukken, tot
    /// hij er minder neemt dan hij kreeg (de FIFO is vol) of de ring op is.
    /// `put` geeft hoeveel bytes eruit zijn. `true` = er blijft iets liggen.
    pub fn pump<const N: usize>(
        &mut self,
        ring: &Ring<N>,
        mut put: impl FnMut(&[u8]) -> usize,
    ) -> bool {
        let mut buf = [0u8; 64];
        loop {
            let from = self.seen.max(ring.oldest());
            self.dropped = self.dropped.wrapping_add(from - self.seen.min(from));
            let (n, _) = ring.since(from, &mut buf);
            let Some(chunk) = buf.get(..n) else {
                return false;
            };
            if n == 0 {
                self.seen = from;
                return false;
            }
            let took = put(chunk).min(n);
            self.seen = from + took as u64;
            if took < n {
                return true;
            }
        }
    }

    /// De overschreven bytes sinds de vorige vraag.
    pub fn take_dropped(&mut self) -> u64 {
        core::mem::take(&mut self.dropped)
    }
}

/// Telt de lezers; [`Readers::admit`] geeft een plaats of `None`, en
/// [`Readers::freed`] telt waarom een plaats vrijkwam.
pub struct Readers {
    seats: AtomicU8,
    /// Vrijgegeven plaatsen per [`Gone`], in die volgorde.
    freed: [AtomicU64; 3],
}

impl Readers {
    /// Nul lezers.
    #[must_use]
    pub const fn new() -> Readers {
        Readers {
            seats: AtomicU8::new(0),
            freed: [AtomicU64::new(0), AtomicU64::new(0), AtomicU64::new(0)],
        }
    }

    /// Een plaats, als er nog een is. `Drop` geeft hem terug.
    pub fn admit(&self) -> Option<Seat<'_>> {
        if self.seats.fetch_add(1, AcqRel) >= MAX_READERS {
            self.seats.fetch_sub(1, AcqRel);
            return None;
        }
        Some(Seat(self))
    }

    /// Telt een plaats die vrijkwam omdat de lezer `why` wegging; elke
    /// [`FREED_LINE`]-ste geeft de tellers voor één luide regel.
    pub fn freed(&self, why: Gone) -> Option<Freed> {
        if let Some(c) = self.freed.get(why as usize) {
            c.fetch_add(1, Relaxed);
        }
        let [fin, reset, stall] = self.freed.each_ref().map(|c| c.load(Relaxed));
        let total = fin.wrapping_add(reset).wrapping_add(stall);
        total
            .is_multiple_of(FREED_LINE)
            .then_some(Freed { fin, reset, stall })
    }
}

impl Default for Readers {
    fn default() -> Self {
        Readers::new()
    }
}

/// Een bezette lezersplaats.
pub struct Seat<'a>(&'a Readers);

impl Drop for Seat<'_> {
    fn drop(&mut self) {
        self.0.seats.fetch_sub(1, AcqRel);
    }
}

/// Waarom een lezer wegging.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Gone {
    /// Een nette FIN (EOF op de leeskant).
    Fin = 0,
    /// Een RST, een mislukte schrijf of een verbinding die de stack al
    /// opruimde.
    Reset = 1,
    /// Data stond langer dan [`STALL`] uit zonder één bevestiging.
    Stall = 2,
}

/// De tellers achter [`Readers::freed`], voor de regel.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Freed {
    /// Vertrokken met een FIN.
    pub fin: u64,
    /// Vertrokken met een RST of een mislukte schrijf.
    pub reset: u64,
    /// Opgegeven na [`STALL`] zonder bevestiging.
    pub stall: u64,
}

impl fmt::Display for Freed {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let total = self.fin.wrapping_add(self.reset).wrapping_add(self.stall);
        write!(
            f,
            "{total} reader seats freed: fin {}, reset {}, no ack in {} s {}",
            self.fin,
            self.reset,
            STALL.as_secs(),
            self.stall
        )
    }
}

/// Speelt de ring af naar `conn` en volgt hem, tot de lezer weggaat; geeft
/// waarom.
///
/// `snapshot` kopieert de bytes vanaf een positie uit de ring (de ring woont
/// bij zijn eigenaar, meestal een `LocalCell`; de lening leeft alleen in die
/// aanroep). `unacked` geeft wat de peer nog niet bevestigde, of `None` als
/// de verbinding weg is (een RST, of al opgeruimd); dat is de sonde die
/// elke beurt kijkt, ook als een schrijf vastzit op een volle zendring.
///
/// Elke wacht, op schrijven of op lezen, duurt hooguit [`POLL`]. Weg is:
/// EOF of een fout op de leeskant, een mislukte schrijf, `unacked` zegt
/// `None`, of data die [`STALL`] uitstaat zonder dat de peer iets bevestigt
/// (een peer die zonder RST verdween, of in een nulvenster bleef hangen).
pub async fn stream(
    conn: &mut impl Conn,
    timer: &impl Timer,
    mut snapshot: impl FnMut(u64, &mut [u8]) -> (usize, u64),
    mut unacked: impl FnMut() -> Option<usize>,
    start: u64,
    buf: &mut [u8],
) -> Gone {
    let mut seen = start;
    let mut sink = [0u8; 64];
    let mut acks = Acks::new(timer.now());
    loop {
        let (n, next) = snapshot(seen, buf);
        if n > 0 {
            let w = select(conn.write(buf.get(..n).unwrap_or(&[])), timer.sleep(POLL)).await;
            match w {
                Either::Left(Ok(w)) if w > 0 => {
                    seen = next - (n - w.min(n)) as u64;
                    acks.sent(w);
                }
                Either::Left(_) => return Gone::Reset,
                Either::Right(()) => {}
            }
        } else {
            // Wat de lezer stuurt, wordt weggegooid; EOF of een fout is: weg.
            match select(conn.read(&mut sink), timer.sleep(POLL)).await {
                Either::Left(Ok(0)) => return Gone::Fin,
                Either::Left(Err(_)) => return Gone::Reset,
                Either::Left(Ok(_)) | Either::Right(()) => {}
            }
        }
        if let Some(why) = acks.check(unacked(), timer.now()) {
            return why;
        }
    }
}

/// Of de peer nog bevestigt: wat de stack van ons aannam min wat nog
/// uitstaat, is wat de peer bevestigde.
struct Acks {
    /// Bytes die de stack van ons aannam.
    sent: u64,
    /// Het hoogste bevestigde getal tot nu toe.
    acked: u64,
    /// Het laatste moment dat de peer leefde: niets uitstaand, of een
    /// nieuwe bevestiging.
    heard: u64,
}

impl Acks {
    fn new(now: u64) -> Acks {
        Acks {
            sent: 0,
            acked: 0,
            heard: now,
        }
    }

    fn sent(&mut self, n: usize) {
        self.sent = self.sent.saturating_add(n as u64);
    }

    /// `None` zolang de peer er is, anders waarom niet.
    fn check(&mut self, unacked: Option<usize>, now: u64) -> Option<Gone> {
        let Some(out) = unacked else {
            return Some(Gone::Reset);
        };
        let out = out as u64;
        let acked = self.sent.saturating_sub(out);
        if out == 0 || acked > self.acked {
            self.acked = acked;
            self.heard = now;
            return None;
        }
        let stall = u64::try_from(STALL.as_nanos()).unwrap_or(u64::MAX);
        (now.saturating_sub(self.heard) >= stall).then_some(Gone::Stall)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Result;
    use crate::testutil::{FakeTimer, block_on};
    use core::future::Future;
    use std::cell::RefCell;
    use std::vec::Vec;

    #[test]
    fn ring_replays_and_drops_the_oldest() {
        let mut r: Ring<8> = Ring::new();
        r.write(b"abc");
        let mut out = [0u8; 16];
        assert_eq!(r.since(0, &mut out), (3, 3));
        assert_eq!(&out[..3], b"abc");
        r.write(b"defghij"); // 10 bytes, de ring houdt 8.
        assert_eq!(r.oldest(), 2);
        let (n, next) = r.since(0, &mut out);
        assert_eq!((&out[..n], next), (&b"cdefghij"[..], 10));
        assert_eq!(r.since(10, &mut out), (0, 10));
        let mut small = [0u8; 3];
        assert_eq!(r.since(5, &mut small), (3, 8));
        assert_eq!(&small, b"fgh");
    }

    #[test]
    fn the_uart_takes_what_fits_and_the_rest_waits() {
        let mut r: Ring<256> = Ring::new();
        let mut u = Uart::new();
        r.write(b"0123456789");
        let mut wire = Vec::new();
        // Een FIFO van vier: de pomp stopt na de eerste weigering.
        let more = u.pump(&r, |b| {
            let n = b.len().min(4);
            wire.extend_from_slice(&b[..n]);
            n
        });
        assert!(more);
        assert_eq!(wire, b"0123");
        assert_eq!(u.behind(&r), 6);
        // De FIFO is leeg: de rest gaat eruit, de pomp is klaar.
        assert!(!u.pump(&r, |b| {
            wire.extend_from_slice(b);
            b.len()
        }));
        assert_eq!(wire, b"0123456789");
        assert_eq!(u.behind(&r), 0);
        // Wat de schrijver zelf wachtend wegschreef, pompt niemand opnieuw.
        r.write(b"abc");
        u.caught_up(&r);
        assert!(!u.pump(&r, |_| panic!("nothing is due")));
        assert_eq!(u.take_dropped(), 0);
    }

    #[test]
    fn a_uart_a_whole_ring_behind_counts_what_it_lost() {
        let mut r: Ring<8> = Ring::new();
        let mut u = Uart::new();
        r.write(b"abcdefghijkl"); // 12 bytes in een ring van 8.
        let mut wire = Vec::new();
        assert!(!u.pump(&r, |b| {
            wire.extend_from_slice(b);
            b.len()
        }));
        assert_eq!(wire, b"efghijkl");
        assert_eq!(u.take_dropped(), 4);
        assert_eq!(u.take_dropped(), 0);
    }

    #[test]
    fn readers_are_capped() {
        let rd = Readers::new();
        let seats: Vec<_> = (0..MAX_READERS).map(|_| rd.admit().unwrap()).collect();
        assert!(rd.admit().is_none());
        drop(seats);
        assert!(rd.admit().is_some());
    }

    #[test]
    fn full_names_the_readers() {
        assert_eq!(FULL, b"console: full, 4 readers\n");
    }

    #[test]
    fn every_hundredth_freed_seat_gives_a_line() {
        let rd = Readers::new();
        for _ in 0..FREED_LINE - 2 {
            assert_eq!(rd.freed(Gone::Reset), None);
        }
        assert_eq!(rd.freed(Gone::Stall), None);
        let line = rd.freed(Gone::Fin).unwrap();
        assert_eq!(
            line,
            Freed {
                fin: 1,
                reset: FREED_LINE - 2,
                stall: 1
            }
        );
        assert!(line.to_string().starts_with("100 reader seats freed"));
    }

    struct Client {
        got: Vec<u8>,
        closed: bool,
        /// Een volle zendring: elke schrijf wacht voorgoed.
        stuck: bool,
    }

    impl Client {
        fn new(closed: bool, stuck: bool) -> Client {
            Client {
                got: Vec::new(),
                closed,
                stuck,
            }
        }
    }

    impl Conn for Client {
        fn read(&mut self, _: &mut [u8]) -> impl Future<Output = Result<usize>> {
            let closed = self.closed;
            async move {
                if !closed {
                    core::future::pending::<()>().await;
                }
                Ok(0)
            }
        }
        fn write(&mut self, b: &[u8]) -> impl Future<Output = Result<usize>> {
            let stuck = self.stuck;
            if !stuck {
                self.got.extend_from_slice(b);
            }
            let n = b.len();
            async move {
                if stuck {
                    core::future::pending::<()>().await;
                }
                Ok(n)
            }
        }
        fn remote_ip4(&self) -> u32 {
            0
        }
    }

    /// Speelt een ring met de bootregel af naar `c`, met `unacked` als
    /// sonde; geeft waarom de lezer wegging en de klok op dat moment.
    fn run(c: &mut Client, unacked: impl FnMut() -> Option<usize>) -> (Gone, u64) {
        let ring = RefCell::new(Ring::<64>::new());
        ring.borrow_mut().write(b"HOPOS_CONPORT_UP\n");
        let timer = FakeTimer::default();
        let mut buf = [0u8; 16];
        let start = ring.borrow().oldest();
        let why = block_on(stream(
            c,
            &timer,
            |s, o| ring.borrow().since(s, o),
            unacked,
            start,
            &mut buf,
        ));
        (why, timer.now.get())
    }

    // Een lezer die zonder verdere consoleregel verdwijnt, geeft zijn
    // plaats terug (conport_test.go).
    #[test]
    fn idle_console_disconnect_releases_stream() {
        let mut c = Client::new(true, false);
        let (why, _) = run(&mut c, || Some(0));
        assert_eq!(why, Gone::Fin);
        assert_eq!(c.got, b"HOPOS_CONPORT_UP\n", "replay missing");
    }

    // Een RST terwijl de schrijf op een volle zendring wacht: de sonde ziet
    // hem na één tik, niet pas als de stack opgeeft.
    #[test]
    fn reset_frees_a_reader_stuck_in_write() {
        let mut c = Client::new(false, true);
        let (why, now) = run(&mut c, || None);
        assert_eq!(why, Gone::Reset);
        assert!(now <= 2 * POLL.as_nanos() as u64, "took {now} ns");
    }

    // Een peer die zonder RST verdween: de data staat uit, er komt geen ack,
    // en na STALL is de plaats terug.
    #[test]
    fn silent_peer_is_dropped_after_stall() {
        let mut c = Client::new(false, false);
        let (why, now) = run(&mut c, || Some(17));
        assert_eq!(why, Gone::Stall);
        assert_eq!(c.got, b"HOPOS_CONPORT_UP\n");
        let stall = STALL.as_nanos() as u64;
        assert!(now >= stall && now <= stall + 2 * POLL.as_nanos() as u64);
    }

    // Wie bevestigt, blijft, ook als er steeds iets uitstaat; wie niets
    // uitstaan heeft, blijft ook. Pas STALL zonder voortgang is weg.
    #[test]
    fn acks_keep_a_slow_reader() {
        let stall = STALL.as_nanos() as u64;
        let mut a = Acks::new(0);
        a.sent(100);
        assert_eq!(a.check(Some(100), stall - 1), None);
        assert_eq!(a.check(Some(60), stall + 1), None); // 40 bevestigd.
        a.sent(100);
        assert_eq!(a.check(Some(150), 2 * stall), None); // Weer 10 erbij.
        assert_eq!(a.check(Some(150), 3 * stall), Some(Gone::Stall));
        let mut idle = Acks::new(0);
        assert_eq!(idle.check(Some(0), 10 * stall), None);
        assert_eq!(idle.check(None, 10 * stall), Some(Gone::Reset));
    }
}
