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
            .set_uplink(Uplink::new(NODE_IP, 24, NIC_MAC).unwrap());
    }

    fn leer_gateway(&mut self) {
        let mut f = mk_frame(
            PROTO_TCP,
            NIC_MAC,
            GW_MAC0,
            EXT_IP,
            NODE_IP,
            443,
            16001,
            &[],
        );
        assert!(!self.sw.nat.inbound(&mut self.sw.core, &mut f, fake_now()));
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
    h.leer_gateway();
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
    h.leer_gateway();
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

#[test]
fn slot_mag_geen_vreemde_bron_mac_of_ip_gebruiken() {
    let mut h = harness();
    h.uplink();
    h.leer_gateway();
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
/// detach als bericht, en stop.
#[test]
fn run_loop_attach_forward_detach() {
    let exec: &'static executor::Executor<8, 8> = leak(executor::Executor::new());
    exec.set_clock(fake_now);
    let mut h = harness();
    let stop: &'static Stop = leak(Stop::new());
    let (commands, door, published) = (h.commands, h.door, h.published);
    let mut dst = h.attach(2);
    let H { sw, .. } = h;
    exec.spawn(async move {
        let mut sw = sw;
        let mut buf = vec![0u8; MAX_LAN_FRAME];
        sw.run(exec, &mut buf, stop).await;
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

    stop.set();
    settle();
    assert_eq!(exec.live_tasks(), 0, "de lus stopte niet op de stopbel");
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
    old.leer_gateway();
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
    assert_eq!(snap.gw_mac, Some(GW_MAC0));

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
        gw_mac: snap.gw_mac,
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
