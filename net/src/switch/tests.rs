//! De switch-tests, naam voor naam uit `gateway_test.go`,
//! `hopswitch_ipv6_test.go` en `backpressure_test.go`, plus de lus zelf over
//! de executor. De ringen zijn host-buffers (`ring::mem`).

use super::*;
use crate::nat::Proto;
use crate::ring::mem::{self, MemReader, MemWriter};
use crate::wire::testutil::*;
use crate::wire::{PROTO_TCP, put16};
use crate::{Egress, Ingress};
use std::cell::{Cell, RefCell};

const NODE_IP: u32 = 0x0A00_020F;
const GW_IP: u32 = 0x0A00_0202;
const EXT_IP: u32 = 0x5DB8_D822;
const GW_MAC0: [u8; 6] = [0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01];
const NIC_MAC: [u8; 6] = [0x52, 0x54, 0x00, 0x12, 0x34, 0x56];
const RING: usize = 256 << 10; // ≥ 2× de grootste LAN-frame

thread_local! {
    static NOW: Cell<u64> = const { Cell::new(1_000_000_000_000) };
    /// Elke kloklezing schuift zoveel op: 0 = stilstaande klok.
    static TICK: Cell<u64> = const { Cell::new(0) };
    static WAKES: RefCell<Vec<usize>> = const { RefCell::new(Vec::new()) };
}

fn fake_now() -> u64 {
    NOW.with(|n| {
        let t = n.get() + TICK.with(Cell::get);
        n.set(t);
        t
    })
}

/// Slot 7 woont in deze tests op de OS-core (zoals Hop in slot 1 op de
/// node): zijn consument kan niet lezen zolang de switch draait.
fn slot_seven_is_resident(i: usize) -> bool {
    i == 7
}

fn record_wake(i: usize) {
    WAKES.with(|w| w.borrow_mut().push(i));
}

fn wakes(slot: usize) -> usize {
    WAKES.with(|w| w.borrow().iter().filter(|&&i| i == slot).count())
}

fn no_log(_: fmt::Arguments<'_>) {}

/// De neighbour-tabel van de node-stack kent hier alleen de gateway: de
/// tabel zelf toetsen leannet en de NAT-toetsen.
const NEIGHBORS: Neighbors = Neighbors {
    resolve: |_, _| Some(GW_MAC0),
    probe: |_, _| {},
    confirm: |_, _, _| {},
};

type Sw = Switch<'static, MemReader, MemWriter>;

struct H {
    sw: Sw,
    commands: &'static Commands<'static, MemReader, MemWriter>,
    door: &'static Signal,
    published: &'static Published<MemReader>,
    stats: &'static Stats,
    host_bell: &'static Signal,
    to_switch: sync::spsc::Sender<'static, Frame, UPLINK_QUEUE>,
    from_switch: sync::spsc::Receiver<'static, Frame, UPLINK_QUEUE>,
}

fn leak<T>(v: T) -> &'static T {
    Box::leak(Box::new(v))
}

fn harness() -> H {
    let commands = leak(Commands::new());
    let door = leak(Signal::new());
    let published = leak(Published::new());
    let stats = leak(Stats::new());
    let host_bell = leak(Signal::new());
    let pump_bell = leak(Signal::new());
    let (to_switch, ingress) = leak(Ingress::new()).split().unwrap();
    let (egress, from_switch) = leak(Egress::new()).split().unwrap();
    let sw = Switch::new(
        Config {
            max_slots: SLOT_CAP,
            clock: fake_now,
            log: no_log,
            slot_wake: record_wake,
            resident: slot_seven_is_resident,
            neighbors: NEIGHBORS,
        },
        Wiring {
            commands,
            door,
            published,
            stats,
            host_bell: Some(host_bell),
            ingress: Some(ingress),
            egress: Some(egress),
            pump_bell: Some(pump_bell),
        },
    )
    .unwrap();
    H {
        sw,
        commands,
        door,
        published,
        stats,
        host_bell,
        to_switch,
        from_switch,
    }
}

/// De app-kant van een slot: schrijven in TX, lezen uit RX.
struct App {
    tx: MemWriter,
    rx: MemReader,
}

impl H {
    fn attach(&mut self, i: usize) -> App {
        let (tx_r, tx_w) = mem::pair(RING);
        let (rx_r, rx_w) = mem::pair(RING);
        self.sw.attach(i, tx_r, rx_w).unwrap();
        App { tx: tx_w, rx: rx_r }
    }

    fn host(&mut self) -> App {
        let (tx_r, tx_w) = mem::pair(RING);
        let (rx_r, rx_w) = mem::pair(RING);
        self.sw.attach_host(tx_r, rx_w);
        App { tx: tx_w, rx: rx_r }
    }

    fn uplink(&mut self) {
        self.sw
            .nat()
            .set_uplink(Uplink::new(NODE_IP, 24, NIC_MAC, GW_IP).unwrap());
    }

    fn forward_once(&mut self, src: usize, p: &[u8]) {
        let mut f = p.to_vec();
        forward(&mut self.sw.core, &mut self.sw.nat, src, &mut f, fake_now());
    }

    fn sent(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        while let Some(f) = self.from_switch.try_recv() {
            out.push(f.bytes().to_vec());
        }
        out
    }

    fn pass(&mut self) -> bool {
        let mut buf = vec![0u8; MAX_LAN_FRAME];
        self.sw.switch_pass(&mut buf)
    }
}

#[test]
fn gateway_ip_gaat_lan_poort_nul_in() {
    let mut h = harness();
    h.uplink();
    let mut host = h.host();
    let f = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        host_ip4(),
        5555,
        9080,
        &[],
    );
    h.forward_once(1, &f);
    assert_eq!(host.rx.frame().as_deref(), Some(&f[..]));
    assert!(
        h.sent().is_empty(),
        "frame voor de gateway lekte de NIC uit"
    );
    assert!(h.host_bell.is_set(), "host-taak niet gewekt");
}

#[test]
fn ingress_framegrens_voor_lan_ringen() {
    let mut h = harness();
    let mut victim = h.attach(2);
    let mut host = h.host();
    let mut exact = vec![0u8; MAX_LAN_FRAME];
    exact[0..6].copy_from_slice(&slot_mac(2));
    exact[6..12].copy_from_slice(&slot_mac(1));
    h.forward_once(1, &exact);
    assert_eq!(victim.rx.frame().map(|f| f.len()), Some(MAX_LAN_FRAME));

    let mut oversized = exact.clone();
    oversized.push(0);
    h.forward_once(1, &oversized);
    assert!(
        victim.rx.frame().is_none(),
        "oversized frame bereikte de buurring"
    );
    assert!(
        victim.rx.corrupt().is_none(),
        "oversized frame maakte de buurring corrupt"
    );

    let mut to_gw = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        host_ip4(),
        5555,
        9080,
        &[],
    );
    to_gw.resize(MAX_LAN_FRAME + 1, 0);
    h.forward_once(1, &to_gw);
    assert!(host.rx.frame().is_none(), "oversized frame in poort 0");

    h.forward_once(1, &exact[..ETH_LEN]);
    assert_eq!(
        victim.rx.frame().map(|f| f.len()),
        Some(ETH_LEN),
        "ring werkte niet meer na reject"
    );
}

#[test]
fn extern_blijft_masquerade() {
    let mut h = harness();
    h.uplink();
    let mut host = h.host();
    let f = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        EXT_IP,
        5555,
        443,
        &[],
    );
    h.forward_once(1, &f);
    assert!(
        host.rx.frame().is_none(),
        "extern verkeer belandde op poort 0"
    );
    assert_eq!(h.sent().len(), 1, "extern verkeer niet gemasqueradeerd");
}

#[test]
fn host_poort_antwoord_wordt_in_dezelfde_switchronde_bezorgd() {
    let mut h = harness();
    let mut host = h.host();
    let mut app = h.attach(1);
    let f = mk_frame(
        PROTO_TCP,
        slot_mac(1),
        HOST_MAC,
        host_ip4(),
        slot_ip4(1),
        9080,
        5555,
        b"hoi",
    );
    assert!(host.tx.push(KIND_FRAME, &f));
    assert!(h.pass(), "switch zag het poort-0-frame niet");
    assert_eq!(app.rx.frame().as_deref(), Some(&f[..]));
}

/// Poort 0 wordt in de ring zelf gelezen (M17): extern verkeer van de
/// node-stack gaat nog steeds de draad op, en de poort is na de ronde terug.
#[test]
fn host_poort_uplink_gaat_de_draad_op_en_de_poort_blijft() {
    let mut h = harness();
    let mut host = h.host();
    let f = mk_frame(
        PROTO_TCP,
        [0xaa; 6],
        HOST_MAC,
        0x0A00_020F,
        0x0808_0808,
        5555,
        53,
        b"uit",
    );
    assert!(host.tx.push(KIND_UPLINK, &f));
    assert!(h.pass());
    assert_eq!(h.sent().len(), 1, "uplink-frame van poort 0 niet verstuurd");
    assert!(h.sw.is_attached(0), "poort 0 kwam niet terug in de tabel");
}

#[test]
fn rx_wake_alleen_op_leeg_naar_niet_leeg() {
    let mut h = harness();
    let mut app = h.attach(2);
    let f = mk_frame(
        PROTO_TCP,
        slot_mac(2),
        slot_mac(1),
        slot_ip4(1),
        slot_ip4(2),
        1111,
        2222,
        &[],
    );
    h.forward_once(1, &f);
    h.forward_once(1, &f);
    assert_eq!(
        wakes(2),
        1,
        "twee writes zonder drain gaven meer dan één wek"
    );
    assert!(app.rx.frame().is_some() && app.rx.frame().is_some());
    h.forward_once(1, &f);
    assert_eq!(wakes(2), 2);
}

/// De Go-versie las sinds 04-09 zónder slot omdat een TryLock een CAS is
/// (1,7M rondes/s op de M4). Hier bestaat er geen slot meer; de eigenschap
/// die blijft is dat de deur leest wat er werkelijk in de TX-ringen ligt.
#[test]
fn switch_pending_leest_onder_lock_contention() {
    let mut h = harness();
    let mut app = h.attach(1);
    assert!(
        !h.published.pending(),
        "niets in de TX-ringen, maar de bel gaat"
    );
    assert!(app.tx.push(KIND_FRAME, &[0u8; 64]));
    assert!(
        h.published.pending(),
        "frame in de TX-ring, maar de bel zwijgt"
    );
    h.sw.detach(1).unwrap();
    assert!(!h.published.pending(), "een ontkoppelde poort belt nog");
}

/// De bitmaps van 03-10: de deur en de ronde kijken alleen naar poorten
/// die hangen, ook voorbij het eerste woord (slot 100), en een ontkoppelde
/// poort valt eruit.
#[test]
fn ronde_en_deur_kennen_alleen_de_poorten_die_hangen() {
    assert_eq!(
        ports_of([1 << 3 | 1 << 63, 0, 1]).collect::<Vec<_>>(),
        vec![3, 63, 128]
    );
    let mut h = harness();
    let mut low = h.attach(2);
    let mut high = h.attach(100);
    assert!(!h.published.pending());
    let f = mk_frame(
        PROTO_TCP,
        slot_mac(2),
        slot_mac(100),
        slot_ip4(100),
        slot_ip4(2),
        1,
        2,
        b"x",
    );
    assert!(high.tx.push(KIND_FRAME, &f));
    assert!(h.published.pending(), "slot 100 belt niet");
    let mut buf = vec![0u8; MAX_LAN_FRAME];
    assert!(h.sw.switch_pass(&mut buf));
    assert_eq!(low.rx.frame().as_deref(), Some(&f[..]));
    assert!(!h.published.pending());
    h.sw.detach(100).unwrap();
    assert!(high.tx.push(KIND_FRAME, &f));
    assert!(!h.published.pending(), "een ontkoppelde poort belt nog");
    assert!(
        !h.sw.switch_pass(&mut buf),
        "een ronde leest een ontkoppelde poort"
    );
}

#[test]
fn slot_mag_geen_vreemde_bron_mac_of_ip_gebruiken() {
    let mut h = harness();
    h.uplink();
    let mut host = h.host();
    let mac = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        host_ip4(),
        5555,
        9080,
        &[],
    );
    h.forward_once(2, &mac);
    assert!(host.rx.frame().is_none(), "vervalste bron-MAC kwam bij HOP");
    let ip = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(2),
        slot_ip4(1),
        host_ip4(),
        5555,
        9080,
        &[],
    );
    h.forward_once(2, &ip);
    assert!(host.rx.frame().is_none(), "vervalst bron-IP kwam bij HOP");
    h.forward_once(1, &mac);
    assert_eq!(
        host.rx.frame().as_deref(),
        Some(&mac[..]),
        "eigen frame kwam niet aan"
    );
}

#[test]
fn arp_voor_de_gateway_beantwoordt_de_switch_zelf() {
    let mut h = harness();
    let mut app = h.attach(1);
    let mut req = ether_frame([0xff; 6], slot_mac(1), 0x0806);
    req.resize(ETH_LEN + 28, 0);
    put16(&mut req, 14, 1);
    put16(&mut req, 16, 0x0800);
    req[18] = 6;
    req[19] = 4;
    put16(&mut req, 20, 1);
    req[22..28].copy_from_slice(&slot_mac(1));
    crate::wire::put32(&mut req, 28, slot_ip4(1));
    crate::wire::put32(&mut req, 38, host_ip4());
    h.forward_once(1, &req);
    let r = app.rx.frame().expect("geen ARP-antwoord");
    assert_eq!(r[0..6], slot_mac(1));
    assert_eq!(
        (be16(&r, 20), mac_at(&r, 22), be32(&r, 28)),
        (2, HOST_MAC, host_ip4())
    );
}

/// M20: een unicast van slot naar slot gaat door de TX-ring van ring naar
/// ring; een vreemd bron-IP komt niet vrij en telt als bron-drop.
#[test]
fn ringpad_unicast_van_slot_naar_slot_en_spoof_niet() {
    let mut h = harness();
    let mut a = h.attach(1);
    let mut b = h.attach(2);
    let f = mk_frame(
        PROTO_TCP,
        slot_mac(2),
        slot_mac(1),
        slot_ip4(1),
        slot_ip4(2),
        1111,
        80,
        b"hallo",
    );
    assert!(a.tx.push(KIND_FRAME, &f));
    assert!(h.pass());
    assert_eq!(b.rx.frame().as_deref(), Some(&f[..]));
    let spoof = mk_frame(
        PROTO_TCP,
        slot_mac(2),
        slot_mac(1),
        slot_ip4(3),
        slot_ip4(2),
        1111,
        80,
        b"nep",
    );
    assert!(a.tx.push(KIND_FRAME, &spoof));
    h.pass();
    assert!(b.rx.frame().is_none(), "spoof kwam vrij");
    assert_eq!(h.stats.slot_src_drops.load(Relaxed), 1);
    assert!(h.sw.is_attached(1), "slot 1 kwam niet terug in de tabel");
}

/// M20: het ARP-antwoord van de gateway gaat naar de afzender zelf, dus de
/// poort moet terug zijn vóór de gewone weg.
#[test]
fn ringpad_arp_voor_de_gateway_bereikt_de_afzender() {
    let mut h = harness();
    let mut app = h.attach(1);
    let mut req = ether_frame([0xff; 6], slot_mac(1), 0x0806);
    req.resize(ETH_LEN + 28, 0);
    put16(&mut req, 14, 1);
    put16(&mut req, 16, 0x0800);
    req[18] = 6;
    req[19] = 4;
    put16(&mut req, 20, 1);
    req[22..28].copy_from_slice(&slot_mac(1));
    crate::wire::put32(&mut req, 28, slot_ip4(1));
    crate::wire::put32(&mut req, 38, host_ip4());
    assert!(app.tx.push(KIND_FRAME, &req));
    assert!(h.pass());
    let r = app.rx.frame().expect("geen ARP-antwoord");
    assert_eq!((be16(&r, 20), mac_at(&r, 22)), (2, HOST_MAC));
}

#[test]
fn ipv6_outbound_branches() {
    // Unicast brugt alleen IPv6.
    {
        let mut h = harness();
        h.uplink();
        let dst = [0x10, 0x20, 0x30, 0x40, 0x50, 0x60];
        let v6 = ether_frame(dst, slot_mac(1), 0x86dd);
        h.forward_once(1, &v6);
        assert_eq!(
            h.sent(),
            [v6],
            "IPv6-unicast ging niet ongewijzigd de uplink op"
        );
        let v4 = ether_frame(dst, slot_mac(1), 0x0800);
        h.forward_once(1, &v4);
        assert!(
            h.sent().is_empty(),
            "onbekende IPv4-unicast omzeilde de NAT-grens"
        );
    }
    // Multicast bereikt buren én uplink.
    {
        let mut h = harness();
        h.uplink();
        let mut peer = h.attach(2);
        let f = ether_frame([0x33, 0x33, 0, 0, 0, 0xfb], slot_mac(1), 0x86dd);
        h.forward_once(1, &f);
        assert_eq!(peer.rx.frame().as_deref(), Some(&f[..]));
        assert_eq!(h.sent(), [f]);
    }
}

#[test]
fn ipv6_inbound_branches() {
    // Slot-unicast accepteert alleen IPv6; IPv4 gaat naar de node-stack.
    {
        let mut h = harness();
        h.uplink();
        let mut app = h.attach(1);
        let mut host = h.host();
        let src = [0x10, 0x20, 0x30, 0x40, 0x50, 0x60];
        let v4 = ether_frame(slot_mac(1), src, 0x0800);
        let v6 = ether_frame(slot_mac(1), src, 0x86dd);
        assert!(
            h.to_switch
                .try_send(Frame::from_slice(&v4).unwrap())
                .is_ok()
        );
        assert!(
            h.to_switch
                .try_send(Frame::from_slice(&v6).unwrap())
                .is_ok()
        );
        assert!(h.pass());
        assert_eq!(
            host.rx.pop(),
            Some((KIND_UPLINK, v4)),
            "IPv4 niet aan de node-stack"
        );
        assert_eq!(
            app.rx.frame().as_deref(),
            Some(&v6[..]),
            "IPv6-unicast niet bij het slot"
        );
        assert!(
            app.rx.frame().is_none(),
            "LAN-IPv4 op een slot-MAC omzeilde de NAT"
        );
    }
    // Multicast floodt naar de aangesloten slots.
    {
        let mut h = harness();
        h.uplink();
        let mut a = h.attach(1);
        let mut b = h.attach(2);
        let f = ether_frame(
            [0x33, 0x33, 0, 0, 0, 0xfb],
            [0x10, 0x20, 0x30, 0x40, 0x50, 0x60],
            0x86dd,
        );
        assert!(h.to_switch.try_send(Frame::from_slice(&f).unwrap()).is_ok());
        assert!(h.pass());
        assert_eq!(a.rx.frame().as_deref(), Some(&f[..]));
        assert_eq!(b.rx.frame().as_deref(), Some(&f[..]));
    }
}

/// Een volle RX-ring wacht kort op zijn consument op een andere core (hier:
/// een ring die de eerste pogingen weigert).
#[test]
fn rx_wacht_kort_op_lokale_consumer() {
    let mut h = harness();
    let mut app = h.attach(1);
    app.rx.0.borrow_mut().refuse = 3;
    let f = vec![0u8; 1000];
    h.sw.core.write_rx(1, KIND_FRAME, &f);
    assert_eq!(app.rx.frame().map(|f| f.len()), Some(1000));
    assert_eq!(h.stats.rx_full.load(Relaxed), 1);
    assert_eq!(h.stats.rx_drops.load(Relaxed), 0);
}

/// Een bewoner van de OS-core wacht niet: de switch zou spinnen op een
/// ring die pas leegloopt als hij zelf afgeeft (30-09). Droppen en tellen,
/// meteen, zonder één kloklezing van de vangrail af te wachten.
#[test]
fn rx_wacht_niet_op_een_bewoner_van_de_os_core() {
    let mut h = harness();
    let mut app = h.attach(7);
    app.rx.0.borrow_mut().refuse = 3;
    TICK.with(|t| t.set(1_000_000_000)); // één lezing = één seconde
    let t0 = NOW.with(Cell::get);
    h.sw.core.write_rx(7, KIND_FRAME, &[0u8; 64]);
    let spent = NOW.with(Cell::get) - t0;
    TICK.with(|t| t.set(0));
    assert!(spent <= 1_000_000_000, "de switch wachtte {spent} ns");
    assert_eq!(h.stats.rx_full.load(Relaxed), 1);
    assert_eq!(h.stats.rx_drops.load(Relaxed), 1);
    assert_eq!(wakes(7), 1, "de bewoner krijgt wel zijn kick");
    // Zodra er ruimte is, gaat het volgende frame er gewoon in.
    app.rx.0.borrow_mut().refuse = 0;
    h.sw.core.write_rx(7, KIND_FRAME, &[1u8; 64]);
    assert_eq!(app.rx.frame().map(|f| f.len()), Some(64));
}

/// En een consument die niet leest, laat de switch na de vangrail los; tot
/// hij weer ruimte maakt dropt elk volgend frame meteen.
#[test]
fn rx_dropt_na_de_vangrail() {
    let mut h = harness();
    let mut app = h.attach(1);
    app.rx.0.borrow_mut().refuse = usize::MAX;
    TICK.with(|t| t.set(1_000_000)); // elke lezing 1 ms verder
    h.sw.core.write_rx(1, KIND_FRAME, &[0u8; 64]);
    h.sw.core.write_rx(1, KIND_FRAME, &[0u8; 64]);
    TICK.with(|t| t.set(0));
    assert_eq!(h.stats.rx_drops.load(Relaxed), 2);
    app.rx.0.borrow_mut().refuse = 0;
    h.sw.core.write_rx(1, KIND_FRAME, &[0u8; 64]);
    assert!(
        app.rx.frame().is_some(),
        "ruimte gemaakt, maar de poort bleef dicht"
    );
}

/// De hele lus over de executor: attach als bericht, een frame erdoor,
/// en detach als bericht.
#[test]
fn run_loop_attach_forward_detach() {
    let exec: &'static executor::Executor<8, 8> = leak(executor::Executor::new());
    exec.set_clock(fake_now);
    let mut h = harness();
    let (commands, door, published) = (h.commands, h.door, h.published);
    let mut dst = h.attach(2);
    let H { sw, .. } = h;
    exec.spawn(async move {
        let mut sw = sw;
        let mut buf = vec![0u8; MAX_LAN_FRAME];
        sw.run(exec, &mut buf).await;
    })
    .unwrap();
    let settle = || {
        let mut n = 0;
        while exec.step() {
            n += 1;
            assert!(n < 1000, "de lus komt niet tot rust");
        }
    };
    settle();

    let ack: &'static Ack = leak(Ack::new());
    let (tx_r, mut tx_w) = mem::pair(RING);
    let (_rx_r, rx_w) = mem::pair(RING);
    assert!(
        commands
            .try_send(Command::Attach {
                slot: 1,
                tx: tx_r,
                rx: rx_w,
                ack
            })
            .is_ok()
    );
    settle();
    assert_eq!(ack.try_take(), Some(Ok(0)));

    let f = mk_frame(
        PROTO_TCP,
        slot_mac(2),
        slot_mac(1),
        slot_ip4(1),
        slot_ip4(2),
        1,
        2,
        b"x",
    );
    assert!(tx_w.push(KIND_FRAME, &f));
    assert!(published.pending());
    door.set();
    settle();
    assert_eq!(dst.rx.frame().as_deref(), Some(&f[..]));

    assert!(
        commands
            .try_send(Command::Publish {
                proto: Proto::Tcp,
                node_port: 80,
                slot: 1,
                slot_port: 80,
                ack,
            })
            .is_ok()
    );
    settle();
    assert_eq!(ack.try_take(), Some(Ok(0)));

    assert!(commands.try_send(Command::Detach { slot: 1, ack }).is_ok());
    settle();
    assert_eq!(ack.try_take(), Some(Ok(0)));
    assert!(tx_w.push(KIND_FRAME, &f));
    assert!(
        !published.pending(),
        "ontkoppelde poort staat nog in de tabel"
    );
}

/// De flip in het klein: een uitgaande TCP-flow van slot 1 op de oude
/// switch, de snapshot via de brievenbus (`SnapshotNat`), en op een verse
/// switch het herstel (`HoldAdoption`, `RestoreNat`, `FinishAdoption`).
/// Het antwoord van de peer op de oude node-poort moet daarna gewoon in
/// slot 1 landen, op de oude poort van de app.
#[test]
fn de_conntrack_overleeft_de_flip_via_de_actor() {
    let mut old = harness();
    old.uplink();
    let _host = old.host();
    let _app = old.attach(1);
    let out = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        EXT_IP,
        5555,
        443,
        &[],
    );
    old.forward_once(1, &out);
    let sent = old.sent();
    assert_eq!(sent.len(), 1, "de SYN ging de uplink niet op");
    let node_port = be16(&sent[0], ETH_LEN + 20);

    let reply: &'static NatReply = leak(NatReply::new());
    let buf = vec![FlowState::default(); crate::MAX_FLOWS];
    old.sw.handle(Command::SnapshotNat { buf, reply });
    let snap = reply.snap.try_recv().expect("geen snapshot");
    assert_eq!(snap.flows.len(), 1);
    assert_eq!(snap.flows[0].node_port, node_port);

    // Bevroren: een nieuwe uitgaande verbinding krijgt geen flow meer.
    let other = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        EXT_IP,
        6666,
        443,
        &[],
    );
    old.forward_once(1, &other);
    assert!(old.sent().is_empty(), "na de snapshot nog een nieuwe flow");
    assert_eq!(old.sw.nat().flow_count(), 1);

    // De nieuwe kern.
    let mut new = harness();
    new.uplink();
    let mut app = new.attach(1);
    let ack: &'static Ack = leak(Ack::new());
    let ports: &'static [u16] = Box::leak(vec![node_port].into_boxed_slice());
    new.sw.handle(Command::HoldAdoption { ports, ack });
    assert_eq!(ack.try_take(), Some(Ok(0)));
    let flows: &'static [FlowState] = Box::leak(snap.flows.clone().into_boxed_slice());
    let state = NatState {
        flows,
        masq_next: snap.masq_next,
    };
    new.sw.handle(Command::RestoreNat { state, ack });
    assert_eq!(ack.try_take(), Some(Ok(1)), "de flow kwam niet terug");
    new.sw.handle(Command::FinishAdoption { ack });
    assert_eq!(ack.try_take(), Some(Ok(0)));

    let mut back = mk_frame(
        PROTO_TCP, NIC_MAC, GW_MAC0, EXT_IP, NODE_IP, 443, node_port, b"hallo",
    );
    assert!(new.sw.nat.inbound(&mut new.sw.core, &mut back, fake_now()));
    let got = app.rx.frame().expect("het antwoord landde niet in slot 1");
    assert_eq!(be32(&got, ETH_LEN + 16), slot_ip4(1));
    assert_eq!(be16(&got, ETH_LEN + 22), 5555);
}

/// Een slaper van het board zoals de deur hem ziet: toetst `ready()` eerst
/// (het contract van [`Sleeper`], dat de slapers van het board houden),
/// telt zijn slapen en onthoudt wat `ready()` aan het eind ervan zei.
/// `during` is wat een app-core doet terwijl de OS-core slaapt.
struct Spy {
    slept: &'static Cell<u32>,
    ready_at_end: &'static Cell<Option<bool>>,
    during: Box<dyn FnMut()>,
}

impl Sleeper for Spy {
    fn sleep(&mut self, _now: u64, _until: Option<u64>, ready: &dyn Fn() -> bool) {
        if ready() {
            return;
        }
        self.slept.set(self.slept.get() + 1);
        (self.during)();
        self.ready_at_end.set(Some(ready()));
    }
}

fn spy(during: impl FnMut() + 'static) -> (Spy, &'static Cell<u32>, &'static Cell<Option<bool>>) {
    let slept = leak(Cell::new(0));
    let ready_at_end = leak(Cell::new(None));
    let s = Spy {
        slept,
        ready_at_end,
        during: Box::new(during),
    };
    (s, slept, ready_at_end)
}

/// De les van 30-09: een frame van een app lag tot de failsafe van 1 ms,
/// omdat de kick de OS-core wel wekte maar niemand de switch belde. Werk
/// dat er al ligt: geen slaap, de bel meteen.
#[test]
fn deur_belt_de_switch_als_er_al_werk_ligt() {
    let mut h = harness();
    let mut app = h.attach(2);
    assert!(app.tx.push(KIND_FRAME, &[0u8; 64]));
    let (s, slept, _) = spy(|| {});
    let mut d = Doorbell::new(s, h.published, h.door);
    d.sleep(0, None, &|| false);
    assert_eq!(slept.get(), 0, "de OS-core sliep met werk in een TX-ring");
    assert!(
        h.door.is_set(),
        "werk in een TX-ring, maar de switch hoorde niets"
    );
}

/// Een app die tijdens de slaap publiceert: de slaper toetst `ready()` na
/// zijn wek (de SEV of de kick), en de deur maakt daar een bel van.
#[test]
fn deur_hoort_een_publicatie_tijdens_de_slaap() {
    let mut h = harness();
    let app: &'static RefCell<App> = leak(RefCell::new(h.attach(3)));
    let (s, slept, ready_at_end) = spy(move || {
        assert!(app.borrow_mut().tx.push(KIND_FRAME, &[1u8; 64]));
    });
    let mut d = Doorbell::new(s, h.published, h.door);
    d.sleep(0, None, &|| false);
    assert_eq!(slept.get(), 1);
    assert_eq!(
        ready_at_end.get(),
        Some(true),
        "de slaper sliep door de publicatie heen"
    );
    assert!(h.door.is_set(), "de publicatie belde de switch niet");
}

/// Stil: de deur slaapt gewoon, zonder bel, en een taak die klaar is
/// blijft de slaap beëindigen.
#[test]
fn deur_slaapt_als_er_niets_ligt() {
    let mut h = harness();
    let _app = h.attach(4);
    let (s, slept, ready_at_end) = spy(|| {});
    let mut d = Doorbell::new(s, h.published, h.door);
    d.sleep(0, None, &|| false);
    assert_eq!(slept.get(), 1);
    assert_eq!(ready_at_end.get(), Some(false));
    assert!(!h.door.is_set(), "een loze bel in een stille idle");
    d.sleep(0, None, &|| true);
    assert_eq!(slept.get(), 1, "de taken van de executor vielen weg");
}

/// Een corrupte TX-ring leest voor de deur als eeuwig werk; de switch haalt
/// hem uit de tabel, anders slaapt de OS-core nooit meer.
#[test]
fn corrupte_ring_gaat_uit_de_deur() {
    let mut h = harness();
    let mut app = h.attach(5);
    assert!(app.tx.push(KIND_FRAME, &[0u8; 64]));
    app.tx.0.borrow_mut().corrupt = Some("test");
    assert!(h.published.pending());
    let mut buf = vec![0u8; MAX_LAN_FRAME];
    assert!(!h.sw.switch_pass(&mut buf));
    assert!(
        !h.published.pending(),
        "een dode ring houdt de deur voor altijd open"
    );
}
