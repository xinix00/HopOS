//! De VOP2-displaycontroller: leest de framebuffer die het board aanwijst
//! en scant hem uit naar Video Port 0, die op dit bord aan de
//! HDMI-transmitter hangt. Plus de klokken erboven: de CRU-gates, HPLL in
//! de PMUCRU, en de IOMMU die uit moet.
//!
//! REFERENTIE (opgehaald 05-08): Linux v6.13
//! drivers/gpu/drm/rockchip/rockchip_drm_vop2.c (vop2_enable,
//! vop2_crtc_atomic_enable, vop2_setup_layer_mixer,
//! vop2_plane_atomic_update, vop2_post_config, vop2_cfg_done),
//! rockchip_drm_vop2.h (offsets), rockchip_vop2_reg.c (de
//! RK3566-platformdata: window-bases, layer_sel_id's, pre_scan_max_dly),
//! rk356x-base.dtsi + rk3566-radxa-zero-3.dtsi (adres, VP0 naar HDMI,
//! DCLK_VOP0 uit HPLL), clk-rk3568.c en clk-pll.c (klokken en de PLL),
//! drivers/iommu/rockchip-iommu.c, en drm_edid.c voor de timing.
//!
//! WAT DE BRON NIET ZEGT, en dus ook hier niet gedaan wordt:
//!
//! - Er is GEEN AXI/QoS/burst-configuratie. De driver raakt
//!   `SYS_AXI_LUT_CTRL` nooit aan. Een 1080p32-scanout is ~475 MB/s en
//!   leunt dus volledig op reset-defaults van de NoC. Gezien de
//!   drie-poten-freeze op de Pi (scanout leest fb, CPU schrijft fb, verkeer
//!   de kooi in) is dit het gebied waar een meting over zal gaan.
//! - Er is GEEN voorgeschreven reset-sequence: het vop-node heeft geen
//!   `resets`-property en de driver reset nooit.

use crate::{
    Chain, Error, H_DISPLAY, H_SYNC_LEN, H_SYNC_START, H_TOTAL, Result, Step, V_DISPLAY,
    V_SYNC_LEN, V_SYNC_START, V_TOTAL, hiword, put,
};
use core::mem::offset_of;
use dev::{Pa, Reg};
use driver_fb::Desc;

// ---------------------------------------------------------------------------
// De registertabellen.
// ---------------------------------------------------------------------------

/// De CRU-registers van VOP2 en HDMI (clk-rk3568.c: CLKSEL_CON(x) =
/// 0x100 + 4x, CLKGATE_CON(x) = 0x300 + 4x).
#[repr(C)]
pub(crate) struct Cru {
    _r0: [u32; 103],
    /// CLKSEL_CON(39): DCLK_VOP0-mux [11:10] en -deler [7:0].
    clksel39: Reg<u32>,
    _r1: [u32; 108],
    /// CLKGATE_CON(20): de VO- en VOP-gates.
    gate20: Reg<u32>,
    /// CLKGATE_CON(21): de HDMI-gates.
    pub(crate) gate21: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Cru, clksel39) == 0x100 + 39 * 4);
    assert!(offset_of!(Cru, gate20) == 0x300 + 20 * 4);
    assert!(offset_of!(Cru, gate21) == 0x354);
};

/// De PMUCRU-registers van HPLL (PLL_HPLL = PMU_PLL_CON(16),
/// RK3568_PMU_PLL_CON(x) = 4x) en de HDMI-referentiemux.
#[repr(C)]
pub(crate) struct PmuCru {
    _r0: [u32; 16],
    /// HPLL CON0: fbdiv [11:0], postdiv1 [14:12] (hiword-masked).
    hpll_con0: Reg<u32>,
    /// HPLL CON1: refdiv [5:0], postdiv2 [8:6], lock bit 10, dsmpd bit 12
    /// (hiword-masked).
    hpll_con1: Reg<u32>,
    /// HPLL CON2: frac [23:0], NIET hiword-masked.
    hpll_con2: Reg<u32>,
    _r1: [u32; 13],
    /// PMU_MODE_CON0: de HPLL-modus op shift 2, 2 bits.
    mode_con0: Reg<u32>,
    _r2: [u32; 39],
    /// PMU_CLKSEL_CON(8): CLK_HDMI_REF kiest bit 7 tussen hpll en hpll_ph0.
    clksel8: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(PmuCru, hpll_con0) == 16 * 4);
    assert!(offset_of!(PmuCru, hpll_con1) == 0x44);
    assert!(offset_of!(PmuCru, hpll_con2) == 0x48);
    assert!(offset_of!(PmuCru, mode_con0) == 0x80);
    assert!(offset_of!(PmuCru, clksel8) == 0x100 + 8 * 4);
};

/// De globale VOP2-registers (vop+0x000).
#[repr(C)]
pub(crate) struct VopSys {
    /// REG_CFG_DONE: per-VP latch-bit plus GLB_CFG_DONE_EN (bit 15).
    cfg_done: Reg<u32>,
    /// VERSION_INFO.
    version: Reg<u32>,
    /// SYS_AUTO_GATING: bit 31 moet UIT.
    auto_gate: Reg<u32>,
    _r0: [u32; 7],
    /// DSP_IF_EN: welke interface aan, en uit welke VP.
    dsp_if_en: Reg<u32>,
    _r1: u32,
    /// DSP_IF_POL: polariteiten plus CFG_DONE_IMD.
    dsp_if_pol: Reg<u32>,
    _r2: [u32; 7],
    /// OTP_WIN_EN: RK3566-only; de bron geeft de waarde, niet de reden.
    otp_win_en: Reg<u32>,
    _r3: [u32; 7],
    /// VP_LINE_FLAG(0).
    line_flag0: Reg<u32>,
    _r4: [u32; 3],
    /// SYS0_INT_EN.
    int0_en: Reg<u32>,
    /// SYS0_INT_CLR.
    int0_clr: Reg<u32>,
    _r5: [u32; 2],
    /// SYS1_INT_EN.
    int1_en: Reg<u32>,
    /// SYS1_INT_CLR.
    int1_clr: Reg<u32>,
    _r6: [u32; 346],
    /// OVL_CTRL.
    ovl_ctrl: Reg<u32>,
    /// OVL_LAYER_SEL.
    layer_sel: Reg<u32>,
    /// OVL_PORT_SEL.
    port_sel: Reg<u32>,
    _r7: [u32; 45],
    /// HDR0_SRC_COLOR_CTRL.
    hdr0_src_color: Reg<u32>,
    _r8: [u32; 7],
    /// VP_BG_MIX_CTRL(0).
    bg_mix_ctrl0: Reg<u32>,
    _r9: [u32; 3],
    /// CLUSTER_DLY_NUM.
    cluster_dly: Reg<u32>,
    _r10: u32,
    /// SMART_DLY_NUM.
    smart_dly: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(VopSys, cfg_done) == 0x000);
    assert!(offset_of!(VopSys, version) == 0x004);
    assert!(offset_of!(VopSys, auto_gate) == 0x008);
    assert!(offset_of!(VopSys, dsp_if_en) == 0x028);
    assert!(offset_of!(VopSys, dsp_if_pol) == 0x030);
    assert!(offset_of!(VopSys, otp_win_en) == 0x050);
    assert!(offset_of!(VopSys, line_flag0) == 0x070);
    assert!(offset_of!(VopSys, int0_en) == 0x080);
    assert!(offset_of!(VopSys, int0_clr) == 0x084);
    assert!(offset_of!(VopSys, int1_en) == 0x090);
    assert!(offset_of!(VopSys, int1_clr) == 0x094);
    assert!(offset_of!(VopSys, ovl_ctrl) == 0x600);
    assert!(offset_of!(VopSys, layer_sel) == 0x604);
    assert!(offset_of!(VopSys, port_sel) == 0x608);
    assert!(offset_of!(VopSys, hdr0_src_color) == 0x6C0);
    assert!(offset_of!(VopSys, bg_mix_ctrl0) == 0x6E0);
    assert!(offset_of!(VopSys, cluster_dly) == 0x6F0);
    assert!(offset_of!(VopSys, smart_dly) == 0x6F8);
};

/// Video Port 0 (vop+0xC00; VP1 = 0xD00, VP2 = 0xE00).
#[repr(C)]
pub(crate) struct Vp {
    /// DSP_CTRL: STANDBY bit 31, OUT_MODE [3:0]. Altijd volledig schrijven.
    dsp_ctrl: Reg<u32>,
    /// MIPI_CTRL.
    mipi_ctrl: Reg<u32>,
    _r0: [u32; 9],
    /// DSP_BG.
    dsp_bg: Reg<u32>,
    /// PRE_SCAN_HTIMING.
    pre_scan_htiming: Reg<u32>,
    /// POST_DSP_HACT_INFO.
    post_dsp_hact: Reg<u32>,
    /// POST_DSP_VACT_INFO.
    post_dsp_vact: Reg<u32>,
    /// POST_SCL_FACTOR_YRGB.
    post_scl_factor: Reg<u32>,
    /// POST_SCL_CTRL.
    post_scl_ctrl: Reg<u32>,
    _r1: u32,
    /// DSP_HTOTAL_HS_END.
    htotal_hs_end: Reg<u32>,
    /// DSP_HACT_ST_END.
    hact_st_end: Reg<u32>,
    /// DSP_VTOTAL_VS_END.
    vtotal_vs_end: Reg<u32>,
    /// DSP_VACT_ST_END.
    vact_st_end: Reg<u32>,
    _r2: [u32; 20],
    /// VP_INT_STATUS: bit 6 = dsp_hold.
    int_status: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Vp, mipi_ctrl) == 0x04);
    assert!(offset_of!(Vp, dsp_bg) == 0x2C);
    assert!(offset_of!(Vp, pre_scan_htiming) == 0x30);
    assert!(offset_of!(Vp, post_dsp_hact) == 0x34);
    assert!(offset_of!(Vp, post_dsp_vact) == 0x38);
    assert!(offset_of!(Vp, post_scl_factor) == 0x3C);
    assert!(offset_of!(Vp, post_scl_ctrl) == 0x40);
    assert!(offset_of!(Vp, htotal_hs_end) == 0x48);
    assert!(offset_of!(Vp, hact_st_end) == 0x4C);
    assert!(offset_of!(Vp, vtotal_vs_end) == 0x50);
    assert!(offset_of!(Vp, vact_st_end) == 0x54);
    assert!(offset_of!(Vp, int_status) == 0xA8);
};

/// Smart0-win0 (vop+0x1C00, layer_sel_id 3).
///
/// De EENVOUDIGSTE bruikbare laag, en dat is geen smaak maar wat Linux op
/// dit silicium zelf kiest: vop2_create_crtcs slaat op soc_id 3566
/// SMART1/ESMART1/CLUSTER1 over ("these windows don't have an independent
/// framebuffer") en geeft de eerste PRIMARY aan VP0: Smart0-win0. Smart
/// kent alleen RGB (geen YUV, geen AFBC, geen CSC) en geen tweede
/// enable-bit zoals Cluster.
#[repr(C)]
pub(crate) struct Smart {
    /// CTRL0: Y2R/R2Y/CSC, alles 0 voor RGB naar RGB.
    ctrl0: Reg<u32>,
    /// CTRL1: bit 31 = YMIRROR.
    ctrl1: Reg<u32>,
    _r0: [u32; 2],
    /// REGION0_CTRL: bit 0 = WIN0_EN, [5:1] = formaat.
    region0_ctrl: Reg<u32>,
    /// REGION0_YRGB_MST: het framebufferadres, volle 32 bits.
    mst: Reg<u32>,
    _r1: u32,
    /// REGION0_VIR: de stride in WOORDEN, niet in bytes.
    vir: Reg<u32>,
    /// REGION0_ACT_INFO: (h-1) << 16 | (w-1).
    act: Reg<u32>,
    /// REGION0_DSP_INFO: idem.
    dsp: Reg<u32>,
    /// REGION0_DSP_ST.
    dsp_st: Reg<u32>,
    _r2: u32,
    /// REGION0_SCL_CTRL.
    scl: Reg<u32>,
    /// REGION0_SCL_FACTOR_YRGB.
    scl_factor: Reg<u32>,
    _r3: [u32; 38],
    /// COLOR_KEY_CTRL.
    color_key_ctrl: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Smart, ctrl1) == 0x04);
    assert!(offset_of!(Smart, region0_ctrl) == 0x10);
    assert!(offset_of!(Smart, mst) == 0x14);
    assert!(offset_of!(Smart, vir) == 0x1C);
    assert!(offset_of!(Smart, act) == 0x20);
    assert!(offset_of!(Smart, dsp) == 0x24);
    assert!(offset_of!(Smart, dsp_st) == 0x28);
    assert!(offset_of!(Smart, scl) == 0x30);
    assert!(offset_of!(Smart, scl_factor) == 0x34);
    assert!(offset_of!(Smart, color_key_ctrl) == 0xD0);
};

/// Eén VOP-IOMMU-instantie (rockchip-iommu.c).
#[repr(C)]
pub(crate) struct Iommu {
    _dte: u32,
    /// RK_MMU_STATUS: bit 0 = paging aan.
    status: Reg<u32>,
    /// RK_MMU_COMMAND.
    command: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Iommu, status) == 0x04);
    assert!(offset_of!(Iommu, command) == 0x08);
};

/// VP0 in het VOP2-blok.
pub(crate) const VP0: u64 = 0xC00;
/// Smart0-win0 in het VOP2-blok.
pub(crate) const SMART0: u64 = 0x1C00;
/// De twee IOMMU's (iommu@fe043e00, @fe043f00), vanaf de VOP2-basis.
pub(crate) const IOMMU: [u64; 2] = [0x3E00, 0x3F00];

const _: () = {
    assert!(crate::RK3566.vop.0 + IOMMU[0] == 0xFE04_3E00);
    assert!(crate::RK3566.vop.0 + IOMMU[1] == 0xFE04_3F00);
    // Het venster-blok ligt binnen de 0x3000 van de VOP2.
    assert!(SMART0 + 0xD4 <= 0x3000);
};

// ---------------------------------------------------------------------------
// Bitwaarden.
// ---------------------------------------------------------------------------

/// REG_CFG_DONE: GLB_CFG_DONE_EN.
pub(crate) const CFG_DONE_GLB_EN: u32 = 1 << 15;
/// REG_CFG_DONE: het latch-bit van VP0.
pub(crate) const CFG_DONE_VP0: u32 = 1 << 0;
/// SYS_AUTO_GATING: aan.
const AUTO_GATE_EN: u32 = 1 << 31;
/// REGION0_CTRL: WIN0_EN. Dit bit ÍS de mute; er is geen apart mute-bit.
pub(crate) const WIN_EN: u32 = 1 << 0;
/// De bus-error-interrupt, hiword-masked (vop2_isr logt "BUS_ERROR irq err").
const INT_BUS_ERROR: u32 = 0x0002_0002;

/// RK_MMU_STATUS: paging aan.
const IOMMU_PAGING: u32 = 1 << 0;
/// RK_MMU_CMD_ENABLE_STALL.
pub(crate) const IOMMU_STALL: u32 = 2;
/// RK_MMU_CMD_DISABLE_PAGING.
pub(crate) const IOMMU_NO_PAGING: u32 = 1;
/// RK_MMU_CMD_DISABLE_STALL.
pub(crate) const IOMMU_UNSTALL: u32 = 3;

// Klokken en power van de VOP2. De keten is drie niveaus diep en elk niveau
// heeft zijn eigen gate; één dichte gate ergens daarin en het hele blok
// leest nullen of houdt de bus vast.
/// aclk_vo (ouder van hclk_vo).
const GATE_ACLK_VO: u32 = 0;
/// hclk_vo (ouder van hclk_vop).
const GATE_HCLK_VO: u32 = 1;
/// pclk_vo (hoort bij PD_VO).
const GATE_PCLK_VO: u32 = 2;
/// aclk_vop_pre (ouder van aclk_vop).
const GATE_ACLK_VOP_PRE: u32 = 6;
/// aclk_vop.
const GATE_ACLK_VOP: u32 = 8;
/// hclk_vop.
const GATE_HCLK_VOP: u32 = 9;
/// dclk_vop0: de pixelklok van VP0.
const GATE_DCLK_VOP0: u32 = 10;

// HPLL KRIJGT 148,5 MHz RECHTSTREEKS, met DCLK-deler 1. 1188 MHz met deler 8
// geeft rekenkundig hetzelfde (en zo had ik het eerst), maar HPLL voedt óók
// CLK_HDMI_REF, en de HDMI-PHY verwacht die referentie op de pixelklok
// zelf. Met HPLL op 1188 lockt de PHY op de verkeerde frequentie, mogelijk
// mét lock, en dan zegt de monitor "geen signaal" terwijl alles er goed
// uitziet. Gevolg: VP0 en HDMI zitten via deze PLL aan elkaar vast; een
// tweede modus verzet ze samen.
//
// rk3568_pll_rates: RK3036_PLL_RATE(148500000, 1, 99, 4, 4, 1, 0), dus
// 24 MHz * 99 / (1 * 4 * 4) = 148,5 MHz.
/// fbdiv.
const HPLL_FBDIV: u32 = 99;
/// postdiv1.
const HPLL_POSTDIV1: u32 = 4;
/// refdiv.
const HPLL_REFDIV: u32 = 1;
/// postdiv2.
const HPLL_POSTDIV2: u32 = 4;
/// dsmpd: integer-modus.
const HPLL_DSMPD: u32 = 1;
/// De fractionele deler.
const HPLL_FRAC: u32 = 0;
/// HPLL CON1: lock.
pub(crate) const PLL_LOCK: u32 = 1 << 10;
/// De modus-mux van HPLL in PMU_MODE_CON0.
const HPLL_MODE_SHIFT: u32 = 2;
/// xin24m rechtstreeks: verplicht tijdens het herprogrammeren.
const PLL_MODE_SLOW: u32 = 0;
/// De PLL-uitgang.
const PLL_MODE_NORM: u32 = 1;
/// Hoe lang HPLL over locken mag doen (Go: 10 ms).
const PLL_WAIT_NS: u64 = 10_000_000;

const _: () =
    assert!(24_000 * HPLL_FBDIV / (HPLL_REFDIV * HPLL_POSTDIV1 * HPLL_POSTDIV2) == 148_500);

// DCLK_VOP0: mux [11:10] over {hpll, vpll, gpll, cpll}, deler [7:0]. Dit
// bord kiest HPLL (rk3566-radxa-zero-3.dtsi: assigned-clock-parents =
// <&pmucru PLL_HPLL>), en de mux heeft CLK_SET_RATE_NO_REPARENT.
/// De DCLK-mux.
const DCLK_MUX_SHIFT: u32 = 10;
/// HPLL als bron.
const DCLK_MUX_HPLL: u32 = 0;
/// De deler: HPLL staat al op de pixelklok.
const DCLK_DIV_1080P60: u32 = 1;
/// CLK_HDMI_REF: bit 7 op 0 is hpll (op 1 hpll_ph0 = 74,25 MHz).
const HDMI_REF_SHIFT: u32 = 7;
/// hpll.
const HDMI_REF_HPLL: u32 = 0;

// De VOP2-timing, precies zoals vop2_crtc_atomic_enable ze rekent. Let op:
// VOP2 telt hact_st vanaf de hsync (htotal - hsync_start = 192), de frame
// composer van de HDMI-TX vanaf het einde van de actieve regio (88). Twee
// getallen uit dezelfde modus; verwissel ze en er komt niets bruikbaars uit.
/// 192.
const H_ACT_ST: u32 = H_TOTAL - H_SYNC_START;
/// 2112.
const H_ACT_END: u32 = H_ACT_ST + H_DISPLAY;
/// 41.
const V_ACT_ST: u32 = V_TOTAL - V_SYNC_START;
/// 1121.
const V_ACT_END: u32 = V_ACT_ST + V_DISPLAY;
/// pre_scan_max_dly[3] van VP0 (rk3568_vop_video_ports[0]); index 3 =
/// sdr2sdr volgens rockchip_vop2_reg.c.
const BG_DLY: u32 = 42;
/// OUT_MODE: dw_hdmi-rockchip zet ROCKCHIP_OUT_MODE_AAAA, en VP0 heeft
/// VOP2_VP_FEATURE_OUTPUT_10BIT, dus niet terug naar P888.
pub(crate) const OUT_MODE_AAAA: u32 = 15;
/// DSP_IF_POL: HDMI-pinpolariteit in [7:4]. 1080p60 is +HSync +VSync, en
/// vop_pol kent HSYNC_POSITIVE = 0, VSYNC_POSITIVE = 1, dus veld 0x3.
const HDMI_PIN_POL: u32 = 0x3;
/// CFG_DONE_IMD: dit register buiten de cfg_done-latch om.
const CFG_DONE_IMD: u32 = 1 << 28;
/// DSP_IF_EN: HDMI aan; HDMI_MUX [11:10] = vp.id = 0, dus geen bits.
const DSP_IF_EN_HDMI: u32 = 1 << 1;
/// OVL_CTRL: LAYER_SEL_REGDONE_IMD.
const LAYER_SEL_REGDONE_IMD: u32 = 1 << 28;
/// OVL_LAYER_SEL: Smart0 (id 3) onderop, de ongebruikte lagen op 0x5.
/// De driver zegt "configure unused layers to 0x5 (reserved)", en 0 is
/// het id van Cluster0-win0: nullen laten staan routeert een
/// ongeconfigureerde cluster naar deze port.
pub(crate) const LAYER_SEL: u32 = 0x0055_5553;
/// OVL_PORT_SEL: Smart0 [29:28] = VP0; PORT0_MUX [3:0] = nlayers-1 = 5.
/// De stand die Linux op dít board zet (win_size 6, nvps 1).
const PORT_SEL: u32 = 0x0000_0885;
/// SCL: bron == doel, SCALE_NONE met de bilineaire filterstand die de
/// driver dan kiest (beide filters op VOP2_SCALE_DOWN_BIL).
const SCL_NONE_BIL: u32 = 0x44;
/// De 1:1-factor van de post-scaler is 0x1000 en niet 0: vop2_post_config
/// rekent scl_cal_scale2(a, a) = ((a-1) << 12) / (a-1).
const POST_SCL_1_1: u32 = 0x1000_1000;
/// Hoe lang het latchen mag duren: drie frames van ~16,7 ms.
const LATCH_WAIT_NS: u64 = 50_000_000;

/// De registers die een mislukte bring-up ontleden.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct VopInfo {
    /// CLKGATE_CON(20).
    pub gate20: u32,
    /// CLKSEL_CON(39).
    pub sel39: u32,
    /// VP0 DSP_CTRL.
    pub dsp_ctrl: u32,
    /// REG_CFG_DONE.
    pub cfg_done: u32,
    /// DSP_IF_EN.
    pub if_en: u32,
    /// Smart0 REGION0_CTRL.
    pub win_ctrl: u32,
    /// Smart0 REGION0_YRGB_MST.
    pub win_mst: u32,
    /// VP0 VP_INT_STATUS.
    pub int_status: u32,
}

impl Chain {
    pub(crate) fn cru(&self) -> &'static Cru {
        // SAFETY: de voorwaarde van `Chain::new`: `b.cru` is de CRU.
        unsafe { dev::regs(self.b.cru) }
    }

    fn pmucru(&self) -> &'static PmuCru {
        // SAFETY: de voorwaarde van `Chain::new`: `b.pmucru` is de PMUCRU.
        unsafe { dev::regs(self.b.pmucru) }
    }

    fn vop_sys(&self) -> &'static VopSys {
        // SAFETY: de voorwaarde van `Chain::new`: `b.vop` is de VOP2.
        unsafe { dev::regs(self.b.vop) }
    }

    fn vp0(&self) -> &'static Vp {
        // SAFETY: VP0 ligt op +0xC00 in de VOP2 (`Chain::new`).
        unsafe { dev::regs(self.b.vop.add(VP0)) }
    }

    fn smart0(&self) -> &'static Smart {
        // SAFETY: Smart0-win0 ligt op +0x1C00 in de VOP2 (`Chain::new`).
        unsafe { dev::regs(self.b.vop.add(SMART0)) }
    }

    fn iommu(&self, off: u64) -> &'static Iommu {
        let pa: Pa = self.b.vop.add(off);
        // SAFETY: de IOMMU's liggen op +0x3E00 en +0x3F00 van de VOP2
        // (`Chain::new`; `off` komt uit `IOMMU`).
        unsafe { dev::regs(pa) }
    }

    /// Opent de hele klokketen van de VOP2 (één hiword-write op
    /// CLKGATE_CON(20), 1 = dicht, dus waarde 0 met het maskerbit).
    /// Los van [`Chain::power_on_vo`] omdat het domein deze klokken nodig
    /// heeft tijdens het schakelen.
    ///
    /// De muxen en delers erboven (CLKSEL_CON 37 en 38) blijven op de stand
    /// van U-Boot, zoals in Go's keten die 06-08 beeld gaf. Leest VOP2 ooit
    /// dood, dan zijn die de eerste verdachte.
    pub fn vop_clock_on(&self) {
        let g = [
            GATE_ACLK_VO,
            GATE_HCLK_VO,
            GATE_PCLK_VO,
            GATE_ACLK_VOP_PRE,
            GATE_ACLK_VOP,
            GATE_HCLK_VOP,
            GATE_DCLK_VOP0,
        ]
        .iter()
        .fold(0, |g, &b| g | hiword(0, 1, b));
        put(&self.cru().gate20, g);
        dev::mb();
    }

    /// Zet HPLL op 148,5 MHz, DCLK_VOP0 daar rechtstreeks op (deler 1) en
    /// de HDMI-PHY-referentie op dezelfde PLL.
    ///
    /// De volgorde is die van rockchip_rk3036_pll_set_params en is niet
    /// vrij: een PLL mag niet worden herprogrammeerd terwijl er iets uit
    /// hangt, dus eerst de modus naar SLOW (xin24m), dan de coëfficiënten,
    /// dan wachten op LOCK, en dan pas terug naar NORM.
    pub fn vop_pixel_clock(&self) -> Result {
        let p = self.pmucru();
        put(&p.mode_con0, hiword(PLL_MODE_SLOW, 0x3, HPLL_MODE_SHIFT));
        dev::mb();
        // CON0 en CON1 zijn hiword-masked; CON2 (frac) NIET: de driver zegt
        // het expliciet ("GPLL CON2 is not HIWORD_MASK") en doet daar een
        // read-modify-write.
        put(
            &p.hpll_con0,
            hiword(HPLL_FBDIV, 0xFFF, 0) | hiword(HPLL_POSTDIV1, 0x7, 12),
        );
        put(
            &p.hpll_con1,
            hiword(HPLL_REFDIV, 0x3F, 0)
                | hiword(HPLL_POSTDIV2, 0x7, 6)
                | hiword(HPLL_DSMPD, 0x1, 12),
        );
        put(
            &p.hpll_con2,
            (p.hpll_con2.read() & !0x00FF_FFFF) | HPLL_FRAC,
        );
        dev::mb();
        if !self.wait(PLL_WAIT_NS, || p.hpll_con1.read() & PLL_LOCK != 0) {
            return Err(Error::Settle {
                step: Step::HpllLock,
                off: offset_of!(PmuCru, hpll_con1) as u32,
                got: p.hpll_con1.read(),
                mask: PLL_LOCK,
                want: PLL_LOCK,
            });
        }
        put(&p.mode_con0, hiword(PLL_MODE_NORM, 0x3, HPLL_MODE_SHIFT));
        dev::mb();
        put(
            &self.cru().clksel39,
            hiword(DCLK_MUX_HPLL, 0x3, DCLK_MUX_SHIFT) | hiword(DCLK_DIV_1080P60 - 1, 0xFF, 0),
        );
        put(&p.clksel8, hiword(HDMI_REF_HPLL, 0x1, HDMI_REF_SHIFT));
        dev::mb();
        Ok(())
    }

    /// Zet beide VOP-IOMMU's uit. ZONDER DIT is het adres in de laag een
    /// IOVA en scant de VOP2 uit een pagina die niemand invulde: zwart of
    /// ruis, zonder één foutmelding. De valkuil van dit hele bestand.
    ///
    /// De sequentie (rk_iommu_disable): stall, paging uit, stall los.
    pub fn vop_iommu_off(&self) {
        for off in IOMMU {
            let m = self.iommu(off);
            if m.status.read() & IOMMU_PAGING == 0 {
                continue;
            }
            for cmd in [IOMMU_STALL, IOMMU_NO_PAGING, IOMMU_UNSTALL] {
                put(&m.command, cmd);
                dev::mb();
            }
        }
    }

    /// Zegt of de APB-kant van de VOP2 antwoordt, op VERSION_INFO.
    ///
    /// DIT IS DE TWEEDE VERSIE, en de eerste was een meetfout die een boot
    /// kostte. Omdat Linux VERSION_INFO nooit leest, had ik een
    /// schrijf-lees-test op VP_DSP_BG gebouwd. Die faalde op ijzer (06-08)
    /// en brak de scanout af, terwijl het blok leefde: in dezelfde boot las
    /// VERSION_INFO 0x4015_8023, noch nul noch alles-één. De les: een
    /// meetinstrument dat ik zelf verzin, wantrouw ik net zo hard als de
    /// code die het meet; een vals negatief kost evenveel als een echte
    /// fout.
    pub fn vop_alive(&self) -> Result {
        match self.vop_sys().version.read() {
            v @ (0 | u32::MAX) => Err(Error::VopDead { version: v }),
            _ => Ok(()),
        }
    }

    /// Brengt de VOP2 op en start de scanout van `fb` naar VP0. Na
    /// [`Chain::power_on_vo`] en [`Chain::vop_alive`].
    ///
    /// Er wordt geen modus gekozen: 1920x1080p60 staat vast, want de buffer
    /// in het plan heeft die maat.
    ///
    /// ER IS BEWUST GEEN STOP-PAD. Komt dat er ooit, dan is dit wat je moet
    /// weten: STANDBY (bit 31 van DSP_CTRL, als VOLLE write) gaat pas in aan
    /// het eind van het huidige frame, en wie daarna aclk uitzet vóórdat
    /// VP_INT_STATUS bit 6 (dsp_hold) staat, kan de geheugenbus laten
    /// hangen.
    pub fn vop_scanout(&self, fb: &Desc) -> Result {
        crate::check_geometry(fb)?;
        self.vop_pixel_clock()?;
        self.vop_iommu_off();
        self.vop_global();
        self.vop_timing();
        self.vop_overlay();
        self.vop_window(fb);
        self.vop_post();
        // Latchen. De venster-registers worden PAS hier geldig en lezen tot
        // dan hun oude waarde terug: een window-write terugleggen vóór
        // cfg_done liegt tegen je. De write-mask uit de datasheet bestaat op
        // dit silicium niet (de driver: op rk3566/8 hebben die bits geen
        // effect), dus gewoon het bit.
        put(&self.vop_sys().cfg_done, CFG_DONE_GLB_EN | CFG_DONE_VP0);
        dev::mb();
        // En dan STANDBY los: dit start de scan. DSP_CTRL altijd volledig
        // schrijven, nooit read-modify-write (zo doet de driver het ook).
        put(&self.vp0().dsp_ctrl, OUT_MODE_AAAA);
        dev::mb();
        Ok(())
    }

    /// Stap 1 en 2: de globale init (vop2_enable) en de interface-routing.
    fn vop_global(&self) {
        let s = self.vop_sys();
        // OTP_WIN_EN is RK3566-only; hij staat er omdat de driver hem zet.
        put(&s.otp_win_en, 1);
        put(&s.cfg_done, CFG_DONE_GLB_EN);
        // Auto-gating UIT. De driver noemt dit een workaround: laat je hem
        // aan, dan SCHUIFT het beeld zodra een window aangaat, een symptoom
        // dat je makkelijk voor een timingfout aanziet.
        put(&s.auto_gate, s.auto_gate.read() & !AUTO_GATE_EN);
        // De bus-error-interrupt aan: de goedkoopste diagnose als het
        // framebufferadres fout is.
        for r in [&s.int0_clr, &s.int0_en, &s.int1_clr, &s.int1_en] {
            put(r, INT_BUS_ERROR);
        }
        dev::mb();
        put(&s.dsp_if_en, s.dsp_if_en.read() | DSP_IF_EN_HDMI);
        put(&s.dsp_if_pol, CFG_DONE_IMD | (HDMI_PIN_POL << 4));
        dev::mb();
    }

    /// Stap 3: de VP0-timing.
    fn vop_timing(&self) {
        let v = self.vp0();
        put(&v.htotal_hs_end, (H_TOTAL << 16) | H_SYNC_LEN);
        put(&v.hact_st_end, (H_ACT_ST << 16) | H_ACT_END);
        put(&v.vtotal_vs_end, (V_TOTAL << 16) | V_SYNC_LEN);
        put(&v.vact_st_end, (V_ACT_ST << 16) | V_ACT_END);
        put(&self.vop_sys().line_flag0, (V_ACT_END << 16) | V_ACT_END);
        put(&v.mipi_ctrl, 0);
        dev::mb();
    }

    /// Stap 4: de overlay, één laag (Smart0) op VP0.
    fn vop_overlay(&self) {
        let s = self.vop_sys();
        put(
            &s.ovl_ctrl,
            (s.ovl_ctrl.read() & !1) | LAYER_SEL_REGDONE_IMD,
        );
        put(&s.layer_sel, LAYER_SEL);
        put(&s.port_sel, PORT_SEL);
        // SMART0-dly [23:16] = 20.
        put(&s.smart_dly, 20 << 16);
        put(&s.cluster_dly, 0);
        put(&s.hdr0_src_color, 0);
        dev::mb();
    }

    /// Stap 5: het venster. Formaat 0 = ARGB8888, en dat is ook wat
    /// XRGB8888 oplevert: de alfabits negeert de hardware bij een bodemlaag
    /// zonder mixer. `fb` is getoetst (`check_geometry`), dus de getallen
    /// passen in 32 bits.
    fn vop_window(&self, fb: &Desc) {
        let w = self.smart0();
        put(&w.ctrl0, 0);
        put(&w.ctrl1, 0);
        put(&w.mst, fb.base.0 as u32);
        // VIR is in WOORDEN. Schrijf je bytes, dan is de stride vier keer te
        // groot en zie je een vierde van het beeld uitgesmeerd.
        put(&w.vir, fb.stride / 4);
        let size = ((fb.height - 1) << 16) | (fb.width - 1);
        put(&w.act, size);
        put(&w.dsp, size);
        put(&w.dsp_st, 0);
        put(&w.scl, SCL_NONE_BIL);
        put(&w.scl_factor, 0);
        put(&w.color_key_ctrl, 0);
        dev::mb();
        // Enable als LAATSTE van dit blok.
        put(&w.region0_ctrl, WIN_EN);
        dev::mb();
    }

    /// Stap 6: de post-config.
    fn vop_post(&self) {
        let v = self.vp0();
        put(&self.vop_sys().bg_mix_ctrl0, BG_DLY << 24);
        put(
            &v.pre_scan_htiming,
            ((BG_DLY + H_DISPLAY / 2 - 1) << 16) | H_SYNC_LEN,
        );
        put(&v.post_dsp_hact, (H_ACT_ST << 16) | H_ACT_END);
        put(&v.post_dsp_vact, (V_ACT_ST << 16) | V_ACT_END);
        put(&v.post_scl_factor, POST_SCL_1_1);
        put(&v.post_scl_ctrl, 0);
        put(&v.dsp_bg, 0);
        dev::mb();
    }

    /// Zegt of VP0 de laatste configuratie overnam: bit 0 van REG_CFG_DONE
    /// valt weg zodra de VP hem bij frame-start latcht (zo leest vop2_isr
    /// het ook). `false` betekent dat de VP niet scant.
    pub fn vop_cfg_done_taken(&self) -> bool {
        let s = self.vop_sys();
        self.wait(LATCH_WAIT_NS, || s.cfg_done.read() & CFG_DONE_VP0 == 0)
    }

    /// De registers die een mislukte bring-up ontleden.
    #[must_use]
    pub fn vop_info(&self) -> VopInfo {
        let (c, s, v, w) = (self.cru(), self.vop_sys(), self.vp0(), self.smart0());
        VopInfo {
            gate20: c.gate20.read(),
            sel39: c.clksel39.read(),
            dsp_ctrl: v.dsp_ctrl.read(),
            cfg_done: s.cfg_done.read(),
            if_en: s.dsp_if_en.read(),
            win_ctrl: w.region0_ctrl.read(),
            win_mst: w.mst.read(),
            int_status: v.int_status.read(),
        }
    }
}
