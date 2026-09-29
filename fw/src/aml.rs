//! Een minimale AML-lezer voor precies één ding: de PCI-routeringstabel
//! (`_PRT`) van een host-bridge, voor INTx als terugval naast MSI-X.
//!
//! Geen interpreter. De lezer zoekt in de DSDT (of een SSDT) de
//! `Name (_PRT, Package () {...})` van een device, leest `_SEG` en `_BBN`
//! van datzelfde device, en decodeert de pakketten `{adres, pin, bron,
//! bronindex}`. Alles wat geen statische data is, weigert hij luid (een
//! [`Error`] met de reden): een `_PRT` als `Method`, een bron die een
//! link-device is (QEMU's `GSI0`..`GSI3`, die pas via hun `_CRS` een GSI
//! worden), een `_SEG` die een methode is. Wie INTx op zo'n machine wil,
//! heeft een interpreter nodig, en die hebben we bewust niet.
//!
//! Gemeten op de echte tabellen: de Cix-DSDT van de O6N (`PCI3`, `_BBN`
//! 0x30: pin A op GSI 0x1dd = INTID 477, de lijn van de NIC; dezelfde
//! waarde die het Go-board uit de device tree kende) en de Ampere-DSDT
//! (`0x0001FFFF`, pin A op 0x80). Het Go-board zocht de lijn niet: een
//! tabel per root-poort, "de discovery-code van die jacht is weg (20-09)".
//!
//! Onvertrouwde invoer: elke lengte wordt begrensd, elke index gaat met
//! `get`, en een kromme blob is een fout, geen panic.

use bounded::BoundedVec;
use core::fmt;

/// Zoveel routeringen houdt een tabel vast: QEMU heeft er 128 (32 slots
/// maal 4 pinnen), de Ampere 16, de O6N 4.
pub const MAX_ROUTES: usize = 128;

/// Eén routering: device (of 0xffff voor elk), pin (0 = INTA) en GSI.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Route {
    /// Het device-nummer op de bus van de bridge (het hoge woord van het
    /// adres); 0xffff = elk device.
    pub dev: u16,
    /// De pin: 0 = INTA tot 3 = INTD.
    pub pin: u8,
    /// De GSI (op ARM de INTID: SPI n = 32 + n).
    pub gsi: u32,
}

/// Een `_PRT` met de host-bridge waar hij bij hoort.
#[derive(Clone, Debug, Default)]
pub struct Prt {
    /// Het PCI-segment (`_SEG`, anders 0).
    pub seg: u16,
    /// De eerste bus (`_BBN`, anders 0).
    pub bbn: u8,
    /// De routeringen.
    pub routes: BoundedVec<Route, MAX_ROUTES>,
}

impl Prt {
    /// De GSI van `pin` (0 = INTA) van device `dev` op de bus van de
    /// bridge.
    #[must_use]
    pub fn lookup(&self, dev: u8, pin: u8) -> Option<u32> {
        self.routes
            .as_slice()
            .iter()
            .find(|r| (r.dev == u16::from(dev) || r.dev == 0xffff) && r.pin == pin)
            .map(|r| r.gsi)
    }
}

/// Waarom de lezer een `_PRT` weigert.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// Geen `_PRT` voor dit segment en deze bus.
    NotFound {
        /// Het segment.
        seg: u16,
        /// De bus.
        bbn: u8,
    },
    /// De `_PRT` (of `_SEG`, `_BBN`) is een methode: dat vraagt een
    /// interpreter.
    Method(&'static str),
    /// Een bron is een naam (een link-device), geen GSI.
    LinkDevice {
        /// Waar in de tabel.
        at: usize,
    },
    /// De bytes zijn geen AML die wij kennen.
    Malformed {
        /// Waar in de tabel.
        at: usize,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NotFound { seg, bbn } => {
                write!(f, "aml: no _PRT for segment {seg} bus {bbn:#x}")
            }
            Self::Method(what) => write!(
                f,
                "aml: {what} is a method, not static data (needs an interpreter we do not have)"
            ),
            Self::LinkDevice { at } => write!(
                f,
                "aml: _PRT entry at {at:#x} routes through a link device, not a fixed GSI"
            ),
            Self::Malformed { at } => write!(f, "aml: unexpected bytes at {at:#x}"),
        }
    }
}

/// De `Result` van deze module.
pub type Result<T, E = Error> = core::result::Result<T, E>;

const NAME_OP: u8 = 0x08;
const METHOD_OP: u8 = 0x14;
const PACKAGE_OP: u8 = 0x12;
const EXT_OP: u8 = 0x5b;
const DEVICE_OP: u8 = 0x82;
/// De ACPI-tabelkop: de AML begint erna.
const HEADER: usize = 36;

/// De `_PRT` van de host-bridge met segment `seg` en eerste bus `bbn` in
/// `aml` (een hele DSDT of SSDT, met kop).
pub fn prt(aml: &[u8], seg: u16, bbn: u8) -> Result<Prt> {
    let body = aml.get(HEADER..).unwrap_or(&[]);
    let mut method = None;
    for at in find_all(body, b"_PRT") {
        // `_PRT` als methode: 0x14 PkgLength "_PRT" (de PkgLength is 1 tot
        // 4 bytes, dus de opcode staat 2 tot 5 bytes ervoor).
        if (2..=5).any(|k| at >= k && body.get(at - k) == Some(&METHOD_OP)) {
            method = Some(at);
            continue;
        }
        if at == 0 || body.get(at - 1) != Some(&NAME_OP) {
            continue;
        }
        let dev = innermost_device(body, at);
        let (s, b) = match dev {
            Some((lo, hi)) => (
                int_name(body, lo, hi, b"_SEG")?.unwrap_or(0),
                int_name(body, lo, hi, b"_BBN")?.unwrap_or(0),
            ),
            None => (0, 0),
        };
        if s != u64::from(seg) || b != u64::from(bbn) {
            continue;
        }
        return Ok(Prt {
            seg,
            bbn,
            routes: routes(body, at + 4)?,
        });
    }
    if method.is_some() {
        return Err(Error::Method("_PRT"));
    }
    Err(Error::NotFound { seg, bbn })
}

/// Alle posities van `name` (vier bytes) in `b`.
fn find_all<'a>(b: &'a [u8], name: &'a [u8; 4]) -> impl Iterator<Item = usize> + 'a {
    b.windows(4)
        .enumerate()
        .filter(move |(_, w)| *w == name)
        .map(|(i, _)| i)
}

/// Een PkgLength op `at`: `(lengte, bytes van de codering)`. De lengte
/// telt vanaf `at` (de PkgLength zelf meegeteld), zoals de spec zegt.
fn pkg_len(b: &[u8], at: usize) -> Option<(usize, usize)> {
    let lead = *b.get(at)?;
    let follow = usize::from(lead >> 6);
    if follow == 0 {
        return Some((usize::from(lead & 0x3f), 1));
    }
    let mut len = usize::from(lead & 0x0f);
    for i in 0..follow {
        len |= usize::from(*b.get(at + 1 + i)?) << (4 + 8 * i);
    }
    Some((len, 1 + follow))
}

/// Het kleinste `Device ()` dat `at` omvat, als bereik `(begin, eind)` van
/// zijn lichaam. Een `5B 82` in een buffer die op een device lijkt, kan
/// alleen een groter of onmogelijk bereik geven; we nemen het kleinste dat
/// klopt, en een kromme lengte telt niet mee.
fn innermost_device(b: &[u8], at: usize) -> Option<(usize, usize)> {
    let mut best: Option<(usize, usize)> = None;
    for i in 0..at.min(b.len()) {
        if b.get(i) != Some(&EXT_OP) || b.get(i + 1) != Some(&DEVICE_OP) {
            continue;
        }
        let Some((len, _)) = pkg_len(b, i + 2) else {
            continue;
        };
        let (lo, hi) = (i + 2, i + 2 + len);
        if lo < at && at < hi && hi <= b.len() && best.is_none_or(|(l, h)| hi - lo < h - l) {
            best = Some((lo, hi));
        }
    }
    best
}

/// De waarde van `Name (name, integer)` direct in het device `[lo, hi)`
/// (niet in een genest device), `None` als hij er niet is.
fn int_name(b: &[u8], lo: usize, hi: usize, name: &[u8; 4]) -> Result<Option<u64>> {
    let body = b.get(lo..hi).unwrap_or(&[]);
    for at in find_all(body, name) {
        let abs = lo + at;
        if (2..=5).any(|k| abs >= k && b.get(abs - k) == Some(&METHOD_OP))
            && innermost_device(b, abs) == Some((lo, hi))
        {
            return Err(Error::Method(if name == b"_SEG" { "_SEG" } else { "_BBN" }));
        }
        if abs == 0 || b.get(abs - 1) != Some(&NAME_OP) {
            continue;
        }
        if innermost_device(b, abs) != Some((lo, hi)) {
            continue;
        }
        return integer(b, abs + 4).map(|(v, _)| Some(v));
    }
    Ok(None)
}

/// Een AML-integer op `at`: `(waarde, lengte)`.
fn integer(b: &[u8], at: usize) -> Result<(u64, usize)> {
    let op = *b.get(at).ok_or(Error::Malformed { at })?;
    let n = |w: usize| -> Result<(u64, usize)> {
        let bytes = b.get(at + 1..at + 1 + w).ok_or(Error::Malformed { at })?;
        let v = bytes
            .iter()
            .rev()
            .fold(0u64, |v, &x| (v << 8) | u64::from(x));
        Ok((v, 1 + w))
    };
    match op {
        0x00 => Ok((0, 1)),
        0x01 => Ok((1, 1)),
        0xff => Ok((u64::MAX, 1)),
        0x0a => n(1),
        0x0b => n(2),
        0x0c => n(4),
        0x0e => n(8),
        _ => Err(Error::Malformed { at }),
    }
}

/// Is dit het begin van een NameString (een link-device als bron)?
fn is_name(op: u8) -> bool {
    matches!(op, b'A'..=b'Z' | b'_' | 0x5c | 0x5e | 0x2e | 0x2f)
}

/// De pakketten van de `Package` op `at`.
fn routes(b: &[u8], at: usize) -> Result<BoundedVec<Route, MAX_ROUTES>> {
    if b.get(at) != Some(&PACKAGE_OP) {
        return Err(Error::Malformed { at });
    }
    let (len, lb) = pkg_len(b, at + 1).ok_or(Error::Malformed { at })?;
    let end = at + 1 + len;
    let count = usize::from(*b.get(at + 1 + lb).ok_or(Error::Malformed { at })?);
    let mut p = at + 2 + lb;
    let mut out = BoundedVec::new();
    for _ in 0..count {
        if p >= end || b.get(p) != Some(&PACKAGE_OP) {
            return Err(Error::Malformed { at: p });
        }
        let (elen, elb) = pkg_len(b, p + 1).ok_or(Error::Malformed { at: p })?;
        let next = p + 1 + elen;
        let mut q = p + 2 + elb; // voorbij de NumElements (4)
        let (addr, n) = integer(b, q)?;
        q += n;
        let (pin, n) = integer(b, q)?;
        q += n;
        if b.get(q).copied().is_some_and(is_name) {
            return Err(Error::LinkDevice { at: q });
        }
        let (_source, n) = integer(b, q)?;
        q += n;
        let (gsi, _) = integer(b, q)?;
        let r = Route {
            dev: (addr >> 16) as u16,
            pin: (pin & 3) as u8,
            gsi: u32::try_from(gsi).map_err(|_| Error::Malformed { at: q })?,
        };
        if out.push(r).is_err() {
            break;
        }
        p = next;
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// De DSDT van de Radxa Orion O6N (Cix-firmware), zoals het Go-board
    /// hem op 18-09 van het ijzer haalde.
    const O6N: &[u8] = include_bytes!("acpi/testdata/o6n-dsdt.aml");

    #[test]
    fn the_o6n_nic_root_port_routes_pin_a_to_477() {
        let p = prt(O6N, 0, 0x30).unwrap();
        assert_eq!(p.routes.len(), 4);
        assert_eq!(p.lookup(0, 0), Some(477));
        assert_eq!(p.lookup(0, 3), Some(0x1e0));
        // De andere root-poorten van de Go-tabel (`intxByRootBus`) wijken af
        // van de DSDT behalve 0x30; de DSDT is de waarheid.
        assert!(prt(O6N, 0, 0xc0).is_ok());
        assert_eq!(
            prt(O6N, 0, 0x31).unwrap_err(),
            Error::NotFound { seg: 0, bbn: 0x31 }
        );
    }

    /// Een tabel in de vorm van de Ampere en van QEMU.
    fn table(body: &[u8]) -> Vec<u8> {
        let mut t = vec![0u8; HEADER];
        t[..4].copy_from_slice(b"DSDT");
        t.extend_from_slice(body);
        t
    }

    /// `Device (PCI0) { Name (_SEG, seg) Name (_BBN, bbn) Name (_PRT, prt) }`.
    fn device(seg: u8, bbn: u8, prt: &[u8]) -> Vec<u8> {
        let mut inner = b"PCI0".to_vec();
        inner.extend_from_slice(&[NAME_OP, b'_', b'S', b'E', b'G', 0x0a, seg]);
        inner.extend_from_slice(&[NAME_OP, b'_', b'B', b'B', b'N', 0x0a, bbn]);
        inner.extend_from_slice(prt);
        let len = inner.len() + 2;
        let mut d = vec![
            EXT_OP,
            DEVICE_OP,
            0x40 | (len & 0xf) as u8,
            (len >> 4) as u8,
        ];
        d.extend_from_slice(&inner);
        d
    }

    fn package(elems: &[Vec<u8>]) -> Vec<u8> {
        let inner: Vec<u8> = elems.concat();
        let len = inner.len() + 3;
        let mut p = vec![
            PACKAGE_OP,
            0x40 | (len & 0xf) as u8,
            (len >> 4) as u8,
            elems.len() as u8,
        ];
        p.extend_from_slice(&inner);
        p
    }

    fn entry(addr: u32, pin: u8, source: &[u8], gsi: u32) -> Vec<u8> {
        let mut inner = vec![0x0c];
        inner.extend_from_slice(&addr.to_le_bytes());
        inner.push(0x0a);
        inner.push(pin);
        inner.extend_from_slice(source);
        inner.push(0x0c);
        inner.extend_from_slice(&gsi.to_le_bytes());
        let mut p = vec![PACKAGE_OP, (inner.len() + 2) as u8, 4];
        p.extend_from_slice(&inner);
        p
    }

    #[test]
    fn an_ampere_style_prt_with_dword_addresses() {
        let mut prt_name = vec![NAME_OP, b'_', b'P', b'R', b'T'];
        prt_name.extend_from_slice(&package(&[
            entry(0x0001_ffff, 0, &[0x00], 0x80),
            entry(0x0001_ffff, 1, &[0x00], 0x81),
        ]));
        let t = table(&device(1, 0, &prt_name));
        let p = prt(&t, 1, 0).unwrap();
        assert_eq!(p.lookup(1, 0), Some(0x80));
        assert_eq!(p.lookup(1, 1), Some(0x81));
        assert_eq!(p.lookup(0, 0), None);
        assert!(prt(&t, 0, 0).is_err());
    }

    #[test]
    fn a_link_device_source_is_refused_loudly() {
        // QEMU virt: de bron is `GSI0`, een link-device.
        let mut prt_name = vec![NAME_OP, b'_', b'P', b'R', b'T'];
        prt_name.extend_from_slice(&package(&[entry(0xffff, 0, b"GSI0", 0)]));
        let t = table(&device(0, 0, &prt_name));
        assert!(matches!(prt(&t, 0, 0), Err(Error::LinkDevice { .. })));
    }

    #[test]
    fn a_prt_method_is_refused_loudly() {
        let body = [METHOD_OP, 0x08, b'_', b'P', b'R', b'T', 0x00, 0xa4, 0x00];
        let t = table(&device(0, 0, &body));
        assert_eq!(prt(&t, 0, 0).unwrap_err(), Error::Method("_PRT"));
    }

    #[test]
    fn garbage_is_an_error_not_a_panic() {
        for n in 0..64 {
            let mut t = table(&[NAME_OP, b'_', b'P', b'R', b'T', PACKAGE_OP, 0xff]);
            t.extend(core::iter::repeat_n(0x12u8, n));
            let _ = prt(&t, 0, 0);
        }
        assert_eq!(pkg_len(&[0x4a, 0x01], 0), Some((0x1a, 2)));
        assert_eq!(pkg_len(&[0x3f], 0), Some((0x3f, 1)));
    }
}
