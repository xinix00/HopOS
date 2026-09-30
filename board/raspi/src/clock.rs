//! De klokknop van de Pi's: de ARM-klok via de VideoCore-mailbox, als
//! [`driver_dvfs::Knob`] voor het klokbeleid van de kern
//! (`hopos/src/telemetry.rs`). Go: `OLD/metal/driver/dvfs` (`Start`, de
//! `mailbox`-knop) en `OLD/metal/board/raspi/hop/dvfs.go`.
//!
//! Twee standen, één klok voor alle cores (de firmware kent geen klok per
//! core): vol is het firmware-maximum, begrensd door `hopos.mhz`; stil is
//! [`QUIET_HZ`], nooit onder het firmware-minimum. Een thermische cap hoort
//! in config.txt (`arm_freq`/`arm_freq_max`): de firmware meldt die als
//! maximum en de knop volgt vanzelf (Go 11-07: een fanloze Pi 5 op 2400 MHz
//! liep binnen minuten naar 84 C, op 1800 MHz niet).
//!
//! Dit bezit alleen de twee getallen; de mailbox is van `board_raspi` (de
//! [`crate::MBOX`]-cel, één eigenaar: de executor van core 0).

use core::fmt;
use driver_dvfs::{Knob, Level};

/// De stil-stand (Go `LowHz`: 600 MHz).
pub const QUIET_HZ: u32 = 600_000_000;

/// De twee standen van de knop, in Hz.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Plan {
    /// Vol: het firmware-maximum, of de cap.
    pub full_hz: u32,
    /// Stil: [`QUIET_HZ`], of het firmware-minimum als dat hoger ligt.
    pub quiet_hz: u32,
    /// Wat de firmware als maximum meldt.
    pub max_hz: u32,
    /// Wat de firmware als minimum meldt (0 = onbekend).
    pub min_hz: u32,
}

/// Waarom er geen knop is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// De mailbox is niet open (de boot kwam niet zo ver, of faalde).
    NoMailbox,
    /// De mailbox antwoordde niet op de vraag naar het ARM-maximum.
    NoMaximum,
    /// De firmware laat één ARM-klok over: stil en vol vallen samen.
    OneClock {
        /// Die ene klok in Hz.
        hz: u32,
    },
    /// `hopos.mhz` ligt op of onder de stil-stand.
    CapBelowQuiet {
        /// De cap in MHz.
        cap_mhz: u32,
        /// De stil-stand in Hz.
        quiet_hz: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoMailbox => f.write_str("the VideoCore mailbox is not open"),
            Self::NoMaximum => f.write_str("the VideoCore mailbox does not answer the ARM maximum"),
            Self::OneClock { hz } => write!(
                f,
                "the firmware leaves one ARM clock, {} MHz (arm_freq_min equals arm_freq in config.txt)",
                hz / 1_000_000
            ),
            Self::CapBelowQuiet { cap_mhz, quiet_hz } => write!(
                f,
                "hopos.mhz={cap_mhz} is not above the quiet clock of {} MHz",
                quiet_hz / 1_000_000
            ),
        }
    }
}

/// De standen uit wat de firmware meldt en `hopos.mhz`. De firmware klemt
/// op zijn eigen minimum (Go, gemeten 11-07 op de Pi 5: SetClockRate(600M)
/// werd stilzwijgend de 1500 MHz-vloer van `arm_freq_min`), dus stil gaat
/// daar niet onder. Vallen stil en vol samen, dan valt er niets te draaien.
pub fn plan(max_hz: u32, min_hz: Option<u32>, cap_mhz: Option<u32>) -> Result<Plan, Error> {
    if max_hz == 0 {
        return Err(Error::NoMaximum);
    }
    let min_hz = min_hz.unwrap_or(0);
    let quiet_hz = QUIET_HZ.max(min_hz).min(max_hz);
    if quiet_hz == max_hz {
        return Err(Error::OneClock { hz: max_hz });
    }
    let full_hz = match cap_mhz {
        Some(m) if m.saturating_mul(1_000_000) <= quiet_hz => {
            return Err(Error::CapBelowQuiet {
                cap_mhz: m,
                quiet_hz,
            });
        }
        Some(m) => max_hz.min(m.saturating_mul(1_000_000)),
        None => max_hz,
    };
    Ok(Plan {
        full_hz,
        quiet_hz,
        max_hz,
        min_hz,
    })
}

/// De knop: de standen, en de mailbox via [`crate::MBOX`].
pub struct MboxKnob {
    plan: Plan,
}

impl MboxKnob {
    /// De standen van deze knop.
    #[must_use]
    pub fn plan(&self) -> Plan {
        self.plan
    }

    /// Zet de ARM-klok; `None` als de mailbox er niet is of weigert.
    fn set(&mut self, hz: u32) -> Option<Level> {
        let mut m = crate::MBOX.borrow_mut();
        let got = m
            .as_mut()?
            .set_clock_rate(driver_vcmail::CLOCK_ARM, hz)
            .ok()?;
        Some(Level {
            value: got / 1_000_000,
            unit: "MHz",
        })
    }
}

impl Knob for MboxKnob {
    fn full(&mut self) -> Option<Level> {
        self.set(self.plan.full_hz)
    }
    fn quiet(&mut self) -> Option<Level> {
        self.set(self.plan.quiet_hz)
    }
}

/// De knop van deze boot: de mailbox moet open zijn en een maximum melden.
pub(crate) fn knob(cap_mhz: Option<u32>) -> Result<MboxKnob, Error> {
    let mut m = crate::MBOX.borrow_mut();
    let mb = m.as_mut().ok_or(Error::NoMailbox)?;
    let max = mb
        .max_clock_rate(driver_vcmail::CLOCK_ARM)
        .map_err(|_| Error::NoMaximum)?;
    let min = mb.min_clock_rate(driver_vcmail::CLOCK_ARM).ok();
    Ok(MboxKnob {
        plan: plan(max, min, cap_mhz)?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const M: u32 = 1_000_000;

    #[test]
    fn full_is_the_firmware_maximum_quiet_is_600() {
        let p = plan(2400 * M, Some(1500 * M), None).unwrap();
        // De vloer van arm_freq_min wint van 600 MHz.
        assert_eq!((p.full_hz, p.quiet_hz), (2400 * M, 1500 * M));
        let p = plan(1800 * M, Some(600 * M), None).unwrap();
        assert_eq!((p.full_hz, p.quiet_hz), (1800 * M, 600 * M));
        // Geen minimum van de firmware: 600.
        assert_eq!(plan(1500 * M, None, None).unwrap().quiet_hz, 600 * M);
    }

    #[test]
    fn the_cap_lowers_full_but_never_below_quiet() {
        let p = plan(2400 * M, Some(600 * M), Some(1800)).unwrap();
        assert_eq!(p.full_hz, 1800 * M);
        let p = plan(2400 * M, Some(600 * M), Some(9999)).unwrap();
        assert_eq!(p.full_hz, 2400 * M);
        assert_eq!(
            plan(2400 * M, Some(600 * M), Some(600)),
            Err(Error::CapBelowQuiet {
                cap_mhz: 600,
                quiet_hz: 600 * M
            })
        );
    }

    #[test]
    fn one_clock_is_no_knob() {
        // De Pi 5 met arm_freq=1500 en zonder arm_freq_min: de firmware
        // klemt de vloer op 1500 (Go 11-07), dus één klok.
        let e = plan(1500 * M, Some(1500 * M), None).unwrap_err();
        assert_eq!(e, Error::OneClock { hz: 1500 * M });
        assert!(e.to_string().contains("1500 MHz"), "{e}");
        // Onder 600 MHz: stil wordt het maximum, dus ook geen knop.
        assert!(plan(500 * M, None, None).is_err());
        assert_eq!(plan(0, None, None), Err(Error::NoMaximum));
    }
}
