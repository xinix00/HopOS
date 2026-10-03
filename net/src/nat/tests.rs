//! De NAT-tests, naam voor naam uit `nat_test.go`, `adoption_test.go` en
//! `hairpin_test.go`. De switch-lus draait hier niet; bezorgde frames landen
//! in een rij per slot van de test-omgeving (Go: `testSlotRing`), frames voor
//! de uplink in `sent` (Go: `fakeNIC`).

use super::*;
use crate::plan::{HOST_MAC, host_ip4};
use crate::wire::testutil::*;
use crate::wire::{PROTO_TCP, PROTO_UDP, TCP_ACK, TCP_FIN, TCP_RST, TCP_SYN};
use std::cell::RefCell;
use std::collections::VecDeque;

const NODE_IP: u32 = 0x0A00_020F; // 10.0.2.15/24
const GW_IP: u32 = 0x0A00_0202; // 10.0.2.2, de gateway uit de lease
const EXT_IP: u32 = 0x5DB8_D822; // 93.184.216.34 (off-subnet)
const LAN_IP: u32 = 0x0A00_0263; // 10.0.2.99 (on-subnet)
const GW_MAC0: [u8; 6] = [0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01];
const LAN_MAC0: [u8; 6] = [0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB];
const NIC_MAC: [u8; 6] = [0x52, 0x54, 0x00, 0x12, 0x34, 0x56];
const T0: u64 = 10_000 * SEC;

pub(crate) struct TestIo {
    pub(crate) stats: Stats,
    pub(crate) attached: Vec<bool>,
    pub(crate) rings: Vec<VecDeque<Vec<u8>>>,
    pub(crate) sent: Vec<Vec<u8>>,
    pub(crate) logs: RefCell<Vec<String>>,
}

impl TestIo {
    fn new() -> Self {
        Self {
            stats: Stats::new(),
            attached: vec![false; SLOT_CAP + 1],
            rings: vec![VecDeque::new(); SLOT_CAP + 1],
            sent: Vec::new(),
            logs: RefCell::new(Vec::new()),
        }
    }
    /// Go: `testSlotRing`, zonder de leesfunctie.
    fn attach(&mut self, i: usize) {
        self.attached[i] = true;
    }
    fn read(&mut self, i: usize) -> Option<Vec<u8>> {
        self.rings[i].pop_front()
    }
}

impl NatIo for TestIo {
    fn deliver(&mut self, slot: usize, f: &[u8]) {
        if self.attached.get(slot).copied().unwrap_or(false) {
            self.rings[slot].push_back(f.to_vec());
        }
    }
    fn uplink_tx(&mut self, f: &[u8]) {
        self.sent.push(f.to_vec());
    }
    fn stats(&self) -> &Stats {
        &self.stats
    }
    fn log(&self, args: fmt::Arguments<'_>) {
        self.logs.borrow_mut().push(args.to_string());
    }
}

/// Go: `resetNAT` plus `setUplink`.
fn setup() -> (Nat, TestIo) {
    let mut nat = Nat::new().unwrap();
    nat.set_uplink(Uplink::new(NODE_IP, 24, NIC_MAC, GW_IP).unwrap());
    (nat, TestIo::new())
}

fn leer_gateway(nat: &mut Nat, io: &mut TestIo) {
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
    assert!(
        !nat.inbound(io, &mut f, T0),
        "leer-frame geclaimd zonder flow of publicatie"
    );
    assert_eq!(
        nat.gw,
        Some(GW_MAC0),
        "gateway-MAC niet geleerd uit off-subnet inbound"
    );
}

#[expect(clippy::too_many_arguments, reason = "Go's flowFor, plat")]
fn flow_for(
    nat: &mut Nat,
    io: &mut TestIo,
    proto: u8,
    slot: usize,
    sport: u16,
    dst: u32,
    dport: u16,
    now: u64,
) -> Option<Id> {
    nat.flow_for(io, proto, slot, slot_ip4(slot), sport, dst, dport, now)
        .map(|(id, _)| id)
}

fn age(nat: &mut Nat, id: Id, seen: u64) {
    nat.flows.get_mut(id).unwrap().seen = seen;
}

fn l4(f: &[u8]) -> &[u8] {
    &f[ETH_LEN + 20..]
}

#[test]
fn publish_validatie() {
    let (mut nat, _) = setup();
    // "icmp" bestaat niet meer: `Proto` kent alleen tcp en udp.
    assert!(
        nat.publish(Proto::Tcp, 80, 0, 80, SLOT_CAP).is_err(),
        "slot 0 geaccepteerd"
    );
    assert!(
        nat.publish(Proto::Tcp, 80, SLOT_CAP + 1, 80, SLOT_CAP)
            .is_err(),
        "slot buiten bereik"
    );
    assert_eq!(
        nat.publish(Proto::Tcp, 0, 1, 80, SLOT_CAP),
        Err(Error::PortZero)
    );
    nat.publish(Proto::Tcp, 80, 1, 80, SLOT_CAP).unwrap();
    assert_eq!(
        nat.publish(Proto::Tcp, 80, 2, 80, SLOT_CAP),
        Err(Error::AlreadyPublished { port: 80, slot: 1 })
    );
    nat.publish(Proto::Udp, 80, 2, 80, SLOT_CAP).unwrap();
}

/// Het volledige masquerade-pad: app belt uit, antwoord komt terug.
#[test]
fn masquerade_uit_en_terug() {
    let (mut nat, mut io) = setup();
    leer_gateway(&mut nat, &mut io);
    let payload = b"GET / HTTP/1.1";
    let slot_ip = slot_ip4(1);
    let mut out = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip,
        EXT_IP,
        5555,
        443,
        payload,
    );
    assert!(nat.outbound(&mut io, 1, &mut out, T0));
    assert_eq!(io.sent.len(), 1);
    let sent = io.sent[0].clone();
    assert_eq!(
        be32(&sent, ETH_LEN + 12),
        NODE_IP,
        "bron-IP niet gemasqueradeerd"
    );
    let masq = be16(l4(&sent), 0);
    assert!((MASQ_BASE..MASQ_END).contains(&masq));
    assert_eq!(sent[0..6], GW_MAC0);
    assert_eq!(sent[6..12], NIC_MAC);
    assert!(sent.ends_with(payload), "payload beschadigd");
    check_frame(&sent, "uitgaand");

    // Zelfde 5-tupel opnieuw: zelfde flow, zelfde masq-poort.
    let mut out2 = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip,
        EXT_IP,
        5555,
        443,
        &[],
    );
    nat.outbound(&mut io, 1, &mut out2, T0);
    assert_eq!(be16(l4(&io.sent[1]), 0), masq);

    // Het antwoord: geclaimd en rechtstreeks in slot 1 bezorgd.
    io.attach(1);
    let mut reply = mk_frame(
        PROTO_TCP,
        NIC_MAC,
        GW_MAC0,
        EXT_IP,
        NODE_IP,
        443,
        masq,
        b"HTTP/1.1 200 OK",
    );
    assert!(nat.inbound(&mut io, &mut reply, T0));
    let inj = io.read(1).expect("antwoord niet bezorgd");
    assert_eq!(be32(&inj, ETH_LEN + 16), slot_ip);
    assert_eq!(be16(l4(&inj), 2), 5555);
    assert_eq!(inj[0..6], slot_mac(1));
    assert_eq!(inj[6..12], HOST_MAC);
    check_frame(&inj, "antwoord");
}

/// Het DNAT-pad: gepubliceerde poort in, slot-antwoord uit (SNAT).
#[test]
fn dnat_in_en_slot_antwoord_uit() {
    let (mut nat, mut io) = setup();
    nat.publish(Proto::Tcp, 8080, 1, 9090, SLOT_CAP).unwrap();
    io.attach(1);
    let mut inf = mk_frame(
        PROTO_TCP, NIC_MAC, LAN_MAC0, LAN_IP, NODE_IP, 1234, 8080, b"hallo",
    );
    assert!(nat.inbound(&mut io, &mut inf, T0));
    let inj = io.read(1).expect("DNAT-frame niet bezorgd");
    assert_eq!(be32(&inj, ETH_LEN + 16), slot_ip4(1));
    assert_eq!(be16(l4(&inj), 2), 9090);
    check_frame(&inj, "DNAT-in");

    let mut los = mk_frame(
        PROTO_TCP,
        NIC_MAC,
        LAN_MAC0,
        LAN_IP,
        NODE_IP,
        1234,
        8081,
        &[],
    );
    assert!(
        !nat.inbound(&mut io, &mut los, T0),
        "niet-gepubliceerde poort geclaimd"
    );

    let mut uit = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        LAN_IP,
        9090,
        1234,
        b"antwoord",
    );
    assert!(nat.slot_reply(&mut io, 1, &mut uit, T0));
    assert_eq!(io.sent.len(), 1);
    let sent = &io.sent[0];
    assert_eq!(be32(sent, ETH_LEN + 12), NODE_IP);
    assert_eq!(be16(l4(sent), 0), 8080);
    assert_eq!(
        sent[0..6],
        LAN_MAC0,
        "dst-MAC hoort de geleerde neighbor te zijn"
    );
    check_frame(sent, "SNAT-uit");

    let mut vreemd = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        LAN_IP,
        7777,
        1234,
        &[],
    );
    assert!(
        !nat.slot_reply(&mut io, 1, &mut vreemd, T0),
        "slot-frame zonder publicatie geclaimd"
    );
}

#[test]
fn alloc_port_slaat_bezet_over() {
    let (mut nat, mut io) = setup();
    // De eerstvolgende twee kandidaten bezet: één door een flow naar dezelfde
    // peer, één door een publicatie.
    nat.flows
        .insert(Flow {
            proto: PROTO_TCP,
            slot: 2,
            fin_fwd: false,
            fin_rev: false,
            slot_ip: slot_ip4(2),
            dst_ip: EXT_IP,
            slot_port: 1,
            dst_port: 443,
            node_port: MASQ_BASE,
            seen: T0,
        })
        .unwrap();
    nat.publish(Proto::Tcp, MASQ_BASE + 1, 1, 80, SLOT_CAP)
        .unwrap();
    assert_eq!(nat.alloc_port(PROTO_TCP, EXT_IP, 443), Some(MASQ_BASE + 2));
    // Naar een andere peer mag alles, tot hij rondgaat.
    for _ in MASQ_BASE..MASQ_END {
        assert!(
            nat.alloc_port(PROTO_TCP, EXT_IP, 444).is_some(),
            "onterecht uitgeput"
        );
    }
    let _ = &mut io;
}

/// Alles bezet: geen poort. (Go vulde 29k reverse-entries; de conntrack
/// draagt er hier 4096, dus de uitputting toetst de keuze los van de tabel.)
#[test]
fn alloc_port_uitputting() {
    let mut next = MASQ_BASE;
    assert_eq!(alloc_port_by(&mut next, |_| true), None);
    assert_eq!(next, MASQ_BASE, "één volle ronde en weer terug op de basis");
}

#[test]
fn sweep_expired() {
    let (mut nat, mut io) = setup();
    let now = T0;
    for (proto, sport, leeftijd) in [
        (PROTO_TCP, 1001, TCP_IDLE + SEC), // verlopen
        (PROTO_TCP, 1002, TCP_IDLE - SEC), // vers genoeg
        (PROTO_UDP, 1003, UDP_IDLE + SEC), // verlopen (kortere timeout)
        (PROTO_UDP, 1004, TCP_IDLE - SEC), // ouder dan UDP_IDLE: verlopen
    ] {
        let id = flow_for(&mut nat, &mut io, proto, 1, sport, EXT_IP, 443, now).unwrap();
        age(&mut nat, id, now - leeftijd);
    }
    nat.sweep_expired(now);
    assert_eq!(nat.flows.len(), 1);
    assert!(
        nat.flows
            .by_fwd(&FKey(PROTO_TCP, slot_ip4(1), EXT_IP, 1002, 443))
            .is_some()
    );
}

#[test]
fn sweep_compacteert_map_na_verkeerspiek() {
    let (mut nat, mut io) = setup();
    let now = T0;
    let mut keep = None;
    for i in 0..128u16 {
        let id = flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1000 + i, EXT_IP, 443, now).unwrap();
        if i == 127 {
            keep = Some(id);
        } else {
            age(&mut nat, id, now - TCP_IDLE - SEC);
        }
    }
    assert_eq!(nat.flows.high_water, 128);
    nat.sweep_expired(now);
    assert_eq!(
        (nat.flows.len(), nat.flows.high_water, nat.flows.count(1)),
        (1, 1, 1)
    );
    let keep = *nat.flows.get(keep.unwrap()).unwrap();
    assert!(
        nat.flows.by_rev(&keep.rkey()).is_some(),
        "omgekeerde mapping van de overlever verloren"
    );
}

#[test]
fn flow_lookup_vervangt_verlopen_entry() {
    let (mut nat, mut io) = setup();
    let now = T0;
    let old = flow_for(&mut nat, &mut io, PROTO_UDP, 1, 1001, EXT_IP, 443, now).unwrap();
    let old_port = nat.flows.get(old).unwrap().node_port;
    age(&mut nat, old, now - UDP_IDLE - SEC);
    let (rep, created) = nat
        .flow_for(&mut io, PROTO_UDP, 1, slot_ip4(1), 1001, EXT_IP, 443, now)
        .unwrap();
    assert!(created, "exacte lookup gaf een verlopen flow terug");
    assert_ne!(nat.flows.get(rep).unwrap().node_port, old_port);
    assert!(
        nat.flows
            .by_rev(&RKey(PROTO_UDP, old_port, EXT_IP, 443))
            .is_none()
    );
    assert_eq!((nat.flows.len(), nat.flows.count(1)), (1, 1));
}

/// Een verlopen reverse match mag de node-poort niet blijven kapen.
#[test]
fn reverse_lookup_verwijdert_expired_en_valt_terug_op_dnat() {
    let (mut nat, mut io) = setup();
    let now = T0;
    let id = flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1001, EXT_IP, 443, now).unwrap();
    age(&mut nat, id, now - TCP_IDLE - SEC);
    let np = nat.flows.get(id).unwrap().node_port;
    nat.publish(Proto::Tcp, np, 2, 8080, SLOT_CAP).unwrap();
    io.attach(2);
    let mut inf = mk_frame(
        PROTO_TCP, NIC_MAC, GW_MAC0, EXT_IP, NODE_IP, 443, np, b"nieuw",
    );
    assert!(
        nat.inbound(&mut io, &mut inf, now),
        "viel niet terug op DNAT"
    );
    let got = io.read(2).expect("niet bij het gepubliceerde slot");
    assert_eq!(
        (be32(&got, ETH_LEN + 16), be16(l4(&got), 2)),
        (slot_ip4(2), 8080)
    );
    check_frame(&got, "DNAT na stale reverse lookup");
    assert_eq!((nat.flows.len(), nat.flows.count(1)), (0, 0));
}

#[test]
fn tcp_fin_verkort_timeout_pas_na_beide_richtingen() {
    let (mut nat, mut io) = setup();
    let base = 1_000 * SEC;
    let id = flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1001, EXT_IP, 443, base).unwrap();
    nat.note_tcp_flags(id, TCP_FIN, false);
    nat.sweep_expired(base + TCP_CLOSING_IDLE + 1);
    assert_eq!(
        nat.flows.len(),
        1,
        "eenzijdige FIN ruimde een half-close op"
    );
    nat.note_tcp_flags(id, TCP_FIN, true);
    nat.sweep_expired(base + TCP_CLOSING_IDLE + 1);
    assert_eq!((nat.flows.len(), nat.flows.count(1)), (0, 0));
}

#[test]
fn tcp_inbound_rst_telt_als_gesloten() {
    let (mut nat, mut io) = setup();
    let base = 1_000 * SEC;
    let id = flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1001, EXT_IP, 443, base).unwrap();
    assert!(
        !nat.note_tcp_flags(id, TCP_RST | TCP_ACK, true),
        "een inbound RST ruimt niet meteen op"
    );
    nat.sweep_expired(base + TCP_CLOSING_IDLE + 1);
    assert_eq!(
        (nat.flows.len(), nat.flows.count(1)),
        (0, 0),
        "een RST-gesloten flow verliep niet op de sluit-TTL"
    );
}

#[test]
fn tcp_rst_bezorging_en_veilige_reclaim() {
    // Outbound zonder flow.
    {
        let (mut nat, mut io) = setup();
        leer_gateway(&mut nat, &mut io);
        let before = nat.masq_next;
        let mut rst = mk_frame(
            PROTO_TCP,
            HOST_MAC,
            slot_mac(1),
            slot_ip4(1),
            EXT_IP,
            5555,
            443,
            &[],
        );
        set_tcp_flags(&mut rst, TCP_RST | TCP_ACK);
        assert!(
            nat.outbound(&mut io, 1, &mut rst, T0),
            "flow-loze RST niet afgehandeld"
        );
        assert!(io.sent.is_empty(), "flow-loze RST toch vertaald");
        assert_eq!(
            (nat.flows.len(), nat.flows.high_water, nat.masq_next),
            (0, 0, before)
        );
    }
    // Inbound.
    {
        let (mut nat, mut io) = setup();
        leer_gateway(&mut nat, &mut io);
        let slot_ip = slot_ip4(1);
        let mut out = mk_frame(
            PROTO_TCP,
            HOST_MAC,
            slot_mac(1),
            slot_ip,
            EXT_IP,
            5555,
            443,
            &[],
        );
        nat.outbound(&mut io, 1, &mut out, T0);
        let id = nat
            .flows
            .by_fwd(&FKey(PROTO_TCP, slot_ip, EXT_IP, 5555, 443))
            .unwrap();
        let np = nat.flows.get(id).unwrap().node_port;
        io.attach(1);
        let mut rst = mk_frame(PROTO_TCP, NIC_MAC, GW_MAC0, EXT_IP, NODE_IP, 443, np, &[]);
        set_tcp_flags(&mut rst, TCP_RST | TCP_ACK);
        assert!(nat.inbound(&mut io, &mut rst, T0));
        let got = io.read(1).expect("RST vóór aflevering weggegooid");
        assert_ne!(got[ETH_LEN + 20 + 13] & TCP_RST, 0);
        check_frame(&got, "inbound RST");
        assert_eq!(
            (nat.flows.len(), nat.flows.count(1)),
            (1, 1),
            "inbound RST ruimde onveilig vroeg op"
        );
    }
    // Outbound.
    {
        let (mut nat, mut io) = setup();
        leer_gateway(&mut nat, &mut io);
        let slot_ip = slot_ip4(1);
        let mut first = mk_frame(
            PROTO_TCP,
            HOST_MAC,
            slot_mac(1),
            slot_ip,
            EXT_IP,
            5555,
            443,
            &[],
        );
        nat.outbound(&mut io, 1, &mut first, T0);
        let mut rst = mk_frame(
            PROTO_TCP,
            HOST_MAC,
            slot_mac(1),
            slot_ip,
            EXT_IP,
            5555,
            443,
            &[],
        );
        set_tcp_flags(&mut rst, TCP_RST | TCP_ACK);
        nat.outbound(&mut io, 1, &mut rst, T0);
        assert_eq!(io.sent.len(), 2);
        assert_ne!(io.sent[1][ETH_LEN + 20 + 13] & TCP_RST, 0);
        check_frame(&io.sent[1], "outbound RST");
        assert_eq!(
            (nat.flows.len(), nat.flows.count(1)),
            (0, 0),
            "outbound RST gaf de lease niet vrij"
        );
    }
}

#[test]
fn vol_slot_rejectpad_is_rate_limited() {
    let (mut nat, mut io) = setup();
    let now = T0;
    let mut oldest = None;
    for i in 0..MAX_FLOWS_PER_SLOT as u16 {
        let id = flow_for(&mut nat, &mut io, PROTO_UDP, 1, 1000 + i, EXT_IP, 443, now)
            .unwrap_or_else(|| panic!("flow {i} vóór het quotum geweigerd"));
        oldest.get_or_insert(id);
    }
    assert!(flow_for(&mut nat, &mut io, PROTO_UDP, 1, 60000, EXT_IP, 443, now).is_none());
    let (first_sweep, first_log) = (nat.next_sweep, nat.next_slot_full_log[1]);
    assert!(
        first_sweep != 0 && first_log != 0,
        "eerste reject zette de cadans niet"
    );
    assert_eq!(io.logs.borrow().len(), 1);

    age(&mut nat, oldest.unwrap(), now - UDP_IDLE - SEC);
    assert!(flow_for(&mut nat, &mut io, PROTO_UDP, 1, 60001, EXT_IP, 443, now).is_none());
    assert!(
        nat.flows
            .by_fwd(&FKey(PROTO_UDP, slot_ip4(1), EXT_IP, 1000, 443))
            .is_some(),
        "tweede reject veegde opnieuw vóór de cadans"
    );
    assert_eq!(
        (nat.next_sweep, nat.next_slot_full_log[1]),
        (first_sweep, first_log)
    );
    assert_eq!(io.logs.borrow().len(), 1, "herhaalde reject logde opnieuw");

    nat.next_sweep = 0; // de volgende goedkope cadans
    assert!(flow_for(&mut nat, &mut io, PROTO_UDP, 1, 60002, EXT_IP, 443, now).is_some());
    assert_eq!(
        (nat.flows.len(), nat.flows.count(1)),
        (MAX_FLOWS_PER_SLOT, MAX_FLOWS_PER_SLOT)
    );
}

#[test]
fn unpublish_slot_ruimt_op() {
    let (mut nat, mut io) = setup();
    nat.publish(Proto::Tcp, 8080, 1, 8080, SLOT_CAP).unwrap();
    nat.publish(Proto::Tcp, 8081, 2, 8081, SLOT_CAP).unwrap();
    flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1001, EXT_IP, 443, T0).unwrap();
    flow_for(&mut nat, &mut io, PROTO_TCP, 2, 1002, EXT_IP, 443, T0).unwrap();
    flow_for(&mut nat, &mut io, PROTO_TCP, 2, 1003, slot_ip4(1), 8080, T0).unwrap(); // hairpin naar 1
    nat.unpublish_slot(1);
    assert_eq!(nat.pubs.len(), 1);
    assert_eq!(nat.pubs[0].slot, 2);
    assert_eq!(nat.flows.len(), 1);
    for id in nat.flows.ids() {
        let fl = nat.flows.get(id).unwrap();
        assert!(
            fl.slot == 2 && fl.dst_ip != slot_ip4(1),
            "verkeerde flow overleefde"
        );
    }
    assert_eq!((nat.flows.count(1), nat.flows.count(2)), (0, 1));
}

/// Go toetste hier dat de backing-array na een publicatiepiek loslaat. De
/// tabel is hier vast begrensd en houdt niets vast; wat overblijft is dat
/// precies de juiste publicatie overleeft.
#[test]
fn unpublish_slot_compacteert_publicatiepiek() {
    let (mut nat, _) = setup();
    for p in 1000u16..1128 {
        nat.publish(Proto::Tcp, p, 1, p, SLOT_CAP).unwrap();
    }
    nat.publish(Proto::Tcp, 9000, 2, 9000, SLOT_CAP).unwrap();
    nat.unpublish_slot(1);
    assert_eq!(nat.pubs.len(), 1);
    assert_eq!((nat.pubs[0].slot, nat.pubs[0].node_port), (2, 9000));
}

#[test]
fn neighbor_cache_en_plafond() {
    let (mut nat, _) = setup();
    let now = T0;
    nat.learn(LAN_IP, LAN_MAC0, now, true);
    assert_eq!(nat.l2_for(LAN_IP, now), Some(LAN_MAC0));
    nat.learn(EXT_IP, GW_MAC0, now, true); // off-subnet: dit is de gateway
    assert_eq!(nat.l2_for(EXT_IP, now), Some(GW_MAC0));
    assert_eq!(
        nat.l2_for((NODE_IP & !0xff) | 0x42, now),
        None,
        "onbekende on-subnet neighbor hoort niet known te zijn: first-contact moet ARP'en"
    );
    let gw2 = [0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x02];
    let mut i = 0u32;
    while nat.neigh.len() < MAX_NEIGH {
        nat.learn(0x0A00_0300 + i, LAN_MAC0, now, true);
        i += 1;
    }
    nat.learn(0x0B00_0001, gw2, now, true); // onbekend IP op het plafond: leging
    assert_eq!(nat.neigh.len(), 1);
    assert_eq!(
        nat.l2_for(EXT_IP, now),
        Some(gw2),
        "gateway-fallback werkt niet na de leging"
    );
}

/// Zonder next-hop: geclaimd maar gedropt, en het enige dat de NIC op mag
/// is de ARP-vraag naar de gateway (30-09: zonder die vraag kwam er op
/// een echt LAN nooit iets van buiten, dus werd de gateway nooit geleerd).
#[test]
fn outbound_zonder_next_hop_dropt() {
    let (mut nat, mut io) = setup();
    let mut f = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        EXT_IP,
        5555,
        443,
        &[],
    );
    assert!(nat.outbound(&mut io, 1, &mut f, T0));
    assert_eq!(io.sent.len(), 1, "alleen de ARP-vraag naar de gateway");
    assert!(is_bcast_arp(&io.sent[0]));
    assert_eq!(
        be32(&io.sent[0], 38),
        GW_IP,
        "de vraag hoort naar de gateway te gaan"
    );
    assert_eq!(nat.flows.len(), 0, "drop hoort geen flow achter te laten");
    assert_eq!(io.stats.nat_no_route.load(Relaxed), 1);
}

/// Off-subnet zonder ooit een frame van buiten (de eerste Pi 5-boot,
/// 30-09): de gateway-MAC komt uit de ARP-reply van de gateway zelf, en
/// daarna gaat elk off-subnet frame die kant op.
#[test]
fn off_subnet_leert_de_gateway_via_arp() {
    let (mut nat, mut io) = setup();
    let mut syn = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        EXT_IP,
        5555,
        443,
        &[],
    );
    set_tcp_flags(&mut syn, TCP_SYN);
    assert!(nat.outbound(&mut io, 1, &mut syn.clone(), T0));
    assert_eq!(
        io.sent.len(),
        1,
        "eerst alleen de ARP-vraag naar de gateway"
    );
    assert_eq!(be32(&io.sent[0], 38), GW_IP);
    assert_eq!(nat.gw, None);

    nat.arp_learn(&arp_reply(GW_MAC0, GW_IP), T0);
    assert!(nat.outbound(&mut io, 1, &mut syn.clone(), T0 + SEC));
    assert_eq!(io.sent.len(), 2, "de retransmit gaat de NIC op");
    assert_eq!(io.sent[1][0..6], GW_MAC0, "niet naar de gateway-MAC");
    assert_eq!(nat.gw, Some(GW_MAC0), "de gateway hoort nu geleerd te zijn");
    assert_eq!(nat.flows.len(), 1);
}

/// Een broadcast of multicast van een off-subnet buurman (link-local, een
/// ander subnet op hetzelfde L2) kwam niet door de router en mag de
/// gateway-MAC niet zetten: gemeten 30-09 op de Pi 5 aan het LAN, 11 van
/// 14 connects naar buiten op de deadline met `noroute` en `flowfull` nul.
#[test]
fn een_off_subnet_broadcast_vergiftigt_de_gateway_niet() {
    let (mut nat, mut io) = setup();
    leer_gateway(&mut nat, &mut io);
    let rogue = [0x02, 0xBA, 0xD0, 0x00, 0x00, 0x77];
    // SSDP van een apparaat op 169.254.7.7, als broadcast.
    let mut ssdp = mk_frame(
        PROTO_UDP,
        [0xff; 6],
        rogue,
        0xA9FE_0707,
        0xFFFF_FFFF,
        1900,
        1900,
        &[1],
    );
    nat.inbound(&mut io, &mut ssdp, T0);
    // Een unicast van een ander subnet op hetzelfde L2, niet aan ons.
    let mut other = mk_frame(
        PROTO_UDP,
        LAN_MAC0,
        rogue,
        0xC0A8_0005,
        LAN_IP,
        5353,
        5353,
        &[1],
    );
    nat.inbound(&mut io, &mut other, T0);
    // En een link-local bron die ons wél unicast aanspreekt: nooit gerouteerd.
    let mut ll = mk_frame(
        PROTO_UDP,
        NIC_MAC,
        rogue,
        0xA9FE_0707,
        NODE_IP,
        5353,
        5353,
        &[1],
    );
    nat.inbound(&mut io, &mut ll, T0);
    assert_eq!(
        nat.gw,
        Some(GW_MAC0),
        "gateway-MAC vergiftigd door een buurman"
    );
    // Een off-subnet unicast aan ons kwam door de router: die MAC telt.
    let mut via = mk_frame(PROTO_TCP, NIC_MAC, rogue, EXT_IP, NODE_IP, 443, 16002, &[]);
    nat.inbound(&mut io, &mut via, T0);
    assert_eq!(
        nat.gw,
        Some(rogue),
        "een unicast van buiten hoort de gateway te verversen"
    );
}

/// De router zelf, vers via ARP, wint van het passief geleerde paar: een
/// vergiftigde gateway-MAC herstelt zodra de router iets zegt, niet pas bij
/// een pakket van buiten (dat zonder juiste MAC nooit komt). Verloopt de
/// router-neighbor, dan vraagt de NAT hem opnieuw en stuurt hij intussen
/// via het paar mee.
#[test]
fn de_verse_router_neighbor_wint_van_het_passieve_paar() {
    let (mut nat, mut io) = setup();
    let rogue = [0x02, 0xBA, 0xD0, 0x00, 0x00, 0x88];
    nat.gw = Some(rogue); // hoe dan ook vergiftigd (een oudere kern, de overdracht)
    nat.arp_learn(&arp_reply(GW_MAC0, GW_IP), T0);
    let syn = || {
        let mut s = mk_frame(
            PROTO_TCP,
            HOST_MAC,
            slot_mac(1),
            slot_ip4(1),
            EXT_IP,
            5556,
            443,
            &[],
        );
        set_tcp_flags(&mut s, TCP_SYN);
        s
    };
    assert!(nat.outbound(&mut io, 1, &mut syn(), T0 + SEC));
    assert_eq!(io.sent.len(), 1);
    assert_eq!(
        io.sent[0][0..6],
        GW_MAC0,
        "de SYN ging naar de vergiftigde MAC"
    );
    assert_eq!(nat.gw, Some(GW_MAC0), "het paar hoort de router te volgen");

    // De router-neighbor verloopt en het paar raakt opnieuw vergiftigd: de
    // SYN-retransmit vraagt de router en gaat intussen via het paar mee.
    let later = T0 + NEIGH_TTL + 2 * SEC;
    nat.gw = Some(rogue);
    assert!(nat.outbound(&mut io, 1, &mut syn(), later));
    assert_eq!(
        io.sent.len(),
        3,
        "een ARP-vraag naar de router en de SYN erachteraan"
    );
    assert!(is_bcast_arp(&io.sent[1]));
    assert_eq!(be32(&io.sent[1], 38), GW_IP);
    assert_eq!(io.sent[2][0..6], rogue, "intussen via het paar");
    assert!(
        !nat.neigh.contains(&GW_IP),
        "de verlopen router-neighbor hoort weg"
    );
    // De router antwoordt, met een andere MAC dan ooit: de volgende SYN
    // volgt hem; de retransmit vraagt de router nog eens (de rate-limit van
    // een seconde is voorbij), dat is de gewone probe.
    let gw_mac1 = [0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x02];
    nat.arp_learn(&arp_reply(gw_mac1, GW_IP), later + SEC);
    assert!(nat.outbound(&mut io, 1, &mut syn(), later + 2 * SEC));
    assert_eq!(io.sent.len(), 5, "de ARP-probe en de SYN erachteraan");
    assert!(is_bcast_arp(&io.sent[3]));
    assert_eq!(
        io.sent[4][0..6],
        gw_mac1,
        "de SYN volgt het antwoord van de router"
    );
    assert_eq!(nat.gw, Some(gw_mac1));
}

fn arp_reply(from_mac: [u8; 6], from_ip: u32) -> Vec<u8> {
    let mut r = vec![0u8; 42];
    r[0..6].copy_from_slice(&NIC_MAC);
    r[6..12].copy_from_slice(&from_mac);
    put16(&mut r, 12, ET_ARP);
    put16(&mut r, 14, 1);
    put16(&mut r, 16, 0x0800);
    r[18] = 6;
    r[19] = 4;
    put16(&mut r, 20, 2);
    r[22..28].copy_from_slice(&from_mac);
    put32(&mut r, 28, from_ip);
    r[32..38].copy_from_slice(&NIC_MAC);
    put32(&mut r, 38, NODE_IP);
    r
}

fn is_bcast_arp(f: &[u8]) -> bool {
    be16(f, 12) == ET_ARP && f[0..6] == [0xff; 6]
}

/// First-contact naar een on-subnet host (Altra 14-07: de eerste
/// loader-golf dropte al zijn SYNs).
#[test]
fn arp_first_contact() {
    let (mut nat, mut io) = setup();
    nat.learn(EXT_IP, GW_MAC0, T0, true); // gateway bekend, zoals na elke echte boot
    let mut syn = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        LAN_IP,
        5555,
        8000,
        &[],
    );
    set_tcp_flags(&mut syn, TCP_SYN);
    assert!(nat.outbound(&mut io, 1, &mut syn.clone(), T0));
    assert_eq!(io.sent.len(), 2, "verwacht ARP-request + SYN-via-gateway");
    let arp = &io.sent[0];
    assert!(is_bcast_arp(arp), "geen broadcast-ARP");
    assert_eq!((be16(arp, 20), be32(arp, 38)), (1, LAN_IP));
    assert_eq!((be32(arp, 28), mac_at(arp, 22)), (NODE_IP, NIC_MAC));
    assert_eq!(io.sent[1][0..6], GW_MAC0);

    // Rate-limit: een tweede SYN direct erna geeft geen tweede ARP.
    nat.outbound(&mut io, 1, &mut syn.clone(), T0);
    assert_eq!(io.sent.len(), 3);
    assert!(!is_bcast_arp(&io.sent[2]));

    nat.arp_learn(&arp_reply(LAN_MAC0, LAN_IP), T0);
    nat.outbound(&mut io, 1, &mut syn.clone(), T0);
    assert_eq!(io.sent.len(), 4);
    assert_eq!(
        io.sent[3][0..6],
        LAN_MAC0,
        "retransmit niet naar het geleerde MAC"
    );
}

/// Een verse maar verweesde neighbor mag een dial niet 120 s stilhouden.
#[test]
fn known_neighbor_syn_retry_probes_arp() {
    let (mut nat, mut io) = setup();
    nat.learn(LAN_IP, LAN_MAC0, T0, true);
    let mut syn = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        LAN_IP,
        5555,
        631,
        &[],
    );
    set_tcp_flags(&mut syn, TCP_SYN);
    nat.outbound(&mut io, 1, &mut syn.clone(), T0);
    assert_eq!(io.sent.len(), 1);
    assert_eq!(io.sent[0][0..6], LAN_MAC0);
    nat.outbound(&mut io, 1, &mut syn.clone(), T0);
    assert_eq!(io.sent.len(), 3, "retry hoort ARP + SYN te sturen");
    assert!(is_bcast_arp(&io.sent[1]) && be32(&io.sent[1], 38) == LAN_IP);
    assert_eq!(io.sent[2][0..6], LAN_MAC0);
    nat.outbound(&mut io, 1, &mut syn.clone(), T0);
    assert_eq!(io.sent.len(), 4);
    assert_eq!(
        be16(&io.sent[3], 12),
        0x0800,
        "ARP-rate-limit liet een storm door"
    );
}

/// ARP-probes en DHCP-broadcasts van buren mogen niets leren.
#[test]
fn geen_poisoning_uit_probes_en_dhcp() {
    let (mut nat, mut io) = setup();
    leer_gateway(&mut nat, &mut io);
    let rogue = [0x02, 0xBA, 0xD0, 0x00, 0x00, 0x99];
    let mut probe = arp_reply(rogue, 0);
    put16(&mut probe, 20, 1);
    nat.arp_learn(&probe, T0);
    let mut dhcp = mk_frame(PROTO_UDP, [0xff; 6], rogue, 0, 0xFFFF_FFFF, 68, 67, &[1]);
    nat.inbound(&mut io, &mut dhcp, T0);
    assert_eq!(nat.gw, Some(GW_MAC0), "gateway-MAC vergiftigd");
    assert!(!nat.neigh.contains(&0), "0.0.0.0 als neighbor geleerd");
}

/// Een geleerde neighbor is geen eeuwige waarheid (de Brother, 20-08).
#[test]
fn neighbor_expires_and_relearns() {
    let (mut nat, mut io) = setup();
    nat.learn(EXT_IP, GW_MAC0, T0, true);
    nat.learn(LAN_IP, LAN_MAC0, T0, true);
    let syn = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        LAN_IP,
        5555,
        631,
        &[],
    );
    nat.outbound(&mut io, 1, &mut syn.clone(), T0);
    assert_eq!(io.sent.len(), 1);
    assert_eq!(io.sent[0][0..6], LAN_MAC0);

    let later = T0 + NEIGH_TTL + SEC;
    nat.outbound(&mut io, 1, &mut syn.clone(), later);
    assert_eq!(
        io.sent.len(),
        3,
        "verlopen neighbor hoort ARP + via-gateway te geven"
    );
    assert!(is_bcast_arp(&io.sent[1]));
    assert_eq!(io.sent[2][0..6], GW_MAC0);

    let new_mac = [0x66, 0x77, 0x88, 0x99, 0xAA, 0xCC];
    nat.learn(LAN_IP, new_mac, later, true);
    nat.outbound(&mut io, 1, &mut syn.clone(), later);
    assert_eq!(io.sent.last().unwrap()[0..6], new_mac);
}

// --- adoption_test.go ------------------------------------------------------

#[test]
fn adoption_keeps_app_ports_away_from_node_stack() {
    let (mut nat, mut io) = setup();
    nat.hold_adoption(&[18090, MASQ_BASE]).unwrap();
    for proto in [PROTO_TCP, PROTO_UDP] {
        for port in [18090, MASQ_BASE] {
            let mut f = mk_frame(proto, NIC_MAC, LAN_MAC0, LAN_IP, NODE_IP, 1234, port, &[]);
            assert!(
                nat.inbound(&mut io, &mut f, T0),
                "adopterende poort {port} bereikte de node-stack"
            );
        }
    }
    let mut mgmt = mk_frame(
        PROTO_TCP,
        NIC_MAC,
        LAN_MAC0,
        LAN_IP,
        NODE_IP,
        1234,
        5555,
        &[],
    );
    assert!(
        !nat.inbound(&mut io, &mut mgmt, T0),
        "adoptie blokkeerde de node-console"
    );
    io.attach(1);
    nat.publish(Proto::Tcp, 18090, 1, 18090, SLOT_CAP).unwrap();
    nat.finish_adoption();
    let mut f = mk_frame(
        PROTO_TCP,
        NIC_MAC,
        LAN_MAC0,
        LAN_IP,
        NODE_IP,
        1234,
        18090,
        b"continued",
    );
    assert!(nat.inbound(&mut io, &mut f, T0) && io.read(1).is_some());
    let mut f = mk_frame(
        PROTO_TCP,
        NIC_MAC,
        LAN_MAC0,
        LAN_IP,
        NODE_IP,
        1234,
        MASQ_BASE,
        &[],
    );
    assert!(
        !nat.inbound(&mut io, &mut f, T0),
        "tijdelijke claim overleefde de adoptie"
    );
}

#[test]
fn adoption_defers_outbound_until_old_mapping_restored() {
    for proto in [PROTO_TCP, PROTO_UDP] {
        let (mut nat, mut io) = setup();
        leer_gateway(&mut nat, &mut io);
        io.attach(1);
        let old_port = MASQ_BASE + 17;
        let flows = [FlowState {
            proto,
            slot: 1,
            slot_ip: slot_ip4(1),
            slot_port: 5555,
            dst_ip: EXT_IP,
            dst_port: 443,
            node_port: old_port,
            fins: 0,
        }];
        let state = NatState {
            flows: &flows,
            masq_next: 0,
            gw_mac: None,
        };
        nat.hold_adoption(&[old_port]).unwrap();
        let send = |nat: &mut Nat, io: &mut TestIo, sport: u16| {
            let mut f = mk_frame(
                proto,
                HOST_MAC,
                slot_mac(1),
                slot_ip4(1),
                EXT_IP,
                sport,
                443,
                b"continued",
            );
            assert!(nat.outbound(io, 1, &mut f, T0), "outbound niet geclaimd");
        };
        send(&mut nat, &mut io, 5555);
        nat.masq_next = old_port;
        send(&mut nat, &mut io, 5556);
        assert!(
            io.sent.is_empty() && nat.flows.len() == 0,
            "alloceerde vóór het herstel"
        );
        assert_eq!(nat.restore(&state, |s| io.attached[s], T0), 1);
        send(&mut nat, &mut io, 5555);
        assert!(io.sent.is_empty(), "adoptie losgelaten vóór finish");
        nat.finish_adoption();
        send(&mut nat, &mut io, 5555);
        assert_eq!(io.sent.len(), 1);
        let sent = &io.sent[0];
        let ip = ipv4_l4(sent).unwrap();
        assert_eq!(
            be16(sent, ip.l4),
            old_port,
            "retransmit veranderde de poort die de peer ziet"
        );
        check_frame(sent, "geadopteerd uitgaand");
    }
}

#[test]
fn adoption_without_ports_also_defers_new_flows() {
    let (mut nat, mut io) = setup();
    nat.hold_adoption(&[]).unwrap();
    assert!(flow_for(&mut nat, &mut io, PROTO_UDP, 1, 5555, EXT_IP, 443, T0).is_none());
    nat.finish_adoption();
    assert!(flow_for(&mut nat, &mut io, PROTO_UDP, 1, 5555, EXT_IP, 443, T0).is_some());
}

/// De snapshot-vorm (Go: `SnapshotNAT`): verlopen flows gaan niet mee, en
/// een restore op een verse NAT geeft dezelfde mappings terug.
#[test]
fn snapshot_and_restore_round_trip() {
    let (mut nat, mut io) = setup();
    leer_gateway(&mut nat, &mut io);
    let live = flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1001, EXT_IP, 443, T0).unwrap();
    nat.note_tcp_flags(live, TCP_FIN, false);
    let dead = flow_for(&mut nat, &mut io, PROTO_UDP, 2, 1002, EXT_IP, 53, T0).unwrap();
    age(&mut nat, dead, T0 - UDP_IDLE - SEC);
    let mut out = [FlowState::default(); 8];
    let s = nat.snapshot(T0, &mut out);
    assert_eq!(s.flows.len(), 1);
    assert_eq!(s.flows[0].fins, 1);
    assert_eq!(s.gw_mac, Some(GW_MAC0));
    let np = s.flows[0].node_port;

    let (mut fresh, _) = setup();
    assert_eq!(fresh.restore(&s, |slot| slot == 1, T0), 1);
    let id = fresh
        .flows
        .by_rev(&RKey(PROTO_TCP, np, EXT_IP, 443))
        .unwrap();
    assert!(fresh.flows.get(id).unwrap().fin_fwd);
    assert_eq!(fresh.gw, Some(GW_MAC0));
    // Een slot dat de flip niet overleefde: niets hersteld.
    let (mut other, _) = setup();
    assert_eq!(other.restore(&s, |_| false, T0), 0);
}

// --- hairpin_test.go ---------------------------------------------------------

/// Slot 2 belt node-IP:7878 (gepubliceerd door slot 1): heen en terug.
#[test]
fn hairpin_heen_en_terug() {
    let (mut nat, mut io) = setup();
    io.attach(1);
    io.attach(2);
    nat.publish(Proto::Tcp, 7878, 1, 7878, SLOT_CAP).unwrap();
    let (cli, srv) = (slot_ip4(2), slot_ip4(1));
    let mut heen = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(2),
        cli,
        NODE_IP,
        5555,
        7878,
        b"hoi",
    );
    assert!(nat.outbound(&mut io, 2, &mut heen, T0));
    assert!(io.sent.is_empty(), "hairpin-frame ging de NIC uit");
    let got = io.read(1).expect("niets bezorgd bij de dienst");
    check_frame(&got, "heen");
    assert_eq!((be32(&got, ETH_LEN + 16), be16(l4(&got), 2)), (srv, 7878));
    assert_eq!(be32(&got, ETH_LEN + 12), NODE_IP);
    let masq = be16(l4(&got), 0);
    assert!((MASQ_BASE..MASQ_END).contains(&masq));
    assert!(got.ends_with(b"hoi"));

    let mut terug = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        srv,
        NODE_IP,
        7878,
        masq,
        b"dag",
    );
    assert!(nat.slot_reply(&mut io, 1, &mut terug, T0));
    assert!(io.sent.is_empty(), "reply ging de NIC uit");
    let got = io.read(2).expect("niets bezorgd bij de beller");
    check_frame(&got, "terug");
    assert_eq!(
        (be32(&got, ETH_LEN + 12), be16(l4(&got), 0)),
        (NODE_IP, 7878)
    );
    assert_eq!((be32(&got, ETH_LEN + 16), be16(l4(&got), 2)), (cli, 5555));
    assert!(got.ends_with(b"dag"));
}

#[test]
fn hairpin_udp() {
    let (mut nat, mut io) = setup();
    io.attach(1);
    io.attach(2);
    nat.publish(Proto::Udp, 5353, 1, 5353, SLOT_CAP).unwrap();
    let mut heen = mk_frame(
        PROTO_UDP,
        HOST_MAC,
        slot_mac(2),
        slot_ip4(2),
        NODE_IP,
        6666,
        5353,
        b"vraag",
    );
    nat.outbound(&mut io, 2, &mut heen, T0);
    let got = io.read(1).expect("heen: niets bij de dienst");
    check_frame(&got, "udp heen");
    let masq = be16(l4(&got), 0);
    let mut terug = mk_frame(
        PROTO_UDP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        NODE_IP,
        5353,
        masq,
        b"antwoord",
    );
    nat.slot_reply(&mut io, 1, &mut terug, T0);
    let got = io.read(2).expect("terug: niets bij de beller");
    check_frame(&got, "udp terug");
    assert!(io.sent.is_empty());
}

#[test]
fn hairpin_ongepubliceerd_dropt() {
    let (mut nat, mut io) = setup();
    io.attach(1);
    let mut f = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(2),
        slot_ip4(2),
        NODE_IP,
        5555,
        8080,
        &[],
    );
    assert!(nat.outbound(&mut io, 2, &mut f, T0));
    assert!(io.sent.is_empty() && io.read(1).is_none());
}

#[test]
fn hairpin_reply_zonder_flow_dropt() {
    let (mut nat, mut io) = setup();
    io.attach(2);
    nat.publish(Proto::Tcp, 7878, 1, 7878, SLOT_CAP).unwrap();
    let mut f = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(1),
        slot_ip4(1),
        NODE_IP,
        7878,
        20001,
        &[],
    );
    assert!(nat.slot_reply(&mut io, 1, &mut f, T0));
    assert!(io.sent.is_empty() && io.read(2).is_none());
    let _ = host_ip4();
}

/// De Pi 4-storm (03-10): slot 3 bestormt zijn eigen gepubliceerde poort
/// via het node-IP, 8 verbindingen tegelijk, 600 achter elkaar (drie
/// rondes binnen de sluit-TTL), terwijl Hop in slot 1 de laagste plekken
/// van de slab vasthoudt. Boven de 512 moet de recycler elke SYN een
/// gesloten flow van slot 3 geven; vóór de fix vond de steekproef na de
/// wrap telkens Hops flows en vielen 64 SYNs (op de node: 1 s RTO).
#[test]
fn hairpin_storm_recyclet_achter_buurflows() {
    let (mut nat, mut io) = setup();
    io.attach(3);
    nat.publish(Proto::Tcp, 8090, 3, 8090, SLOT_CAP).unwrap();
    for i in 0..60 {
        flow_for(&mut nat, &mut io, PROTO_TCP, 1, 1000 + i, EXT_IP, 443, T0).unwrap();
    }
    let ip = slot_ip4(3);
    // Per verbinding: client-poort, masq-poort, fase van de handshake.
    let mut act: Vec<(u16, u16, u8)> = Vec::new();
    let (mut started, mut done, mut now) = (0u16, 0, T0);
    while done < 600 {
        while act.len() < 8 && started < 600 {
            act.push((50000 + started, 0, 0));
            started += 1;
        }
        now += 100_000;
        let mut k = 0;
        while let Some(&(cport, masq, fase)) = act.get(k) {
            let (sport, dport, fl) = match fase {
                0 => (cport, 8090, TCP_SYN),
                1 => (8090, masq, TCP_SYN | TCP_ACK),
                3 => (cport, 8090, TCP_FIN | TCP_ACK),
                4 => (8090, masq, TCP_FIN | TCP_ACK),
                _ => (cport, 8090, TCP_ACK),
            };
            let mut f = mk_frame(
                PROTO_TCP,
                HOST_MAC,
                slot_mac(3),
                ip,
                NODE_IP,
                sport,
                dport,
                &[],
            );
            set_tcp_flags(&mut f, fl);
            if sport == 8090 {
                nat.slot_reply(&mut io, 3, &mut f, now);
            } else {
                nat.outbound(&mut io, 3, &mut f, now);
            }
            let got = io
                .read(3)
                .unwrap_or_else(|| panic!("verbinding {cport}: fase {fase} gedropt"));
            if fase == 5 {
                act.remove(k);
                done += 1;
                continue;
            }
            let masq = if fase == 0 { be16(l4(&got), 0) } else { masq };
            act[k] = (cport, masq, fase + 1);
            k += 1;
        }
    }
    assert_eq!(nat.flows.count(3), MAX_FLOWS_PER_SLOT);
    assert_eq!(io.stats.nat_flow_full.load(Relaxed), 0);
}

/// Run 3 van de Pi 4-storm (03-10): slot 3 op zijn maximum met 504
/// gesloten en 8 lopende hairpin-flows, tussen 300 flows van Hop (slot 1)
/// die de slab domineren. De nieuwe SYN moet slagen en de oudste gesloten
/// flow van slot 3 nemen, waar die ook in de slab staat.
#[test]
fn vol_slot_neemt_altijd_de_oudste_gesloten_flow() {
    let (mut nat, mut io) = setup();
    io.attach(3);
    nat.publish(Proto::Tcp, 8090, 3, 8090, SLOT_CAP).unwrap();
    let srv = slot_ip4(3);
    let mut hop = 1000;
    let mut buren = |nat: &mut Nat, io: &mut TestIo, n: u16| {
        for _ in 0..n {
            flow_for(nat, io, PROTO_TCP, 1, hop, EXT_IP, 443, T0).unwrap();
            hop += 1;
        }
    };
    buren(&mut nat, &mut io, 150);
    let mut oudste = None;
    for i in 0..MAX_FLOWS_PER_SLOT as u16 {
        if i == 256 {
            buren(&mut nat, &mut io, 150);
        }
        let id = flow_for(&mut nat, &mut io, PROTO_TCP, 3, 50000 + i, srv, 8090, T0).unwrap();
        let fl = nat.flows.get_mut(id).unwrap();
        if i >= 8 {
            (fl.fin_fwd, fl.fin_rev) = (true, true);
            fl.seen = T0 + u64::from(i) * SEC / 100;
        }
        if i == 8 {
            oudste = Some(fl.fkey());
        }
    }
    let mut syn = mk_frame(
        PROTO_TCP,
        HOST_MAC,
        slot_mac(3),
        srv,
        NODE_IP,
        60000,
        8090,
        &[],
    );
    set_tcp_flags(&mut syn, TCP_SYN);
    let now = T0 + 20 * SEC;
    nat.next_sweep = now + SEC; // geen veeg: de recycler moet het doen
    assert!(nat.outbound(&mut io, 3, &mut syn, now));
    assert!(io.read(3).is_some(), "de SYN viel bij een vol slot");
    assert_eq!(nat.flows.count(3), MAX_FLOWS_PER_SLOT);
    assert!(
        nat.flows.by_fwd(&oudste.unwrap()).is_none(),
        "niet de oudste gesloten flow gerecycled"
    );
    assert_eq!(io.stats.nat_flow_full.load(Relaxed), 0);
    assert!(io.logs.borrow().is_empty());
}
