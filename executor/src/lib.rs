//! Eén executor per core (handboek §4).
//!
//! De executor is de governor uit de Go-kern, maar dan als voordeur: hij
//! bezit de takenlijst en het timerwiel, en als er niets te doen is vraagt
//! hij het board te slapen tot de vroegste deadline of tot een wek. Een
//! wek is één bit (`Slot::ready`) plus `dev::notify`, en dat mag uit een
//! ISR of van een andere core komen.
//!
//! De ronde: alle taken met een gezet bit pollen, dan de timers die
//! verstreken zijn wekken, en als er in die ronde niets gebeurd is slapen.
//! De verloren-wek-race is dicht doordat de [`Sleeper`] het `ready`-
//! predicaat nog één keer toetst mét interrupts gemaskeerd.
//!
//! Wat hier niet staat: prioriteiten, werk-stelen, een tweede core. Elke
//! core heeft zijn eigen executor en zijn eigen taken; tussen cores gaan
//! berichten door een ring.

#![cfg_attr(not(test), no_std)]
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

use alloc::boxed::Box;
use core::alloc::Layout;
use core::cell::{Cell, RefCell};
use core::fmt;
use core::future::Future;
use core::pin::Pin;
use core::sync::atomic::{
    AtomicBool, AtomicU64,
    Ordering::{AcqRel, Acquire, Relaxed, Release},
};
use core::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
use core::time::Duration;
use sync::mpsc::Mailbox;

/// Een taak: een `'static` future die niets teruggeeft. Gespawnd op de
/// executor van de core waar hij leeft, en daar blijft hij: taken verhuizen
/// nooit, dus `Send` is geen eis. De executor zelf woont in een
/// [`sync::Local`]; dat is de belofte die dit sluitend maakt.
pub type Task = Pin<Box<dyn Future<Output = ()> + 'static>>;

/// De klok: monotone nanoseconden sinds boot. Het board levert hem.
pub type Clock = fn() -> u64;

/// De slaap van het board: wat de executor doet als er niets te doen is.
///
/// `sleep` keert terug bij een wek (een `Signal::set`, een timer, een
/// interrupt) of zodra `until` bereikt is. De implementatie MOET, nadat zij
/// interrupts gemaskeerd heeft en vóór de WFE/WFI, `ready()` nog één keer
/// toetsen: dat is de deur die de verloren wek dichthoudt.
pub trait Sleeper {
    /// Slaap tot `until` (nanoseconden, `None` = tot een wek) of tot
    /// `ready()` waar is.
    fn sleep(&mut self, now: u64, until: Option<u64>, ready: &dyn Fn() -> bool);
}

/// Waarom een `spawn` niet lukte.
#[derive(Debug, PartialEq, Eq)]
pub enum SpawnError {
    /// De heap kon de taak niet plaatsen.
    OutOfMemory,
    /// De spawn-brievenbus zit vol: de executor heeft nog geen ronde gedraaid.
    Full,
}

impl fmt::Display for SpawnError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::OutOfMemory => f.write_str("out of memory placing the task"),
            Self::Full => f.write_str("spawn mailbox full"),
        }
    }
}

/// De meetlat van de executor (handboek §4): zonder deze getallen is "de
/// node slaapt" niet te onderscheiden van "de node spint".
#[derive(Default)]
pub struct Stats {
    /// Rondes met werk.
    pub rounds: AtomicU64,
    /// Gepolde taken, totaal.
    pub polls: AtomicU64,
    /// Keren geslapen.
    pub sleeps: AtomicU64,
    /// Taken die geen slot kregen (gedropt).
    pub dropped: AtomicU64,
    /// Timers die geen slot kregen (de wachter spint op ronde-korrel).
    pub timer_overflows: AtomicU64,
}

struct Slot {
    ready: AtomicBool,
    task: RefCell<Option<Task>>,
}

impl Slot {
    const fn new() -> Self {
        Self {
            ready: AtomicBool::new(false),
            task: RefCell::new(None),
        }
    }
}

static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, wake, wake, drop_waker);

unsafe fn clone(p: *const ()) -> RawWaker {
    RawWaker::new(p, &VTABLE)
}

unsafe fn wake(p: *const ()) {
    // SAFETY: `p` komt uit `Executor::waker` en wijst naar een `Slot` in een
    // executor die voor altijd leeft (`&'static self`).
    let slot = unsafe { &*p.cast::<Slot>() };
    slot.ready.store(true, Release);
    dev::notify();
}

unsafe fn drop_waker(_: *const ()) {}

/// Een executor met plaats voor `TASKS` taken en `TIMERS` lopende timers.
///
/// Leeft voor altijd: in de kern een `static` in een [`sync::Local`], in
/// een test een gelekte `Box`. Wakers wijzen naar zijn slots.
pub struct Executor<const TASKS: usize = 512, const TIMERS: usize = 256> {
    slots: [Slot; TASKS],
    spawn: Mailbox<Task, 64>,
    timers: RefCell<[Option<(u64, Waker)>; TIMERS]>,
    clock: Cell<Option<Clock>>,
    /// De meetlat.
    pub stats: Stats,
}

impl<const TASKS: usize, const TIMERS: usize> Default for Executor<TASKS, TIMERS> {
    fn default() -> Self {
        Self::new()
    }
}

impl<const TASKS: usize, const TIMERS: usize> Executor<TASKS, TIMERS> {
    /// Een lege executor zonder klok.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            slots: [const { Slot::new() }; TASKS],
            spawn: Mailbox::new(),
            timers: RefCell::new([const { None }; TIMERS]),
            clock: Cell::new(None),
            stats: Stats {
                rounds: AtomicU64::new(0),
                polls: AtomicU64::new(0),
                sleeps: AtomicU64::new(0),
                dropped: AtomicU64::new(0),
                timer_overflows: AtomicU64::new(0),
            },
        }
    }

    /// Zet de klok. Vóór de eerste `after`; het board doet dit bij boot.
    pub fn set_clock(&self, c: Clock) {
        self.clock.set(Some(c));
    }

    /// Nu, in nanoseconden; 0 zolang er geen klok is.
    #[must_use]
    pub fn now(&self) -> u64 {
        self.clock.get().map_or(0, |c| c())
    }

    /// Zet een taak in de rij; hij krijgt zijn slot in de volgende ronde.
    pub fn spawn(&self, f: impl Future<Output = ()> + 'static) -> Result<(), SpawnError> {
        let task: Task = try_box(f).ok_or(SpawnError::OutOfMemory)?;
        self.spawn.try_send(task).map_err(|_| SpawnError::Full)
    }

    /// Slaapt `d` lang op het timerwiel.
    pub fn after(&'static self, d: Duration) -> After<TASKS, TIMERS> {
        let ns = u64::try_from(d.as_nanos()).unwrap_or(u64::MAX);
        After {
            exec: self,
            deadline: self.now().saturating_add(ns),
            slot: None,
        }
    }

    /// Slaapt tot tijdstip `deadline` (nanoseconden op de klok).
    pub fn until(&'static self, deadline: u64) -> After<TASKS, TIMERS> {
        After {
            exec: self,
            deadline,
            slot: None,
        }
    }

    /// Ligt er werk: een gezet bit of een spawn in de rij?
    #[must_use]
    pub fn has_ready(&self) -> bool {
        !self.spawn.is_empty() || self.slots.iter().any(|s| s.ready.load(Acquire))
    }

    /// De vroegste timer-deadline, als er een timer loopt.
    #[must_use]
    pub fn next_deadline(&self) -> Option<u64> {
        self.timers
            .borrow()
            .iter()
            .filter_map(|e| e.as_ref().map(|(dl, _)| *dl))
            .min()
    }

    fn waker(slot: &'static Slot) -> Waker {
        let raw = RawWaker::new(core::ptr::from_ref(slot).cast::<()>(), &VTABLE);
        // SAFETY: de vtable hierboven houdt zich aan het `RawWaker`-contract:
        // clone en drop doen niets met eigendom, wake raakt alleen atomics.
        unsafe { Waker::from_raw(raw) }
    }

    fn drain_spawns(&'static self) -> bool {
        let mut worked = false;
        while let Some(task) = self.spawn.try_recv() {
            worked = true;
            let free = self.slots.iter().find(|s| s.task.borrow().is_none());
            match free {
                Some(slot) => {
                    *slot.task.borrow_mut() = Some(task);
                    slot.ready.store(true, Release);
                }
                None => {
                    self.stats.dropped.fetch_add(1, Relaxed);
                    drop(task);
                }
            }
        }
        worked
    }

    fn expire_timers(&self, now: u64) -> bool {
        let mut worked = false;
        let mut timers = self.timers.borrow_mut();
        for entry in timers.iter_mut() {
            if entry.as_ref().is_some_and(|(dl, _)| *dl <= now)
                && let Some((_, w)) = entry.take()
            {
                w.wake();
                worked = true;
            }
        }
        worked
    }

    /// Eén ronde: spawns plaatsen, verstreken timers wekken, gereed taken
    /// pollen. `true` als er iets gebeurd is.
    pub fn step(&'static self) -> bool {
        let mut worked = self.drain_spawns();
        worked |= self.expire_timers(self.now());
        for slot in &self.slots {
            if !slot.ready.swap(false, AcqRel) {
                continue;
            }
            // De taak komt uit zijn slot zolang hij gepolld wordt: zo houdt
            // niemand een lening op de tabel terwijl vreemde code draait.
            let Some(mut task) = slot.task.borrow_mut().take() else {
                continue;
            };
            let waker = Self::waker(slot);
            let mut cx = Context::from_waker(&waker);
            self.stats.polls.fetch_add(1, Relaxed);
            if task.as_mut().poll(&mut cx).is_pending() {
                *slot.task.borrow_mut() = Some(task);
            }
            worked = true;
        }
        if worked {
            self.stats.rounds.fetch_add(1, Relaxed);
        }
        worked
    }

    /// De hoofdlus: rondes draaien, en slapen als een ronde niets deed.
    pub fn run(&'static self, sleeper: &mut dyn Sleeper) -> ! {
        loop {
            if self.step() {
                continue;
            }
            self.stats.sleeps.fetch_add(1, Relaxed);
            sleeper.sleep(self.now(), self.next_deadline(), &|| self.has_ready());
        }
    }

    /// Hoeveel taken een slot hebben.
    #[must_use]
    pub fn live_tasks(&self) -> usize {
        self.slots
            .iter()
            .filter(|s| s.task.borrow().is_some())
            .count()
    }
}

/// De future van [`Executor::after`] en [`Executor::until`].
#[must_use = "een future doet niets tot hij gepolld wordt"]
pub struct After<const TASKS: usize, const TIMERS: usize> {
    exec: &'static Executor<TASKS, TIMERS>,
    deadline: u64,
    slot: Option<usize>,
}

impl<const TASKS: usize, const TIMERS: usize> Future for After<TASKS, TIMERS> {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let this = self.get_mut();
        if this.exec.now() >= this.deadline {
            return Poll::Ready(());
        }
        let mut timers = this.exec.timers.borrow_mut();
        if this.slot.is_none() {
            this.slot = timers.iter().position(Option::is_none);
        }
        match this.slot {
            Some(i) => timers[i] = Some((this.deadline, cx.waker().clone())),
            None => {
                // Geen timerslot: spin op ronde-korrel, en tel het.
                this.exec.stats.timer_overflows.fetch_add(1, Relaxed);
                cx.waker().wake_by_ref();
            }
        }
        Poll::Pending
    }
}

impl<const TASKS: usize, const TIMERS: usize> Drop for After<TASKS, TIMERS> {
    fn drop(&mut self) {
        if let Some(i) = self.slot.take() {
            self.exec.timers.borrow_mut()[i] = None;
        }
    }
}

/// `Box::pin` dat faalt in plaats van abort bij een volle heap.
fn try_box<F: Future<Output = ()> + 'static>(f: F) -> Option<Task> {
    let layout = Layout::new::<F>();
    if layout.size() == 0 {
        // Een lege future alloceert niets; `Box::new` kan hier niet falen.
        return Some(Box::pin(f));
    }
    // SAFETY: de layout heeft een positieve grootte.
    let p = unsafe { alloc::alloc::alloc(layout) }.cast::<F>();
    if p.is_null() {
        return None;
    }
    // SAFETY: `p` is vers gealloceerd voor precies een `F` en nog niet
    // geïnitialiseerd; na `write` bezit de `Box` hem met dezelfde layout.
    let boxed = unsafe {
        p.write(f);
        Box::from_raw(p)
    };
    Some(Box::into_pin(boxed) as Task)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::Ordering::SeqCst;
    use sync::Signal;

    static NOW: AtomicU64 = AtomicU64::new(0);
    fn fake_now() -> u64 {
        NOW.load(SeqCst)
    }

    fn exec() -> &'static Executor<8, 4> {
        let e: &'static Executor<8, 4> = Box::leak(Box::new(Executor::new()));
        e.set_clock(fake_now);
        e
    }

    #[test]
    fn task_waits_on_signal_and_finishes() {
        static BELL: Signal = Signal::new();
        static DONE: AtomicBool = AtomicBool::new(false);
        let e = exec();
        e.spawn(async {
            BELL.wait().await;
            DONE.store(true, SeqCst);
        })
        .unwrap();
        assert!(e.step()); // geplaatst en één keer gepolld: Pending
        assert!(!e.step()); // niets te doen
        assert_eq!(e.live_tasks(), 1);
        BELL.set();
        assert!(e.has_ready());
        assert!(e.step());
        assert!(DONE.load(SeqCst));
        assert_eq!(e.live_tasks(), 0);
    }

    #[test]
    fn timer_fires_when_the_clock_passes() {
        static FIRED: AtomicBool = AtomicBool::new(false);
        let e = exec();
        NOW.store(1_000, SeqCst);
        e.spawn(async move {
            e.after(Duration::from_nanos(500)).await;
            FIRED.store(true, SeqCst);
        })
        .unwrap();
        assert!(e.step());
        assert_eq!(e.next_deadline(), Some(1_500));
        NOW.store(1_400, SeqCst);
        assert!(!e.step());
        NOW.store(1_500, SeqCst);
        assert!(e.step()); // timer gewekt
        assert!(e.step() || FIRED.load(SeqCst)); // en de taak gepolld
        assert!(FIRED.load(SeqCst));
        assert_eq!(e.next_deadline(), None);
    }

    #[test]
    fn spawn_from_inside_a_task_and_ping_pong() {
        static A: Signal = Signal::new();
        static B: Signal = Signal::new();
        static ROUNDS: AtomicU64 = AtomicU64::new(0);
        let e = exec();
        e.spawn(async move {
            e.spawn(async {
                for _ in 0..3 {
                    A.wait().await;
                    B.set();
                }
            })
            .unwrap();
            for _ in 0..3 {
                A.set();
                B.wait().await;
                ROUNDS.fetch_add(1, SeqCst);
            }
        })
        .unwrap();
        let mut n = 0;
        while e.step() {
            n += 1;
            assert!(n < 100, "loopt niet uit");
        }
        assert_eq!(ROUNDS.load(SeqCst), 3);
        assert_eq!(e.live_tasks(), 0);
    }

    #[test]
    fn full_table_drops_and_counts() {
        let e = exec();
        for _ in 0..9 {
            e.spawn(core::future::pending()).unwrap();
        }
        e.step();
        assert_eq!(e.live_tasks(), 8);
        assert_eq!(e.stats.dropped.load(SeqCst), 1);
    }
}
