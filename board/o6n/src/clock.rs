//! De klok van de O6N: per DVFS-domein één desired-perf-woord in een
//! SCMI-fastchannel (het `_CPC`-register, een 32-bit perf-woord op de
//! abstracte `_CPC`-schaal, geen MHz). Het beleid is `driver-dvfs`; dit is
//! de knop ([`CpcKnob`]).
//!
//! Zonder OS-ingreep blijven de cores op de boot-OPP staan (1,8 GHz of
//! lager; FreeBSD: "stuck at 1 GHz"; gemeten: de firmware liet de grote
//! cores op 1,5 GHz). SCMI-perf is OS-gestuurd: de SCP zet wat je vraagt en
//! regelt alleen thermisch zelf terug (85°C passief in de DSDT), dus vol is
//! veilig. Gemeten op het board (L83 p66, 23-09): de knop schaalt exact met
//! de klok (867 tegen 267 Msteps/s = 2600/800 MHz), het beleid zakt na 30 s
//! en klokt onder last binnen ~15 ms op.

use bounded::BoundedVec;
use dev::Pa;
use driver_dvfs::{Knob, Level};
use fw::aml::cpc::Cpc;

/// Hoeveel domeinen: de O6N heeft er vijf (vier A720-paren en de A520's).
pub const MAX_DOMAINS: usize = 8;

/// Eén DVFS-domein.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Domain {
    /// Het desired-perf-register.
    pub reg: Pa,
    /// De grenzen uit de `_CPC`.
    pub lowest: u32,
    /// Het plafond (eventueel geklemd door `hopos.mhz`).
    pub highest: u32,
    /// De stil-stand: LowestNonlinear (onder dat punt spaart zakken geen
    /// energie per instructie meer, alleen tijd; Linux' `cppc_cpufreq` legt
    /// zijn minimum daar ook), anders Lowest.
    pub quiet: u32,
    /// Hoeveel cores erin.
    pub cores: u32,
    /// De eerste `_CPC` van het domein: frequenties.
    pub cpc: Cpc,
}

/// Groepeert de `_CPC`'s op desired-perf-register: één domein per woord.
/// Alleen SystemMemory-registers van 32 bits tellen (de O6N-vorm).
#[must_use]
pub fn domains(cpcs: &[Cpc]) -> BoundedVec<Domain, MAX_DOMAINS> {
    let mut out: BoundedVec<Domain, MAX_DOMAINS> = BoundedVec::new();
    for c in cpcs
        .iter()
        .filter(|c| c.desired_reg != 0 && c.desired_bits == 32)
    {
        if let Some(d) = out
            .as_mut_slice()
            .iter_mut()
            .find(|d| d.reg.0 == c.desired_reg)
        {
            d.cores += 1;
            continue;
        }
        let quiet = if c.lowest_nl > c.lowest && c.lowest_nl <= c.highest {
            c.lowest_nl
        } else {
            c.lowest
        };
        let _ = out.push(Domain {
            reg: Pa(c.desired_reg),
            lowest: c.lowest,
            highest: c.highest,
            quiet,
            cores: 1,
            cpc: *c,
        });
    }
    out
}

/// Klemt het plafond van elk domein op `mhz` (via NominalFrequency; zonder
/// frequenties geldt `mhz` als perf-waarde), nooit onder Lowest, en de
/// stil-stand nooit boven het plafond.
pub fn cap(ds: &mut [Domain], mhz: u32) {
    for d in ds {
        let (perf, _) = d.cpc.perf(mhz);
        if perf < d.highest {
            d.highest = perf.max(d.lowest);
        }
        d.quiet = d.quiet.min(d.highest);
    }
}

/// De knop: per domein één perf-woord. Het beleid is node-breed zoals op de
/// Pi (één drukke core zet alle domeinen vol); per domein schakelen vraagt
/// een idle-teller per fysieke core, en die kent de control-page niet.
pub struct CpcKnob {
    ds: BoundedVec<Domain, MAX_DOMAINS>,
}

impl CpcKnob {
    /// De knop over `ds`.
    ///
    /// # Safety
    ///
    /// Elk `reg` is gemapt als Device en is het desired-perf-fastchannel
    /// van zijn domein, zoals de `_CPC` van deze firmware het noemt.
    #[must_use]
    pub unsafe fn new(ds: BoundedVec<Domain, MAX_DOMAINS>) -> Self {
        Self { ds }
    }

    fn write(&mut self, pick: impl Fn(&Domain) -> u32) -> Level {
        let mut top = 0;
        for d in self.ds.as_slice() {
            let v = pick(d);
            dev::write32(d.reg, v);
            top = top.max(d.cpc.mhz(v));
        }
        dev::mb();
        Level {
            value: top,
            unit: "MHz (fastest domain)",
        }
    }

    /// Wat er nu in de woorden staat: wat we vroegen, niet wat de SCP levert
    /// (thermisch terugregelen ziet hier niemand).
    pub fn asked(&self) -> impl Iterator<Item = u32> + '_ {
        self.ds.as_slice().iter().map(|d| dev::read32(d.reg))
    }

    /// De domeinen.
    #[must_use]
    pub fn domains(&self) -> &[Domain] {
        self.ds.as_slice()
    }
}

/// Voor de regel `HOPOS_CLOCK_UP`: hoeveel domeinen de knop draait.
impl core::fmt::Display for CpcKnob {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(f, "{} _CPC domains", self.ds.len())
    }
}

impl Knob for CpcKnob {
    fn full(&mut self) -> Option<Level> {
        Some(self.write(|d| d.highest))
    }

    fn quiet(&mut self) -> Option<Level> {
        Some(self.write(|d| d.quiet))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cpc(uid: u32, reg: u64, highest: u32) -> Cpc {
        Cpc {
            uid,
            highest,
            nominal: 6554,
            lowest_nl: 2016,
            lowest: 1000,
            desired_reg: reg,
            desired_bits: 32,
            lowest_mhz: 800,
            nominal_mhz: 2080,
        }
    }

    #[test]
    fn cpcs_group_into_domains_by_register() {
        let cs = [
            cpc(0, 0x100, 2232),
            cpc(1, 0x100, 2232),
            cpc(4, 0x104, 8192),
            cpc(5, 0x104, 8192),
            Cpc {
                desired_bits: 64,
                ..cpc(6, 0x108, 8192)
            },
            cpc(7, 0, 8192),
        ];
        let ds = domains(&cs);
        assert_eq!(ds.len(), 2);
        assert_eq!((ds.as_slice()[0].cores, ds.as_slice()[1].cores), (2, 2));
        assert_eq!(ds.as_slice()[1].quiet, 2016, "LowestNonlinear");
    }

    #[test]
    fn the_cap_clamps_ceiling_and_quiet() {
        let mut ds = domains(&[cpc(0, 0x100, 8192)]);
        cap(ds.as_mut_slice(), 1800);
        let d = ds.as_slice()[0];
        assert_eq!(d.highest, 1800 * 6554 / 2080);
        cap(ds.as_mut_slice(), 1);
        let d = ds.as_slice()[0];
        assert_eq!((d.highest, d.quiet), (1000, 1000));
    }

    #[test]
    fn the_knob_writes_one_word_per_domain() {
        let mut regs = [0u32; 4];
        let base = regs.as_mut_ptr() as usize as u64;
        let ds = domains(&[cpc(0, base, 2232), cpc(4, base + 4, 8192)]);
        // SAFETY: de "registers" zijn `regs`, dat de test overleeft.
        let mut k = unsafe { CpcKnob::new(ds) };
        let l = k.full().unwrap();
        assert_eq!(k.asked().collect::<std::vec::Vec<_>>(), [2232, 8192]);
        assert_eq!(l.value, 8192 * 2080 / 6554);
        k.quiet();
        assert_eq!(k.asked().collect::<std::vec::Vec<_>>(), [2016, 2016]);
        let _ = regs;
    }
}
