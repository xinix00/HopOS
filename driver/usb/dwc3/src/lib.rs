//! Een Synopsys DesignWare USB3-core (DWC3) in hostmodus, zodat de
//! xHCI-registers eronder betekenen wat ze horen te betekenen. De Rust-vorm
//! van `OLD/metal/gui/driver/usb/dwc3`.
//!
//! WAAROM DIT NODIG IS. Een DWC3-core is niet alleen een xHCI: het is één
//! blok dat óók een device-controller kan zijn. Welke van de twee hij is,
//! staat in GCTL.PRTCAPDIR, en op een OTG-poort (de USB-C van de Radxa) is
//! de defaultstand niet host. De xHCI-registers zijn dan aanwezig maar
//! dood: je leest een nette CAPLENGTH en er gebeurt vervolgens niets. Dat is
//! precies het soort stilte dat je op ijzer een avond kost, dus dit blok zet
//! de modus expliciet (Go, 06-08).
//!
//! Het venster: 0x0000 tot 0x7FFF zijn de xHCI-registers, 0xC100 en verder
//! de globale registers van de core. Eén basisadres, twee
//! registerwerelden.
//!
//! Wat deze crate BEZIT: niets dan de registers van één core, zolang de
//! aanroeper hem vasthoudt; de klokken en de PHY's zet hij niet aan (de
//! CRU- en GRF-bits van de RK3566 zijn niet geverifieerd, en gokken met
//! Rockchip-klokregisters is precies wat de TSADC van juli kostte). Wat
//! U-Boot achterlaat, is het startpunt; leest GSNPSID nul, dan zegt
//! [`Core::host_mode`] "niet geklokt", en dat is dan de volgende meting.
//!
//! REFERENTIE: `drivers/usb/dwc3/core.c` (`dwc3_core_soft_reset`,
//! `dwc3_phy_setup`, `dwc3_core_init`) en de DWC_usb3 Programming Guide.
//! De volgorde hieronder is die van de referentie.
//!
//! De wachten (tweemaal 100 ms reset, 25 ms na de modewissel) slapen op de
//! [`Timer`] van de USB-taak: de rest van de node draait door.

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

use core::fmt;
use core::mem::offset_of;
use dev::{Pa, Reg};
pub use driver_xhci::Timer;

/// De globale registers vanaf GCTL (DWC_usb3 §6.1.1), op
/// [`GLOBALS_OFF`] in het corevenster.
#[repr(C)]
struct Globals {
    /// Global Core Control.
    gctl: Reg<u32>,
    _gpmsts: u32,
    /// Global Status.
    gsts: Reg<u32>,
    _guctl1: u32,
    /// Synopsys ID: familie (31:16) en revisie.
    gsnpsid: Reg<u32>,
}

/// De plek van [`Globals`] in het corevenster.
const GLOBALS_OFF: u64 = 0xC110;

const _: () = {
    assert!(GLOBALS_OFF + offset_of!(Globals, gctl) as u64 == 0xC110);
    assert!(GLOBALS_OFF + offset_of!(Globals, gsts) as u64 == 0xC118);
    assert!(GLOBALS_OFF + offset_of!(Globals, gsnpsid) as u64 == 0xC120);
};

/// GUSB2PHYCFG0 (poort 0).
const USB2_PHY_OFF: u64 = 0xC200;
/// GUSB3PIPECTL0 (poort 0).
const USB3_PIPE_OFF: u64 = 0xC2C0;

// GCTL-bits (DWC_usb3 §6.1.1.5).
const CTL_DIS_CLK_GATING: u32 = 1 << 0;
const CTL_DIS_SCRAMBLE: u32 = 1 << 3;
const CTL_SCALE_DOWN_MASK: u32 = 0x3 << 4;
const CTL_CORE_SOFT_RESET: u32 = 1 << 11;
const CTL_PRT_CAP_MASK: u32 = 0x3 << 12;
const CTL_PRT_CAP_HOST: u32 = 1 << 12;

// GUSB2PHYCFG- en GUSB3PIPECTL-bits.
const U2_PHY_IF_16BIT: u32 = 1 << 3;
const U2_SUS_PHY: u32 = 1 << 6;
const U2_ENBL_SLP_M: u32 = 1 << 8;
const U2_TRD_TIM_MASK: u32 = 0xF << 10;
const U2_TRD_TIM_16BIT: u32 = 5 << 10;
const U2_TRD_TIM_8BIT: u32 = 9 << 10;
const U2_PHY_SOFT_RESET: u32 = 1 << 31;
const U3_SUS_PHY: u32 = 1 << 17;
const U3_PHY_SOFT_RESET: u32 = 1 << 31;

/// De families in GSNPSID (31:16): DWC_usb3, DWC_usb31, DWC_usb32.
const FAMILIES: [u32; 3] = [0x5533, 0x3331, 0x3332];

/// Hoe lang core en PHY's in reset blijven, en hoe lang na het loslaten
/// (`core.c`: de PHY-reset vraagt 100 ms).
const RESET_NS: u64 = 100_000_000;
/// De modewissel heeft even nodig voor de poortlogica hem volgt.
const MODE_SETTLE_NS: u64 = 25_000_000;

/// Waarom de core niet in hostmodus kwam.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    /// GSNPSID noemt geen DWC3-familie: niet geklokt (0) of een dode bus
    /// (alle enen).
    NotDwc3 {
        /// Het venster.
        base: u64,
        /// GSNPSID.
        id: u32,
    },
}

impl Error {
    /// Wat er misging, in het Engels, voor een logregel zonder `fmt`.
    #[must_use]
    pub const fn what(&self) -> &'static str {
        match self {
            Self::NotDwc3 { .. } => {
                "GSNPSID names no DWC3 core (0 = not clocked, 0xFF.. = dead bus)"
            }
        }
    }

    /// Het getal erbij: GSNPSID.
    #[must_use]
    pub const fn value(&self) -> u64 {
        match self {
            Self::NotDwc3 { id, .. } => *id as u64,
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NotDwc3 { base, id } => {
                write!(f, "dwc3: GSNPSID {id:#010x} at {base:#x}: {}", self.what())
            }
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// De globale registers in één keer, voor één diagnoseregel.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Snapshot {
    /// GSNPSID.
    pub id: u32,
    /// GCTL.
    pub ctl: u32,
    /// GUSB2PHYCFG0.
    pub usb2_phy: u32,
    /// GUSB3PIPECTL0.
    pub usb3_pipe: u32,
    /// GSTS.
    pub sts: u32,
}

/// Is `id` een DWC3-familie?
#[must_use]
pub fn is_dwc3(id: u32) -> bool {
    FAMILIES.contains(&(id >> 16))
}

/// De nieuwe GUSB2PHYCFG0: suspend en slaap uit, en de turnaround-tijd die
/// bij de vastgedraaide UTMI-breedte hoort (8-bit = 9, 16-bit = 5,
/// `dwc3_phy_setup`).
#[must_use]
pub fn usb2_phy_cfg(v: u32) -> u32 {
    let v = v & !(U2_SUS_PHY | U2_ENBL_SLP_M | U2_TRD_TIM_MASK);
    if v & U2_PHY_IF_16BIT != 0 {
        v | U2_TRD_TIM_16BIT
    } else {
        v | U2_TRD_TIM_8BIT
    }
}

/// De nieuwe GCTL: geen scaledown (een simulatiestand), scrambling en
/// klokpoorten aan, de poort als HOST.
#[must_use]
pub fn host_ctl(v: u32) -> u32 {
    v & !(CTL_SCALE_DOWN_MASK | CTL_DIS_SCRAMBLE | CTL_DIS_CLK_GATING | CTL_PRT_CAP_MASK)
        | CTL_PRT_CAP_HOST
}

/// Eén DWC3-core.
pub struct Core {
    base: Pa,
}

impl Core {
    /// De core op `base`; raakt nog geen register aan.
    ///
    /// # Safety
    ///
    /// `[base, base + 0xD000)` is het venster van een DWC3-core, Device
    /// gemapt, zolang het programma draait.
    #[must_use]
    pub const unsafe fn new(base: Pa) -> Self {
        Self { base }
    }

    fn globals(&self) -> &'static Globals {
        // SAFETY: de voorwaarde van `new`: de globale registers liggen op
        // +0xC110 in het venster.
        unsafe { dev::regs(self.base.add(GLOBALS_OFF)) }
    }

    fn usb2_phy(&self) -> &'static Reg<u32> {
        // SAFETY: zie `globals`; GUSB2PHYCFG0 ligt op +0xC200.
        unsafe { dev::regs(self.base.add(USB2_PHY_OFF)) }
    }

    fn usb3_pipe(&self) -> &'static Reg<u32> {
        // SAFETY: zie `globals`; GUSB3PIPECTL0 ligt op +0xC2C0.
        unsafe { dev::regs(self.base.add(USB3_PIPE_OFF)) }
    }

    /// GSNPSID: de familie en de revisie; nul of alle enen betekent dat het
    /// venster niets terugpraat.
    #[must_use]
    pub fn id(&self) -> u32 {
        self.globals().gsnpsid.read()
    }

    /// De globale registers voor één diagnoseregel.
    #[must_use]
    pub fn regs(&self) -> Snapshot {
        let g = self.globals();
        Snapshot {
            id: g.gsnpsid.read(),
            ctl: g.gctl.read(),
            usb2_phy: self.usb2_phy().read(),
            usb3_pipe: self.usb3_pipe().read(),
            sts: g.gsts.read(),
        }
    }

    /// De volledige bring-up: soft reset van core en PHY's, dan de core in
    /// hostmodus met de suspend-bits uit.
    ///
    /// De suspend-bits (SUSPHY) zijn geen detail. Staan ze aan, dan mag de
    /// PHY zich slapend leggen zodra er even niets gebeurt, en zonder
    /// interruptpad om hem te wekken (wij pollen) blijft hij dat. Linux zet
    /// ze pas aan als het hele apparaat draait; wij hebben ze niet nodig.
    pub async fn host_mode(&self, t: &impl Timer) -> Result {
        let id = self.id();
        if !is_dwc3(id) {
            return Err(Error::NotDwc3 {
                base: self.base.0,
                id,
            });
        }
        let g = self.globals();
        // De core in reset vóór de PHY's eruit gaan: een PHY-reset onder
        // een lopende core is undefined (`dwc3_core_soft_reset`).
        modify(&g.gctl, 0, CTL_CORE_SOFT_RESET);
        modify(self.usb3_pipe(), 0, U3_PHY_SOFT_RESET);
        modify(self.usb2_phy(), 0, U2_PHY_SOFT_RESET);
        t.sleep(RESET_NS).await;
        modify(self.usb3_pipe(), U3_PHY_SOFT_RESET, 0);
        modify(self.usb2_phy(), U2_PHY_SOFT_RESET, 0);
        t.sleep(RESET_NS).await;
        modify(&g.gctl, CTL_CORE_SOFT_RESET, 0);

        // PHY's wakker en op volle snelheid.
        self.usb2_phy().update(usb2_phy_cfg);
        modify(self.usb3_pipe(), U3_SUS_PHY, 0);
        g.gctl.update(host_ctl);
        dev::mb();
        t.sleep(MODE_SETTLE_NS).await;
        Ok(())
    }
}

/// Wist `clear` en zet `set` in `r`, met de barrière erachter.
fn modify(r: &Reg<u32>, clear: u32, set: u32) {
    r.update(|v| v & !clear | set);
    dev::mb();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_families_of_linux_are_dwc3() {
        assert!(is_dwc3(0x5533_300a));
        assert!(is_dwc3(0x3331_0110));
        assert!(!is_dwc3(0));
        assert!(!is_dwc3(u32::MAX));
    }

    #[test]
    fn host_ctl_clears_the_simulation_bits_and_sets_host() {
        let v = CTL_DIS_CLK_GATING | CTL_DIS_SCRAMBLE | CTL_SCALE_DOWN_MASK | (2 << 12) | 1 << 20;
        assert_eq!(host_ctl(v), CTL_PRT_CAP_HOST | 1 << 20);
    }

    #[test]
    fn usb2_turnaround_follows_the_utmi_width() {
        assert_eq!(usb2_phy_cfg(U2_SUS_PHY | U2_ENBL_SLP_M), U2_TRD_TIM_8BIT);
        assert_eq!(
            usb2_phy_cfg(U2_PHY_IF_16BIT | U2_TRD_TIM_MASK),
            U2_PHY_IF_16BIT | U2_TRD_TIM_16BIT
        );
    }

    #[test]
    fn a_silent_window_is_named() {
        let e = Error::NotDwc3 {
            base: 0xfcc0_0000,
            id: 0,
        };
        assert_eq!(e.value(), 0);
        assert!(e.to_string().contains("not clocked"));
    }
}
