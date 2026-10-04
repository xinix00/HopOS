//! De hardware-entropie van de Pi's: de RNG200 van de SoC als bron van de
//! DRBG van de kern (Go: `OLD/metal/board/raspi/rng.go`, de adressen uit
//! `rpi4.go` en `rpi5.go`).
//!
//! Dit bezit de ene RNG200 van deze boot en de [`cpu::trng::Fill`] die de
//! DRBG krijgt. Niet van hier: de DRBG zelf en de jitter-terugval
//! (`cpu::drbg`). Bewust niet via `cpu::trng::fill`: de A72 en de A76 hebben
//! geen FEAT_RNG, en de SMCCC-weg is een SMC naar een EL3 die er op QEMU
//! `raspi4b` wel is maar zonder firmware (de SMC hangt, gemeten 29-09 met
//! PSCI).
//!
//! Eén eigenaar: de executor van core 0. De DRBG trekt alleen daar (hij is
//! zelf een `LocalCell`), bij de boot en bij elke herzaaiing.

use crate::Soc;
use cpu::trng;
use dev::Mmio;
use driver_rng200::Rng200;
use sync::LocalCell;

/// De naam van de bron, in de boot-log en in `cpu::drbg::source`.
pub const NAME: &str = "rng200";

/// De DT-compatible van het blok (beide Pi's).
pub const COMPATIBLE: &str = "brcm,bcm2711-rng200";

/// De RNG200 van deze boot en zijn laatste fout (voor de bootregel).
struct Source {
    rng: Rng200<Mmio>,
    last: Option<driver_rng200::Error>,
}

/// De bron; `None` tot [`seed`].
static SOURCE: LocalCell<Option<Source>> = LocalCell::cell(None);

/// De [`trng::Fill`] van de Pi's: de RNG200, anders een fout (en dan zaait
/// de DRBG op jitter).
fn fill(dst: &mut [u8]) -> trng::Result<trng::Kind> {
    let mut s = SOURCE.borrow_mut();
    let src = s.as_mut().ok_or(trng::Error::NoSource)?;
    match src.rng.fill(dst) {
        Ok(()) => Ok(trng::Kind::Soc(NAME)),
        Err(e) => {
            src.last = Some(e);
            Err(match e {
                driver_rng200::Error::Absent(_) => trng::Error::NoSource,
                driver_rng200::Error::Empty => trng::Error::Empty,
                _ => trng::Error::Exhausted(trng::Kind::Soc(NAME)),
            })
        }
    }
}

/// Zaait de DRBG van de kern uit de RNG200 van `S` en zegt in één regel
/// waar de entropie vandaan komt. Eén keer, in `discover`, vóór de eerste
/// `spawn` (`cpu::drbg::init`).
///
/// `enabled` is wat de DTB over de node zegt ([`crate::device_enabled`]).
/// Alleen `Some(true)` raakt het blok aan: QEMU `raspi4b` haalt de node uit
/// de DTB die hij doorgeeft (hij modelleert geen RNG200), en de eerste
/// lees op 0xFE10_4000 is daar een synchrone external abort (gemeten 30-09:
/// ESR 0x96000010, FAR 0xfe104000). Geen DTB is ook geen toestemming.
pub(crate) fn seed<S: Soc>(enabled: Option<bool>) {
    let refused = match enabled {
        Some(true) => None,
        Some(false) => Some("disabled in the DTB"),
        None => Some("no such node in the DTB (QEMU models none), not touched"),
    };
    if let Some(why) = refused {
        cpu::drbg::init(no_fill, cpu::idle::counter);
        insecure::<S>(format_args!("{COMPATIBLE} {why}"));
        return;
    }
    // SAFETY: S::RNG200 is het RNG200-blok van deze SoC, Device-gemapt in de
    // vaste tabel (const-asserties in het board-crate); alleen deze module
    // raakt het aan.
    let rng = Rng200::new(unsafe { Mmio::new(S::RNG200) }, cpu::idle::now);
    *SOURCE.borrow_mut() = Some(Source { rng, last: None });
    cpu::drbg::init(fill, cpu::idle::counter);
    match cpu::drbg::source() {
        cpu::drbg::Source::Hardware(k) => cpu::println!(
            "trng: RNG200 at {:#x} ({}) online, the kernel DRBG is seeded from {k} HOPOS_RNG200_UP",
            S::RNG200.0,
            S::SOC
        ),
        cpu::drbg::Source::Jitter => {
            let why = SOURCE.borrow().as_ref().and_then(|s| s.last);
            match why {
                Some(e) => insecure::<S>(format_args!("{e}")),
                None => insecure::<S>(format_args!("no answer")),
            }
        }
    }
}

/// Geen bron: de DRBG zaait op jitter.
fn no_fill(_: &mut [u8]) -> trng::Result<trng::Kind> {
    Err(trng::Error::NoSource)
}

/// De luide regel als de RNG200 niets gaf (de LicheeRV-regel,
/// `cpu::drbg::Seeded`): de node draait, maar niet stil.
fn insecure<S: Soc>(why: core::fmt::Arguments<'_>) {
    cpu::println!(
        "trng: WARNING RNG200 at {:#x} ({}): {why}; the DRBG runs on jitter-seeded entropy, not hardware entropy; avoid high-value secrets on this node HOPOS_RNG_INSECURE",
        S::RNG200.0,
        S::SOC
    );
}
