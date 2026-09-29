//! De DW-watchdog van de SG2002 (`snps,dw-wdt` op 0x0301_0000, pclk 25 MHz),
//! in de vorm van `board_uefi::watchdog` (`arm`, `pet`, `off`, [`Desc`]),
//! zodat de watchdog-taak van de kern (`hopos/src/watchdog.rs`) hem aait
//! zoals elke andere (Go, `OLD/metal/board/licheerv/wdt.go`).
//!
//! Alleen wapenen als de probe van de boot antwoordde (`probed`): een
//! bus-fout op het WDT-blok overleeft de kern niet, en die gok nam
//! `discover` al één keer, met een regel ervoor. Eenmaal gewapend is de
//! DW-WDT niet meer uit te zetten (CR.enable is write-once tot de reset):
//! `off` weigert dus eerlijk.

use crate::WDT;
use core::sync::atomic::{AtomicBool, AtomicU32, Ordering::Relaxed};

/// Het control-register.
const CR: u64 = 0x00;
/// De timeout-range (TOP in bit 3..0, TOP_INIT in bit 7..4).
const TORR: u64 = 0x04;
/// De restart: het vaste wachtwoord.
const CRR: u64 = 0x0C;
/// De vendor-lijm van de TOP_WDT (Go, verbatim).
const GLUE: u64 = 0x1C;
/// Het DW-restart-wachtwoord.
const KICK: u32 = 0x76;
/// De klok van de teller: pclk, 25 MHz.
const HZ: u64 = 25_000_000;

/// Antwoordde de probe van de boot?
static PROBED: AtomicBool = AtomicBool::new(false);
/// De gewapende TOP (0 = niet gewapend; TOP 0 bestaat wel, maar is 2,6 ms
/// en wordt hier nooit gekozen).
static ARMED: AtomicU32 = AtomicU32::new(0);

/// De timeout van TOP `t`: 2^(16 + t) cycli.
#[must_use]
pub const fn top_ms(t: u32) -> u64 {
    (1u64 << (16 + t)) * 1000 / HZ
}

/// De kleinste TOP (1..=15) waarvan de timeout minstens `timeout_ms` is;
/// 15 (~86 s) als niets volstaat.
#[must_use]
pub fn top_for(timeout_ms: u64) -> u32 {
    (1..=15).find(|&t| top_ms(t) >= timeout_ms).unwrap_or(15)
}

/// Meldt de uitkomst van de probe (`LicheeRv::watchdog_probe`).
pub fn probed(ok: bool) {
    PROBED.store(ok, Relaxed);
}

/// Wapent de watchdog op minstens `timeout_ms` (de volgorde van Go:
/// TORR, de lijm, een restart, dan CR = enable met de vendor-pulslengte en
/// response = directe reset).
pub fn arm(timeout_ms: u64) -> Result<Desc, &'static str> {
    if !PROBED.load(Relaxed) {
        return Err("DW-WDT did not answer the boot probe, not armed");
    }
    let t = top_for(timeout_ms);
    dev::write32(WDT.add(TORR), t | t << 4);
    dev::write32(WDT.add(GLUE), 0x20);
    dev::write32(WDT.add(CRR), KICK);
    dev::write32(WDT.add(CR), 0x11);
    dev::mb();
    ARMED.store(t, Relaxed);
    Ok(Desc {
        timeout_ms: top_ms(t),
        top: t,
    })
}

/// Zet de teller terug op vol.
pub fn pet() {
    if ARMED.load(Relaxed) != 0 {
        dev::write32(WDT.add(CRR), KICK);
    }
}

/// De DW-WDT gaat na het wapenen niet meer uit: `false`.
#[must_use]
pub fn off() -> bool {
    false
}

/// Wat er gewapend is, voor de consoleregel.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Desc {
    /// De timeout zoals hij werkelijk staat.
    pub timeout_ms: u64,
    /// De TOP-waarde.
    pub top: u32,
}

impl core::fmt::Display for Desc {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(
            f,
            "DW-WDT at {:#x}, TOP {} ({} ms), reset on expiry",
            WDT.0, self.top, self.timeout_ms
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn top_covers_the_asked_timeout() {
        assert_eq!(top_ms(15), 85_899); // Go's ~86 s
        assert_eq!(top_for(12_000), 13); // 21,5 s: de eerste boven 12 s
        assert!(top_ms(top_for(12_000)) >= 12_000);
        assert!(top_ms(top_for(12_000) - 1) < 12_000);
        assert_eq!(top_for(1_000_000), 15);
        assert!(arm(12_000).is_err()); // geen probe: niet wapenen
    }
}
