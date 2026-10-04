//! De hardware-entropie van de Radxa: het TRNG-blok van de RK3566
//! (`driver-rkrng`) als bron van de DRBG van de kern (Go:
//! `OLD/metal/board/rk3566/rng.go` en `trng.go`, `UseHardwareRNG`).
//!
//! Dit bezit het ene TRNG van deze boot en de [`cpu::trng::Fill`] die de
//! DRBG krijgt. Niet van hier: de DRBG zelf en de jitter-terugval
//! (`cpu::drbg`). Bewust niet via `cpu::trng::fill`: de Cortex-A55 heeft geen
//! FEAT_RNG, en of TF-A van Rockchip DEN 0098 kent is nooit gemeten.
//!
//! Het adres komt uit rk356x-base.dtsi (`rng@fe388000`, daar `status =
//! "disabled"` op elk board), niet uit de DTB: die van de Radxa-U-Boot
//! (2023.10, uit de donor) noemt het blok helemaal niet. Dat is waarom dit
//! board, anders dan de Pi's, niet op de DTB wacht voordat het het
//! blok aanraakt: de toestemming is de meting (Go, 06-08, drie boots: twee
//! onafhankelijke rondes), en de klokken en de reset gaan eerst
//! ([`crate::soc::trng_clock_on`]).
//!
//! Eén eigenaar: de executor van de OS-core. De DRBG trekt alleen daar (hij
//! is zelf een `LocalCell`), bij de boot en bij elke herzaaiing.

use cpu::trng;
use dev::{Mmio, Pa};
use driver_rkrng::Trng;
use sync::LocalCell;

/// Het TRNG-blok (rk356x-base.dtsi: `rng@fe388000`, `rockchip,rk3568-rng`).
pub(crate) const TRNG: Pa = Pa(0xFE38_8000);

/// De naam van de bron, in de boot-log en in `cpu::drbg::source`.
pub(crate) const NAME: &str = "rk3568-rng";

const _: () = {
    // In de Device-MMIO van de SoC (de gigabyte vanaf 0xC000_0000, `mmu`).
    assert!(TRNG.0 >= crate::RAM_MAPPED_END && TRNG.0 + driver_rkrng::SIZE <= 1 << 32);
};

/// Het TRNG van deze boot en zijn laatste fout (voor de bootregel).
struct Source {
    trng: Trng<Mmio>,
    last: Option<driver_rkrng::Error>,
}

/// De bron; `None` tot [`seed`].
static SOURCE: LocalCell<Option<Source>> = LocalCell::cell(None);

/// De [`trng::Fill`] van de Radxa: het TRNG, anders een fout (en dan zaait
/// de DRBG op jitter).
fn fill(dst: &mut [u8]) -> trng::Result<trng::Kind> {
    let mut s = SOURCE.borrow_mut();
    let src = s.as_mut().ok_or(trng::Error::NoSource)?;
    match src.trng.fill(dst) {
        Ok(()) => Ok(trng::Kind::Soc(NAME)),
        Err(e) => {
            src.last = Some(e);
            Err(match e {
                driver_rkrng::Error::Empty => trng::Error::Empty,
                _ => trng::Error::Exhausted(trng::Kind::Soc(NAME)),
            })
        }
    }
}

/// Zaait de DRBG van de kern uit het TRNG en zegt in één regel waar de
/// entropie vandaan komt. Eén keer, in `discover`, vóór de eerste `spawn`
/// (`cpu::drbg::init`).
pub(crate) fn seed() {
    crate::soc::trng_clock_on();
    // SAFETY: TRNG is het TRNG-blok van de RK3566, Device-gemapt (de
    // const-assertie hierboven), zijn klokken zijn net geopend en zijn reset
    // gepulst; alleen deze module raakt het aan.
    let trng = Trng::new(unsafe { Mmio::new(TRNG) }, cpu::idle::now);
    *SOURCE.borrow_mut() = Some(Source { trng, last: None });
    cpu::drbg::init(fill, cpu::idle::counter);
    match cpu::drbg::source() {
        cpu::drbg::Source::Hardware(k) => cpu::println!(
            "trng: {NAME} at {:#x} online (not in the U-Boot DTB; the address from rk356x-base.dtsi, measured in Go 06-08), the kernel DRBG is seeded from {k} HOPOS_RNG_RK3566_UP",
            TRNG.0
        ),
        cpu::drbg::Source::Jitter => {
            let why = SOURCE.borrow().as_ref().and_then(|s| s.last);
            let why: &dyn core::fmt::Display = match &why {
                Some(e) => e,
                None => &"no answer",
            };
            cpu::println!(
                "trng: WARNING {NAME} at {:#x}: {why}; the DRBG runs on jitter-seeded entropy, not hardware entropy; avoid high-value secrets on this node HOPOS_RNG_INSECURE",
                TRNG.0
            );
        }
    }
}
