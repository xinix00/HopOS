//! De console over TCP: een ring met de laatste bytes van de console, en een
//! lezer-taak die hem vanaf het begin afspeelt en daarna volgt.
//!
//! De ring is geheugen, geen UART: wie verbindt, krijgt eerst de replay (wat
//! er nog in de ring staat) en dan de rest zodra het komt. Een trage lezer
//! verliest de oudste bytes, nooit de node: de schrijver wacht op niemand.
//! Hooguit [`MAX_READERS`] lezers tegelijk; elke lezer houdt een verbinding
//! vast op HOP's eigen stack.

use crate::cage::Timer;
use crate::system::Conn;
use core::sync::atomic::{AtomicU8, Ordering::AcqRel};
use core::time::Duration;
use sync::{Either, select};

/// Hooguit zoveel lezers tegelijk.
pub const MAX_READERS: u8 = 4;
/// Hoe vaak een lezer naar nieuwe bytes kijkt.
pub const POLL: Duration = Duration::from_millis(100);

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

/// Telt de lezers; [`Readers::admit`] geeft een plaats of `None`.
pub struct Readers(AtomicU8);

impl Readers {
    /// Nul lezers.
    #[must_use]
    pub const fn new() -> Readers {
        Readers(AtomicU8::new(0))
    }

    /// Een plaats, als er nog een is. `Drop` geeft hem terug.
    pub fn admit(&self) -> Option<Seat<'_>> {
        if self.0.fetch_add(1, AcqRel) >= MAX_READERS {
            self.0.fetch_sub(1, AcqRel);
            return None;
        }
        Some(Seat(self))
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
        (self.0).0.fetch_sub(1, AcqRel);
    }
}

/// Speelt de ring af naar `conn` en volgt hem, tot de lezer weggaat.
///
/// `snapshot` kopieert de bytes vanaf een positie uit de ring (de ring woont
/// bij zijn eigenaar, meestal een `LocalCell`; de lening leeft alleen in die
/// aanroep). Een lezer die sluit, wordt gezien zonder dat er eerst nog een
/// consoleregel hoeft te komen: de lus wacht op lezen OF de tik.
pub async fn stream(
    conn: &mut impl Conn,
    timer: &impl Timer,
    mut snapshot: impl FnMut(u64, &mut [u8]) -> (usize, u64),
    start: u64,
    buf: &mut [u8],
) {
    let mut seen = start;
    let mut sink = [0u8; 64];
    loop {
        loop {
            let (n, next) = snapshot(seen, buf);
            if n == 0 {
                break;
            }
            let mut off = 0;
            while off < n {
                match conn.write(buf.get(off..n).unwrap_or(&[])).await {
                    Ok(w) if w > 0 => off += w,
                    _ => return, // Lezer weg.
                }
            }
            seen = next;
        }
        // Wat de lezer stuurt, wordt weggegooid; EOF of een fout is: weg.
        if let Either::Left(r) = select(conn.read(&mut sink), timer.sleep(POLL)).await
            && !matches!(r, Ok(n) if n > 0)
        {
            return;
        }
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
    fn readers_are_capped() {
        let rd = Readers::new();
        let seats: Vec<_> = (0..MAX_READERS).map(|_| rd.admit().unwrap()).collect();
        assert!(rd.admit().is_none());
        drop(seats);
        assert!(rd.admit().is_some());
    }

    struct Client {
        got: Vec<u8>,
        closed: bool,
    }

    impl Conn for Client {
        fn read(&mut self, _: &mut [u8]) -> impl Future<Output = Result<usize>> {
            let closed = self.closed;
            async move {
                if closed {
                    Ok(0)
                } else {
                    core::future::pending::<()>().await;
                    Ok(0)
                }
            }
        }
        fn write(&mut self, b: &[u8]) -> impl Future<Output = Result<usize>> {
            self.got.extend_from_slice(b);
            core::future::ready(Ok(b.len()))
        }
        fn remote_ip4(&self) -> u32 {
            0
        }
    }

    // Een lezer die zonder verdere consoleregel verdwijnt, geeft zijn
    // plaats terug (conport_test.go).
    #[test]
    fn idle_console_disconnect_releases_stream() {
        let ring = RefCell::new(Ring::<64>::new());
        ring.borrow_mut().write(b"HOPOS_CONPORT_UP\n");
        let mut c = Client {
            got: Vec::new(),
            closed: true,
        };
        let timer = FakeTimer::default();
        let mut buf = [0u8; 16];
        let start = ring.borrow().oldest();
        block_on(stream(
            &mut c,
            &timer,
            |s, o| ring.borrow().since(s, o),
            start,
            &mut buf,
        ));
        assert_eq!(c.got, b"HOPOS_CONPORT_UP\n", "replay missing");
    }
}
