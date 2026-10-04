//! De weg van HOP naar het scherm: een stroom van dezelfde JSON-events die de
//! browser-KVM al post, één per regel, over één verbinding.
//!
//! DE APP BELT, HOP LUISTERT, en het adres reist mee in de fb-grant naast de
//! FB_*-velden (besluit Derek 06-08: "we kunnen hem ook meesturen met de
//! grant van de GUI, want dat is TOCH wat nodig is voor keyboard en muis").
//! Het glas, het toetsenbord en de muis zijn één zitplaats, dus ze worden in
//! één keer overgedragen.
//!
//! WAT ER OVERBLIJFT AAN WANTROUWEN: op accept controleert HOP dat de beller
//! het slot ís dat het glas vasthoudt ([`allowed`]). Sinds de switch-harding
//! draagt dat gewicht: een slot kan alleen zijn eigen MAC gebruiken en de
//! interne buren staan statisch, dus een opgezette TCP-verbinding kan niet
//! van een ander slot komen dan zijn bron-IP zegt.
//!
//! GEEN TWEEDE VOCABULAIRE: exact het object dat `/input` al aanneemt, één
//! per regel. De display ontleedt het met dezelfde code als een browserklik,
//! dus een echt toetsenbord blijft niet te onderscheiden van de KVM-pagina.
//!
//! WAT HIER NIET STAAT: de socket. De binary luistert op [`INPUT_PORT`] met
//! de node-stack, laat alleen een verbinding door waarvoor [`allowed`] waar
//! is, laat een nieuwe verbinding de oude verdringen (een display die
//! herstart moet het toetsenbord terugkrijgen zonder dat iemand de dode
//! verbinding hoeft op te ruimen), en schrijft elke regel met een deadline
//! van [`WRITE_DEADLINE_NS`] zodat een vastgelopen display de USB-pollus niet
//! meesleept. De taak van de binary:
//!
//! ```ignore
//! let (tx, rx) = INPUT.split()?;          // tx naar de Sink van de manager
//! let mut d = Deliverer::new(rx, fb_size);
//! loop {
//!     let lines = d.next(after(KEEPALIVE)).await;
//!     if let Some(c) = conn.as_mut() {
//!         for l in lines.iter() {
//!             if c.write(l).await.is_err() { conn = None; break; }
//!         }
//!     }
//! }
//! ```
//!
//! Zonder verbinding gaan de regels weg: er kijkt dan niemand, en invoer
//! bewaren voor later levert alleen een lawine bij het aansluiten. De cursor
//! loopt wel mee, zoals in Go.

use abi::glass::Input;
use abi::layout::{HOST_IP4, Ip4, Slot, slot_ip4};
use core::fmt::{self, Write as _};
use core::future::Future;
use driver_hid::{Event, Kind};
use sync::spsc::{Channel, Receiver, Sender};
use sync::{Either, select};

/// De poort op het interne gateway-adres (10.100.0.1) en de langste regel:
/// het contract met de display-app staat in `abi`.
pub use abi::glass::{INPUT_PORT, LINE_MAX};

/// Invoer is LOSSY BY DESIGN, dezelfde afspraak als de input-pomp in de
/// display zelf. Een display die even niet leest mag de USB-pollus niet
/// stilzetten, dus vol = weggooien.
pub const QUEUE_DEPTH: usize = 256;

/// De display detecteert met zijn leesdeadline een verloren stroom na een
/// FLIP. Lege regels houden een stil toetsenbord levend zonder
/// invoergebeurtenissen te maken.
pub const KEEPALIVE_NS: u64 = 5_000_000_000;

/// Hoe lang één regel schrijven mag duren voor de binary de verbinding
/// dichtdoet en op een nieuwe wacht.
pub const WRITE_DEADLINE_NS: u64 = 1_000_000_000;

/// De rij van de manager naar de deliverer: één producer (de taak die de
/// bus bezit), één consument.
pub type InputQueue = Channel<Event, QUEUE_DEPTH>;
/// De zendkant van de rij, voor de sink van de manager. De pollus zendt
/// met `try_send` en blokkeert nooit: vol is weggooien.
pub type InputTx<'a> = Sender<'a, Event, QUEUE_DEPTH>;
/// De ontvangkant van de rij, voor de deliverer.
pub type InputRx<'a> = Receiver<'a, Event, QUEUE_DEPTH>;

/// Het adres dat met de fb-grant meereist: `10.100.0.1:7879`. De switch
/// vertaalt het gateway-adres naar het interface-adres van de node-stack.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct InputAddr;

impl fmt::Display for InputAddr {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}:{INPUT_PORT}", Ip4(HOST_IP4))
    }
}

/// Vergelijkt de beller met het slot dat het glas vasthoudt. Het slot-IP is
/// een deterministische functie van het slotnummer
/// ([`abi::layout::slot_ip4`]), dus hier is geen tabel en geen netwerkvlak
/// voor nodig. Zonder houder komt er niemand binnen.
#[must_use]
pub fn allowed(remote_ip4: u32, holder: Option<Slot>) -> bool {
    holder.is_some_and(|s| slot_ip4(s) == remote_ip4)
}

/// De KVM-pagina stuurt ABSOLUTE coördinaten (een canvas kent geen deltas),
/// een USB-muis relatieve. Deze laag houdt dus de cursor bij en klemt hem op
/// de schermmaat: dezelfde plek waar de display hem tekent.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Cursor {
    x: i32,
    y: i32,
    w: i32,
    h: i32,
}

impl Cursor {
    fn step(&mut self, dx: i32, dy: i32) {
        self.x = clamp(self.x.saturating_add(dx), self.w - 1);
        self.y = clamp(self.y.saturating_add(dy), self.h - 1);
    }
}

/// Klemt op `[0, max]`; een `max` van nul of minder is "geen schermmaat
/// bekend" en klemt alleen onder.
fn clamp(v: i32, max: i32) -> i32 {
    if v < 0 {
        0
    } else if max > 0 && v > max {
        max
    } else {
        v
    }
}

/// Eén regel in een vaste buffer, met de afsluitende newline.
pub type Line = bounded::Text<LINE_MAX>;

/// Wat één gebeurtenis uit de rij oplevert: hoogstens twee regels (de
/// samengevoegde beweging plus de gebeurtenis die de samenvoeging afbrak).
#[derive(Clone, Copy, Debug, Default)]
pub struct Lines {
    lines: [Line; 2],
    n: usize,
}

impl Lines {
    fn push(&mut self) -> Option<&mut Line> {
        let l = self.lines.get_mut(self.n)?;
        *l = Line::new();
        self.n += 1;
        Some(l)
    }

    /// De regels, in volgorde.
    pub fn iter(&self) -> impl Iterator<Item = &[u8]> {
        self.lines.iter().take(self.n).map(Line::as_bytes)
    }

    /// Het aantal regels.
    #[must_use]
    pub fn len(&self) -> usize {
        self.n
    }

    /// Geen regels?
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.n == 0
    }
}

/// Waarom een schrijf naar de display mislukte.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ConnError {
    /// De verbinding is dicht.
    Closed,
    /// De schrijf haalde [`WRITE_DEADLINE_NS`] niet.
    Timeout,
}

impl fmt::Display for ConnError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Closed => f.write_str("usb: input stream closed"),
            Self::Timeout => write!(
                f,
                "usb: input stream write exceeded {} ms",
                WRITE_DEADLINE_NS / 1_000_000
            ),
        }
    }
}

/// De display-verbinding zoals de binary hem met de node-stack maakt.
pub trait InputConn {
    /// Schrijft één regel, binnen [`WRITE_DEADLINE_NS`].
    fn write(&mut self, line: &[u8]) -> impl Future<Output = Result<(), ConnError>>;

    /// Het IPv4 van de beller (big-endian als getal), voor [`allowed`].
    fn remote_ip4(&self) -> u32;
}

/// De invoerstroom: de ontvangkant van de rij plus de cursor.
pub struct Deliverer<'a> {
    rx: InputRx<'a>,
    cur: Cursor,
}

impl<'a> Deliverer<'a> {
    /// Een deliverer over `rx`. `fb` is de schermmaat (breedte, hoogte) als
    /// het board een framebuffer heeft; de cursor begint in het midden.
    #[must_use]
    pub fn new(rx: InputRx<'a>, fb: Option<(u32, u32)>) -> Self {
        let mut cur = Cursor::default();
        if let Some((w, h)) = fb {
            cur.w = i32::try_from(w).unwrap_or(i32::MAX);
            cur.h = i32::try_from(h).unwrap_or(i32::MAX);
            cur.x = cur.w / 2;
            cur.y = cur.h / 2;
        }
        Self { rx, cur }
    }

    /// De cursorpositie.
    #[must_use]
    pub fn cursor(&self) -> (i32, i32) {
        (self.cur.x, self.cur.y)
    }

    /// Wacht op de volgende gebeurtenis, of op `tick` (de keepalive van
    /// [`KEEPALIVE_NS`], een timer van de binary), en geeft de regels die
    /// eruit volgen.
    pub async fn next<T: Future<Output = ()>>(&mut self, tick: T) -> Lines {
        let mut out = Lines::default();
        match select(self.rx.recv(), tick).await {
            Either::Left(e) => self.handle(e, &mut out),
            Either::Right(()) => {
                if let Some(l) = out.push() {
                    let _ = l.write_str("\n");
                }
            }
        }
        out
    }

    /// Serveert één verbinding tot hij faalt; de binary wacht daarna op een
    /// nieuwe (en gooit tot dan de regels van [`Deliverer::next`] weg).
    pub async fn serve<C, T>(&mut self, conn: &mut C, mut tick: impl FnMut() -> T) -> ConnError
    where
        C: InputConn,
        T: Future<Output = ()>,
    {
        loop {
            let lines = self.next(tick()).await;
            for l in lines.iter() {
                if let Err(e) = conn.write(l).await {
                    return e;
                }
            }
        }
    }

    /// De regels voor één gebeurtenis uit de rij.
    ///
    /// Bewegingen worden samengevoegd: een muis meldt zich 125x per seconde
    /// en alleen de eindpositie is interessant. Zonder dit staat er per
    /// tussenstap een bericht in de weg van de volgende toetsaanslag.
    pub fn handle(&mut self, e: Event, out: &mut Lines) {
        if e.kind != Kind::MouseMove {
            if let Some(l) = out.push() {
                self.body(&e, l);
            }
            return;
        }
        self.cur.step(e.dx, e.dy);
        let next = self.drain_moves();
        if let Some(l) = out.push() {
            self.body(
                &Event {
                    kind: Kind::MouseMove,
                    ..Event::default()
                },
                l,
            );
        }
        if let Some(n) = next
            && let Some(l) = out.push()
        {
            self.body(&n, l);
        }
    }

    /// Telt alle direct wachtende bewegingen bij de cursor op en geeft de
    /// eerste gebeurtenis terug die géén beweging was: die mag niet
    /// verdwijnen, want een klik hoort bij de plek waar hij gebeurde.
    fn drain_moves(&mut self) -> Option<Event> {
        while let Some(e) = self.rx.try_recv() {
            if e.kind != Kind::MouseMove {
                return Some(e);
            }
            self.cur.step(e.dx, e.dy);
        }
        None
    }

    /// Het JSON-event dat `/input` verwacht (surfserve, inputMsg), in de
    /// vorm van het contract ([`Input`]) op de plek van de cursor.
    pub fn body(&self, e: &Event, out: &mut Line) {
        let (x, y) = (self.cur.x, self.cur.y);
        let input = match e.kind {
            Kind::KeyDown | Kind::KeyUp => Input::Key {
                code: e.code,
                down: e.kind == Kind::KeyDown,
            },
            Kind::MouseMove => Input::Move { x, y },
            Kind::MouseDown | Kind::MouseUp => Input::Button {
                code: e.code,
                down: e.kind == Kind::MouseDown,
                x,
                y,
            },
            Kind::MouseWheel => Input::Wheel { v: e.dy, x, y },
        };
        // Een regel past altijd in LINE_MAX (drie i32's plus de vaste
        // tekst); een fout hier laat hoogstens een afgekapte regel achter.
        let _ = writeln!(out, "{input}");
    }
}
