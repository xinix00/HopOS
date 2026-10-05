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
//!    netjes met NOT_SUPPORTED (-1) beantwoordt. De probe is die van Linux
//!    (`psci_init_smccc`, `smccc_probe_trng`): PSCI_FEATURES(SMCCC_VERSION),
//!    SMCCC 1.1 of hoger, dan TRNG_VERSION als 32-bit getal met teken
//!    (minstens 1.0); [`probe`] zegt bij boot wat hij zag.
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
/// SMCCC TRNG_FEATURES (DEN 0098): kent de firmware deze TRNG-functie?
pub const TRNG_FEATURES: u32 = 0x8400_0051;
/// SMCCC TRNG_RND64 (64-bit conventie).
pub const TRNG_RND64: u32 = 0xC400_0053;
/// De SMCCC-foutcode "entropie tijdelijk op": opnieuw proberen.
const TRNG_NO_ENTROPY: i64 = -3;
/// Het maximum per TRNG_RND64-call: 192 bits, 24 bytes in x1:x2:x3.
const TRNG_RND_BITS: u64 = 192;
/// De laagste SMCCC met de TRNG (Linux: `ARM_SMCCC_VERSION_1_1`).
const SMCCC_1_1: i32 = 0x1_0001;
/// De laagste TRNG-versie (Linux: `ARM_SMCCC_TRNG_MIN_VERSION`).
const TRNG_1_0: i32 = 0x1_0000;

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
    if !matches!(probe(), Smccc::Trng { .. }) {
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

/// Zit er een EL3-monitor onder ons (ID_AA64PFR0_EL1.EL3)? Alleen dan
/// bestaat de weg naar de SMCCC TRNG; voor de regel van een jitter-zaad.
#[must_use]
pub fn has_monitor() -> bool {
    arch::has_el3()
}

/// Wat de SMCCC-probe zag, in de volgorde van Linux. De ruwe woorden zijn
/// die van w0: een SMC32-call antwoordt in 32 bits, dus `as i32` is het
/// teken (Linux: `(s32)res.a0`), niet `as i64`.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Smccc {
    /// Geen EL3-monitor (ID_AA64PFR0_EL1.EL3 = 0): geen SMC, want die is
    /// dan UNDEF.
    NoMonitor,
    /// SMCCC onder 1.1: `None` als PSCI_FEATURES SMCCC_VERSION niet kent
    /// (dan is het 1.0), anders het antwoord van SMCCC_VERSION.
    Old(Option<u32>),
    /// SMCCC 1.1 of hoger, maar TRNG_VERSION gaf een fout of iets onder 1.0.
    NoTrng {
        /// SMCCC_VERSION.
        smccc: u32,
        /// Het antwoord van TRNG_VERSION.
        version: u32,
    },
    /// DEN 0098 is er.
    Trng {
        /// SMCCC_VERSION.
        smccc: u32,
        /// TRNG_VERSION.
        version: u32,
        /// TRNG_FEATURES(TRNG_RND64): niet-negatief als RND64 er is.
        rnd64: u32,
    },
}

/// Probeert de SMCCC TRNG van de firmware, zonder entropie te trekken.
#[must_use]
pub fn probe() -> Smccc {
    if !arch::has_el3() {
        return Smccc::NoMonitor;
    }
    probe_with(|func, a1| psci::smc(func, a1, 0, 0) as u32)
}

/// [`probe`] met de firmware als `call` (functie-ID, x1; terug: w0).
fn probe_with(mut call: impl FnMut(u32, u64) -> u32) -> Smccc {
    if (call(psci::PSCI_FEATURES, u64::from(psci::SMCCC_VERSION)) as i32) < 0 {
        return Smccc::Old(None);
    }
    let smccc = call(psci::SMCCC_VERSION, 0);
    if (smccc as i32) < SMCCC_1_1 {
        return Smccc::Old(Some(smccc));
    }
    let version = call(TRNG_VERSION, 0);
    if (version as i32) < TRNG_1_0 {
        return Smccc::NoTrng { smccc, version };
    }
    let rnd64 = call(TRNG_FEATURES, u64::from(TRNG_RND64));
    Smccc::Trng {
        smccc,
        version,
        rnd64,
    }
}

/// Een versiewoord als major.minor, of de foutcode als het negatief is.
struct Version(u32);

impl fmt::Display for Version {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.0 as i32 {
            -1 => f.write_str("-1 (NOT_SUPPORTED)"),
            c if c < 0 => write!(f, "{c}"),
            _ => {
                let (major, minor) = psci::split_version(u64::from(self.0));
                write!(f, "{major}.{minor}")
            }
        }
    }
}

/// De bootregel: wat de probe zag, met marker.
impl fmt::Display for Smccc {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Trng {
                smccc,
                version,
                rnd64,
            } => {
                write!(
                    f,
                    "trng: SMCCC TRNG version {} (SMCCC {}), RND64 ",
                    Version(version),
                    Version(smccc)
                )?;
                if (rnd64 as i32) < 0 {
                    write!(f, "refused by TRNG_FEATURES: {}", Version(rnd64))?;
                } else {
                    f.write_str("available")?;
                }
                f.write_str(" HOPOS_SMCCC_TRNG")
            }
            Self::NoMonitor => f.write_str(
                "trng: no SMCCC TRNG: no EL3 monitor under the kernel (ID_AA64PFR0_EL1.EL3 = 0), no SMC HOPOS_SMCCC_NO_TRNG",
            ),
            Self::Old(None) => f.write_str(
                "trng: no SMCCC TRNG: PSCI_FEATURES does not know SMCCC_VERSION, so SMCCC 1.0 (the TRNG needs 1.1) HOPOS_SMCCC_NO_TRNG",
            ),
            Self::Old(Some(v)) => write!(
                f,
                "trng: no SMCCC TRNG: SMCCC_VERSION {} (the TRNG needs 1.1) HOPOS_SMCCC_NO_TRNG",
                Version(v)
            ),
            Self::NoTrng { smccc, version } => write!(
                f,
                "trng: no SMCCC TRNG: SMCCC {}, TRNG_VERSION {} HOPOS_SMCCC_NO_TRNG",
                Version(smccc),
                Version(version)
            ),
        }
    }
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
        assert_eq!(probe(), Smccc::NoMonitor);
        assert!(probe().to_string().ends_with("HOPOS_SMCCC_NO_TRNG"));
    }

    /// Een nep-firmware: SMCCC 1.2, en TRNG_VERSION/TRNG_FEATURES zoals
    /// gegeven; geeft de probe en de calls (functie-ID, x1).
    fn fake(features: u32, smccc: u32, trng: u32) -> (Smccc, Vec<(u32, u64)>) {
        let mut calls = Vec::new();
        let got = probe_with(|func, a1| {
            calls.push((func, a1));
            match func {
                psci::PSCI_FEATURES => features,
                psci::SMCCC_VERSION => smccc,
                TRNG_VERSION => trng,
                TRNG_FEATURES => 0,
                _ => u32::MAX,
            }
        });
        (got, calls)
    }

    #[test]
    fn probe_follows_linux() {
        // TF-A met DEN 0098: vier calls, in de volgorde van Linux.
        let (p, calls) = fake(0, 0x1_0002, 0x1_0000);
        assert_eq!(
            calls,
            [
                (psci::PSCI_FEATURES, u64::from(psci::SMCCC_VERSION)),
                (psci::SMCCC_VERSION, 0),
                (TRNG_VERSION, 0),
                (TRNG_FEATURES, u64::from(TRNG_RND64)),
            ]
        );
        assert_eq!(
            p.to_string(),
            "trng: SMCCC TRNG version 1.0 (SMCCC 1.2), RND64 available HOPOS_SMCCC_TRNG"
        );

        // Een monitor zonder DEN 0098: NOT_SUPPORTED als 32 bits (de
        // bovenste helft van x0 telt niet).
        let (p, _) = fake(0, 0x1_0002, u32::MAX);
        assert_eq!(
            p,
            Smccc::NoTrng {
                smccc: 0x1_0002,
                version: u32::MAX
            }
        );
        assert_eq!(
            p.to_string(),
            "trng: no SMCCC TRNG: SMCCC 1.2, TRNG_VERSION -1 (NOT_SUPPORTED) HOPOS_SMCCC_NO_TRNG"
        );

        // SMCCC 1.0: geen TRNG-call meer.
        let (p, calls) = fake(u32::MAX, 0, 0);
        assert_eq!((p, calls.len()), (Smccc::Old(None), 1));
        let (p, calls) = fake(0, 0x1_0000, 0);
        assert_eq!((p, calls.len()), (Smccc::Old(Some(0x1_0000)), 2));
        assert!(
            p.to_string()
                .contains("SMCCC_VERSION 1.0 (the TRNG needs 1.1)")
        );
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
