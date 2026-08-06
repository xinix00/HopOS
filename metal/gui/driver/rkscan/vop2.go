package rkscan

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De VOP2 display-controller van de RK3566: leest de framebuffer die het board
// aanwijst (rk3566.FB(), via de VOPScanout-argumenten) en scant hem uit naar
// Video Port 0, die op dit bord aan de HDMI-transmitter hangt.
//
// WAAROM DIT BESTAAT, want elders in de boom staat het omgekeerde: op de Pi
// bouwen we géén HVS-driver, omdat beeld daar een firmware-buffer is. Dit bord
// heeft geen firmware-buffer én geen U-Boot-video (gemeten: `Out:
// serial@fe660000`, geen vidconsole), en de HDMI-uitgang zit er niet voor niets
// op (Derek, 05-08). Dus is de scanout hier wél ons werk.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13
// drivers/gpu/drm/rockchip/rockchip_drm_vop2.c (vop2_enable,
// vop2_crtc_atomic_enable, vop2_setup_layer_mixer, vop2_plane_atomic_update,
// vop2_post_config, vop2_cfg_done), rockchip_drm_vop2.h (offsets),
// rockchip_vop2_reg.c (de RK3566-platformdata: window-bases, layer_sel_id's,
// pre_scan_max_dly), rk356x-base.dtsi + rk3566-radxa-zero-3.dtsi (adres, VP0 →
// HDMI, DCLK_VOP0 ← HPLL), clk-rk3568.c en clk-pll.c (klokken en de PLL),
// drivers/iommu/rockchip-iommu.c (de VOP-IOMMU), en drm_edid.c voor de
// 1080p60-timing (CEA VIC 16).
//
// WAT DE BRON NIET ZEGT, en dus ook hier niet gedaan wordt:
//
//   - Er is GEEN version-/ID-register om op te controleren. `VERSION_INFO`
//     (0x004) bestaat als offset maar wordt in de hele Linux-driver nooit
//     gelezen, dus is er geen bewezen liveness-check. VOPAlive hieronder doet
//     daarom een schrijf-lees-test op een onschuldig register — dat is MIJN
//     constructie en niet uit de bron; zie de toelichting daar.
//   - Er is GEEN AXI/QoS/burst-configuratie. De driver raakt `SYS_AXI_LUT_CTRL`
//     nooit aan. Een 1080p32-scanout is ~475MB/s en leunt dus volledig op
//     reset-defaults van de NoC. Gezien de drie-poten-freeze op de Pi
//     (scanout leest fb + CPU schrijft fb + verkeer de kooi in) is dit precies
//     het gebied waar ik geen bron heb en waar een meting over zal gaan.
//   - Er is GEEN voorgeschreven reset-sequence: het vop-node heeft geen
//     `resets`-property en de driver reset nooit. De SRST_*-id's bestaan wel.
const (
	vopBase = 0xFE040000 // rk356x-base.dtsi: vop@fe040000, 0x3000 groot

	// --- globaal ---
	vopRegCfgDone   = 0x0000 // per-VP latch-bit + GLB_CFG_DONE_EN (bit 15)
	vopSysAutoGate  = 0x0008 // bit 31 = auto-gating; moet UIT
	vopSysInt0En    = 0x0080
	vopSysInt0Clr   = 0x0084
	vopSysInt1En    = 0x0090
	vopSysInt1Clr   = 0x0094
	vopDSPIfEn      = 0x0028 // welke interface aan, en uit welke VP
	vopDSPIfPol     = 0x0030 // polariteiten + CFG_DONE_IMD
	vopOTPWinEn     = 0x0050 // RK3566-ONLY; de bron geeft de waarde, niet de reden
	vopVPLineFlag0  = 0x0070
	vopVPBGMixCtrl0 = 0x06E0

	// --- overlay ---
	vopOvlCtrl       = 0x0600
	vopOvlLayerSel   = 0x0604
	vopOvlPortSel    = 0x0608
	vopClusterDlyNum = 0x06F0
	vopSmartDlyNum   = 0x06F8
	vopHDR0SrcColor  = 0x06C0

	// --- Video Port 0 (offset 0xC00; VP1 = 0xD00, VP2 = 0xE00) ---
	vp0 = 0x0C00

	vpDSPCtrl        = vp0 + 0x00
	vpMIPICtrl       = vp0 + 0x04
	vpDSPBG          = vp0 + 0x2C
	vpPreScanHTiming = vp0 + 0x30
	vpPostDSPHAct    = vp0 + 0x34
	vpPostDSPVAct    = vp0 + 0x38
	vpPostSclFactor  = vp0 + 0x3C
	vpPostSclCtrl    = vp0 + 0x40
	vpDSPHTotalHSEnd = vp0 + 0x48
	vpDSPHActStEnd   = vp0 + 0x4C
	vpDSPVTotalVSEnd = vp0 + 0x50
	vpDSPVActStEnd   = vp0 + 0x54
	vpIntStatus      = vp0 + 0xA8

	// --- Smart0-win0 (base 0x1C00, layer_sel_id 3) ---
	//
	// De EENVOUDIGSTE bruikbare layer, en dat is niet een smaak maar wat de
	// Linux-driver op dit silicium zelf kiest: vop2_create_crtcs slaat op
	// soc_id 3566 SMART1/ESMART1/CLUSTER1 over ("these windows don't have an
	// independent framebuffer, they share it with smart0/esmart0/cluster0") en
	// wijst dan de eerste PRIMARY in array-volgorde aan VP0 toe — dat is
	// Smart0-win0. Bovendien kent Smart alleen RGB (geen YUV, geen AFBC, geen
	// CSC-pad) en geen tweede enable-bit zoals Cluster.
	sm0 = 0x1C00

	smCtrl0        = sm0 + 0x00 // Y2R/R2Y/CSC — alles 0 voor RGB→RGB
	smCtrl1        = sm0 + 0x04 // bit31 = YMIRROR
	smRegion0Ctrl  = sm0 + 0x10 // bit0 = WIN0_EN, [5:1] = formaat
	smRegion0MST   = sm0 + 0x14 // framebufferadres, volle 32 bits
	smRegion0VIR   = sm0 + 0x1C // stride in DWORDS, niet in bytes
	smRegion0Act   = sm0 + 0x20
	smRegion0DSP   = sm0 + 0x24
	smRegion0DSPSt = sm0 + 0x28
	smRegion0Scl   = sm0 + 0x30
	smRegion0SclF  = sm0 + 0x34
	smColorKeyCtrl = sm0 + 0xD0

	cfgDoneGlbEn = 1 << 15
	autoGateEn   = 1 << 31
	winEnable    = 1 << 0

	// De VOP-IOMMU. Dit is de belangrijkste valkuil van het hele bestand: staat
	// hij aan, dan is het adres in smRegion0MST een IOVA en niet ons fysieke
	// 0x07000000 — en dan zie je zwart of ruis zonder enige foutmelding. Twee
	// instanties, beide uit.
	iommu0 = 0xFE043E00
	iommu1 = 0xFE043F00

	iommuStatus  = 0x04
	iommuCommand = 0x08

	iommuPagingEnabled    = 1 << 0
	iommuCmdEnableStall   = 2
	iommuCmdDisablePaging = 1
	iommuCmdDisableStall  = 3
)

// Klokken en power van de VOP2. De keten is drie niveaus diep en elk niveau
// heeft zijn eigen gate; één dichte gate ergens daarin en het hele blok leest
// als nullen of houdt de bus vast — en er is (zie de kop) geen
// versieregister waarmee je dat onderscheidt.
const (
	cruCLKGATE20 = 0x300 + 20*4 // = 0x350
	cruCLKSEL39  = 0x100 + 39*4 // = 0x19C — DCLK_VOP0 mux/div

	gateACLKVO     = 0  // aclk_vo   (ouder van hclk_vo)
	gateHCLKVO     = 1  // hclk_vo   (ouder van hclk_vop)
	gatePCLKVO     = 2  // pclk_vo   (hoort bij PD_VO)
	gateACLKVOPPre = 6  // aclk_vop_pre (ouder van aclk_vop)
	gateACLKVOP    = 8  // aclk_vop
	gateHCLKVOP    = 9  // hclk_vop
	gateDCLKVOP0   = 10 // dclk_vop0 — de pixelklok van VP0

	// DCLK_VOP0: mux [11:10] over {hpll, vpll, gpll, cpll}, deler [7:0].
	// Dit bord kiest HPLL (rk3566-radxa-zero-3.dtsi: assigned-clock-parents
	// = <&pmucru PLL_HPLL>, ...), en de mux heeft CLK_SET_RATE_NO_REPARENT —
	// Linux herkiest die ouder dus nooit zelf.
	dclkMuxShift = 10
	dclkDivShift = 0
	dclkMuxHPLL  = 0
)

// HPLL zit in de PMUCRU, niet in de CRU — één verkeerd basisadres en je verzet
// een PLL waar DDR of de CPU aan hangt.
//
// HPLL KRIJGT 148,5MHz RECHTSTREEKS, met DCLK-deler 1. Dat is niet de enige
// manier om aan 148,5 te komen (1188MHz met deler 8 kan ook, en zo had ik het
// eerst), maar het is wél de enige juiste: **HPLL voedt óók CLK_HDMI_REF**, en de
// HDMI-PHY verwacht die referentie op de pixelklok zelf. Met HPLL op 1188 lockt
// de PHY op de verkeerde frequentie — mogelijk mét lock, en dan een monitor die
// "geen signaal" zegt terwijl alles er goed uitziet. Precies het soort
// koppeling dat je pas ziet als je beide kanten van de keten naast elkaar legt.
//
// Gevolg om te weten: VP0 en HDMI zitten via deze PLL aan elkaar vast. Een
// tweede modus verzet ze samen.
const (
	pmucruBase = 0xFDD00000 // rk356x-base.dtsi: clock-controller@fdd00000

	// PLL_HPLL = PMU_PLL_CON(16); RK3568_PMU_PLL_CON(x) = x*4.
	hpllCon0 = pmucruBase + 16*4 + 0x00 // fbdiv [11:0], postdiv1 [14:12]
	hpllCon1 = pmucruBase + 16*4 + 0x04 // refdiv [5:0], postdiv2 [8:6], lock bit10, dsmpd bit12, pwrdown bit13
	hpllCon2 = pmucruBase + 16*4 + 0x08 // frac [23:0] — NIET hiword-masked

	pmuModeCon0   = pmucruBase + 0x80 // HPLL-modus op shift 2, 2 bits
	hpllModeShift = 2
	pllModeSlow   = 0 // xin24m rechtstreeks — verplicht tijdens herprogrammeren
	pllModeNorm   = 1 // de PLL-uitgang

	pllLockStatus = 1 << 10

	// rk3568_pll_rates: RK3036_PLL_RATE(148500000, 1, 99, 4, 4, 1, 0)
	// → 24MHz × 99 / (1 × 4 × 4) = 148,5MHz.
	hpllFbdiv    = 99
	hpllPostdiv1 = 4
	hpllRefdiv   = 1
	hpllPostdiv2 = 4
	hpllDsmpd    = 1
	hpllFrac     = 0

	dclkDivFor1080p60 = 1 // HPLL staat al op de pixelklok

	// CLK_HDMI_REF kiest tussen hpll en hpll_ph0 (= HPLL/2). Bit 7 op 0 =
	// hpll. Staat hij op 1, dan is de PHY-referentie 74,25MHz.
	pmuCLKSEL8      = pmucruBase + 0x100 + 8*4 // = 0xFDD00120
	hdmiRefSelShift = 7
	hdmiRefSelHPLL  = 0
)

// VOPClockOn opent de hele klokketen van de VOP2 en zet de DCLK-bron op HPLL.
// Losgetrokken van PowerOnVO omdat het power-domein deze klokken nodig heeft
// tijdens het schakelen (rockchip_pd_power doet clk_bulk_enable eromheen).
func VOPClockOn() {
	var g uint32
	for _, b := range []uint32{gateACLKVO, gateHCLKVO, gatePCLKVO,
		gateACLKVOPPre, gateACLKVOP, gateHCLKVOP, gateDCLKVOP0} {
		g |= hiword(0, 1, b)
	}
	dev.Write32(cruBase+cruCLKGATE20, g)
	dev.MB()
}

// VOPPixelClock zet HPLL op 148,5MHz en DCLK_VOP0 daar rechtstreeks op (deler 1)
// — zie het const-blok hierboven voor waarom 1188MHz met deler 8 (rekenkundig
// hetzelfde) juist fout is: HPLL voedt óók de HDMI-PHY-referentie.
//
// De volgorde is die van rockchip_rk3036_pll_set_params en is niet vrij: een
// PLL mag niet worden herprogrammeerd terwijl er iets uit hangt, dus gaat de
// modus eerst naar SLOW (xin24m rechtstreeks), dan de coëfficiënten, dan wachten
// op LOCK, en dan pas terug naar NORM.
func VOPPixelClock() error {
	// 1. Modus naar SLOW. De mode-mux is hiword-masked, 2 bits.
	dev.Write32(pmuModeCon0, hiword(pllModeSlow, 0x3, hpllModeShift))
	dev.MB()

	// 2. De coëfficiënten. CON0 en CON1 zijn hiword-masked; CON2 (frac) is dat
	//    NIET — de driver zegt het expliciet ("GPLL CON2 is not HIWORD_MASK") en
	//    doet daar een read-modify-write.
	dev.Write32(hpllCon0, hiword(hpllFbdiv, 0xFFF, 0)|hiword(hpllPostdiv1, 0x7, 12))
	dev.Write32(hpllCon1, hiword(hpllRefdiv, 0x3F, 0)|
		hiword(hpllPostdiv2, 0x7, 6)|
		hiword(hpllDsmpd, 0x1, 12))
	dev.Write32(hpllCon2, dev.Read32(hpllCon2)&^0xFFFFFF|hpllFrac)
	dev.MB()

	// 3. Wachten op LOCK. Begrensd: een PLL die niet lockt mag de boot niet
	//    ophouden, en de melding draagt de registerinhoud zodat één boot genoeg
	//    is om te zien of het aan de coëfficiënten of aan de bron ligt.
	deadline := time.Now().Add(10 * time.Millisecond)
	for dev.Read32(hpllCon1)&pllLockStatus == 0 {
		if time.Now().After(deadline) {
			return &pmuError{what: "HPLL lock", off: 16*4 + 0x04,
				got: dev.Read32(hpllCon1), mask: pllLockStatus, want: pllLockStatus}
		}
	}

	// 4. Terug naar NORM, en dan de deler van de VP.
	dev.Write32(pmuModeCon0, hiword(pllModeNorm, 0x3, hpllModeShift))
	dev.MB()
	dev.Write32(cruBase+cruCLKSEL39,
		hiword(dclkMuxHPLL, 0x3, dclkMuxShift)|
			hiword(dclkDivFor1080p60-1, 0xFF, dclkDivShift))
	// En de HDMI-referentie op hpll (niet op hpll_ph0): dezelfde PLL, want de
	// PHY moet op de pixelklok locken.
	dev.Write32(pmuCLKSEL8, hiword(hdmiRefSelHPLL, 0x1, hdmiRefSelShift))
	dev.MB()
	return nil
}

// VOPIOMMUOff zet beide VOP-IOMMU-instanties uit. ZONDER DIT is het adres in de
// window-descriptor een IOVA en scant de VOP2 uit een pagina die wij nooit
// hebben ingevuld — zwart of ruis, en geen enkele foutmelding.
//
// De sequentie (rk_iommu_disable): eerst stall, dan paging uit, dan stall los.
func VOPIOMMUOff() {
	for _, base := range []uintptr{iommu0, iommu1} {
		if dev.Read32(base+iommuStatus)&iommuPagingEnabled == 0 {
			continue
		}
		dev.Write32(base+iommuCommand, iommuCmdEnableStall)
		dev.MB()
		dev.Write32(base+iommuCommand, iommuCmdDisablePaging)
		dev.MB()
		dev.Write32(base+iommuCommand, iommuCmdDisableStall)
		dev.MB()
	}
}

// De 1080p60-timing, CEA VIC 16 uit drm_edid.c:
//
//	clock 148500 kHz, hdisplay 1920, hsync_start 2008, hsync_end 2052, htotal 2200
//	vdisplay 1080, vsync_start 1084, vsync_end 1089, vtotal 1125, +HSync +VSync
//
// De afgeleiden precies zoals vop2_crtc_atomic_enable ze rekent.
const (
	hDisplay = 1920
	hTotal   = 2200
	hSyncLen = 2052 - 2008       // 44
	hActSt   = hTotal - 2008     // 192
	hActEnd  = hActSt + hDisplay // 2112
	vDisplay = 1080
	vTotal   = 1125
	vSyncLen = 1089 - 1084       // 5
	vActSt   = vTotal - 1084     // 41
	vActEnd  = vActSt + vDisplay // 1121

	// pre_scan_max_dly[3] van VP0 (rk3568_vop_video_ports[0]); index 3 =
	// sdr2sdr volgens het comment in rockchip_vop2_reg.c.
	bgDly = 42

	// OUT_MODE: dw_hdmi-rockchip.c zet ROCKCHIP_OUT_MODE_AAAA, en VP0 heeft
	// VOP2_VP_FEATURE_OUTPUT_10BIT dus wordt niet teruggezet naar P888.
	outModeAAAA = 15

	// DSP_IF_POL: HDMI-pinpolariteit in [7:4]. 1080p60 is +HSync +VSync, en
	// vop_pol kent HSYNC_POSITIVE=0, VSYNC_POSITIVE=1 → veld 0x3.
	// CFG_DONE_IMD (bit 28) laat dit register buiten de cfg_done-latch om.
	hdmiPinPol         = 0x3
	cfgDoneIMD         = 1 << 28
	dspIfEnHDMI        = 1 << 1 // en HDMI_MUX [11:10] = vp.id = 0, dus geen bits
	layerSelRegDoneIMD = 1 << 28
)

// VOPScanout brengt de VOP2 op en start de scanout van de framebuffer naar
// VP0. Aanroepen ná PowerOnVO. De buffer (base/w/h/stride) is board-kennis —
// hij ligt in het PA-plan — en komt daarom als argument binnen (rk3566.FB()
// past exact op deze signatuur).
//
// ER IS BEWUST GEEN STOP-PAD. Dat is geen omissie maar een keuze: niets in HopOS
// zet het beeld ooit uit — de buffer blijft van de node, de fb-grant wisselt
// alleen wie erin tekent. Zou er ooit een stop moeten komen, dan is dit wat je
// moet weten en wat een lege functie hier nu níét mag suggereren dat al geregeld
// is: STANDBY (bit 31 van DSP_CTRL, als VOLLE write) gaat pas in aan het eind
// van het huidige frame, en wie daarna aclk uitzet vóórdat VP_INT_STATUS bit 6
// (dsp_hold) staat, kan de memory bus laten hangen.
//
// Wat deze functie NIET doet is een modus kiezen: 1920x1080p60 staat vast, want
// er is geen EDID-lezer (dat zit in de HDMI-kant) en de framebuffer in het plan
// heeft die maat. Een monitor die 1080p60 niet kan is buiten bereik van deze
// eerste versie.
func VOPScanout(base uintptr, w, h, stride int) error {
	if err := VOPPixelClock(); err != nil {
		return err
	}
	VOPIOMMUOff()

	// 1. Globale init (vop2_enable). OTP_WIN_EN is RK3566-only en de bron geeft
	//    de waarde zonder uitleg — hij staat er omdat de driver hem zet.
	dev.Write32(vopBase+vopOTPWinEn, 1)
	dev.Write32(vopBase+vopRegCfgDone, cfgDoneGlbEn)
	// Auto-gating UIT. De driver noemt dit een workaround: laat je hem aan, dan
	// SCHUIFT het beeld zodra een window aangaat — een symptoom dat je makkelijk
	// voor een timingfout aanziet.
	dev.Write32(vopBase+vopSysAutoGate, dev.Read32(vopBase+vopSysAutoGate)&^autoGateEn)
	// Bus-error-interrupt aan: onze goedkoopste diagnose als het
	// framebufferadres fout is (vop2_isr logt "BUS_ERROR irq err").
	for _, r := range []uintptr{vopSysInt0Clr, vopSysInt0En, vopSysInt1Clr, vopSysInt1En} {
		dev.Write32(vopBase+r, 0x0002_0002)
	}
	dev.MB()

	// 2. Interface-routing: HDMI aan, gevoed uit VP0 (mux-veld = 0).
	dev.Write32(vopBase+vopDSPIfEn, dev.Read32(vopBase+vopDSPIfEn)|dspIfEnHDMI)
	dev.Write32(vopBase+vopDSPIfPol, cfgDoneIMD|hdmiPinPol<<4)
	dev.MB()

	// 3. VP0-timing.
	dev.Write32(vopBase+vpDSPHTotalHSEnd, hTotal<<16|hSyncLen)
	dev.Write32(vopBase+vpDSPHActStEnd, hActSt<<16|hActEnd)
	dev.Write32(vopBase+vpDSPVTotalVSEnd, vTotal<<16|vSyncLen)
	dev.Write32(vopBase+vpDSPVActStEnd, vActSt<<16|vActEnd)
	dev.Write32(vopBase+vopVPLineFlag0, vActEnd<<16|vActEnd)
	dev.Write32(vopBase+vpMIPICtrl, 0)
	dev.MB()

	// 4. Overlay: één laag (Smart0, layer_sel_id 3) op VP0.
	//
	//    De ongebruikte lagen krijgen 0x5 en niet 0 — de driver zegt
	//    "configure unused layers to 0x5 (reserved)", en 0 is het layer_sel_id
	//    van Cluster0-win0: nullen laten staan zou een ongeconfigureerde
	//    cluster naar deze port routeren.
	dev.Write32(vopBase+vopOvlCtrl,
		dev.Read32(vopBase+vopOvlCtrl)&^1|layerSelRegDoneIMD)
	dev.Write32(vopBase+vopOvlLayerSel, 0x0055_5553)
	// PORT_SEL: Smart0 [29:28] = vp.id = 0; PORT0_MUX [3:0] = nlayers-1 = 5.
	// Dit is de stand die Linux op dít board zet (win_size 6, nvps 1) — de
	// waarde 5 is dus gemeten gedrag en geen extrapolatie.
	dev.Write32(vopBase+vopOvlPortSel, 0x0000_0885)
	dev.Write32(vopBase+vopSmartDlyNum, 20<<16) // SMART0-dly [23:16] = 20
	dev.Write32(vopBase+vopClusterDlyNum, 0)
	dev.Write32(vopBase+vopHDR0SrcColor, 0)
	dev.MB()

	// 5. Het window zelf. Formaat 0 = ARGB8888 (VOP2_FMT_ARGB8888), en dat is
	//    ook wat XRGB8888 oplevert — de alfabits negeert de hardware bij een
	//    bodemlaag zonder mixer.
	dev.Write32(vopBase+smCtrl0, 0)
	dev.Write32(vopBase+smCtrl1, 0)
	dev.Write32(vopBase+smRegion0MST, uint32(base))
	// VIR is in DWORDS. Schrijf je bytes, dan is de stride vier keer te groot en
	// zie je een vierde van het beeld uitgesmeerd.
	dev.Write32(vopBase+smRegion0VIR, uint32(stride/4))
	dev.Write32(vopBase+smRegion0Act, uint32(h-1)<<16|uint32(w-1))
	dev.Write32(vopBase+smRegion0DSP, uint32(h-1)<<16|uint32(w-1))
	dev.Write32(vopBase+smRegion0DSPSt, 0)
	// SCL: bron == doel, dus SCALE_NONE met de bilineaire filterstand die de
	// driver dan kiest (0x44 = beide filters op VOP2_SCALE_DOWN_BIL).
	dev.Write32(vopBase+smRegion0Scl, 0x44)
	dev.Write32(vopBase+smRegion0SclF, 0)
	dev.Write32(vopBase+smColorKeyCtrl, 0)
	dev.MB()
	// Enable als LAATSTE van dit blok — WIN0_EN ís de mute, er is geen apart
	// mute-bit.
	dev.Write32(vopBase+smRegion0Ctrl, winEnable)
	dev.MB()

	// 6. Post-config. De 1:1-factor is 0x1000 (4096) en niet 0 — vop2_post_config
	//    rekent scl_cal_scale2(a,a) = ((a-1)<<12)/(a-1).
	dev.Write32(vopBase+vopVPBGMixCtrl0, bgDly<<24)
	dev.Write32(vopBase+vpPreScanHTiming, (bgDly+hDisplay/2-1)<<16|hSyncLen)
	dev.Write32(vopBase+vpPostDSPHAct, hActSt<<16|hActEnd)
	dev.Write32(vopBase+vpPostDSPVAct, vActSt<<16|vActEnd)
	dev.Write32(vopBase+vpPostSclFactor, 0x1000_1000)
	dev.Write32(vopBase+vpPostSclCtrl, 0)
	dev.Write32(vopBase+vpDSPBG, 0)
	dev.MB()

	// 7. Latchen. De window-registers (0x1000..0x23FF) worden PAS hier geldig en
	//    lezen tot dat moment hun oude waarde terug — dus een window-write
	//    terugleggen vóór cfg_done liegt tegen je.
	//
	//    De write-mask uit de datasheet bestaat op dit silicium niet (de driver
	//    zegt het letterlijk: op rk3566/8 hebben die bits geen effect), dus
	//    schrijven we het bit gewoon.
	dev.Write32(vopBase+vopRegCfgDone, cfgDoneGlbEn|1)
	dev.MB()

	// 8. En dan STANDBY los: dit start de scan. DSP_CTRL wordt altijd volledig
	//    geschreven, nooit read-modify-write (zo doet de driver het ook).
	dev.Write32(vopBase+vpDSPCtrl, outModeAAAA)
	dev.MB()
	return nil
}

// VOPCfgDoneTaken pollt of de VOP2 de laatste configuratie heeft overgenomen:
// bit 0 van REG_CFG_DONE valt weg zodra de VP hem bij frame-start latcht (zo
// leest vop2_isr het ook). Eén frame is ~16,7ms; false betekent dat de VP niet
// scant.
func VOPCfgDoneTaken() bool {
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if dev.Read32(vopBase+vopRegCfgDone)&1 == 0 {
			return true
		}
	}
	return false
}

// VOPAlive zegt of de APB-kant van de VOP2 antwoordt, op basis van het
// VERSION_INFO-register.
//
// DIT IS DE TWEEDE VERSIE, en de eerste was een meetfout die me een boot kostte.
// Ik had een schrijf-lees-test op VP_DSP_BG gebouwd omdat de Linux-driver
// VERSION_INFO nooit leest en er dus "geen bewezen liveness-check" was. Die test
// faalde op ijzer (06-08) en ik brak de hele scanout erop af — terwijl het blok
// gewoon leefde: in dezelfde boot las VERSION_INFO **0x40158023**, een keurige
// waarde die noch nul noch alles-één is.
//
// Waarom die schrijf-lees-test loog weet ik niet zeker (VP-registers latchen
// vermoedelijk óók, of DSP_BG accepteert dat patroon niet), en dat hoeft ook
// niet meer: er is nu een gemeten, werkende check. De les die wél blijft: een
// meetinstrument dat ik zelf verzin moet ik net zo hard wantrouwen als de code
// die het meet — een vals negatief kost precies zo veel als een echte fout.
func VOPAlive() bool {
	v := dev.Read32(vopBase + 0x004)
	return v != 0 && v != 0xFFFFFFFF
}

// VOPWriteReadBack is de oude schrijf-lees-test, gedegradeerd tot DIAGNOSE: hij
// is geen poort meer, alleen een extra regel in de log. Zo blijft zichtbaar of
// VP-registers wél of niet direct terugleesbaar zijn zonder dat een onbegrepen
// uitslag de bring-up ophoudt.
func VOPWriteReadBack() bool {
	const pat = 0x00A5_5A00
	old := dev.Read32(vopBase + vpDSPBG)
	dev.Write32(vopBase+vpDSPBG, pat)
	dev.MB()
	got := dev.Read32(vopBase + vpDSPBG)
	dev.Write32(vopBase+vpDSPBG, old)
	dev.MB()
	return got == pat
}

// De muxen en delers boven de VOP2-klokken. GEMETEN NOODZAAK 06-08: met alleen
// de gates open bleef de APB-kant dood (schrijf-lees-test faalde) terwijl PD_VO
// aantoonbaar aan was en uit idle. Een open gate is niets waard als de mux
// erboven naar een bron wijst die niet loopt, of als de deler nul is.
//
// De veldindelingen zijn uit clk-rk3568.c; de RATES zijn een keuze:
//
//	ACLK_VO      CLKSEL_CON(37) [1:0] over {gpll_300m, cpll_250m, gpll_100m, xin24m}
//	HCLK_VO      CLKSEL_CON(37) [11:8]  deler op aclk_vo
//	PCLK_VO      CLKSEL_CON(37) [15:12] deler op aclk_vo
//	ACLK_VOP_PRE CLKSEL_CON(38) [7:6] over {cpll, gpll, hpll, vpll}, deler [4:0]
//
// Delervelden zijn "waarde + 1". 300MHz voor de AXI-kant is ruim voor één
// 1080p32-scanout (~475MB/s) en het is de bron die de dtsi zelf als eerste
// noemt.
const (
	cruCLKSEL37 = 0x100 + 37*4 // = 0x194
	cruCLKSEL38 = 0x100 + 38*4 // = 0x198

	aclkVOMuxGPLL300   = 0 // gpll_300m
	hclkVODivField     = 1 // ÷2 → 150MHz
	pclkVODivField     = 3 // ÷4 → 75MHz
	aclkVOPPreMuxGPLL  = 1 // gpll (1200MHz)
	aclkVOPPreDivField = 3 // ÷4 → 300MHz
)

// VOPClockTree zet de muxen en delers boven de VOP2-gates. Apart van
// VOPClockOn zodat het meetinstrument het verschil kan tonen: eerst met alleen
// de gates, dan met de hele boom.
func VOPClockTree() {
	dev.Write32(cruBase+cruCLKSEL37,
		hiword(aclkVOMuxGPLL300, 0x3, 0)|
			hiword(hclkVODivField, 0xF, 8)|
			hiword(pclkVODivField, 0xF, 12))
	dev.Write32(cruBase+cruCLKSEL38,
		hiword(aclkVOPPreMuxGPLL, 0x3, 6)|
			hiword(aclkVOPPreDivField, 0x1F, 0))
	dev.MB()
}

// VOPClockInfo geeft de klokregisters plus twee rauwe VOP2-reads terug. Die
// twee laatste scheiden "alles nul" (blok niet geklokt) van "alles één" (dode
// bus) van iets ertussenin.
func VOPClockInfo() (gate20, sel37, sel38, sel39, raw0, raw4 uint32) {
	return dev.Read32(cruBase + cruCLKGATE20),
		dev.Read32(cruBase + cruCLKSEL37),
		dev.Read32(cruBase + cruCLKSEL38),
		dev.Read32(cruBase + cruCLKSEL39),
		dev.Read32(vopBase + 0x000),
		dev.Read32(vopBase + 0x004) // VERSION_INFO — Linux leest hem nooit, wij wel
}

// VOPInfo geeft de registers die een mislukte bring-up moeten kunnen ontleden.
func VOPInfo() (dspCtrl, cfgDone, ifEn, ifPol, winCtrl, winMST, intSts uint32) {
	return dev.Read32(vopBase + vpDSPCtrl),
		dev.Read32(vopBase + vopRegCfgDone),
		dev.Read32(vopBase + vopDSPIfEn),
		dev.Read32(vopBase + vopDSPIfPol),
		dev.Read32(vopBase + smRegion0Ctrl),
		dev.Read32(vopBase + smRegion0MST),
		dev.Read32(vopBase + vpIntStatus)
}
