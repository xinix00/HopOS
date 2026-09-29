//! De idle van de app-core: [`AppSleeper`], de [`executor::Sleeper`] van een
//! app.
//!
//! De Go-governor (`cpu/idle`) bestuurde tamago's scheduler van buitenaf,
//! met deuren, `nested`-vlaggen en een sysmon-lite. Hier is de executor van
//! ons, en blijft er één vraag over: als geen taak iets te doen heeft, hoe
//! slaapt deze core? Drie antwoorden, in deze volgorde:
//!
//! 1. **De deurbel.** Heeft de app een RX-ring (zie [`crate::net`]), dan
//!    wapent de slaper vlak vóór de slaap `CtrlRXDoor` met "gezien tot
//!    head H" (H | bit 63), en kijkt hij daarna nog één keer: een frame dat
//!    net vóór het wapenen kwam valt anders tussen wal en schip. Ligt er
//!    iets, dan belt hij de pomp en slaapt hij niet. Na de slaap ontwapent
//!    hij, zodat de kern alleen een slapende pomp kickt: één kick per burst,
//!    niet per frame (een kick per frame kostte HOP naar app 3,5×, 534 naar
//!    150 MB/s, gemeten 04-09). Een app zonder RX-ring wapent nooit; anders
//!    maakte elke ARP-flood hem permanent "due" en at de resume/yield-
//!    pingpong de gedeelde core op.
//! 2. **De yield.** Deelt dit slot zijn core (`CtrlShared`), of vraagt het
//!    board om yield-idle (`CtrlIdleMode`, Apple silicon: op de M4 slaapt een
//!    app-core op EL1 niet, gemeten 02-09), dan HVC #1 met de wektijd.
//! 3. **WFE** op de event-stream, tot er echt geslapen is (zie
//!    [`AppSleeper::wfe_sleep`]).
//!
//! De verloren-wek-race: een app-core draait met de interrupts permanent
//! gemaskeerd (hij heeft geen vectoren; de deurbel-als-vFIQ is niet
//! geport), en zijn wekken zijn SEV's en de event-stream. Een SEV die na de
//! laatste `ready()`-toets valt, zet het event-register, en de WFE daarna
//! keert meteen terug. Dat is de deur die de executor eist.

use crate::arch;
use crate::clock;
use crate::contract::RX_ARMED;
use crate::ctrl::Ctrl;
use crate::ring::Peek;
use core::cell::Cell;
use executor::Sleeper;
use sync::{Local, Signal};

/// De instructies van de slaap, achter een trait zodat de logica op de host
/// test met een nep-core.
pub trait Idle {
    /// De vrijlopende teller.
    fn counter(&self) -> u64;
    /// Tikken per seconde.
    fn counter_hz(&self) -> u64;
    /// Eén WFE; de tikken die hij duurde.
    fn wfe(&mut self) -> u64;
    /// De yield naar EL2 met wektijd `deadline` (tellerstand, 0 = nu); de
    /// tikken tot we terug waren.
    fn hvc_yield(&mut self, deadline: u64) -> u64;
}

/// De echte core.
#[derive(Debug, Default)]
pub struct Hw;

impl Idle for Hw {
    fn counter(&self) -> u64 {
        arch::counter()
    }
    fn counter_hz(&self) -> u64 {
        arch::counter_hz()
    }
    fn wfe(&mut self) -> u64 {
        arch::wfe()
    }
    fn hvc_yield(&mut self, deadline: u64) -> u64 {
        arch::hvc_yield(deadline)
    }
}

/// De deurbel van de RX-ring: de leesblik op zijn kop en de bel van de pomp.
#[derive(Copy, Clone)]
pub struct RxDoor {
    /// De kop van de RX-ring.
    pub peek: Peek,
    /// De bel waarop de RX-pomp wacht.
    pub bell: &'static Signal,
}

/// De deurbel, als de app er een heeft. Gezet door [`watch_rx`] vanuit de
/// app-executor; gelezen door de slaper op dezelfde executor.
static RX_DOOR: DoorSlot = Local::new(Cell::new(None));

/// Waar een slaper zijn deurbel zoekt: de static van de app, of in een test
/// een eigen exemplaar.
pub type DoorSlot = Local<Cell<Option<RxDoor>>>;

/// Hangt de deurbel aan: vanaf nu wapent de idle `CtrlRXDoor` en belt hij
/// `door.bell` zodra er RX ligt. Eén keer, door wie de RX-ring leest.
pub fn watch_rx(door: RxDoor) {
    RX_DOOR.get().set(Some(door));
}

/// De grens tussen "de WFE slikte alleen een verschaald event" en "de core
/// heeft echt geslapen", in tikken. Het is een TIJD, ~2 µs: de vaste 64
/// tikken van vroeger waren op de M4 (1 GHz) 64 ns, minder dan de WFE zelf,
/// en dan spinde de core (3,6M wakes/s, gemeten 29-08). 64 blijft de bodem.
#[must_use]
pub fn wfe_min_sleep(hz: u64) -> u64 {
    (hz / 500_000).max(64)
}

/// De slaap van een app-core.
pub struct AppSleeper<I: Idle = Hw> {
    ctrl: Ctrl,
    idle: I,
    door: &'static DoorSlot,
    min_sleep: u64,
    idle_ticks: u64,
    wakes: u64,
}

impl AppSleeper<Hw> {
    /// De slaper van deze core, op de control-page `ctrl`.
    #[must_use]
    pub fn new(ctrl: Ctrl) -> Self {
        Self::with(ctrl, Hw)
    }
}

impl<I: Idle> AppSleeper<I> {
    /// Een slaper met core `idle` (een nep-core in de tests).
    pub fn with(ctrl: Ctrl, idle: I) -> Self {
        let min_sleep = wfe_min_sleep(idle.counter_hz());
        Self {
            ctrl,
            idle,
            door: &RX_DOOR,
            min_sleep,
            idle_ticks: 0,
            wakes: 0,
        }
    }

    /// Zoekt de deurbel in `slot` in plaats van in de static van de app.
    #[must_use]
    pub fn with_door(mut self, slot: &'static DoorSlot) -> Self {
        self.door = slot;
        self
    }

    /// De core, voor de tests.
    pub fn idle(&self) -> &I {
        &self.idle
    }

    /// Geslapen tikken tot nu (de `CtrlIdle`-teller).
    #[must_use]
    pub fn idle_ticks(&self) -> u64 {
        self.idle_ticks
    }

    /// Wapent de deurbel. `false`: er ligt al RX, de pomp is gebeld en er
    /// wordt niet geslapen.
    fn arm(&self, d: RxDoor) -> bool {
        let (head, pending) = d.peek.head_pending();
        if pending {
            d.bell.set();
            return false;
        }
        self.ctrl.set_rx_door(head | RX_ARMED);
        // De hercontrole: zonder haar valt een frame dat net vóór het
        // wapenen kwam tussen wal en schip.
        if d.peek.head_pending().1 {
            self.ctrl.set_rx_door(0);
            d.bell.set();
            return false;
        }
        true
    }

    /// Ontwapent de deurbel na de slaap en belt als er iets ligt.
    fn disarm(&self, d: RxDoor) {
        self.ctrl.set_rx_door(0);
        if d.peek.head_pending().1 {
            d.bell.set();
        }
    }

    /// WFE's tot er echt geslapen is. De lus is nodig omdat het
    /// event-register vrijwel altijd vol zit als we hier komen: elke
    /// exclusive (onze eigen atomics) zet op de N1 een wek-event, en de
    /// eerste WFE keert dan meteen terug (gemeten 18-07 op de Altra: 4,7M
    /// wakes/s, slaap 0,0 µs). Maar een snelle terugkeer kan ook de échte bel
    /// zijn (een SEV van de kern), en die slikten we tot 04-09 weg: 6% van
    /// de system calls kostte 1 ms in plaats van 20 µs. Dus na elke WFE
    /// kijken of er werk of een verstreken deadline is.
    pub fn wfe_sleep(&mut self, deadline: u64, ready: &dyn Fn() -> bool) -> u64 {
        let mut slept: u64 = 0;
        for _ in 0..4 {
            slept = slept.saturating_add(self.idle.wfe());
            if slept >= self.min_sleep || ready() || self.idle.counter() >= deadline {
                break;
            }
        }
        slept
    }

    /// De slaap zelf, zonder deurbel: yield of WFE.
    fn nap(&mut self, now: u64, until: Option<u64>, ready: &dyn Fn() -> bool) -> u64 {
        // De laatste toets. Een wek hierna is een SEV en die laat de WFE
        // meteen terugkeren.
        if ready() {
            return 0;
        }
        let deadline = clock::wake_at(now, until, self.idle.counter(), self.idle.counter_hz());
        if self.ctrl.is_shared() || self.ctrl.is_yield_mode() {
            // Eén yield per idle-ronde: de switcher doet zelf de slaap en de
            // rotatie, en de wektijd houdt twee wachtende buren uit een
            // pingpong.
            return self.idle.hvc_yield(deadline);
        }
        if deadline == 0 {
            return 0; // de deadline is al voorbij
        }
        self.wfe_sleep(deadline, ready)
    }
}

impl<I: Idle> Sleeper for AppSleeper<I> {
    fn sleep(&mut self, now: u64, until: Option<u64>, ready: &dyn Fn() -> bool) {
        self.wakes = self.wakes.wrapping_add(1);
        let door = self.door.get().get();
        if let Some(d) = door
            && !self.arm(d)
        {
            self.ctrl.publish_idle(self.idle_ticks, self.wakes);
            return;
        }
        let slept = self.nap(now, until, ready);
        self.idle_ticks = self.idle_ticks.wrapping_add(slept);
        self.ctrl.publish_idle(self.idle_ticks, self.wakes);
        if let Some(d) = door {
            self.disarm(d);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract::{
        CTRL_IDLE, CTRL_IDLE_MODE, CTRL_RX_DOOR, CTRL_SHARED, CTRL_WAKES, IDLE_YIELD,
    };
    use crate::ctrl::tests::Page;
    use crate::ring::tests::Backing;
    use crate::ring::{Kind, Writer};

    /// Een nep-core: telt de instructies en laat de teller lopen.
    #[derive(Default)]
    struct Fake {
        now: Cell<u64>,
        wfes: u32,
        yields: Vec<u64>,
        per_wfe: u64,
    }

    impl Idle for Fake {
        fn counter(&self) -> u64 {
            self.now.get()
        }
        fn counter_hz(&self) -> u64 {
            1_000_000_000
        }
        fn wfe(&mut self) -> u64 {
            self.wfes += 1;
            self.now.set(self.now.get() + self.per_wfe);
            self.per_wfe
        }
        fn hvc_yield(&mut self, deadline: u64) -> u64 {
            self.yields.push(deadline);
            500
        }
    }

    fn fake(per_wfe: u64) -> Fake {
        Fake {
            per_wfe,
            ..Fake::default()
        }
    }

    #[test]
    fn ready_work_means_no_sleep() {
        let p = Page::new();
        let mut s = AppSleeper::with(p.ctrl(), fake(10_000));
        s.sleep(0, Some(1_000_000), &|| true);
        assert_eq!(s.idle().wfes, 0);
        assert_eq!(p.word(CTRL_WAKES), 1);
    }

    #[test]
    fn wfe_loop_stops_once_it_really_slept() {
        let p = Page::new();
        // 1 GHz: de grens is 2.000 tikken. Snelle terugkeren van 100 tikken
        // (verschaalde events) tellen door tot vier pogingen.
        let mut s = AppSleeper::with(p.ctrl(), fake(100));
        s.sleep(0, Some(1_000_000), &|| false);
        assert_eq!(s.idle().wfes, 4);
        let mut s = AppSleeper::with(p.ctrl(), fake(5_000));
        s.sleep(0, Some(1_000_000), &|| false);
        assert_eq!(s.idle().wfes, 1);
        assert_eq!(s.idle_ticks(), 5_000);
        assert_eq!(p.word(CTRL_IDLE), 5_000);
    }

    #[test]
    fn shared_core_yields_with_the_deadline() {
        let mut p = Page::new();
        p.put(CTRL_SHARED, 1);
        let mut s = AppSleeper::with(p.ctrl(), fake(0));
        s.idle.now.set(1_000);
        s.sleep(0, Some(2_000_000), &|| false);
        assert_eq!(s.idle().yields, [2_001_000]);
        assert_eq!(s.idle().wfes, 0);
        // Yield-modus van het board: dezelfde weg, ook zonder buurman.
        let mut p = Page::new();
        p.put(CTRL_IDLE_MODE, IDLE_YIELD);
        let mut s = AppSleeper::with(p.ctrl(), fake(0));
        s.sleep(0, None, &|| false);
        assert_eq!(s.idle().yields.len(), 1);
    }

    #[test]
    fn doorbell_arms_before_and_disarms_after_the_sleep() {
        static BELL: Signal = Signal::new();
        static DOOR: DoorSlot = Local::new(Cell::new(None));
        let b = Backing::new(4096);
        let mut switch = Writer::open(b.pa(), 4096).unwrap();
        DOOR.get().set(Some(RxDoor {
            peek: Peek::new(b.pa(), 4096),
            bell: &BELL,
        }));

        // Leeg: gewapend met head 0 tijdens de slaap, ontwapend erna.
        let p = Page::new();
        struct Watch<'a>(&'a Page, Cell<u64>);
        let w = Watch(&p, Cell::new(0));
        let mut s = AppSleeper::with(p.ctrl(), fake(5_000)).with_door(&DOOR);
        s.sleep(0, Some(1_000_000), &|| {
            w.1.set(w.0.word(CTRL_RX_DOOR));
            false
        });
        assert_eq!(w.1.get(), RX_ARMED); // gewapend op head 0
        assert_eq!(p.word(CTRL_RX_DOOR), 0);
        assert!(!BELL.take());

        // Er ligt een frame: bellen en niet slapen.
        switch.write(Kind::FRAME, &[1; 60]).unwrap();
        let mut s = AppSleeper::with(p.ctrl(), fake(5_000)).with_door(&DOOR);
        s.sleep(0, Some(1_000_000), &|| false);
        assert!(BELL.take());
        assert_eq!(s.idle().wfes, 0);
        assert_eq!(p.word(CTRL_RX_DOOR), 0);
    }
}
