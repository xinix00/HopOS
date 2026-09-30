//! De initrd van de Radxa: één bestand dat `hopos.cfg` en het image van de
//! bewoner samen draagt, en de splitsing ervan bij [`Board::discover`].
//!
//! Waarom één bestand (30-09, de keuze tussen drie wegen):
//!
//! - U-Boot van de donor (radxa-build/radxa-zero3 b6: "U-Boot
//!   latest-2023.10-8-eed05a18") laadt per extlinux-label precies één
//!   initrd: `boot/pxe_utils.c` van v2023.10 geeft `label->initrd` als één
//!   pad aan `get_relfile_envaddr`. Een `initrd /hopos.cfg,/hop.elf` is daar
//!   één bestandsnaam met een komma erin; de load faalt en U-Boot slaat het
//!   hele label over ("Skipping hopos for failure retrieving initrd"): geen
//!   boot. En zelfs waar U-Boot een lijst kent, plakt hij de bestanden
//!   achter elkaar zonder grenzen, dus de kern moest toch splitsen.
//! - Hop in de kern bakken (de weg van de LicheeRV, die geen initrd-kanaal
//!   heeft) koppelt elke kern-build aan de hop-repo, laat elke kern-flip Hop
//!   als dood gewicht meedragen, en maakt van een nieuwe Hop een nieuwe kern.
//! - Eén container houdt de drie GEMETEN kanalen van 05-08 (kernel, initrd,
//!   append) precies zoals ze zijn; alleen de inhoud van de initrd groeit.
//!
//! Een initrd zonder de magic is de oude vorm: kale `hopos.cfg`-tekst zonder
//! image. Een kaart van vóór 30-09 boot dus hetzelfde als toen.
//!
//! De vorm, byte voor byte gelijk aan `image/radxa-initrd.py` (de fixture
//! `testdata/mini.ird` komt uit dat script en houdt de twee gelijk):
//!
//! | Offset | Maat | Wat |
//! | --- | --- | --- |
//! | 0 | 8 | magic `HOPOSIRD` |
//! | 8 | u32 LE | versie ([`VERSION`]) |
//! | 12 | u32 LE | lengte van de config, C |
//! | 16 | C | `hopos.cfg`, UTF-8 |
//! | 16 + C | 0..7 | nullen tot een veelvoud van 8 |
//! | P | u64 LE | lengte van het image, E (0 = geen) |
//! | P + 8 | E | het image (ELF), 8-gealigneerd |
//!
//! Het bestand eindigt precies op P + 8 + E: U-Boot zet `initrd-end` op
//! begin plus bestandsmaat, dus een staart of een tekort is een ander
//! bestand dan het script schreef.
//!
//! Dit module bezit alleen de vorm. De kopie uit het DRAM en wie de delen
//! leest, staan in de crate-root; het image zelf is onvertrouwd en gaat
//! door de ELF-lezer en `abi::place`.
//!
//! [`Board::discover`]: board::Board::discover

use core::fmt;

/// De eerste acht bytes van een container.
pub const MAGIC: [u8; 8] = *b"HOPOSIRD";
/// De versie die deze kern leest.
pub const VERSION: u32 = 1;
/// De vaste kop: magic, versie en de lengte van de config.
pub const HEADER_LEN: usize = 16;

/// De delen van een initrd.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Parts<'a> {
    /// De tekst van `hopos.cfg` (bij de oude vorm: de hele initrd).
    pub cfg: &'a [u8],
    /// Het image van de bewoner, of `None` als er geen in zat.
    pub image: Option<&'a [u8]>,
}

/// Waarom een container niet te splitsen is; de getallen staan erbij.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// De magic staat er, maar de kop past niet in `len` bytes.
    Short {
        /// De maat van de initrd.
        len: usize,
    },
    /// Een versie die deze kern niet kent.
    Version(u32),
    /// De config plus zijn lengteveld voor het image loopt voorbij het einde.
    Cfg {
        /// De lengte uit de kop.
        len: usize,
        /// De maat van de initrd.
        total: usize,
    },
    /// Het image eindigt niet precies op het einde van de initrd.
    Image {
        /// De lengte uit het veld.
        len: u64,
        /// Waar het image begint.
        at: usize,
        /// De maat van de initrd.
        total: usize,
    },
    /// De config is geen UTF-8 (tot deze byte wel).
    NotText {
        /// De offset in de config.
        at: usize,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Short { len } => {
                write!(f, "HOPOSIRD header needs {HEADER_LEN} bytes, have {len}")
            }
            Self::Version(v) => write!(f, "HOPOSIRD version {v}, this kernel reads {VERSION}"),
            Self::Cfg { len, total } => {
                write!(f, "config of {len} bytes runs past the end ({total} bytes)")
            }
            Self::Image { len, at, total } => write!(
                f,
                "image of {len} bytes at {at} does not end at the end ({total} bytes)"
            ),
            Self::NotText { at } => write!(f, "config is not UTF-8 at byte {at}"),
        }
    }
}

/// Het `u32` op `at`, little-endian.
fn le32(b: &[u8], at: usize) -> Option<u32> {
    let w = b.get(at..at.checked_add(4)?)?;
    Some(u32::from_le_bytes(w.try_into().ok()?))
}

/// Het `u64` op `at`, little-endian.
fn le64(b: &[u8], at: usize) -> Option<u64> {
    let w = b.get(at..at.checked_add(8)?)?;
    Some(u64::from_le_bytes(w.try_into().ok()?))
}

/// Is dit een container (en geen kale config)?
#[must_use]
pub fn is_container(b: &[u8]) -> bool {
    b.starts_with(&MAGIC)
}

/// Splitst een initrd in config en image. Zonder magic is de hele initrd de
/// config (de oude vorm); met magic moet elk getal kloppen, anders een
/// [`Error`] en gebruikt de kern geen van beide delen.
pub fn split(b: &[u8]) -> Result<Parts<'_>, Error> {
    if !is_container(b) {
        return Ok(Parts {
            cfg: b,
            image: None,
        });
    }
    let total = b.len();
    let (Some(version), Some(clen)) = (le32(b, 8), le32(b, 12)) else {
        return Err(Error::Short { len: total });
    };
    if version != VERSION {
        return Err(Error::Version(version));
    }
    let clen = clen as usize;
    let cfg_err = Error::Cfg { len: clen, total };
    let cend = HEADER_LEN.checked_add(clen).ok_or(cfg_err)?;
    let p = cend.checked_next_multiple_of(8).ok_or(cfg_err)?;
    let elen = le64(b, p).ok_or(cfg_err)?;
    let at = p.checked_add(8).ok_or(cfg_err)?;
    let image_err = Error::Image {
        len: elen,
        at,
        total,
    };
    let end = usize::try_from(elen)
        .ok()
        .and_then(|e| at.checked_add(e))
        .ok_or(image_err)?;
    if end != total {
        return Err(image_err);
    }
    let cfg = b.get(HEADER_LEN..cend).ok_or(cfg_err)?;
    if let Err(e) = core::str::from_utf8(cfg) {
        return Err(Error::NotText {
            at: e.valid_up_to(),
        });
    }
    let image = b.get(at..end).filter(|i| !i.is_empty());
    Ok(Parts { cfg, image })
}
