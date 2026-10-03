//! De temperatuursensor van de SG2002 (TEMPSEN op 0x030E_0000, één kanaal),
//! in milligraden voor de tik en de heartbeat van Hop
//! (`hopos/src/telemetry.rs`).
//!
//! Dit bezit het TEMPSEN-blok en zijn klokgate (`clk_tempsen`, bit 9 van
//! `CLK_EN0`). Eén op één `OLD/metal/board/licheerv/temp.go`, en daarachter
//! de vendor-driver (linux_5.10/drivers/thermal/cv181x_thermal.c): chop
//! 1024T, acc 2048T, een cycle-clock van 0,5 MHz, en de auto-cycle. Zonder
//! die laatste converteert het blok één keer bij de enable en blijft het
//! resultaat daarna staan (Go, 11-08: per boot een geloofwaardige waarde,
//! binnen een boot tien metingen exact hetzelfde getal).

use dev::Pa;

/// De sensor.
pub const TEMPSEN: Pa = Pa(0x030E_0000);

/// Bit 0 enable, bit 7..4 de kanaalkeuze.
const CTL: u64 = 0x004;
/// Bit 5..4 chopsel, 7..6 accsel, 15..8 cyc_clkdiv.
const CFG: u64 = 0x00C;
/// Bit 12..0 het resultaat van kanaal 0.
const RESULT: u64 = 0x020;
/// Bit 23..0 auto_cycle, 31..24 auto_prediv.
const AUTO: u64 = 0x064;

/// De klokgates van de CV181x (dezelfde als in [`crate::ephy`]).
const CLK_EN0: Pa = Pa(0x0300_2000);
/// `clk_tempsen`, van de osc van 25 MHz.
const CLK_TEMPSEN: u32 = 1 << 9;

/// Milligraden uit de rauwe code (Go `TempMilliC`):
/// `code * 1000 * 716 / 2048 - 273000`, in 64 bits (13 bits maal 716000
/// past niet in 32). `None` buiten -40 tot 125 C, het bereik van de tabel
/// van de Radxa: code 0 (geen conversie) gaf in Go -273 C, en een
/// onmogelijke waarde wordt nooit een plausibel getal.
#[must_use]
pub fn code_to_milli(code: u32) -> Option<i32> {
    let m = i64::from(code & 0x1FFF) * 1000 * 716 / 2048 - 273_000;
    i32::try_from(m)
        .ok()
        .filter(|m| (-40_000..=125_000).contains(m))
}

/// De temperatuur in milligraden, of `None` zonder geldige meting.
#[must_use]
pub fn temp_millic() -> Option<i32> {
    code_to_milli(dev::read32(TEMPSEN.add(RESULT)))
}

/// Zet de sensor aan (Go `TempInit`): de klokgate, chopsel 3, accsel 2,
/// cyc_clkdiv 0x31 (25 MHz / 50), de auto-cycle 0x10_0000 (op 2 us per
/// cycle een meting per ~2 s; de prediv ongemoeid), kanaal 0 en enable, en
/// 10 ms voor de eerste meting. Busy-waits, dus alleen bij de boot.
pub fn init() {
    dev::write32(CLK_EN0, dev::read32(CLK_EN0) | CLK_TEMPSEN);
    let rmw = |off: u64, mask: u32, v: u32| {
        let at = TEMPSEN.add(off);
        dev::write32(at, dev::read32(at) & !mask | v);
    };
    rmw(CFG, 0xFFF0, 0x3 << 4 | 0x2 << 6 | 0x31 << 8);
    rmw(AUTO, 0xFF_FFFF, 0x10_0000);
    rmw(CTL, 0xF0, 0x1 << 4 | 0x1);
    crate::wait_us(10_000);
}

/// [`init`] en één regel over wat het blok teruggeeft: een temperatuur, of
/// de registers waarmee de volgende meting verder kan. Daarna stil: de tik
/// zegt `temp=-` zolang er geen geldige code is.
pub fn open() {
    init();
    let raw = dev::read32(TEMPSEN.add(RESULT));
    match code_to_milli(raw) {
        Some(m) => cpu::println!(
            "hwmon: TEMPSEN at {:#x} {}.{}C (raw {}), on the tick and the heartbeat HOPOS_TEMPSEN_UP",
            TEMPSEN.0,
            m / 1000,
            (m % 1000).abs() / 100,
            raw & 0x1FFF
        ),
        None => cpu::println!(
            "hwmon: TEMPSEN at {:#x} gives no valid code (raw {:#x}, ctl {:#x} cfg {:#x} auto {:#x} clk_en0 {:#x}); temperature stays - HOPOS_TEMPSEN_NONE",
            TEMPSEN.0,
            raw,
            dev::read32(TEMPSEN.add(CTL)),
            dev::read32(TEMPSEN.add(CFG)),
            dev::read32(TEMPSEN.add(AUTO)),
            dev::read32(CLK_EN0)
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn go_formula() {
        // code * 716000 / 2048 - 273000, afgekapt zoals in Go.
        assert_eq!(code_to_milli(852), Some(24_867));
        assert_eq!(code_to_milli(900), Some(41_648));
        assert_eq!(code_to_milli(1024), Some(85_000));
        // Alleen de 13 bits van het resultaat tellen.
        assert_eq!(code_to_milli(0xFFFF_E000 | 900), Some(41_648));
    }

    #[test]
    fn impossible_codes_stay_none() {
        // Code 0 was in Go -273 C; alle enen 2590 C.
        assert_eq!(code_to_milli(0), None);
        assert_eq!(code_to_milli(0x1FFF), None);
        assert_eq!(code_to_milli(1200), None);
    }
}
