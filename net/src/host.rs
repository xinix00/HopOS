//! HOP's poort 0: de node-stack aan de switch (Go: `hostDevice` in
//! `hopswitch.go`, en `locnet.go`, `internal.go` in `hopnet`).
//!
//! De node-stack heeft één NIC en drie soorten frames die de draad nooit op
//! mogen; die vangen we allemaal op déze naad, zodat de stack zelf nergens
//! van hoeft te weten:
//!
//! 1. self-dial: dst-MAC is onze eigen MAC, dus terug de eigen RX-rij in. De
//!    agent belt de leader op het eigen externe IP (de S3-lock adverteert dat
//!    adres).
//! 2. ARP naar het eigen IP: zelf beantwoorden; niemand op het LAN gaat onze
//!    vraag naar ons eigen adres beantwoorden.
//! 3. het interne subnet (10.100.0.0/24): niet de draad op maar de switch
//!    in, via de statische 1:1-gateway-vertaling ([`crate::gw`]).
//!
//! De rest gaat als [`KIND_UPLINK`] de host-TX-ring in; de switch zet hem in
//! de egress-rij van de pomp. Zo voedt alleen de switch de uplink, en
//! bestaan Go's `uplinkTxMu` en `hostDevice.txMu` niet meer: elke ring heeft
//! één producer, en die is een taak.

use crate::gw;
use crate::plan::is_internal;
use crate::ring::{KIND_FRAME, KIND_UPLINK, Reader, Writer};
use crate::wire::{ET_ARP, ET_IPV4, ETH_LEN, be16, be32, mac_at, put_mac, put16, put32};
use crate::{Error, Frame, Result, Stats};
use alloc::vec::Vec;
use core::sync::atomic::Ordering::Relaxed;
use executor::Executor;
use sync::{Either, Signal, Stop, select, yield_now};

/// De lokale rij (self-dial, ARP-antwoorden aan onszelf): node-intern
/// verkeer, een handvol frames volstaat. Vol = drop, TCP herstelt; nooit
/// ongebonden groeien op een app-gedreven pad.
pub const LOOPBACK_QUEUE: usize = 64;

/// Frames per richting per ronde, zodat de host-taak de rest niet
/// verhongert.
pub const HOST_BURST: usize = 32;

/// De node-netstack zoals poort 0 hem ziet. De stack zelf komt uit
/// `leannet`; dit is de naad.
pub trait HostStack {
    /// Eén frame de stack in.
    fn receive(&mut self, frame: &[u8]);

    /// Het volgende frame dat de stack wil zenden, in `buf`; `None` als er
    /// niets ligt.
    fn poll_transmit(&mut self, buf: &mut [u8]) -> Option<usize>;

    /// Het vroegste moment (nanoseconden op de klok van de executor) waarop
    /// de stack gepolld wil worden voor zijn timers (retransmit, keepalive);
    /// `None` = alleen op een bel. Wie buiten de host-taak iets aan de stack
    /// geeft (een socket-write), zet de bel van de host-taak.
    fn poll_at(&self) -> Option<u64> {
        None
    }
}

/// De host-kant van poort 0 plus de node-stack.
pub struct HostPort<'a, S, R, W> {
    stack: S,
    /// Switch → HOP (leeskant).
    rx: R,
    /// HOP → switch (schrijfkant).
    tx: W,
    /// De bel van deze taak: de switch zet hem na een leeg→niet-leeg-schrijf.
    bell: &'a Signal,
    /// De deur van de switch: na een schrijf in de TX-ring.
    door: &'a Signal,
    stats: &'a Stats,
    /// De MAC en het IP van de node-stack op de uplink.
    mac: [u8; 6],
    ip: u32,
    max_slots: usize,
    loopback: Vec<Frame>,
    lb_head: usize,
    lb_len: usize,
}

impl<'a, S: HostStack, R: Reader, W: Writer> HostPort<'a, S, R, W> {
    /// Poort 0 voor `stack` met extern adres `ip`/`mac`. Alloceert de
    /// lokale rij (boot).
    #[expect(
        clippy::too_many_arguments,
        reason = "de bedrading van poort 0, één keer bij boot"
    )]
    pub fn new(
        stack: S,
        rx: R,
        tx: W,
        bell: &'a Signal,
        door: &'a Signal,
        stats: &'a Stats,
        mac: [u8; 6],
        ip: u32,
        max_slots: usize,
    ) -> Result<Self> {
        let mut loopback = Vec::new();
        loopback
            .try_reserve_exact(LOOPBACK_QUEUE)
            .map_err(|_| Error::OutOfMemory(LOOPBACK_QUEUE * core::mem::size_of::<Frame>()))?;
        loopback.resize(LOOPBACK_QUEUE, Frame::new());
        Ok(Self {
            stack,
            rx,
            tx,
            bell,
            door,
            stats,
            mac,
            ip,
            max_slots,
            loopback,
            lb_head: 0,
            lb_len: 0,
        })
    }

    /// De stack, voor wie hem van buiten de lus aanraakt (dezelfde taak).
    pub fn stack(&mut self) -> &mut S {
        &mut self.stack
    }

    fn enqueue(&mut self, p: &[u8]) {
        if self.lb_len >= LOOPBACK_QUEUE {
            self.stats.host_rx_drops.fetch_add(1, Relaxed);
            return;
        }
        let i = (self.lb_head + self.lb_len) % LOOPBACK_QUEUE;
        if let Some(f) = self.loopback.get_mut(i)
            && f.set(p)
        {
            self.lb_len += 1;
        } else {
            self.stats.host_rx_drops.fetch_add(1, Relaxed);
        }
    }

    fn drain_loopback(&mut self) -> bool {
        let worked = self.lb_len > 0;
        while self.lb_len > 0 {
            let i = self.lb_head;
            self.lb_head = (self.lb_head + 1) % LOOPBACK_QUEUE;
            self.lb_len -= 1;
            if let Some(f) = self.loopback.get(i) {
                self.stack.receive(f.bytes());
            }
        }
        worked
    }

    /// Eén frame van de stack de naad over (Go: `locdev.Transmit`).
    fn transmit(&mut self, p: &mut [u8]) {
        if p.len() >= ETH_LEN {
            if mac_at(p, 0) == self.mac {
                self.enqueue(p); // self-dial: nooit de draad op
                return;
            }
            if let Some(r) = self.arp_self_reply(p) {
                self.enqueue(&r);
                return;
            }
            if be16(p, 12) == ET_IPV4
                && p.len() >= ETH_LEN + 20
                && is_internal(be32(p, ETH_LEN + 16))
            {
                // Intern verkeer verlaat de node nooit: ook een frame dat de
                // vertaling weigert (fragment, vreemd slot) gaat niet de
                // draad op, dat zou het interne adresplan naar buiten lekken.
                if gw::from_host(p, self.ip, self.max_slots) {
                    self.write(KIND_FRAME, p);
                } else {
                    self.stats.host_rx_drops.fetch_add(1, Relaxed);
                }
                return;
            }
        }
        self.write(KIND_UPLINK, p);
    }

    fn write(&mut self, kind: u32, p: &[u8]) {
        // Geen wachten: de consument is de switch op déze core, en die kan
        // pas draaien als wij afgeven. Vol = drop, TCP herstelt.
        if self.tx.write_notify(kind, p).is_none() {
            self.stats.host_tx_drops.fetch_add(1, Relaxed);
        }
    }

    /// Beantwoordt een ARP-request naar het eigen IP (RFC 826).
    fn arp_self_reply(&self, p: &[u8]) -> Option<[u8; ETH_LEN + 28]> {
        let a = ETH_LEN;
        if p.len() < a + 28 || be16(p, 12) != ET_ARP {
            return None;
        }
        if be16(p, a) != 1 || be16(p, a + 2) != 0x0800 || be16(p, a + 6) != 1 {
            return None;
        }
        if be32(p, a + 24) != self.ip {
            return None; // niet ons adres: gewoon de draad op
        }
        let mut r = [0u8; ETH_LEN + 28];
        put_mac(&mut r, 0, &mac_at(p, a + 8));
        put_mac(&mut r, 6, &self.mac);
        put16(&mut r, 12, ET_ARP);
        put16(&mut r, a, 1);
        put16(&mut r, a + 2, 0x0800);
        r[a + 4] = 6;
        r[a + 5] = 4;
        put16(&mut r, a + 6, 2);
        put_mac(&mut r, a + 8, &self.mac);
        put32(&mut r, a + 14, self.ip);
        put_mac(&mut r, a + 18, &mac_at(p, a + 8));
        put32(&mut r, a + 24, be32(p, a + 14));
        Some(r)
    }

    /// Het system-service-IP is een capability die uitsluitend uit een
    /// slotring mag komen. Een fysiek LAN-frame met zo'n bronadres is spoof.
    fn external_spoofs_internal(p: &[u8]) -> bool {
        p.len() >= ETH_LEN + 20 && be16(p, 12) == ET_IPV4 && is_internal(be32(p, ETH_LEN + 12))
    }

    /// Eén ronde: de lokale rij en de host-RX-ring de stack in, dan wat de
    /// stack wil zenden de naad over. `buf` is de ene framebuffer van de
    /// taak (groot genoeg voor een LAN-jumbo). `true` = er was werk.
    pub fn pass(&mut self, buf: &mut [u8]) -> bool {
        let mut worked = self.drain_loopback();
        for _ in 0..HOST_BURST {
            let Some((kind, n)) = self.rx.read_into(buf) else {
                break;
            };
            let Some(f) = buf.get_mut(..n) else { break };
            worked = true;
            match kind {
                // Op de LAN-ring is HOP 10.100.0.1 met MAC slot 0; de stack
                // houdt zijn echte adres. Vertalen op deze naad, zonder rij.
                KIND_FRAME if gw::to_host(f, self.ip) => {
                    put_mac(f, 0, &self.mac);
                    self.stack.receive(f);
                }
                KIND_UPLINK if !Self::external_spoofs_internal(f) => self.stack.receive(f),
                _ => {
                    self.stats.host_rx_drops.fetch_add(1, Relaxed);
                }
            }
        }
        let mut sent = false;
        for _ in 0..HOST_BURST {
            let Some(n) = self.stack.poll_transmit(buf) else {
                break;
            };
            let Some(f) = buf.get_mut(..n) else { break };
            self.transmit(f);
            sent = true;
        }
        if sent {
            // De switch nú wekken: de bel via de deur van de executor werkt
            // alleen als HOP idle is, en onder verkeer is HOP dat niet.
            self.door.set();
        }
        worked | sent
    }

    /// De lus van de host-taak. Keert terug als `stop` luidt.
    pub async fn run<const T: usize, const M: usize>(
        &mut self,
        exec: &'static Executor<T, M>,
        buf: &mut [u8],
        stop: &Stop,
    ) {
        loop {
            if stop.is_set() {
                return;
            }
            if self.pass(buf) {
                yield_now().await;
                continue;
            }
            let woke = match self.stack.poll_at() {
                Some(at) => {
                    match select(stop.wait(), select(self.bell.wait(), exec.until(at))).await {
                        Either::Left(()) => true,
                        Either::Right(_) => false,
                    }
                }
                None => matches!(
                    select(stop.wait(), self.bell.wait()).await,
                    Either::Left(())
                ),
            };
            if woke {
                return;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::plan::{HOST_MAC, SLOT_CAP, host_ip4, slot_ip4, slot_mac};
    use crate::ring::mem::{self, MemReader, MemWriter};
    use crate::wire::PROTO_TCP;
    use crate::wire::testutil::*;
    use std::collections::VecDeque;

    const IP: u32 = 0x0A00_020F;
    const MAC: [u8; 6] = [0x52, 0x54, 0x00, 0x12, 0x34, 0x56];

    #[derive(Default)]
    struct FakeStack {
        got: Vec<Vec<u8>>,
        out: VecDeque<Vec<u8>>,
    }

    impl HostStack for FakeStack {
        fn receive(&mut self, f: &[u8]) {
            self.got.push(f.to_vec());
        }
        fn poll_transmit(&mut self, buf: &mut [u8]) -> Option<usize> {
            let f = self.out.pop_front()?;
            buf[..f.len()].copy_from_slice(&f);
            Some(f.len())
        }
    }

    struct T {
        port: HostPort<'static, FakeStack, MemReader, MemWriter>,
        /// De switch-kant: leest de host-TX, schrijft de host-RX.
        sw_tx: MemReader,
        sw_rx: MemWriter,
        door: &'static Signal,
    }

    fn setup() -> T {
        let (sw_tx, tx) = mem::pair(256 << 10);
        let (rx, sw_rx) = mem::pair(256 << 10);
        let bell: &'static Signal = Box::leak(Box::new(Signal::new()));
        let door: &'static Signal = Box::leak(Box::new(Signal::new()));
        let stats: &'static Stats = Box::leak(Box::new(Stats::new()));
        let port = HostPort::new(
            FakeStack::default(),
            rx,
            tx,
            bell,
            door,
            stats,
            MAC,
            IP,
            SLOT_CAP,
        )
        .unwrap();
        T {
            port,
            sw_tx,
            sw_rx,
            door,
        }
    }

    fn pass(t: &mut T) -> bool {
        let mut buf = vec![0u8; crate::plan::MAX_LAN_FRAME];
        t.port.pass(&mut buf)
    }

    #[test]
    fn self_dial_gaat_nooit_de_draad_op() {
        let mut t = setup();
        let f = mk_frame(PROTO_TCP, MAC, MAC, IP, IP, 1, 2, b"zelf");
        t.port.stack().out.push_back(f.clone());
        assert!(pass(&mut t));
        assert!(t.sw_tx.pop().is_none(), "self-dial ging de switch in");
        assert!(pass(&mut t));
        assert_eq!(t.port.stack().got, [f]);
    }

    #[test]
    fn arp_naar_het_eigen_ip_beantwoordt_de_naad() {
        let mut t = setup();
        let mut req = ether_frame([0xff; 6], MAC, 0x0806);
        req.resize(ETH_LEN + 28, 0);
        put16(&mut req, 14, 1);
        put16(&mut req, 16, 0x0800);
        req[18] = 6;
        req[19] = 4;
        put16(&mut req, 20, 1);
        req[22..28].copy_from_slice(&MAC);
        put32(&mut req, 28, IP);
        put32(&mut req, 38, IP);
        t.port.stack().out.push_back(req);
        pass(&mut t);
        pass(&mut t);
        let got = &t.port.stack().got;
        assert_eq!(got.len(), 1);
        assert_eq!((be16(&got[0], 20), be32(&got[0], 28)), (2, IP));
        assert!(t.sw_tx.pop().is_none());
    }

    #[test]
    fn intern_verkeer_gaat_vertaald_de_switch_in() {
        let mut t = setup();
        let f = mk_frame(
            PROTO_TCP,
            [0xaa; 6],
            MAC,
            IP,
            slot_ip4(3),
            8080,
            40000,
            b"x",
        );
        t.port.stack().out.push_back(f);
        assert!(pass(&mut t));
        assert!(t.door.is_set(), "switch niet gewekt");
        let (kind, got) = t.sw_tx.pop().unwrap();
        assert_eq!(kind, KIND_FRAME);
        assert_eq!(be32(&got, ETH_LEN + 12), host_ip4());
        assert_eq!((mac_at(&got, 0), mac_at(&got, 6)), (slot_mac(3), HOST_MAC));
        check_frame(&got, "intern");
        // Een intern frame dat de vertaling weigert, lekt niet naar buiten.
        let bad = mk_frame(PROTO_TCP, [0xaa; 6], MAC, IP, host_ip4(), 8080, 40000, &[]);
        t.port.stack().out.push_back(bad);
        pass(&mut t);
        assert!(t.sw_tx.pop().is_none());
    }

    #[test]
    fn extern_verkeer_gaat_als_uplink_de_switch_in() {
        let mut t = setup();
        let f = mk_frame(PROTO_TCP, [0xaa; 6], MAC, IP, 0x0808_0808, 5555, 53, &[]);
        t.port.stack().out.push_back(f.clone());
        pass(&mut t);
        assert_eq!(t.sw_tx.pop(), Some((KIND_UPLINK, f)));
    }

    #[test]
    fn rx_vertaalt_gateway_en_weigert_spoof() {
        let mut t = setup();
        let app = mk_frame(
            PROTO_TCP,
            HOST_MAC,
            slot_mac(1),
            slot_ip4(1),
            host_ip4(),
            5555,
            9080,
            b"hoi",
        );
        assert!(t.sw_rx.push(KIND_FRAME, &app));
        let ext = mk_frame(PROTO_TCP, MAC, [0xbb; 6], 0x0808_0808, IP, 53, 5555, &[]);
        assert!(t.sw_rx.push(KIND_UPLINK, &ext));
        let spoof = mk_frame(PROTO_TCP, MAC, [0xbb; 6], slot_ip4(4), IP, 53, 5555, &[]);
        assert!(t.sw_rx.push(KIND_UPLINK, &spoof));
        pass(&mut t);
        let got = &t.port.stack().got;
        assert_eq!(got.len(), 2, "spoof met intern bronadres bereikte de stack");
        assert_eq!(be32(&got[0], ETH_LEN + 16), IP);
        assert_eq!(mac_at(&got[0], 0), MAC);
        check_frame(&got[0], "gateway naar host");
        assert_eq!(got[1], ext);
    }
}
