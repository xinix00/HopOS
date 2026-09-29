//! De tien xHCI-hosts van de Cix P1 uit de DSDT lezen (Go:
//! `OLD/metal/board/o6n/hop/usb_firmware.go`).
//!
//! Dit module bezit niets: het is pure parsing van de bytes van de DSDT. Het
//! mapt geen venster, zet geen controller aan en leest geen firmwarevariabele;
//! dat doet de board-bedrading (`usb.rs`) met wat [`parse`] teruggeeft.
//!
//! De Cix-firmware beschrijft tien native xHCI-vensters in zijn DSDT. We
//! lezen alleen de statische resource-buffers en de twee exacte vormen van
//! `_STA` op basis van `GETV`; dit is bewust geen AML-interpreter. Firmware
//! die er anders uitziet, is niet ondersteund: liever geen USB dan een
//! venster dat we verkeerd gelezen hebben.
//!
//! Bronnen: Cix Sky1 `Dsdt-USB.asl` en `Dsdt-AcpiRam.asl` (`cix_p1_dev`).
//! Ondanks de macronamen `USB_EHCI_HOST*` van Cix zijn USB0..3 ook xHCI: de
//! DT van Sky1 noemt hun vensters "xhci" onder `cdns,usbssp` (commit
//! 57e018a398248d7e5e4d798610df79a557c0629f in Sky1-Linux/linux-sky1,
//! `patches-latest/0001-arm64-dts-cix-Add-Sky1-SoC-and-Radxa-Orion-O6-device.patch`).
//! De vastgelegde DSDT van de O6N beschrijft ook alle tien met `_HID`
//! `PNP0D10`.

use core::fmt;

/// Het aantal native xHCI-hosts: XHC0..XHC5 en USB0..USB3.
pub(crate) const HOSTS: usize = 10;

/// De namen van de hosts, op hun vaste plek: XHC0..5 op 0..5, USB0..3 op
/// 6..9. De plek is ook de `_UID` in de DSDT.
const NAMES: [&str; HOSTS] = [
    "XHC0", "XHC1", "XHC2", "XHC3", "XHC4", "XHC5", "USB0", "USB1", "USB2", "USB3",
];

/// De lengte van de ACPI-tabelkop; de AML begint daarna.
const HEADER: usize = 36;

/// `Name (_HID, "PNP0D10")`: NameOp, NameSeg, StringPrefix, de string en
/// zijn nul.
const HID: &[u8] = &[
    0x08, b'_', b'H', b'I', b'D', 0x0d, b'P', b'N', b'P', b'0', b'D', b'1', b'0', 0,
];

/// Het begin van de body van `_CRS`: methodevlaggen 8 (serialized, geen
/// argumenten), dan `Name (RBUF, Buffer (...`.
const CRS_HEAD: &[u8] = &[0x08, 0x08, b'R', b'B', b'U', b'F', 0x11];

/// Het einde van `_CRS`: `Return (RBUF)`.
const CRS_TAIL: &[u8] = &[0xa4, b'R', b'B', b'U', b'F'];

/// Het begin van de alleen-host-vorm van `_STA`: methodevlaggen 0, dan
/// `If (GETV (` met een ByteConst-argument.
const STA_HOST: &[u8] = b"\x00\xa0\x0aGETV\x0a";

/// Het begin van de dual-role-vorm: methodevlaggen 0, `If (LAnd (GETV (`.
const STA_DUAL: &[u8] = b"\x00\xa0\x13\x90GETV\x0a";

/// Het tweede predicaat van de dual-role-vorm: `LEqual (GETV (`.
const STA_ROLE: &[u8] = b"\x93GETV\x0a";

/// Het einde van beide vormen: `Return (0x0f)`, `Else`, `Return (0)`.
const STA_RETURN: &[u8] = b"\xa4\x0a\x0f\xa1\x03\xa4\x00";

/// Eén xHCI-host zoals de DSDT hem beschrijft.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub(crate) struct Host {
    /// Of de firmwarevariabelen hem aan zetten (alleen na [`Firmware::enabled`]).
    pub(crate) enabled: bool,
    /// De DSDT-naam: "XHC0".."XHC5" of "USB0".."USB3".
    pub(crate) name: &'static str,
    /// Het registervenster (Memory32Fixed).
    pub(crate) base: u64,
    /// De maat van het venster.
    pub(crate) size: u64,
    /// De offset van de aan-byte in het variabelen-RAM (`GETV`).
    pub(crate) enable_off: u8,
    /// De offset van de rol-byte; 0 betekent een controller die alleen host is.
    pub(crate) role_off: u8,
}

/// Wat de DSDT over USB zegt: het variabelen-RAM van de firmware en de tien
/// hosts op hun vaste plek.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Firmware {
    /// `GNVA`: het fysieke adres van het variabelen-RAM.
    pub(crate) base: u64,
    /// `GNVL`: de maat ervan; elke offset van een host ligt eronder.
    pub(crate) size: u64,
    /// Alle tien hosts, met `enabled` op false.
    pub(crate) hosts: [Host; HOSTS],
}

/// Waarom de DSDT niet de vorm heeft die we kennen.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Unsupported {
    /// `GNVA` ontbreekt of staat er meer dan eens (`None`), of is 0 of
    /// 0xffffffff.
    Base(Option<u64>),
    /// `GNVL` ontbreekt of staat er meer dan eens (`None`), of ligt buiten
    /// 0x1f..=0xff, of loopt met `GNVA` over het einde van de adresruimte.
    Size(Option<u64>),
    /// De methode `GETV` staat er niet precies één keer in de bekende vorm.
    Getv {
        /// De `GNVL` waarmee de sleutel gebouwd is.
        size: u64,
    },
    /// Een host heeft niet de exacte vorm (`_UID`, `_CCA`, `_STA`, `_CRS`).
    Host {
        /// De plek van de host (0..9).
        index: usize,
    },
    /// Een host staat er twee keer in.
    Duplicate {
        /// De plek van de host (0..9).
        index: usize,
    },
    /// Een offset van een host ligt buiten het variabelen-RAM.
    Offset {
        /// De plek van de host (0..9).
        index: usize,
        /// De aan-offset.
        enable: u8,
        /// De rol-offset.
        role: u8,
        /// `GNVL`.
        size: u64,
    },
    /// Twee hostvensters overlappen.
    Overlap {
        /// De nieuwe host.
        index: usize,
        /// De host waar hij mee overlapt.
        other: usize,
    },
    /// Een host ontbreekt; alle tien moeten er zijn.
    Missing {
        /// De plek van de host (0..9).
        index: usize,
    },
}

impl fmt::Display for Unsupported {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("unsupported CIX USB firmware description: ")?;
        match *self {
            Self::Base(None) => f.write_str("GNVA missing or ambiguous"),
            Self::Base(Some(base)) => write!(f, "GNVA {base:#x} out of range"),
            Self::Size(None) => f.write_str("GNVL missing or ambiguous"),
            Self::Size(Some(size)) => write!(f, "GNVL {size:#x} out of range"),
            Self::Getv { size } => write!(f, "GETV for GNVL {size:#x} not found exactly once"),
            Self::Host { index } => write!(f, "host {index} not in the known form"),
            Self::Duplicate { index } => write!(f, "host {index} described twice"),
            Self::Offset {
                index,
                enable,
                role,
                size,
            } => write!(
                f,
                "host {index} offsets {enable:#x}/{role:#x} outside GNVL {size:#x}"
            ),
            Self::Overlap { index, other } => {
                write!(f, "host {index} window overlaps host {other}")
            }
            Self::Missing { index } => write!(f, "host {index} missing"),
        }
    }
}

/// Leest de tien xHCI-hosts en het variabelen-RAM uit de DSDT (AML met
/// tabelkop).
pub(crate) fn parse(dsdt: &[u8]) -> Result<Firmware, Unsupported> {
    let base = named_integer(dsdt, *b"GNVA").ok_or(Unsupported::Base(None))?;
    if base == 0 || base == 0xffff_ffff {
        return Err(Unsupported::Base(Some(base)));
    }
    // Go eist 0x1f..=4096 en daarna nog dat de maat in één byte past, want
    // `GETV` vergelijkt met een BytePrefix-constante; samen is dat 0x1f..=0xff.
    let size = named_integer(dsdt, *b"GNVL").ok_or(Unsupported::Size(None))?;
    let key = match u8::try_from(size) {
        Ok(byte) if byte >= 0x1f && base.checked_add(size).is_some() => getv(byte),
        _ => return Err(Unsupported::Size(Some(size))),
    };
    // De exacte byte-lees van `GETV` en zijn grens: alleen dan betekenen de
    // offsets in `_STA` wat we denken.
    if unique(dsdt, &key).is_none() {
        return Err(Unsupported::Getv { size });
    }

    let mut found: [Option<Host>; HOSTS] = [None; HOSTS];
    for at in HEADER..dsdt.len().saturating_sub(3) {
        let Some((index, body)) = dsdt.get(at..).and_then(candidate) else {
            continue;
        };
        let host = add(&found, index, body, size)?;
        if let Some(slot) = found.get_mut(index) {
            *slot = Some(host);
        }
    }

    let mut hosts = [Host::default(); HOSTS];
    for (index, (slot, host)) in hosts.iter_mut().zip(found).enumerate() {
        *slot = host.ok_or(Unsupported::Missing { index })?;
    }
    Ok(Firmware { base, size, hosts })
}

impl Firmware {
    /// Zet `enabled` per host uit de bytes van het variabelen-RAM: de
    /// aan-byte niet nul, en voor een dual-role-controller de rol-byte nul
    /// (host). Een offset buiten `values` telt als uit.
    ///
    /// Alle tien plekken blijven staan, ook de uitgeschakelde: hun
    /// DMA-plakken moeten over een kern-flip dezelfde eigenaar houden.
    pub(crate) fn enabled(&self, values: &[u8]) -> [Host; HOSTS] {
        let mut hosts = self.hosts;
        for host in &mut hosts {
            let on = values
                .get(usize::from(host.enable_off))
                .is_some_and(|&v| v != 0);
            let as_host = host.role_off == 0 || values.get(usize::from(host.role_off)) == Some(&0);
            host.enabled = on && as_host;
        }
        hosts
    }
}

/// Toetst één host tegen wat er al gevonden is en leest hem.
fn add(
    found: &[Option<Host>; HOSTS],
    index: usize,
    body: &[u8],
    size: u64,
) -> Result<Host, Unsupported> {
    if found.get(index).is_none_or(Option::is_some) {
        return Err(Unsupported::Duplicate { index });
    }
    let host = device(body, index).ok_or(Unsupported::Host { index })?;
    if u64::from(host.enable_off) >= size || u64::from(host.role_off) >= size {
        return Err(Unsupported::Offset {
            index,
            enable: host.enable_off,
            role: host.role_off,
            size,
        });
    }
    for (other, seen) in found.iter().enumerate() {
        if seen.is_some_and(|seen| overlaps(&host, &seen)) {
            return Err(Unsupported::Overlap { index, other });
        }
    }
    Ok(host)
}

/// Of twee vensters elkaar raken.
fn overlaps(a: &Host, b: &Host) -> bool {
    // Een basis is een u32 en een maat hoogstens 0x10000, dus verzadigen
    // gebeurt nooit; het staat er zodat er geen overflow-pad is.
    a.base < b.base.saturating_add(b.size) && b.base < a.base.saturating_add(a.size)
}

/// Een `Device` (ExtOpPrefix 0x5b, DeviceOp 0x82) met een van onze tien
/// namen en direct daarna `_HID "PNP0D10"`: de plek en de body.
///
/// Gelijknamige kinderen zonder die `_HID` (bijvoorbeeld een USB0 met alleen
/// `_ADR`) slaan we over.
fn candidate(at: &[u8]) -> Option<(usize, &[u8])> {
    let rest = at.strip_prefix(&[0x5b, 0x82][..])?;
    let (body, _) = pkg(rest)?;
    let (seg, tail) = body.split_first_chunk::<4>()?;
    let index = match *seg {
        [b'X', b'H', b'C', digit @ b'0'..=b'5'] => usize::from(digit - b'0'),
        [b'U', b'S', b'B', digit @ b'0'..=b'3'] => 6 + usize::from(digit - b'0'),
        _ => return None,
    };
    tail.starts_with(HID).then_some((index, body))
}

/// Leest één host in de exacte vorm: naam, `_HID`, `_UID` gelijk aan de
/// plek, `_CCA 0`, `_STA` en als laatste `_CRS`.
fn device(body: &[u8], index: usize) -> Option<Host> {
    let name = *NAMES.get(index)?;
    let rest = body.strip_prefix(name.as_bytes())?.strip_prefix(HID)?;
    let (uid, n) = read_name(rest, *b"_UID")?;
    if uid != u64::try_from(index).ok()? {
        return None;
    }
    let rest = rest.get(n..)?;
    let (cca, n) = read_name(rest, *b"_CCA")?;
    if cca != 0 {
        return None;
    }
    let rest = rest.get(n..)?;
    let (sta, n) = method(rest, *b"_STA")?;
    let (enable_off, role_off) = status(sta)?;
    let (base, size) = resources(rest.get(n..)?)?;
    let valid = base != 0 && base % 4096 == 0 && (0x1000..=0x10000).contains(&size);
    valid.then_some(Host {
        enabled: false,
        name,
        base,
        size,
        enable_off,
        role_off,
    })
}

/// De body van `_STA` in een van de twee exacte vormen, met de offsets die
/// de firmware aan `GETV` meegeeft: (aan, rol), rol 0 voor alleen-host.
fn status(sta: &[u8]) -> Option<(u8, u8)> {
    // If (GETV (enable)) { Return (0x0f) } Else { Return (0) }.
    if let Some(&[enable, ref tail @ ..]) = sta.strip_prefix(STA_HOST) {
        return (tail == STA_RETURN).then_some((enable, 0));
    }
    // If (LAnd (GETV (enable), LEqual (GETV (role), Zero))) { Return (0x0f) }
    // Else { Return (0) }.
    let (&enable, rest) = sta.strip_prefix(STA_DUAL)?.split_first()?;
    let (&role, rest) = rest.strip_prefix(STA_ROLE)?.split_first()?;
    let tail = rest.strip_prefix(&[0][..])?;
    // Rol-offset 0 zou dit met een alleen-host-controller verwarren.
    (role != 0 && tail == STA_RETURN).then_some((enable, role))
}

/// De body die na `_STA` rest: precies `_CRS` en niets daarna, met één
/// Memory32Fixed (basis, maat), één Extended Interrupt en de EndTag.
fn resources(rest: &[u8]) -> Option<(u64, u64)> {
    let (crs, n) = method(rest, *b"_CRS")?;
    if n != rest.len() {
        return None;
    }
    let tail = crs.strip_prefix(CRS_HEAD)?;
    let (buffer, k) = pkg(tail)?;
    if tail.get(k..)? != CRS_TAIL {
        return None;
    }
    let (length, k) = integer(buffer)?;
    let resource = buffer.get(k..)?;
    if length != u64::try_from(resource.len()).ok()? {
        return None;
    }
    // Memory32Fixed: 0x86, lengte 9, read-write, dan basis en maat.
    let (memory, rest) = resource.split_first_chunk::<12>()?;
    let [0x86, 9, 0, 1, b0, b1, b2, b3, s0, s1, s2, s3] = *memory else {
        return None;
    };
    // Extended Interrupt (0x89, lengte 6, consumer, één interrupt; het nummer
    // zelf lezen we niet) en de EndTag (0x79, checksum 0). Geen extra
    // vensters: het patroon eist ook de lengte.
    let [0x89, 6, 0, 1, 1, _, _, _, _, 0x79, 0] = *rest else {
        return None;
    };
    Some((
        u64::from(u32::from_le_bytes([b0, b1, b2, b3])),
        u64::from(u32::from_le_bytes([s0, s1, s2, s3])),
    ))
}

/// De sleutel voor `GETV`: de methode met één argument die één byte uit
/// `GNVA + Arg0` leest, en 0 teruggeeft als `Arg0 >= GNVL`.
fn getv(size: u8) -> [u8; 51] {
    [
        b'G', b'E', b'T', b'V', 9, 0xa0, 8, 0x92, 0x95, 0x68, 0x0a, size, 0xa4, 0, 0x72, 0x68,
        b'G', b'N', b'V', b'A', 0x60, 0x5b, 0x80, b'G', b'P', b'N', b'V', 0, 0x60, 1, 0x5b, 0x81,
        0x0b, b'G', b'P', b'N', b'V', 1, b'V', b'A', b'R', b'V', 8, 0x99, b'V', b'A', b'R', b'V',
        0x60, 0xa4, 0x60,
    ]
}

/// Een AML-integer: ZeroOp, OneOp of een Byte/Word/DWord/QWord-constante.
/// Geeft de waarde en het aantal gelezen bytes.
fn integer(b: &[u8]) -> Option<(u64, usize)> {
    let (&op, rest) = b.split_first()?;
    match op {
        0x00 => Some((0, 1)),
        0x01 => Some((1, 1)),
        0x0a => Some((u64::from(*rest.first()?), 2)),
        0x0b => Some((u64::from(u16::from_le_bytes(*rest.first_chunk()?)), 3)),
        0x0c => Some((u64::from(u32::from_le_bytes(*rest.first_chunk()?)), 5)),
        0x0e => Some((u64::from_le_bytes(*rest.first_chunk()?), 9)),
        _ => None,
    }
}

/// De waarde van `Name (name, integer)`, als die naam er precies één keer
/// staat: een tweede `GNVA` maakt de tabel dubbelzinnig.
fn named_integer(b: &[u8], name: [u8; 4]) -> Option<u64> {
    let [n0, n1, n2, n3] = name;
    let key = [0x08, n0, n1, n2, n3];
    let at = unique(b, &key)?;
    let (value, _) = integer(b.get(at.checked_add(key.len())?..)?)?;
    Some(value)
}

/// `Name (name, integer)` aan het begin van `b`: de waarde en de lengte.
fn read_name(b: &[u8], name: [u8; 4]) -> Option<(u64, usize)> {
    let rest = b.strip_prefix(&[0x08][..])?.strip_prefix(&name[..])?;
    let (value, n) = integer(rest)?;
    Some((value, n + 5))
}

/// De body van een pakket en zijn totale lengte, PkgLength meegeteld.
fn pkg(b: &[u8]) -> Option<(&[u8], usize)> {
    let lead = *b.first()?;
    let n = usize::from(lead >> 6) + 1;
    let head = b.get(..n)?;
    let size = if n == 1 {
        usize::from(lead & 0x3f)
    } else {
        // Meerbyte-PkgLength: de onderste vier bits van de eerste byte, dan
        // hele bytes.
        head.iter()
            .skip(1)
            .enumerate()
            .fold(usize::from(lead & 0x0f), |size, (i, &byte)| {
                size | usize::from(byte) << (4 + 8 * i)
            })
    };
    if size < n {
        return None;
    }
    Some((b.get(n..size)?, size))
}

/// `Method (name, ...)` aan het begin van `b`: de body na de naam
/// (methodevlaggen eerst) en de totale lengte, MethodOp meegeteld.
fn method(b: &[u8], name: [u8; 4]) -> Option<(&[u8], usize)> {
    let (body, n) = pkg(b.strip_prefix(&[0x14][..])?)?;
    if body.len() < 5 {
        return None;
    }
    Some((body.strip_prefix(&name[..])?, n + 1))
}

/// De plek van `needle` in `hay`, alleen als hij er precies één keer staat
/// (niet-overlappend geteld, zoals Go's `bytes.Count`).
fn unique(hay: &[u8], needle: &[u8]) -> Option<usize> {
    let at = find(hay, needle)?;
    let rest = hay.get(at.checked_add(needle.len())?..)?;
    find(rest, needle).is_none().then_some(at)
}

/// De eerste plek van `needle` in `hay`.
fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

#[cfg(test)]
mod tests {
    use super::*;

    const DSDT: &[u8] = include_bytes!("../../../fw/src/acpi/testdata/o6n-dsdt.aml");

    #[test]
    fn actual_firmware() {
        let f = parse(DSDT).unwrap();
        assert_eq!((f.base, f.size), (0xfffe_0000, 0x4d));
        let bases: [u64; HOSTS] = [
            0x901_8000, 0x908_8000, 0x90f_8000, 0x916_8000, 0x91d_8000, 0x91e_8000, 0x926_8000,
            0x929_8000, 0x92c_8000, 0x92f_8000,
        ];
        let enables: [u8; HOSTS] = [0x12, 0x13, 0x16, 0x15, 0x17, 0x18, 0x14, 0x1b, 0x1a, 0x19];
        let roles: [u8; HOSTS] = [0x1c, 0, 0, 0, 0x1d, 0x1e, 0, 0, 0, 0];
        for (i, h) in f.hosts.iter().enumerate() {
            assert_eq!(h.name, NAMES[i]);
            assert!(
                h.base == bases[i]
                    && h.size == 0x8000
                    && h.enable_off == enables[i]
                    && h.role_off == roles[i],
                "host{i}: {h:?}"
            );
        }
        let keys: [&[u8]; 5] = [b"GNVA", b"GNVL", b"XHC2", b"PNP0D10", b"GETV\x09\xa0"];
        for key in keys {
            let mut damaged = DSDT.to_vec();
            let at = find(&damaged, key).expect("missing key");
            damaged[at] ^= 0x20;
            assert!(parse(&damaged).is_err(), "accepted changed {key:?}");
        }
        assert!(
            parse(&DSDT[..DSDT.len() / 2]).is_err(),
            "accepted truncated firmware"
        );
        let mut duplicate = DSDT.to_vec();
        duplicate.extend_from_slice(&[0x08, b'G', b'N', b'V', b'A', 0]);
        assert_eq!(parse(&duplicate), Err(Unsupported::Base(None)));
    }

    #[test]
    fn stable_slots_across_enable_masks() {
        let f = parse(DSDT).unwrap();
        for mask in 0..1u32 << HOSTS {
            let mut values = vec![0u8; usize::try_from(f.size).unwrap()];
            for (i, h) in f.hosts.iter().enumerate() {
                if mask & (1 << i) != 0 {
                    values[usize::from(h.enable_off)] = 1;
                }
            }
            for (i, h) in f.enabled(&values).iter().enumerate() {
                assert!(
                    h.name == f.hosts[i].name
                        && h.base == f.hosts[i].base
                        && h.enabled == (mask & (1 << i) != 0),
                    "mask {mask} slot {i} changed ownership/status: {h:?}"
                );
            }
            for h in &f.hosts {
                if h.role_off != 0 {
                    values[usize::from(h.role_off)] = 1;
                }
            }
            for (i, h) in f.enabled(&values).iter().enumerate() {
                assert!(
                    f.hosts[i].role_off == 0 || !h.enabled,
                    "device role enabled host {i}"
                );
            }
        }
        for (i, h) in f.enabled(&[]).iter().enumerate() {
            assert!(!h.enabled, "missing state enabled host {i}");
        }
    }

    #[test]
    fn rejects_ambiguous_role_zero() {
        let mut dsdt = DSDT.to_vec();
        let key = [0x93, b'G', b'E', b'T', b'V', 0x0a, 0x1c, 0];
        let at = find(&dsdt, &key).expect("role predicate missing");
        dsdt[at + 6] = 0;
        assert_eq!(
            parse(&dsdt),
            Err(Unsupported::Host { index: 0 }),
            "dual-role offset zero accepted as host-only"
        );
    }

    #[test]
    fn display_carries_numbers() {
        let e = Unsupported::Offset {
            index: 3,
            enable: 0x50,
            role: 0,
            size: 0x4d,
        };
        assert_eq!(
            e.to_string(),
            "unsupported CIX USB firmware description: host 3 offsets 0x50/0x0 outside GNVL 0x4d"
        );
    }
}
