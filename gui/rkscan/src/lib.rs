//! De beeldketen van de RK3566 (Radxa Zero 3E): het power-domein PD_VO
//! (`pd`), de VOP2-scanout (`vop2`) en de DW-HDMI-transmitter (`hdmi`).
//! Eén crate, want de drie lagen zijn als één geheel geschreven en delen de
//! 1080p60-modus, de foutvorm en de hiword-helper (Go: `gui/driver/rkscan`,
//! gemeten werkend 06-08: 1920x1080p60 in DVI-mode op Dereks monitor). De
//! modus staat vast en de EDID wordt niet gelezen: dit is de
//! registervolgorde die Go op 06-08 werkend mat.
//!
//! Dit crate bezit de registers van dit silicium (de adressen veranderen
//! niet per bord: [`RK3566`]) en de volgorde waarin ze geschreven worden.
//! Het bezit NIET de framebuffer: die is board-kennis, ligt in het PA-plan
//! en komt als [`driver_fb::Desc`] binnen. Het bezit ook niet het "één
//! keer": wie [`start`] twee keer roept, verzet de PLL onder een lopende
//! scanout; dat bewaakt het board.
//!
//! WAAROM DIT BESTAAT, want elders is het omgekeerd: op de Pi is beeld een
//! firmware-buffer en bouwen we géén HVS-driver. Dit bord heeft geen
//! firmware-buffer en geen U-Boot-video (GEMETEN 05-08: `Out:
//! serial@fe660000`, geen vidconsole), en de HDMI-uitgang zit er niet voor
//! niets op (Derek, 05-08). Een headless node linkt dit crate niet: het
//! board trekt hem alleen binnen met de feature `gui`.
//!
//! De volgorde van [`start`] is niet vrij: eerst het domein, dan VOP2 (die
//! levert pixels én de dclk), dan pas de HDMI-TX. Andersom staat de frame
//! composer geprogrammeerd tegen een stilstaande klok; DRM houdt dezelfde
//! ordening aan (eerst alle CRTC's, dan de bridges).
//!
//! Er is bewust GEEN stop-pad: niets in HopOS zet het beeld ooit uit. Wie
//! er een bouwt, leest eerst het commentaar bij `Chain::vop_scanout`.

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

mod hdmi;
mod pd;
mod vop2;

#[cfg(test)]
mod tests;

use core::fmt;
use dev::{Pa, Reg};
use driver_fb::Desc;

pub use hdmi::{HdmiIds, HdmiInfo};
pub use pd::PowerInfo;
pub use vop2::VopInfo;

/// De basisadressen van de blokken die de keten aanraakt.
///
/// Op ijzer altijd [`RK3566`]; de tests richten ze op nep-registerblokken
/// in RAM.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Blocks {
    /// De CRU: klokgates en -muxen van VOP2 en HDMI.
    pub cru: Pa,
    /// De PMUCRU: HPLL en de HDMI-referentiemux.
    pub pmucru: Pa,
    /// De PMU: het power-domein PD_VO.
    pub pmu: Pa,
    /// De VOP2 (0x3000 groot) met zijn twee IOMMU's op +0x3E00 en +0x3F00.
    pub vop: Pa,
    /// De DW-HDMI-TX (0x20000 groot, registers op 4-byte-afstand).
    pub hdmi: Pa,
}

/// De blokken van de RK3566, uit rk356x-base.dtsi.
pub const RK3566: Blocks = Blocks {
    // clock-controller@fdd20000. Zelfde blok als board-rk3566 voor het
    // GMAC gebruikt; het adres staat hier nogmaals zodat dit crate geen
    // board hoeft te kennen: het is een siliciumconstante, geen bordkeuze.
    cru: Pa(0xFDD2_0000),
    // clock-controller@fdd00000. HPLL zit HIER en niet in de CRU: één
    // verkeerd basisadres en je verzet een PLL waar DDR of de CPU aan hangt.
    pmucru: Pa(0xFDD0_0000),
    // power-management@fdd90000.
    pmu: Pa(0xFDD9_0000),
    // vop@fe040000; iommu@fe043e00 en @fe043f00 liggen er net achter.
    vop: Pa(0xFE04_0000),
    // hdmi@fe0a0000.
    hdmi: Pa(0xFE0A_0000),
};

/// De breedte van de enige modus die de keten drijft.
pub const WIDTH: u32 = 1920;
/// De hoogte van die modus.
pub const HEIGHT: u32 = 1080;

// De 1080p60-timing, CEA VIC 16 uit drm_edid.c:
//
//     clock 148500 kHz, hdisplay 1920, hsync_start 2008, hsync_end 2052,
//     htotal 2200, vdisplay 1080, vsync_start 1084, vsync_end 1089,
//     vtotal 1125, +HSync +VSync
//
// VOP2 en de frame composer rekenen er elk hun eigen afgeleiden uit (zie
// daar); de ruwe getallen staan hier één keer.
/// De pixelklok in kHz.
const PIXEL_KHZ: u32 = 148_500;
/// Horizontale actieve pixels.
const H_DISPLAY: u32 = WIDTH;
/// Het begin van de hsync.
const H_SYNC_START: u32 = 2008;
/// Het einde van de hsync.
const H_SYNC_END: u32 = 2052;
/// De hele regel.
const H_TOTAL: u32 = 2200;
/// Verticale actieve regels.
const V_DISPLAY: u32 = HEIGHT;
/// Het begin van de vsync.
const V_SYNC_START: u32 = 1084;
/// Het einde van de vsync.
const V_SYNC_END: u32 = 1089;
/// Het hele beeld.
const V_TOTAL: u32 = 1125;
/// De hsync-breedte (44).
const H_SYNC_LEN: u32 = H_SYNC_END - H_SYNC_START;
/// De vsync-breedte (5).
const V_SYNC_LEN: u32 = V_SYNC_END - V_SYNC_START;

const _: () = {
    // 148,5 MHz / (2200 * 1125) = 60 Hz precies.
    assert!(PIXEL_KHZ * 1000 == 60 * H_TOTAL * V_TOTAL);
    assert!(H_SYNC_LEN == 44 && V_SYNC_LEN == 5);
};

/// Bouwt een schrijfactie voor een hiword-masked veld: waarde in de
/// onderste 16 bits, maskerbits 16 posities hoger.
///
/// Elke CRU-, PMU- en GRF-schrijfactie op dit silicium heeft deze vorm:
/// een bit waarvan het maskerbit niet staat, verandert niet. Dat maakt
/// read-modify-write overbodig, en het vergeten van het masker stil: de
/// write gebeurt, en er verandert niets.
#[must_use]
pub const fn hiword(val: u32, mask: u32, shift: u32) -> u32 {
    (val << shift) | (mask << (shift + 16))
}

/// In welke laag de keten brak.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Layer {
    /// Het power-domein PD_VO in de PMU.
    Power,
    /// De VOP2 met zijn klokken (HPLL, CRU) en IOMMU.
    Vop2,
    /// De DW-HDMI-transmitter en zijn PHY.
    Hdmi,
}

impl fmt::Display for Layer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Power => "power domain PD_VO",
            Self::Vop2 => "VOP2",
            Self::Hdmi => "HDMI-TX",
        })
    }
}

/// Een begrensd wachten dat niet uitkwam.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Step {
    /// PD_VO aanzetten: PWR_ST bit 7 naar 0.
    PdPower,
    /// De NIU-idle-request los: BUS_IDLE_ACK bit 4 naar 0.
    PdIdleAck,
    /// De interconnect uit idle: BUS_IDLE_ST bit 4 naar 0.
    PdIdle,
    /// HPLL op 148,5 MHz: CON1 bit 10 (lock).
    HpllLock,
    /// De HDMI-PHY: PHY_STAT0 bit 0 (TX_PHY_LOCK) na twee rondes.
    PhyLock,
}

impl Step {
    /// De laag waar deze stap bij hoort.
    #[must_use]
    pub const fn layer(self) -> Layer {
        match self {
            Self::PdPower | Self::PdIdleAck | Self::PdIdle => Layer::Power,
            Self::HpllLock => Layer::Vop2,
            Self::PhyLock => Layer::Hdmi,
        }
    }

    /// Het blok waar het register in staat, voor de foutregel.
    const fn block(self) -> &'static str {
        match self {
            Self::PdPower | Self::PdIdleAck | Self::PdIdle => "pmu",
            Self::HpllLock => "pmucru",
            Self::PhyLock => "hdmi",
        }
    }

    const fn name(self) -> &'static str {
        match self {
            Self::PdPower => "power",
            Self::PdIdleAck => "idle-ack",
            Self::PdIdle => "idle",
            Self::HpllLock => "HPLL lock",
            Self::PhyLock => "PHY lock",
        }
    }
}

/// Waarom de keten niet opkwam. De [`Display`](fmt::Display) begint met de
/// laag en draagt de rauwe registerinhoud: één boot moet genoeg zijn om te
/// weten wélke stap bleef hangen.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// De framebuffer is niet 1920x1080 bij 32 bpp, of ligt niet volledig
    /// onder 4 GB (het MST-register is 32 bits). Geweigerd vóór er één
    /// register geschreven is.
    Geometry {
        /// Het adres van de buffer.
        base: u64,
        /// De breedte.
        width: u32,
        /// De hoogte.
        height: u32,
        /// Bytes per rij.
        stride: u32,
        /// Bits per pixel.
        bpp: u8,
    },
    /// Een veld kreeg zijn waarde niet binnen de grens.
    Settle {
        /// Welke stap.
        step: Step,
        /// De offset van het register in zijn blok.
        off: u32,
        /// Wat er stond.
        got: u32,
        /// Het veld.
        mask: u32,
        /// Wat er had moeten staan.
        want: u32,
    },
    /// VOP2 antwoordt niet: VERSION_INFO leest nul of alles-één.
    VopDead {
        /// VERSION_INFO (GEMETEN 06-08 op een levend blok: 0x4015_8023).
        version: u32,
    },
    /// De HDMI-TX identificeert zich niet als DW-HDMI-TX (dw_hdmi_probe).
    HdmiId {
        /// PRODUCT_ID0, moet 0xA0 zijn.
        prod0: u8,
        /// PRODUCT_ID1, moet (zonder de HDCP-bits) 0x01 zijn.
        prod1: u8,
    },
    /// Een PHY-register-write via de PHY-I2C-master kreeg geen done.
    PhyI2c {
        /// Het PHY-register.
        reg: u8,
        /// De ronde van de sequentie (1 of 2).
        round: u8,
    },
}

impl Error {
    /// De laag die faalde.
    #[must_use]
    pub const fn layer(&self) -> Layer {
        match self {
            Self::Geometry { .. } | Self::VopDead { .. } => Layer::Vop2,
            Self::Settle { step, .. } => step.layer(),
            Self::HdmiId { .. } | Self::PhyI2c { .. } => Layer::Hdmi,
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let layer = self.layer();
        match *self {
            Self::Geometry {
                base,
                width,
                height,
                stride,
                bpp,
            } => write!(
                f,
                "{layer}: framebuffer {width}x{height} stride {stride} bpp {bpp} at {base:#x} is not {WIDTH}x{HEIGHT}x32 within 32-bit DMA"
            ),
            Self::Settle {
                step,
                off,
                got,
                mask,
                want,
            } => write!(
                f,
                "{layer}: {} did not settle ({}+{off:#x} = {got:#010x}, masked {:#010x}, want {want:#010x})",
                step.name(),
                step.block(),
                got & mask
            ),
            Self::VopDead { version } => {
                write!(f, "{layer}: does not answer (VERSION_INFO {version:#010x})")
            }
            Self::HdmiId { prod0, prod1 } => write!(
                f,
                "{layer}: product id {prod0:#04x}/{prod1:#04x}, want 0xa0/0x01 (iahb clock or PD_VO dead?)"
            ),
            Self::PhyI2c { reg, round } => write!(
                f,
                "{layer}: PHY register {reg:#04x} not acknowledged by the PHY I2C master (round {round})"
            ),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Hoe de keten erbij staat na een geslaagde [`start`].
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Status {
    /// Hangt er een sink aan de kabel (HPD, PHY_STAT0 bit 1)? Geen eis om
    /// beeld te sturen, wel een gratis signaal.
    pub sink: bool,
    /// Nam VP0 de configuratie over binnen drie frames (REG_CFG_DONE bit 0
    /// viel weg)? `false` betekent dat de VP niet scant.
    pub latched: bool,
    /// De identificatie van de HDMI-TX. De bron noemt de RK3568 niet bij
    /// naam, dus printen we ze in plaats van erop te controleren.
    pub ids: HdmiIds,
}

/// De keten op één set blokken, met een klok voor de grenzen.
pub struct Chain {
    b: Blocks,
    clock: fn() -> u64,
}

impl Chain {
    /// Een keten op `blocks`. `clock` geeft monotone nanoseconden.
    ///
    /// # Safety
    ///
    /// Elk adres in `blocks` is de basis van een gemapt blok met de indeling
    /// van dat blok op de RK3566 (in een test: RAM van die maat), dat blijft
    /// bestaan zolang het programma draait, en niemand anders programmeert
    /// deze blokken.
    #[must_use]
    pub const unsafe fn new(blocks: Blocks, clock: fn() -> u64) -> Self {
        Self { b: blocks, clock }
    }

    /// De keten op het silicium van de RK3566.
    #[must_use]
    pub fn rk3566(clock: fn() -> u64) -> Self {
        // SAFETY: RK3566 zijn de blokken uit rk356x-base.dtsi; het board
        // mapt alles vanaf 0xC000_0000 als Device (board-rk3566 `mmu`), en
        // de keten is van het board, dat hem één keer start.
        unsafe { Self::new(RK3566, clock) }
    }

    /// Brengt de hele keten op: power-domein, VOP2-scanout, HDMI-TX.
    ///
    /// Eén keer per boot: een tweede aanroep verzet de PLL onder een
    /// lopende scanout. Faalt een laag, dan zegt de fout welke; de buffer
    /// zelf blijft bruikbaar (over het netwerk), en dat is waarom het board
    /// hier geen reden van maakt om de node tegen te houden.
    pub fn start(&self, fb: &Desc) -> Result<Status> {
        check_geometry(fb)?;
        self.power_on_vo()?;
        self.vop_alive()?;
        self.vop_scanout(fb)?;
        let ids = self.hdmi_enable()?;
        let latched = self.vop_cfg_done_taken();
        Ok(Status {
            sink: self.hdmi_hotplug(),
            latched,
            ids,
        })
    }
}

/// Brengt de beeldketen van de RK3566 op voor `fb` (zie [`Chain::start`]).
pub fn start(fb: Desc, clock: fn() -> u64) -> Result<Status> {
    Chain::rk3566(clock).start(&fb)
}

/// Toetst de framebuffer tegen de vaste laag, vóór er één register
/// geschreven wordt: 1920x1080, 32 bpp, een stride die een rij draagt en
/// een veelvoud van vier is (VIR telt in woorden), en de hele buffer onder
/// 4 GB (MST is 32 bits; de VOP2 heeft hier geen hoge adresbits).
pub fn check_geometry(fb: &Desc) -> Result {
    let bad = Error::Geometry {
        base: fb.base.0,
        width: fb.width,
        height: fb.height,
        stride: fb.stride,
        bpp: fb.bpp,
    };
    let end = u64::from(fb.stride)
        .checked_mul(u64::from(fb.height))
        .and_then(|n| fb.base.0.checked_add(n));
    let fits = end.is_some_and(|e| e <= 1 << 32);
    if fb.width != WIDTH
        || fb.height != HEIGHT
        || fb.bpp != 32
        || u64::from(fb.stride) < u64::from(fb.width) * 4
        || !fb.stride.is_multiple_of(4)
        || fb.base.0 == 0
        || !fits
    {
        return Err(bad);
    }
    Ok(())
}

/// Schrijft een register en noteert de schrijf in het journaal (alleen in
/// de tests; daar bewijst het de volgorde).
fn put(r: &Reg<u32>, v: u32) {
    r.write(v);
    journal::note(core::ptr::from_ref(r).addr() as u64, v);
}

/// Het journaal van de schrijfacties: in de tests een lijst per thread, op
/// ijzer niets.
#[cfg(not(test))]
mod journal {
    #[inline(always)]
    pub(crate) fn note(_pa: u64, _v: u32) {}
}

#[cfg(test)]
mod journal {
    use std::cell::RefCell;
    use std::vec::Vec;

    std::thread_local! {
        static LOG: RefCell<Vec<(u64, u32)>> = const { RefCell::new(Vec::new()) };
    }

    /// Noteert de schrijf en geeft hem aan het model van het silicium.
    pub(crate) fn note(pa: u64, v: u32) {
        LOG.with(|l| l.borrow_mut().push((pa, v)));
        crate::tests::on_write(pa, v);
    }

    /// Haalt het journaal van deze thread op en leegt het.
    pub(crate) fn take() -> Vec<(u64, u32)> {
        LOG.with(|l| core::mem::take(&mut *l.borrow_mut()))
    }
}
