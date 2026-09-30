//! Hardware-entropie op arm64, uit de twee standaardbronnen, in aflopende
//! voorkeur:
//!
//! 1. FEAT_RNG: de RNDR-systeemregisterinstructie (Armv8.5+; onder meer de
//!    Orion O6N). Puur een CPU-instructie, dus overal veilig te proberen:
//!    ontbreekt de feature, dan zegt ID_AA64ISAR0_EL1 nee en raken we RNDR
//!    nooit.
//! 2. SMCCC TRNG (Arm DEN 0098): een firmware-SMC via [`crate::psci::smc4`].
//!    TF-A op de Altra levert dit; QEMU's kale PSCI niet. Een niet-herkende
//!    SMC op een machine zónder EL3-monitor is architecturaal UNDEF (en dus
//!    een crash), daarom proberen we dit alleen als ID_AA64PFR0_EL1.EL3 niet
//!    nul is: dán zit er een monitor onder ons die onbekende functie-ID's
//!    netjes met NOT_SUPPORTED (-1) beantwoordt.
//!
//! Dit bezit: de keuze van de bron en het vullen van een buffer. Niet van
//! hier: wat er gebeurt als er geen bron is. Dat is de [`crate::drbg`], die
//! dan op timing-jitter seedt; deze module zegt alleen hardop dát het zo is
//! ([`describe`]). De les van de LicheeRV (HOPOS_RNG_INSECURE): een node
//! zonder TRNG mag draaien, maar niet stil.
//!
//! [`fill`] (met SMCCC-terugval) is voor de kern; een gekooide app gebruikt
//! [`fill_cpu`]: een app praat nooit met de firmware, HCR_EL2.TSC trapt elke
//! SMC uit de kooi als isolatie-overtreding.
//!
//! Een board met een eigen TRNG-blok in de SoC (de RNG200 van de Pi's) geeft
//! de DRBG zijn eigen [`Fill`], die [`Kind::Soc`] meldt: dezelfde haak,
//! geen tweede weg ernaast.

use crate::psci;
use core::fmt;

/// SMCCC TRNG_VERSION (DEN 0098).
pub const TRNG_VERSION: u32 = 0x8400_0050;
/// SMCCC TRNG_RND64 (64-bit conventie).
pub const TRNG_RND64: u32 = 0xC400_0053;
/// De SMCCC-foutcode "entropie tijdelijk op": opnieuw proberen.
const TRNG_NO_ENTROPY: i64 = -3;
/// Het maximum per TRNG_RND64-call: 192 bits, 24 bytes in x1:x2:x3.
const TRNG_RND_BITS: u64 = 192;

/// Hoe vaak RNDR per woord opnieuw mag als de core net geen woord klaar had.
const RNDR_TRIES: usize = 16;
/// Hoe vaak TRNG_RND64 per blok NO_ENTROPY mag geven.
const SMCCC_TRIES: usize = 128;

/// Welke hardwarebron een buffer vulde.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Kind {
    /// RNDR (FEAT_RNG), een CPU-instructie.
    Rndr,
    /// SMCCC TRNG (DEN 0098), een firmware-call.
    SmcccTrng,
    /// Een TRNG-blok van de SoC, via de driver van het board; de naam is
    /// die van het blok ("rng200").
    Soc(&'static str),
}

impl Kind {
    /// De naam zoals de console en de Go-kern hem noemden.
    #[must_use]
    pub const fn name(self) -> &'static str {
        match self {
            Self::Rndr => "rndr",
            Self::SmcccTrng => "smccc-trng",
            Self::Soc(name) => name,
        }
    }
}

impl fmt::Display for Kind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.name())
    }
}

/// Waarom er geen hardware-entropie kwam.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Error {
    /// Er is geen bron: geen FEAT_RNG, en geen EL3 of geen DEN 0098.
    NoSource,
    /// De bron bestaat maar bleef leeg binnen de herhaalgrens.
    Exhausted(Kind),
    /// De firmware weigerde TRNG_RND64 met deze code.
    Firmware(i64),
    /// De doelbuffer was leeg; er valt niets te vullen.
    Empty,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoSource => {
                f.write_str("no hardware entropy source (no FEAT_RNG, no SMCCC TRNG)")
            }
            Self::Exhausted(k) => write!(f, "{k}: no entropy within the retry limit"),
            Self::Firmware(c) => write!(f, "smccc-trng: TRNG_RND64 refused with {c}"),
            Self::Empty => f.write_str("empty destination buffer"),
        }
    }
}

/// Het resultaat van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Een vulfunctie zoals de [`crate::drbg`] hem krijgt: [`fill`] voor de kern,
/// [`fill_cpu`] voor een app, of een eigen bron van het board.
pub type Fill = fn(&mut [u8]) -> Result<Kind>;

/// Vult `dst` volledig met hardware-entropie: RNDR, anders SMCCC TRNG.
///
/// Alleen voor de kern: het SMCCC-pad is een firmware-call.
pub fn fill(dst: &mut [u8]) -> Result<Kind> {
    if dst.is_empty() {
        return Err(Error::Empty);
    }
    // Faalt RNDR om welke reden ook, dan de firmware, zoals de Go-kern; is
    // die er niet, dan de reden van RNDR (een lege RNDR zegt meer dan
    // "geen bron").
    let cpu = match fill_cpu(dst) {
        Ok(k) => return Ok(k),
        Err(e) => e,
    };
    if !arch::has_el3() || !smccc_present() {
        return Err(cpu);
    }
    fill_smccc_with(dst, || psci::smc4(TRNG_RND64, TRNG_RND_BITS, 0, 0))?;
    Ok(Kind::SmcccTrng)
}

/// Vult `dst` uitsluitend uit RNDR, zonder firmware-SMC: de bron voor een
/// gekooide app.
pub fn fill_cpu(dst: &mut [u8]) -> Result<Kind> {
    if dst.is_empty() {
        return Err(Error::Empty);
    }
    if !arch::has_rndr() {
        return Err(Error::NoSource);
    }
    fill_rndr_with(dst, arch::rndr)?;
    Ok(Kind::Rndr)
}

/// Welke bron [`fill`] op deze core zou kiezen, zonder entropie te trekken.
#[must_use]
pub fn source() -> Option<Kind> {
    if arch::has_rndr() {
        return Some(Kind::Rndr);
    }
    if arch::has_el3() && smccc_present() {
        return Some(Kind::SmcccTrng);
    }
    None
}

/// De consoleregel over de entropiebron.
///
/// Geen bron is een luide regel met marker, bij elke boot: de DRBG draait
/// dan op timing-jitter, en daar hoort niemand per ongeluk geheimen op te
/// bouwen (de LicheeRV-regel, HOPOS_RNG_INSECURE).
pub fn describe(f: &mut dyn fmt::Write) -> fmt::Result {
    match source() {
        Some(Kind::Rndr) => f.write_str("trng: rndr (FEAT_RNG)"),
        Some(Kind::SmcccTrng) => {
            let (major, minor) = psci::split_version(psci::smc(TRNG_VERSION, 0, 0, 0));
            write!(f, "trng: smccc-trng v{major}.{minor} (DEN 0098)")
        }
        // Nooit uit `source()`: een SoC-blok meldt zijn board zelf.
        Some(Kind::Soc(name)) => write!(f, "trng: {name} (SoC)"),
        None => f.write_str(
            "trng: WARNING no hardware TRNG on this core: the DRBG runs on \
             jitter-seeded entropy, not hardware entropy; avoid high-value secrets on \
             this node HOPOS_RNG_INSECURE",
        ),
    }
}

/// Kent de firmware DEN 0098? TRNG_VERSION geeft een negatieve code als
/// niet. Alleen aanroepen als [`arch::has_el3`] waar is.
fn smccc_present() -> bool {
    (psci::smc(TRNG_VERSION, 0, 0, 0) as i64) >= 0
}

/// Vult `dst` per 8 bytes uit `read` (big-endian, zoals de Go-kern), met
/// [`RNDR_TRIES`] pogingen per woord.
fn fill_rndr_with(dst: &mut [u8], mut read: impl FnMut() -> Option<u64>) -> Result {
    for chunk in dst.chunks_mut(8) {
        let v = (0..RNDR_TRIES)
            .find_map(|_| read())
            .ok_or(Error::Exhausted(Kind::Rndr))?;
        let word = v.to_be_bytes();
        chunk.copy_from_slice(word.get(..chunk.len()).unwrap_or(&word));
    }
    Ok(())
}

/// Vult `dst` per 24 bytes uit `call` (TRNG_RND64: x0 = status, x1:x2:x3 =
/// bits 191..0, big-endian gelegd). NO_ENTROPY wordt begrensd herprobeerd;
/// elke andere fout stopt.
fn fill_smccc_with(dst: &mut [u8], mut call: impl FnMut() -> [u64; 4]) -> Result {
    for chunk in dst.chunks_mut(24) {
        let mut got = None;
        for _ in 0..SMCCC_TRIES {
            let r = call();
            let status = r[0] as i64;
            if status >= 0 {
                got = Some(r);
                break;
            }
            if status != TRNG_NO_ENTROPY {
                return Err(Error::Firmware(status));
            }
        }
        let [_, r1, r2, r3] = got.ok_or(Error::Exhausted(Kind::SmcccTrng))?;
        let mut block = [0u8; 24];
        block[..8].copy_from_slice(&r1.to_be_bytes());
        block[8..16].copy_from_slice(&r2.to_be_bytes());
        block[16..].copy_from_slice(&r3.to_be_bytes());
        chunk.copy_from_slice(block.get(..chunk.len()).unwrap_or(&block));
    }
    Ok(())
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    //! De drie instructies: RNDR en de twee ID-registers.
    use core::arch::asm;

    /// Eén RNDR-lees. RNDR zet NZCV: Z=1 betekent "geen entropie" (en de
    /// waarde is dan 0). Geen `preserves_flags`, want dat is precies wat
    /// hij niet doet.
    pub(super) fn rndr() -> Option<u64> {
        let (v, ok): (u64, u64);
        // SAFETY: RNDR (S3_3_C2_C4_0) lezen heeft geen geheugeneffect; we
        // roepen hem alleen aan als ID_AA64ISAR0_EL1 FEAT_RNG meldt, anders
        // zou hij UNDEF zijn.
        unsafe {
            asm!(
                "mrs {v}, s3_3_c2_c4_0",
                "cset {ok}, ne",
                v = out(reg) v,
                ok = out(reg) ok,
                options(nomem, nostack),
            );
        }
        (ok != 0).then_some(v)
    }

    /// ID_AA64ISAR0_EL1.RNDR ([63:60]) niet nul: FEAT_RNG.
    pub(super) fn has_rndr() -> bool {
        let v: u64;
        // SAFETY: een ID-register lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, id_aa64isar0_el1", out(reg) v, options(nomem, nostack)) };
        (v >> 60) != 0
    }

    /// ID_AA64PFR0_EL1.EL3 ([15:12]) niet nul: er zit een monitor onder ons.
    pub(super) fn has_el3() -> bool {
        let v: u64;
        // SAFETY: een ID-register lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, id_aa64pfr0_el1", out(reg) v, options(nomem, nostack)) };
        (v >> 12) & 0xF != 0
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-stub: geen FEAT_RNG, geen EL3. [`super::fill`] geeft daar
    //! `NoSource`, precies de toestand waarin de DRBG op jitter seedt.
    pub(super) fn rndr() -> Option<u64> {
        None
    }
    pub(super) fn has_rndr() -> bool {
        false
    }
    pub(super) fn has_el3() -> bool {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rndr_words_are_big_endian_and_tails_are_cut() {
        let mut n = 0u64;
        let mut buf = [0u8; 11];
        fill_rndr_with(&mut buf, || {
            n += 1;
            Some(0x0102_0304_0506_0700 | n)
        })
        .unwrap();
        assert_eq!(buf, [1, 2, 3, 4, 5, 6, 7, 1, 1, 2, 3]);
    }

    #[test]
    fn rndr_retries_then_gives_up() {
        let mut calls = 0;
        let mut buf = [0u8; 8];
        let r = fill_rndr_with(&mut buf, || {
            calls += 1;
            (calls == RNDR_TRIES).then_some(7)
        });
        assert_eq!(r, Ok(()));
        assert_eq!(buf[7], 7);
        let r = fill_rndr_with(&mut buf, || None);
        assert_eq!(r, Err(Error::Exhausted(Kind::Rndr)));
    }

    #[test]
    fn smccc_blocks_and_status_codes() {
        // Eerst twee keer NO_ENTROPY, dan een blok: x1:x2:x3 big-endian.
        let mut calls = 0;
        let mut buf = [0u8; 30];
        fill_smccc_with(&mut buf, || {
            calls += 1;
            if calls <= 2 {
                return [TRNG_NO_ENTROPY as u64, 0, 0, 0];
            }
            [0, 0x1111_1111_1111_1111, 0x2222_2222_2222_2222, calls]
        })
        .unwrap();
        assert!(buf[..8].iter().all(|&b| b == 0x11));
        assert!(buf[8..16].iter().all(|&b| b == 0x22));
        assert_eq!(buf[23], 3);
        assert!(buf[24..].iter().all(|&b| b == 0x11));

        // NOT_SUPPORTED stopt meteen.
        let r = fill_smccc_with(&mut buf, || [(-1i64) as u64, 0, 0, 0]);
        assert_eq!(r, Err(Error::Firmware(-1)));
        // Eindeloos NO_ENTROPY is begrensd.
        let r = fill_smccc_with(&mut buf, || [TRNG_NO_ENTROPY as u64, 0, 0, 0]);
        assert_eq!(r, Err(Error::Exhausted(Kind::SmcccTrng)));
    }

    #[test]
    fn host_has_no_source_and_says_so() {
        let mut buf = [0u8; 16];
        assert_eq!(fill(&mut buf), Err(Error::NoSource));
        assert_eq!(fill_cpu(&mut buf), Err(Error::NoSource));
        assert_eq!(fill(&mut []), Err(Error::Empty));
        assert_eq!(source(), None);
        let mut s = String::new();
        describe(&mut s).unwrap();
        assert!(s.contains("HOPOS_RNG_INSECURE"), "{s}");
    }

    #[test]
    fn a_soc_block_is_named_by_its_board() {
        assert_eq!(Kind::Soc("rng200").to_string(), "rng200");
        assert_eq!(
            Error::Exhausted(Kind::Soc("rng200")).to_string(),
            "rng200: no entropy within the retry limit"
        );
    }
}
