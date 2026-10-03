//! NAT tussen de externe NIC en het interne net: twee richtingen, geen
//! tunnel (Go: `nat.go`, `handoff.go`).
//!
//! - Poort-publicatie (DNAT): een vaste bestemming node-IP:poort naar
//!   slot-IP:poort. Stateloos: per pakket alleen koppen herschrijven,
//!   checksums incrementeel (RFC 1624). [`Nat::inbound`] (extern naar slot)
//!   en [`Nat::from_slot`] (slot-antwoord naar extern).
//! - Uitgaand (masquerade/PAT): een app belt naar buiten; HOP herschrijft
//!   bron slot-IP:poort naar node-IP:node-poort en houdt een kleine
//!   conntrack bij zodat het antwoord terugvindt. TCP én UDP (DNS, QUIC).
//!   Nooit TCP-terminatie op core 0: HOP herschrijft alleen koppen.
//! - Hairpin (Dereks model 27-07: poorten publiceren áltijd naar buiten, DNS
//!   kiest de host, en is dat je eigen node dan is de switch de sluiproute):
//!   een app die het node-IP belt op een gepubliceerde poort wordt intern
//!   omgelegd, DNAT en masquerade in één, en ring-in bezorgd; er gaat geen
//!   byte de NIC uit.
//!
//! De L2-next-hop komt uit een neighbor-cache die passief leert uit inbound
//! frames: srcIP naar srcMAC, en een frame van búíten ons subnet is via de
//! gateway gerelayed, dus dat is de gateway-MAC. Een on-subnet host die nog
//! nooit sprak wordt actief ge-ARP't.
//!
//! Alle NAT-staat is van de switch-actor en wordt alleen vanuit zijn lus
//! aangeraakt: er is geen moment waarop de tabel half-gemuteerd is, en een
//! snapshot voor de kern-flip is een gewone lees-ronde.

use crate::flows::{FKey, Flow, FlowTable, Id, MAX_FLOWS_PER_SLOT, RKey};
use crate::map::Map;
use crate::plan::{HOST_MAC, SLOT_CAP, slot_ip4, slot_mac};
use crate::wire::{
    ET_ARP, ETH_LEN, PROTO_TCP, PROTO_UDP, TCP_ACK, TCP_FIN, TCP_RST, TCP_SYN, be16, be32, byte,
    fix_csum32, ipv4_l4, mac_at, put_mac, put16, put32, rewrite_l4,
};
use crate::{Error, Stats};
use bounded::BoundedVec;
use core::fmt;
use core::sync::atomic::Ordering::Relaxed;

const SEC: u64 = 1_000_000_000;

/// Het masquerade-poortbereik (PAT). Bewust disjunct van het efemere bereik
/// van HOP's eigen stack (die deelt sequentieel uit in [49152, 65535]), dus
/// de masq stopt op 49152: anders kan een inbound antwoord op HOP's eigen
/// DNS/S3-poort per ongeluk een masquerade-flow naar dezelfde peer matchen.
/// 29k poorten blijft ruim boven het conntrack-plafond.
pub const MASQ_BASE: u16 = 20000;
/// Het einde (exclusief) van het masquerade-bereik.
pub const MASQ_END: u16 = 49152;

/// Idle-timeouts. Keepalives van een langlopende tunnel (~30-90 s) blijven
/// ruim binnen de TCP-timeout.
const TCP_IDLE: u64 = 300 * SEC;
const UDP_IDLE: u64 = 60 * SEC;
/// Na een FIN in beide richtingen is alleen de afsluitende ACK/retransmit
/// nog onderweg: een conservatieve minuut. Een eenzijdige FIN kan een
/// legitieme half-close zijn en houdt daarom gewoon de TCP-timeout.
const TCP_CLOSING_IDLE: u64 = 60 * SEC;

/// Echte wall-clock-expiry zonder scan per pakket. De kortste TTL is een
/// minuut; twee vegen per TTL houden het terugwinnen tijdig zonder core 0
/// elke seconde door 4096 entries te laten lopen.
pub const FLOW_SWEEP_EVERY: u64 = 30 * SEC;
const FLOW_LOG_EVERY: u64 = 30 * SEC;

/// De neighbor-cache is begrensd (spoofbare srcIP als sleutel): bij het
/// plafond legen en herleren.
const MAX_NEIGH: usize = 4096;

/// Hoe lang een geleerde neighbor geldt zonder nieuw levensteken. Dezelfde
/// 120 s als leannets eigen tabel: een wifi-host die slaapt of zwerft
/// wisselt van L2-pad, en een cache zonder veroudering maakt hem dan
/// PERMANENT onbereikbaar. Gemeten 20-08 met de Brother op .201: dials
/// stierven stil terwijl de Mac hem gewoon kon pingen; printer uit-aan
/// (gratuitous ARP) heelde het.
const NEIGH_TTL: u64 = 120 * SEC;

/// Eigen ARP-requests: per bestemming hoogstens één per seconde, zodat een
/// storm van 127 loaders naar dezelfde onbekende host geen ARP-storm wordt.
const ARP_EVERY: u64 = SEC;

/// Een bron-IP dat nooit door de router kwam: 0.0.0.0 (DHCP), link-local
/// 169.254/16 (RFC 3927: nooit gerouteerd), multicast en hoger (224/4 en
/// 240/4, plus broadcast). Zo'n frame zegt niets over de gateway-MAC.
const fn bogus_source(ip: u32) -> bool {
    ip == 0 || ip & 0xFFFF_0000 == 0xA9FE_0000 || ip >= 0xE000_0000
}

/// Het plafond van de publicatietabel.
pub const MAX_PUBS: usize = 512;
/// Het plafond van de adoptieclaims tijdens een kern-flip.
pub const MAX_ADOPT: usize = 256;

/// Een transportprotocol dat gepubliceerd kan worden. De Go-API nam een
/// string en weigerde "icmp" bij runtime; hier kan het niet eens.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Proto {
    /// TCP.
    Tcp,
    /// UDP.
    Udp,
}

impl Proto {
    const fn num(self) -> u8 {
        match self {
            Self::Tcp => PROTO_TCP,
            Self::Udp => PROTO_UDP,
        }
    }
}

/// Het externe adres van de node: IP, prefix, de MAC van de NIC en de
/// gateway uit de lease (0 = geen: dan is alleen het subnet bereikbaar).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Uplink {
    /// Het node-IP.
    pub ip: u32,
    /// Het subnetmasker, afgeleid uit de prefix.
    pub mask: u32,
    /// De MAC van de uplink-NIC.
    pub mac: [u8; 6],
    /// De gateway (de next-hop voor alles buiten het subnet), 0 = geen.
    pub gateway: u32,
}

impl Uplink {
    /// Het uplink-adres uit IP, prefixlengte, MAC en gateway.
    pub fn new(ip: u32, prefix: u32, mac: [u8; 6], gateway: u32) -> Result<Self, Error> {
        if prefix > 32 {
            return Err(Error::Prefix(prefix));
        }
        let mask = if prefix == 0 {
            0
        } else {
            u32::MAX << (32 - prefix)
        };
        Ok(Self {
            ip,
            mask,
            mac,
            gateway,
        })
    }
}

/// Eén gepubliceerde poort. De conventie is dat de app hetzelfde
/// poortnummer bindt als de node-poort, maar de vertaling kan het aan als
/// ze verschillen.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Pub {
    proto: u8,
    node_port: u16,
    slot: u8,
    slot_port: u16,
}

#[derive(Clone, Copy)]
struct Neighbor {
    mac: [u8; 6],
    /// De laatste keer dat de host het zélf bewees (een inbound frame);
    /// uitgaand gebruik telt bewust niet.
    seen: u64,
}

/// Eén masquerade-flow in overdraagbare vorm: platte velden, vaste maten,
/// 24 bytes (de volle conntrack past zo in ~96 KB van de handoff-staart).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct FlowState {
    /// IP-protocol.
    pub proto: u8,
    /// Het slot (1..=255; de switch-MAC codeert hem ook in één byte).
    pub slot: u8,
    /// Bit 0 = FIN gezien richting peer, bit 1 = richting client.
    pub fins: u8,
    /// Poort in het slot.
    pub slot_port: u16,
    /// Poort van de peer.
    pub dst_port: u16,
    /// De node-poort die de peer kent.
    pub node_port: u16,
    /// IP in het slot.
    pub slot_ip: u32,
    /// IP van de peer.
    pub dst_ip: u32,
}

/// Alles wat de NAT aan een volgende kern doorgeeft.
///
/// Wat BEWUST niet meegaat: de neighbor-cache (die leert passief terug
/// binnen één ARP-ronde, en een verkeerd overgenomen next-hop is erger dan
/// een korte leerpauze), en `seen` per flow (de nieuwe kern zet hem op
/// "nu"; de flip-duur telt zo niet als idle-tijd). `masq_next` gaat wél
/// mee: één woord, en het scheelt de eerste allocaties een scan.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NatState<'a> {
    /// De levende flows.
    pub flows: &'a [FlowState],
    /// De volgende masquerade-kandidaat.
    pub masq_next: u16,
    /// De geleerde gateway-MAC.
    pub gw_mac: Option<[u8; 6]>,
}

/// Wat de NAT van zijn omgeving nodig heeft: bezorgen in een slot-ring, een
/// frame de uplink op, de tellers, en een logregel.
pub(crate) trait NatIo {
    /// Legt een (al herschreven) frame in de RX-ring van slot `slot`. Niet
    /// aangesloten of vol = drop, zoals echt Ethernet.
    fn deliver(&mut self, slot: usize, f: &[u8]);
    /// Zet een frame op de uplink.
    fn uplink_tx(&mut self, f: &[u8]);
    /// De gedeelde tellers.
    fn stats(&self) -> &Stats;
    /// Eén logregel.
    fn log(&self, args: fmt::Arguments<'_>);
}

/// De NAT-staat: publicaties, conntrack, neighbor-cache.
pub struct Nat {
    pubs: BoundedVec<Pub, MAX_PUBS>,
    uplink: Option<Uplink>,
    neigh: Map<u32, Neighbor>,
    arp_last: Map<u32, u64>,
    gw: Option<[u8; 6]>,
    flows: FlowTable,
    masq_next: u16,
    next_sweep: u64,
    next_full_log: u64,
    next_slot_full_log: [u64; SLOT_CAP + 1],
    /// Tijdens een adoptie zijn deze node-poorten nog van de overgedragen
    /// apps, ook als hun routering nog niet terug is. `Some` houdt ook
    /// nieuwe uitgaande toewijzing tegen tot het herstel.
    adoption: Option<BoundedVec<u16, MAX_ADOPT>>,
}

impl Nat {
    /// Een lege NAT; alloceert zijn tabellen één keer (boot).
    pub fn new() -> Result<Self, Error> {
        Ok(Self {
            pubs: BoundedVec::new(),
            uplink: None,
            neigh: Map::new(MAX_NEIGH)?,
            arp_last: Map::new(MAX_NEIGH)?,
            gw: None,
            flows: FlowTable::new()?,
            masq_next: MASQ_BASE,
            next_sweep: 0,
            next_full_log: 0,
            next_slot_full_log: [0; SLOT_CAP + 1],
            adoption: None,
        })
    }

    /// Registreert het externe adres (Go: `WrapUplink`). Zonder uplink claimt
    /// de NAT niets.
    pub fn set_uplink(&mut self, u: Uplink) {
        self.uplink = Some(u);
    }

    /// Het externe adres, als het er is.
    #[must_use]
    pub fn uplink(&self) -> Option<Uplink> {
        self.uplink
    }

    fn on_subnet(&self, ip: u32) -> bool {
        self.uplink.is_some_and(|u| ip & u.mask == u.ip & u.mask)
    }

    // --- Publicaties ------------------------------------------------------

    /// Routeert node-IP:`node_port` naar `slot`:`slot_port`. Fout bij een al
    /// gepubliceerde poort. De publicatie leeft tot [`unpublish_slot`].
    ///
    /// [`unpublish_slot`]: Self::unpublish_slot
    pub fn publish(
        &mut self,
        proto: Proto,
        node_port: u16,
        slot: usize,
        slot_port: u16,
        max_slots: usize,
    ) -> Result<(), Error> {
        let Some(s) = slot_byte(slot, max_slots) else {
            return Err(Error::SlotRange(slot));
        };
        if node_port == 0 || slot_port == 0 {
            return Err(Error::PortZero);
        }
        if let Some(e) = self.pub_by_node_port(proto.num(), node_port) {
            return Err(Error::AlreadyPublished {
                port: node_port,
                slot: usize::from(e.slot),
            });
        }
        self.pubs
            .push(Pub {
                proto: proto.num(),
                node_port,
                slot: s,
                slot_port,
            })
            .map_err(|_| Error::Full("publications", MAX_PUBS))
    }

    fn pub_by_node_port(&self, proto: u8, node_port: u16) -> Option<Pub> {
        self.pubs
            .iter()
            .find(|p| p.proto == proto && p.node_port == node_port)
            .copied()
    }

    /// Trekt alle publicaties van slot `i` in en ruimt zijn flows op. Een
    /// hairpin-flow staat op naam van de CLIENT, maar hoort net zo hard bij
    /// de dienst in `dst_ip`: stoppen van óf client óf dienst trekt de
    /// mapping in, anders erft een nieuwe huurder van het dienstslot een
    /// verouderde omgekeerde route.
    pub fn unpublish_slot(&mut self, i: usize) {
        self.pubs.retain(|p| usize::from(p.slot) != i);
        let target = if (1..=SLOT_CAP).contains(&i) {
            slot_ip4(i)
        } else {
            0
        };
        let mut removed = false;
        for id in 0..crate::flows::MAX_FLOWS as Id {
            let hit = self
                .flows
                .get(id)
                .is_some_and(|f| usize::from(f.slot) == i || (target != 0 && f.dst_ip == target));
            if hit {
                removed |= self.flows.remove(id);
            }
        }
        if removed {
            self.flows.maybe_compact();
        }
    }

    // --- Neighbors --------------------------------------------------------

    /// Leert de L2-next-hop uit een inbound frame: srcIP naar srcMAC. Een
    /// off-subnet bron die ons UNICAST aanspreekt (`to_us`) draagt de
    /// gateway-MAC: zo'n frame kwam door de router. Een broadcast of
    /// multicast van een off-subnet buurman kwam daar níét door en zegt
    /// niets over de gateway: link-local (169.254) apparaten zonder lease,
    /// een ander subnet op hetzelfde L2, SSDP en mDNS. GEMETEN 30-09 op de
    /// Pi 5 aan Dereks LAN: 11 van 14 connects naar buiten liepen op de
    /// deadline terwijl `noroute` en `flowfull` op nul stonden; de
    /// gateway-MAC wees naar een buurman, en alleen een pakket van buiten
    /// zette hem terug, dat zonder juiste MAC niet kwam. Het gateway-paar
    /// veroudert bewust niet: elk unicast pakket van buiten ververst het.
    fn learn(&mut self, src_ip: u32, mac: [u8; 6], now: u64, to_us: bool) {
        if !self.neigh.contains(&src_ip) && self.neigh.is_full() {
            self.neigh.clear(); // plafond: legen en herleren
        }
        self.neigh.insert(src_ip, Neighbor { mac, seen: now });
        if to_us && !self.on_subnet(src_ip) && !bogus_source(src_ip) {
            self.gw = Some(mac);
        }
    }

    /// De dst-MAC om `dst_ip` te bereiken: on-subnet alleen een écht
    /// geleerde, nog verse neighbor, off-subnet de gateway. Vóór 20-08 gaf
    /// dit hier stil de gateway-MAC terug, waardoor er nooit ge-ARP't werd:
    /// de wifi-Brother-jacht.
    ///
    /// De gateway-MAC komt van twee kanten: passief uit elk frame van buiten
    /// (`learn`), en actief uit de neighbor van het gateway-IP. Dat tweede
    /// pad is de les van de eerste Pi 5-boot (30-09): zolang er nooit iets
    /// van buiten kwam, was `gw` leeg, en alles wat de node naar buiten wilde
    /// (SNTP, een download) werd gedropt met "geen spoor". Op QEMU zag je
    /// dat niet, want slirp is de gateway én het hele internet in één hop.
    ///
    /// De router zelf, vers als neighbor van het gateway-IP (zijn ARP, zijn
    /// DHCP en DNS), is gezaghebbend; het passief geleerde paar is de
    /// terugval, zonder ARP. Een verlopen router-neighbor gaat weg; de
    /// SYN-retransmit in `outbound` vraagt hem dan opnieuw, en zo herstelt
    /// een vergiftigd paar binnen een seconde nadat de router antwoordt
    /// (30-09).
    fn l2_for(&mut self, dst_ip: u32, now: u64) -> Option<[u8; 6]> {
        if !self.on_subnet(dst_ip) {
            let gateway = self.uplink.map(|u| u.gateway).unwrap_or(0);
            if gateway != 0
                && self.on_subnet(gateway)
                && let Some(n) = self.neigh.get(&gateway)
            {
                if now.saturating_sub(n.seen) <= NEIGH_TTL {
                    self.gw = Some(n.mac);
                    return self.gw;
                }
                self.neigh.remove(&gateway);
            }
            return self.gw;
        }
        let n = self.neigh.get(&dst_ip)?;
        if now.saturating_sub(n.seen) > NEIGH_TTL {
            self.neigh.remove(&dst_ip); // verlopen: first-contact leert opnieuw
            return None;
        }
        Some(n.mac)
    }

    /// Leert spa naar sha uit een inbound ARP. Alleen on-subnet en spa ≠ 0:
    /// een ARP-probe (spa 0.0.0.0, RFC 5227) draagt geen bruikbaar adres, en
    /// zou via de off-subnet-tak zelfs de gateway-MAC vergiftigen.
    pub(crate) fn arp_learn(&mut self, f: &[u8], now: u64) {
        if f.len() < 42 || be16(f, 12) != ET_ARP {
            return;
        }
        let a = ETH_LEN;
        if be16(f, a) != 1 || be16(f, a + 2) != 0x0800 || byte(f, a + 4) != 6 || byte(f, a + 5) != 4
        {
            return;
        }
        let op = be16(f, a + 6);
        if op != 1 && op != 2 {
            return;
        }
        let spa = be32(f, a + 14);
        if spa != 0 && self.on_subnet(spa) {
            self.learn(spa, mac_at(f, a + 8), now, false);
        }
    }

    /// Een ARP-request voor een on-subnet bestemming. Off-subnet leert
    /// passief (elk pakket van buiten draagt de gateway-MAC); on-subnet
    /// first-contact niet, en daar is dit de enige weg.
    fn arp_for(&mut self, io: &mut impl NatIo, dst_ip: u32, now: u64) {
        let Some(u) = self.uplink else { return };
        if !self.on_subnet(dst_ip) {
            return;
        }
        if self
            .arp_last
            .get(&dst_ip)
            .is_some_and(|t| now.saturating_sub(t) < ARP_EVERY)
        {
            return;
        }
        if !self.arp_last.contains(&dst_ip) && self.arp_last.is_full() {
            self.arp_last.clear();
        }
        self.arp_last.insert(dst_ip, now);
        let mut f = [0u8; 42];
        put_mac(&mut f, 0, &[0xff; 6]);
        put_mac(&mut f, 6, &u.mac);
        put16(&mut f, 12, ET_ARP);
        put16(&mut f, 14, 1); // Ethernet
        put16(&mut f, 16, 0x0800); // IPv4
        f[18] = 6;
        f[19] = 4;
        put16(&mut f, 20, 1); // request
        put_mac(&mut f, 22, &u.mac);
        put32(&mut f, 28, u.ip);
        // tha blijft 0: onbekend, dat is de vraag.
        put32(&mut f, 38, dst_ip);
        io.uplink_tx(&f);
    }

    // --- De vier herschrijfpaden ------------------------------------------

    /// Een frame van de externe NIC; `true` = geclaimd (Go: `natInbound`).
    /// Leert de neighbor, probeert dan een masquerade-antwoord en anders
    /// DNAT.
    pub(crate) fn inbound(&mut self, io: &mut impl NatIo, f: &mut [u8], now: u64) -> bool {
        let Some(ip) = ipv4_l4(f) else { return false };
        let Some(u) = self.uplink else { return false };
        let src_ip = be32(f, ETH_LEN + 12);
        // srcIP 0.0.0.0 (DHCP van een buurman) of een multicast-bron-MAC niet
        // leren: 0.0.0.0 is off-subnet en zou de gateway-MAC vergiftigen; elk
        // apparaat op het LAN dat DHCP't werd dan even "de gateway".
        if src_ip != 0 && byte(f, 6) & 1 == 0 {
            let to_us = mac_at(f, 0) == u.mac;
            self.learn(src_ip, mac_at(f, 6), now, to_us);
        }
        if be32(f, ETH_LEN + 16) != u.ip {
            return false; // niet van ons: de node-stack mag hem hebben
        }
        let dport = be16(f, ip.l4 + 2);
        if self.adoption.as_ref().is_some_and(|a| a.contains(&dport)) {
            // Nog van de overgedragen app; nooit naar de node-stack, want een
            // TCP-reset daar zou een levende appverbinding doden.
            return true;
        }
        if self.reply_in(io, f, ip.l4, ip.proto, u, now) {
            io.stats().nat_reply_in.fetch_add(1, Relaxed);
            return true;
        }
        if self.dnat_in(io, f, ip.l4, ip.proto, u) {
            return true;
        }
        io.stats().nat_in_unmatched.fetch_add(1, Relaxed);
        false
    }

    /// De gedeelde staart van elk pad richting een app: bestemming
    /// herschrijven naar het slot, MAC's zetten, bezorgen.
    #[expect(clippy::too_many_arguments, reason = "Go's dnatToSlotLocked, plat")]
    fn dnat_to_slot(
        io: &mut impl NatIo,
        slot: usize,
        f: &mut [u8],
        l4: usize,
        proto: u8,
        old_ip: u32,
        new_ip: u32,
        old_port: u16,
        new_port: u16,
    ) {
        put32(f, ETH_LEN + 16, new_ip);
        fix_csum32(f, ETH_LEN + 10, old_ip, new_ip);
        rewrite_l4(f, l4, proto, 2, old_ip, new_ip, old_port, new_port);
        put_mac(f, 0, &slot_mac(slot));
        put_mac(f, 6, &HOST_MAC);
        io.deliver(slot, f);
    }

    /// Herschrijft de bron naar het node-IP (SNAT).
    fn snat_src(
        u: Uplink,
        f: &mut [u8],
        l4: usize,
        proto: u8,
        old_ip: u32,
        old_port: u16,
        new_port: u16,
    ) {
        put32(f, ETH_LEN + 12, u.ip);
        fix_csum32(f, ETH_LEN + 10, old_ip, u.ip);
        rewrite_l4(f, l4, proto, 0, old_ip, u.ip, old_port, new_port);
    }

    /// Zet de MAC's en stuurt het frame de externe NIC uit.
    fn tx_uplink(io: &mut impl NatIo, u: Uplink, f: &mut [u8], next_hop: [u8; 6]) {
        put_mac(f, 0, &next_hop);
        put_mac(f, 6, &u.mac);
        io.uplink_tx(f);
    }

    fn reply_in(
        &mut self,
        io: &mut impl NatIo,
        f: &mut [u8],
        l4: usize,
        proto: u8,
        u: Uplink,
        now: u64,
    ) -> bool {
        let peer_ip = be32(f, ETH_LEN + 12);
        let peer_port = be16(f, l4);
        let node_port = be16(f, l4 + 2);
        let Some(id) = self
            .flows
            .by_rev(&RKey(proto, node_port, peer_ip, peer_port))
        else {
            return false;
        };
        if self.expired(id, now) {
            self.remove_flow(id, true);
            return false; // eventueel dezelfde node-poort als publicatie: DNAT mag
        }
        self.note_tcp_flags(id, byte(f, l4 + 13), true);
        let Some(fl) = self.touch(id, now) else {
            return false;
        };
        Self::dnat_to_slot(
            io,
            usize::from(fl.slot),
            f,
            l4,
            proto,
            u.ip,
            fl.slot_ip,
            node_port,
            fl.slot_port,
        );
        true
    }

    fn dnat_in(
        &mut self,
        io: &mut impl NatIo,
        f: &mut [u8],
        l4: usize,
        proto: u8,
        u: Uplink,
    ) -> bool {
        let dport = be16(f, l4 + 2);
        let Some(m) = self.pub_by_node_port(proto, dport) else {
            return false;
        };
        let slot = usize::from(m.slot);
        Self::dnat_to_slot(
            io,
            slot,
            f,
            l4,
            proto,
            u.ip,
            slot_ip4(slot),
            dport,
            m.slot_port,
        );
        true
    }

    /// Een frame van slot `src` richting de gateway; `true` = geclaimd (Go:
    /// `natFromSlot`). Het antwoordpad van een gepubliceerde poort.
    pub(crate) fn slot_reply(
        &mut self,
        io: &mut impl NatIo,
        src: usize,
        f: &mut [u8],
        now: u64,
    ) -> bool {
        let Some(ip) = ipv4_l4(f) else { return false };
        let Some(u) = self.uplink else { return false };
        let sport = be16(f, ip.l4);
        let Some(m) = self
            .pubs
            .iter()
            .find(|p| p.proto == ip.proto && usize::from(p.slot) == src && p.slot_port == sport)
            .copied()
        else {
            return false;
        };
        // Een échte reply begint nooit met een kale SYN: dit pad is stateloos
        // en matcht alleen (proto, slot, poort), dus een app die zélf naar
        // buiten belt en toevallig zijn eigen listen-poort als bronpoort
        // krijgt, werd hier als "antwoord" geclaimd en stil ge-SNAT. UDP kent
        // geen handshake en blijft op de poort-match staan.
        let flags = byte(f, ip.l4 + 13);
        if ip.proto == PROTO_TCP && flags & TCP_SYN != 0 && flags & TCP_ACK == 0 {
            return false;
        }
        let dst = be32(f, ETH_LEN + 16);
        // Draagt de reply het node-IP als bestemming, dan was de client een
        // buurslot (hairpin): terugvertalen via de conntrack en ring-in.
        if dst == u.ip {
            return self.hairpin_back(io, f, ip.l4, ip.proto, m, u, now);
        }
        let Some(next_hop) = self.l2_for(dst, now) else {
            return true; // next-hop onbekend: drop, de retransmit leert hem
        };
        let src_ip = be32(f, ETH_LEN + 12);
        Self::snat_src(u, f, ip.l4, ip.proto, src_ip, sport, m.node_port);
        Self::tx_uplink(io, u, f, next_hop);
        true
    }

    /// Een app belt naar buiten (Go: `natOutbound`). Masquerade, dan de
    /// externe NIC uit. `true` = afgehandeld, ook als het gedropt is.
    pub(crate) fn outbound(
        &mut self,
        io: &mut impl NatIo,
        src: usize,
        f: &mut [u8],
        now: u64,
    ) -> bool {
        let Some(ip) = ipv4_l4(f) else { return false };
        let Some(u) = self.uplink else { return false };
        let dst_ip = be32(f, ETH_LEN + 16);
        let slot_ip = slot_ip4(src);
        let sport = be16(f, ip.l4);
        let dport = be16(f, ip.l4 + 2);
        let flags = byte(f, ip.l4 + 13);
        let proto = ip.proto;

        // Het node-IP zelf: hairpin, nooit de NIC uit; vóór l2_for, anders
        // stuurt first-contact een ARP-request naar ons eigen IP.
        if dst_ip == u.ip {
            return self.hairpin_out(io, src, f, ip.l4, proto, slot_ip, sport, dport, u, now);
        }

        let hop = self.l2_for(dst_ip, now);
        let known = hop.is_some();
        let next_hop = match hop {
            Some(m) => m,
            None => {
                // First-contact (Altra 14-07): een on-subnet bestemming die
                // ons nooit iets stuurde is onbekend; vraag het net. Een
                // off-subnet bestemming vraagt de gateway (Pi 5, 30-09).
                let ask = if self.on_subnet(dst_ip) {
                    dst_ip
                } else {
                    u.gateway
                };
                self.arp_for(io, ask, now);
                let Some(gw) = self.gw else {
                    io.stats().nat_no_route.fetch_add(1, Relaxed);
                    return true; // geen enkel spoor: drop, retransmit volgt
                };
                // En stuur het frame alvast via de gateway mee (Brother-jacht
                // 20-08): er bestaan hosts die broadcast-ARP niet beantwoorden
                // (wifi-powersave, mesh). Relayt de router, dan antwoordt de
                // host óns rechtstreeks en is de neighbor alsnog geleerd.
                gw
            }
        };
        let Some((id, created)) =
            self.flow_for_packet(io, proto, src, slot_ip, sport, dst_ip, dport, flags, now)
        else {
            io.stats().nat_flow_full.fetch_add(1, Relaxed);
            return true; // pool vol: drop
        };
        // Een tweede kale SYN op dezelfde levende flow is de retransmit van
        // de TCP-stack: de eerste SYN was de gratis unicast-probe. Vraag dan
        // óók broadcast-ARP, zonder eigen NUD-machine of timer; de
        // rate-limit begrenst hem op één per seconde. Off-subnet gaat de
        // vraag naar de router: een vergiftigd gateway-paar (30-09) herstelt
        // zo op zijn antwoord, in plaats van nooit.
        if known && !created && proto == PROTO_TCP && flags & TCP_SYN != 0 && flags & TCP_ACK == 0 {
            let ask = if self.on_subnet(dst_ip) {
                dst_ip
            } else {
                u.gateway
            };
            self.arp_for(io, ask, now);
        }
        let reap = self.note_tcp_flags(id, flags, false);
        let Some(np) = self.flows.get(id).map(|f| f.node_port) else {
            return true;
        };
        Self::snat_src(u, f, ip.l4, proto, slot_ip, sport, np);
        Self::tx_uplink(io, u, f, next_hop);
        if reap {
            self.remove_flow(id, true);
        }
        true
    }

    /// Een app belt het node-IP: de gepubliceerde poort van een buurslot.
    /// DNAT en masquerade in één beweging, dan ring-in bij de dienst. Niets
    /// gepubliceerd op die poort = drop, zoals een dichte poort.
    #[expect(clippy::too_many_arguments, reason = "Go's hairpinOutLocked, plat")]
    fn hairpin_out(
        &mut self,
        io: &mut impl NatIo,
        src: usize,
        f: &mut [u8],
        l4: usize,
        proto: u8,
        slot_ip: u32,
        sport: u16,
        dport: u16,
        u: Uplink,
        now: u64,
    ) -> bool {
        let Some(m) = self.pub_by_node_port(proto, dport) else {
            return true;
        };
        let srv = usize::from(m.slot);
        let srv_ip = slot_ip4(srv);
        let flags = byte(f, l4 + 13);
        let Some((id, _)) = self.flow_for_packet(
            io,
            proto,
            src,
            slot_ip,
            sport,
            srv_ip,
            m.slot_port,
            flags,
            now,
        ) else {
            io.stats().nat_flow_full.fetch_add(1, Relaxed);
            return true;
        };
        let reap = self.note_tcp_flags(id, flags, false);
        let Some(np) = self.flows.get(id).map(|f| f.node_port) else {
            return true;
        };
        Self::snat_src(u, f, l4, proto, slot_ip, sport, np);
        Self::dnat_to_slot(io, srv, f, l4, proto, u.ip, srv_ip, dport, m.slot_port);
        if reap {
            self.remove_flow(id, true);
        }
        true
    }

    /// De reply van de dienst op een hairpin-verbinding: de conntrack wijst
    /// de beller aan; beide kanten terugschrijven en ring-in bezorgen. Geen
    /// flow = drop.
    #[expect(clippy::too_many_arguments, reason = "Go's hairpinBackLocked, plat")]
    fn hairpin_back(
        &mut self,
        io: &mut impl NatIo,
        f: &mut [u8],
        l4: usize,
        proto: u8,
        m: Pub,
        u: Uplink,
        now: u64,
    ) -> bool {
        let srv_ip = slot_ip4(usize::from(m.slot));
        let np = be16(f, l4 + 2);
        let Some(id) = self.flows.by_rev(&RKey(proto, np, srv_ip, m.slot_port)) else {
            return true;
        };
        if self.expired(id, now) {
            self.remove_flow(id, true);
            return true;
        }
        self.note_tcp_flags(id, byte(f, l4 + 13), true);
        let Some(fl) = self.touch(id, now) else {
            return true;
        };
        // Eerst de src-helft, dan de dst-helft (die ook bezorgt): disjuncte
        // velden en incrementele checksums, dus de volgorde is vrij.
        Self::snat_src(u, f, l4, proto, srv_ip, m.slot_port, m.node_port);
        Self::dnat_to_slot(
            io,
            usize::from(fl.slot),
            f,
            l4,
            proto,
            u.ip,
            fl.slot_ip,
            np,
            fl.slot_port,
        );
        true
    }

    // --- Conntrack --------------------------------------------------------

    /// Maakt voor gewone uitgaande pakketten zo nodig een mapping. Een RST
    /// mag alleen een bestaande sluiten: zonder mapping ontbreekt het
    /// node-poortnummer dat de peer kent. Geeft `(flow, nieuw aangemaakt)`.
    #[expect(clippy::too_many_arguments, reason = "de 5-tupel plus context")]
    fn flow_for_packet(
        &mut self,
        io: &mut impl NatIo,
        proto: u8,
        slot: usize,
        slot_ip: u32,
        slot_port: u16,
        dst_ip: u32,
        dst_port: u16,
        flags: u8,
        now: u64,
    ) -> Option<(Id, bool)> {
        let k = FKey(proto, slot_ip, dst_ip, slot_port, dst_port);
        if proto == PROTO_TCP && flags & TCP_RST != 0 {
            return self.lookup_flow(&k, now).map(|id| (id, false));
        }
        self.flow_for(io, proto, slot, slot_ip, slot_port, dst_ip, dst_port, now)
    }

    /// Vindt of maakt de conntrack-entry van een uitgaande flow; `None` als
    /// de pool vol is. Een volle pool veegt hoogstens eens per
    /// `FLOW_SWEEP_EVERY` en logt hoogstens eens per `FLOW_LOG_EVERY`, zodat
    /// een dader niet via het weigerpad core 0 kan gijzelen.
    #[expect(clippy::too_many_arguments, reason = "de 5-tupel plus context")]
    pub(crate) fn flow_for(
        &mut self,
        io: &mut impl NatIo,
        proto: u8,
        slot: usize,
        slot_ip: u32,
        slot_port: u16,
        dst_ip: u32,
        dst_port: u16,
        now: u64,
    ) -> Option<(Id, bool)> {
        // Een aangesloten app kan zenden vóór zijn oude conntrack terug is.
        // Een nieuwe mapping zou de oude tupel en poort voor het herstel
        // verbergen; de retransmit na het einde van de adoptie volgt.
        if self.adoption.is_some() {
            return None;
        }
        let k = FKey(proto, slot_ip, dst_ip, slot_port, dst_port);
        if let Some(id) = self.lookup_flow(&k, now) {
            return Some((id, false));
        }
        let s = slot_byte(slot, SLOT_CAP)?;
        if self.flows.is_full() || self.flows.count(slot) >= MAX_FLOWS_PER_SLOT {
            self.maybe_sweep(now);
            // Als de veeg niets opleverde: recycle de oudste al gesloten flow
            // van dit slot. Een flow met beide FIN's houdt zijn plek alleen
            // nog vast voor late retransmits; een nieuwe verbinding heeft meer
            // recht op de tabel dan een dode. GEMETEN 21-09 op de Pi 5 (L83
            // p51): 200 korte verbindingen per storm vullen de 512 van een
            // slot sneller dan die minuut ze vrijgeeft, en dan dropte dit pad
            // de SYN; in zes van twaalf rondes, met `nat_flowfull` als bewijs.
            if (self.flows.is_full() || self.flows.count(slot) >= MAX_FLOWS_PER_SLOT)
                && let Some(old) = self.oldest_closed(s)
            {
                self.flows.remove(old);
            }
        }
        if self.flows.is_full() {
            if now >= self.next_full_log {
                io.log(format_args!(
                    "HOPOS_MASQ_FULL: conntrack full ({}), new outbound flows dropped",
                    crate::flows::MAX_FLOWS
                ));
                self.next_full_log = now.saturating_add(FLOW_LOG_EVERY);
            }
            return None;
        }
        let n = self.flows.count(slot);
        if n >= MAX_FLOWS_PER_SLOT {
            if let Some(t) = self.next_slot_full_log.get_mut(slot)
                && now >= *t
            {
                io.log(format_args!(
                    "HOPOS_MASQ_SLOT_FULL: slot {slot} has {n} flows (max {MAX_FLOWS_PER_SLOT}), new outbound flow dropped"
                ));
                *t = now.saturating_add(FLOW_LOG_EVERY);
            }
            return None;
        }
        let np = self.alloc_port(proto, dst_ip, dst_port)?;
        let id = self.flows.insert(Flow {
            proto,
            slot: s,
            fin_fwd: false,
            fin_rev: false,
            slot_ip,
            dst_ip,
            slot_port,
            dst_port,
            node_port: np,
            seen: now,
        })?;
        Some((id, true))
    }

    /// Vindt een verse mapping, werkt zijn idle-klok bij en ruimt een
    /// verlopen mapping op.
    fn lookup_flow(&mut self, k: &FKey, now: u64) -> Option<Id> {
        let id = self.flows.by_fwd(k)?;
        if self.expired(id, now) {
            self.remove_flow(id, true);
            return None;
        }
        self.touch(id, now)?;
        Some(id)
    }

    fn touch(&mut self, id: Id, now: u64) -> Option<Flow> {
        let fl = self.flows.get_mut(id)?;
        fl.seen = now;
        Some(*fl)
    }

    /// De oudste al gesloten flow van `slot` uit een begrensde steekproef.
    fn oldest_closed(&mut self, slot: u8) -> Option<Id> {
        const SAMPLE: usize = 64;
        let mut best: Option<(Id, u64)> = None;
        let ids: BoundedVec<Id, SAMPLE> = self.flows.sample();
        for &id in ids.iter() {
            let Some(fl) = self.flows.get(id) else {
                continue;
            };
            if fl.slot != slot || !fl.fin_fwd || !fl.fin_rev {
                continue;
            }
            if best.is_none_or(|(_, seen)| fl.seen < seen) {
                best = Some((id, fl.seen));
            }
        }
        best.map(|(id, _)| id)
    }

    /// Het enige verwijderpad voor de conntrack.
    fn remove_flow(&mut self, id: Id, compact: bool) -> bool {
        let removed = self.flows.remove(id);
        if compact {
            self.flows.maybe_compact();
        }
        removed
    }

    /// Kiest een vrije node-poort: rollend door het masquerade-bereik, niet
    /// botsend met een lopende flow naar dezelfde peer, noch met een
    /// gepubliceerde poort (die is voor DNAT).
    fn alloc_port(&mut self, proto: u8, dst_ip: u32, dst_port: u16) -> Option<u16> {
        let flows = &self.flows;
        let pubs = &self.pubs;
        alloc_port_by(&mut self.masq_next, |p| {
            flows.by_rev(&RKey(proto, p, dst_ip, dst_port)).is_some()
                || pubs.iter().any(|q| q.proto == proto && q.node_port == p)
        })
    }

    /// Verwerkt alleen de terminale hints waarvoor geen volledige
    /// TCP-state-machine nodig is. Een RST van de slotkant mag na het
    /// doorsturen meteen weg; een inbound RST niet, want zonder
    /// sequence-tracking weet de NAT niet of de ontvanger hem accepteert.
    /// FIN wordt per richting onthouden. Geeft "opruimen na doorsturen".
    fn note_tcp_flags(&mut self, id: Id, flags: u8, reverse: bool) -> bool {
        let Some(fl) = self.flows.get_mut(id) else {
            return false;
        };
        if fl.proto != PROTO_TCP {
            return false;
        }
        if flags & TCP_RST != 0 {
            if reverse {
                // Een inbound RST: de flow blijft voor late pakketten
                // (zonder sequence-tracking weet de NAT niet of de
                // ontvanger hem accepteert), maar telt als gesloten in
                // beide richtingen: de sluit-TTL geldt en de recycler mag
                // hem bij een volle tabel nemen. Zonder dit bleef een
                // RST-gesloten flow TCP_IDLE (300 s) staan en liep de
                // tabel van 512 per slot in een storm vol (Pi 5 en Pi 4,
                // 30-09: HOPOS_MASQ_SLOT_FULL, de SYN pas na 1 s).
                fl.fin_fwd = true;
                fl.fin_rev = true;
                return false;
            }
            return true;
        }
        if flags & TCP_FIN != 0 {
            if reverse {
                fl.fin_rev = true;
            } else {
                fl.fin_fwd = true;
            }
        }
        false
    }

    fn expired(&self, id: Id, now: u64) -> bool {
        self.flows.get(id).is_some_and(|fl| flow_expired(fl, now))
    }

    /// Verwijdert alle verlopen flows.
    pub(crate) fn sweep_expired(&mut self, now: u64) {
        let mut removed = false;
        for id in 0..crate::flows::MAX_FLOWS as Id {
            if self.expired(id, now) {
                removed |= self.flows.remove(id);
            }
        }
        if removed {
            self.flows.maybe_compact();
        }
    }

    /// De veeg van de flow-expiry-tik: maakt de idle-timeouts echt, ook
    /// onder de quota en zonder een nieuwe allocatie.
    pub fn sweep(&mut self, now: u64) {
        self.sweep_expired(now);
        self.next_sweep = now.saturating_add(FLOW_SWEEP_EVERY);
    }

    /// Een drukgedreven veeg, begrensd in tempo.
    fn maybe_sweep(&mut self, now: u64) {
        if self.next_sweep != 0 && now < self.next_sweep {
            return;
        }
        self.sweep(now);
    }

    /// Het aantal levende flows.
    #[must_use]
    pub fn flow_count(&self) -> usize {
        self.flows.len()
    }

    // --- Kern-flip --------------------------------------------------------

    /// Houdt `node_ports` vast voor de overgedragen apps, vóór de externe RX
    /// start. Pakketten wachten niet in een nieuwe rij; TCP probeert opnieuw
    /// zodra de gewone routering klaarstaat.
    pub fn hold_adoption(&mut self, node_ports: &[u16]) -> Result<(), Error> {
        let mut v = BoundedVec::new();
        for &p in node_ports {
            v.push(p)
                .map_err(|_| Error::Full("adoption ports", MAX_ADOPT))?;
        }
        self.adoption = Some(v);
        Ok(())
    }

    /// Geeft de tijdelijke claims vrij ná slot- en NAT-herstel.
    pub fn finish_adoption(&mut self) {
        self.adoption = None;
    }

    /// Beschrijft de levende conntrack in `out` voor het handoff-blob.
    /// Verlopen flows gaan niet mee. Past niet alles, dan gaat het eerste
    /// deel mee (de staart heeft plaats voor `MAX_FLOWS`).
    pub fn snapshot<'o>(&self, now: u64, out: &'o mut [FlowState]) -> NatState<'o> {
        let mut n = 0;
        for id in self.flows.ids() {
            let Some(fl) = self.flows.get(id) else {
                continue;
            };
            if flow_expired(fl, now) || fl.slot < 1 {
                continue;
            }
            let Some(o) = out.get_mut(n) else { break };
            *o = FlowState {
                proto: fl.proto,
                slot: fl.slot,
                fins: u8::from(fl.fin_fwd) | (u8::from(fl.fin_rev) << 1),
                slot_port: fl.slot_port,
                dst_port: fl.dst_port,
                node_port: fl.node_port,
                slot_ip: fl.slot_ip,
                dst_ip: fl.dst_ip,
            };
            n += 1;
        }
        let masq_next = self.masq_next;
        let gw_mac = self.gw;
        NatState {
            flows: out.get(..n).unwrap_or(&[]),
            masq_next,
            gw_mac,
        }
    }

    /// Bouwt de conntrack terug en geeft het aantal herstelde flows. Na de
    /// slot-adoptie aanroepen: een flow van een slot dat de flip niet
    /// overleefde (`attached` zegt nee) hoort niet te blijven staan, en zijn
    /// poort hoort vrij te komen.
    pub fn restore(
        &mut self,
        s: &NatState<'_>,
        attached: impl Fn(usize) -> bool,
        now: u64,
    ) -> usize {
        if let Some(m) = s.gw_mac {
            self.gw = Some(m);
        }
        if (MASQ_BASE..MASQ_END).contains(&s.masq_next) {
            self.masq_next = s.masq_next;
        }
        let mut n = 0;
        for f in s.flows {
            let slot = usize::from(f.slot);
            if !(1..=SLOT_CAP).contains(&slot) || !attached(slot) {
                continue; // het slot leeft niet meer: de flow ook niet
            }
            if self.flows.is_full() || self.flows.count(slot) >= MAX_FLOWS_PER_SLOT {
                continue;
            }
            let fl = Flow {
                proto: f.proto,
                slot: f.slot,
                fin_fwd: f.fins & 1 != 0,
                fin_rev: f.fins & 2 != 0,
                slot_ip: f.slot_ip,
                dst_ip: f.dst_ip,
                slot_port: f.slot_port,
                dst_port: f.dst_port,
                node_port: f.node_port,
                seen: now,
            };
            if self.flows.by_fwd(&fl.fkey()).is_some() || self.flows.by_rev(&fl.rkey()).is_some() {
                continue;
            }
            if self.flows.insert(fl).is_some() {
                n += 1;
            }
        }
        n
    }
}

/// Het slotnummer als byte, als het in 1..=`max` ligt.
fn slot_byte(slot: usize, max: usize) -> Option<u8> {
    if slot < 1 || slot > max.min(SLOT_CAP) {
        return None;
    }
    u8::try_from(slot).ok()
}

fn flow_idle(fl: &Flow) -> u64 {
    if fl.proto != PROTO_TCP {
        UDP_IDLE
    } else if fl.fin_fwd && fl.fin_rev {
        TCP_CLOSING_IDLE
    } else {
        TCP_IDLE
    }
}

fn flow_expired(fl: &Flow, now: u64) -> bool {
    now.saturating_sub(fl.seen) > flow_idle(fl)
}

/// De poortkeuze los van de tabellen, zodat de uitputting te toetsen is
/// zonder 29k flows (de conntrack draagt er maar 4096).
fn alloc_port_by(next: &mut u16, busy: impl Fn(u16) -> bool) -> Option<u16> {
    for _ in MASQ_BASE..MASQ_END {
        let p = *next;
        *next = if p + 1 >= MASQ_END { MASQ_BASE } else { p + 1 };
        if !busy(p) {
            return Some(p);
        }
    }
    None
}

#[cfg(test)]
mod tests;
