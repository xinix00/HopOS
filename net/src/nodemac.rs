//! Het MAC-adres van een node, afgeleid uit zijn config (Go: `nodemac`).
//!
//! Waarom dit niet per board is: elk bordje zonder MAC in een leesbare fuse
//! heeft dezelfde drie eisen, en die staan met elkaar in spanning:
//!
//! - STABIEL over reboots. Een willekeurig adres kost bij iedere herstart een
//!   nieuwe DHCP-lease en een nieuwe hopdns-registratie.
//! - UNIEK per node. Eén vaste constante laat twee bordjes van hetzelfde type
//!   op één LAN botsen: precies de gemengde fleet waar ze voor bedoeld zijn.
//! - ZONDER nieuwe bron. Adressen van efuse-blokken gokken is hoe je een board
//!   stilzet, en de node-naam staat toch al in de config.
//!
//! Daarom: een expliciete `hopos.mac` heeft voorrang, anders volgt het adres
//! uit `hopos.node`. Het voorvoegsel is `02:48:4f:50`: locally administered
//! (bit 1 van het eerste byte) met "HOP" in ASCII erachter, zodat een node van
//! ons herkenbaar is in een ARP-tabel.
//!
//! De MAC's van de slots op het interne net staan in [`crate::plan`]; die
//! volgen uit het slotnummer, niet uit de config.

/// De vaste kop van elk HopOS-node-adres.
pub const PREFIX: [u8; 4] = [0x02, 0x48, 0x4f, 0x50];

/// Het adres als er géén mac én géén node in de config staat: de enige stand
/// waarin twee bordjes elkaar in de weg zitten.
pub const FALLBACK: [u8; 6] = [0x02, 0x48, 0x4f, 0x50, 0x00, 0x01];

/// Waar het adres vandaan kwam; `Fallback` hoort een waarschuwing te geven
/// (`HOPOS_MAC_FIXED`), en die logregel is van de aanroeper: deze crate
/// heeft geen console.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Source {
    /// Uit `hopos.mac`.
    Explicit,
    /// Uit de node-naam.
    Name,
    /// De vaste terugval: een tweede bordje van dit type op dit LAN botst.
    Fallback,
}

/// Het MAC-adres voor deze node: `mac` (`aa:bb:cc:dd:ee:ff`) heeft voorrang,
/// anders volgt het uit `node`.
#[must_use]
pub fn identity(mac: &str, node: &str) -> ([u8; 6], Source) {
    if let Some(m) = parse(mac) {
        return (m, Source::Explicit);
    }
    if node.is_empty() {
        return (FALLBACK, Source::Fallback);
    }
    // FNV-1a over de naam, de onderste twee bytes eruit. Geen crypto nodig:
    // het enige dat telt is dat verschillende namen verschillende adressen
    // geven en dezelfde naam hetzelfde.
    let mut h: u32 = 2_166_136_261;
    for b in node.bytes() {
        h = (h ^ u32::from(b)).wrapping_mul(16_777_619);
    }
    let [_, _, hi, lo] = h.to_be_bytes();
    (
        [PREFIX[0], PREFIX[1], PREFIX[2], PREFIX[3], hi, lo],
        Source::Name,
    )
}

/// Leest `aa:bb:cc:dd:ee:ff`. Faalt stil (`None`), zodat een typefout in de
/// config terugvalt op de naam-afleiding in plaats van de node zonder netwerk
/// te zetten; met de naam erbij is dat nog altijd een uniek adres.
#[must_use]
pub fn parse(s: &str) -> Option<[u8; 6]> {
    let b = s.as_bytes();
    if b.len() != 17 {
        return None;
    }
    let mut m = [0u8; 6];
    for (i, out) in m.iter_mut().enumerate() {
        let hi = nibble(*b.get(i * 3)?)?;
        let lo = nibble(*b.get(i * 3 + 1)?)?;
        if i < 5 && *b.get(i * 3 + 2)? != b':' {
            return None;
        }
        *out = (hi << 4) | lo;
    }
    Some(m)
}

fn nibble(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn explicite_mac_heeft_voorrang() {
        let (got, src) = identity("02:48:4f:50:aa:bb", "genegeerd");
        assert_eq!(got, [0x02, 0x48, 0x4f, 0x50, 0xaa, 0xbb]);
        assert_eq!(src, Source::Explicit);
    }

    #[test]
    fn naam_afleiding_is_stabiel_en_uniek() {
        assert_eq!(identity("", "radxa-1"), identity("", "radxa-1"));
        let names = [
            "radxa-1",
            "radxa-2",
            "radxa-10",
            "pi5-1",
            "licheerv-1",
            "hopos-a1b2c3d4",
        ];
        let mut seen: Vec<[u8; 6]> = Vec::new();
        for n in names {
            let (m, _) = identity("", n);
            assert!(!seen.contains(&m), "{n} botst op {m:x?}");
            seen.push(m);
            assert_eq!(m[..4], PREFIX, "{n}");
        }
    }

    #[test]
    fn lokaal_beheerd_en_unicast() {
        for m in [FALLBACK, identity("", "radxa-1").0, identity("", "x").0] {
            assert_ne!(m[0] & 0x02, 0, "{m:x?} is niet locally administered");
            assert_eq!(m[0] & 0x01, 0, "{m:x?} is multicast");
        }
    }

    #[test]
    fn geen_mac_en_geen_naam_valt_terug_op_de_vaste_waarde() {
        assert_eq!(identity("", ""), (FALLBACK, Source::Fallback));
    }

    #[test]
    fn parse_weigert_rommel_in_plaats_van_halve_adressen() {
        for s in [
            "",
            "02:48:4f:50:aa",
            "02:48:4f:50:aa:bb:cc",
            "02-48-4f-50-aa-bb",
            "02:48:4f:50:aa:bg",
            "0248.4f50.aabb",
        ] {
            assert_eq!(parse(s), None, "{s:?}");
        }
        assert_eq!(parse("02:48:4F:50:AA:BB").map(|m| m[4]), Some(0xAA));
    }
}
