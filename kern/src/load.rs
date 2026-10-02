//! De meetlat per slot (docs/apps.md): de regel `HOPOS_SLOT_LOAD` uit twee
//! standen van de tellers die een app op zijn control-page publiceert,
//! `CTRL_IDLE` (ticks van de architectuurteller die de slaper weg was, alle
//! cores van de app bij elkaar) en `CTRL_WAKES` (aanroepen van de slaper).
//! De kern leest ze; het rekenwerk en de vorm van de regel staan hier, zodat
//! ze op de host getoetst zijn.

use alloc::string::String;
use core::fmt::Write;

/// Eén stand: de twee tellers en de kloktijd (ns) waarop ze gelezen zijn.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Sample {
    /// `CTRL_IDLE`.
    pub idle: u64,
    /// `CTRL_WAKES`.
    pub wakes: u64,
    /// Wanneer, in nanoseconden op de klok van de kern.
    pub at_ns: u64,
}

/// De regel voor slot `slot` uit `prev` en `now`, met `cores` cores en een
/// teller van `freq` ticks per seconde. Idle is gedeeld door het aantal
/// cores: 100% is "elke core de hele tijd weg", en nooit meer. De tellers
/// mogen omlopen. `None` als het interval leeg is of de regel niet past.
#[must_use]
pub fn slot_load(slot: usize, prev: Sample, now: Sample, cores: u64, freq: u64) -> Option<String> {
    let dt_ns = now.at_ns.saturating_sub(prev.at_ns);
    if dt_ns == 0 {
        return None;
    }
    // In u128: freq (tot 1 GHz) maal dt (30 s) past niet in u64.
    let ticks = u128::from(freq) * u128::from(dt_ns) / 1_000_000_000;
    let idle = u128::from(now.idle.wrapping_sub(prev.idle));
    let pct = if ticks == 0 {
        0
    } else {
        (idle * 100 / (ticks * u128::from(cores.max(1)))).min(100)
    };
    let wakes = u128::from(now.wakes.wrapping_sub(prev.wakes)) * 1_000_000_000 / u128::from(dt_ns);
    let mut s = String::new();
    s.try_reserve(72).ok()?;
    write!(
        s,
        "slot {slot}: idle={pct}% wakes={wakes}/s cores={cores} HOPOS_SLOT_LOAD"
    )
    .ok()?;
    Some(s)
}

#[cfg(test)]
mod tests {
    use super::*;

    const F: u64 = 24_000_000; // de teller van de LicheeRV

    #[test]
    fn a_quiet_single_core_app_reads_as_idle_with_few_wakes() {
        let prev = Sample {
            idle: 1_000,
            wakes: 10,
            at_ns: 0,
        };
        // 30 s later: 28,5 s aan idle-ticks en 600 wekken (20/s).
        let now = Sample {
            idle: 1_000 + F * 285 / 10,
            wakes: 610,
            at_ns: 30_000_000_000,
        };
        assert_eq!(
            slot_load(3, prev, now, 1, F).unwrap(),
            "slot 3: idle=95% wakes=20/s cores=1 HOPOS_SLOT_LOAD"
        );
    }

    #[test]
    fn two_cores_split_the_idle_and_wrapped_counters_still_count() {
        let prev = Sample {
            idle: u64::MAX - 5,
            wakes: u64::MAX - 1,
            at_ns: 0,
        };
        let now = Sample {
            idle: F * 10 - 6,
            wakes: 9,
            at_ns: 10_000_000_000,
        };
        assert_eq!(
            slot_load(2, prev, now, 2, F).unwrap(),
            "slot 2: idle=50% wakes=1/s cores=2 HOPOS_SLOT_LOAD"
        );
    }

    #[test]
    fn more_idle_than_time_is_capped_and_zero_cores_counts_as_one() {
        let prev = Sample {
            idle: 0,
            wakes: 0,
            at_ns: 0,
        };
        let now = Sample {
            idle: F * 100,
            wakes: 0,
            at_ns: 1_000_000_000,
        };
        assert_eq!(
            slot_load(1, prev, now, 0, F).unwrap(),
            "slot 1: idle=100% wakes=0/s cores=0 HOPOS_SLOT_LOAD"
        );
    }

    #[test]
    fn an_empty_interval_gives_no_line() {
        let s = Sample {
            idle: 0,
            wakes: 0,
            at_ns: 5,
        };
        assert!(slot_load(1, s, s, 1, F).is_none());
    }
}
