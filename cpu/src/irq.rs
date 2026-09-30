//! Het interrupt-contract van HopOS: een lijn, een controller, en één
//! werkwoord: [`wait`].
//!
//! Dit bezit: de tabel lijn-naar-wachter, één [`Signal`] per lijn, en de
//! dispatcher die claimt, het device ackt, het signaal zet en completeert.
//! Niet van hier: de controller zelf (driver/gicv3, straks de AIC) en de
//! vector (het boot-spoor), die alleen [`on_irq`] roept.
//!
//! Tot 02-09 had HopOS géén interrupt-afhandeling: DAIF gemaskeerd, alles
//! gepold, en HOP's RX-lus sliep 300 µs per ronde, ruim 3.000 wekmomenten
//! per seconde op een node die niets doet en 300 µs latency op élk
//! app-pakket. Het model sindsdien, en hier in Rust-vorm: de vector zet een
//! vlag en keert terug met I gemaskeerd; de dispatcher-taak ([`run`]) ziet
//! de vlag, claimt tot de controller niets meer heeft, en opent I weer. Geen
//! logica in exception-context (handboek §5): op de Ampere gaf runtime-code
//! in de vector een stille hang binnen seconden (19-09), op de M4 een
//! verloren heropening van het I-masker onder load (21-09).
//!
//! Twee regels, beide isolatie:
//!
//! - Interrupts zijn uitsluitend HOP-werk. Een app-core wordt nooit een
//!   target van de controller en houdt zijn maskers dicht; een app heeft
//!   geen lijn, geen controller en geen `wait`.
//! - Een verloren flank mag nooit een hang worden: [`wait`] heeft een
//!   maximum, en de wachter behandelt een time-out als "kijk toch maar"
//!   (liever pollen dan hangen).

use core::cell::{Cell, RefCell};
use core::fmt;
use core::future::Future;
use core::pin::Pin;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::task::{Context, Poll};
use sync::{Local, Signal};

/// Hoeveel lijnen er tegelijk een wachter kunnen hebben. HOP bedient een
/// handvol devices (NIC, UART, NVMe); zestien is ruim.
pub const MAX_LINES: usize = 16;

/// Hoe vaak één lijn binnen één dispatch-ronde mag terugkomen voordat de
/// ronde wordt afgebroken.
///
/// Een lijn die binnen één ronde blijft terugkomen is een level-bron die
/// niemand laat zakken, of een NIC onder last. Zonder grens spint de
/// dispatcher voor eeuwig in claim, EOI, claim met I gemaskeerd: de
/// O6N-"freeze bij de eerste wachttijd" (17-09/18-09), geen pets meer,
/// watchdog na 12 s. Ruim boven wat een NIC-burst legitiem doet (een ack
/// per claim laat de lijn zakken), ver onder "voor eeuwig".
pub const STRAY_LIMIT: u32 = 256;

/// Eén interruptlijn zoals de controller hem nummert (GICv3: INTID, SPI n =
/// 32+n; AIC: het hw-nummer; PLIC: de source-id).
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Line(pub u32);

/// Hoe een lijn afgaat: het board kiest, per lijn, bij [`enable_as`].
///
/// Waarom dit bij de registratie hoort en niet los bij de controller: een
/// MSI die via een brug een SPI wordt (de MIP van de Pi 5) is een flank,
/// een SPI staat in de GIC standaard op level, en een level-lijn achter
/// een puls blijft staan tot iemand hem laat zakken. Staat de soort in de
/// registratie, dan zet de dispatcher hem vóór de enable (de GIC wil dat
/// zo, IHI 0048B 4.3.13) en weet hij ook wat een lijn die blijft
/// terugkomen betekent: bij level zonder ack laat niemand hem ooit zakken
/// ([`Dispatcher::dispatch`]). Les van 30-09 (de eerste Pi 5-boot).
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub enum Trigger {
    /// Level: de lijn staat zolang het device hem vasthoudt. De
    /// standaard van een SPI en van de meeste devices.
    #[default]
    Level,
    /// Flank: elke puls is één interrupt (een MSI).
    Edge,
}

/// De device-kant van de bevestiging: wat het device nodig heeft om zijn
/// lijn weer los te laten (virtio: InterruptACK), gedaan vóór de wachter
/// gewekt wordt.
pub type Ack = &'static dyn Fn();

/// Wat een interrupt-controller moet kunnen. Vier werkwoorden en een
/// instelling, en niets over prioriteiten of groepen: die zijn van de
/// driver.
///
/// De methoden nemen `&self`: een controller is een registerblok, en
/// MMIO-registers zijn per definitie gedeeld (`dev::Reg`).
pub trait Controller {
    /// Zet de soort van de lijn, vóór [`Controller::enable`]. Een
    /// controller die dat niet per lijn kan (de AIC, de PLIC: hun lijnen
    /// hebben één vaste soort) laat de standaard staan: niets te doen.
    fn set_trigger(&self, l: Line, t: Trigger) -> Result<(), Error> {
        let _ = (l, t);
        Ok(())
    }
    /// Maakt de lijn scherp én routeert hem naar de aanroepende core.
    fn enable(&self, l: Line) -> Result<(), Error>;
    /// Zet de lijn uit.
    fn disable(&self, l: Line);
    /// De lijn die vuurde (en bevestigt hem bij de controller); `None` =
    /// niets (meer) te claimen.
    fn claim(&self) -> Option<Line>;
    /// Sluit de afhandeling van `l` af (EOI, waar dat los van claim is).
    fn complete(&self, l: Line);
}

/// Waarom een lijn niet scherp kon.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Error {
    /// Er is (nog) geen controller geregistreerd.
    NoController,
    /// Alle [`MAX_LINES`] plaatsen zijn bezet.
    Full {
        /// De lijn die geen plaats kreeg.
        line: u32,
    },
    /// De controller kent deze lijn niet of weigert hem.
    Rejected {
        /// De lijn.
        line: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::NoController => f.write_str("irq: no interrupt controller on this board"),
            Self::Full { line } => {
                write!(f, "irq: no room for INTID {line} (max {MAX_LINES} lines)")
            }
            Self::Rejected { line } => write!(f, "irq: controller rejected INTID {line}"),
        }
    }
}

/// De meetlat van de dispatcher.
#[derive(Default, Debug)]
pub struct Stats {
    /// Geclaimde interrupts, alle lijnen. Zonder dit getal is "de RX-lus
    /// wordt wakker" niet te onderscheiden van "de vangrail van `wait`
    /// liep af": dít zegt of het silicium sprak.
    pub fired: AtomicU64,
    /// Dispatch-rondes.
    pub passes: AtomicU64,
    /// Rondes afgebroken op [`STRAY_LIMIT`].
    pub stray_passes: AtomicU64,
    /// Onbekende lijnen die uitgezet zijn.
    pub unknown: AtomicU64,
    /// Level-lijnen zonder device-ack die uitgezet zijn omdat ze binnen één
    /// ronde bleven terugkomen.
    pub stuck: AtomicU64,
}

/// Wat één dispatch-ronde zag; de dispatcher-taak logt het.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub struct Pass {
    /// Aantal claims in deze ronde.
    pub claimed: u32,
    /// Een lijn zonder wachter die vuurde en is uitgezet (de laatste).
    pub disabled: Option<Line>,
    /// De lijn waarop de ronde werd afgebroken ([`STRAY_LIMIT`]).
    pub stray: Option<Line>,
    /// Een level-lijn zonder device-ack die op [`STRAY_LIMIT`] uitgezet is.
    pub stuck: Option<Line>,
}

#[derive(Copy, Clone)]
struct Entry {
    line: Line,
    ack: Option<Ack>,
    trigger: Trigger,
}

/// De dispatcher: de tabel lijn-naar-wachter en een signaal per lijn.
///
/// Eén per systeem, op HOP's core; [`global`] is die ene. Hij woont in een
/// [`Local`]: alleen taken op HOP's executor raken hem aan, nooit een ISR
/// (de vector raakt alleen [`IRQ_PENDING`], een kale atomic).
pub struct Dispatcher {
    ctrl: Cell<Option<&'static dyn Controller>>,
    lines: RefCell<[Option<Entry>; MAX_LINES]>,
    signals: [Signal; MAX_LINES],
    /// De meetlat.
    pub stats: Stats,
}

impl Default for Dispatcher {
    fn default() -> Self {
        Self::new()
    }
}

impl Dispatcher {
    /// Een dispatcher zonder controller en zonder lijnen.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            ctrl: Cell::new(None),
            lines: RefCell::new([None; MAX_LINES]),
            signals: [const { Signal::new() }; MAX_LINES],
            stats: Stats {
                fired: AtomicU64::new(0),
                passes: AtomicU64::new(0),
                stray_passes: AtomicU64::new(0),
                unknown: AtomicU64::new(0),
                stuck: AtomicU64::new(0),
            },
        }
    }

    /// Registreert de controller. Eén keer, vanuit de board-bedrading, ná
    /// de controller-init en vóór de eerste `enable`.
    pub fn use_controller(&self, c: &'static dyn Controller) {
        self.ctrl.set(Some(c));
    }

    /// Is er een controller?
    #[must_use]
    pub fn is_ready(&self) -> bool {
        self.ctrl.get().is_some()
    }

    /// Registreert `l` als level-wek-doel (met de device-ack) en maakt hem
    /// scherp bij de controller. Geeft het signaal van de lijn.
    pub fn enable(&'static self, l: Line, ack: Option<Ack>) -> Result<&'static Signal, Error> {
        self.enable_as(l, Trigger::Level, ack)
    }

    /// Registreert `l` als wek-doel van soort `trigger` (met de
    /// device-ack), zet de soort bij de controller en maakt hem dan pas
    /// scherp. Geeft het signaal van de lijn.
    pub fn enable_as(
        &'static self,
        l: Line,
        trigger: Trigger,
        ack: Option<Ack>,
    ) -> Result<&'static Signal, Error> {
        let ctrl = self.ctrl.get().ok_or(Error::NoController)?;
        let i = {
            let mut lines = self.lines.borrow_mut();
            let pos = lines
                .iter()
                .position(|e| e.is_some_and(|e| e.line == l))
                .or_else(|| lines.iter().position(Option::is_none))
                .ok_or(Error::Full { line: l.0 })?;
            if let Some(slot) = lines.get_mut(pos) {
                *slot = Some(Entry {
                    line: l,
                    ack,
                    trigger,
                });
            }
            pos
        };
        ctrl.set_trigger(l, trigger)?;
        ctrl.enable(l)?;
        self.signals.get(i).ok_or(Error::Full { line: l.0 })
    }

    /// Het signaal van een geregistreerde lijn.
    fn signal(&'static self, l: Line) -> Option<&'static Signal> {
        let i = self
            .lines
            .borrow()
            .iter()
            .position(|e| e.is_some_and(|e| e.line == l))?;
        self.signals.get(i)
    }

    fn entry(&self, l: Line) -> Option<(usize, Entry)> {
        self.lines
            .borrow()
            .iter()
            .enumerate()
            .find_map(|(i, e)| e.filter(|e| e.line == l).map(|e| (i, e)))
    }

    /// Eén ronde: alle gevuurde lijnen claimen, per lijn het device acken,
    /// de wachter wekken en de lijn completeren.
    ///
    /// Onbekend = meteen uit (een lijn die de firmware aan liet staan:
    /// UEFI-timer, UART, een watchdog-waarschuwing). Een level-lijn zonder
    /// device-ack die [`STRAY_LIMIT`] keer terugkomt, laat niemand ooit
    /// zakken: uit, en luid (`pass.stuck`); zijn wachter valt terug op het
    /// maximum van [`wait`], liever pollen dan een dispatcher die de
    /// rotatie van de OS-core voor eeuwig onderbreekt (de vrees van 30-09
    /// bij de eerste Pi 5-boot). Bekend met een ack, of een flank, maar
    /// blijvend = de ronde afbreken na [`STRAY_LIMIT`], en de lijn NIET
    /// uitzetten: een
    /// bediende lijn die blijft terugkomen is een NIC onder last (tg3: een
    /// status-update per frame houdt de lijn bij 25k frames/s praktisch
    /// continu hoog). Die voorgoed uitzetten was de M4-dood van 20-09: ná
    /// een pull 0 interrupts/s, elke frame op de failsafe van 10 ms, 118
    /// naar 45 MB/s, en de O6N kreeg de schuld (bundels 47-55). Afbreken
    /// laat de pomp en de wachter aan de beurt; de device-ack houdt de lijn
    /// intussen laag.
    pub fn dispatch(&self) -> Pass {
        let mut pass = Pass::default();
        let Some(ctrl) = self.ctrl.get() else {
            return pass;
        };
        self.stats.passes.fetch_add(1, Relaxed);
        let mut seen = [0u32; MAX_LINES];
        while let Some(l) = ctrl.claim() {
            pass.claimed = pass.claimed.saturating_add(1);
            self.stats.fired.fetch_add(1, Relaxed);
            let Some((i, e)) = self.entry(l) else {
                ctrl.disable(l);
                ctrl.complete(l);
                self.stats.unknown.fetch_add(1, Relaxed);
                pass.disabled = Some(l);
                continue;
            };
            if let Some(ack) = e.ack {
                ack();
            }
            // Al gewekt en nog niet opgehaald: één is genoeg (level).
            if let Some(s) = self.signals.get(i) {
                s.set();
            }
            ctrl.complete(l);
            let Some(n) = seen.get_mut(i) else { continue };
            *n += 1;
            if *n > STRAY_LIMIT {
                self.stats.stray_passes.fetch_add(1, Relaxed);
                pass.stray = Some(l);
                if e.trigger == Trigger::Level && e.ack.is_none() {
                    ctrl.disable(l);
                    self.stats.stuck.fetch_add(1, Relaxed);
                    pass.stuck = Some(l);
                }
                break;
            }
        }
        pass
    }
}

/// De vlag van de vector: gezet in exception-context, gewacht door [`run`].
///
/// Een kale `Signal` (atomic plus waker) buiten elke `Local`, want de ISR
/// raakt nooit een `Local` (handboek §1.1).
pub static IRQ_PENDING: Signal = Signal::new();

/// De ingang voor de IRQ-vector van HOP's core: zet de vlag en wekt de
/// dispatcher. Verder niets: geen claim, geen allocatie, geen logica.
///
/// De vector keert daarna terug met I gemaskeerd (SPSR_EL1.I gezet): de
/// lijn staat nog tot de dispatcher hem claimt, en met I open werd dat een
/// storm. [`run`] opent I weer na zijn ronde.
pub fn on_irq() {
    IRQ_PENDING.set();
}

static GLOBAL: Local<Dispatcher> = Local::new(Dispatcher::new());

/// De dispatcher van dit systeem.
#[must_use]
pub fn global() -> &'static Dispatcher {
    GLOBAL.get()
}

/// Registreert de controller bij [`global`].
pub fn use_controller(c: &'static dyn Controller) {
    global().use_controller(c);
}

/// Maakt level-lijn `l` scherp met optionele device-ack; geeft zijn
/// signaal.
pub fn enable(l: Line, ack: Option<Ack>) -> Result<&'static Signal, Error> {
    global().enable(l, ack)
}

/// Maakt lijn `l` van soort `trigger` scherp met optionele device-ack;
/// geeft zijn signaal ([`Dispatcher::enable_as`]).
pub fn enable_as(l: Line, trigger: Trigger, ack: Option<Ack>) -> Result<&'static Signal, Error> {
    global().enable_as(l, trigger, ack)
}

/// Wacht tot lijn `l` vuurde of tot `max` afloopt: `true` = gevuurd.
///
/// `max` is een timer-future van de executor (`EXEC.after(d)`), zodat deze
/// crate niet aan één executor-type vastzit. Een lijn die niet
/// geregistreerd is wacht gewoon `max`: dat is de poll-terugval, geen fout.
pub fn wait<T: Future<Output = ()>>(l: Line, max: T) -> Wait<T> {
    Wait {
        signal: global().signal(l),
        max,
    }
}

/// De future van [`wait`].
#[must_use = "een future doet niets tot hij gepolld wordt"]
pub struct Wait<T> {
    signal: Option<&'static Signal>,
    max: T,
}

impl<T: Future<Output = ()>> Future for Wait<T> {
    type Output = bool;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<bool> {
        // SAFETY: structurele pinning: `max` wordt nooit uit `self`
        // verplaatst en `Wait` heeft geen `Drop`-impl.
        let this = unsafe { self.get_unchecked_mut() };
        if let Some(s) = this.signal
            && Pin::new(&mut s.wait()).poll(cx).is_ready()
        {
            return Poll::Ready(true);
        }
        // SAFETY: zie hierboven.
        let max = unsafe { Pin::new_unchecked(&mut this.max) };
        max.poll(cx).map(|()| false)
    }
}

/// De dispatcher-taak van HOP's core: wacht op de vector, draait een ronde,
/// meldt wat opviel en opent I weer. Spawnen na [`use_controller`].
///
/// `log` krijgt één Engelse regel per bijzonderheid: de eerste drie claims
/// en de eerste vijf rondes als bewijs dat het pad leeft, elke uitgezette
/// onbekende lijn, en de eerste drie afgebroken rondes.
pub async fn run(log: fn(fmt::Arguments<'_>)) {
    let d = global();
    loop {
        IRQ_PENDING.wait().await;
        let before = d.stats.fired.load(Relaxed);
        let pass = d.dispatch();
        report(d, before, pass, log);
        arch::unmask_irq();
    }
}

fn report(d: &Dispatcher, before: u64, pass: Pass, log: fn(fmt::Arguments<'_>)) {
    if before < 3 && pass.claimed > 0 {
        log(format_args!(
            "irq: first claims arrived ({} this pass)",
            pass.claimed
        ));
    }
    let n = d.stats.passes.load(Relaxed);
    if n <= 5 {
        log(format_args!("irq: isr pass #{n} done"));
    }
    if let Some(l) = pass.disabled {
        log(format_args!(
            "irq: INTID {} fired but nobody serves it (left enabled by the firmware?), line disabled",
            l.0
        ));
    }
    if let Some(l) = pass.stuck {
        log(format_args!(
            "irq: INTID {} came back {STRAY_LIMIT} times in one pass, a level line without a device ack that nobody lowers: line disabled, its waiter polls HOPOS_IRQ_STUCK",
            l.0
        ));
        return;
    }
    if let Some(l) = pass.stray
        && d.stats.stray_passes.load(Relaxed) <= 3
    {
        log(format_args!(
            "irq: INTID {} came back {STRAY_LIMIT} times in one pass, pass ended, line stays enabled",
            l.0
        ));
    }
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    /// Opent I (DAIFClr #2); F blijft zoals hij stond. Op Apple is F de
    /// weg van de timer-FIQ die WFI wekt zonder ooit genomen te worden.
    pub(super) fn unmask_irq() {
        // SAFETY: raakt alleen het I-masker van deze core. Geen `nomem`:
        // geheugentoegang mag niet over het openen heen schuiven.
        unsafe { core::arch::asm!("msr daifclr, #2", options(nostack)) };
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-stub: er is geen masker.
    pub(super) fn unmask_irq() {}
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU32, Ordering::SeqCst};
    use std::vec::Vec;

    /// Een controller met een rij pending lijnen en een logboek.
    #[derive(Default)]
    struct Fake {
        pending: RefCell<Vec<u32>>,
        log: RefCell<Vec<(char, u32)>>,
        refuse: Option<u32>,
    }

    impl Controller for Fake {
        fn set_trigger(&self, l: Line, t: Trigger) -> Result<(), Error> {
            if t == Trigger::Edge {
                self.log.borrow_mut().push(('f', l.0));
            }
            Ok(())
        }
        fn enable(&self, l: Line) -> Result<(), Error> {
            if self.refuse == Some(l.0) {
                return Err(Error::Rejected { line: l.0 });
            }
            self.log.borrow_mut().push(('e', l.0));
            Ok(())
        }
        fn disable(&self, l: Line) {
            self.log.borrow_mut().push(('d', l.0));
        }
        fn claim(&self) -> Option<Line> {
            let mut p = self.pending.borrow_mut();
            if p.is_empty() {
                None
            } else {
                Some(Line(p.remove(0)))
            }
        }
        fn complete(&self, l: Line) {
            self.log.borrow_mut().push(('c', l.0));
        }
    }

    fn setup(fake: Fake) -> (&'static Dispatcher, &'static Fake) {
        let d: &'static Dispatcher = Box::leak(Box::new(Dispatcher::new()));
        let f: &'static Fake = Box::leak(Box::new(fake));
        d.use_controller(f);
        (d, f)
    }

    #[test]
    fn enable_without_controller_fails() {
        let d: &'static Dispatcher = Box::leak(Box::new(Dispatcher::new()));
        assert_eq!(d.enable(Line(33), None).err(), Some(Error::NoController));
    }

    #[test]
    fn known_line_acks_signals_and_completes() {
        static ACKS: AtomicU32 = AtomicU32::new(0);
        static ACK: fn() = || {
            ACKS.fetch_add(1, SeqCst);
        };
        let (d, f) = setup(Fake::default());
        let s = d.enable(Line(79), Some(&ACK)).unwrap();
        assert!(!s.is_set());
        f.pending.borrow_mut().extend([79, 79]);
        let pass = d.dispatch();
        assert_eq!(pass.claimed, 2);
        assert_eq!(pass.disabled, None);
        assert_eq!(ACKS.load(SeqCst), 2);
        assert!(s.take()); // twee vuren, één wek: level, samengevoegd
        assert_eq!(*f.log.borrow(), [('e', 79), ('c', 79), ('c', 79)]);
        assert_eq!(d.stats.fired.load(SeqCst), 2);
    }

    #[test]
    fn unknown_line_is_disabled_once() {
        let (d, f) = setup(Fake::default());
        f.pending.borrow_mut().push(27);
        let pass = d.dispatch();
        assert_eq!(pass.disabled, Some(Line(27)));
        assert_eq!(*f.log.borrow(), [('d', 27), ('c', 27)]);
        assert_eq!(d.stats.unknown.load(SeqCst), 1);
    }

    #[test]
    fn stray_line_ends_the_pass_but_stays_enabled() {
        static ACK: fn() = || {};
        let (d, f) = setup(Fake::default());
        // Een NIC onder last: bediend (een ack per claim), dus nooit uit.
        let s = d.enable(Line(40), Some(&ACK)).unwrap();
        f.pending
            .borrow_mut()
            .extend(core::iter::repeat_n(40, STRAY_LIMIT as usize + 10));
        let pass = d.dispatch();
        assert_eq!(pass.stray, Some(Line(40)));
        assert_eq!(pass.claimed, STRAY_LIMIT + 1);
        assert!(s.is_set());
        assert!(!f.log.borrow().iter().any(|&(op, _)| op == 'd'));
        // De rest blijft pending voor de volgende ronde.
        assert_eq!(f.pending.borrow().len(), 9);
    }

    // Een level-lijn die niemand laat zakken (geen device-ack) komt binnen
    // één ronde STRAY_LIMIT keer terug: uit, en de ronde meldt hem. Een
    // flank zonder ack die even hard komt, is verkeer en blijft aan; de
    // soort gaat vóór de enable naar de controller.
    #[test]
    fn stuck_level_line_without_ack_is_disabled_an_edge_is_not() {
        let (d, f) = setup(Fake::default());
        let s = d.enable(Line(41), None).unwrap();
        f.pending
            .borrow_mut()
            .extend(core::iter::repeat_n(41, STRAY_LIMIT as usize + 10));
        let pass = d.dispatch();
        assert_eq!(pass.stuck, Some(Line(41)));
        assert_eq!(pass.stray, Some(Line(41)));
        assert!(s.is_set());
        assert!(f.log.borrow().contains(&('d', 41)));
        assert_eq!(d.stats.stuck.load(SeqCst), 1);

        let (d, f) = setup(Fake::default());
        d.enable_as(Line(166), Trigger::Edge, None).unwrap();
        assert_eq!(f.log.borrow()[..2], [('f', 166), ('e', 166)]);
        f.pending
            .borrow_mut()
            .extend(core::iter::repeat_n(166, STRAY_LIMIT as usize + 10));
        let pass = d.dispatch();
        assert_eq!(pass.stray, Some(Line(166)));
        assert_eq!(pass.stuck, None);
        assert!(!f.log.borrow().iter().any(|&(op, _)| op == 'd'));
    }

    #[test]
    fn refused_line_and_full_table() {
        let (d, _) = setup(Fake {
            refuse: Some(5),
            ..Fake::default()
        });
        assert_eq!(
            d.enable(Line(5), None).err(),
            Some(Error::Rejected { line: 5 })
        );
        let (d, _) = setup(Fake::default());
        for i in 0..MAX_LINES as u32 {
            d.enable(Line(100 + i), None).unwrap();
        }
        assert_eq!(d.enable(Line(7), None).err(), Some(Error::Full { line: 7 }));
        // Opnieuw een bestaande lijn is een her-registratie, geen nieuwe plaats.
        assert!(d.enable(Line(100), None).is_ok());
    }

    #[test]
    fn wait_resolves_on_signal_or_timeout() {
        use std::sync::Arc;
        use std::task::Wake;
        struct Nop;
        impl Wake for Nop {
            fn wake(self: Arc<Self>) {}
        }
        let w = std::task::Waker::from(Arc::new(Nop));
        let mut cx = Context::from_waker(&w);
        let (d, f) = setup(Fake::default());
        let s = d.enable(Line(9), None).unwrap();
        let mut fut = core::pin::pin!(Wait {
            signal: d.signal(Line(9)),
            max: core::future::pending::<()>(),
        });
        assert_eq!(fut.as_mut().poll(&mut cx), Poll::Pending);
        f.pending.borrow_mut().push(9);
        d.dispatch();
        assert_eq!(fut.as_mut().poll(&mut cx), Poll::Ready(true));
        assert!(!s.is_set());
        // Onbekende lijn: alleen het maximum telt.
        let mut t = core::pin::pin!(Wait {
            signal: d.signal(Line(10)),
            max: core::future::ready(()),
        });
        assert_eq!(t.as_mut().poll(&mut cx), Poll::Ready(false));
    }
}
