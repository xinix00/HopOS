use super::*;
use core::cell::Cell;
use core::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering::SeqCst};
use std::task::Wake;

/// Een schijf in RAM zonder tags (zoals virtio-blk): één opdracht tegelijk
/// op ticket 0, die pas na `lag` keer kijken klaar is; telt hoe vaak er
/// gekeken werd.
struct Lagging {
    disk: Vec<u8>,
    lag: usize,
    left: usize,
    pending: Option<(bool, u64, usize)>,
    polls: usize,
    submits: Vec<(u64, usize)>,
    flushes: usize,
    bell: Option<&'static Signal>,
    max: usize,
}

impl Lagging {
    fn new(bytes: usize, lag: usize, max: usize) -> Self {
        Self {
            disk: vec![0; bytes],
            lag,
            left: 0,
            pending: None,
            polls: 0,
            submits: Vec::new(),
            flushes: 0,
            bell: None,
            max,
        }
    }
}

impl AsyncBlockDevice for Lagging {
    fn max_transfer(&self) -> usize {
        self.max
    }
    fn start_tag(&mut self, op: Op<'_>) -> Result<usize> {
        if self.pending.is_some() {
            return Err(Error::Busy);
        }
        self.left = self.lag;
        match op {
            Op::Read { lba, len } => {
                self.submits.push((lba, len));
                self.pending = Some((true, lba, len));
            }
            Op::Write { lba, data } => {
                self.submits.push((lba, data.len()));
                let o = (lba * LBA_SIZE) as usize;
                self.disk[o..o + data.len()].copy_from_slice(data);
                self.pending = Some((false, lba, data.len()));
            }
            Op::Flush => {
                self.flushes += 1;
                self.pending = Some((false, 0, 0));
            }
        }
        Ok(0)
    }
    fn poll_tag(&mut self, _t: usize, into: &mut [u8]) -> Poll<Result> {
        self.polls += 1;
        if self.left > 0 {
            self.left -= 1;
            return Poll::Pending;
        }
        let Some((read, lba, len)) = self.pending.take() else {
            return Poll::Ready(Err(Error::Io { lba: 0 }));
        };
        if read {
            let o = (lba * LBA_SIZE) as usize;
            into[..len].copy_from_slice(&self.disk[o..o + len]);
        }
        Poll::Ready(Ok(()))
    }
    fn irq(&self) -> Option<&'static Signal> {
        self.bell
    }
}

/// Een klok die de test zet, en slapen die telt.
struct Clock {
    now: Cell<u64>,
    sleeps: Cell<usize>,
    last: Cell<Duration>,
}

impl Clock {
    fn new() -> Self {
        Self {
            now: Cell::new(0),
            sleeps: Cell::new(0),
            last: Cell::new(Duration::ZERO),
        }
    }
}

/// Een slaap die nooit afloopt: alleen een bel of een nieuwe poll gaat verder.
struct Never;

impl Future for Never {
    type Output = ();
    fn poll(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<()> {
        Poll::Pending
    }
}

impl Pace for &Clock {
    type Sleep = Never;
    fn now(&self) -> u64 {
        self.now.get()
    }
    fn sleep(&self, d: Duration) -> Never {
        self.sleeps.set(self.sleeps.get() + 1);
        self.last.set(d);
        Never
    }
}

struct Count(AtomicUsize);

impl Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, SeqCst);
    }
}

/// Een lees van 512 bytes op LBA 0.
const RD: Op<'static> = Op::Read { lba: 0, len: 512 };

#[test]
fn a_request_is_cut_by_max_transfer_and_round_trips() {
    let q = Queue::new(Lagging::new(64 * 1024, 2, 4096), Spin);
    let mut io = &q;
    let data: Vec<u8> = (0..10_240u32).map(|i| (i % 251) as u8).collect();
    block_on(io.write(8, &data)).unwrap();
    let mut got = vec![0; data.len()];
    block_on(io.read(8, &mut got)).unwrap();
    assert_eq!(got, data);
    block_on(io.flush()).unwrap();
    q.with_dev(|d| {
        // 10 KiB in brokken van 4 KiB: 4 + 4 + 2, elk op de juiste LBA.
        assert_eq!(d.submits[..3], [(8, 4096), (16, 4096), (24, 2048)]);
        assert_eq!(d.flushes, 1);
    });
}

#[test]
fn without_a_line_it_yields_first_and_then_sleeps_a_poll_period() {
    let clock = Clock::new();
    let q = Queue::new(Lagging::new(4096, usize::MAX, 4096), &clock);
    let c = Arc::new(Count(AtomicUsize::new(0)));
    let waker = Waker::from(c.clone());
    let mut cx = Context::from_waker(&waker);
    let mut f = core::pin::pin!(q.io(RD, &mut []));
    // Binnen het spin-venster: een yield (zelf wekken), geen timer.
    assert!(f.as_mut().poll(&mut cx).is_pending());
    assert_eq!(c.0.load(SeqCst), 1);
    assert_eq!(clock.sleeps.get(), 0);
    // Daarna: een timer van de pollperiode, en niet meer zelf wekken.
    clock.now.set(POLL_SPIN_NS);
    assert!(f.as_mut().poll(&mut cx).is_pending());
    assert_eq!(c.0.load(SeqCst), 1);
    assert_eq!(clock.sleeps.get(), 1);
    assert_eq!(clock.last.get(), POLL_PERIOD);
    // Een volgende poll hergebruikt de lopende timer.
    assert!(f.as_mut().poll(&mut cx).is_pending());
    assert_eq!(clock.sleeps.get(), 1);
}

/// Een [`Lagging`] die zijn eigen pollritme opgeeft, zoals de ANS.
struct OwnPace(Lagging);

impl AsyncBlockDevice for OwnPace {
    fn max_transfer(&self) -> usize {
        self.0.max_transfer()
    }
    fn start_tag(&mut self, op: Op<'_>) -> Result<usize> {
        self.0.start_tag(op)
    }
    fn poll_tag(&mut self, t: usize, into: &mut [u8]) -> Poll<Result> {
        self.0.poll_tag(t, into)
    }
    fn poll_pace(&self) -> (u64, Duration) {
        (20_000, Duration::from_micros(20))
    }
}

#[test]
fn a_driver_can_choose_its_own_poll_pace() {
    let clock = Clock::new();
    let q = Queue::new(OwnPace(Lagging::new(4096, usize::MAX, 4096)), &clock);
    let c = Arc::new(Count(AtomicUsize::new(0)));
    let waker = Waker::from(c.clone());
    let mut cx = Context::from_waker(&waker);
    let mut f = core::pin::pin!(q.io(RD, &mut []));
    assert!(f.as_mut().poll(&mut cx).is_pending());
    assert_eq!(clock.sleeps.get(), 0);
    // Na zijn eigen spin-venster de timer van zijn eigen periode.
    clock.now.set(20_000);
    assert!(f.as_mut().poll(&mut cx).is_pending());
    assert_eq!(clock.last.get(), Duration::from_micros(20));
}

#[test]
fn with_a_line_it_waits_on_the_bell_behind_a_guard() {
    static BELL: Signal = Signal::new();
    let clock = Clock::new();
    let mut dev = Lagging::new(4096, 3, 4096);
    dev.bell = Some(&BELL);
    let q = Queue::new(dev, &clock);
    let c = Arc::new(Count(AtomicUsize::new(0)));
    let waker = Waker::from(c.clone());
    let mut cx = Context::from_waker(&waker);
    let mut f = core::pin::pin!(q.io(Op::Flush, &mut []));
    assert!(f.as_mut().poll(&mut cx).is_pending());
    assert_eq!(clock.last.get(), IRQ_GUARD, "de vangrail staat");
    assert_eq!(c.0.load(SeqCst), 0, "geen yield: de executor mag slapen");
    // De bel wekt de wachter; die kijkt tot het device klaar is.
    BELL.set();
    assert_eq!(c.0.load(SeqCst), 1);
    assert!(f.as_mut().poll(&mut cx).is_pending());
    BELL.set();
    assert_eq!(f.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
}

#[test]
fn a_borrowed_driver_is_the_same_path_and_comes_back() {
    // De meetbank vóór de executor: de driver geleend, `block_on` met een
    // pollende `Pace`, en daarna is hij weer van de eigenaar.
    let mut dev = Lagging::new(8192, 3, 4096);
    {
        let q = Queue::new(&mut dev, Spin);
        let mut io = &q;
        block_on(io.write(2, &[9; 1024])).unwrap();
        let mut got = [0; 1024];
        block_on(io.read(2, &mut got)).unwrap();
        assert_eq!(got, [9; 1024]);
    }
    assert_eq!(dev.submits, vec![(2, 1024), (2, 1024)]);
    // Elk verzoek werd gepolld tot het device klaar was: drie keer te
    // vroeg plus één keer raak, per opdracht.
    assert_eq!(dev.polls, 8);
}
