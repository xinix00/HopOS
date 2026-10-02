//! Een optische drive (Blu-ray, DVD, CD) als USB mass storage: de
//! bulk-only-transportlaag (BOT 1.0) en de sense van SCSI (Go:
//! `OLD/metal/media/driver/optical`).
//!
//! BOT is drie bulk-transfers per opdracht, altijd in die volgorde: een
//! commandowrapper (CBW) eruit, de data heen of terug, een statuswrapper
//! (CSW) terug. Wat hier NIET in zit is USB: dit crate kent alleen een
//! [`asynchronous::Transport`] met twee pijpen en een uitweg. De xHCI-stack
//! is van het gui-spoor (`driver/usb`), en zo is de hele commandolaag
//! testbaar zonder ijzer.
//!
//! [`asynchronous::Bot`] is de transportlaag: grenzen, één richting, geen
//! herhaling, de CSW-toetsen (signature, tag, residu, status), de
//! reset-recovery op elk pad waar het gesprek zoek is, en REQUEST SENSE.
//! [`mmc::Drive`] staat erboven: openen, de maat en READ(10) over sectoren
//! van 2048 bytes.
//!
//! # Eigendom
//!
//! Een [`asynchronous::Bot`] bezit zijn transport en zijn tag-teller en is
//! van één taak; Go's `commandMu` bestaat niet, want `&mut self` IS "één
//! commando tegelijk".

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]
#![forbid(unsafe_code)]

use core::fmt;

pub mod asynchronous;
pub mod mmc;

/// Een fout van de USB-kant; het getal is dat van de controller.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct UsbError(pub u32);

/// De wrappers van BOT 1.0 §5.1 en §5.2.
const CBW_SIGNATURE: u32 = 0x4342_5355; // "USBC"
const CSW_SIGNATURE: u32 = 0x5342_5355; // "USBS"
/// De lengte van een CBW.
pub const CBW_LEN: usize = 31;
/// De lengte van een CSW.
pub const CSW_LEN: usize = 13;
const CBW_FLAG_IN: u8 = 0x80;

/// REQUEST SENSE.
pub const OP_REQUEST_SENSE: u8 = 0x03;

/// De status in een CSW.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Status {
    /// Het commando lukte.
    Passed,
    /// CHECK CONDITION: de reden staat in de sense.
    Failed,
}

/// Wat een drive antwoordt als hij nee zei: de drie getallen die in elke
/// MMC-tabel staan.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Default)]
pub struct Sense {
    /// Sense key (2 = not ready, 5 = illegal request, ...).
    pub key: u8,
    /// Additional sense code.
    pub asc: u8,
    /// Additional sense code qualifier.
    pub ascq: u8,
}

impl Sense {
    /// De handvol combinaties die een lezende drive echt geeft; de rest
    /// komt als nummers, want een verzonnen tekst helpt niemand.
    #[must_use]
    pub fn what(&self) -> &'static str {
        match (self.key, self.asc, self.ascq) {
            (0, _, _) => "no sense",
            (2, 0x3a, _) => "no medium",
            (2, 0x04, 0x01) => "becoming ready",
            (2, 0x04, _) => "not ready",
            (6, 0x28, _) => "medium changed",
            (6, 0x29, _) => "device reset",
            (5, 0x24, _) => "invalid field in command",
            (5, 0x20, _) => "command not supported",
            (3, _, _) => "medium error",
            _ => "see MMC table",
        }
    }

    /// Leest vaste (0x70/0x71) of descriptor-sense (0x72/0x73).
    #[must_use]
    pub fn parse(buf: &[u8]) -> Option<Sense> {
        match buf.first()? & 0x7f {
            0x70 | 0x71 if buf.len() >= 14 => Some(Sense {
                key: buf[2] & 15,
                asc: buf[12],
                ascq: buf[13],
            }),
            0x72 | 0x73 if buf.len() >= 4 => Some(Sense {
                key: buf[1] & 15,
                asc: buf[2],
                ascq: buf[3],
            }),
            _ => None,
        }
    }
}

/// Waarom een uitwisseling niet lukte.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// Een commandoblok van 0 of meer dan 16 bytes, of data in twee
    /// richtingen: nooit naar USB gestuurd.
    Invalid,
    /// Meer data dan de transport in één keer draagt.
    TooLarge {
        /// De lengte.
        len: usize,
        /// Het maximum.
        max: usize,
    },
    /// De CBW kwam niet weg (de drive is gereset).
    Command(UsbError),
    /// De CSW kwam niet (ook niet na de ene herkansing); gereset.
    StatusTransport(UsbError),
    /// Een CSW die er geen is: te kort, vreemde signature, andere tag,
    /// onbekende status, of een residu groter dan de transfer. Gereset.
    Status {
        /// Het veld dat faalde (offset in de CSW).
        at: u8,
        /// De waarde.
        got: u32,
    },
    /// De datafase faalde terwijl de status "passed" zei: niet verbergen.
    Data(UsbError),
    /// Phase error: de drive is gereset.
    Phase,
    /// De reset zelf faalde ook.
    Reset(UsbError),
    /// CHECK CONDITION met deze sense.
    Check(Sense),
    /// REQUEST SENSE gaf geen bruikbare sense.
    NoSense,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Error::Invalid => f.write_str("optical: invalid command"),
            Error::TooLarge { len, max } => {
                write!(f, "optical: {len} bytes exceed the transport limit {max}")
            }
            Error::Command(e) => write!(f, "optical: command transport: usb {}", e.0),
            Error::StatusTransport(e) => write!(f, "optical: status transport: usb {}", e.0),
            Error::Status { at, got } => write!(f, "optical: bad status at {at}: {got:#x}"),
            Error::Data(e) => write!(
                f,
                "optical: data transport failed despite successful status: usb {}",
                e.0
            ),
            Error::Phase => f.write_str("optical: phase error; the drive was reset"),
            Error::Reset(e) => write!(f, "optical: reset failed: usb {}", e.0),
            Error::Check(s) => write!(
                f,
                "sense {}/{:02x}/{:02x} ({})",
                s.key,
                s.asc,
                s.ascq,
                s.what()
            ),
            Error::NoSense => f.write_str("optical: request sense gave no sense"),
        }
    }
}

/// Het resultaat van een uitwisseling.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// De data van één commando: hoogstens één richting.
pub enum Data<'a> {
    /// Geen datafase.
    None,
    /// Naar de drive.
    Out(&'a [u8]),
    /// Van de drive.
    In(&'a mut [u8]),
}

impl Data<'_> {
    fn len(&self) -> usize {
        match self {
            Data::None => 0,
            Data::Out(b) => b.len(),
            Data::In(b) => b.len(),
        }
    }
}

/// Zet een commandoblok in zijn wrapper (BOT 1.0 §5.1). LUN 0: een
/// optische drive heeft er precies één.
#[must_use]
pub fn build_cbw(tag: u32, len: u32, input: bool, cdb: &[u8]) -> [u8; CBW_LEN] {
    let mut b = [0u8; CBW_LEN];
    b[0..4].copy_from_slice(&CBW_SIGNATURE.to_le_bytes());
    b[4..8].copy_from_slice(&tag.to_le_bytes());
    b[8..12].copy_from_slice(&len.to_le_bytes());
    if input {
        b[12] = CBW_FLAG_IN;
    }
    let n = cdb.len().min(16);
    b[14] = n as u8;
    b[15..15 + n].copy_from_slice(&cdb[..n]);
    b
}

#[cfg(test)]
mod tests;
