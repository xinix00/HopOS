//! Het contract tussen een blok-driver en hopfs.
//!
//! Precies de grens die hopfs nodig heeft: `buf.len()` bytes lezen of
//! schrijven vanaf een LBA van 512 bytes, en alles wat geschreven is
//! duurzaam maken. De virtio-blk-driver op QEMU en straks de NVMe-driver op
//! ijzer leveren hem; de tests een schijf in RAM. Het trait woont hier en
//! niet in `kern`, omdat een driver de kern niet kent (handboek §7, dezelfde
//! reden als `netdev`).
//!
//! De grens is synchroon: een verzoek keert terug als het klaar is. Dat
//! houdt de executor even vast; een asynchrone vorm komt met de splitsing
//! in een metadata-actor en een blok-actor (PORT.md §3).

#![cfg_attr(not(test), no_std)]
#![forbid(unsafe_code)]

use core::fmt;

/// Waarom een blok-verzoek niet lukte. Elke variant draagt de LBA, want
/// "I/O failed" zonder plek is niets waard op een headless node.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// Het device meldde een fout, of antwoordde niet binnen zijn grens.
    Io {
        /// De LBA waar het verzoek begon.
        lba: u64,
    },
    /// Het verzoek valt buiten de schijf.
    OutOfRange {
        /// De LBA waar het verzoek begon.
        lba: u64,
        /// De lengte in bytes.
        len: usize,
    },
    /// Het device is blijvend dood (een stil device kan nog in zijn
    /// DMA-buffer schrijven, dus na één stilte is er geen weg terug).
    Dead,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io { lba } => write!(f, "block I/O failed at LBA {lba}"),
            Self::OutOfRange { lba, len } => {
                write!(
                    f,
                    "block request {len} bytes at LBA {lba} falls outside the disk"
                )
            }
            Self::Dead => f.write_str("block device is dead"),
        }
    }
}

/// Het resultaat van een blok-verzoek.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Een blokapparaat zoals hopfs het ziet.
pub trait BlockDevice {
    /// Leest `buf.len()` bytes vanaf `lba`.
    fn read(&mut self, lba: u64, buf: &mut [u8]) -> Result;
    /// Schrijft `buf` vanaf `lba`.
    fn write(&mut self, lba: u64, buf: &[u8]) -> Result;
    /// Maakt alles wat geschreven is duurzaam (NVMe FLUSH, virtio FLUSH).
    fn flush(&mut self) -> Result {
        Ok(())
    }
}

impl<D: BlockDevice + ?Sized> BlockDevice for &mut D {
    fn read(&mut self, lba: u64, buf: &mut [u8]) -> Result {
        (**self).read(lba, buf)
    }
    fn write(&mut self, lba: u64, buf: &[u8]) -> Result {
        (**self).write(lba, buf)
    }
    fn flush(&mut self) -> Result {
        (**self).flush()
    }
}
