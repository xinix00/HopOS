//! Het ene app-naar-kern-callcontract: frames over een blijvende
//! TCP-verbinding naar 10.100.0.1:10100 op het geïsoleerde slot-LAN.
//!
//! ARM64 en RISC-V spreken exact deze bytes; alleen hun ring-doorbell onder
//! het netwerk verschilt. Een frame is een kop van 12 bytes plus payload:
//!
//! ```text
//! magic u32 ("HOPS", little-endian) | version u8 | kind u8 | 0 u16 | len u32
//! ```
//!
//! Een `Call` draagt een [`crate::hopabi::Req`], een `Result` een
//! [`crate::hopabi::Resp`], een `Log` een logregel.
//!
//! Hier staan ook de **bevoegde operaties** ([`PrivOp`], PORT.md §6
//! beslissing 1b): de agent draait als bevoorrechte app in
//! [`PRIVILEGED_SLOT`] en bestuurt de lifecycle over deze API. De kern en de
//! agent delen de opcodes; de kern weigert ze van elk ander slot.
//!
//! Wat hier NIET staat: de socket zelf. De kop wordt hier geschreven en
//! gelezen ([`encode_header`], [`HeaderReader`]); de bytes verplaatsen doet
//! de netstack van wie de verbinding bezit. Een payload van een MiB landt zo
//! rechtstreeks in de buffer van de lezer, zonder tussenkopie (04-09: elke
//! verse MiB per call was GC-werk aan beide kanten).

use crate::layout::{HOST_IP4, Slot};
use crate::{Error, Result};

/// De versie van het frame-formaat.
pub const VERSION: u8 = 1;
/// De TCP-poort van de kern op het slot-LAN.
pub const PORT: u16 = 10100;
/// Het adres van de kern op het slot-LAN: 10.100.0.1.
pub const ADDRESS: [u8; 4] = HOST_IP4.to_be_bytes();

const _: () = assert!(u32::from_be_bytes(ADDRESS) == HOST_IP4);
const _: () = assert!(ADDRESS[0] == 10 && ADDRESS[1] == 100 && ADDRESS[3] == 1);

/// De grootste I/O-hap: amortiseert protocol- en opslagkosten zonder ooit
/// een heel bestand in kerngeheugen te houden.
pub const MAX_IO_CHUNK: usize = 1 << 20;
/// De grootste payload: een hap plus ruimte voor kop en pad.
pub const MAX_PAYLOAD: usize = MAX_IO_CHUNK + (64 << 10);
/// De lengte van de framekop.
pub const HEADER_LEN: usize = 12;
/// Het magische getal: "HOPS" little-endian op de draad.
pub const MAGIC: u32 = 0x5350_4f48;

const _: () = assert!(MAX_PAYLOAD <= u32::MAX as usize);
const _: () = assert!(u32::from_le_bytes(*b"HOPS") == MAGIC);

/// De soort van een frame.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum Kind {
    /// App naar kern: een request.
    Call = 1,
    /// Kern naar app: de response.
    Result = 2,
    /// App naar kern: een logregel.
    Log = 3,
}

impl Kind {
    /// De soort van een rauw getal.
    pub const fn from_raw(v: u8) -> Result<Kind> {
        match v {
            1 => Ok(Self::Call),
            2 => Ok(Self::Result),
            3 => Ok(Self::Log),
            _ => Err(Error::BadKind(v)),
        }
    }
}

/// Een gelezen framekop.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Header {
    /// De soort.
    pub kind: Kind,
    /// De payloadlengte, hoogstens [`MAX_PAYLOAD`].
    pub len: usize,
}

/// Toetst een payloadlengte tegen [`MAX_PAYLOAD`].
fn check_len(len: usize) -> Result {
    if len > MAX_PAYLOAD {
        return Err(Error::PayloadTooLarge {
            len,
            max: MAX_PAYLOAD,
        });
    }
    Ok(())
}

/// Schrijft de kop van een begrensd, zelfbeschrijvend frame. De lengte
/// wordt vóór het schrijven getoetst: een te groot frame gaat nooit de
/// draad op.
pub fn encode_header(kind: Kind, len: usize) -> Result<[u8; HEADER_LEN]> {
    check_len(len)?;
    let mut h = [0u8; HEADER_LEN];
    h[0..4].copy_from_slice(&MAGIC.to_le_bytes());
    h[4] = VERSION;
    h[5] = kind as u8;
    // Geen omloop: check_len begrensde op MAX_PAYLOAD < u32::MAX.
    h[8..12].copy_from_slice(&(len as u32).to_le_bytes());
    Ok(h)
}

/// Leest en toetst een framekop: magie, versie, soort en maat worden
/// geweigerd vóór er één payloadbyte gelezen is.
pub fn decode_header(h: &[u8; HEADER_LEN]) -> Result<Header> {
    let [m0, m1, m2, m3, ver, kind, _, _, l0, l1, l2, l3] = *h;
    let magic = u32::from_le_bytes([m0, m1, m2, m3]);
    if magic != MAGIC {
        return Err(Error::BadMagic(magic));
    }
    if ver != VERSION {
        return Err(Error::BadVersion {
            got: ver,
            want: VERSION,
        });
    }
    let len = u32::from_le_bytes([l0, l1, l2, l3]) as usize;
    check_len(len)?;
    Ok(Header {
        kind: Kind::from_raw(kind)?,
        len,
    })
}

/// Verzamelt een framekop uit stukken zoals TCP ze levert.
///
/// Een verbinding levert bytes in willekeurige happen, tot één byte per
/// keer; de lezer voert ze hier in tot de kop compleet is en leest daarna
/// de payload waar hij hem hebben wil.
#[derive(Clone, Debug, Default)]
pub struct HeaderReader {
    buf: [u8; HEADER_LEN],
    have: usize,
}

impl HeaderReader {
    /// Een lege lezer.
    #[must_use]
    pub const fn new() -> HeaderReader {
        HeaderReader {
            buf: [0; HEADER_LEN],
            have: 0,
        }
    }

    /// Neemt bytes uit `src` tot de kop compleet is; geeft hoeveel er
    /// genomen zijn. De rest van `src` is payload.
    pub fn fill(&mut self, src: &[u8]) -> usize {
        let want = HEADER_LEN - self.have;
        let n = want.min(src.len());
        self.buf[self.have..self.have + n].copy_from_slice(&src[..n]);
        self.have += n;
        n
    }

    /// Is de kop compleet?
    #[must_use]
    pub fn is_complete(&self) -> bool {
        self.have == HEADER_LEN
    }

    /// De getoetste kop, of `None` zolang hij niet compleet is. Zet de
    /// lezer terug voor het volgende frame.
    pub fn take(&mut self) -> Option<Result<Header>> {
        if !self.is_complete() {
            return None;
        }
        self.have = 0;
        Some(decode_header(&self.buf))
    }
}

/// Het slot van de bevoorrechte app: de agent (PORT.md §6 beslissing 1b).
/// Alleen dit slot mag een [`PrivOp`] aanroepen.
pub const PRIVILEGED_SLOT: Slot = Slot::FIRST;

/// De bevoegde operaties: het opnummer van een [`crate::hopabi::Req`] in
/// een `Call`-frame, boven de gewone ops ([`crate::hopabi::OP_MAX`]).
///
/// Hoe de velden van de request gebruikt worden, staat per operatie; de
/// response draagt de status en `size` zoals elke call.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum PrivOp {
    /// Start een slot: `n` het slot, `off` de partitiemaat in bytes, `path`
    /// de naam van een eerder gestreamd image, `data` de env-blob. Niet
    /// idempotent.
    StartSlot = 0x40,
    /// Stop een slot: `n` het slot. Gelukt betekent bevestigd gestopt; een
    /// onzekere stop is een fout en het slot blijft in quarantaine.
    StopSlot = 0x41,
    /// De status van een slot: `n` het slot; `size` de
    /// [`crate::hopabi::AppStatus`], `data` de laatste woorden.
    SlotStatus = 0x42,
    /// Stream een image naar de kern: `path` de naam, `off` de offset in het
    /// image, `n` de totale maat, `data` een hap van hoogstens
    /// [`MAX_IO_CHUNK`].
    StreamImage = 0x43,
    /// Zet de klok: `off` de wall-ns bij tellerstand 0, als
    /// [`crate::hopabi::CTRL_WALL_OFF`].
    SetClock = 0x44,
    /// Flip naar een nieuwe kern: `path` de naam van een gestreamd
    /// kern-image.
    Flip = 0x45,
}

const _: () = assert!(PrivOp::StartSlot as u8 > crate::hopabi::OP_MAX);

impl PrivOp {
    /// De bevoegde operatie van een opnummer, of `None` voor een gewone of
    /// onbekende op.
    #[must_use]
    pub const fn from_op(op: u8) -> Option<PrivOp> {
        Some(match op {
            0x40 => Self::StartSlot,
            0x41 => Self::StopSlot,
            0x42 => Self::SlotStatus,
            0x43 => Self::StreamImage,
            0x44 => Self::SetClock,
            0x45 => Self::Flip,
            _ => return None,
        })
    }

    /// Het opnummer.
    #[must_use]
    pub const fn op(self) -> u8 {
        self as u8
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frame_roundtrip_en_fragmentatie() {
        let p = vec![0xa5u8; MAX_IO_CHUNK];
        let mut wire = encode_header(Kind::Call, p.len()).unwrap().to_vec();
        wire.extend_from_slice(&p);
        // Eén byte per keer, zoals de oneByteReader van de Go-test.
        let mut r = HeaderReader::new();
        let mut pos = 0;
        while !r.is_complete() {
            pos += r.fill(&wire[pos..pos + 1]);
        }
        let h = r.take().unwrap().unwrap();
        assert_eq!(
            h,
            Header {
                kind: Kind::Call,
                len: MAX_IO_CHUNK
            }
        );
        assert_eq!(&wire[pos..pos + h.len], &p[..]);
        assert!(
            !r.is_complete(),
            "lezer staat klaar voor het volgende frame"
        );
    }

    #[test]
    fn rejects_oversize_before_write() {
        assert!(matches!(
            encode_header(Kind::Call, MAX_PAYLOAD + 1),
            Err(Error::PayloadTooLarge { .. })
        ));
    }

    #[test]
    fn kop_weigert_magie_versie_soort_en_maat() {
        let good = encode_header(Kind::Log, 5).unwrap();
        let mut bad = good;
        bad[0] ^= 1;
        assert!(matches!(decode_header(&bad), Err(Error::BadMagic(_))));
        let mut bad = good;
        bad[4] = 2;
        assert!(matches!(decode_header(&bad), Err(Error::BadVersion { .. })));
        let mut bad = good;
        bad[5] = 9;
        assert_eq!(decode_header(&bad), Err(Error::BadKind(9)));
        let mut bad = good;
        bad[8..12].copy_from_slice(&u32::MAX.to_le_bytes());
        assert!(matches!(
            decode_header(&bad),
            Err(Error::PayloadTooLarge { .. })
        ));
    }

    #[test]
    fn bevoegde_ops_botsen_niet() {
        for op in 0..=u8::MAX {
            if let Some(p) = PrivOp::from_op(op) {
                assert_eq!(p.op(), op);
                assert!(op > crate::hopabi::OP_MAX);
            }
        }
    }
}
