//! De thermometer van de O6N: de heetste CPU-sensor van de SCP, via het
//! SCMI-kanaal dat de DSDT zelf voor zijn `_TMP` gebruikt (device PMMX,
//! OperationRegion MBXO op 0x065d0000, doorbell BEEL op +0x80). Wij lopen
//! exact dezelfde bytes.
//!
//! De thermiek is van één eigenaar (de taak die het board de temperatuur
//! vraagt); de Go-`thermMu` verdwijnt daarmee. Eén SCMI-ronde per seconde
//! hoogstens: de heartbeat mag vaker vragen, de mailbox niet vaker draaien.

use bounded::BoundedVec;
use driver_scmi::{CELSIUS, Channel, Sensor};

/// Het SCMI-shmem-kanaal van de DSDT.
pub const SCMI_CHANNEL: u64 = 0x065d_0000;
/// Hoeveel sensoren meetellen.
pub const MAX_SENSORS: usize = 16;
/// Hoe vaak het kanaal hoogstens draait.
const MIN_INTERVAL_NS: u64 = 1_000_000_000;
/// Lezingen boven 200°C zijn glitches (bekend van acpitz, 1200°C).
const GLITCH_MILLI_C: i64 = 200_000;

/// Kiest de sensoren die meetellen: de Celsius-sensoren met "CPU" in de
/// naam; zijn die er niet (een andere firmware, een andere naamgeving), dan
/// álle Celsius-sensoren: liever de heetste van het board dan niets.
#[must_use]
pub fn pick(all: &[Sensor]) -> BoundedVec<Sensor, MAX_SENSORS> {
    let celsius = all.iter().filter(|s| s.kind == CELSIUS);
    let is_cpu = |s: &&Sensor| {
        s.name()
            .as_bytes()
            .windows(3)
            .any(|w| w.eq_ignore_ascii_case(b"CPU"))
    };
    let mut out = BoundedVec::new();
    let any_cpu = celsius.clone().any(|s| is_cpu(&s));
    for s in celsius.filter(|s| !any_cpu || is_cpu(s)) {
        if out.push(*s).is_err() {
            break;
        }
    }
    out
}

/// Het heetste van `readings` (milligraden), glitches eruit; 0 = geen.
#[must_use]
pub fn hottest(readings: impl Iterator<Item = i64>) -> i32 {
    readings
        .filter(|&m| m > 0 && m < GLITCH_MILLI_C)
        .max()
        .map_or(0, |m| m as i32)
}

/// De thermometer: het kanaal, de gekozen sensoren, de laatste lezing.
pub struct Thermo {
    ch: Channel,
    sensors: BoundedVec<Sensor, MAX_SENSORS>,
    last: i32,
    at: Option<u64>,
}

impl Thermo {
    /// Een thermometer over `ch` met `sensors`.
    #[must_use]
    pub fn new(ch: Channel, sensors: BoundedVec<Sensor, MAX_SENSORS>) -> Self {
        Self {
            ch,
            sensors,
            last: 0,
            at: None,
        }
    }

    /// De sensoren.
    #[must_use]
    pub fn sensors(&self) -> &[Sensor] {
        self.sensors.as_slice()
    }

    /// De heetste sensor in milligraden (0 = geen meting); hoogstens één
    /// SCMI-ronde per seconde.
    pub fn milli_c(&mut self, now: u64) -> i32 {
        if let Some(at) = self.at
            && now.saturating_sub(at) < MIN_INTERVAL_NS
        {
            return self.last;
        }
        self.at = Some(now);
        let ch = &mut self.ch;
        self.last = hottest(
            self.sensors
                .as_slice()
                .iter()
                .filter_map(|s| ch.reading(s.id).ok().map(|r| s.milli_c(r))),
        );
        self.last
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sensor(id: u32, kind: u8, name: &str) -> Sensor {
        let mut n = [0u8; 16];
        n[..name.len()].copy_from_slice(name.as_bytes());
        Sensor {
            id,
            kind,
            exponent: 0,
            name: n,
        }
    }

    #[test]
    fn cpu_sensors_first_then_any_celsius() {
        let all = [
            sensor(1, CELSIUS, "cpu_b0"),
            sensor(2, CELSIUS, "GPU"),
            sensor(3, 5, "CPU_VDD"),
            sensor(4, CELSIUS, "CPU_M1"),
        ];
        let p = pick(&all);
        assert_eq!(
            p.as_slice()
                .iter()
                .map(|s| s.id)
                .collect::<std::vec::Vec<_>>(),
            [1, 4]
        );
        let p = pick(&all[1..3]);
        assert_eq!(
            p.as_slice()
                .iter()
                .map(|s| s.id)
                .collect::<std::vec::Vec<_>>(),
            [2]
        );
        assert!(pick(&all[2..3]).is_empty());
    }

    #[test]
    fn the_hottest_ignores_glitches() {
        assert_eq!(hottest([41_000, 1_200_000, 47_500].into_iter()), 47_500);
        assert_eq!(hottest([].into_iter()), 0);
    }
}
