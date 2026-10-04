//! De `_CPC`-objecten (Collaborative Processor Performance Control, ACPI
//! 8.4.6.1) uit de DSDT- en SSDT-AML, zonder interpreter: een `_CPC` is een
//! Name met een Package van integers en `Register()`-buffers, en dát
//! patroon is deterministisch te lezen (`OLD/metal/fw/acpi/cpc.go`).
//!
//! De O6N heeft ze nodig voor twee dingen: de klok (het desired-perf-woord
//! per DVFS-domein, een SCMI-fastchannel) en de core-klassen (de
//! HighestPerformance per core, omdat de Cix-firmware de efficiëntieklasse
//! in de MADT niet invult). Wat niet parsebaar is, wordt overgeslagen: dit
//! is invoer voor de klok, nooit een boot-blokker. PkgLength en de
//! integer-constanten zijn die van de `_PRT`-lezer erboven.

use super::{NAME_OP, PACKAGE_OP, integer, pkg_len};
use crate::bytes::le64;
use bounded::BoundedVec;

/// Hoeveel `_CPC`'s we bewaren: één per core, en de O6N heeft er twaalf.
pub const MAX_CPCS: usize = 32;

/// Het `_CPC` van één processor, voor zover HopOS het nodig heeft.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Cpc {
    /// De ACPI processor-UID (de `_UID` vóór dit `_CPC`), de koppeling met
    /// de MADT.
    pub uid: u32,
    /// [2] HighestPerformance.
    pub highest: u32,
    /// [3] NominalPerformance.
    pub nominal: u32,
    /// [4] LowestNonlinearPerformance.
    pub lowest_nl: u32,
    /// [5] LowestPerformance.
    pub lowest: u32,
    /// [7] DesiredPerformanceRegister: een SystemMemory-adres (0 = geen of
    /// een andere ruimte).
    pub desired_reg: u64,
    /// De registerbreedte in bits (32 op de O6N).
    pub desired_bits: u8,
    /// [21] LowestFrequency in MHz (revisie 3), 0 = niet gedragen.
    pub lowest_mhz: u32,
    /// [22] NominalFrequency in MHz, 0 = niet gedragen. Pas deze twee maken
    /// van de abstracte perf-schaal een klok.
    pub nominal_mhz: u32,
}

impl Cpc {
    /// Een perf-waarde als klok, via Nominal en NominalFrequency; 0 als het
    /// package geen frequenties draagt.
    #[must_use]
    pub fn mhz(&self, perf: u32) -> u32 {
        if self.nominal == 0 || self.nominal_mhz == 0 {
            return 0;
        }
        (u64::from(perf) * u64::from(self.nominal_mhz) / u64::from(self.nominal)) as u32
    }

    /// De perf-waarde bij een klok (naar beneden afgerond); zonder
    /// frequenties is `mhz` al een perf-waarde (`false`).
    #[must_use]
    pub fn perf(&self, mhz: u32) -> (u32, bool) {
        if self.nominal == 0 || self.nominal_mhz == 0 {
            return (mhz, false);
        }
        let p = u64::from(mhz) * u64::from(self.nominal) / u64::from(self.nominal_mhz);
        (p as u32, true)
    }
}

const AML_BUFFER: u8 = 0x11;
/// Generic Register descriptor (large item).
const GAS_DESCRIPTOR: u8 = 0x82;
const GAS_SYSTEM_MEMORY: u8 = 0x00;

/// Loopt de AML-bytes af op `_CPC` (na NameOp) en parseert het Package
/// erachter; de laatst geziene `_UID`-integer is de processor. Voegt toe
/// aan `out` tot die vol is.
pub fn scan(aml: &[u8], out: &mut BoundedVec<Cpc, MAX_CPCS>) {
    let mut uid = None;
    let mut i = 0;
    while i + 5 <= aml.len() {
        if aml.get(i) == Some(&NAME_OP) {
            match aml.get(i + 1..i + 5) {
                Some(b"_UID") => {
                    if let Ok((v, _)) = integer(aml, i + 5) {
                        uid = Some(v as u32);
                    }
                }
                Some(b"_CPC") => {
                    if let (Some(u), Some(mut c)) = (uid, package(aml, i + 5)) {
                        c.uid = u;
                        if out.push(c).is_err() {
                            return;
                        }
                    }
                }
                _ => {}
            }
        }
        i += 1;
    }
}

/// Eén element van het package: een integer óf een register.
#[derive(Clone, Copy, Default)]
struct Elem {
    is_reg: bool,
    val: u64,
    space: u8,
    bits: u8,
    addr: u64,
}

/// Het Package op `i`, element voor element.
fn package(b: &[u8], i: usize) -> Option<Cpc> {
    if *b.get(i)? != PACKAGE_OP {
        return None;
    }
    let (l, n) = pkg_len(b, i + 1)?;
    let end = i + 1 + l;
    if end > b.len() {
        return None;
    }
    let mut p = i + 1 + n;
    let count = usize::from(*b.get(p)?);
    p += 1;
    let mut elems = [Elem::default(); 24];
    let mut k = 0;
    while k < count && p < end {
        let e = if let Ok((v, w)) = integer(b, p) {
            p += w;
            Elem {
                val: v,
                ..Elem::default()
            }
        } else {
            // Iets anders dan een buffer (een Method-call, een naam): niet
            // ons patroon.
            if *b.get(p)? != AML_BUFFER {
                return None;
            }
            let (bl, bn) = pkg_len(b, p + 1)?;
            let buf_end = p + 1 + bl;
            if buf_end > end {
                return None;
            }
            // BufferSize (een integer-constante), dan de ruwe bytes.
            let (_, w) = integer(b, p + 1 + bn).ok()?;
            let q = p + 1 + bn + w;
            let mut e = Elem {
                is_reg: true,
                ..Elem::default()
            };
            // Generic Register (ACPI 6.5 §6.4.3.7): +3 AddressSpaceID, +4
            // BitWidth, +5 BitOffset, +6 AccessSize, +7 Address. Tot 17-09
            // stond hier +6: de AccessSize (3 = dword) werd de laagste byte
            // van het adres, 0x0659009c werd 0x659009c03, en de eerste lees
            // daarvan op de O6N een external abort.
            if q + 15 <= buf_end && b.get(q..q + 3) == Some(&[GAS_DESCRIPTOR, 0x0c, 0x00][..]) {
                e.space = *b.get(q + 3)?;
                e.bits = *b.get(q + 4)?;
                e.addr = le64(b, q + 7)?;
            }
            p = buf_end;
            e
        };
        if let Some(slot) = elems.get_mut(k) {
            *slot = e;
        }
        k += 1;
    }
    // ACPI 8.4.6.1.1: 0 NumEntries, 1 Revision, 2 Highest, 3 Nominal, 4
    // LowestNonlinear, 5 Lowest, 6 GuaranteedReg, 7 DesiredReg.
    if k < 8 {
        return None;
    }
    // Een register in plaats van een constante: niet ondersteund, 0.
    let pick = |i: usize| {
        elems
            .get(i)
            .filter(|e| !e.is_reg && i < k)
            .map_or(0, |e| e.val as u32)
    };
    let mut c = Cpc {
        highest: pick(2),
        nominal: pick(3),
        lowest_nl: pick(4),
        lowest: pick(5),
        ..Cpc::default()
    };
    if let Some(d) = elems.get(7)
        && d.is_reg
        && d.space == GAS_SYSTEM_MEMORY
        && d.addr != 0
    {
        c.desired_reg = d.addr;
        c.desired_bits = d.bits;
    }
    if k > 22 {
        c.lowest_mhz = pick(21);
        c.nominal_mhz = pick(22);
    }
    Some(c)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::vec::Vec;

    const AML_ZERO: u8 = 0x00;
    const AML_BYTE: u8 = 0x0a;
    const AML_DWORD: u8 = 0x0c;

    /// Een `Register(SystemMemory, bits, 0, addr, access)` als AML-buffer.
    fn reg(bits: u8, addr: u64) -> Vec<u8> {
        let mut gas = std::vec![GAS_DESCRIPTOR, 0x0c, 0x00, GAS_SYSTEM_MEMORY, bits, 0, 3];
        gas.extend(addr.to_le_bytes());
        gas.extend([0x79, 0x00]); // EndTag
        let mut b = std::vec![AML_BUFFER];
        let body_len = 2 + gas.len(); // ByteConst size + bytes
        b.push((1 + body_len) as u8); // PkgLength, één byte
        b.extend([AML_BYTE, gas.len() as u8]);
        b.extend(gas);
        b
    }

    fn dword(v: u32) -> Vec<u8> {
        let mut b = std::vec![AML_DWORD];
        b.extend(v.to_le_bytes());
        b
    }

    /// `Name(_UID, uid)` en `Name(_CPC, Package(23) {...})` zoals de Cix-DSDT
    /// ze draagt.
    fn processor(uid: u32, highest: u32, nl: u32, lowest: u32, desired: u64) -> Vec<u8> {
        let mut elems: Vec<Vec<u8>> = std::vec![
            std::vec![AML_BYTE, 23],
            std::vec![AML_BYTE, 3],
            dword(highest),
            dword(highest * 8 / 10),
            dword(nl),
            dword(lowest),
            reg(32, 0),
            reg(32, desired),
        ];
        while elems.len() < 21 {
            elems.push(std::vec![AML_ZERO]);
        }
        elems.push(dword(800)); // LowestFrequency
        elems.push(dword(highest * 8 / 10 * 2600 / 8192)); // NominalFrequency
        let body: Vec<u8> = elems.concat();
        let len = 1 + 1 + body.len() + 1; // count + body, PkgLength twee bytes
        let mut pkg = std::vec![PACKAGE_OP, 0x40 | (len & 0x0f) as u8, (len >> 4) as u8, 23];
        pkg.extend(body);
        let mut out = std::vec![NAME_OP];
        out.extend(b"_UID");
        out.extend(dword(uid));
        out.extend([NAME_OP]);
        out.extend(b"_CPC");
        out.extend(pkg);
        out
    }

    #[test]
    fn scans_uid_and_cpc_pairs_from_aml() {
        let mut aml = std::vec![0x5b, 0x82]; // wat rommel ervoor
        aml.extend(processor(0, 2232, 800, 400, 0x0659_0000));
        aml.extend(processor(4, 8192, 800, 400, 0x0659_009c));
        let mut out = BoundedVec::new();
        scan(&aml, &mut out);
        let c = out.as_slice();
        assert_eq!(c.len(), 2);
        assert_eq!(
            (c[1].uid, c[1].highest, c[1].lowest_nl, c[1].lowest),
            (4, 8192, 800, 400)
        );
        assert_eq!((c[1].desired_reg, c[1].desired_bits), (0x0659_009c, 32));
        assert_eq!(c[1].lowest_mhz, 800);
        assert_eq!(c[1].mhz(c[1].nominal), c[1].nominal_mhz);
        let (p, ok) = c[1].perf(c[1].nominal_mhz);
        assert!(ok && p.abs_diff(c[1].nominal) <= 1);
    }

    #[test]
    fn a_cpc_without_uid_or_with_a_method_is_skipped() {
        let mut aml = processor(1, 2000, 800, 400, 0x1000);
        aml.drain(..9); // weg met Name(_UID, ...)
        let mut out = BoundedVec::new();
        scan(&aml, &mut out);
        assert!(out.is_empty());
        // Een package waarvan element 2 een naam is (een Method-verwijzing).
        let mut odd = processor(1, 2000, 800, 400, 0x1000);
        let at = odd.windows(4).position(|w| w == b"_CPC").unwrap() + 4 + 4;
        odd[at] = b'X';
        let mut out = BoundedVec::new();
        scan(&odd, &mut out);
        assert!(out.is_empty());
        assert_eq!(Cpc::default().perf(1800), (1800, false));
        assert_eq!(Cpc::default().mhz(1800), 0);
    }
}
