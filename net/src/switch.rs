//! De switch-actor: HOP's interne L2-frame-switch over de per-slot
//! frame-ringen (Go: `hopswitch.go`, `gateway.go`).
//!
//! Elke app-core draait een eigen netstack over rauwe Ethernet-frames door
//! zijn frame-ringen; HOP kopieert die frames uitsluitend ring-naar-ring op
//! de dst-MAC. App-naar-app-verkeer raakt nooit een TCP-stack op core 0:
//! "Apps rekenen, HOP sjouwt data."
//!
//! HOP is poort 0 op hetzelfde LAN. ARP voor de gateway (10.100.0.1)
//! beantwoordt de switch zelf; verkeer naar buiten gaat door de NAT
//! ([`crate::nat`]) en de uplink-rij naar de pomp.
//!
//! Eigendom (handboek §1.1): [`Switch`] bezit de poorten, de NAT-tabel en de
//! switch-kant van elke ring als `&mut self`. Aan- en afmelden zijn berichten
//! ([`Command`]); de aanroeper wacht op zijn [`Ack`]. Zo is de actor vanzelf
//! de enige producer per RX-ring en de enige consument per TX-ring, zonder
//! slot. De deur van de executor leest alleen [`Published`], met kale loads.

use crate::nat::{FlowState, Nat, NatIo, NatState, Proto, Uplink};
use crate::plan::{
    HOST_MAC, MAX_LAN_FRAME, PORTS, SLOT_CAP, UPLINK_MAX_FRAME, host_ip4, slot_ip4, slot_mac,
};
use crate::ring::{KIND_FRAME, KIND_UPLINK, Reader, Writer};
use crate::wire::{ET_ARP, ET_IPV4, ET_IPV6, ETH_LEN, be16, be32, byte, mac_at, put_mac, put16};
use crate::{Error, Frame, LogFn, Result, Stats, UPLINK_QUEUE};
use alloc::vec::Vec;
use core::fmt;
use core::marker::PhantomData;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use executor::Executor;
use sync::mpsc::Mailbox;
use sync::spsc::{Receiver, Sender};
use sync::{Either, Signal, Stop, select, yield_now};

/// Het aantal frames per poort per switch-ronde, zodat één drukke poort de
/// rest niet verhongert.
pub const MAX_BURST: usize = 64;

/// Begrensde lokale backpressure: een korte TCP-burst verdwijnt niet stil,
/// en een niet-lezende poort kan de switch niet blokkeren.
const TX_BACKPRESSURE: u64 = 10_000_000;

/// De failsafe van de lus. De bel is level-triggered via de deur van de
/// executor, maar die kijkt alleen als HOP níets te doen heeft. Is HOP bezig
/// (een 1 MiB-write naar de NVMe, een S3-transfer), dan mist de SEV van een
/// app zijn WFE en ligt het frame tot deze failsafe. 1 ms is dan de klok,
/// zoals de waker die al heeft; korter zou pollen zijn, 10 ms een zichtbare
/// hik in elk request. Weg zodra een app-core HOP een IPI kan sturen, en
/// zolang [`Stats::work_by_timer`] zegt dat hij nog geraakt wordt.
pub const FAILSAFE: Duration = Duration::from_millis(1);

/// De diepte van de brievenbus van de switch.
pub const COMMANDS: usize = 16;

/// De brievenbus van de switch.
pub type Commands<'a, R, W> = Mailbox<Command<'a, R, W>, COMMANDS>;

/// De bevestiging van een [`Command`]: de actor zet het resultaat en luidt
/// de bel. Eén `Ack` per aanroeper, herbruikbaar.
pub struct Ack {
    done: Signal,
    result: Mailbox<Result<u32>, 1>,
}

impl Ack {
    /// Een lege bevestiging.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            done: Signal::new(),
            result: Mailbox::new(),
        }
    }

    fn complete(&self, r: Result<u32>) {
        // De plaats is leeg: één commando per `Ack` tegelijk. Een aanroeper
        // die dat breekt, krijgt het eerste resultaat.
        let _ = self.result.try_send(r);
        self.done.set();
    }

    /// Wacht tot de actor het commando heeft uitgevoerd. Bij `Detach`
    /// betekent terugkeer: de switch raakt de ringen gegarandeerd niet meer
    /// aan (ze zijn gedropt), dus een ring-herinit mag.
    pub async fn wait(&self) -> Result<u32> {
        loop {
            self.done.wait().await;
            if let Some(r) = self.result.try_recv() {
                return r;
            }
        }
    }

    /// Het resultaat als het er al is, zonder te wachten.
    pub fn try_take(&self) -> Option<Result<u32>> {
        let r = self.result.try_recv()?;
        self.done.take();
        Some(r)
    }
}

impl Default for Ack {
    fn default() -> Self {
        Self::new()
    }
}

/// De conntrack zoals de kern-flip hem meeneemt: de flows in de buffer
/// die de aanroeper meegaf (verplaatst, niet gedeeld), plus de twee
/// woorden van [`NatState`].
#[derive(Debug, Default)]
pub struct NatSnapshot {
    /// De levende flows (de buffer van de aanroeper, ingekort).
    pub flows: Vec<FlowState>,
    /// De volgende masquerade-kandidaat.
    pub masq_next: u16,
    /// De geleerde gateway-MAC.
    pub gw_mac: Option<[u8; 6]>,
}

impl NatSnapshot {
    /// De snapshot als [`NatState`] over zijn eigen flows.
    #[must_use]
    pub fn state(&self) -> NatState<'_> {
        NatState {
            flows: &self.flows,
            masq_next: self.masq_next,
            gw_mac: self.gw_mac,
        }
    }
}

/// De antwoordplek van [`Command::SnapshotNat`]: de actor legt de snapshot
/// erin en luidt de bel. Eén aanroeper tegelijk (de flip-taak).
pub struct NatReply {
    done: Signal,
    snap: Mailbox<NatSnapshot, 1>,
}

impl NatReply {
    /// Een lege antwoordplek.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            done: Signal::new(),
            snap: Mailbox::new(),
        }
    }

    fn complete(&self, s: NatSnapshot) {
        let _ = self.snap.try_send(s);
        self.done.set();
    }

    /// Wacht op de snapshot. Er leeft geen lening over de `.await`: de
    /// snapshot komt als waarde terug.
    pub async fn wait(&self) -> NatSnapshot {
        loop {
            self.done.wait().await;
            if let Some(s) = self.snap.try_recv() {
                return s;
            }
        }
    }
}

impl Default for NatReply {
    fn default() -> Self {
        Self::new()
    }
}

/// Een bericht aan de switch-actor.
pub enum Command<'a, R, W> {
    /// Koppelt slot `slot` aan de switch met zijn twee ringen (door de
    /// slot-lifecycle, ná de ring-init). De switch wordt eigenaar.
    Attach {
        /// Het slot (1..=max).
        slot: usize,
        /// De TX-ring (app → switch), leeskant.
        tx: R,
        /// De RX-ring (switch → app), schrijfkant.
        rx: W,
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Ontkoppelt slot `slot`; de ringen worden gedropt.
    Detach {
        /// Het slot.
        slot: usize,
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Publiceert node-poort naar slot-poort.
    Publish {
        /// Protocol.
        proto: Proto,
        /// De node-poort.
        node_port: u16,
        /// Het slot.
        slot: usize,
        /// De poort in het slot.
        slot_port: u16,
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Trekt de publicaties en flows van een slot in.
    UnpublishSlot {
        /// Het slot.
        slot: usize,
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Zet het externe adres (na DHCP).
    SetUplink {
        /// Het adres.
        uplink: Uplink,
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Houdt node-poorten vast tijdens een kern-flip.
    HoldAdoption {
        /// De poorten; de aanroeper houdt ze vast tot de bevestiging.
        ports: &'a [u16],
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Einde van de adoptie.
    FinishAdoption {
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Herstelt de conntrack uit het handoff-blob; het resultaat is het
    /// aantal herstelde flows.
    RestoreNat {
        /// De staat van de vorige kern.
        state: NatState<'a>,
        /// Bevestiging.
        ack: &'a Ack,
    },
    /// Beschrijft de conntrack voor de kern-flip in `buf` (de aanroeper
    /// geeft een buffer van `MAX_FLOWS` plaatsen mee; de actor alloceert
    /// niets) en bevriest de masquerade-toewijzing: een nieuwe uitgaande
    /// verbinding tussen deze snapshot en de sprong zou geen flow in het
    /// blob hebben, en zijn SYN-retransmit krijgt er op de nieuwe kern
    /// gewoon een. Gaat de flip niet door, dan ontdooit
    /// [`Command::FinishAdoption`] hem weer.
    SnapshotNat {
        /// De buffer, als waarde: hij komt gevuld terug in `reply`.
        buf: Vec<FlowState>,
        /// De antwoordplek.
        reply: &'a NatReply,
    },
    /// De tik van de flow-expiry-taak: veeg de verlopen flows.
    Sweep,
}

/// De poortentabel zoals de deur van de executor hem ziet: per poort het
/// probe-handvat van de TX-ring (plus één; 0 = geen poort).
///
/// Gepubliceerd in de zin van het handboek §2.1: de actor schrijft, de deur
/// leest zonder slot. De grace-periode is "na de volgende ronde van de
/// executor": de deur draait tussen rondes, nooit tijdens de actor, dus na
/// een `Detach` ziet geen lezer het oude handvat nog. Een lezer op een andere
/// core mag racen: een verouderd handvat geeft hooguit een overbodige bel,
/// en de actor kijkt zelf nog een keer.
pub struct Published<R> {
    tx: [AtomicU64; PORTS],
    _ring: PhantomData<fn() -> R>,
}

impl<R: Reader> Published<R> {
    /// Een lege tabel; voor een `static`.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            tx: [const { AtomicU64::new(0) }; PORTS],
            _ring: PhantomData,
        }
    }

    /// Ligt er werk in een TX-ring? Alleen kale loads (Go: `switchPending`,
    /// en zie [`Reader::probe`] voor de 1,7M-rondes/s-les van 04-09).
    #[must_use]
    pub fn pending(&self) -> bool {
        self.tx.iter().any(|h| {
            let v = h.load(Relaxed);
            v != 0 && R::probe(v - 1)
        })
    }

    fn set(&self, port: usize, handle: Option<u64>) {
        if let Some(h) = self.tx.get(port) {
            h.store(handle.map_or(0, |v| v.wrapping_add(1)), Relaxed);
        }
    }
}

impl<R: Reader> Default for Published<R> {
    fn default() -> Self {
        Self::new()
    }
}

/// Eén switch-poort: de ringen van een actief slot (of van HOP op poort 0).
struct Port<R, W> {
    tx: R,
    rx: W,
    /// Na één begrensde wacht droppen volgende frames totdat de consument
    /// aantoonbaar weer ruimte heeft gemaakt.
    rx_blocked: bool,
    /// De corrupt-verklaring van de TX-ring is al gemeld: één regel per
    /// leven van de poort, anders verzuipt de console.
    tx_warned: bool,
}

/// De vaste instellingen van de switch.
#[derive(Clone, Copy)]
pub struct Config {
    /// Het hoogste slotnummer van dit board (≤ [`SLOT_CAP`]).
    pub max_slots: usize,
    /// Monotone nanoseconden.
    pub clock: fn() -> u64,
    /// De console.
    pub log: LogFn,
    /// De doelkick van slot `i` na een leeg→niet-leeg-overgang van zijn
    /// RX-ring (een IPI, of de wek van een app die naar EL2 yieldde).
    pub slot_wake: fn(usize),
}

/// De gedeelde kanten van de switch: wat andere taken van hem zien.
pub struct Wiring<'a, R: Reader, W: Writer> {
    /// De brievenbus.
    pub commands: &'a Commands<'a, R, W>,
    /// De bel: zet hem na elke schrijf in een TX-ring (Go: `Kick`).
    pub door: &'a Signal,
    /// De tabel voor de deur van de executor.
    pub published: &'a Published<R>,
    /// De meetlat.
    pub stats: &'a Stats,
    /// De bel van de host-taak (poort 0).
    pub host_bell: Option<&'a Signal>,
    /// Van de pomp: ontvangen uplink-frames.
    pub ingress: Option<Receiver<'a, Frame, UPLINK_QUEUE>>,
    /// Naar de pomp: frames voor de draad.
    pub egress: Option<Sender<'a, Frame, UPLINK_QUEUE>>,
    /// De bel van de pomp, na een ronde met egress-werk.
    pub pump_bell: Option<&'a Signal>,
}

/// De poorten en de uitgangen: alles wat de NAT als [`NatIo`] ziet.
struct Core<'a, R, W> {
    ports: [Option<Port<R, W>>; PORTS],
    cfg: Config,
    stats: &'a Stats,
    host_bell: Option<&'a Signal>,
    egress: Option<Sender<'a, Frame, UPLINK_QUEUE>>,
    pump_bell: Option<&'a Signal>,
    egress_dirty: bool,
}

/// De switch-actor.
pub struct Switch<'a, R: Reader, W: Writer> {
    core: Core<'a, R, W>,
    nat: Nat,
    commands: &'a Commands<'a, R, W>,
    door: &'a Signal,
    published: &'a Published<R>,
    ingress: Option<Receiver<'a, Frame, UPLINK_QUEUE>>,
}

impl<'a, R: Reader, W: Writer> Switch<'a, R, W> {
    /// Een switch zonder poorten. Alloceert de NAT-tabellen (boot).
    pub fn new(cfg: Config, w: Wiring<'a, R, W>) -> Result<Self> {
        let cfg = Config {
            max_slots: cfg.max_slots.min(SLOT_CAP),
            ..cfg
        };
        Ok(Self {
            core: Core {
                ports: core::array::from_fn(|_| None),
                cfg,
                stats: w.stats,
                host_bell: w.host_bell,
                egress: w.egress,
                pump_bell: w.pump_bell,
                egress_dirty: false,
            },
            nat: Nat::new()?,
            commands: w.commands,
            door: w.door,
            published: w.published,
            ingress: w.ingress,
        })
    }

    /// Hangt HOP's eigen ringen aan poort 0 (bij boot, vóór `run`).
    pub fn attach_host(&mut self, tx: R, rx: W) {
        self.published.set(0, Some(tx.probe_handle()));
        if let Some(p) = self.core.ports.get_mut(0) {
            *p = Some(Port {
                tx,
                rx,
                rx_blocked: false,
                tx_warned: false,
            });
        }
    }

    /// Koppelt slot `i` (de actor-kant van [`Command::Attach`]; vóór `run`
    /// ook direct te roepen). Een oude poort op dat slot wordt gedropt.
    pub fn attach(&mut self, i: usize, tx: R, rx: W) -> Result {
        if i < 1 || i > self.core.cfg.max_slots {
            return Err(Error::SlotRange(i));
        }
        let handle = tx.probe_handle();
        let Some(p) = self.core.ports.get_mut(i) else {
            return Err(Error::SlotRange(i));
        };
        *p = Some(Port {
            tx,
            rx,
            rx_blocked: false,
            tx_warned: false,
        });
        self.published.set(i, Some(handle));
        Ok(())
    }

    /// Ontkoppelt slot `i`: eerst uit de gepubliceerde tabel, dan gedropt.
    pub fn detach(&mut self, i: usize) -> Result {
        if i < 1 || i > self.core.cfg.max_slots {
            return Err(Error::SlotRange(i));
        }
        self.published.set(i, None);
        if let Some(p) = self.core.ports.get_mut(i) {
            *p = None;
        }
        Ok(())
    }

    /// De NAT, voor setup vóór `run` en voor de kern-flip ná `run`.
    pub fn nat(&mut self) -> &mut Nat {
        &mut self.nat
    }

    /// Hangt slot `i` aan de switch?
    #[must_use]
    pub fn is_attached(&self, i: usize) -> bool {
        self.core.ports.get(i).is_some_and(Option::is_some)
    }

    /// Voert één commando uit.
    pub fn handle(&mut self, cmd: Command<'a, R, W>) {
        let now = (self.core.cfg.clock)();
        match cmd {
            Command::Attach { slot, tx, rx, ack } => {
                ack.complete(self.attach(slot, tx, rx).map(|()| 0))
            }
            Command::Detach { slot, ack } => ack.complete(self.detach(slot).map(|()| 0)),
            Command::Publish {
                proto,
                node_port,
                slot,
                slot_port,
                ack,
            } => {
                let max = self.core.cfg.max_slots;
                ack.complete(
                    self.nat
                        .publish(proto, node_port, slot, slot_port, max)
                        .map(|()| 0),
                );
            }
            Command::UnpublishSlot { slot, ack } => {
                self.nat.unpublish_slot(slot);
                ack.complete(Ok(0));
            }
            Command::SetUplink { uplink, ack } => {
                self.nat.set_uplink(uplink);
                ack.complete(Ok(0));
            }
            Command::HoldAdoption { ports, ack } => {
                ack.complete(self.nat.hold_adoption(ports).map(|()| 0))
            }
            Command::FinishAdoption { ack } => {
                self.nat.finish_adoption();
                ack.complete(Ok(0));
            }
            Command::RestoreNat { state, ack } => {
                let ports = &self.core.ports;
                let n =
                    self.nat
                        .restore(&state, |s| ports.get(s).is_some_and(Option::is_some), now);
                ack.complete(Ok(u32::try_from(n).unwrap_or(u32::MAX)));
            }
            Command::SnapshotNat { mut buf, reply } => {
                // Eerst dicht, dan lezen: zo staat er na de snapshot geen
                // nieuwe flow meer in de tabel die het blob mist. Een lege
                // claimlijst past altijd.
                let _ = self.nat.hold_adoption(&[]);
                let st = self.nat.snapshot(now, &mut buf);
                let (n, masq_next, gw_mac) = (st.flows.len(), st.masq_next, st.gw_mac);
                buf.truncate(n);
                reply.complete(NatSnapshot {
                    flows: buf,
                    masq_next,
                    gw_mac,
                });
            }
            Command::Sweep => self.nat.sweep(now),
        }
    }

    fn drain_commands(&mut self) -> bool {
        let mut worked = false;
        while let Some(cmd) = self.commands.try_recv() {
            self.handle(cmd);
            worked = true;
        }
        worked
    }

    /// Eén ronde: alle TX-ringen draineren (hoogstens [`MAX_BURST`] per
    /// poort) en per frame op dst-MAC bezorgen, dan de uplink-ingress. `buf`
    /// is de ene hergebruikte framebuffer: geen allocatie per frame.
    pub fn switch_pass(&mut self, buf: &mut [u8]) -> bool {
        let now = (self.core.cfg.clock)();
        let mut worked = false;
        for i in 0..=self.core.cfg.max_slots {
            for _ in 0..MAX_BURST {
                let rec = match self.core.ports.get_mut(i).and_then(Option::as_mut) {
                    Some(p) => p.tx.read_into(buf),
                    None => break,
                };
                let Some((kind, n)) = rec else { break };
                let Some(f) = buf.get_mut(..n) else { break };
                match kind {
                    KIND_FRAME => forward(&mut self.core, &mut self.nat, i, f, now),
                    // Extern verkeer van HOP's eigen stack: de draad op.
                    KIND_UPLINK if i == 0 => self.core.uplink_tx(f),
                    _ => continue,
                }
                worked = true;
            }
            self.warn_corrupt(i);
        }
        for _ in 0..MAX_BURST {
            let Some(mut fr) = self.ingress.as_mut().and_then(|r| r.try_recv()) else {
                break;
            };
            uplink_in(&mut self.core, &mut self.nat, fr.bytes_mut(), now);
            worked = true;
        }
        // De RX-koppen die deze ronde bewogen één keer naar het geheugen,
        // voor de switcher-peek; niet per frame.
        for p in self.core.ports.iter_mut().skip(1).flatten() {
            p.rx.publish_head();
        }
        worked
    }

    fn warn_corrupt(&mut self, i: usize) {
        let log = self.core.cfg.log;
        if let Some(p) = self.core.ports.get_mut(i).and_then(Option::as_mut)
            && !p.tx_warned
            && let Some(why) = p.tx.corrupt()
        {
            p.tx_warned = true;
            log(format_args!("HOPOS_NETRING_TX_CORRUPT: slot {i}: {why}"));
        }
    }

    /// Eén doorbell per ronde, niet per frame (Go: `FlushUplinkTX`): de pomp
    /// wekken als er egress-werk ligt.
    fn flush_uplink(&mut self) {
        if self.core.egress_dirty {
            self.core.egress_dirty = false;
            if let Some(b) = self.core.pump_bell {
                b.set();
            }
        }
    }

    /// De lus: commando's, een switch-ronde, de uplink-doorbell, dan
    /// `yield_now` zodat de doelnetstack consumeert vóór een nieuwe volle
    /// burst. Niets te doen: wachten op de bel, een commando of de
    /// failsafe. Keert terug als `stop` luidt; de staat blijft van de
    /// aanroeper (de kern-flip leest dan de NAT).
    pub async fn run<const T: usize, const M: usize>(
        &mut self,
        exec: &'static Executor<T, M>,
        buf: &mut [u8],
        stop: &Stop,
    ) {
        let mut by_timer = false;
        loop {
            if stop.is_set() {
                return;
            }
            let cmds = self.drain_commands();
            if self.switch_pass(buf) {
                self.flush_uplink();
                // Werk ná de failsafe in plaats van na een bel is een SEV die
                // HOP's WFE miste: een frame dat tot een milliseconde lag.
                let c = if by_timer {
                    &self.core.stats.work_by_timer
                } else {
                    &self.core.stats.work_by_door
                };
                c.fetch_add(1, Relaxed);
                yield_now().await;
                continue;
            }
            self.flush_uplink();
            if cmds {
                continue;
            }
            let commands = self.commands;
            let idle = select(
                stop.wait(),
                select(
                    commands.recv(),
                    select(self.door.wait(), exec.after(FAILSAFE)),
                ),
            )
            .await;
            match idle {
                Either::Left(()) => return,
                Either::Right(Either::Left(cmd)) => self.handle(cmd),
                Either::Right(Either::Right(Either::Left(()))) => by_timer = false,
                Either::Right(Either::Right(Either::Right(()))) => by_timer = true,
            }
        }
    }
}

/// De flow-expiry-taak: elke `FLOW_SWEEP_EVERY` een `Sweep` in de
/// brievenbus. De tabel blijft van de actor; deze taak is alleen de klok.
/// Vol = deze tik overslaan (de volgende komt, en een volle pool veegt zelf).
pub async fn flow_expiry<R: Reader, W: Writer, const T: usize, const M: usize>(
    exec: &'static Executor<T, M>,
    commands: &Commands<'_, R, W>,
    stop: &Stop,
) {
    let every = Duration::from_nanos(crate::nat::FLOW_SWEEP_EVERY);
    loop {
        if let Either::Left(()) = select(stop.wait(), exec.after(every)).await {
            return;
        }
        let _ = commands.try_send(Command::Sweep);
    }
}

impl<R: Reader, W: Writer> Core<'_, R, W> {
    fn port(&mut self, i: usize) -> Option<&mut Port<R, W>> {
        self.ports.get_mut(i).and_then(Option::as_mut)
    }

    fn attached(&self, i: usize) -> bool {
        self.ports.get(i).is_some_and(Option::is_some)
    }

    /// De enige producergrens voor LAN-RX (Go: `writeRXLocked`). Een korte
    /// lokale burst mag wachten tot de consument ruimte maakt; daarna geldt
    /// weer Ethernet: timeout = drop, TCP herstelt.
    ///
    /// Het wachten is een spin, geen `.await`: de consument van een slot-ring
    /// is een andere core, dus spinnen helpt, en de actor blijft zo één
    /// synchrone ronde. Poort 0 wacht niet: zijn consument is de host-taak op
    /// déze core, en die kan pas draaien als de actor afgeeft.
    fn write_rx(&mut self, i: usize, kind: u32, p: &[u8]) {
        let clock = self.cfg.clock;
        let deadline = clock().saturating_add(TX_BACKPRESSURE);
        let mut woken = false;
        loop {
            let Some(port) = self.port(i) else { return };
            if let Some(notify) = port.rx.write_notify(kind, p) {
                port.rx_blocked = false;
                if notify {
                    // Kop naar het geheugen vóór de kick: een core die op EL2
                    // slaapt peekt na zijn wekker de kop in DRAM, en een kop
                    // die nog in HOP's cache staat is voor hem leeg (T30,
                    // 04-09: schrijven 690 → 40 MB/s). Eén clean per burst.
                    port.rx.publish_head();
                    self.wake(i);
                }
                return;
            }
            if port.rx_blocked {
                self.stats.rx_drops.fetch_add(1, Relaxed);
                return;
            }
            if !woken {
                self.stats.rx_full.fetch_add(1, Relaxed);
                woken = true;
                self.wake(i);
            }
            if i == 0 || clock() > deadline {
                if let Some(port) = self.port(i) {
                    port.rx_blocked = true;
                }
                self.stats.rx_drops.fetch_add(1, Relaxed);
                return;
            }
            core::hint::spin_loop();
        }
    }

    fn wake(&self, i: usize) {
        // Dedicated ARM-apps kunnen zelf in WFE staan; hetzelfde generieke
        // event wekt hen zonder dat de switch hun architectuur kent. Een app
        // die naar EL2 yieldde krijgt daarna de doelkick.
        dev::notify();
        if i == 0 {
            if let Some(b) = self.host_bell {
                b.set();
            }
        } else {
            (self.cfg.slot_wake)(i);
        }
    }

    /// Een frame de uplink op, via de egress-rij naar de pomp.
    fn uplink_tx(&mut self, f: &[u8]) {
        if f.len() > UPLINK_MAX_FRAME {
            // Een jumbo van het slot-LAN hoort hier nooit te komen (de stacks
            // klemmen de MSS per bestemming); de NIC-driver zou zijn
            // descriptor overlopen. Tellen en droppen.
            self.stats.nat_oversize.fetch_add(1, Relaxed);
            return;
        }
        let sent = match (self.egress.as_mut(), Frame::from_slice(f)) {
            (Some(tx), Some(fr)) => tx.try_send(fr).is_ok(),
            _ => false,
        };
        if sent {
            self.egress_dirty = true;
        } else {
            self.stats.uplink_tx_drops.fetch_add(1, Relaxed);
        }
    }
}

impl<R: Reader, W: Writer> NatIo for Core<'_, R, W> {
    fn deliver(&mut self, slot: usize, f: &[u8]) {
        if f.len() > MAX_LAN_FRAME || slot < 1 || slot > self.cfg.max_slots {
            return;
        }
        self.write_rx(slot, KIND_FRAME, f);
    }

    fn uplink_tx(&mut self, f: &[u8]) {
        Core::uplink_tx(self, f);
    }

    fn stats(&self) -> &Stats {
        self.stats
    }

    fn log(&self, args: fmt::Arguments<'_>) {
        (self.cfg.log)(args);
    }
}

/// Is dit een IP-multicast-bestemming (01:00:5e IPv4, 33:33 IPv6)?
fn is_ip_multicast(p: &[u8]) -> bool {
    matches!(p, [0x01, 0x00, 0x5e, ..] | [0x33, 0x33, ..])
}

/// Bezorgt één frame op grond van de dst-MAC; meer switch is er niet.
/// Onbekende bestemming of volle ring = drop (Go: `forward`).
fn forward<R: Reader, W: Writer>(
    core: &mut Core<'_, R, W>,
    nat: &mut Nat,
    src: usize,
    p: &mut [u8],
    now: u64,
) {
    if p.len() < ETH_LEN || p.len() > MAX_LAN_FRAME {
        return;
    }
    // Bron-MAC-controle: een slot mag alleen zijn ÉIGEN MAC gebruiken. De
    // switch weet uit welke ring hij dit frame las en de nummering is
    // deterministisch, dus dit is een gratis feit, geen leertabel die te
    // vergiftigen valt. Zonder deze regel kan een slot ARP-antwoorden geven
    // namens een adres dat niet van hem is: precies de aanval die je niet
    // wilt op een node waar HOP toetsaanslagen rondstuurt. Poort 0 is HOP
    // zelf, de vertrouwde kant.
    if src >= 1 && (mac_at(p, 6) != slot_mac(src) || !valid_source_ip(src, p)) {
        return;
    }
    if byte(p, 0) & 1 != 0 {
        // Broadcast/multicast (ARP): iedereen behalve de bron.
        if arp_reply_gateway(core, src, p) {
            return; // who-has de gateway? HOP antwoordt zelf
        }
        for i in 1..=core.cfg.max_slots {
            if i != src && core.attached(i) {
                core.write_rx(i, KIND_FRAME, p);
            }
        }
        // IP-multicast (mDNS, matter, NDP) gaat óók het LAN op: de scope is
        // de hele link. Broadcast en het interne ARP-verkeer blijven binnen
        // (dat lekt anders de 10.100-namen).
        if is_ip_multicast(p) && nat.uplink().is_some() {
            core.uplink_tx(p);
        }
        return;
    }
    if mac_at(p, 0)[..5] != slot_mac(0)[..5] {
        // Geen switch-MAC. IPv6-unicast van een slot naar een LAN-buur gaat
        // als écht L2-frame de NIC op, mét de slot-MAC als bron: v6 kent
        // geen NAT-pad, NDP heeft de slot al als buur geadverteerd. IPv4
        // kent alleen de switch-MAC's (NAT is de uitweg).
        if be16(p, 12) == ET_IPV6 && nat.uplink().is_some() {
            core.uplink_tx(p);
        }
        return;
    }
    let dst = usize::from(byte(p, 5));
    if dst == 0 {
        if src != 0 {
            // Volgorde: eerst de gateway (10.100.0.1 = "mijn node"), dan het
            // antwoord van een gepubliceerde poort, anders masquerade.
            if gateway_claim(core, p) || nat.slot_reply(core, src, p, now) {
                return;
            }
            nat.outbound(core, src, p, now);
        }
        return;
    }
    if dst != src && dst <= core.cfg.max_slots && core.attached(dst) {
        core.write_rx(dst, KIND_FRAME, p);
    }
}

/// Bindt L3 aan dezelfde slotidentiteit als de bron-MAC. Daardoor is het
/// remote IP van een verbinding met 10.100.0.1 een betrouwbare capability
/// voor precies dat slot: een app kan niet de volumes of credentials van een
/// buur lenen door diens adres als bron te schrijven.
fn valid_source_ip(src: usize, p: &[u8]) -> bool {
    let want = slot_ip4(src);
    if p.len() >= ETH_LEN + 20 && be16(p, 12) == ET_IPV4 {
        return be32(p, ETH_LEN + 12) == want;
    }
    if p.len() >= ETH_LEN + 28 && be16(p, 12) == ET_ARP {
        return mac_at(p, ETH_LEN + 8) == slot_mac(src) && be32(p, ETH_LEN + 14) == want;
    }
    true
}

/// Hoort dit gateway-frame bij HOP's poort 0? Ja voor IPv4 naar het
/// gateway-IP. `true` = bezorgd.
fn gateway_claim<R: Reader, W: Writer>(core: &mut Core<'_, R, W>, p: &[u8]) -> bool {
    if !core.attached(0) || p.len() < ETH_LEN + 20 || be16(p, 12) != ET_IPV4 {
        return false;
    }
    if be32(p, ETH_LEN + 16) != host_ip4() {
        return false; // IPv4 naar elders: NAT-terrein
    }
    core.write_rx(0, KIND_FRAME, p);
    true
}

/// Beantwoordt een ARP-request voor de gateway namens HOP, in de RX-ring van
/// de vrager; `true` = afgehandeld. Andere ARP's (slot naar slot) worden
/// geflood; die beantwoordt het doelslot zelf.
fn arp_reply_gateway<R: Reader, W: Writer>(
    core: &mut Core<'_, R, W>,
    src: usize,
    p: &[u8],
) -> bool {
    if src < 1 || src > core.cfg.max_slots || !core.attached(src) {
        return false;
    }
    if p.len() < ETH_LEN + 28 || be16(p, 12) != ET_ARP {
        return false;
    }
    let a = ETH_LEN;
    // Ethernet/IPv4-request (oper = 1) naar het gateway-IP?
    if be16(p, a) != 1 || be16(p, a + 2) != 0x0800 || be16(p, a + 6) != 1 {
        return false;
    }
    if be32(p, a + 24) != host_ip4() {
        return false;
    }
    let sha = mac_at(p, a + 8);
    let spa = be32(p, a + 14);
    let mut r = [0u8; ETH_LEN + 28];
    put_mac(&mut r, 0, &sha);
    put_mac(&mut r, 6, &HOST_MAC);
    put16(&mut r, 12, ET_ARP);
    put16(&mut r, a, 1);
    put16(&mut r, a + 2, 0x0800);
    r[a + 4] = 6;
    r[a + 5] = 4;
    put16(&mut r, a + 6, 2); // reply
    put_mac(&mut r, a + 8, &HOST_MAC);
    crate::wire::put32(&mut r, a + 14, host_ip4());
    put_mac(&mut r, a + 18, &sha);
    crate::wire::put32(&mut r, a + 24, spa);
    core.write_rx(src, KIND_FRAME, &r);
    true
}

/// Eén frame van de uplink-NIC (Go: de lus in `Uplink.Receive`): multicast
/// floodt naar álle slots, IPv6-unicast op een slot-MAC gaat naar dat slot,
/// de NAT claimt de rest die van een app is, en wat overblijft is voor HOP's
/// eigen stack (poort 0, soort [`KIND_UPLINK`]).
fn uplink_in<R: Reader, W: Writer>(
    core: &mut Core<'_, R, W>,
    nat: &mut Nat,
    f: &mut [u8],
    now: u64,
) {
    if f.len() >= ETH_LEN && is_ip_multicast(f) {
        // HOP's eigen stack joint geen groepen; de slot-stacks filteren zelf
        // op lidmaatschap, dus dit is een kopie per aangesloten slot.
        for i in 1..=core.cfg.max_slots {
            core.deliver(i, f);
        }
        return;
    }
    let dst = usize::from(byte(f, 5));
    if f.len() >= ETH_LEN
        && be16(f, 12) == ET_IPV6
        && mac_at(f, 0)[..5] == slot_mac(0)[..5]
        && (1..=core.cfg.max_slots).contains(&dst)
    {
        // De terugweg van het IPv6-L2-pad: een LAN-buur antwoordt de slot
        // rechtstreeks op de MAC die NDP adverteerde.
        core.deliver(dst, f);
        return;
    }
    // ARP eerst, niet claimen: de node-stack wil replies óók zien.
    nat.arp_learn(f, now);
    if nat.inbound(core, f, now) {
        return;
    }
    core.write_rx(0, KIND_UPLINK, f);
}

#[cfg(test)]
mod tests;
