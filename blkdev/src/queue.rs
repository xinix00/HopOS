//! De voorkant voor meer opdrachten tegelijk: [`Queue`].
//!
//! Wie I/O wil, krijgt een ticket ([`AsyncBlockDevice::start_tag`]) en wacht
//! op zijn eigen completion. Het device pollen doet precies één wachter
//! tegelijk, de pacer: hij haalt met één [`reap`](AsyncBlockDevice::reap)
//! alle completions op, wekt de wachters van wat terug is, en wacht zelf op
//! het ritme van de driver (eerst per ronde, dan op een timer, of op de bel
//! van de lijn), precies zoals [`crate::Done`]. Is zijn eigen opdracht
//! klaar, dan geeft hij de rol aan een andere wachter. Linux doet dit met
//! een interrupt per completion (blk-mq, `nvme_irq`); de ANS heeft hier
//! geen lijn, en zestien wachters die elk zelf pollen kosten zestien keer
//! het device lezen per ronde.
//!
//! Eigendom: de [`Queue`] bezit de driver. Hij leeft in de taak die hem
//! gebruikt (de hopfs-actor) en wordt gedeeld door de futures van die ene
//! taak: de staat is een [`LocalCell`] die alleen binnen één poll geleend
//! wordt, nooit over een `.await` (handboek §1.1). Een buffer die aan het
//! device is gegeven, blijft van de driver tot de completion (§1.2): een
//! wachter die weggaat vóór zijn completion, laat zijn ticket achter als
//! wees, en de pacer ruimt het op als het device klaar is.

use crate::{AsyncBlockDevice, BlockIo, Error, IRQ_GUARD, LBA_SIZE, Op, Pace, Result};
use core::future::Future;
use core::pin::Pin;
use core::task::{Context, Poll, Waker};
use sync::LocalCell;

/// Zoveel tickets houdt een [`Queue`] hoogstens bij: één bit per ticket.
pub const MAX_DEPTH: usize = 64;

/// De voorkant van een device met tickets (zie de module).
pub struct Queue<D, P> {
    st: LocalCell<State<D>>,
    pace: P,
}

/// De staat achter de voorkant, alleen binnen één poll geleend.
struct State<D> {
    dev: D,
    depth: usize,
    /// Tickets die op het device staan (bit `t`).
    out: u64,
    /// Tickets waarvan de wachter wegging vóór de completion.
    orphan: u64,
    /// De waker per ticket, van wie erop wacht.
    wake: [Option<Waker>; MAX_DEPTH],
    /// De wachter die het device pollt.
    pacer: Option<usize>,
    /// Meetlat: het hoogste aantal tickets tegelijk.
    peak: usize,
}

const fn bit(t: usize) -> u64 {
    1u64 << (t % MAX_DEPTH)
}

impl<D: AsyncBlockDevice> State<D> {
    /// Haalt de completions op en wekt hun wachters; een dood device wekt
    /// iedereen (elke `poll_tag` geeft dan de fout). Daarna de wezen.
    fn collect(&mut self) {
        let done = self.dev.reap().unwrap_or(u64::MAX);
        let mut m = done & self.out;
        while m != 0 {
            let t = m.trailing_zeros() as usize;
            m &= m - 1;
            if let Some(w) = self.wake.get_mut(t).and_then(Option::take) {
                w.wake();
            }
        }
        let mut o = self.orphan;
        while o != 0 {
            let t = o.trailing_zeros() as usize;
            o &= o - 1;
            if self.dev.poll_tag(t, &mut []).is_ready() {
                self.out &= !bit(t);
                self.orphan &= !bit(t);
            }
        }
    }

    /// Ticket `t` is klaar: vrij, en de pacer-rol gaat door.
    fn release(&mut self, t: usize) {
        self.out &= !bit(t);
        self.leave(t);
    }

    /// De wachter van `t` gaat weg: geen waker meer, en was hij de pacer,
    /// dan gaat de rol door.
    fn leave(&mut self, t: usize) {
        if let Some(w) = self.wake.get_mut(t) {
            *w = None;
        }
        if self.pacer == Some(t) {
            self.pacer = None;
            // De volgende wachter pollt verder; geen wachter, dan neemt de
            // eerstvolgende die kijkt de rol.
            if let Some(w) = self.wake.iter_mut().find_map(Option::take) {
                w.wake();
            }
        }
    }

    fn park(&mut self, t: usize, w: &Waker) {
        if let Some(s) = self.wake.get_mut(t)
            && !s.as_ref().is_some_and(|o| o.will_wake(w))
        {
            *s = Some(w.clone());
        }
    }
}

impl<D: AsyncBlockDevice, P: Pace> Queue<D, P> {
    /// De voorkant van `dev`, wachtend op `pace`.
    pub fn new(dev: D, pace: P) -> Self {
        let depth = dev.depth().clamp(1, MAX_DEPTH);
        Self {
            st: LocalCell::cell(State {
                dev,
                depth,
                out: 0,
                orphan: 0,
                wake: [const { None }; MAX_DEPTH],
                pacer: None,
                peak: 0,
            }),
            pace,
        }
    }

    /// Hoeveel tickets er tegelijk kunnen zijn.
    pub fn depth(&self) -> usize {
        self.st.borrow().depth
    }

    /// Meetlat: het hoogste aantal tickets tegelijk tot nu.
    pub fn peak(&self) -> usize {
        self.st.borrow().peak
    }

    /// Hoeveel tickets er nu uitstaan.
    pub fn in_flight(&self) -> usize {
        self.st.borrow().out.count_ones() as usize
    }

    /// De driver, kort geleend (een diagnose, een meetlat).
    pub fn with_dev<R>(&self, f: impl FnOnce(&mut D) -> R) -> R {
        f(&mut self.st.borrow_mut().dev)
    }

    /// Een ticket voor `op`, of [`Error::Busy`] als er nu geen plaats is.
    fn try_start(&self, op: Op<'_>) -> Result<usize> {
        let mut st = self.st.borrow_mut();
        if st.out.count_ones() as usize >= st.depth {
            st.collect();
        }
        if st.out.count_ones() as usize >= st.depth {
            return Err(Error::Busy);
        }
        let t = match st.dev.start_tag(op) {
            Err(Error::Busy) => {
                // De ruimte zit misschien bij een wees die al terug is.
                st.collect();
                st.dev.start_tag(op)?
            }
            r => r?,
        };
        // Een ticket buiten de diepte of dat al uitstaat, is een driverfout;
        // dan is er geen weg terug (de DMA-buffer kan van twee zijn).
        if t >= st.depth || st.out & bit(t) != 0 {
            return Err(Error::Dead);
        }
        st.out |= bit(t);
        st.peak = st.peak.max(st.out.count_ones() as usize);
        Ok(t)
    }

    /// Eén opdracht: een ticket (wachtend op plaats als alles bezet is),
    /// dan de completion; bij een lees de bytes in `into`.
    pub async fn io(&self, op: Op<'_>, into: &mut [u8]) -> Result {
        let t = loop {
            match self.try_start(op) {
                Err(Error::Busy) => {
                    let period = self.st.borrow().dev.poll_pace().1;
                    self.pace.sleep(period).await;
                }
                r => break r?,
            }
        };
        Wait {
            q: self,
            t,
            into,
            t0: self.pace.now(),
            sleep: None,
            done: false,
        }
        .await
    }

    /// De brokmaat: de grootste transfer in hele LBA's, minstens één.
    fn step(&self) -> usize {
        let lba = LBA_SIZE as usize;
        let m = self.st.borrow().dev.max_transfer();
        (m - m % lba).max(lba)
    }
}

/// Het wachten op één ticket (zie de module).
#[must_use = "een future doet niets tot hij gepolld wordt"]
struct Wait<'q, 'b, D: AsyncBlockDevice, P: Pace> {
    q: &'q Queue<D, P>,
    t: usize,
    into: &'b mut [u8],
    t0: u64,
    sleep: Option<P::Sleep>,
    done: bool,
}

impl<D: AsyncBlockDevice, P: Pace> Future for Wait<'_, '_, D, P> {
    type Output = Result;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result> {
        // `Wait` is `Unpin`: verwijzingen, getallen en een `Unpin`-slaap.
        let this = self.get_mut();
        loop {
            let (irq, (spin, period)) = {
                let mut st = this.q.st.borrow_mut();
                let t = this.t;
                let pacer = *st.pacer.get_or_insert(t) == t;
                if pacer {
                    st.collect();
                }
                if let Poll::Ready(r) = st.dev.poll_tag(t, this.into) {
                    st.release(t);
                    this.done = true;
                    return Poll::Ready(r);
                }
                if !pacer {
                    st.park(t, cx.waker());
                    return Poll::Pending;
                }
                (st.dev.irq(), st.dev.poll_pace())
            };
            let wait = match irq {
                Some(bell) => {
                    if Pin::new(&mut bell.wait()).poll(cx).is_ready() {
                        this.sleep = None;
                        continue;
                    }
                    IRQ_GUARD
                }
                None => {
                    if this.q.pace.now().saturating_sub(this.t0) < spin {
                        cx.waker().wake_by_ref();
                        return Poll::Pending;
                    }
                    period
                }
            };
            let pace = &this.q.pace;
            let s = this.sleep.get_or_insert_with(|| pace.sleep(wait));
            if Pin::new(s).poll(cx).is_pending() {
                return Poll::Pending;
            }
            this.sleep = None;
        }
    }
}

impl<D: AsyncBlockDevice, P: Pace> Drop for Wait<'_, '_, D, P> {
    fn drop(&mut self) {
        if self.done {
            return;
        }
        // Weg vóór de completion: het ticket blijft van het device tot het
        // terug is (de wees), en de pacer-rol gaat door.
        if let Ok(mut st) = self.q.st.try_borrow_mut() {
            st.orphan |= bit(self.t);
            st.leave(self.t);
        }
    }
}

/// De voorkant als [`BlockIo`] voor hopfs: een gedeelde verwijzing, dus
/// meerdere verzoeken tegelijk, elk brok een ticket.
impl<D: AsyncBlockDevice, P: Pace> BlockIo for &Queue<D, P> {
    async fn read(&mut self, lba: u64, buf: &mut [u8]) -> Result {
        let q = *self;
        let mut l = lba;
        for chunk in buf.chunks_mut(q.step()) {
            let len = chunk.len();
            q.io(Op::Read { lba: l, len }, chunk).await?;
            l += len as u64 / LBA_SIZE;
        }
        Ok(())
    }

    async fn write(&mut self, lba: u64, buf: &[u8]) -> Result {
        let q = *self;
        let mut l = lba;
        for chunk in buf.chunks(q.step()) {
            q.io(
                Op::Write {
                    lba: l,
                    data: chunk,
                },
                &mut [],
            )
            .await?;
            l += chunk.len() as u64 / LBA_SIZE;
        }
        Ok(())
    }

    async fn flush(&mut self) -> Result {
        let q = *self;
        q.io(Op::Flush, &mut []).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::rc::Rc;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering::SeqCst};
    use std::task::Wake;
    use std::vec;
    use std::vec::Vec;

    /// De nep-controller: opdrachten op tags, completions die de test
    /// loslaat. Een schrijf staat bij de start op de schijf (de DMA is
    /// gebeurd), maar is pas klaar als de test hem loslaat.
    struct Ctl {
        disk: Vec<u8>,
        /// Per tag: (lees, lba, lengte).
        tags: Vec<Option<(bool, u64, usize)>>,
        /// Losgelaten, nog niet opgehaald.
        ready: u64,
        /// Opgehaald, nog niet afgerond.
        done: u64,
        reaps: usize,
    }

    #[derive(Clone)]
    struct Fake(Rc<RefCell<Ctl>>);

    fn fake(depth: usize) -> Fake {
        Fake(Rc::new(RefCell::new(Ctl {
            disk: (0..64 * 512).map(|i| (i / 512) as u8).collect(),
            tags: vec![None; depth],
            ready: 0,
            done: 0,
            reaps: 0,
        })))
    }

    impl Fake {
        fn release(&self, t: usize) {
            self.0.borrow_mut().ready |= 1 << t;
        }
        fn reaps(&self) -> usize {
            self.0.borrow().reaps
        }
    }

    impl AsyncBlockDevice for Fake {
        fn max_transfer(&self) -> usize {
            4096
        }
        fn start(&mut self, _op: Op<'_>) -> Result {
            Err(Error::Busy)
        }
        fn poll_done(&mut self, _into: &mut [u8]) -> Poll<Result> {
            Poll::Ready(Err(Error::Dead))
        }
        fn depth(&self) -> usize {
            self.0.borrow().tags.len()
        }
        fn start_tag(&mut self, op: Op<'_>) -> Result<usize> {
            let mut c = self.0.borrow_mut();
            let t = c.tags.iter().position(Option::is_none).ok_or(Error::Busy)?;
            let rec = match op {
                Op::Read { lba, len } => (true, lba, len),
                Op::Write { lba, data } => {
                    let o = lba as usize * 512;
                    c.disk[o..o + data.len()].copy_from_slice(data);
                    (false, lba, data.len())
                }
                Op::Flush => (false, 0, 0),
            };
            c.tags[t] = Some(rec);
            Ok(t)
        }
        fn poll_tag(&mut self, t: usize, into: &mut [u8]) -> Poll<Result> {
            let mut c = self.0.borrow_mut();
            if c.done & (1 << t) == 0 {
                return Poll::Pending;
            }
            c.done &= !(1 << t);
            if let Some((true, lba, len)) = c.tags[t].take() {
                let o = lba as usize * 512;
                let n = len.min(into.len());
                into[..n].copy_from_slice(&c.disk[o..o + n]);
            }
            Poll::Ready(Ok(()))
        }
        fn reap(&mut self) -> Result<u64> {
            let mut c = self.0.borrow_mut();
            c.reaps += 1;
            let m = c.ready;
            c.ready = 0;
            c.done |= m;
            Ok(m)
        }
    }

    /// Een klok die stilstaat (de pacer blijft in zijn yield-venster) en
    /// een slaap die één ronde duurt.
    struct Tick;

    impl Pace for Tick {
        type Sleep = sync::YieldNow;
        fn now(&self) -> u64 {
            0
        }
        fn sleep(&self, _d: core::time::Duration) -> sync::YieldNow {
            sync::yield_now()
        }
    }

    struct Count(AtomicUsize);

    impl Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, SeqCst);
        }
    }

    fn waker() -> (Arc<Count>, Waker) {
        let c = Arc::new(Count(AtomicUsize::new(0)));
        (c.clone(), Waker::from(c))
    }

    fn poll<F: Future>(f: Pin<&mut F>, w: &Waker) -> Poll<F::Output> {
        f.poll(&mut Context::from_waker(w))
    }

    fn rd(lba: u64) -> Op<'static> {
        Op::Read { lba, len: 512 }
    }

    #[test]
    fn three_reads_overlap_one_waiter_polls_and_each_is_woken_by_its_own_completion() {
        let dev = fake(4);
        let q = Queue::new(dev.clone(), Tick);
        let (mut a, mut b, mut c) = (vec![0u8; 512], vec![0u8; 512], vec![0u8; 512]);
        {
            let mut fa = core::pin::pin!(q.io(rd(1), &mut a));
            let mut fb = core::pin::pin!(q.io(rd(2), &mut b));
            let mut fc = core::pin::pin!(q.io(rd(3), &mut c));
            let ((_, wa), (nb, wb), (nc, wc)) = (waker(), waker(), waker());
            assert!(poll(fa.as_mut(), &wa).is_pending());
            assert!(poll(fb.as_mut(), &wb).is_pending());
            assert!(poll(fc.as_mut(), &wc).is_pending());
            assert_eq!(q.in_flight(), 3, "drie opdrachten tegelijk op het device");
            assert_eq!(dev.reaps(), 1, "alleen de pacer kijkt naar het device");
            // De completion van b: de pacer haalt hem op en wekt alleen b.
            dev.release(1);
            assert!(poll(fa.as_mut(), &wa).is_pending());
            assert_eq!((nb.0.load(SeqCst), nc.0.load(SeqCst)), (1, 0));
            assert_eq!(poll(fb.as_mut(), &wb), Poll::Ready(Ok(())));
            // De pacer is klaar: de rol gaat naar c.
            dev.release(0);
            assert_eq!(poll(fa.as_mut(), &wa), Poll::Ready(Ok(())));
            assert_eq!(nc.0.load(SeqCst), 1, "c neemt het pollen over");
            assert!(poll(fc.as_mut(), &wc).is_pending());
            dev.release(2);
            assert_eq!(poll(fc.as_mut(), &wc), Poll::Ready(Ok(())));
        }
        assert_eq!((q.in_flight(), q.peak()), (0, 3));
        assert!(b.iter().all(|&x| x == 2) && c.iter().all(|&x| x == 3));
    }

    #[test]
    fn a_full_device_makes_the_next_request_wait_for_a_completion() {
        let dev = fake(2);
        let q = Queue::new(dev.clone(), Tick);
        let (_, w) = waker();
        let mut f1 = core::pin::pin!(q.io(rd(1), &mut []));
        let mut f2 = core::pin::pin!(q.io(rd(2), &mut []));
        let mut f3 = core::pin::pin!(q.io(rd(3), &mut []));
        assert!(poll(f1.as_mut(), &w).is_pending());
        assert!(poll(f2.as_mut(), &w).is_pending());
        for _ in 0..4 {
            assert!(poll(f3.as_mut(), &w).is_pending());
        }
        assert_eq!(q.in_flight(), 2, "de derde wacht op plaats");
        dev.release(0);
        assert_eq!(poll(f1.as_mut(), &w), Poll::Ready(Ok(())));
        assert!(poll(f3.as_mut(), &w).is_pending());
        assert!(poll(f3.as_mut(), &w).is_pending());
        assert_eq!(q.in_flight(), 2, "nu staat de derde erop");
    }

    #[test]
    fn a_waiter_that_leaves_early_keeps_its_ticket_until_the_device_is_done() {
        let dev = fake(1);
        let q = Queue::new(dev.clone(), Tick);
        let (_, w) = waker();
        let mut buf = vec![0u8; 512];
        {
            let mut gone = core::pin::pin!(q.io(rd(5), &mut buf));
            assert!(poll(gone.as_mut(), &w).is_pending());
        }
        assert_eq!(q.in_flight(), 1, "de wees houdt zijn tag");
        let mut next = core::pin::pin!(q.io(rd(6), &mut []));
        for _ in 0..4 {
            assert!(poll(next.as_mut(), &w).is_pending());
        }
        assert_eq!(
            dev.0.borrow().tags[0].map(|t| t.1),
            Some(5),
            "niet hergebruikt"
        );
        dev.release(0);
        for _ in 0..4 {
            let _ = poll(next.as_mut(), &w);
        }
        assert_eq!(dev.0.borrow().tags[0].map(|t| t.1), Some(6));
        dev.release(0);
        assert_eq!(poll(next.as_mut(), &w), Poll::Ready(Ok(())));
        assert_eq!(q.in_flight(), 0);
    }
}
