//! De deuren van een vaste pool werkers: [`Doors`].

use crate::{Local, Signal};
use core::cell::Cell;

/// De stoel achter één deur.
enum Seat<T> {
    /// De werker wacht op werk.
    Free,
    /// De acceptor gaf werk; de werker haalt het op.
    Handed(T),
    /// De werker dient zijn werk.
    Busy,
}

/// Een vaste pool van `N` werkers met één acceptor (handboek §2: geen taak
/// per verbinding).
///
/// De acceptor zet het werk achter de eerste vrije deur en luidt haar bel;
/// de werker neemt het ([`take`](Self::take)), dient het, en meldt zich vrij
/// ([`free`](Self::free)), en dat luidt de bel van de acceptor. Met alle
/// werkers bezet wacht [`place`](Self::place) dus op een gebeurtenis, niet
/// op de klok (docs/apps.md); wie liever weigert, neemt
/// [`hand`](Self::hand). Elke bel heeft één wachter: werker `i` de zijne,
/// de acceptor die van de vrije deuren.
///
/// Alles op de executor van één core: de stoelen staan in een [`Local`].
pub struct Doors<T, const N: usize> {
    seats: Local<[Cell<Seat<T>>; N]>,
    bells: [Signal; N],
    freed: Signal,
}

impl<T, const N: usize> Doors<T, N> {
    /// Een pool met alle deuren vrij, voor in een `static`.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            seats: Local::new([const { Cell::new(Seat::Free) }; N]),
            bells: [const { Signal::new() }; N],
            freed: Signal::new(),
        }
    }

    /// Geeft `job` aan de eerste vrije werker en wekt hem; alle werkers
    /// bezet geeft de job terug.
    ///
    /// # Errors
    ///
    /// `job` zelf, als geen deur vrij is.
    pub fn hand(&self, job: T) -> Result<(), T> {
        for (seat, bell) in self.seats.iter().zip(&self.bells) {
            match seat.replace(Seat::Busy) {
                Seat::Free => {
                    seat.set(Seat::Handed(job));
                    bell.set();
                    return Ok(());
                }
                other => seat.set(other),
            }
        }
        Err(job)
    }

    /// Geeft `job` aan een vrije werker; zijn ze alle bezet, dan wacht hij
    /// tot er een [`free`](Self::free) meldt.
    pub async fn place(&self, mut job: T) {
        while let Err(back) = self.hand(job) {
            job = back;
            self.freed.wait().await;
        }
    }

    /// Werker `i` wacht op werk en neemt het; zijn deur is bezet tot
    /// [`free`](Self::free). Een werker zonder deur (`i >= N`) krijgt nooit
    /// iets.
    pub async fn take(&self, i: usize) -> T {
        let (Some(seat), Some(bell)) = (self.seats.get().get(i), self.bells.get(i)) else {
            return core::future::pending().await;
        };
        loop {
            bell.wait().await;
            match seat.replace(Seat::Busy) {
                Seat::Handed(job) => return job,
                // Een bel zonder werk: de stoel blijft zoals hij was.
                other => seat.set(other),
            }
        }
    }

    /// Werker `i` is klaar: zijn deur gaat open en de acceptor hoort het.
    pub fn free(&self, i: usize) {
        if let Some(seat) = self.seats.get().get(i) {
            seat.set(Seat::Free);
        }
        self.freed.set();
    }
}

impl<T, const N: usize> Default for Doors<T, N> {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{poll_once, waker};
    use core::task::Poll;

    #[test]
    fn the_first_free_door_gets_the_job_and_a_full_pool_gives_it_back() {
        let d: Doors<u32, 2> = Doors::new();
        let (_, w) = waker();
        assert_eq!(d.hand(1), Ok(()));
        assert_eq!(d.hand(2), Ok(()));
        assert_eq!(d.hand(3), Err(3));
        let mut t1 = core::pin::pin!(d.take(1));
        assert_eq!(poll_once(&mut t1, &w), Poll::Ready(2));
        // Werker 1 dient nog: zijn deur blijft dicht tot hij vrij meldt.
        assert_eq!(d.hand(3), Err(3));
        d.free(1);
        assert_eq!(d.hand(3), Ok(()));
        // Een werker zonder deur wacht voor altijd.
        let mut none = core::pin::pin!(d.take(2));
        assert_eq!(poll_once(&mut none, &w), Poll::Pending);
    }

    #[test]
    fn a_full_pool_waits_for_a_free_door_not_for_the_clock() {
        let d: Doors<u32, 1> = Doors::new();
        let (woken, w) = waker();
        let mut t0 = core::pin::pin!(d.take(0));
        assert_eq!(poll_once(&mut t0, &w), Poll::Pending);
        assert_eq!(d.hand(7), Ok(()));
        assert_eq!(poll_once(&mut t0, &w), Poll::Ready(7));
        let mut p = core::pin::pin!(d.place(8));
        assert_eq!(poll_once(&mut p, &w), Poll::Pending);
        let before = woken.0.load(std::sync::atomic::Ordering::SeqCst);
        d.free(0);
        assert!(woken.0.load(std::sync::atomic::Ordering::SeqCst) > before);
        assert_eq!(poll_once(&mut p, &w), Poll::Ready(()));
        let mut again = core::pin::pin!(d.take(0));
        assert_eq!(poll_once(&mut again, &w), Poll::Ready(8));
    }
}
