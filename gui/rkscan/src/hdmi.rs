//! De HDMI-transmitter: een Synopsys DesignWare HDMI-TX, gevoed door VP0
//! van de VOP2, met zijn interne PHY.
//!
//! REFERENTIE (opgehaald 05-08): Linux v6.13
//! drivers/gpu/drm/bridge/synopsys/dw-hdmi.c en dw-hdmi.h (registers, de
//! init-volgorde, de PHY-I2C-master),
//! drivers/gpu/drm/rockchip/dw_hdmi-rockchip.c (de PHY-tabellen en de
//! GRF-bits voor dít silicium), rk356x-base.dtsi en rk3568-pinctrl.dtsi.
//!
//! DRIE DINGEN DIE DE VORM VAN DIT BESTAND BEPALEN:
//!
//! 1. **reg-io-width = 4.** Het dtsi zegt het en de driver rekent
//!    `offset << 2`. Elk offset uit dw-hdmi.h moet dus maal vier:
//!    FC_INVIDCONF staat niet op +0x1000 maar op +0x4000. Tweede
//!    bevestiging: max_register = 0x7E12 << 2 = 0x1F848 past net in het
//!    venster van 0x20000. Wie de factor mist, schrijft ergens anders in
//!    het blok en krijgt geen foutmelding. Het blok is ijl (een paar
//!    honderd byte-registers verspreid over 128 KB), dus staan de offsets
//!    hier als benoemde constanten met asserties in plaats van als
//!    `#[repr(C)]`-tabel.
//! 2. **DVI-mode, niet HDMI-mode.** Eén bit (FC_INVIDCONF bit 3), maar het
//!    scheelt de hele infoframe-, audio- en HDCP-tak. Een monitor synct
//!    hierop, zoals op een DVI-naar-HDMI-verloopje; wat je misloopt is
//!    VIC-, aspect- en quantisatie-signalering, wat op sommige tv's
//!    overscan of limited-range kan geven.
//! 3. **Er is GEEN aparte PHY-driver.** De RK3566 gebruikt de INTERNE
//!    Synopsys-PHY (geen phys-property, geen phy_ops in de Rockchip-glue);
//!    de configuratie loopt via een PHY-I2C-master ín dit blok.
//!
//! WAT DE BRON NIET ZEGT: welke PHY-variant dit silicium heeft (CONFIG2_ID
//! moet je LEZEN: alleen 0xb2, 0xc2 en 0xf3 hebben SVSRET), en de
//! verwachte DESIGN_ID/REVISION_ID (de driver noemt de RK3568 niet; we
//! melden ze in [`HdmiIds`] in plaats van erop te controleren).

use crate::{
    Chain, Error, H_DISPLAY, H_SYNC_LEN, H_SYNC_START, H_TOTAL, Result, Step, V_DISPLAY,
    V_SYNC_LEN, V_SYNC_START, V_TOTAL, hiword, journal, put,
};
use dev::Pa;

// ---------------------------------------------------------------------------
// De registers, in de nummering van dw-hdmi.h (maal vier in het blok).
// ---------------------------------------------------------------------------

/// Het byte-adres van register `off` in het blok (reg-io-width = 4).
pub(crate) const fn at(off: u16) -> u64 {
    (off as u64) << 2
}

// Identificatie: lezen vóór er één configuratieregister geschreven wordt.
/// DESIGN_ID.
pub(crate) const DESIGN_ID: u16 = 0x0000;
/// REVISION_ID.
pub(crate) const REVISION_ID: u16 = 0x0001;
/// PRODUCT_ID0: moet 0xA0 zijn.
pub(crate) const PRODUCT_ID0: u16 = 0x0002;
/// PRODUCT_ID1: zonder de HDCP-bits moet 0x01 zijn.
pub(crate) const PRODUCT_ID1: u16 = 0x0003;
/// CONFIG2_ID: het PHY-type.
pub(crate) const CONFIG2_ID: u16 = 0x0006;

// Interrupts. Wij pollen, dus alles blijft gemute; de IH-statusbits zetten
// zich ook gemute.
/// IH_I2CMPHY_STAT0: done en error van de PHY-I2C-master.
pub(crate) const IH_I2CMPHY_STAT0: u16 = 0x0108;
/// IH_MUTE_FC_STAT0: de eerste van tien mute-registers (0x0180..0x0189).
pub(crate) const IH_MUTE_BASE: u16 = 0x0180;
/// IH_MUTE_FC_STAT2: de overflow-interrupts.
pub(crate) const IH_MUTE_FC_STAT2: u16 = 0x0182;
/// IH_MUTE: de hoofdschakelaar.
pub(crate) const IH_MUTE: u16 = 0x01FF;

// Video sample (de ingang van de FIFO).
/// TX_INVID0.
pub(crate) const TX_INVID0: u16 = 0x0200;
/// TX_INSTUFFING.
pub(crate) const TX_INSTUFFING: u16 = 0x0201;
/// TX_GYDATA0: zes datastuffing-registers, 0x0202..0x0207.
pub(crate) const TX_GYDATA0: u16 = 0x0202;

// Video packetizer.
/// VP_PR_CD.
pub(crate) const VP_PR_CD: u16 = 0x0801;
/// VP_STUFF.
pub(crate) const VP_STUFF: u16 = 0x0802;
/// VP_REMAP.
pub(crate) const VP_REMAP: u16 = 0x0803;
/// VP_CONF.
pub(crate) const VP_CONF: u16 = 0x0804;
/// VP_MASK.
pub(crate) const VP_MASK: u16 = 0x0807;

// Frame composer: timings en de DVI/HDMI-keuze.
/// FC_INVIDCONF.
pub(crate) const FC_INVIDCONF: u16 = 0x1000;
/// FC_INHACTV0 (laag byte; 1 = hoog).
pub(crate) const FC_INHACTV0: u16 = 0x1001;
/// FC_INHACTV1.
pub(crate) const FC_INHACTV1: u16 = 0x1002;
/// FC_INHBLANK0.
pub(crate) const FC_INHBLANK0: u16 = 0x1003;
/// FC_INHBLANK1.
pub(crate) const FC_INHBLANK1: u16 = 0x1004;
/// FC_INVACTV0.
pub(crate) const FC_INVACTV0: u16 = 0x1005;
/// FC_INVACTV1.
pub(crate) const FC_INVACTV1: u16 = 0x1006;
/// FC_INVBLANK.
pub(crate) const FC_INVBLANK: u16 = 0x1007;
/// FC_HSYNCINDELAY0.
pub(crate) const FC_HSYNCINDELAY0: u16 = 0x1008;
/// FC_HSYNCINDELAY1.
pub(crate) const FC_HSYNCINDELAY1: u16 = 0x1009;
/// FC_HSYNCINWIDTH0.
pub(crate) const FC_HSYNCINWIDTH0: u16 = 0x100A;
/// FC_HSYNCINWIDTH1.
pub(crate) const FC_HSYNCINWIDTH1: u16 = 0x100B;
/// FC_VSYNCINDELAY.
pub(crate) const FC_VSYNCINDELAY: u16 = 0x100C;
/// FC_VSYNCINWIDTH.
pub(crate) const FC_VSYNCINWIDTH: u16 = 0x100D;
/// FC_CTRLDUR.
pub(crate) const FC_CTRLDUR: u16 = 0x1011;
/// FC_EXCTRLDUR.
pub(crate) const FC_EXCTRLDUR: u16 = 0x1012;
/// FC_EXCTRLSPAC.
pub(crate) const FC_EXCTRLSPAC: u16 = 0x1013;
/// FC_CH0PREAM.
pub(crate) const FC_CH0PREAM: u16 = 0x1014;
/// FC_CH1PREAM.
pub(crate) const FC_CH1PREAM: u16 = 0x1015;
/// FC_CH2PREAM.
pub(crate) const FC_CH2PREAM: u16 = 0x1016;
/// FC_DATAUTO3.
pub(crate) const FC_DATAUTO3: u16 = 0x10B7;
/// FC_MASK0.
pub(crate) const FC_MASK0: u16 = 0x10D2;
/// FC_MASK1.
pub(crate) const FC_MASK1: u16 = 0x10D6;
/// FC_MASK2.
pub(crate) const FC_MASK2: u16 = 0x10DA;

// De PHY.
/// PHY_CONF0.
pub(crate) const PHY_CONF0: u16 = 0x3000;
/// PHY_TST0.
pub(crate) const PHY_TST0: u16 = 0x3001;
/// PHY_STAT0: bit 0 lock, bit 1 HPD.
pub(crate) const PHY_STAT0: u16 = 0x3004;
/// PHY_MASK0.
pub(crate) const PHY_MASK0: u16 = 0x3006;

// De PHY-I2C-master: hiermee programmeer je de PHY-registers.
/// PHY_I2CM_SLAVE.
pub(crate) const PHY_I2CM_SLAVE: u16 = 0x3020;
/// PHY_I2CM_ADDRESS.
pub(crate) const PHY_I2CM_ADDRESS: u16 = 0x3021;
/// PHY_I2CM_DATAO_1 (hoog byte).
pub(crate) const PHY_I2CM_DATAO_1: u16 = 0x3022;
/// PHY_I2CM_DATAO_0 (laag byte).
pub(crate) const PHY_I2CM_DATAO_0: u16 = 0x3023;
/// PHY_I2CM_OPERATION.
pub(crate) const PHY_I2CM_OPERATION: u16 = 0x3026;
/// PHY_I2CM_INT.
pub(crate) const PHY_I2CM_INT: u16 = 0x3027;
/// PHY_I2CM_CTLINT.
pub(crate) const PHY_I2CM_CTLINT: u16 = 0x3028;

// Audio-klokregeneratie. Met audio uit horen deze op nul, en dat is geen
// cosmetica: de driver zet ze vóór de PHY aangaat "to prevent overflows in
// HDMI_IH_FC_STAT2".
/// AUD_N1: zes registers, 0x3200..0x3205 (N1..N3, CTS1..CTS3).
pub(crate) const AUD_N1: u16 = 0x3200;

// Main controller: klokken, resets, flowcontrol.
/// MC_CLKDIS: bit per klok, 1 = uit.
pub(crate) const MC_CLKDIS: u16 = 0x4001;
/// MC_SWRSTZ.
pub(crate) const MC_SWRSTZ: u16 = 0x4002;
/// MC_FLOWCTRL.
pub(crate) const MC_FLOWCTRL: u16 = 0x4004;
/// MC_PHYRSTZ.
pub(crate) const MC_PHYRSTZ: u16 = 0x4005;
/// MC_HEACPHY_RST.
pub(crate) const MC_HEACPHY_RST: u16 = 0x4007;

// HDCP: niet gebruiken, maar wél in de juiste stand.
/// A_HDCPCFG0.
pub(crate) const A_HDCPCFG0: u16 = 0x5000;
/// A_HDCPCFG1.
pub(crate) const A_HDCPCFG1: u16 = 0x5001;
/// A_VIDPOLCFG.
pub(crate) const A_VIDPOLCFG: u16 = 0x5009;

const _: () = {
    assert!(at(FC_INVIDCONF) == 0x4000);
    assert!(at(PHY_CONF0) == 0xC000);
    assert!(at(MC_CLKDIS) == 0x10004);
    // dw-hdmi: max_register = HDMI_I2CM_FS_SCL_LCNT_0_ADDR (0x7E12) << 2.
    assert!(at(0x7E12) == 0x1F848 && at(0x7E12) < 0x20000);
};

// ---------------------------------------------------------------------------
// Bitwaarden, alle uit dw-hdmi.h.
// ---------------------------------------------------------------------------

/// PRODUCT_ID0 van een HDMI-TX.
const PROD_ID0_HDMITX: u8 = 0xA0;
/// PRODUCT_ID1: de HDCP-bits.
const PROD_ID1_HDCP: u8 = 0xC0;
/// PRODUCT_ID1 zonder de HDCP-bits.
const PROD_ID1_VALUE: u8 = 0x01;

/// PHY_CONF0: SVSRET.
pub(crate) const CONF0_SVSRET: u8 = 0x20;
/// PHY_CONF0: GEN2_PDDQ.
pub(crate) const CONF0_PDDQ: u8 = 0x10;
/// PHY_CONF0: GEN2_TXPWRON.
pub(crate) const CONF0_TXPWRON: u8 = 0x08;
/// PHY_CONF0: SELDATAENPOL.
const CONF0_SELDATAENPOL: u8 = 0x02;
/// PHY_CONF0: SELDIPIF.
const CONF0_SELDIPIF: u8 = 0x01;
/// PHY_TST0: TSTCLR.
const TST0_TSTCLR: u8 = 0x20;
/// PHY_STAT0: TX_PHY_LOCK.
pub(crate) const STAT0_LOCK: u8 = 0x01;
/// PHY_STAT0: HPD.
pub(crate) const STAT0_HPD: u8 = 0x02;

/// Het I2C-adres van een Gen2-PHY.
pub(crate) const PHY_I2C_SLAVE_GEN2: u8 = 0x69;
/// PHY_I2CM_OPERATION: schrijf.
pub(crate) const I2C_OP_WRITE: u8 = 0x10;
/// IH_I2CMPHY_STAT0: error.
pub(crate) const I2C_STAT_ERROR: u8 = 0x01;
/// IH_I2CMPHY_STAT0: done.
pub(crate) const I2C_STAT_DONE: u8 = 0x02;

/// MC_PHYRSTZ: op Gen2 ACTIEF HOOG, dus 1 en dan 0.
const MC_PHYRSTZ_ASSERT: u8 = 0x01;
/// MC_HEACPHY_RST: assert.
const MC_HEACPHY_ASSERT: u8 = 0x01;
/// MC_SWRSTZ: alleen de TMDS-softreset (~TMDSSWRST_REQ).
pub(crate) const MC_SWRSTZ_TMDS: u8 = 0xFD;
/// FC_DATAUTO3: GCP automatisch.
const DATAUTO3_GCP_AUTO: u8 = 0x04;
/// A_HDCPCFG0: RX-detectie.
const HDCPCFG0_RXDETECT: u8 = 0x04;
/// A_HDCPCFG1: encryptie uit.
const HDCPCFG1_ENCRYPT_DISABLE: u8 = 0x02;
/// A_VIDPOLCFG: het DE-polariteitsveld en de stand actief-hoog.
const VIDPOLCFG_DATAEN_HIGH: u8 = 0x10;

/// FC_INVIDCONF voor DVI: VSYNC_HIGH | HSYNC_HIGH | DE_HIGH, progressief,
/// en DVI_MODE = 0 (HDMI-mode zou 0x08 erbij zetten).
pub(crate) const INVIDCONF_DVI: u8 = 0x40 | 0x20 | 0x10;
/// MC_CLKDIS: alleen de pixelklok loopt.
pub(crate) const CLKDIS_PIXEL: u8 = 0x7E;
/// MC_CLKDIS: de TMDS-klok erbij. TWEE APARTE WRITES: de driver doet dat
/// expliciet gescheiden, dus wij ook.
pub(crate) const CLKDIS_PIXEL_TMDS: u8 = 0x7C;

// De timings zoals de FRAME COMPOSER ze wil: h_de_hs = hsync_start -
// hdisplay (88), en niet de VOP2-conventie (192).
/// 280.
const FC_HBLANK: u32 = H_TOTAL - H_DISPLAY;
/// 45.
const FC_VBLANK: u32 = V_TOTAL - V_DISPLAY;
/// 88.
const FC_H_DE_HS: u32 = H_SYNC_START - H_DISPLAY;
/// 4.
const FC_V_DE_VS: u32 = V_SYNC_START - V_DISPLAY;

// De PHY-registers ín de PHY (via de I2C-master), met de waarden die bij
// 148,5 MHz horen. De selectie is "eerste tabelregel met mpixelclock >=
// 148500000" uit dw_hdmi-rockchip.c: mpll-regel 184 MHz, cur_ctr-regel
// 600 MHz, phy_config-regel 165 MHz. De volgorde is die van de driver.
/// (register, waarde) voor 148,5 MHz.
pub(crate) const PHY_148M5: [(u8, u16); 9] = [
    (0x06, 0x0051), // CPCE_CTRL
    (0x15, 0x0002), // GMPCTRL
    (0x10, 0x0000), // CURRCTRL
    (0x13, 0x0000), // PLLPHBYCTRL
    (0x17, 0x0006), // MSM_CTRL: CKO_SEL_FB_CLK
    (0x19, 0x0004), // TXTERM
    (0x09, 0x802B), // CKSYMTXCTRL
    (0x0E, 0x0209), // VLEVCTRL
    (0x05, 0x8000), // CKCALCTRL: OVERRIDE
];

/// Hoe lang een PHY-I2C-write mag duren (Go: 50 ms).
const PHY_I2C_WAIT_NS: u64 = 50_000_000;
/// Hoe lang de PHY over (ont)locken mag doen (Go: 20 ms).
const PHY_LOCK_WAIT_NS: u64 = 20_000_000;

// ---------------------------------------------------------------------------
// De klokken.
// ---------------------------------------------------------------------------

// De klokken van het blok (rk356x-base.dtsi: "iahb", "isfr", "cec", "ref",
// plus een naamloze phandle naar HCLK_VO die via het power-domein
// binnenkomt; die zet `vop_clock_on` al). cec blijft dicht.
/// CLKGATE_CON(21): PCLK_HDMI_HOST ("iahb").
const GATE_PCLK_HDMI_HOST: u32 = 3;
/// CLKGATE_CON(21): CLK_HDMI_SFR ("isfr").
const GATE_CLK_HDMI_SFR: u32 = 4;

/// De identificatie van de HDMI-TX.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct HdmiIds {
    /// DESIGN_ID.
    pub design: u8,
    /// REVISION_ID.
    pub rev: u8,
    /// PRODUCT_ID0.
    pub prod0: u8,
    /// PRODUCT_ID1.
    pub prod1: u8,
    /// CONFIG2_ID: het PHY-type.
    pub config2: u8,
}

impl HdmiIds {
    /// Exact de controle die dw_hdmi_probe doet.
    #[must_use]
    pub const fn is_hdmi_tx(&self) -> bool {
        self.prod0 == PROD_ID0_HDMITX && self.prod1 & !PROD_ID1_HDCP == PROD_ID1_VALUE
    }

    /// Heeft deze PHY-variant de SVSRET-bit? Uit dw_hdmi_phys[]: alleen
    /// 0xb2, 0xc2 en 0xf3. Gelezen in plaats van gegokt, want die bit
    /// verkeerd zetten kan betekenen dat de PLL niet lockt.
    #[must_use]
    pub const fn has_svsret(&self) -> bool {
        matches!(self.config2, 0xB2 | 0xC2 | 0xF3)
    }
}

/// De registers die een mislukte bring-up ontleden.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct HdmiInfo {
    /// PHY_STAT0.
    pub phy_stat: u8,
    /// PHY_CONF0.
    pub phy_conf: u8,
    /// MC_CLKDIS.
    pub clkdis: u8,
    /// FC_INVIDCONF.
    pub invidconf: u8,
    /// VP_CONF.
    pub vp_conf: u8,
}

impl Chain {
    fn hd(&self, off: u16) -> Pa {
        self.b.hdmi.add(at(off))
    }

    /// Leest HDMI-register `off` (acht bits in een 32-bits woord).
    fn hrd(&self, off: u16) -> u8 {
        dev::read32(self.hd(off)) as u8
    }

    /// Schrijft HDMI-register `off`.
    fn hwr(&self, off: u16, v: u8) {
        let pa = self.hd(off);
        dev::write32(pa, u32::from(v));
        journal::note(pa.0, u32::from(v));
    }

    /// Read-modify-write van de bits in `mask`.
    fn hmod(&self, off: u16, mask: u8, val: u8) {
        self.hwr(off, (self.hrd(off) & !mask) | (val & mask));
    }

    /// Opent de twee klokken die het registerblok nodig heeft. Zonder iahb
    /// lezen álle registers 0x00 of 0xFF, en daarom leest
    /// [`Chain::hdmi_ids`] vóór elke schrijfactie.
    pub fn hdmi_clock_on(&self) {
        put(
            &self.cru().gate21,
            hiword(0, 1, GATE_PCLK_HDMI_HOST) | hiword(0, 1, GATE_CLK_HDMI_SFR),
        );
        dev::mb();
    }

    /// De identificatie. In één boot bewijst dit het basisadres, de
    /// maal-vier-stap, de klokken en het power-domein.
    #[must_use]
    pub fn hdmi_ids(&self) -> HdmiIds {
        HdmiIds {
            design: self.hrd(DESIGN_ID),
            rev: self.hrd(REVISION_ID),
            prod0: self.hrd(PRODUCT_ID0),
            prod1: self.hrd(PRODUCT_ID1),
            config2: self.hrd(CONFIG2_ID),
        }
    }

    /// De eenmalige blok-init: alles gemute, de PHY-I2C-master op polling,
    /// en de audioteller op nul.
    fn hdmi_init_hw(&self) {
        self.hwr(IH_MUTE, 0x03); // MUTE_WAKEUP | MUTE_ALL
        for r in [VP_MASK, FC_MASK0, FC_MASK1, FC_MASK2, PHY_MASK0] {
            self.hwr(r, 0xFF);
        }
        for i in 0..10 {
            self.hwr(IH_MUTE_BASE + i, 0xFF);
        }
        self.hwr(IH_MUTE_FC_STAT2, 0x03);
        // DE VALKUIL waar een hele boot-cyclus in kan verdwijnen: de
        // mute-ronde hierboven laat PHY_I2CM_INT op 0xFF (masker AAN), maar
        // de driver zet hem daarna op 0x08: DONE_POL, met DONE_MASK GEWIST.
        // Laat je dat weg, dan blijft IH_I2CMPHY_STAT0 nul en loopt ELKE
        // PHY-write in een timeout terwijl er niets kapot is.
        self.hwr(PHY_I2CM_INT, 0x08);
        self.hwr(PHY_I2CM_CTLINT, 0x88); // NAC_POL | ARBITRATION_POL
        for i in 0..6 {
            self.hwr(AUD_N1 + i, 0);
        }
        dev::mb();
    }

    /// Schrijft één 16-bits PHY-register via de PHY-I2C-master.
    fn phy_i2c_write(&self, reg: u8, data: u16, round: u8) -> Result {
        self.hwr(IH_I2CMPHY_STAT0, 0xFF); // status wissen
        self.hwr(PHY_I2CM_ADDRESS, reg);
        let [hi, lo] = data.to_be_bytes();
        self.hwr(PHY_I2CM_DATAO_1, hi);
        self.hwr(PHY_I2CM_DATAO_0, lo);
        self.hwr(PHY_I2CM_OPERATION, I2C_OP_WRITE);
        dev::mb();
        let mut st = 0;
        if self.wait(PHY_I2C_WAIT_NS, || {
            st = self.hrd(IH_I2CMPHY_STAT0) & (I2C_STAT_ERROR | I2C_STAT_DONE);
            st != 0
        }) {
            self.hwr(IH_I2CMPHY_STAT0, st); // de gezette bits terugschrijven
            return Ok(());
        }
        Err(Error::PhyI2c { reg, round })
    }

    /// Eén ronde van de PHY-sequentie. DE HELE SEQUENTIE MOET TWEE KEER; de
    /// driver zegt het letterlijk: "HDMI Phy spec says to do the phy
    /// initialization sequence twice". Eén keer werkt soms, en dat is erger
    /// dan nooit.
    fn hdmi_phy_configure(&self, svsret: bool, round: u8) -> Result {
        self.hmod(PHY_CONF0, CONF0_SELDATAENPOL, CONF0_SELDATAENPOL);
        self.hmod(PHY_CONF0, CONF0_SELDIPIF, 0);
        // Power off: TXPWRON uit, wachten tot LOCK weg is, dan PDDQ aan. Een
        // lock die blijft staan is geen reden om te stoppen (Go ook niet):
        // de reset hieronder haalt hem toch weg.
        self.hmod(PHY_CONF0, CONF0_TXPWRON, 0);
        let _ = self.wait(PHY_LOCK_WAIT_NS, || self.hrd(PHY_STAT0) & STAT0_LOCK == 0);
        self.hmod(PHY_CONF0, CONF0_PDDQ, CONF0_PDDQ);
        if svsret {
            self.hmod(PHY_CONF0, CONF0_SVSRET, CONF0_SVSRET);
        }
        // Gen2-reset: ACTIEF HOOG, dus 1 dan 0. Op Gen1 is het omgekeerd, en
        // die polariteit verwisselen betekent dat de PHY níet reset en met
        // oude MPLL-instellingen lockt, of niet lockt.
        self.hwr(MC_PHYRSTZ, MC_PHYRSTZ_ASSERT);
        self.hwr(MC_PHYRSTZ, 0);
        self.hwr(MC_HEACPHY_RST, MC_HEACPHY_ASSERT);
        // De I2C-master op de PHY.
        self.hmod(PHY_TST0, TST0_TSTCLR, TST0_TSTCLR);
        self.hwr(PHY_I2CM_SLAVE, PHY_I2C_SLAVE_GEN2);
        self.hmod(PHY_TST0, TST0_TSTCLR, 0);
        for (reg, val) in PHY_148M5 {
            self.phy_i2c_write(reg, val, round)?;
        }
        // Power on: TXPWRON aan, PDDQ uit, wachten op LOCK.
        self.hmod(PHY_CONF0, CONF0_TXPWRON, CONF0_TXPWRON);
        self.hmod(PHY_CONF0, CONF0_PDDQ, 0);
        dev::mb();
        if self.wait(PHY_LOCK_WAIT_NS, || self.hrd(PHY_STAT0) & STAT0_LOCK != 0) {
            return Ok(());
        }
        Err(Error::Settle {
            step: Step::PhyLock,
            off: at(PHY_STAT0) as u32,
            got: u32::from(self.hrd(PHY_STAT0)),
            mask: u32::from(STAT0_LOCK),
            want: u32::from(STAT0_LOCK),
        })
    }

    /// Brengt de transmitter op in DVI-mode voor 1920x1080p60 RGB. NÁ
    /// [`Chain::vop_scanout`]: als de PHY aangaat zonder dat VOP2 pixels en
    /// een dclk levert, staat de frame composer tegen een stilstaande klok.
    pub fn hdmi_enable(&self) -> Result<HdmiIds> {
        self.hdmi_clock_on();
        let ids = self.hdmi_ids();
        if !ids.is_hdmi_tx() {
            return Err(Error::HdmiId {
                prod0: ids.prod0,
                prod1: ids.prod1,
            });
        }
        self.hdmi_init_hw();
        self.hdmi_frame_composer();
        // De PHY, twee keer.
        let svsret = ids.has_svsret();
        self.hdmi_phy_configure(svsret, 1)?;
        self.hdmi_phy_configure(svsret, 2)?;
        self.hdmi_video_path();
        Ok(ids)
    }

    /// Stap 1: frame composer, timings, polariteiten, DVI-mode.
    fn hdmi_frame_composer(&self) {
        let lo = |v: u32| (v & 0xFF) as u8;
        let hi = |v: u32| (v >> 8) as u8;
        self.hwr(FC_INVIDCONF, INVIDCONF_DVI);
        self.hwr(FC_INHACTV1, hi(H_DISPLAY));
        self.hwr(FC_INHACTV0, lo(H_DISPLAY));
        self.hwr(FC_INVACTV1, hi(V_DISPLAY));
        self.hwr(FC_INVACTV0, lo(V_DISPLAY));
        self.hwr(FC_INHBLANK1, hi(FC_HBLANK));
        self.hwr(FC_INHBLANK0, lo(FC_HBLANK));
        self.hwr(FC_INVBLANK, lo(FC_VBLANK));
        self.hwr(FC_HSYNCINDELAY1, hi(FC_H_DE_HS));
        self.hwr(FC_HSYNCINDELAY0, lo(FC_H_DE_HS));
        self.hwr(FC_VSYNCINDELAY, lo(FC_V_DE_VS));
        self.hwr(FC_HSYNCINWIDTH1, hi(H_SYNC_LEN));
        self.hwr(FC_HSYNCINWIDTH0, lo(H_SYNC_LEN));
        self.hwr(FC_VSYNCINWIDTH, lo(V_SYNC_LEN));
        dev::mb();
    }

    /// Stap 3 tot en met 7: het video-pad aan, packetizer, sample, HDCP en
    /// de overflow-workaround.
    fn hdmi_video_path(&self) {
        self.hwr(FC_CTRLDUR, 12);
        self.hwr(FC_EXCTRLDUR, 32);
        self.hwr(FC_EXCTRLSPAC, 1);
        self.hwr(FC_CH0PREAM, 0x0B);
        self.hwr(FC_CH1PREAM, 0x16);
        self.hwr(FC_CH2PREAM, 0x21);
        // Twee aparte writes, in deze volgorde.
        self.hwr(MC_CLKDIS, CLKDIS_PIXEL);
        self.hwr(MC_CLKDIS, CLKDIS_PIXEL_TMDS);
        self.hwr(MC_FLOWCTRL, 0); // CSC bypass: RGB in, RGB uit
        dev::mb();
        // Packetizer, 8-bit RGB. De eindwaarden staan vast; de driver komt
        // er via vier read-modify-writes op uit.
        self.hwr(VP_PR_CD, 0x40); // color_depth 4 (8-bit), geen pixel repetition
        self.hmod(FC_DATAUTO3, DATAUTO3_GCP_AUTO, 0);
        self.hwr(VP_STUFF, 0x27);
        self.hwr(VP_REMAP, 0x00);
        self.hwr(VP_CONF, 0x47); // bypass aan, output = bypass
        dev::mb();
        // Video sample: RGB888 op de ingang.
        self.hwr(TX_INVID0, 0x01); // video_mapping 1 = RGB888_1X24
        self.hwr(TX_INSTUFFING, 0x07);
        for i in 0..6 {
            self.hwr(TX_GYDATA0 + i, 0);
        }
        dev::mb();
        // HDCP in de juiste stand: detectie uit, encryptie uit. De
        // DVI/HDMI-keuze zit NIET hier (bit 0 van A_HDCPCFG0 raakt de driver
        // nooit aan) maar in FC_INVIDCONF.
        self.hmod(A_HDCPCFG0, HDCPCFG0_RXDETECT, 0);
        self.hmod(A_VIDPOLCFG, VIDPOLCFG_DATAEN_HIGH, VIDPOLCFG_DATAEN_HIGH);
        self.hmod(
            A_HDCPCFG1,
            HDCPCFG1_ENCRYPT_DISABLE,
            HDCPCFG1_ENCRYPT_DISABLE,
        );
        dev::mb();
        // De overflow-workaround, NIET optioneel: de klassieke "geen beeld
        // ondanks perfecte registers". De FC-rekeneenheid kan een write
        // missen; de fix is een TMDS-softreset plus FC_INVIDCONF opnieuw.
        self.hwr(MC_SWRSTZ, MC_SWRSTZ_TMDS);
        self.hwr(FC_INVIDCONF, self.hrd(FC_INVIDCONF));
        dev::mb();
    }

    /// Hangt er een sink aan de kabel (PHY_STAT0 bit 1)?
    #[must_use]
    pub fn hdmi_hotplug(&self) -> bool {
        self.hrd(PHY_STAT0) & STAT0_HPD != 0
    }

    /// De registers die een mislukte bring-up ontleden.
    #[must_use]
    pub fn hdmi_info(&self) -> HdmiInfo {
        HdmiInfo {
            phy_stat: self.hrd(PHY_STAT0),
            phy_conf: self.hrd(PHY_CONF0),
            clkdis: self.hrd(MC_CLKDIS),
            invidconf: self.hrd(FC_INVIDCONF),
            vp_conf: self.hrd(VP_CONF),
        }
    }
}
