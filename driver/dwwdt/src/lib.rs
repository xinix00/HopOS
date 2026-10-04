//! De DesignWare APB-watchdog (DT `snps,dw-wdt`): de RK3566 van de Radxa
//! (`rockchip,rk3568-wdt`) en de SG2002 van de LicheeRV. Linux:
//! `drivers/watchdog/dw_wdt.c`.
//!
//! Dit bezit de registers en de TOP-tabel: een TOP `n` is 2^(16+n) tikken
//! van de teller, en een kick (het vaste wachtwoord in CRR) laadt de teller
//! opnieuw. Niet van hier: het adres, de klok van de teller, de klokgates
//! en de reset-lijnen (de CRU van de Rockchip) of de reset-routering (het
//! RTC-domein van de SG2002), en het beleid (`kern::watchdog`). Een DW-WDT
//! kent geen uit-knop: eenmaal ENABLE is hij aan tot een reset; wie hem toch
//! stopt, doet dat met de reset-lijnen van zijn SoC.

#![cfg_attr(not(test), no_std)]
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::indexing_slicing))]

use dev::Pa;

/// Control: bit 0 ENABLE, bit 1 response mode (0 = meteen resetten), bit
/// 4..2 de lengte van de reset-puls.
pub const CR: u64 = 0x00;
/// Timeout range: TOP in [3:0], TOP_INIT in [7:4].
pub const TORR: u64 = 0x04;
/// De teller, loopt af.
pub const CCVR: u64 = 0x08;
/// Counter restart: [`KICK`] is aaien.
pub const CRR: u64 = 0x0C;
/// COMP_PARAMS_1: bit 6 = de vaste TOP-tabel is gesynthetiseerd.
pub const PARAMS: u64 = 0xF4;
/// CR: aan.
pub const ENABLE: u32 = 1 << 0;
/// Het vaste DesignWare-restart-wachtwoord.
pub const KICK: u32 = 0x76;
/// COMP_PARAMS_1: USE_FIX_TOP.
pub const USE_FIX_TOP: u32 = 1 << 6;
/// De hoogste TOP.
pub const TOP_MAX: u32 = 15;

/// De timeout van TOP `top` in milliseconden op een teller van `hz` (de
/// vaste tabel: 2^(16+top) tikken).
#[must_use]
pub const fn top_ms(top: u32, hz: u64) -> u64 {
    (1u64 << (16 + top)) * 1000 / hz
}

/// De kleinste TOP vanaf `lo` met een timeout van minstens `timeout_ms` op
/// een teller van `hz`; [`TOP_MAX`] als niets volstaat.
#[must_use]
pub fn top_for(timeout_ms: u64, hz: u64, lo: u32) -> u32 {
    (lo..=TOP_MAX)
        .find(|&t| top_ms(t, hz) >= timeout_ms)
        .unwrap_or(TOP_MAX)
}

/// Eén DW-WDT op zijn registerblok.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct DwWdt {
    base: Pa,
}

impl DwWdt {
    /// Het blok op `base`. Het board zorgt dat het gemapt en geklokt is
    /// vóór een lees of schrijf: een ongeklokt blok kan de bus vasthouden.
    #[must_use]
    pub const fn new(base: Pa) -> Self {
        Self { base }
    }

    /// Het adres van het blok.
    #[must_use]
    pub const fn base(&self) -> Pa {
        self.base
    }

    /// Staat ENABLE aan?
    #[must_use]
    pub fn enabled(&self) -> bool {
        dev::read32(self.base.add(CR)) & ENABLE != 0
    }

    /// De TOP in TORR.
    #[must_use]
    pub fn top(&self) -> u32 {
        dev::read32(self.base.add(TORR)) & 0xF
    }

    /// Zet TOP en TOP_INIT op `top`.
    pub fn set_top(&self, top: u32) {
        dev::write32(self.base.add(TORR), top | (top << 4));
    }

    /// Laadt de teller opnieuw: één schrijf, geen read-modify-write. Op een
    /// uitgeschakelde DW-WDT doet hij niets.
    pub fn kick(&self) {
        dev::write32(self.base.add(CRR), KICK);
    }

    /// De teller nu.
    #[must_use]
    pub fn counter(&self) -> u32 {
        dev::read32(self.base.add(CCVR))
    }

    /// Meldt het silicium de vaste TOP-tabel?
    #[must_use]
    pub fn fixed_top(&self) -> bool {
        dev::read32(self.base.add(PARAMS)) & USE_FIX_TOP != 0
    }

    /// Schrijft CR: `cr` met ENABLE erin wapent hem, onomkeerbaar tot een
    /// reset.
    pub fn enable(&self, cr: u32) {
        dev::write32(self.base.add(CR), cr);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_top_table_doubles_per_step() {
        // De RK3566: xin24m, TOP 15 las 89,48 s terug uit CCVR (Go, 06-08).
        assert_eq!(top_ms(15, 24_000_000), 89_478);
        assert_eq!(top_ms(0, 24_000_000), 2);
        // De SG2002: pclk 25 MHz, Go's ~86 s.
        assert_eq!(top_ms(15, 25_000_000), 85_899);
        // 12 s valt tussen TOP 12 en 13, op beide klokken.
        assert_eq!(top_for(12_000, 24_000_000, 0), 13);
        assert_eq!(top_for(12_000, 25_000_000, 1), 13);
        assert_eq!(top_for(0, 24_000_000, 0), 0);
        assert_eq!(top_for(0, 25_000_000, 1), 1);
        assert_eq!(top_for(u64::MAX, 24_000_000, 0), TOP_MAX);
    }

    #[test]
    fn the_registers_are_where_linux_has_them() {
        let mut w = [0u32; 0x40];
        w[(PARAMS / 4) as usize] = USE_FIX_TOP;
        w[(CCVR / 4) as usize] = 1 << 31;
        let d = DwWdt::new(Pa(w.as_mut_ptr() as usize as u64));
        assert!(!d.enabled() && d.fixed_top());
        d.set_top(15);
        d.kick();
        d.enable(ENABLE);
        assert_eq!(d.counter(), 1 << 31);
        assert!(d.enabled());
        assert_eq!(d.top(), 15);
        assert_eq!(w[(TORR / 4) as usize], 0xFF);
        assert_eq!(w[(CRR / 4) as usize], KICK);
    }
}
