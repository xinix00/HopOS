//! Het netwerkvlak van de kern-binary: de RX-pomp op de NIC, de switch-actor,
//! HOP's poort 0 met de node-stack (`leannet`), de DHCP-lease en de
//! system-listener (Go: `hopnet.Up`, `hopswitch.Up` en `ServeSystem` uit
//! `OLD/metal/cmd/hopos/main.go`).
//!
//! Eigendom (PORT.md §3 en §4):
//!
//! - de pomp bezit de NIC; frames gaan als waarde door de ingress- en
//!   egress-rij (`net::pump`);
//! - de switch bezit de poorten, de NAT en de switch-kant van elke ring; wie
//!   iets wil, stuurt een [`Command`] naar [`COMMANDS`];
//! - de host-taak bezit HOP's twee ringen van poort 0: eerst rauw voor DHCP,
//!   daarna in de `HostPort` met de node-stack erachter;
//! - de system-listener bezit de listen-socket en geeft elke toegelaten
//!   verbinding (handvat plus [`Admitted`]) als waarde aan een vrije taak uit
//!   een vaste pool van [`SYSTEM_WORKERS`]; die taak bezit haar buffers en
//!   haar [`Reply`], en sluit het handvat zelf;
//! - de node-stack zelf staat in [`STACK`], een `LocalCell`. Dat is een
//!   bewuste afwijking van "een actor met berichten" (PORT.md §3 zegt: een
//!   `net.Conn` wordt een handvat met een rij): `leannet` is sans-I/O en elke
//!   call is synchroon en kort, dus de lening leeft binnen één call en nooit
//!   over een `.await` (de lint weigert de rest). De rij per verbinding komt
//!   als meting zegt dat de korte leningen de host-taak hinderen.
//!
//! Wat hier niet staat: de lifecycle achter de system-API (die krijgt de
//! listener als [`SystemApi`] van `main`), SNTP en DNS.

use abi::layout::{HOST_IP4, NET_MTU, NET_PREFIX, NET_RING_DATA_CAP};
use alloc::boxed::Box;
use alloc::vec::Vec;
use core::fmt;
use core::future::{Future, poll_fn};
use core::net::Ipv4Addr;
use core::sync::atomic::{AtomicU32, Ordering::Relaxed};
use core::task::{Context, Poll, Waker};
use core::time::Duration;
use cpu::println;
use dev::Pa;
use executor::{Clock, Executor};
use kern::cage::{Console, PhysMem, Timer};
use kern::slots::Reply;
use kern::system::{
    Admitted, Conn, End, Hooks, MAX_IO_CHUNK, MAX_PAYLOAD, MAX_SYSTEM_CONNS, PORT, System,
};
use leandhcp::{Action, Client, Instant, KeepAction, Keeper, Lease};
use leannet::{Endpoint, ListenHandle, Stack, TcpHandle, UdpHandle};
use net::host::{HostPort, HostStack};
use net::nat::Uplink;
use net::plan::MAX_LAN_FRAME;
use net::pump::Pump;
use net::ring::{AbiTx, KIND_UPLINK, Reader as _, Writer as _};
use net::switch::{self, Ack, Command, Commands, Published, Switch, Wiring};
use net::{Egress, Ingress, Stats};
use netdev::Device;
use sync::mpsc::Mailbox;
use sync::{LocalCell, Signal, Stop, select};

/// De leeskant van een ring zoals de switch en poort 0 hem zien.
type RingRx = AbiTx;
/// De schrijfkant.
type RingTx = abi::ring::Writer;

/// De meetlat van het netwerkvlak.
pub(crate) static STATS: Stats = Stats::new();
/// De brievenbus van de switch: `Attach`/`Detach` van de slots komen hier.
pub(crate) static COMMANDS: Commands<'static, RingRx, RingTx> = Mailbox::new();
/// De tabel voor de deur van de executor.
static PUBLISHED: Published<RingRx> = Published::new();
/// Pomp naar switch.
static INGRESS: Ingress = Ingress::new();
/// Switch naar pomp.
static EGRESS: Egress = Egress::new();
/// De deur van de switch: na elke schrijf in een TX-ring.
pub(crate) static DOOR: Signal = Signal::new();
/// De bel van de pomp: de switch zet hem na een ronde met egress-werk.
static PUMP_BELL: Signal = Signal::new();
/// De bel van de host-taak: de switch zet hem na een schrijf in poort 0, en
/// elke taak die de node-stack iets gaf (een write, een accept) ook.
static HOST_BELL: Signal = Signal::new();
/// De stop van de vier netwerktaken. Hij luidt nooit: de kern-flip die hem
/// ooit zet, bestaat nog niet. Vier wachters, dus de standaard `Stop<4>`.
static STOP: Stop = Stop::new();
/// De bevestiging van `SetUplink` na DHCP.
static UPLINK_ACK: Ack = Ack::new();
/// Het uplink-adres na de lease (big-endian als getal; 0 = nog geen lease).
/// Eén schrijver (de host-taak), gelezen door de plaatsing van Hop: die
/// zet het in `HOPOS_NODE_IP`, want de leader moet het endpoint zien dat
/// van buiten bereikbaar is, niet het slot-adres.
static UPLINK_IP: AtomicU32 = AtomicU32::new(0);
/// De DNS-server uit de lease (big-endian als getal; 0 = geen): Hop krijgt
/// hem in zijn env, want zonder resolver haalt hij niets op naam.
static UPLINK_DNS: AtomicU32 = AtomicU32::new(0);
/// De bevestiging van een `Publish`; de plaatsing van Hop is de enige
/// zender en wacht elke bevestiging af voor hij de volgende stuurt.
static PUBLISH_ACK: Ack = Ack::new();
/// De node-stack, gezet door de host-taak na de lease. Zie de moduledoc
/// voor waarom dit een `LocalCell` is en geen actor.
static STACK: LocalCell<Option<Stack>> = LocalCell::cell(None);

/// De datacapaciteit van elk van de twee host-ringen: die van een slot-ring,
/// zodat een burst van een app en een jumboframe (64 KB, hoogstens de halve
/// ring) altijd passen.
const HOST_RING_DATA: u64 = NET_RING_DATA_CAP;

/// Hoe lang één DHCP-poging mag duren. QEMU's user-net antwoordt binnen een
/// milliseconde; een LAN-server binnen een seconde. Tien seconden is ruim
/// genoeg voor een trage router en kort genoeg om het in de log te zien.
const DHCP_TIMEOUT: Duration = Duration::from_secs(10);

/// De wacht tussen twee mislukte DHCP-pogingen.
const DHCP_RETRY: Duration = Duration::from_secs(5);

/// Het bufferbudget van de node-stack. De Go-kern nam 1/8 van het RAM-raam
/// (30 MB op QEMU), maar de heap is hier nog een bump-allocator: wat een
/// verbinding teruggeeft, lekt. 8 MB is dus het plafond op wat tegelijk
/// leeft, niet op wat er in totaal verdwijnt; een echte allocator heft dat
/// op.
const NET_BUDGET: usize = 8 << 20;

/// De antwoordbuffer van een system-verbinding: de kop plus één I/O-brok,
/// want een `read` antwoordt met tot [`MAX_IO_CHUNK`] bytes (Hop leest zijn
/// staat in brokken van 1 MiB). De callbuffer is één frame van de grootste
/// payload ([`MAX_PAYLOAD`]). Beide gaan per bestandscall als waarde naar
/// de hopfs-actor en terug (`kern::rpc`); er wordt niets per call
/// gealloceerd.
const OUT_BUF: usize = kern::system::REQ_HEADER + MAX_IO_CHUNK;

/// Het totaalplafond op gelijktijdige system-verbindingen: de poolgrootte
/// van de verbindingstaken (handboek §2: een verbinding is een taak uit een
/// vaste pool). Per slot laat `admit` er [`MAX_SYSTEM_CONNS`] toe; dit is
/// dat maal de drie app-slots van QEMU virt, plus twee voor Hop. Elke taak
/// houdt een callbuffer van [`MAX_PAYLOAD`] (1 MiB plus 64 KiB) en een
/// antwoordbuffer van 1 MiB vast, dus 8 taken zijn ruim 17 MB van de 236 MB
/// heap; een board met meer slots
/// krijgt zijn weigering luid (`HOPOS_SYSTEM_FULL`) en tilt dit getal met
/// een meting op.
pub(crate) const SYSTEM_WORKERS: usize = 3 * MAX_SYSTEM_CONNS as usize + 2;

/// Weigeringen van de listener die een eigen regel krijgen; daarna tellen
/// we alleen (handboek §6: falen is luid, en één keer).
const LOUD_REFUSALS: u64 = 3;

/// De DNS-server uit de lease, of `None` zolang er geen lease is.
pub(crate) fn uplink_dns() -> Option<Ipv4Addr> {
    match UPLINK_DNS.load(Relaxed) {
        0 => None,
        ip => Some(Ipv4Addr::from(ip)),
    }
}

/// Het uplink-adres na de lease, of `None` zolang er geen lease is.
pub(crate) fn uplink_ip() -> Option<Ipv4Addr> {
    match UPLINK_IP.load(Relaxed) {
        0 => None,
        ip => Some(Ipv4Addr::from(ip)),
    }
}

/// Zet TCP-poort `port` van de uplink door naar dezelfde poort in `slot`
/// (DNAT in de switch, `Command::Publish`), zoals de Go-kern dat deed met
/// de poorten uit een jobspec. Wacht op de bevestiging van de switch.
pub(crate) async fn publish(slot: usize, port: u16) -> Result<(), net::Error> {
    let cmd = Command::Publish {
        proto: net::nat::Proto::Tcp,
        node_port: port,
        slot,
        slot_port: port,
        ack: &PUBLISH_ACK,
    };
    if COMMANDS.try_send(cmd).is_err() {
        return Err(net::Error::Full("switch mailbox", switch::COMMANDS));
    }
    PUBLISH_ACK.wait().await.map(|_| ())
}

/// De vaste instellingen van het netwerkvlak, uit `main`.
pub(crate) struct Params {
    /// Het hoogste slotnummer van dit board.
    pub(crate) max_slots: usize,
    /// De klok van het board (monotone nanoseconden).
    pub(crate) clock: Clock,
    /// De kick van slot `i` na een leeg-naar-niet-leeg-schrijf in zijn
    /// RX-ring (van de slot-lifecycle; zonder slots niets).
    pub(crate) slot_wake: fn(usize),
}

/// Wat de system-listener van de lifecycle-kant krijgt: de system-API en de
/// lijm die `serve` vraagt. De gedeelde delen zijn `static` (alle
/// verbindingstaken lenen ze); het geheugenhandvat krijgt elke taak als
/// eigen kloon.
pub(crate) struct SystemApi<const N: usize, M, H: 'static, C: 'static> {
    /// De system-API over de lifecycle-inbox en de servicers.
    pub(crate) system: &'static System<'static, 'static, N>,
    /// Eén antwoordplek per verbindingstaak: een antwoord van de actor
    /// landt nooit bij een andere verbinding.
    pub(crate) replies: &'static [Reply; SYSTEM_WORKERS],
    /// Fysiek geheugen voor de image-stream.
    pub(crate) mem: M,
    /// Klok en flip.
    pub(crate) hooks: &'static H,
    /// De console voor app-logregels.
    pub(crate) log: &'static C,
}

/// Waarom het netwerkvlak niet opkwam.
#[derive(Debug)]
pub(crate) enum Error {
    /// Een boot-allocatie faalde (bytes).
    OutOfMemory(usize),
    /// Een ring kon niet op zijn backing (`abi::ring`).
    Ring(abi::Error),
    /// De switch of de host-poort weigerde.
    Net(net::Error),
    /// Een rij was al gesplitst: `start` twee keer.
    Twice,
    /// De executor nam een taak niet aan.
    Spawn(&'static str),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::OutOfMemory(n) => write!(f, "out of memory allocating {n} bytes"),
            Self::Ring(e) => write!(f, "host ring: {e}"),
            Self::Net(e) => write!(f, "{e}"),
            Self::Twice => f.write_str("net started twice"),
            Self::Spawn(what) => write!(f, "spawn {what} refused"),
        }
    }
}

/// Zet het netwerkvlak op en spawnt zijn taken, in de volgorde van het
/// handboek §4 (de pompen eerst): de RX-pomp, de switch, de flow-expiry, de
/// host-taak. De host-taak haalt de lease, zet de stack op en spawnt dan de
/// DHCP-keeper en de system-listener.
pub(crate) fn start<D, const N: usize, M, H, C>(
    exec: &'static Executor,
    nic: D,
    p: Params,
    api: SystemApi<N, M, H, C>,
) -> Result<(), Error>
where
    D: Device + 'static,
    M: PhysMem + Clone + 'static,
    H: Hooks + 'static,
    C: Console + 'static,
{
    let mac = nic.mac().0;
    let (ing_tx, ing_rx) = INGRESS.split().ok_or(Error::Twice)?;
    let (eg_tx, eg_rx) = EGRESS.split().ok_or(Error::Twice)?;

    // Poort 0: twee ringen in de kern-heap. Ze zijn kern-intern (geen ABI,
    // geen app ziet ze), dus hoeven ze niet in een DMA-regio.
    let host_tx = ring_backing(HOST_RING_DATA)?; // HOP → switch
    let host_rx = ring_backing(HOST_RING_DATA)?; // switch → HOP
    let sw_tx = AbiTx::open(host_tx, HOST_RING_DATA).map_err(Error::Ring)?;
    let sw_rx = RingTx::open(host_rx, HOST_RING_DATA).map_err(Error::Ring)?;
    let hop_rx = AbiTx::open(host_rx, HOST_RING_DATA).map_err(Error::Ring)?;
    let hop_tx = RingTx::open(host_tx, HOST_RING_DATA).map_err(Error::Ring)?;

    let mut sw = Switch::new(
        switch::Config {
            max_slots: p.max_slots,
            clock: p.clock,
            log: log_line,
            slot_wake: p.slot_wake,
        },
        Wiring {
            commands: &COMMANDS,
            door: &DOOR,
            published: &PUBLISHED,
            stats: &STATS,
            host_bell: Some(&HOST_BELL),
            ingress: Some(ing_rx),
            egress: Some(eg_tx),
            pump_bell: Some(&PUMP_BELL),
        },
    )
    .map_err(Error::Net)?;
    sw.attach_host(sw_tx, sw_rx);

    let mut pump = Pump::new(nic, ing_tx, eg_rx, &PUMP_BELL, &DOOR, &STATS);
    let irq = pump.nic().irq().is_some();
    exec.spawn(async move { pump.run(exec, &STOP).await })
        .map_err(|_| Error::Spawn("pump"))?;
    println!(
        "net: pump on the nic ({}), uplink queues 2x{} HOPOS_NET_PUMP",
        if irq {
            "irq line, 10 ms guard"
        } else {
            "polled every 300 us"
        },
        net::UPLINK_QUEUE,
    );

    let mut sw_buf = boot_buf(MAX_LAN_FRAME)?;
    exec.spawn(async move { sw.run(exec, &mut sw_buf, &STOP).await })
        .map_err(|_| Error::Spawn("switch"))?;
    exec.spawn(switch::flow_expiry(exec, &COMMANDS, &STOP))
        .map_err(|_| Error::Spawn("flow expiry"))?;
    println!(
        "net: switch up, ports 0..={}, host rings 2x{} bytes HOPOS_SWITCH_UP",
        p.max_slots, HOST_RING_DATA
    );

    let host_buf = boot_buf(MAX_LAN_FRAME)?;
    let node = Node {
        exec,
        mac,
        max_slots: p.max_slots,
        rx: hop_rx,
        tx: hop_tx,
        buf: host_buf,
    };
    exec.spawn(node.run(api))
        .map_err(|_| Error::Spawn("host"))?;
    Ok(())
}

/// De console als `LogFn` voor de switch.
fn log_line(args: fmt::Arguments<'_>) {
    println!("{args}");
}

/// Eén cacheline: de eenheid van de host-ringen. Kop, staart en rand delen
/// zo nooit een regel met ander heap-geheugen, en dat moet: `dev::pull`
/// invalideert hele regels.
#[derive(Clone, Copy)]
#[repr(C, align(64))]
struct Line([u8; 64]);

/// Een ring-backing met `data` bytes datacapaciteit in de kern-heap,
/// klaargezet met `abi::ring::init`. Hij leeft voor altijd: de heap geeft
/// hem nooit terug en niemand buiten `dev` raakt hem nog aan.
fn ring_backing(data: u64) -> Result<Pa, Error> {
    let bytes = usize::try_from(abi::ring::DATA_OFF + data).map_err(|_| Error::OutOfMemory(0))?;
    let lines = bytes.div_ceil(core::mem::size_of::<Line>());
    let mut v: Vec<Line> = Vec::new();
    v.try_reserve_exact(lines)
        .map_err(|_| Error::OutOfMemory(bytes))?;
    v.resize(lines, Line([0; 64]));
    // Uit de handen van Rust: vanaf hier is het geheugen van de ring, en de
    // ring praat alleen via `dev` met een adres (de kern-RAM is identity
    // gemapt, dus het virtuele adres is het fysieke).
    let raw = Box::into_raw(v.into_boxed_slice());
    let pa = Pa(raw.cast::<Line>().expose_provenance() as u64);
    abi::ring::init(pa, data).map_err(Error::Ring)?;
    Ok(pa)
}

/// Een framebuffer uit de heap (boot): te groot voor de stack van een taak.
fn boot_buf(n: usize) -> Result<Vec<u8>, Error> {
    let mut v = Vec::new();
    v.try_reserve_exact(n).map_err(|_| Error::OutOfMemory(n))?;
    v.resize(n, 0);
    Ok(v)
}

// ---------------------------------------------------------------------------
// De host-taak: eerst DHCP rauw over de ringen, dan de stack in poort 0.
// ---------------------------------------------------------------------------

/// De host-taak met zijn eigendom: de twee ringen van poort 0 en zijn
/// framebuffer.
struct Node {
    exec: &'static Executor,
    mac: [u8; 6],
    max_slots: usize,
    rx: RingRx,
    tx: RingTx,
    buf: Vec<u8>,
}

impl Node {
    /// De levensloop: lease, uplink, stack, keeper en listener, en dan de
    /// lus van poort 0 tot de stop.
    async fn run<const N: usize, M, H, C>(mut self, api: SystemApi<N, M, H, C>)
    where
        M: PhysMem + Clone + 'static,
        H: Hooks + 'static,
        C: Console + 'static,
    {
        let exec = self.exec;
        let lease = self.lease().await;
        let cidr = lease.cidr();
        let ip = u32::from(lease.ip);
        let mac = self.mac;

        // De switch moet het externe adres kennen vóór de stack praat: pas
        // dan scheidt de NAT gepubliceerde poorten en masquerade-flows van
        // verkeer voor de node zelf.
        match Uplink::new(ip, cidr.prefix, mac) {
            Ok(uplink) => {
                let cmd = Command::SetUplink {
                    uplink,
                    ack: &UPLINK_ACK,
                };
                if COMMANDS.try_send(cmd).is_ok() {
                    if let Err(e) = UPLINK_ACK.wait().await {
                        println!("net: switch refused the uplink: {e} HOPOS_NET_FAIL");
                    }
                } else {
                    println!("net: switch mailbox full, uplink not set HOPOS_NET_FAIL");
                }
            }
            Err(e) => println!("net: uplink /{}: {e} HOPOS_NET_FAIL", cidr.prefix),
        }

        let stack = match Stack::new(stack_config(&lease, mac), iss_seed(exec, mac)) {
            Ok(s) => s,
            Err(e) => {
                println!("net: node stack: {e} HOPOS_NET_FAIL");
                return;
            }
        };
        *STACK.borrow_mut() = Some(stack);
        UPLINK_IP.store(ip, Relaxed);
        UPLINK_DNS.store(u32::from(lease.dns), Relaxed);
        println!(
            "net: {} (mac {}, gw {}) HOPOS_NET_UP",
            lease.ip,
            netdev::Mac(mac),
            lease.gateway
        );

        if exec.spawn(keep_lease(exec, mac, lease)).is_err() {
            println!("net: dhcp keeper not spawned, the lease will lapse HOPOS_DHCP_LOST");
        }
        if exec.spawn(system_listener(exec, lease.ip, api)).is_err() {
            println!("system: listener not spawned HOPOS_SYSTEM_FAIL");
        }

        let Node {
            rx,
            tx,
            mut buf,
            max_slots,
            ..
        } = self;
        let node = NodeStack { exec };
        match HostPort::new(node, rx, tx, &HOST_BELL, &DOOR, &STATS, mac, ip, max_slots) {
            Ok(mut port) => port.run(exec, &mut buf, &STOP).await,
            Err(e) => println!("net: host port: {e} HOPOS_NET_FAIL"),
        }
    }

    /// Haalt een lease, en blijft het proberen: een node zonder adres kan
    /// niets, maar de rest van de kern draait door.
    async fn lease(&mut self) -> Lease {
        let mut attempt: u32 = 0;
        loop {
            attempt = attempt.saturating_add(1);
            let t0 = self.exec.now();
            match self.dhcp().await {
                Ok(l) => {
                    let ms = self.exec.now().saturating_sub(t0) / 1_000_000;
                    println!(
                        "dhcp: lease {} from {}, dns {}, {} s, attempt {attempt}, {ms} ms HOPOS_DHCP_BOUND",
                        l.cidr(),
                        l.server,
                        l.dns,
                        l.lease_secs,
                    );
                    return l;
                }
                Err(e) => {
                    println!("{e}, attempt {attempt}, retry in 5 s HOPOS_DHCP_FAIL");
                    self.exec.after(DHCP_RETRY).await;
                }
            }
        }
    }

    /// Eén DHCP-poging over de rauwe ringen van poort 0: zonder uplink-adres
    /// geeft de switch alles van de draad aan ons (`KIND_UPLINK`), en wat wij
    /// als `KIND_UPLINK` schrijven gaat de draad op. Dat is Go's
    /// `hopnet.Up`-volgorde: eerst de lease, dan de stack.
    async fn dhcp(&mut self) -> leandhcp::Result<Lease> {
        let exec = self.exec;
        let now = || Instant::from_duration(Duration::from_nanos(exec.now()));
        let mut c = Client::new(self.mac, now(), DHCP_TIMEOUT);
        loop {
            // Eén frame toevoeren en dan pollen, zoals de client vraagt.
            let fed = match self.rx.read_into(&mut self.buf) {
                Some((KIND_UPLINK, n)) => {
                    if let Some(f) = self.buf.get(..n) {
                        c.receive(f, now());
                    }
                    true
                }
                Some(_) => true, // Geen uplink-frame: niet voor de client.
                None => false,
            };
            let step = match c.poll(now())? {
                Action::Transmit(f) => Step::Sent(self.tx.write_notify(KIND_UPLINK, f).is_some()),
                Action::Wait(until) => Step::Wait(until),
                Action::Bound(l) => return Ok(l),
            };
            match step {
                Step::Sent(true) => DOOR.set(),
                Step::Sent(false) => c.transmit_failed(),
                // Lag er een frame, dan eerst de ring leeg.
                Step::Wait(_) if fed => {}
                Step::Wait(until) => {
                    let at = u64::try_from(until.as_duration().as_nanos()).unwrap_or(u64::MAX);
                    let _ = select(HOST_BELL.wait(), exec.until(at)).await;
                }
            }
        }
    }
}

/// Wat de DHCP-lus na een poll doet; los van de lening op de client.
enum Step {
    /// Het frame ging de ring in (of niet).
    Sent(bool),
    /// Wachten tot dit tijdstip of een frame.
    Wait(Instant),
}

/// De stack-instellingen uit de lease, zoals Go's `stackUp`: het adres van
/// de uplink, maar het device is ook het slot-LAN, en dáár gelden de
/// jumbo-MTU en de vertrouwde link (de ringen zijn geheugen).
fn stack_config(lease: &Lease, mac: [u8; 6]) -> leannet::Config {
    let prefix = u8::try_from(lease.cidr().prefix).unwrap_or(32);
    leannet::Config {
        ip: lease.ip.octets(),
        prefix,
        mac,
        gw: lease.gateway.octets(),
        budget: NET_BUDGET,
        // Het venster naar een app blijft onder de halve slot-ring, anders
        // loopt die vol zodra HOP sneller is dan de pomp van de app (Go,
        // 04-09: 128x rx-full bij vier hameraars).
        max_buf_per_conn: usize::try_from(NET_RING_DATA_CAP / 2).unwrap_or(0),
        adv_ws: ws_shift_for(NET_BUDGET / 4),
        mtu: NET_MTU,
        mtu_net: HOST_IP4.to_be_bytes(),
        mtu_prefix: u8::try_from(NET_PREFIX).unwrap_or(24),
        link_trusted: true,
    }
}

/// De kleinste window-scale-shift die een venster van `max` bytes draagt
/// (RFC 7323, plafond 14).
fn ws_shift_for(max: usize) -> u8 {
    let mut shift = 0u8;
    while shift < 14 && (0xffff_usize << shift) < max {
        shift += 1;
    }
    shift
}

/// De ISS-seed: niet kryptografisch, wel per boot anders. Er is nog geen
/// wandklok, dus de teller sinds boot plus de MAC (Go deed de boot-tijd).
fn iss_seed(exec: &Executor, mac: [u8; 6]) -> u32 {
    let m = u32::from_be_bytes([mac[2], mac[3], mac[4], mac[5]]);
    // Afkappen is hier de bedoeling: alleen de lage bits variëren.
    (exec.now() as u32) ^ m.rotate_left(13)
}

// ---------------------------------------------------------------------------
// De node-stack achter poort 0.
// ---------------------------------------------------------------------------

/// De `HostStack` van poort 0 over de `leannet::Stack` in [`STACK`].
struct NodeStack {
    exec: &'static Executor,
}

impl HostStack for NodeStack {
    fn receive(&mut self, frame: &[u8]) {
        let now = self.exec.now();
        // Een frame dat de stack weigert (stack dicht) telt als host-drop;
        // wat hij zelf weggooit, telt hij zelf.
        if on_stack(|st| st.receive(frame, now)).is_err() {
            STATS.host_rx_drops.fetch_add(1, Relaxed);
        }
    }

    fn poll_transmit(&mut self, buf: &mut [u8]) -> Option<usize> {
        let now = self.exec.now();
        on_stack(|st| Ok(st.poll_transmit(now, buf))).ok().flatten()
    }

    fn poll_at(&self) -> Option<u64> {
        let now = self.exec.now();
        on_stack(|st| Ok(st.next_timeout(now))).ok().flatten()
    }
}

/// Eén korte lening op de stack, zonder `.await`. Geen stack (nog niet, of
/// een lening die al loopt) is `StackClosed`: de aanroeper probeert het na
/// een bel opnieuw.
fn on_stack<T>(f: impl FnOnce(&mut Stack) -> leannet::Result<T>) -> leannet::Result<T> {
    let Ok(mut cell) = STACK.try_borrow_mut() else {
        return Err(leannet::Error::StackClosed);
    };
    match cell.as_mut() {
        Some(st) => f(st),
        None => Err(leannet::Error::StackClosed),
    }
}

/// Een socket-call van buiten de host-taak: na succes de bel van de
/// host-taak, want een write, read of close kan uitgaand werk maken (data,
/// een ACK, een vensterupdate, een FIN).
fn io<T>(f: impl FnOnce(&mut Stack) -> leannet::Result<T>) -> leannet::Result<T> {
    let r = on_stack(f);
    if r.is_ok() {
        HOST_BELL.set();
    }
    r
}

/// Probeert `op`; bij `WouldBlock` registreert `park` de waker van deze
/// taak op het handvat en is het `Pending`. Registreren en opnieuw kijken
/// gebeuren in dezelfde lening, dus een wek kan er niet tussendoor vallen.
fn poll_stack<T>(
    exec: &Executor,
    cx: &mut Context<'_>,
    op: impl FnOnce(&mut Stack, u64) -> leannet::Result<T>,
    park: impl FnOnce(&mut Stack, &Waker) -> leannet::Result,
) -> Poll<leannet::Result<T>> {
    let now = exec.now();
    let r = io(|st| match op(st, now) {
        Err(leannet::Error::WouldBlock) => {
            park(st, cx.waker())?;
            Err(leannet::Error::WouldBlock)
        }
        r => r,
    });
    match r {
        Err(leannet::Error::WouldBlock) => Poll::Pending,
        r => Poll::Ready(r),
    }
}

// ---------------------------------------------------------------------------
// De DHCP-keeper: de lease in leven houden over UDP op de stack.
// ---------------------------------------------------------------------------

/// De keeper-taak (Go: `keepLease`): RENEW op T1, REBIND op T2, luid als het
/// adres weg is. Zodra de stack de ringen bezit, spreekt de keeper UDP op
/// poort 68 en zendt de stack.
async fn keep_lease(exec: &'static Executor, mac: [u8; 6], lease: Lease) {
    let now = || Instant::from_duration(Duration::from_nanos(exec.now()));
    let h = match io(|st| st.udp_bind(leandhcp::CLIENT_PORT)) {
        Ok(h) => h,
        Err(e) => {
            println!(
                "dhcp: bind udp {}: {e}, the lease will lapse HOPOS_DHCP_LOST",
                leandhcp::CLIENT_PORT
            );
            return;
        }
    };
    let mut k = Keeper::new(mac, lease, now());
    let mut out = [0u8; leannet::UDP_MAX_PAYLOAD];
    let mut inb = [0u8; leannet::UDP_MAX_PAYLOAD];
    loop {
        let step = match k.poll(now()) {
            Ok(KeepAction::Send { to, payload }) => {
                let n = payload.len().min(out.len());
                if let (Some(d), Some(s)) = (out.get_mut(..n), payload.get(..n)) {
                    d.copy_from_slice(s);
                }
                Keep::Send(to, n)
            }
            Ok(KeepAction::Wait(until)) => Keep::Wait(until),
            Ok(KeepAction::Event(e)) => {
                println!("{e}");
                continue;
            }
            Ok(KeepAction::Done) => {
                println!("dhcp: lease without end, nothing to keep");
                return;
            }
            Err(e) => {
                println!("{e} HOPOS_DHCP_LOST");
                return;
            }
        };
        match step {
            Keep::Send(to, n) => {
                let dst = Endpoint {
                    ip: to.octets(),
                    port: leandhcp::SERVER_PORT,
                };
                let t = exec.now();
                let data = out.get(..n).unwrap_or(&[]);
                if io(|st| st.udp_send_to(h, dst, data, t)).is_err() {
                    k.transmit_failed(now());
                }
            }
            Keep::Wait(until) => {
                let at = u64::try_from(until.as_duration().as_nanos()).unwrap_or(u64::MAX);
                let _ = select(udp_readable(exec, h), exec.until(at)).await;
                let t = exec.now();
                while let Ok((n, _)) = io(|st| st.udp_recv_from(h, &mut inb, t)) {
                    if let Some(p) = inb.get(..n) {
                        k.receive(p, now());
                    }
                }
            }
        }
    }
}

/// Wat de keeper na een poll doet; los van de lening op de keeper.
enum Keep {
    /// Zend `n` bytes uit de zendbuffer naar deze server.
    Send(Ipv4Addr, usize),
    /// Wachten tot dit tijdstip of een datagram.
    Wait(Instant),
}

/// Wacht tot er een datagram op `h` ligt.
fn udp_readable(exec: &'static Executor, h: UdpHandle) -> impl Future<Output = ()> {
    poll_fn(move |cx| {
        let r = poll_stack(
            exec,
            cx,
            |st, _| match st.udp_readable(h) {
                Ok(true) => Ok(()),
                Ok(false) => Err(leannet::Error::WouldBlock),
                Err(e) => Err(e),
            },
            |st, w| st.udp_register_read_waker(h, w),
        );
        r.map(|_| ())
    })
}

// ---------------------------------------------------------------------------
// De system-listener.
// ---------------------------------------------------------------------------

/// Eén toegelaten verbinding, zoals de listener hem aan een taak geeft: het
/// handvat, het bronadres en de toelating (die in haar `Drop` de plaats bij
/// het slot teruggeeft).
struct Job {
    h: TcpHandle,
    remote: u32,
    who: Admitted<'static>,
}

/// De deur van één verbindingstaak.
enum Seat {
    /// De taak wacht op werk.
    Free,
    /// De listener gaf een verbinding; de taak haalt hem op.
    Handed(Job),
    /// De taak dient een verbinding.
    Busy,
}

/// De deur van één verbindingstaak: de listener schrijft `Free` naar
/// `Handed` en luidt de bel; de taak neemt het werk en zet `Busy`, en na
/// de verbinding weer `Free`. Een leesbare tabel (handboek §1.1): elke
/// lening is één statement.
struct Door {
    seat: LocalCell<Seat>,
    bell: Signal,
}

impl Door {
    const fn new() -> Door {
        Door {
            seat: LocalCell::cell(Seat::Free),
            bell: Signal::new(),
        }
    }
}

/// De deuren van de pool, één per verbindingstaak.
static DOORS: [Door; SYSTEM_WORKERS] = [const { Door::new() }; SYSTEM_WORKERS];

/// De klok van de executor als `kern::cage::Timer`, voor het toezicht op
/// een verbinding (`LIFE_TICK`, `IDLE_TIMEOUT`).
struct NetTimer(&'static Executor);

impl Timer for NetTimer {
    fn now(&self) -> u64 {
        self.0.now()
    }
    fn sleep(&self, d: Duration) -> impl Future<Output = ()> {
        self.0.after(d)
    }
}

/// De listener op [`PORT`] (Go: `ServeSystem`): per verbinding `admit`
/// (het slot uit het bron-IP, een levende servicer, hooguit
/// [`MAX_SYSTEM_CONNS`]) en dan de verbinding naar een vrije taak uit de
/// pool. De listener zelf leest nooit van een verbinding, dus hij staat
/// altijd weer bij `accept`: een zwijgende peer houdt niemand op.
async fn system_listener<const N: usize, M, H, C>(
    exec: &'static Executor,
    ip: Ipv4Addr,
    api: SystemApi<N, M, H, C>,
) where
    M: PhysMem + Clone + 'static,
    H: Hooks + 'static,
    C: Console + 'static,
{
    let l = match io(|st| st.tcp_listen(PORT)) {
        Ok(l) => l,
        Err(e) => {
            println!("system: listen on {PORT}: {e} HOPOS_SYSTEM_FAIL");
            return;
        }
    };
    let workers = spawn_workers(exec, &api);
    if workers == 0 {
        println!("system: no connection tasks, listener closed HOPOS_SYSTEM_FAIL");
        io(|st| {
            st.tcp_listen_close(l);
            Ok(())
        })
        .ok();
        return;
    }
    println!(
        "system: listening on {ip}:{PORT} and {}:{PORT}, {workers} connection tasks HOPOS_SYSTEM_UP",
        Ipv4Addr::from(HOST_IP4)
    );
    let (mut served, mut refused, mut full) = (0u64, 0u64, 0u64);
    loop {
        let h = match accept(exec, l).await {
            Ok(h) => h,
            Err(e) => {
                println!("system: accept on {PORT}: {e}, listener closed HOPOS_SYSTEM_FAIL");
                return;
            }
        };
        let remote = io(|st| st.tcp_remote(h)).map_or(0, |ep| u32::from_be_bytes(ep.ip));
        let Some(who) = api.system.admit(remote) else {
            refused = refused.wrapping_add(1);
            if refused <= LOUD_REFUSALS {
                println!(
                    "system: {} refused (no live slot behind it, or {MAX_SYSTEM_CONNS} open), {refused} so far HOPOS_SYSTEM_REFUSED",
                    Ipv4Addr::from(remote)
                );
            }
            close(exec, h);
            continue;
        };
        match hand(Job { h, remote, who }) {
            Ok(i) => {
                served = served.wrapping_add(1);
                if let Some(d) = DOORS.get(i) {
                    d.bell.set();
                }
            }
            Err(job) => {
                // Alle taken bezet: de toelating gaat met de job terug.
                full = full.wrapping_add(1);
                if full <= LOUD_REFUSALS {
                    println!(
                        "system: {} refused, all {SYSTEM_WORKERS} connection tasks busy, {full} so far, {served} served HOPOS_SYSTEM_FULL",
                        Ipv4Addr::from(job.remote)
                    );
                }
                close(exec, job.h);
            }
        }
    }
}

/// Geeft `job` aan de eerste vrije taak; geeft haar index, of de job terug
/// als alles bezet is. Eén lening per deur.
fn hand(job: Job) -> Result<usize, Job> {
    for (i, d) in DOORS.iter().enumerate() {
        let Ok(mut seat) = d.seat.try_borrow_mut() else {
            continue;
        };
        if matches!(*seat, Seat::Free) {
            *seat = Seat::Handed(job);
            return Ok(i);
        }
    }
    Err(job)
}

/// Spawnt de verbindingstaken, elk met haar eigen buffers (boot: de heap
/// geeft ze eenmalig) en haar eigen antwoordplek. Geeft hoeveel er draaien.
fn spawn_workers<const N: usize, M, H, C>(
    exec: &'static Executor,
    api: &SystemApi<N, M, H, C>,
) -> usize
where
    M: PhysMem + Clone + 'static,
    H: Hooks + 'static,
    C: Console + 'static,
{
    let mut n = 0;
    for (i, reply) in api.replies.iter().enumerate() {
        let (Ok(buf), Ok(out)) = (boot_buf(MAX_PAYLOAD), boot_buf(OUT_BUF)) else {
            println!(
                "system: no call buffers ({MAX_PAYLOAD} bytes) for task {i} HOPOS_SYSTEM_FAIL"
            );
            break;
        };
        let w = Worker {
            exec,
            door: i,
            system: api.system,
            reply,
            mem: api.mem.clone(),
            hooks: api.hooks,
            log: api.log,
            buf,
            out,
        };
        if exec.spawn(w.run()).is_err() {
            println!("system: connection task {i} not spawned HOPOS_SYSTEM_FAIL");
            break;
        }
        n += 1;
    }
    n
}

/// Eén verbindingstaak met haar eigendom: buffers, antwoordplek en
/// geheugenhandvat. Ze dient de ene verbinding na de andere, nooit twee
/// tegelijk.
struct Worker<const N: usize, M, H: 'static, C: 'static> {
    exec: &'static Executor,
    door: usize,
    system: &'static System<'static, 'static, N>,
    reply: &'static Reply,
    mem: M,
    hooks: &'static H,
    log: &'static C,
    buf: Vec<u8>,
    out: Vec<u8>,
}

impl<const N: usize, M: PhysMem, H: Hooks, C: Console> Worker<N, M, H, C> {
    async fn run(mut self) {
        let Some(door) = DOORS.get(self.door) else {
            return;
        };
        let timer = NetTimer(self.exec);
        loop {
            door.bell.wait().await;
            let seat = core::mem::replace(&mut *door.seat.borrow_mut(), Seat::Busy);
            let Seat::Handed(job) = seat else {
                // Een bel zonder werk (samengevoegd): de deur blijft zoals hij was.
                *door.seat.borrow_mut() = seat;
                continue;
            };
            let mut conn = TcpConn {
                exec: self.exec,
                h: job.h,
                remote: job.remote,
            };
            let end = self
                .system
                .serve(
                    &mut conn,
                    &job.who,
                    self.reply,
                    &timer,
                    &mut self.mem,
                    self.hooks,
                    self.log,
                    &mut self.buf,
                    &mut self.out,
                )
                .await;
            let slot = job.who.slot();
            match end {
                End::Peer => println!("system: slot {slot} done"),
                End::Evicted => println!("system: slot {slot} closed: {end} HOPOS_SYSTEM_EVICTED"),
                End::Idle | End::Failed(_) => println!("system: slot {slot} closed: {end}"),
            }
            // Eerst het handvat dicht, dan de plaats terug (de `Drop` van
            // de toelating): een nieuwe verbinding van het slot vindt de
            // oude nooit nog half open.
            close(self.exec, job.h);
            drop(job);
            *door.seat.borrow_mut() = Seat::Free;
        }
    }
}

/// Sluit een handvat; een al gesloten handvat is geen fout.
fn close(exec: &Executor, h: TcpHandle) {
    let t = exec.now();
    let _ = io(|st| st.tcp_close(h, t));
}

/// Wacht op de volgende verbinding van de listener.
fn accept(
    exec: &'static Executor,
    l: ListenHandle,
) -> impl Future<Output = leannet::Result<TcpHandle>> {
    poll_fn(move |cx| {
        poll_stack(
            exec,
            cx,
            |st, now| st.tcp_accept(l, now),
            |st, w| st.listen_register_waker(l, w),
        )
    })
}

/// Een TCP-verbinding op de node-stack als `kern::system::Conn`: een
/// handvat plus wakers, geen buffer van zichzelf.
struct TcpConn {
    exec: &'static Executor,
    h: TcpHandle,
    remote: u32,
}

impl Conn for TcpConn {
    fn read(&mut self, buf: &mut [u8]) -> impl Future<Output = kern::Result<usize>> {
        let (exec, h) = (self.exec, self.h);
        poll_fn(move |cx| {
            poll_stack(
                exec,
                cx,
                |st, now| st.tcp_read(h, buf, now),
                |st, w| st.tcp_register_read_waker(h, w),
            )
            .map(|r| r.map_err(|_| kern::Error::Conn))
        })
    }

    fn write(&mut self, buf: &[u8]) -> impl Future<Output = kern::Result<usize>> {
        let (exec, h) = (self.exec, self.h);
        poll_fn(move |cx| {
            poll_stack(
                exec,
                cx,
                |st, now| st.tcp_write(h, buf, now),
                |st, w| st.tcp_register_write_waker(h, w),
            )
            .map(|r| r.map_err(|_| kern::Error::Conn))
        })
    }

    fn remote_ip4(&self) -> u32 {
        self.remote
    }
}
