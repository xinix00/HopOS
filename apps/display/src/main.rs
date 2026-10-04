//! display: de display-app van HopOS v3. Houdt het glas van de node (de
//! framebuffer-grant van de kern), tekent een achtergrond, een klok en de
//! laatste invoer, en leest toetsenbord en muis van de kern.
//!
//! De Rust-vorm van het display-deel van Go's `cmd/display` (hop-os-surf),
//! teruggebracht tot wat het glas en de invoer bewijzen: geen compositor,
//! geen `/screen.png`, geen `/kvm`. Wat hij van de kern krijgt, staat in
//! zijn env (docs/gui.md, "De grant"): `FB_*` voor het glas, `INPUT_ADDR`
//! voor de invoerstroom als het board werkende USB heeft. De jobspec vraagt
//! het glas met `"env":{"GUI":"display"}`.
//!
//! Twee taken, elk met één eigenaar (handboek §1):
//!
//! - de **tekentaak** (de app zelf) bezit het glas: de [`paint::Painter`],
//!   de cursor, de tellers en de regels. Ze wacht op een gebeurtenis of op
//!   de volgende seconde van de klok, nooit op allebei.
//! - de **invoertaak** bezit de verbinding met `INPUT_ADDR` en de
//!   [`LineReader`]; wat ze ontleedt, gaat als waarde door een SPSC-rij
//!   naar de tekentaak. Vol is weggooien en tellen: de invoer is lossy by
//!   design, net als in de kern. Een verbroken stroom (een kern-flip, een
//!   herstart van HOP) is opnieuw bellen, met een oplopende pauze.
//!
//! Markers: `HOPOS_DISPLAY_UP w=<b> h=<h>` als het glas getekend is,
//! `HOPOS_DISPLAY_CONN` als de invoerstroom staat, en
//! `HOPOS_DISPLAY_INPUT keys=<n> moves=<n>` als de tellers veranderen
//! (hooguit vijf keer per seconde).
//!
//! De app blijft leven: een display die stopt, geeft het glas terug aan de
//! console van de kern. Alleen met `DISPLAY_QUIT=<code>` in de env stopt
//! hij netjes op die toets (`HOPOS_DISPLAY_QUIT`, exit 0); dat is voor de
//! QEMU-poort, die zo de hele levensloop van de grant toetst (terug aan de
//! console, een tweede houder, een nieuwe verbinding). Een jobspec zet hem
//! niet.

#![cfg_attr(target_os = "none", no_std, no_main)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

mod paint;

use applib::appnet::TcpStream;
use applib::fb::{self, Glass, Input, LINE_CAP, LineReader};
use applib::rt::Exec;
use applib::{App, EXEC, clock, log};
use core::fmt::Write as _;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use paint::{CURSOR, Cursor, Painter};
use sync::spsc::{Channel, Receiver, Sender};
use sync::{Either, select};

applib::main!(display);

/// Op de host bestaat dit image niet: daar is dit een lege binary, zodat de
/// host-poort het tekenwerk kan toetsen en clippy de rest kan lezen.
#[cfg(not(target_os = "none"))]
fn main() {}

/// De achtergrond: donkergroen, zodat een screendump de app onderscheidt
/// van de console van de kern (die is 0x101828).
const BG: u32 = 0x001E_3A2F;
/// De tekst.
const FG: u32 = 0x00F0_E68C;
/// De titel en de klok.
const ACCENT: u32 = 0x007F_DBCA;
/// De cursor en zijn rand.
const CURSOR_RGB: u32 = 0x00FF_5030;
const CURSOR_EDGE: u32 = 0x0000_0000;

/// Hoeveel invoerregels de tekentaak laat zien.
const EVENT_LINES: usize = 10;
/// De breedte van zo'n regel in tekens.
const LINE_CHARS: usize = 44;

/// De rij van de invoertaak naar de tekentaak. Een muis meldt zich 125x
/// per seconde, maar de kern voegt bewegingen al samen; 64 dekt een
/// tekenronde ruim.
const QUEUE: usize = 64;
static EVENTS: Channel<Input, QUEUE> = Channel::new();

/// Gebeurtenissen die de rij niet meer in pasten.
static DROPS: AtomicU64 = AtomicU64::new(0);

/// Hoe lang een verbinding opzetten mag duren.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(2);
/// Stilte op de stroom die "weg" betekent: drie keepalives van de kern
/// (elke 5 s) gemist. Na een kern-flip is de oude verbinding dood zonder
/// dat er een FIN komt; zo bellen we opnieuw.
const READ_TIMEOUT: Duration = Duration::from_secs(15);
/// De pauze na een mislukte verbinding: oplopend van de eerste tot de
/// laatste.
const RETRY_FIRST: Duration = Duration::from_millis(250);
const RETRY_MAX: Duration = Duration::from_secs(5);
/// Mislukte verbindingen die een eigen regel krijgen; daarna tellen.
const LOUD: u64 = 3;
/// Hoe vaak de tellers hooguit een logregel krijgen.
const LOG_EVERY: Duration = Duration::from_millis(200);

#[expect(
    clippy::expect_used,
    reason = "de start van de bin: zonder rij voor de invoer is er niets te lezen, en een luide paniek met reden is het goede einde"
)]
async fn display(app: &'static App) {
    let exec: &'static Exec = EXEC.get();
    let glass = match Glass::from_env(|k| app.env(k)) {
        Ok(g) => g,
        Err(e) => {
            // Geen glas is geen crash: de jobspec vroeg het niet, of een
            // ander slot houdt het al. Luid, en klaar.
            log!("display: no glass: {e}, nothing to draw HOPOS_DISPLAY_NOGLASS");
            return;
        }
    };
    match fb::map(app, &glass) {
        Ok(m) => log!(
            "display: window {:#x}+{:#x} mapped Normal-NC in stage-1 ({} bytes of tables)",
            m.base,
            m.size,
            m.table_bytes
        ),
        // Geen stage-1 (de MMU staat uit): het glas is Device, trager maar
        // correct.
        Err(e @ fb::FbError::NoStage1(_)) => {
            log!("display: {e}, drawing through Device HOPOS_DISPLAY_NOMAP")
        }
        // De MMU staat aan maar het glas kwam er niet in: de eerste pixel
        // zou een vertaalfout zijn (sinds 30-09 heeft elke app een
        // stage-1, applib::mmu). Luid, en niet tekenen.
        Err(e) => {
            log!("display: {e}, the glass is not mapped, nothing to draw HOPOS_DISPLAY_NOMAP");
            return;
        }
    }
    let (tx, rx) = EVENTS.split().expect("display: event queue split once");
    let mut screen = Screen::new(Painter::new(glass), app);
    screen.draw_all();
    log!(
        "display: glass {}x{} stride {} bpp {} at {:#x} HOPOS_DISPLAY_UP w={} h={}",
        glass.width,
        glass.height,
        glass.stride,
        glass.bpp,
        glass.base,
        glass.width,
        glass.height
    );
    start_input(app, exec, tx, &mut screen);
    let quit = app
        .env("DISPLAY_QUIT")
        .and_then(|v| v.trim().parse::<i32>().ok());
    draw(exec, screen, rx, quit).await;
}

/// Start de invoertaak als de grant een `INPUT_ADDR` meegaf.
fn start_input(
    app: &'static App,
    exec: &'static Exec,
    tx: Sender<'static, Input, QUEUE>,
    screen: &mut Screen,
) {
    let (ip, port) = match fb::input_addr(|k| app.env(k)) {
        None => {
            log!("display: no INPUT_ADDR, this node has no USB input HOPOS_DISPLAY_NOINPUT");
            screen.set_status(b"input: none on this node");
            return;
        }
        Some(Err(e)) => {
            log!("display: {e} HOPOS_DISPLAY_NOINPUT");
            screen.set_status(b"input: bad INPUT_ADDR");
            return;
        }
        Some(Ok(a)) => a,
    };
    if let Err(e) = applib::appnet::up(app) {
        log!("display: network stack: {e}, no input HOPOS_DISPLAY_NOINPUT");
        screen.set_status(b"input: no network");
        return;
    }
    if exec.spawn(input(exec, ip, port, tx)).is_err() {
        log!("display: input task not spawned HOPOS_DISPLAY_NOINPUT");
        return;
    }
    screen.set_status(b"input: calling the kernel");
}

/// De tekentaak: een gebeurtenis of de volgende seconde. Keert alleen terug
/// op de toets van `DISPLAY_QUIT`.
async fn draw(
    exec: &'static Exec,
    mut screen: Screen,
    mut rx: Receiver<'static, Input, QUEUE>,
    quit: Option<i32>,
) {
    let mut next = exec.now().saturating_add(1_000_000_000);
    loop {
        match select(rx.recv(), exec.until(next)).await {
            Either::Left(ev) => {
                let mut ev = Some(ev);
                // Wat er intussen nog kwam, in dezelfde ronde.
                while let Some(e) = ev {
                    if let Input::Key { code, down: true } = e
                        && Some(code) == quit
                    {
                        screen.log_counts(exec.now(), true);
                        log!("display: quit key {code}, giving the glass back HOPOS_DISPLAY_QUIT");
                        return;
                    }
                    screen.apply(e);
                    ev = rx.try_recv();
                }
                screen.log_counts(exec.now(), false);
            }
            Either::Right(()) => {
                screen.draw_clock();
                screen.log_counts(exec.now(), true);
                next = next.saturating_add(1_000_000_000);
            }
        }
    }
}

/// De invoertaak: bellen, regels lezen en doorgeven, en opnieuw bellen als
/// de stroom breekt.
async fn input(exec: &'static Exec, ip: [u8; 4], port: u16, mut tx: Sender<'static, Input, QUEUE>) {
    let [a, b, c, d] = ip;
    let mut retry = RETRY_FIRST;
    let mut failed: u64 = 0;
    let mut line = [0u8; LINE_CAP];
    loop {
        match TcpStream::connect_timeout(ip, port, CONNECT_TIMEOUT).await {
            Ok(mut s) => {
                log!("display: input stream from {a}.{b}.{c}.{d}:{port} HOPOS_DISPLAY_CONN");
                let _ = tx.try_send(Input::Keepalive);
                retry = RETRY_FIRST;
                // Een verse lezer per verbinding: een halve regel van de
                // vorige hoort niet bij de eerste van deze.
                let mut reader = LineReader::new();
                loop {
                    s.set_timeout(Some(READ_TIMEOUT));
                    let n = match s.read(reader.spare()).await {
                        Ok(0) | Err(_) => break,
                        Ok(n) => n,
                    };
                    reader.commit(n);
                    while let Some(len) = reader.pop(&mut line) {
                        let Some(ev) = line.get(..len).and_then(Input::parse) else {
                            continue;
                        };
                        if ev != Input::Keepalive && tx.try_send(ev).is_err() {
                            DROPS.fetch_add(1, Relaxed);
                        }
                    }
                }
                log!("display: input stream closed, calling again HOPOS_DISPLAY_RECONNECT");
                let _ = s.close();
            }
            Err(e) => {
                failed = failed.wrapping_add(1);
                if failed <= LOUD {
                    log!("display: input {a}.{b}.{c}.{d}:{port}: {e}, {failed} so far");
                }
            }
        }
        exec.after(retry).await;
        retry = (retry * 2).min(RETRY_MAX);
    }
}

/// Eén regel tekst van vaste maat.
type Text = bounded::Text<LINE_CHARS>;

/// De tellers van de invoer.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Counts {
    keys: u64,
    moves: u64,
    buttons: u64,
    wheel: u64,
}

/// Het beeld en zijn staat: van de tekentaak alleen.
struct Screen {
    p: Painter,
    app: &'static App,
    /// Tekstcellen van `8 * scale` pixels.
    scale: u32,
    cursor: Cursor,
    /// Waar de cursor hoort (de laatste beweging); de [`Cursor`] zelf is
    /// even weg zolang er tekst onder hem getekend wordt.
    pos: Option<(u32, u32)>,
    counts: Counts,
    /// De tellers bij de laatste logregel, en wanneer die was.
    logged: (Counts, u64),
    lines: [Text; EVENT_LINES],
    /// De volgende regel die overschreven wordt (een ring).
    next_line: usize,
    status: &'static [u8],
    started_ns: u64,
}

impl Screen {
    fn new(p: Painter, app: &'static App) -> Screen {
        let g = p.glass();
        // 16x16-cellen vanaf een echt scherm, zoals de console van de kern
        // (`driver_fb::SCALE2_FROM`).
        let scale = if g.height >= driver_fb::SCALE2_FROM {
            2
        } else {
            1
        };
        Screen {
            p,
            app,
            scale,
            cursor: Cursor::default(),
            pos: None,
            counts: Counts::default(),
            logged: (Counts::default(), 0),
            lines: [Text::new(); EVENT_LINES],
            next_line: 0,
            status: b"input: -",
            started_ns: clock::now_ns(),
        }
    }

    fn cell(&self) -> u32 {
        8 * self.scale
    }

    /// Een regel op rij `row`, opgevuld met spaties tot [`LINE_CHARS`]: zo
    /// wist de nieuwe tekst de oude zonder eerst te vegen.
    fn put_line(&mut self, col: u32, row: u32, s: &[u8]) {
        let mut padded = [b' '; LINE_CHARS];
        for (d, b) in padded.iter_mut().zip(s) {
            *d = *b;
        }
        self.put_text(col, row, &padded, FG);
    }

    /// Tekst op rij `row` (in cellen), kolom `col`.
    fn put_text(&mut self, col: u32, row: u32, s: &[u8], fg: u32) {
        let c = self.cell();
        self.cursor.hide(&self.p);
        self.p.text(col * c, row * c, self.scale, s, fg, BG);
        self.restore_cursor();
    }

    fn restore_cursor(&mut self) {
        if let Some((x, y)) = self.pos {
            self.cursor.show(&self.p, x, y, CURSOR_RGB, CURSOR_EDGE);
        }
    }

    /// Het hele beeld: achtergrond, titel, feiten, tellers, regels, klok.
    fn draw_all(&mut self) {
        let g = self.p.glass();
        self.p.fill(0, 0, g.width, g.height, BG);
        self.put_text(1, 1, b"HopOS display", ACCENT);
        let mut facts = Text::new();
        let _ = write!(
            facts,
            "slot {}  {}x{}  {} bpp",
            self.app.slot(),
            g.width,
            g.height,
            g.bpp
        );
        self.put_line(1, 3, facts.as_bytes());
        self.put_line(1, 4, self.status);
        self.draw_counts();
        self.put_text(1, 8, b"last input:", ACCENT);
        for i in 0..EVENT_LINES {
            self.draw_line(i);
        }
        self.draw_clock();
    }

    fn set_status(&mut self, s: &'static [u8]) {
        self.status = s;
        self.put_line(1, 4, s);
    }

    fn draw_counts(&mut self) {
        let mut t = Text::new();
        let c = self.counts;
        let _ = write!(
            t,
            "keys {}  moves {}  buttons {}  wheel {}",
            c.keys, c.moves, c.buttons, c.wheel
        );
        self.put_line(1, 6, t.as_bytes());
    }

    fn draw_line(&mut self, i: usize) {
        let Some(t) = self.lines.get(i).copied() else {
            return;
        };
        self.put_line(3, 10 + i as u32, t.as_bytes());
    }

    /// De klok rechtsboven: de wandklok als de kern hem synct, anders de
    /// tijd sinds de start.
    fn draw_clock(&mut self) {
        let mut t = Text::new();
        match self.app.wall_ns() {
            Some(ns) => {
                let s = ns / 1_000_000_000 % 86_400;
                let _ = write!(t, "{:02}:{:02}:{:02} UTC", s / 3600, s % 3600 / 60, s % 60);
            }
            None => {
                let s = clock::now_ns().saturating_sub(self.started_ns) / 1_000_000_000;
                let _ = write!(t, "up {:02}:{:02}:{:02}", s / 3600, s % 3600 / 60, s % 60);
            }
        }
        let g = self.p.glass();
        let cols = g.width / self.cell();
        let col = cols.saturating_sub(t.len() as u32 + 1);
        let s = t.as_bytes();
        self.put_text(col, 1, s, ACCENT);
    }

    /// Eén gebeurtenis: tellers, een regel, en bij een beweging de cursor.
    fn apply(&mut self, ev: Input) {
        let mut t = Text::new();
        match ev {
            Input::Keepalive => {
                self.set_status(b"input: connected to the kernel");
                return;
            }
            Input::Key { code, down } => {
                if down {
                    self.counts.keys += 1;
                }
                let _ = write!(t, "key {code} {}", if down { "down" } else { "up" });
            }
            Input::Move { x, y } => {
                self.counts.moves += 1;
                self.move_cursor(x, y);
                let _ = write!(t, "move {x},{y}");
            }
            Input::Button { code, down, x, y } => {
                if down {
                    self.counts.buttons += 1;
                }
                let _ = write!(
                    t,
                    "button {code} {} at {x},{y}",
                    if down { "down" } else { "up" }
                );
            }
            Input::Wheel { v, x, y } => {
                self.counts.wheel += 1;
                let _ = write!(t, "wheel {v} at {x},{y}");
            }
        }
        let i = self.next_line;
        if let Some(slot) = self.lines.get_mut(i) {
            *slot = t;
        }
        self.next_line = (i + 1) % EVENT_LINES;
        self.draw_line(i);
        self.draw_counts();
    }

    fn move_cursor(&mut self, x: i32, y: i32) {
        let g = self.p.glass();
        let clamp = |v: i32, max: u32| -> u32 {
            u32::try_from(v.max(0))
                .unwrap_or(0)
                .min(max.saturating_sub(CURSOR))
        };
        self.pos = Some((clamp(x, g.width), clamp(y, g.height)));
        self.restore_cursor();
    }

    /// De tellers als logregel, als ze veranderden: meteen als de vorige
    /// regel lang genoeg geleden is, anders bij de volgende tik van de klok.
    fn log_counts(&mut self, now: u64, tick: bool) {
        let (last, at) = self.logged;
        if self.counts == last {
            return;
        }
        let every = u64::try_from(LOG_EVERY.as_nanos()).unwrap_or(u64::MAX);
        if !tick && now.saturating_sub(at) < every {
            return;
        }
        let c = self.counts;
        log!(
            "display: keys={} moves={} buttons={} wheel={} dropped={} HOPOS_DISPLAY_INPUT keys={} moves={}",
            c.keys,
            c.moves,
            c.buttons,
            c.wheel,
            DROPS.load(Relaxed),
            c.keys,
            c.moves
        );
        self.logged = (c, now);
    }
}
