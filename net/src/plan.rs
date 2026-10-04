//! Het interne net-plan: subnet, per-slot IP en MAC, de gateway, en de
//! framematen van het slot-LAN.
//!
//! Deterministisch, geen tabellen die leren: HOP is de gateway op .1, slot
//! `i` op .(i+1)/24, MAC `02:00:00:00:00:<slot>` (HOP = `..:00`). Eén bron van
//! waarheid, zodat de switch en de app-stacks nooit uiteenlopen; beide kanten
//! leiden het uit het slotnummer af.
//!
//! De waarheid staat in `abi::layout` (Go: `metal/abi/layout`), de
//! adresfuncties ook: een slotnummer als `usize` waarin 0 de kern is, want
//! de switch-poorten zijn 0..=`SLOT_CAP` en poort 0 bestaat als poort, niet
//! als app (`abi::layout::Slot` begint bij 1). Dit bestand voegt de
//! framematen van het slot-LAN toe.

pub use abi::layout::{HOST_MAC, NET_PREFIX, SLOT_CAP, ip4_port, port_ip4, port_mac};

/// Het aantal switch-poorten: poort 0 is HOP, 1..=`SLOT_CAP` zijn apps.
pub const PORTS: usize = SLOT_CAP + 1;

/// De MTU van het slot-LAN (de frame-ringen). Geen draad, geen bitfouten,
/// dus zo groot als IPv4 toelaat: één 1 MiB-chunk is dan 16 segmenten in
/// plaats van 700, met 8 ACK's in plaats van 350 (gemeten 04-09). Beide
/// stacks klemmen de MSS per bestemming: naar buiten het prefix blijft het
/// 1500, de uplink-NIC ziet nooit een jumbo.
pub const LAN_MTU: usize = abi::layout::NET_MTU;

/// Het grootste frame op het slot-LAN: MTU plus Ethernet-kop en marge (Go:
/// `maxFrameLen = netdev.MTU + netdev.EthernetMaximumSize`). Een groter
/// record wordt vóór elke flood, gateway-kopie of NAT-route geweigerd: anders
/// kan één slot de MTU-buffer van een buurslot corrupt verklaren of de
/// LAN-ringen met reuzenrecords vullen.
pub const MAX_LAN_FRAME: usize = LAN_MTU + 18;

/// De klassieke Ethernet-grens van de fysieke NIC's (Go: `uplinkMaxFrame`).
pub const UPLINK_MAX_FRAME: usize = 1500 + 18;

/// HOP's interne adres: de gateway die de apps als default route krijgen.
#[must_use]
pub const fn host_ip4() -> u32 {
    abi::layout::HOST_IP4
}

/// Ligt `ip` in het interne subnet (10.100.0.0/24)?
#[must_use]
pub const fn is_internal(ip: u32) -> bool {
    ip >> 8 == host_ip4() >> 8
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn plan_matches_the_go_numbers() {
        assert_eq!(host_ip4(), 0x0A64_0001);
        assert_eq!(port_ip4(1), 0x0A64_0002);
        assert_eq!(port_mac(3), [2, 0, 0, 0, 0, 3]);
        assert_eq!(MAX_LAN_FRAME, 65553);
        assert!(is_internal(port_ip4(7)));
        assert!(!is_internal(0x0A00_020F));
        assert_eq!(HOST_MAC, port_mac(0));
    }
}
