package rkscan

import (
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// De HDMI-transmitter van de RK3566: een Synopsys DesignWare HDMI-TX, gevoed
// door VOP2's Video Port 0. Samen met vop2.go is dit het pad van de framebuffer
// in het PA-plan naar de connector.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13
// drivers/gpu/drm/bridge/synopsys/dw-hdmi.c en dw-hdmi.h (registers, de
// init-volgorde, de PHY-I2C-master), drivers/gpu/drm/rockchip/dw_hdmi-rockchip.c
// (de PHY-tabellen voor dít silicium), rk356x-base.dtsi (adres, reg-io-width,
// klokken) en drm_edid.c voor de 1080p60-timing.
//
// DRIE DINGEN DIE DE VORM VAN DIT BESTAND BEPALEN:
//
//  1. **reg-io-width = 4.** Het dtsi zegt het en de driver rekent
//     `offset << 2`. Elk offset uit dw-hdmi.h moet dus maal vier: FC_INVIDCONF
//     staat niet op +0x1000 maar op +0x4000. Er is een tweede bevestiging:
//     max_register = 0x7E12<<2 = 0x1F848 past net in het 0x20000-venster uit de
//     DTS. Wie deze factor mist schrijft ergens anders in het blok en krijgt
//     geen enkele foutmelding.
//  2. **DVI-mode, niet HDMI-mode.** Eén bit verschil (FC_INVIDCONF bit 3), maar
//     het scheelt de hele infoframe-, audio- en HDCP-tak: ~45 registers in
//     plaats van een veelvoud. Een monitor synct hierop — dat is precies wat een
//     DVI→HDMI-verloopje ook doet; de sink leidt het formaat af uit de timings.
//     Wat je misloopt is VIC/aspect/quantisatie-signalering, wat op sommige
//     tv's overscan of limited-range kan geven. Voor een monitor is dat geen
//     probleem, en AVI-infoframes zijn later ~15 registers.
//  3. **Er is GEEN aparte PHY-driver.** Anders dan op de RK3228/RK3328 (die de
//     Innosilicon-PHY hebben) gebruikt de RK3566 de INTERNE Synopsys-PHY: geen
//     phys-property in het dtsi, geen phy_ops in de Rockchip-glue, en de
//     configuratie loopt via een PHY-I2C-master ín dit registerblok.
//
// WAT DE BRON NIET ZEGT, en waar dus gemeten moet worden:
//
//   - Welke PHY-variant dit silicium heeft. CONFIG2_ID moet je LEZEN: alleen de
//     types 0xb2, 0xc2 en 0xf3 hebben SVSRET, en die bit verkeerd zetten kan
//     betekenen dat de PLL niet lockt. HDMIPhyHasSVSRET doet die lezing.
//   - De verwachte DESIGN_ID/REVISION_ID voor dit SoC. De driver noemt RK3288 en
//     RK3328/RK3399 bij naam, de RK3568 niet. We printen ze dus in plaats van
//     erop te controleren.
//   - De reset-waarden van registers die de DVI-pad nooit schrijft (FC_PRCONF,
//     FC_GCP, de AVICONF's). Indirect bewijs dat dat goed gaat: Linux geeft
//     beeld op DVI-sinks zonder ze aan te raken.
const (
	hdmiBase = 0xFE0A0000 // rk356x-base.dtsi: hdmi@fe0a0000, 0x20000 groot

	// Identificatie — lezen vóór je één configuratieregister schrijft.
	hdDesignID  = 0x0000
	hdRevID     = 0x0001
	hdProdID0   = 0x0002 // moet 0xA0 zijn
	hdProdID1   = 0x0003 // (& ~0xC0) moet 0x01 zijn
	hdConfig2ID = 0x0006 // het PHY-type; LEZEN, niet gokken

	// Interrupt-maskering. Wij pollen, dus alles blijft gemute — op twee
	// uitzonderingen na (zie hdmiInitHW).
	hdIHI2CMPhyStat0 = 0x0108
	hdIHMuteFCStat2  = 0x0182
	hdIHMuteBase     = 0x0180 // tien registers, 0x0180..0x0189
	hdIHMute         = 0x01FF

	// Video sample (de ingang van de FIFO).
	hdTXInvid0     = 0x0200
	hdTXInstuffing = 0x0201
	hdTXGYData0    = 0x0202 // zes datastuffing-registers, 0x0202..0x0207

	// Video packetizer.
	hdVPPrCd  = 0x0801
	hdVPStuff = 0x0802
	hdVPRemap = 0x0803
	hdVPConf  = 0x0804
	hdVPMask  = 0x0807

	// Frame composer: timings en de DVI/HDMI-keuze.
	hdFCInvidconf     = 0x1000
	hdFCInhactv0      = 0x1001
	hdFCInhactv1      = 0x1002
	hdFCInhblank0     = 0x1003
	hdFCInhblank1     = 0x1004
	hdFCInvactv0      = 0x1005
	hdFCInvactv1      = 0x1006
	hdFCInvblank      = 0x1007
	hdFCHsyncindelay0 = 0x1008
	hdFCHsyncindelay1 = 0x1009
	hdFCHsyncinwidth0 = 0x100A
	hdFCHsyncinwidth1 = 0x100B
	hdFCVsyncindelay  = 0x100C
	hdFCVsyncinwidth  = 0x100D
	hdFCCtrldur       = 0x1011
	hdFCExctrldur     = 0x1012
	hdFCExctrlspac    = 0x1013
	hdFCCh0pream      = 0x1014
	hdFCCh1pream      = 0x1015
	hdFCCh2pream      = 0x1016
	hdFCDatauto3      = 0x10B7
	hdFCMask0         = 0x10D2
	hdFCMask1         = 0x10D6
	hdFCMask2         = 0x10DA

	// PHY.
	hdPhyConf0 = 0x3000
	hdPhyTst0  = 0x3001
	hdPhyStat0 = 0x3004
	hdPhyMask0 = 0x3006

	// De PHY-I2C-master: hiermee programmeer je de PHY-registers.
	hdPhyI2CSlave     = 0x3020
	hdPhyI2CAddress   = 0x3021
	hdPhyI2CDataO1    = 0x3022
	hdPhyI2CDataO0    = 0x3023
	hdPhyI2COperation = 0x3026
	hdPhyI2CInt       = 0x3027
	hdPhyI2CCtlint    = 0x3028

	// Audio-klokregeneratie. Met audio uit horen deze op nul te staan, en dat is
	// géén cosmetica: de driver zet ze expliciet vóór de PHY aangaat "to prevent
	// overflows in HDMI_IH_FC_STAT2".
	hdAudN1 = 0x3200 // zes registers, 0x3200..0x3205 (N1..N3, CTS1..CTS3)

	// Main controller: klokken, resets, flowcontrol.
	hdMCClkdis     = 0x4001
	hdMCSwrstz     = 0x4002
	hdMCFlowctrl   = 0x4004
	hdMCPhyrstz    = 0x4005
	hdMCHeacphyRst = 0x4007

	// HDCP — niet gebruiken, maar wél in de juiste stand zetten.
	hdAHdcpcfg0  = 0x5000
	hdAHdcpcfg1  = 0x5001
	hdAVidpolcfg = 0x5009
)

// Bitwaarden, alle uit dw-hdmi.h.
const (
	prodID0HDMITX = 0xA0
	prodID1HDCP   = 0xC0
	prodID1Value  = 0x01

	phyConf0SVSRET       = 0x20
	phyConf0Gen2PDDQ     = 0x10
	phyConf0Gen2TXPwrOn  = 0x08
	phyConf0SelDataEnPol = 0x02
	phyConf0SelDIPIF     = 0x01
	phyTst0TSTCLR        = 0x20

	phyStat0TXPhyLock = 0x01
	phyStat0HPD       = 0x02

	phyI2CSlaveGen2 = 0x69
	phyI2COpWrite   = 0x10

	mcPhyrstz          = 0x01 // op Gen2 ACTIEF HOOG: 1 dan 0
	mcHeacphyRstAssert = 0x01
	mcSwrstzTMDS       = 0xFD // ~TMDSSWRST_REQ

	fcDatauto3GCPAuto = 0x04

	aHdcpcfg0RXDetect       = 0x04
	aHdcpcfg1EncryptDisable = 0x02
	aVidpolcfgDataEnPolMask = 0x10
	aVidpolcfgDataEnPolHigh = 0x10

	// De PHY-registers ín de PHY (via de I2C-master), met de waarden die bij
	// 148,5MHz horen. De selectie is "eerste tabelregel met mpixelclock >=
	// 148500000" uit dw_hdmi-rockchip.c: mpll-regel 184MHz, cur_ctr-regel
	// 600MHz, phy_config-regel 165MHz.
	phyRegCPCECtrl    = 0x06
	phyRegGMPCtrl     = 0x15
	phyRegCurrCtrl    = 0x10
	phyRegPLLPhbyCtrl = 0x13
	phyRegMSMCtrl     = 0x17
	phyRegTXTerm      = 0x19
	phyRegCKSymTXCtrl = 0x09
	phyRegVLevCtrl    = 0x0E
	phyRegCKCalCtrl   = 0x05

	phyValCPCE    = 0x0051
	phyValGMP     = 0x0002
	phyValCurr    = 0x0000
	phyValPLLPhby = 0x0000
	phyValMSM     = 0x0006 // CKO_SEL_FB_CLK
	phyValTXTerm  = 0x0004
	phyValCKSym   = 0x802B
	phyValVLev    = 0x0209
	phyValCKCal   = 0x8000 // OVERRIDE

	// De timings van 1080p60 zoals de FRAME COMPOSER ze wil. Let op: dit is een
	// ándere conventie dan VOP2 gebruikt — VOP2 rekent hact_st = htotal −
	// hsync_start (192), de FC rekent h_de_hs = hsync_start − hdisplay (88).
	// Twee verschillende getallen uit dezelfde modus; verwisselen ze en er komt
	// niets bruikbaars uit.
	fcHBlank = hTotal - hDisplay // 280
	fcVBlank = vTotal - vDisplay // 45
	fcHDeHs  = 2008 - hDisplay   // 88
	fcVDeVs  = 1084 - vDisplay   // 4

	// FC_INVIDCONF voor DVI: VSYNC_HIGH|HSYNC_HIGH|DE_HIGH, progressief, en
	// DVI_MODE = 0 (HDMI-mode zou 0x08 erbij zetten).
	fcInvidconfDVI = 0x40 | 0x20 | 0x10

	// MC_CLKDIS: bit per klok, 1 = uit. 0x7E laat alleen de pixelklok lopen,
	// 0x7C zet de TMDS-klok erbij. TWEE APARTE WRITES — de driver doet dat
	// expliciet gescheiden, dus wij ook.
	mcClkdisPixelOnly = 0x7E
	mcClkdisPixelTMDS = 0x7C
)

// De klokken van het HDMI-blok (rk356x-base.dtsi: clock-names "iahb", "isfr",
// "cec", "ref" — plus een vijfde, NAAMLOZE phandle naar HCLK_VO die dus nooit
// "by name" wordt opgevraagd en via het power-domein binnenkomt; die zet
// VOPClockOn al).
//
// cec laten we dicht: geen CEC, en MC_CLKDIS houdt die klok in de video-only
// pad toch gemute.
const (
	cruCLKGATE21     = 0x300 + 21*4 // = 0x354
	gatePCLKHDMIHost = 3            // "iahb"
	gateCLKHDMISFR   = 4            // "isfr"
)

// HDMIClockOn opent de twee klokken die het registerblok nodig heeft. Zonder
// iahb lezen álle registers 0x00 of 0xFF — en dat is precies waarom HDMIIDs
// vóór elke schrijfactie hoort te lopen.
func HDMIClockOn() {
	dev.Write32(cruBase+cruCLKGATE21,
		hiword(0, 1, gatePCLKHDMIHost)|hiword(0, 1, gateCLKHDMISFR))
	dev.MB()
}

func hdrd(off uintptr) uint32    { return dev.Read32(hdmiBase + off<<2) }
func hdwr(off uintptr, v uint32) { dev.Write32(hdmiBase+off<<2, v&0xFF) }
func hdmod(off uintptr, mask, val uint32) {
	hdwr(off, hdrd(off)&^mask|val&mask)
}

// HDMIIDs geeft de vier identificatieregisters plus het PHY-type. LEES DIT
// EERST, vóór er één configuratieregister geschreven wordt: in één boot bewijst
// het het basisadres, de ×4-stap, de klokken en het power-domein. Leest alles
// 0x00 of 0xFF, dan is er iets mis onder de driver en heeft configureren geen
// zin.
func HDMIIDs() (design, rev, prod0, prod1, config2 uint32, ok bool) {
	design, rev = hdrd(hdDesignID), hdrd(hdRevID)
	prod0, prod1 = hdrd(hdProdID0), hdrd(hdProdID1)
	config2 = hdrd(hdConfig2ID)
	// Exact de controle die dw_hdmi_probe zelf doet.
	ok = prod0 == prodID0HDMITX && prod1&^prodID1HDCP == prodID1Value
	return
}

// HDMIPhyHasSVSRET zegt of deze PHY-variant de SVSRET-bit heeft. Uit
// dw_hdmi_phys[]: alleen 0xb2, 0xc2 en 0xf3. Gemeten in plaats van gegokt,
// want die bit verkeerd zetten kan betekenen dat de PLL niet lockt.
func HDMIPhyHasSVSRET() bool {
	switch hdrd(hdConfig2ID) {
	case 0xB2, 0xC2, 0xF3:
		return true
	}
	return false
}

// hdmiInitHW is de eenmalige blok-init: alles gemute, en de audio-teller op nul.
func hdmiInitHW() {
	hdwr(hdIHMute, 0x03) // MUTE_WAKEUP | MUTE_ALL
	hdwr(hdVPMask, 0xFF)
	hdwr(hdFCMask0, 0xFF)
	hdwr(hdFCMask1, 0xFF)
	hdwr(hdFCMask2, 0xFF)
	hdwr(hdPhyMask0, 0xFF)
	for i := uintptr(0); i < 10; i++ {
		hdwr(hdIHMuteBase+i, 0xFF)
	}
	hdwr(hdIHMuteFCStat2, 0x03) // de overflow-interrupts

	// EN NU DE VALKUIL waar een hele boot-cyclus in kan verdwijnen: de
	// mute-ronde hierboven zet PHY_I2CM_INT op 0xFF (masker AAN), maar de
	// I2C-init in de driver zet hem daarna op 0x08 — DONE_POL, met DONE_MASK
	// GEWIST. Laat je dat weg, dan blijft IH_I2CMPHY_STAT0 nul en loopt ELKE
	// PHY-write in een timeout terwijl er niets kapot is.
	hdwr(hdPhyI2CInt, 0x08)
	hdwr(hdPhyI2CCtlint, 0x88) // NAC_POL | ARBITRATION_POL

	// De klokregeneratie op nul (audio uit). De driver doet dit vóór de PHY
	// aangaat, met de reden erbij: anders overloopt IH_FC_STAT2.
	for i := uintptr(0); i < 6; i++ {
		hdwr(hdAudN1+i, 0)
	}
	dev.MB()
}

// phyI2CWrite schrijft één 16-bits PHY-register via de I2C-master in dit blok.
func phyI2CWrite(addr uintptr, data uint32) bool {
	hdwr(hdIHI2CMPhyStat0, 0xFF) // status wissen
	hdwr(hdPhyI2CAddress, uint32(addr))
	hdwr(hdPhyI2CDataO1, data>>8)
	hdwr(hdPhyI2CDataO0, data&0xFF)
	hdwr(hdPhyI2COperation, phyI2COpWrite)
	dev.MB()

	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if st := hdrd(hdIHI2CMPhyStat0); st&0x03 != 0 {
			hdwr(hdIHI2CMPhyStat0, st) // de gezette bits terugschrijven
			return true
		}
	}
	return false
}

// hdmiPhyConfigure doet één ronde van de PHY-sequentie. DE HELE SEQUENTIE MOET
// TWEE KEER — de driver zegt het letterlijk: "HDMI Phy spec says to do the phy
// initialization sequence twice". Eén keer werkt soms, en dat is erger dan
// nooit werken.
func hdmiPhyConfigure(svsret bool) bool {
	// Deze twee horen per ronde, en vóór de PHY-configuratie.
	hdmod(hdPhyConf0, phyConf0SelDataEnPol, phyConf0SelDataEnPol)
	hdmod(hdPhyConf0, phyConf0SelDIPIF, 0)

	// Power off: TXPWRON uit, wachten tot LOCK weg is, dan PDDQ aan.
	hdmod(hdPhyConf0, phyConf0Gen2TXPwrOn, 0)
	deadline := time.Now().Add(20 * time.Millisecond)
	for hdrd(hdPhyStat0)&phyStat0TXPhyLock != 0 && time.Now().Before(deadline) {
	}
	hdmod(hdPhyConf0, phyConf0Gen2PDDQ, phyConf0Gen2PDDQ)

	if svsret {
		hdmod(hdPhyConf0, phyConf0SVSRET, phyConf0SVSRET)
	}

	// Gen2-reset: ACTIEF HOOG, dus 1 dan 0. Op Gen1 is het omgekeerd, en die
	// polariteit verwisselen betekent dat de PHY níet reset en met oude
	// MPLL-instellingen lockt — of niet lockt.
	hdwr(hdMCPhyrstz, mcPhyrstz)
	hdwr(hdMCPhyrstz, 0)
	hdwr(hdMCHeacphyRst, mcHeacphyRstAssert)

	// De I2C-master op de PHY zetten.
	hdmod(hdPhyTst0, phyTst0TSTCLR, phyTst0TSTCLR)
	hdwr(hdPhyI2CSlave, phyI2CSlaveGen2)
	hdmod(hdPhyTst0, phyTst0TSTCLR, 0)

	// De negen PHY-registers voor 148,5MHz.
	for _, w := range []struct {
		addr uintptr
		val  uint32
	}{
		{phyRegCPCECtrl, phyValCPCE},
		{phyRegGMPCtrl, phyValGMP},
		{phyRegCurrCtrl, phyValCurr},
		{phyRegPLLPhbyCtrl, phyValPLLPhby},
		{phyRegMSMCtrl, phyValMSM},
		{phyRegTXTerm, phyValTXTerm},
		{phyRegCKSymTXCtrl, phyValCKSym},
		{phyRegVLevCtrl, phyValVLev},
		{phyRegCKCalCtrl, phyValCKCal},
	} {
		if !phyI2CWrite(w.addr, w.val) {
			return false
		}
	}

	// Power on: TXPWRON aan, PDDQ uit, wachten op LOCK.
	hdmod(hdPhyConf0, phyConf0Gen2TXPwrOn, phyConf0Gen2TXPwrOn)
	hdmod(hdPhyConf0, phyConf0Gen2PDDQ, 0)
	dev.MB()

	deadline = time.Now().Add(20 * time.Millisecond)
	for time.Now().Before(deadline) {
		if hdrd(hdPhyStat0)&phyStat0TXPhyLock != 0 {
			return true
		}
	}
	return false
}

// HDMIEnable brengt de transmitter op in DVI-mode voor 1920x1080p60 RGB.
// Aanroepen NÁ VOPScanout: als de PHY aangaat zonder dat VOP2 pixels en een
// dclk levert, staat de frame composer tegen een stilstaande klok geprogrammeerd.
// De DRM-ordening (eerst alle CRTC's, dan de bridges) is niet toevallig.
func HDMIEnable() error {
	HDMIClockOn()
	if _, _, p0, p1, _, ok := HDMIIDs(); !ok {
		return &pmuError{what: "HDMI controller id", off: hdProdID0 << 2,
			got: p0<<8 | p1, mask: 0xFFFF, want: prodID0HDMITX<<8 | prodID1Value}
	}

	hdmiInitHW()

	// 1. Frame composer: timings, polariteiten, DVI-mode.
	hdwr(hdFCInvidconf, fcInvidconfDVI)
	hdwr(hdFCInhactv1, hDisplay>>8)
	hdwr(hdFCInhactv0, hDisplay&0xFF)
	hdwr(hdFCInvactv1, vDisplay>>8)
	hdwr(hdFCInvactv0, vDisplay&0xFF)
	hdwr(hdFCInhblank1, fcHBlank>>8)
	hdwr(hdFCInhblank0, fcHBlank&0xFF)
	hdwr(hdFCInvblank, fcVBlank)
	hdwr(hdFCHsyncindelay1, fcHDeHs>>8)
	hdwr(hdFCHsyncindelay0, fcHDeHs&0xFF)
	hdwr(hdFCVsyncindelay, fcVDeVs)
	hdwr(hdFCHsyncinwidth1, hSyncLen>>8)
	hdwr(hdFCHsyncinwidth0, hSyncLen&0xFF)
	hdwr(hdFCVsyncinwidth, vSyncLen)
	dev.MB()

	// 2. De PHY, twee keer.
	svsret := HDMIPhyHasSVSRET()
	if !hdmiPhyConfigure(svsret) || !hdmiPhyConfigure(svsret) {
		return &pmuError{what: "HDMI PHY lock", off: hdPhyStat0 << 2,
			got: hdrd(hdPhyStat0), mask: phyStat0TXPhyLock, want: phyStat0TXPhyLock}
	}

	// 3. Video-pad aan.
	hdwr(hdFCCtrldur, 12)
	hdwr(hdFCExctrldur, 32)
	hdwr(hdFCExctrlspac, 1)
	hdwr(hdFCCh0pream, 0x0B)
	hdwr(hdFCCh1pream, 0x16)
	hdwr(hdFCCh2pream, 0x21)
	// Twee aparte writes, in deze volgorde: eerst alleen de pixelklok, dan de
	// TMDS-klok erbij. De driver splitst dit expliciet.
	hdwr(hdMCClkdis, mcClkdisPixelOnly)
	hdwr(hdMCClkdis, mcClkdisPixelTMDS)
	hdwr(hdMCFlowctrl, 0) // CSC bypass — RGB in, RGB uit
	dev.MB()

	// 4. Packetizer, 8-bit RGB. De eindwaarden staan vast; de driver komt er via
	//    vier read-modify-writes op uit.
	hdwr(hdVPPrCd, 0x40) // color_depth 4 (8-bit), geen pixel repetition
	hdmod(hdFCDatauto3, fcDatauto3GCPAuto, 0)
	hdwr(hdVPStuff, 0x27)
	hdwr(hdVPRemap, 0x00)
	hdwr(hdVPConf, 0x47) // bypass aan, output = bypass
	dev.MB()

	// 5. Video sample: RGB888 op de ingang.
	hdwr(hdTXInvid0, 0x01) // video_mapping 1 = RGB888_1X24, DE-generator uit
	hdwr(hdTXInstuffing, 0x07)
	for i := uintptr(0); i < 6; i++ {
		hdwr(hdTXGYData0+i, 0)
	}
	dev.MB()

	// 6. HDCP in de juiste stand: detectie uit, encryptie uit. Let op dat de
	//    DVI/HDMI-keuze NIET hier zit (bit 0 van A_HDCPCFG0 raakt de driver
	//    nooit aan) maar in FC_INVIDCONF.
	hdmod(hdAHdcpcfg0, aHdcpcfg0RXDetect, 0)
	hdmod(hdAVidpolcfg, aVidpolcfgDataEnPolMask, aVidpolcfgDataEnPolHigh)
	hdmod(hdAHdcpcfg1, aHdcpcfg1EncryptDisable, aHdcpcfg1EncryptDisable)
	dev.MB()

	// 7. De overflow-workaround, en die is NIET optioneel: dit is de klassieke
	//    "geen beeld ondanks perfecte registers". De FC-rekeneenheid kan een
	//    register-write missen, en de fix is een TMDS-softreset plus FC_INVIDCONF
	//    opnieuw schrijven.
	hdwr(hdMCSwrstz, mcSwrstzTMDS)
	hdwr(hdFCInvidconf, hdrd(hdFCInvidconf))
	dev.MB()
	return nil
}

// HDMIHotplug zegt of er een sink aan de kabel hangt (PHY_STAT0 bit 1). Geen eis
// om beeld te sturen, maar wel een gratis signaal dat er een kabel in zit.
func HDMIHotplug() bool { return hdrd(hdPhyStat0)&phyStat0HPD != 0 }

// HDMIInfo geeft de registers die een mislukte bring-up moeten kunnen ontleden.
func HDMIInfo() (phyStat, phyConf, clkdis, invidconf, vpConf uint32) {
	return hdrd(hdPhyStat0), hdrd(hdPhyConf0), hdrd(hdMCClkdis),
		hdrd(hdFCInvidconf), hdrd(hdVPConf)
}
