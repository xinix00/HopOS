//! De klokknop van de Radxa: de vier A55's tussen 816 MHz (stil) en
//! 1800 MHz (vol), met hun spanning, als [`driver_dvfs::Knob`] voor het
//! klokbeleid van de kern (`hopos/src/telemetry.rs`).
//!
//! Afgekeken van Linux, en dat is niet de CRU-weg van U-Boot: in mainline
//! rk356x-base.dtsi, en in de DTB die U-Boot ons geeft, hangen de cores aan
//! `<&scmi_clk SCMI_CLK_CPU>`. De klok is van de TF-A, die de APLL zelf
//! verzet; Linux (`drivers/clk/clk-scmi.c`) vraagt alleen een rate, over
//! SCMI met een SMC als bel (`arm,scmi-smc`, functie 0x82000010, shmem op
//! 0x0010_f000). De armclk-code in clk-rk3568.c gebruikt Linux voor deze
//! cores niet. De spanning is wel van het OS (cpufreq-dt), en vdd_cpu is
//! op dit bord niet de RK817 maar een losse buck op i2c0 0x40: `rockchip,
//! rk8600` in mainline rk3566-radxa-zero-3.dtsi, `silergy,syr827` in de DTB
//! van U-Boot, voor Linux allebei `drivers/regulator/fan53555.c` met
//! dezelfde vorm: VSEL0 is de actieve stand (`fcs,suspend-voltage-selector
//! = <1>`), 712,5 mV plus n maal 12,5 mV in bit 5..0. De volgorde is die
//! van cpufreq-dt (`_set_opp`): omhoog eerst de spanning, omlaag eerst de
//! klok. De werkpunten zijn `cpu0_opp_table` van mainline rk3566.dtsi; de
//! DTB van U-Boot noemt lagere spanningen (1800 MHz op 1050 mV), mainline
//! is de ruimste en daarmee de veilige.
//!
//! Veilig betekent hier: de knop kiest nooit een klok boven wat de
//! teruggelezen spanning draagt. Een spanning die niet terug te lezen is,
//! telt als de laagste van oud en nieuw; een klok zonder antwoord als de
//! hoogste. Weigert de buck, dan blijft de klok onder zijn spanning (bij
//! 900 mV 1104 MHz) en zegt de regel dat.
//!
//! Geen temperatuurrem: de TSADC geeft op dit bord geen code (`tsadc`).
//! Linux remt de rk3566 bij 85 C (`cpu_thermal`, passief) en de hardware
//! schakelt bij 95 C uit (`rockchip,hw-tshut-temp`); hier geen van beide.

use crate::i2c::{self, I2c};
use core::fmt;
use dev::Pa;
use driver_dvfs::{Knob, Level};
use driver_scmi::Channel;

/// Het SCMI-shmem van de TF-A (`scmi_shmem: sram@0` onder `sram@10f000`).
const SCMI_SHMEM: Pa = Pa(0x0010_F000);
/// `arm,smc-id`: de bel.
const SCMI_SMC: u32 = 0x8200_0010;
/// `SCMI_CLK_CPU` (rk3568-cru.h): de klok van de vier cores.
const SCMI_CLK_CPU: u32 = 0;

/// vdd_cpu: `regulator@40` op i2c0.
const VDD: u8 = 0x40;
const VSEL0: u8 = 0x00;
const VSEL1: u8 = 0x01;
const ID1: u8 = 0x03;
/// VSEL: de buck staat aan.
const BUCK_EN: u8 = 1 << 7;
/// VSEL: de stand, 64 waarden.
const VSEL_MASK: u8 = 0x3f;
/// `fan53555_voltages_setup_rockchip` (RK8600_CHIP_ID_08) en `_silergy`
/// (SYR82X, SYR83X): beide 712,5 mV in stappen van 12,5.
const MIN_UV: u32 = 712_500;
const STEP_UV: u32 = 12_500;
/// De die-id's met die vorm: 8 (RK8600, SYR82X) en 9 (SYR83X).
const DIE_IDS: [u8; 2] = [8, 9];
/// `regulator-ramp-delay` van vdd_cpu in de DTB van het board (en in
/// mainline): 2,3 mV/µs. Linux wacht daarop (`_regulator_set_voltage_time`),
/// niet op de slew van de chip (die U-Boot op 64 mV/µs liet, gemeten 03-10):
/// de DT kent de condensatoren van het bord.
const RAMP_UV_PER_US: u32 = 2_300;

/// Eén werkpunt: klok en spanning.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Opp {
    /// De klok in MHz.
    pub mhz: u32,
    /// De spanning in µV.
    pub uv: u32,
}

impl Opp {
    const fn new(mhz: u32, uv: u32) -> Self {
        Self { mhz, uv }
    }

    /// De klok in Hz.
    #[must_use]
    pub fn hz(&self) -> u64 {
        u64::from(self.mhz) * 1_000_000
    }
}

/// `cpu0_opp_table` van mainline rk3566.dtsi vanaf de stil-stand
/// (`opp-suspend`), oplopend.
pub const OPPS: [Opp; 5] = [
    Opp::new(816, 850_000),
    Opp::new(1104, 900_000),
    Opp::new(1416, 1_025_000),
    Opp::new(1608, 1_100_000),
    Opp::new(1800, 1_150_000),
];

/// De stil-stand: waar U-Boot de cores liet.
pub const QUIET: Opp = OPPS[0];

/// De stand van VSEL voor `uv`, naar boven afgerond zoals
/// `regulator_map_voltage_linear`.
#[must_use]
pub fn vsel(uv: u32) -> u8 {
    let n = uv.saturating_sub(MIN_UV).div_ceil(STEP_UV);
    n.min(u32::from(VSEL_MASK)) as u8
}

/// De spanning van een VSEL-byte in µV.
#[must_use]
pub fn uv_of(vsel: u8) -> u32 {
    MIN_UV + u32::from(vsel & VSEL_MASK) * STEP_UV
}

/// Het VSEL-woord voor `uv` uit het oude: BUCK_EN en MODE blijven
/// (`regulator_set_voltage_sel_regmap` schrijft alleen het masker).
#[must_use]
pub fn vsel_word(old: u8, uv: u32) -> u8 {
    (old & !VSEL_MASK) | vsel(uv)
}

/// Het hoogste werkpunt dat `uv` draagt; `None` onder de stil-stand.
#[must_use]
pub fn carried(uv: u32) -> Option<Opp> {
    OPPS.iter().rev().find(|o| o.uv <= uv).copied()
}

/// Het plafond: het hoogste werkpunt op of onder `hopos.mhz`.
pub fn ceiling(cap_mhz: Option<u32>) -> Result<Opp, Error> {
    let cap = cap_mhz.unwrap_or(u32::MAX);
    match OPPS.iter().rev().find(|o| o.mhz <= cap) {
        Some(&o) if o != QUIET => Ok(o),
        _ => Err(Error::CapBelowQuiet { cap_mhz: cap }),
    }
}

/// De ramp van `from` naar `to` in µs (Linux `regulator_set_voltage_time`
/// met [`RAMP_UV_PER_US`]).
#[must_use]
pub fn ramp_us(from: u32, to: u32) -> u64 {
    u64::from(from.abs_diff(to).div_ceil(RAMP_UV_PER_US))
}

/// Waarom er geen knop is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    /// `hopos.mhz` laat geen werkpunt boven de stil-stand.
    CapBelowQuiet {
        /// De cap in MHz.
        cap_mhz: u32,
    },
    /// vdd_cpu antwoordt niet over i2c0.
    I2c(i2c::Error),
    /// Op 0x40 zit een buck met een andere die-id.
    Regulator {
        /// ID1.
        id: u8,
    },
    /// VSEL0 is niet de actieve stand (BUCK_EN uit).
    Vsel0Off {
        /// VSEL0.
        vsel0: u8,
    },
    /// De TF-A antwoordt niet op SCMI.
    Scmi(driver_scmi::Error),
    /// De teruggelezen spanning draagt de klok van nu niet.
    Mismatch {
        /// De spanning in µV.
        uv: u32,
        /// De klok in MHz.
        mhz: u64,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::CapBelowQuiet { cap_mhz } => write!(
                f,
                "hopos.mhz={cap_mhz} leaves no operating point above the quiet {} MHz",
                QUIET.mhz
            ),
            Self::I2c(e) => write!(f, "vdd_cpu at i2c0 {VDD:#04x}: {e}"),
            Self::Regulator { id } => write!(
                f,
                "vdd_cpu at i2c0 {VDD:#04x} has id1 {id:#04x}, not an rk8600/syr82x (die 8 or 9)"
            ),
            Self::Vsel0Off { vsel0 } => write!(
                f,
                "vdd_cpu vsel0 {vsel0:#04x} has no BUCK_EN: vsel0 is not the running selector"
            ),
            Self::Scmi(e) => write!(f, "SCMI_CLK_CPU via the TF-A: {e}"),
            Self::Mismatch { uv, mhz } => write!(
                f,
                "vdd_cpu reads {} mV, which does not carry the {mhz} MHz the cores run at",
                uv / 1000
            ),
        }
    }
}

/// De twee kanten van de knop: op ijzer de buck over I2C en de SCMI-klok
/// van de TF-A ([`Hw`]), in de toetsen een nep.
pub trait Rails {
    /// Zet vdd_cpu op `uv`, wacht de ramp, en geeft de teruggelezen stand;
    /// `None` als hij niet terug te lezen is.
    fn set_uv(&mut self, uv: u32) -> Option<u32>;
    /// Zet de klok en geeft wat de firmware daarna meldt; `None` zonder
    /// antwoord.
    fn set_hz(&mut self, hz: u64) -> Option<u64>;
    /// De APLL zoals de CRU hem leest, in MHz (0 = geen), voor de regel.
    fn apll_mhz(&self) -> u64;
}

/// De knop: de bevestigde stand en het plafond.
pub struct RkKnob<R> {
    rails: R,
    uv: u32,
    hz: u64,
    full: Opp,
}

impl<R: Rails> RkKnob<R> {
    /// Een knop die bij `uv` en `hz` begint, met plafond `full`.
    pub fn new(rails: R, uv: u32, hz: u64, full: Opp) -> Self {
        Self {
            rails,
            uv,
            hz,
            full,
        }
    }

    /// De rails, voor de toetsen.
    #[must_use]
    pub fn rails(&self) -> &R {
        &self.rails
    }

    /// Naar `to`, in de volgorde van cpufreq-dt, nooit voorbij de spanning.
    /// Geeft altijd de klok die er nu staat: een weigering van de buck is
    /// een lagere stand, geen mislukking, anders probeert het beleid het
    /// elke 10 ms opnieuw.
    fn go(&mut self, to: Opp) -> Level {
        let (uv0, hz0, apll0) = (self.uv, self.hz, self.rails.apll_mhz());
        if to.uv > self.uv {
            // Niet terug te lezen: de oude, de laagste.
            self.uv = self.rails.set_uv(to.uv).unwrap_or(self.uv);
        }
        let hz = carried(self.uv).map_or(self.hz, |o| o.hz().min(to.hz()));
        if hz != self.hz {
            // Geen antwoord: de hoogste van oud en nieuw.
            self.hz = self.rails.set_hz(hz).unwrap_or(self.hz.max(hz));
        }
        if to.uv < self.uv && carried(to.uv).is_some_and(|o| o.hz() >= self.hz) {
            // Niet terug te lezen: de nieuwe, de laagste.
            self.uv = self.rails.set_uv(to.uv).unwrap_or(to.uv);
        }
        let mhz = self.hz / 1_000_000;
        cpu::println!(
            "dvfs: rk3566 core {} -> {mhz} MHz (apll {apll0} -> {}), vdd_cpu {} -> {} mV{} HOPOS_RK_DVFS",
            hz0 / 1_000_000,
            self.rails.apll_mhz(),
            uv0 / 1000,
            self.uv / 1000,
            if mhz == u64::from(to.mhz) {
                ""
            } else {
                ", short of the asked operating point"
            },
        );
        Level {
            value: mhz as u32,
            unit: "MHz",
        }
    }
}

impl<R: Rails> Knob for RkKnob<R> {
    fn full(&mut self) -> Option<Level> {
        Some(self.go(self.full))
    }

    fn quiet(&mut self) -> Option<Level> {
        Some(self.go(QUIET))
    }
}

impl<R> fmt::Display for RkKnob<R> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "SCMI_CLK_CPU via the TF-A and vdd_cpu at i2c0 {VDD:#04x}, full {} MHz at {} mV, quiet {} MHz at {} mV",
            self.full.mhz,
            self.full.uv / 1000,
            QUIET.mhz,
            QUIET.uv / 1000
        )
    }
}

/// De rails op ijzer.
pub struct Hw {
    i2c: I2c,
    scmi: Channel,
}

impl Hw {
    fn vsel0(&mut self) -> Option<u8> {
        self.i2c
            .read(VDD, VSEL0)
            .inspect_err(|e| cpu::println!("dvfs: rk3566 vdd_cpu read: {e} HOPOS_RK_VDD_FAIL"))
            .ok()
    }
}

impl Rails for Hw {
    fn set_uv(&mut self, uv: u32) -> Option<u32> {
        let old = self.vsel0()?;
        if let Err(e) = self.i2c.write(VDD, VSEL0, vsel_word(old, uv)) {
            cpu::println!("dvfs: rk3566 vdd_cpu write: {e} HOPOS_RK_VDD_FAIL");
        }
        let now = self.vsel0()?;
        crate::soc::delay_us(ramp_us(uv_of(old), uv_of(now)));
        Some(uv_of(now))
    }

    fn set_hz(&mut self, hz: u64) -> Option<u64> {
        if let Err(e) = self.scmi.set_clock_rate(SCMI_CLK_CPU, hz) {
            cpu::println!("dvfs: rk3566 clock to {hz} Hz: {e} HOPOS_RK_CLOCK_FAIL");
        }
        self.scmi
            .clock_rate(SCMI_CLK_CPU)
            .inspect_err(|e| cpu::println!("dvfs: rk3566 clock read: {e} HOPOS_RK_CLOCK_FAIL"))
            .ok()
    }

    fn apll_mhz(&self) -> u64 {
        crate::soc::core_hz().map_or(0, |hz| hz / 1_000_000)
    }
}

/// De bel van het SCMI-kanaal: de SMC zonder argumenten (`smc_send_message`
/// zonder `arm,scmi-smc-param`); de TF-A antwoordt voor hij terugkeert.
fn scmi_ring() {
    let _ = cpu::psci::smc(SCMI_SMC, 0, 0, 0);
}

/// De knop van deze boot: leest vdd_cpu en de klok terug, meldt ze in één
/// regel, en weigert als de spanning de klok van nu niet draagt.
pub(crate) fn knob(cap_mhz: Option<u32>) -> Result<RkKnob<Hw>, Error> {
    let full = ceiling(cap_mhz)?;
    // SAFETY: I2C0 staat als Device in de map (`mmu`), U-Boot liet klok en
    // pinnen aan, en deze knop is de enige gebruiker van de bus.
    let mut i2c = unsafe { I2c::new(i2c::I2C0) };
    let mut rd = |reg| i2c.read(VDD, reg).map_err(Error::I2c);
    let id = rd(ID1)?;
    let (v0, v1) = (rd(VSEL0)?, rd(VSEL1)?);
    if !DIE_IDS.contains(&(id & 0x0f)) {
        return Err(Error::Regulator { id });
    }
    if v0 & BUCK_EN == 0 {
        return Err(Error::Vsel0Off { vsel0: v0 });
    }
    // SAFETY: het shmem ligt in blok 0, dat `mmu` als Device mapt; de TF-A
    // geeft het aan de normal world (Linux mapt hetzelfde adres), en de SMC
    // luidt alleen de bel.
    let mut scmi = unsafe { Channel::with_ring(SCMI_SHMEM, cpu::idle::now, scmi_ring) };
    let hz = scmi.clock_rate(SCMI_CLK_CPU).map_err(Error::Scmi)?;
    let uv = uv_of(v0);
    cpu::println!(
        "dvfs: rk3566 vdd_cpu at i2c0 {VDD:#04x} id1 {id:#04x}: vsel0 {v0:#04x} = {} mV (running), vsel1 {v1:#04x} = {} mV (suspend), i2c clkdiv {:#x}; core {} MHz by SCMI, apll {} MHz by the CRU HOPOS_RK_VDD",
        uv / 1000,
        uv_of(v1) / 1000,
        i2c.clkdiv(),
        hz / 1_000_000,
        crate::soc::core_hz().map_or(0, |h| h / 1_000_000),
    );
    let mhz = hz / 1_000_000;
    if carried(uv).is_none_or(|o| u64::from(o.mhz) < mhz) {
        return Err(Error::Mismatch { uv, mhz });
    }
    Ok(RkKnob::new(Hw { i2c, scmi }, uv, hz, full))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::vec::Vec;

    #[test]
    fn the_operating_points_are_mainline_and_encode_exactly() {
        // rk3566.dtsi: 816/850, 1104/900, 1416/1025, 1608/1100, 1800/1150.
        let codes: Vec<u8> = OPPS.iter().map(|o| vsel(o.uv)).collect();
        assert_eq!(codes, [11, 15, 25, 31, 35]);
        for o in OPPS {
            assert_eq!(uv_of(vsel(o.uv)), o.uv, "{o:?}");
        }
        // Tussenwaarden naar boven, de randen geklemd.
        assert_eq!(uv_of(vsel(860_000)), 862_500);
        assert_eq!(vsel(600_000), 0);
        assert_eq!(uv_of(vsel(2_000_000)), 1_500_000);
        // BUCK_EN en MODE blijven staan.
        assert_eq!(vsel_word(0x8f, 1_150_000), 0xa3);
        assert_eq!(vsel_word(0xcf, 850_000), 0xcb);
    }

    #[test]
    fn a_voltage_carries_the_highest_point_at_or_below_it() {
        let mhz = |uv| carried(uv).map(|o| o.mhz);
        assert_eq!(mhz(850_000), Some(816));
        assert_eq!(mhz(899_999), Some(816));
        assert_eq!(mhz(900_000), Some(1104));
        assert_eq!(mhz(1_150_000), Some(1800));
        assert_eq!(mhz(1_390_000), Some(1800));
        assert_eq!(mhz(800_000), None);
    }

    #[test]
    fn the_cap_picks_a_point_above_quiet() {
        assert_eq!(ceiling(None).unwrap().mhz, 1800);
        assert_eq!(ceiling(Some(1500)).unwrap().mhz, 1416);
        assert_eq!(ceiling(Some(9999)).unwrap().mhz, 1800);
        assert_eq!(
            ceiling(Some(1000)),
            Err(Error::CapBelowQuiet { cap_mhz: 1000 })
        );
        // 850 naar 1150 mV bij 2,3 mV/us: 131 us, beide kanten op.
        assert_eq!(ramp_us(850_000, 1_150_000), 131);
        assert_eq!(ramp_us(1_150_000, 850_000), 131);
    }

    #[derive(Debug, PartialEq, Eq)]
    enum Op {
        Uv(u32),
        Hz(u64),
    }

    /// Een buck en een klok in RAM: `uv_cap` is waar de buck ophoudt
    /// (hoger vragen levert dit), `blind` maakt de stand of de klok
    /// onleesbaar.
    struct Fake {
        uv: u32,
        hz: u64,
        uv_cap: u32,
        uv_blind: bool,
        hz_blind: bool,
        ops: Vec<Op>,
    }

    impl Fake {
        fn at(o: Opp) -> Self {
            Self {
                uv: o.uv,
                hz: o.hz(),
                uv_cap: u32::MAX,
                uv_blind: false,
                hz_blind: false,
                ops: Vec::new(),
            }
        }
    }

    impl Rails for Fake {
        fn set_uv(&mut self, uv: u32) -> Option<u32> {
            self.ops.push(Op::Uv(uv));
            self.uv = uv.min(self.uv_cap);
            (!self.uv_blind).then_some(self.uv)
        }
        fn set_hz(&mut self, hz: u64) -> Option<u64> {
            self.ops.push(Op::Hz(hz));
            self.hz = hz;
            (!self.hz_blind).then_some(hz)
        }
        fn apll_mhz(&self) -> u64 {
            self.hz / 1_000_000
        }
    }

    fn knob(f: Fake, full: Opp) -> RkKnob<Fake> {
        let (uv, hz) = (f.uv, f.hz);
        RkKnob::new(f, uv, hz, full)
    }

    const TOP: Opp = OPPS[4];

    #[test]
    fn up_is_voltage_first_down_is_clock_first() {
        let mut k = knob(Fake::at(QUIET), TOP);
        assert_eq!(k.full().unwrap().value, 1800);
        assert_eq!(k.quiet().unwrap().value, 816);
        assert_eq!(
            k.rails().ops,
            [
                Op::Uv(1_150_000),
                Op::Hz(1_800_000_000),
                Op::Hz(816_000_000),
                Op::Uv(850_000)
            ]
        );
        // Al vol: niets te doen.
        let mut k = knob(Fake::at(TOP), TOP);
        assert_eq!(k.full().unwrap().value, 1800);
        assert!(k.rails().ops.is_empty());
    }

    #[test]
    fn a_buck_that_stops_short_caps_the_clock() {
        // U-Boot's 900 mV en een buck die niet hoger gaat: 1104, niet 1800.
        let mut f = Fake::at(OPPS[1]);
        f.uv_cap = 900_000;
        let mut k = knob(f, TOP);
        assert_eq!(k.full().unwrap().value, 1104);
        // Een buck die 1100 haalt: 1608.
        let mut f = Fake::at(QUIET);
        f.uv_cap = 1_100_000;
        let mut k = knob(f, TOP);
        assert_eq!(k.full().unwrap().value, 1608);
    }

    #[test]
    fn an_unreadable_voltage_counts_as_the_lower() {
        // Omhoog en blind: de oude 850 mV, dus de klok blijft 816.
        let mut f = Fake::at(QUIET);
        f.uv_blind = true;
        let mut k = knob(f, TOP);
        assert_eq!(k.full().unwrap().value, 816);
        assert_eq!(k.rails().ops, [Op::Uv(1_150_000)]);
        // Omlaag en blind: de nieuwe 850 mV, dus de volgende keer omhoog
        // eerst weer de spanning.
        let mut f = Fake::at(TOP);
        f.uv_blind = true;
        let mut k = knob(f, TOP);
        k.quiet();
        k.full();
        assert_eq!(
            k.rails().ops,
            [Op::Hz(816_000_000), Op::Uv(850_000), Op::Uv(1_150_000),]
        );
    }

    #[test]
    fn a_silent_clock_counts_as_the_higher() {
        // De klok antwoordt niet na de vraag om 816: hij kan nog 1800 zijn,
        // dus de spanning blijft 1150.
        let mut f = Fake::at(TOP);
        f.hz_blind = true;
        let mut k = knob(f, TOP);
        assert_eq!(k.quiet().unwrap().value, 1800);
        assert_eq!(k.rails().ops, [Op::Hz(816_000_000)]);
    }

    #[test]
    fn the_i2c_words_follow_rk3x() {
        // Adres met schrijfbit, register, waarde.
        assert_eq!(i2c::write_word(VDD, VSEL0, 0xa3), 0x00a3_0080);
        // De gecombineerde lees: het adres zonder leesbit, beide geldig.
        assert_eq!(
            i2c::read_words(VDD, ID1),
            (0x80 | (1 << 24), 0x03 | (1 << 24))
        );
    }
}
