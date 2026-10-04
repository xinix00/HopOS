//! Het TRNG-blok van de RK3566 en de RK3568 (DT `rockchip,rk3568-rng`):
//! een los blok met een oscillatorring, geen crypto-engine.
//!
//! De specificatie is `OLD/metal/board/rk3566/trng.go`, en daarachter Linux
//! `drivers/char/hw_random/rockchip-rng.c` (de rk3568-tak: `rk3568_rng_init`,
//! `rk3568_rng_read`, `rk3568_rng_cleanup`, nagelezen 30-09 op master). Per
//! ronde: START zetten in RNG_CTL, wachten tot het blok hem zelf laat vallen
//! (Linux: hoogstens 10 ms), dan 32 bytes uit RNG_DOUT. RNG_CTL is
//! hiword-masked: de bovenste 16 bits zeggen welke van de onderste 16 de
//! schrijf raakt.
//!
//! Dit bezit het registermodel en één trekking. Niet van hier: het adres, de
//! klokgates en de reset (het board, `board_rk3566::soc`), en wat er gebeurt
//! als het blok niets levert. Dat is de DRBG van de kern (`cpu::drbg`), die
//! dan op timing-jitter zaait.
//!
//! GEMETEN in Go (06-08, drie boots): twee onafhankelijke rondes, de DRBG
//! erop omgehangen. Terwijl geen enkel mainline-board de node op `okay` zet
//! (rk356x-base.dtsi: `status = "disabled"`), en de DTB van de Radxa-U-Boot
//! hem helemaal niet noemt.
//!
//! Het blok heeft geen eigen gezondheidstoets; Linux geeft hem kwaliteit 900
//! (ongeveer 87,5% van de FIPS 140-2-toetsen). Dit is een SEED, geen stroom,
//! en hij hoort achter de SHA-256-DRBG. Twee controles die goedkoop zijn:
//!
//! - een ronde van 32 nullen is geen entropie maar een blok zonder klok (een
//!   ongeklokt Rockchip-blok leest nul, als het de bus al niet vasthoudt);
//!   de kans op een vals alarm is 2^-256;
//! - twee gelijke opeenvolgende rondes is een vastgelopen bron, de continue
//!   toets van FIPS 140-2 §4.9.2 per ronde van 256 bits.
//!
//! Sans-I/O: de driver praat via [`dev::Io`], op ijzer [`dev::Mmio`] en op
//! de host een nep-blok (tests.rs). Eén eigenaar (de DRBG op de executor van de
//! OS-core), dus `&mut self` en geen slot.

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

use core::fmt;
use dev::Io;

#[cfg(test)]
mod tests;

/// RNG_CTL: START, ENABLE, de ringsnelheid en de lengte; hiword-masked.
pub const RNG_CTL: u64 = 0x0400;
/// RNG_SAMPLE_CNT: oscillatorcycli per bit, een gewone schrijf.
pub const RNG_SAMPLE_CNT: u64 = 0x0404;
/// RNG_DOUT: acht woorden, samen 32 bytes.
pub const RNG_DOUT: u64 = 0x0410;
/// De grootte van het registerblok (rk356x-base.dtsi: `reg = <... 0x4000>`).
pub const SIZE: u64 = 0x4000;

/// START: de trigger van één ronde; het blok laat hem vallen als hij klaar is.
const CTL_START: u32 = 1 << 0;
/// ENABLE: de oscillatorring aan.
const CTL_ENABLE: u32 = 1 << 1;
/// Ringsnelheid 0 in [3:2] (Linux `TRNG_RNG_CTL_OSC_RING_SPEED_0`).
const CTL_RING0: u32 = 0 << 2;
/// 256 bits per ronde in [5:4].
const CTL_LEN_256: u32 = 3 << 4;
/// Het volle maskerveld: alle onderste 16 bits.
const CTL_MASK: u32 = 0xFFFF;

/// Oscillatorcycli per bit: de waarde waar Linux zijn kwaliteit op baseert
/// (`RK_RNG_SAMPLE_CNT`). Lager is sneller en slechter.
pub const SAMPLES: u32 = 1000;
/// Bytes per ronde.
pub const ROUND: usize = 32;
/// Hoe lang één ronde mag duren (Linux `RK_RNG_POLL_TIMEOUT_US`).
pub const ROUND_NS: u64 = 10_000_000;

/// Waarom het TRNG niets leverde.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// START viel niet binnen [`ROUND_NS`]; RNG_CTL staat erbij.
    Timeout(u32),
    /// Een ronde van 32 nullen: geen klok, geen blok.
    Zero,
    /// Twee gelijke opeenvolgende rondes (de continue toets); de onderste
    /// helft van hun [`fingerprint`] staat erbij.
    Stuck(u32),
    /// De doelbuffer was leeg.
    Empty,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Timeout(ctl) => write!(
                f,
                "START still set after {} ms (RNG_CTL {ctl:#x})",
                ROUND_NS / 1_000_000
            ),
            Self::Zero => f.write_str("a round of 32 zero bytes, the block is not clocked"),
            Self::Stuck(w) => write!(
                f,
                "two equal rounds in a row (fingerprint {w:#010x}), source stuck"
            ),
            Self::Empty => f.write_str("empty destination buffer"),
        }
    }
}

/// Het resultaat van deze driver.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén TRNG: de registers, de klok en de vingerafdruk van de vorige ronde
/// (voor de continue toets). Een afdruk en niet de ronde zelf: die bytes zijn
/// seed-materiaal, en de DRBG wist zijn seed na gebruik.
pub struct Trng<R: Io> {
    regs: R,
    clock: fn() -> u64,
    last: Option<u64>,
}

impl<R: Io> Trng<R> {
    /// Een TRNG; raakt nog geen register aan. `clock` geeft monotone
    /// nanoseconden.
    pub const fn new(regs: R, clock: fn() -> u64) -> Self {
        Self {
            regs,
            clock,
            last: None,
        }
    }

    /// Vult `dst` volledig, per ronde van 32 bytes, en zet de ring daarna
    /// weer uit (Linux `rk3568_rng_cleanup`; de DRBG trekt alleen bij de boot
    /// en elke MB). Een fout laat `dst` half gevuld achter; de aanroeper
    /// gebruikt hem dan niet.
    pub fn fill(&mut self, dst: &mut [u8]) -> Result {
        if dst.is_empty() {
            return Err(Error::Empty);
        }
        self.start();
        let r = self.fill_rounds(dst);
        self.stop();
        r
    }

    /// De rondes van [`fill`](Self::fill), met de ring aan.
    fn fill_rounds(&mut self, dst: &mut [u8]) -> Result {
        for chunk in dst.chunks_mut(ROUND) {
            let round = self.round()?;
            chunk.copy_from_slice(round.get(..chunk.len()).unwrap_or(&round));
        }
        Ok(())
    }

    /// De aan-stand van `rk3568_rng_init`: de sample-telling, dan ring,
    /// snelheid en lengte in één schrijf met het volle masker.
    fn start(&mut self) {
        self.regs.write(RNG_SAMPLE_CNT, SAMPLES);
        self.regs.write(
            RNG_CTL,
            (CTL_MASK << 16) | CTL_LEN_256 | CTL_RING0 | CTL_ENABLE,
        );
    }

    /// Alles uit (het volle masker, waarde nul).
    fn stop(&mut self) {
        self.regs.write(RNG_CTL, CTL_MASK << 16);
    }

    /// Eén ronde: START (alleen die bit in het masker, anders wissen we
    /// ENABLE en de lengte), wachten tot hij valt, 32 bytes lezen, en de
    /// twee controles.
    fn round(&mut self) -> Result<[u8; ROUND]> {
        self.regs.write(RNG_CTL, (CTL_START << 16) | CTL_START);
        let mut ctl = 0;
        if !dev::poll_until(self.clock, ROUND_NS, || {
            ctl = self.regs.read(RNG_CTL);
            ctl & CTL_START == 0
        }) {
            return Err(Error::Timeout(ctl));
        }
        let mut out = [0u8; ROUND];
        for (i, w) in out.chunks_exact_mut(4).enumerate() {
            let v = self.regs.read(RNG_DOUT + 4 * i as u64);
            w.copy_from_slice(&v.to_le_bytes());
        }
        if out.iter().all(|&b| b == 0) {
            return Err(Error::Zero);
        }
        let print = fingerprint(&out);
        if self.last == Some(print) {
            return Err(Error::Stuck(print as u32));
        }
        self.last = Some(print);
        Ok(out)
    }
}

/// FNV-1a over een ronde: 64 bits volstaan voor de continue toets (een vals
/// alarm is 2^-64 per ronde) en zeggen niets bruikbaars over de seed.
#[must_use]
pub fn fingerprint(b: &[u8]) -> u64 {
    b.iter().fold(0xcbf2_9ce4_8422_2325, |h, &x| {
        (h ^ u64::from(x)).wrapping_mul(0x0000_0100_0000_01b3)
    })
}
