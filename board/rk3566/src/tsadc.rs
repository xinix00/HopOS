//! De temperatuursensor van de RK3566 (TSADC, `tsadc@fe710000`,
//! `rockchip,rk3568-tsadc`): twee kanalen, 0 = CPU en 1 = GPU, in
//! milligraden voor de tik en de heartbeat van Hop (`hopos/src/telemetry.rs`).
//!
//! Dit bezit het TSADC-blok, zijn klokken en resets in de CRU en de analoge
//! voorkant in de SYS-GRF (`GRF_TSADC_CON`). Het meten zelf is een lezing van
//! twee registers; de conversie loopt in de auto-modus van het blok.
//!
//! De specificatie is `OLD/metal/board/rk3566/tsadc.go`, en daarachter Linux
//! `drivers/thermal/rockchip_thermal.c` (`rk_tsadcv7_initialize`,
//! `rk_tsadcv3_control`, `rk3568_code_table`), clk-rk3568.c (de gates in
//! `CLKGATE_CON(26)` bit 4, 5, 6 en de delers in `CLKSEL_CON(51)`) en
//! rk3568-cru.h (de resets 385, 386 en 471); op 30-09 opnieuw nagelezen op
//! master, en alles klopt nog met Go.
//!
//! EERLIJK VOORAF, want het hoort in de code: in Go is deze sensor op dit bord
//! NOOIT gaan converteren. GEMETEN 06-08: alles wat we schrijven landt
//! (`clksel51 0x1715`, `grf_tsadc_con 0x107`, AUTO_CON `0x00010033`), maar
//! DATA0 en DATA1 lazen nul, en drie hypothesen zijn met een meting
//! weggestreept: de afgeleide delers, de ouderklok (gpll_100m en xin24m), en
//! de volgorde van de resets. Eén onverklaarde observatie: USER_CON leest
//! `0x8fc0` terug terwijl we `0x0fc0` schrijven. Deze module doet daarom wat
//! Linux doet, meldt één keer luid wat het blok teruggeeft, en de tik zegt
//! `temp=-` zolang de code onder de tabel blijft: een onmogelijke nul wordt
//! nooit een plausibel koud getal.
//!
//! De hardware-thermal-shutdown blijft UIT (Go): de DT routeert TSHUT naar
//! een GPIO-pen op 95 C, en een sensor die verkeerd gekalibreerd blijkt zou
//! de node dan onherstelbaar cyclen. Eerst meten, dan een noodrem.

use crate::soc::{CRU, GRF, delay_us, hiword};
use dev::Pa;

/// De sensor (rk356x-base.dtsi: `tsadc@fe710000`, `reg = <... 0x100>`).
pub const TSADC: Pa = Pa(0xFE71_0000);

/// Interleave-timing van de conversie.
const USER_CON: u64 = 0x00;
/// De auto-modus: EN, Q_SEL, de bronnen; een VOLLE schrijf (geen masker).
const AUTO_CON: u64 = 0x04;
/// Interrupts en de TSHUT-routering.
const INT_EN: u64 = 0x08;
/// De data van kanaal 0; kanaal n op `DATA0 + 4n`.
const DATA0: u64 = 0x20;
const INT_DEBOUNCE: u64 = 0x60;
const SHUT_DEBOUNCE: u64 = 0x64;
const AUTO_PERIOD: u64 = 0x68;
const AUTO_PERIOD_HT: u64 = 0x6C;

/// AUTO_CON: de conversie loopt.
const AUTO_EN: u32 = 1 << 0;
/// AUTO_CON: Q_SEL, hoort bij de oplopende codetabel hieronder.
const Q_SEL_EN: u32 = 1 << 1;
/// AUTO_CON: kanaal 0 en 1 als bron.
const SRC_EN: u32 = (1 << 4) | (1 << 5);
/// De datamaat: 12 bits.
const DATA_MASK: u32 = 0xFFF;
/// `TSADCV5_USER_INTER_PD_SOC`: "97us, at least 90us".
const USER_CON_V5: u32 = 0xFC0;
/// `TSADCV5_AUTO_PERIOD_TIME` en `_HT_TIME`: "2.5ms" (bij ~700 kHz 2,3 ms).
const AUTO_PERIOD_V5: u32 = 1622;
/// `TSADCV2_HIGHT_INT_DEBOUNCE_COUNT` en `_TSHUT_`.
const DEBOUNCE: u32 = 4;

/// `RK3568_GRF_TSADC_CON` in de SYS-GRF: TSEN op bit 8, ANA_REG0..2 op 0..2.
const GRF_TSADC_CON: u64 = 0x0600;
const GRF_TSEN: u32 = 8;

/// `CLKGATE_CON(26)`: pclk (4), tsen (5), tsadc (6); actief-laag.
const CLKGATE26: u64 = 0x300 + 26 * 4;
/// `CLKSEL_CON(51)`: tsen-mux [5:4], tsen-deler [2:0], tsadc-deler [14:8].
const CLKSEL51: u64 = 0x100 + 51 * 4;
/// `SOFTRST_CON(24)`: SRST_P_TSADC = 385 (bit 1), SRST_TSADC = 386 (bit 2).
const SOFTRST24: u64 = 0x400 + 24 * 4;
/// `SOFTRST_CON(29)`: SRST_TSADCPHY = 471 (bit 7).
const SOFTRST29: u64 = 0x400 + 29 * 4;

/// De delers voor de rates die de DT vraagt (17 MHz en 700 kHz); de enige
/// AFGELEIDE stap (Go): tsen van gpll_100m (mux 1) gedeeld door 6 = 16,67
/// MHz, tsadc daaruit door 24 = 694 kHz. Delervelden zijn "waarde + 1", en
/// het register is hiword-masked. Go las precies dit terug: `0x1715`.
#[must_use]
pub const fn clksel51_word() -> u32 {
    hiword(1, 0x3, 4) | hiword(5, 0x7, 0) | hiword(23, 0x7F, 8)
}

/// De rk3568-kalibratietabel (`rk3568_code_table`, mode ADC_INCREMENT: de
/// code loopt OP met de temperatuur), code en milligraden, per 5 C. Hoort
/// bij dit silicium; de rk3288-tabel loopt af.
const TABLE: [(u32, i32); 34] = [
    (1584, -40000),
    (1620, -35000),
    (1652, -30000),
    (1688, -25000),
    (1720, -20000),
    (1756, -15000),
    (1788, -10000),
    (1824, -5000),
    (1856, 0),
    (1892, 5000),
    (1924, 10000),
    (1956, 15000),
    (1992, 20000),
    (2024, 25000),
    (2060, 30000),
    (2092, 35000),
    (2128, 40000),
    (2160, 45000),
    (2196, 50000),
    (2228, 55000),
    (2264, 60000),
    (2300, 65000),
    (2332, 70000),
    (2368, 75000),
    (2400, 80000),
    (2436, 85000),
    (2468, 90000),
    (2500, 95000),
    (2536, 100_000),
    (2572, 105_000),
    (2604, 110_000),
    (2636, 115_000),
    (2672, 120_000),
    (2704, 125_000),
];

/// Een rauwe code naar milligraden, lineair tussen de 5 C-stappen. `None`
/// buiten de tabel: een ongeklokt of stil blok leest nul, en dat is "geen
/// meting", geen -40 C.
#[must_use]
pub fn code_to_milli(raw: u32) -> Option<i32> {
    let c = raw & DATA_MASK;
    let (first, last) = (TABLE.first()?, TABLE.last()?);
    if c < first.0 || c > last.0 {
        return None;
    }
    let hi = TABLE.iter().position(|&(code, _)| c <= code)?;
    let Some(lo) = hi.checked_sub(1) else {
        return Some(first.1);
    };
    let ((c0, t0), (c1, t1)) = (*TABLE.get(lo)?, *TABLE.get(hi)?);
    let span = i32::try_from(c1 - c0).ok()?;
    let into = i32::try_from(c - c0).ok()?;
    Some(t0 + (t1 - t0) * into / span)
}

/// De twee rauwe codes.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Raw {
    /// Kanaal 0: de CPU.
    pub cpu: u32,
    /// Kanaal 1: de GPU.
    pub gpu: u32,
}

impl Raw {
    /// De warmste geldige meting in milligraden (die gaat op de heartbeat),
    /// of `None` als geen van beide kanalen in de tabel valt.
    #[must_use]
    pub fn hottest(self) -> Option<i32> {
        match (code_to_milli(self.cpu), code_to_milli(self.gpu)) {
            (Some(a), Some(b)) => Some(a.max(b)),
            (a, b) => a.or(b),
        }
    }
}

/// De rauwe codes van beide kanalen.
#[must_use]
pub fn raw() -> Raw {
    Raw {
        cpu: dev::read32(TSADC.add(DATA0)) & DATA_MASK,
        gpu: dev::read32(TSADC.add(DATA0 + 4)) & DATA_MASK,
    }
}

/// De warmste temperatuur in milligraden, of `None` zonder geldige meting.
#[must_use]
pub fn temp_millic() -> Option<i32> {
    raw().hottest()
}

/// Klokken, delers en resets (Go `TempClockOn`): de reset zet ALLE
/// TSADC-registers terug, dus dit gaat vóór de rest. Digitale kant eerst uit
/// reset, dan de PHY (Go mat de omgekeerde volgorde ook: geen verschil).
fn clock_on() {
    dev::write32(
        CRU.add(CLKGATE26),
        hiword(0, 1, 4) | hiword(0, 1, 5) | hiword(0, 1, 6),
    );
    dev::write32(CRU.add(CLKSEL51), clksel51_word());
    dev::mb();
    dev::write32(CRU.add(SOFTRST24), hiword(1, 1, 1) | hiword(1, 1, 2));
    dev::write32(CRU.add(SOFTRST29), hiword(1, 1, 7));
    dev::mb();
    delay_us(20);
    dev::write32(CRU.add(SOFTRST24), hiword(0, 1, 1) | hiword(0, 1, 2));
    dev::write32(CRU.add(SOFTRST29), hiword(0, 1, 7));
    dev::mb();
}

/// Brengt de sensor op en zet de auto-conversie aan (Go `TempInit`, Linux
/// `rk_tsadcv7_initialize` plus `rk_tsadcv3_control`): de timing, AUTO_CON
/// met een volle schrijf op nul, geen interrupts en geen TSHUT, dan de
/// analoge voorkant in de GRF (TSEN, 15 us, de drie ANA-bits, 100 tot 200
/// us; wij nemen 20 en 200), en pas dan EN, Q_SEL en beide bronnen. Daarna
/// één conversieperiode (~2,3 ms) plus marge. Busy-waits, dus alleen bij de
/// boot (`telemetry::start`).
pub fn init() {
    clock_on();
    let t = |off: u64, v: u32| dev::write32(TSADC.add(off), v);
    t(USER_CON, USER_CON_V5);
    t(AUTO_PERIOD, AUTO_PERIOD_V5);
    t(INT_DEBOUNCE, DEBOUNCE);
    t(AUTO_PERIOD_HT, AUTO_PERIOD_V5);
    t(SHUT_DEBOUNCE, DEBOUNCE);
    t(AUTO_CON, 0);
    t(INT_EN, 0);
    dev::mb();
    dev::write32(GRF.add(GRF_TSADC_CON), hiword(1, 1, GRF_TSEN));
    dev::mb();
    delay_us(20);
    for bit in 0..3 {
        dev::write32(GRF.add(GRF_TSADC_CON), hiword(1, 1, bit));
    }
    dev::mb();
    delay_us(200);
    let v = dev::read32(TSADC.add(AUTO_CON));
    t(AUTO_CON, v | AUTO_EN | Q_SEL_EN | SRC_EN);
    dev::mb();
    delay_us(5000);
}

/// [`init`] en één regel over wat het blok teruggeeft: een temperatuur, of
/// de registers waarmee de volgende meting verder kan (dezelfde vier die Go
/// op 06-08 las).
pub fn open() {
    init();
    let r = raw();
    match (code_to_milli(r.cpu), code_to_milli(r.gpu)) {
        (Some(c), g) => cpu::println!(
            "hwmon: TSADC at {:#x} cpu {}.{}C gpu {} (raw {}/{}), the hottest goes on the tick and the heartbeat HOPOS_TSADC_UP",
            TSADC.0,
            c / 1000,
            (c % 1000).abs() / 100,
            Milli(g),
            r.cpu,
            r.gpu
        ),
        (None, _) => cpu::println!(
            "hwmon: TSADC at {:#x} gives no valid code (raw cpu {} gpu {}, below 1584 = no conversion; user_con {:#x} auto_con {:#x} clksel51 {:#x} grf_tsadc_con {:#x}), as in Go 06-08; temperature stays - HOPOS_TSADC_NONE",
            TSADC.0,
            r.cpu,
            r.gpu,
            dev::read32(TSADC.add(USER_CON)),
            dev::read32(TSADC.add(AUTO_CON)),
            dev::read32(CRU.add(CLKSEL51)),
            dev::read32(GRF.add(GRF_TSADC_CON))
        ),
    }
}

/// Milligraden als `41.5C`, of `-`.
struct Milli(Option<i32>);

impl core::fmt::Display for Milli {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self.0 {
            Some(m) => write!(f, "{}.{}C", m / 1000, (m % 1000).abs() / 100),
            None => f.write_str("-"),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codes_convert_along_the_rk3568_table() {
        assert_eq!(code_to_milli(1584), Some(-40000));
        assert_eq!(code_to_milli(2024), Some(25000));
        assert_eq!(code_to_milli(2704), Some(125_000));
        // Halverwege 2024 en 2060: 27,5 C.
        assert_eq!(code_to_milli(2042), Some(27500));
        // Bovenin de 12 bits genegeerd.
        assert_eq!(code_to_milli(0xF000 | 2024), Some(25000));
    }

    #[test]
    fn a_silent_block_is_no_reading() {
        // Wat Go op 06-08 las: nul op beide kanalen.
        assert_eq!(code_to_milli(0), None);
        assert_eq!(code_to_milli(1583), None);
        assert_eq!(code_to_milli(2705), None);
        assert_eq!(Raw { cpu: 0, gpu: 0 }.hottest(), None);
    }

    #[test]
    fn the_hottest_valid_channel_wins() {
        assert_eq!(
            Raw {
                cpu: 2024,
                gpu: 2060
            }
            .hottest(),
            Some(30000)
        );
        assert_eq!(Raw { cpu: 2024, gpu: 0 }.hottest(), Some(25000));
        assert_eq!(Raw { cpu: 0, gpu: 2060 }.hottest(), Some(30000));
    }

    #[test]
    fn the_divider_word_is_what_go_read_back() {
        // Go 06-08: `clksel51 0x1715` na deze schrijf.
        assert_eq!(clksel51_word() & 0xFFFF, 0x1715);
        assert_eq!(clksel51_word() >> 16, (0x3 << 4) | 0x7 | (0x7F << 8));
    }

    #[test]
    fn the_table_rises() {
        assert!(TABLE.windows(2).all(|w| w[0].0 < w[1].0 && w[0].1 < w[1].1));
    }
}
