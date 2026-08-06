package rk3566

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De temperatuursensor (TSADC) van de RK3566. Twee kanalen: 0 = CPU, 1 = GPU.
//
// WAAROM DIT BLOK EERST KOMT en niet een DVFS-driver: Derek vermoedt (05-08)
// dat dit bordje onder last terugklokt moet worden. Dat kán waar zijn — het is
// een A55-cluster in een creditcard-formaat zonder koellichaam — maar het is een
// vermoeden, en een DVFS-driver bouwen op een vermoeden is precies de omgekeerde
// volgorde. Deze sensor maakt er een getal van. Wordt het bord onder de
// glaslast niet warm, dan is terugklokken een oplossing voor een probleem dat
// niet bestaat; wordt het wél warm, dan weten we hóé warm en hoe snel.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13 drivers/thermal/rockchip_thermal.c
// (de rk3568-tak: rk_tsadcv7_initialize, rk3568_code_table,
// rk_tsadcv2_code_to_temp) plus rk356x-base.dtsi voor adres, klokken, resets en
// de gevraagde kloksnelheden.
const (
	tsadcBase = 0xFE710000 // rk356x-base.dtsi: tsadc@fe710000
	grfTSADC  = 0x0600     // RK3568_GRF_TSADC_CON, in de SYS-GRF

	tsUserCon      = tsadcBase + 0x00 // interleave-timing
	tsAutoCon      = tsadcBase + 0x04
	tsIntEn        = tsadcBase + 0x08
	tsData0        = tsadcBase + 0x20 // + kanaal*4
	tsIntDebounce  = tsadcBase + 0x60
	tsShutDebounce = tsadcBase + 0x64
	tsAutoPeriod   = tsadcBase + 0x68
	tsAutoPeriodHT = tsadcBase + 0x6C

	tsAutoEn    = 1 << 0 // conversie loopt
	tsQSelEn    = 1 << 1 // hoort bij de code-tabel hieronder
	tsDataMask  = 0xFFF  // 12 bits
	tsUserConV5 = 0xFC0  // "97us, at least 90us" bij ~700kHz

	// Twee onafhankelijke constanten uit de driver bevestigen dat clk_tsadc
	// écht ~700kHz is, en dat is de reden dat de delerkeuze hieronder klopt:
	// 0xFC0 = 63<<6 en 63 tikken bij 700kHz = 90µs (het commentaar zegt 97µs),
	// en AUTO_PERIOD 1622 heet "2.5ms" wat bij 700kHz 2,32ms is.
	tsAutoPeriodVal = 1622
	tsDebounceVal   = 4

	// Klokgate CLKGATE_CON(26): bit 4 = pclk, 5 = tsen, 6 = tsadc.
	gatePCLKTSADC = 4
	gateTSADCTsen = 5
	gateTSADC     = 6

	// Delers in CLKSEL_CON(51). De DTS vraagt 17MHz voor tsen en 700kHz voor
	// tsadc; de bron noemt die RATES en niet de veldwaarden, dus dit is de enige
	// AFGELEIDE stap in dit bestand — expliciet zo benoemd:
	//
	//	tsen: mux [5:4] = 1 (gpll_100m = GPLL/12 = 100MHz), deler [2:0] = 5
	//	      → ÷6 → 16,667MHz (de beste stand ónder de gevraagde 17MHz)
	//	tsadc: deler [14:8] = 23 → ÷24 → 694,4kHz uit die 16,667MHz
	//
	// Deler-velden zijn "waarde + 1". De registers zijn hiword-masked
	// (clk-rk3568.c: DFLAGS/MFLAGS = CLK_DIVIDER/MUX_HIWORD_MASK), dus deze
	// schrijfactie raakt niets anders in CLKSEL_CON(51).
	cruCLKSEL51    = 0x100 + 51*4 // = 0x1CC
	tsenMuxGPLL100 = 1
	tsenDivField   = 5
	tsadcDivField  = 23

	// Resets: de DTS noemt alle drie als array, en rk_tsadcv7 pulst ze samen.
	//	SRST_P_TSADC   = 385 → bank 24, bit 1
	//	SRST_TSADC     = 386 → bank 24, bit 2
	//	SRST_TSADCPHY  = 471 → bank 29, bit 7
	cruSOFTRST24 = 0x400 + 24*4 // = 0x460
	cruSOFTRST29 = 0x400 + 29*4 // = 0x474
	srstPTSADC   = 1
	srstTSADC    = 2
	srstTSADCPHY = 7

	// De analoge voorkant zit in de GRF, niet in het blok zelf: eerst de
	// sensor-enable, wachten, dan drie ana_reg-bits, en dan lang genoeg wachten
	// dat de analoge kant is ingekomen. De TRM eist ≥10µs resp. ≥90µs; de
	// driver neemt 15µs en 100-200µs, en die ruimere marge nemen wij over —
	// dit pad loopt één keer per boot, dus zuinig zijn kost hier niets en
	// levert niets.
	grfTsenEn  = 8
	grfAnaReg0 = 0
	grfAnaReg1 = 1
	grfAnaReg2 = 2
)

// TempSetTsenParent zet de bron van de sensorklok: 0 = xin24m, 1 = gpll_100m,
// 2 = cpll_100m. De DTS kiest impliciet gpll_100m (dat is de enige parent die
// 17MHz kan halen), maar GEMETEN 06-08: met die stand doet de sensor niets
// terwijl de delers en de GRF-bits aantoonbaar landden. Dan is de vraag of de
// PARENT zelf loopt, en xin24m is de enige bron waarvan dat gegarandeerd is —
// die voedt de hele bootketen.
//
// De prijs van xin24m is dat de tijdconstanten iets verschuiven (24MHz i.p.v.
// 16,7MHz aan de voorkant, dus ~1MHz i.p.v. 700kHz voor de conversie): de
// AUTO_PERIOD komt dan op ~1,6ms uit in plaats van 2,3ms. Voor een eerste
// temperatuurlezing is dat ruim goed genoeg — de codetabel hangt aan de analoge
// kant, niet aan de sampleperiode.
func TempSetTsenParent(mux uint32) {
	dev.Write32(CRUBase+cruCLKSEL51, hiword(mux, 0x3, 4))
	dev.MB()
}

// TempClockOn opent de drie klokgates, zet de delers en pulst de drie resets.
// De reset zet ÁLLE TSADC-registers terug, dus dit hoort vóór TempInit en niet
// erna.
func TempClockOn() { TempClockOnOrder(false) }

// TempClockOnOrder is TempClockOn met de DEASSERT-VOLGORDE als knop.
//
// Waarom dat een knop verdient. Dit blok heeft drie resets (rk356x-base.dtsi:
// SRST_P_TSADC, SRST_TSADC, SRST_TSADCPHY — ids 385/386/471, dus bank 24 bit
// 1/2 en bank 29 bit 7, geverifieerd tegen rk3568-cru.h). De Linux-driver
// gebruikt reset_control_ARRAY en laat de volgorde daarmee aan het framework;
// de bron zégt dus nergens wie er eerst uit reset moet. Wij kozen: eerst de
// digitale kant, dan de PHY.
//
// En precies die keuze is verdacht, want op DIT silicium hebben we de
// omgekeerde les al een keer betaald: bij de GMAC kwam de MAC-softreset niet
// door zolang de PHY nog in reset stond — de referentieklok van de MAC kómt uit
// die PHY. Een analoge voorkant die nog in reset zit terwijl de digitale kant
// al begint te converteren, verklaart precies wat we meten: een stabiele,
// permanente 0 op beide kanalen, met alle registers en delers correct.
//
// phyFirst=true haalt de PHY er als eerste uit. Nog niet de default: eerst
// meten (cmd/proberk3566 probeert beide standen in één boot).
func TempClockOnOrder(phyFirst bool) {
	dev.Write32(CRUBase+cruCLKGATE26,
		hiword(0, 1, gatePCLKTSADC)|hiword(0, 1, gateTSADCTsen)|hiword(0, 1, gateTSADC))
	dev.Write32(CRUBase+cruCLKSEL51,
		hiword(tsenMuxGPLL100, 0x3, 4)|
			hiword(tsenDivField, 0x7, 0)|
			hiword(tsadcDivField, 0x7F, 8))
	dev.MB()

	dev.Write32(CRUBase+cruSOFTRST24, hiword(1, 1, srstPTSADC)|hiword(1, 1, srstTSADC))
	dev.Write32(CRUBase+cruSOFTRST29, hiword(1, 1, srstTSADCPHY))
	dev.MB()
	time.Sleep(20 * time.Microsecond)
	if phyFirst {
		dev.Write32(CRUBase+cruSOFTRST29, hiword(0, 1, srstTSADCPHY))
		dev.MB()
		// Even laten aanslaan vóór de digitale kant erop gaat kijken —
		// dezelfde marge die de GMAC-PHY nodig had.
		time.Sleep(20 * time.Microsecond)
		dev.Write32(CRUBase+cruSOFTRST24, hiword(0, 1, srstPTSADC)|hiword(0, 1, srstTSADC))
		dev.MB()
		return
	}
	dev.Write32(CRUBase+cruSOFTRST24, hiword(0, 1, srstPTSADC)|hiword(0, 1, srstTSADC))
	dev.Write32(CRUBase+cruSOFTRST29, hiword(0, 1, srstTSADCPHY))
	dev.MB()
}

// TempInit brengt de sensor op en zet de auto-conversie aan.
//
// De volgorde is niet vrij: AUTO_CON wordt in stap 3 met een VOLLE write gezet
// (geen hiword-masker — zo doet de driver het ook), dus alles wat er verder in
// moet, moet daarna. En de analoge sequentie in de GRF moet vóór de
// auto-enable, want anders staat er geen sensor achter de conversie.
//
// De hardware-thermal-shutdown laten we UIT. Dat is een bewuste keuze: de DTS
// van dit bord routeert TSHUT naar een GPIO-pen (`hw-tshut-mode = <1>`) op 95°C,
// en een pen die wiebelt doet in onze wereld niets. De CRU-variant zou de hele
// chip resetten, maar dan zou een sensor die verkeerd gekalibreerd blijkt de
// node onherstelbaar cyclen — en we hebben nog geen enkele meting van dit
// silicium. Eerst meten, dan pas een noodrem inbouwen.
func TempInit() { TempInitOrder(false) }

// TempInitOrder is TempInit met de reset-deassert-volgorde als knop; zie
// TempClockOnOrder voor waarom die knop bestaat.
func TempInitOrder(phyFirst bool) {
	TempClockOnOrder(phyFirst)

	dev.Write32(tsUserCon, tsUserConV5)
	dev.Write32(tsAutoPeriod, tsAutoPeriodVal)
	dev.Write32(tsIntDebounce, tsDebounceVal)
	dev.Write32(tsAutoPeriodHT, tsAutoPeriodVal)
	dev.Write32(tsShutDebounce, tsDebounceVal)
	dev.Write32(tsAutoCon, 0) // volle write; polariteit laag, alle bronnen uit
	dev.Write32(tsIntEn, 0)   // geen interrupts, geen tshut-routering
	dev.MB()

	// De analoge voorkant, hiword-masked in de GRF.
	dev.Write32(GRFBase+grfTSADC, hiword(1, 1, grfTsenEn))
	dev.MB()
	time.Sleep(20 * time.Microsecond)
	dev.Write32(GRFBase+grfTSADC, hiword(1, 1, grfAnaReg0))
	dev.Write32(GRFBase+grfTSADC, hiword(1, 1, grfAnaReg1))
	dev.Write32(GRFBase+grfTSADC, hiword(1, 1, grfAnaReg2))
	dev.MB()
	time.Sleep(200 * time.Microsecond)

	// En nu conversie aan, beide kanalen als bron. Q_SEL hoort bij de tabel
	// hieronder — de driver zet die twee bits altijd samen.
	dev.Write32(tsAutoCon, dev.Read32(tsAutoCon)|tsAutoEn|tsQSelEn|1<<4|1<<5)
	dev.MB()
	time.Sleep(5 * time.Millisecond) // één conversieperiode (~2,3ms) plus marge
}

// tsCode is de rk3568-kalibratietabel (rockchip_thermal.c rk3568_code_table),
// mode ADC_INCREMENT: de code LOOPT OP met de temperatuur. Let op — dat is
// niet universeel binnen Rockchip (de rk3288-tabel loopt af), dus deze tabel
// hoort bij dít silicium en mag niet naar een ander board gekopieerd worden.
//
// De sentinels van de Linux-tabel (code 0 en 0xFFF) staan hier NIET in: die
// bestaan daar om de zoeklus te begrenzen, en hier doet de ondergrens-check in
// Temp dat werk explicieter.
var tsCode = [...]struct {
	code int
	mC   int
}{
	{1584, -40000}, {1620, -35000}, {1652, -30000}, {1688, -25000},
	{1720, -20000}, {1756, -15000}, {1788, -10000}, {1824, -5000},
	{1856, 0}, {1892, 5000}, {1924, 10000}, {1956, 15000},
	{1992, 20000}, {2024, 25000}, {2060, 30000}, {2092, 35000},
	{2128, 40000}, {2160, 45000}, {2196, 50000}, {2228, 55000},
	{2264, 60000}, {2300, 65000}, {2332, 70000}, {2368, 75000},
	{2400, 80000}, {2436, 85000}, {2468, 90000}, {2500, 95000},
	{2536, 100000}, {2572, 105000}, {2604, 110000}, {2636, 115000},
	{2672, 120000}, {2704, 125000},
}

// codeToMilli rekent een rauwe TSADC-code om naar millicelsius, met lineaire
// interpolatie binnen de 5°C-stappen van de tabel. ok=false betekent "geen
// geldige meting" en niet "0 graden" — een niet-geïnitialiseerd of ongeklokt
// blok levert een code onder de tabel, en dat moet als zodanig doorkomen in
// plaats van als een plausibel koud getal.
func codeToMilli(raw uint32) (mC int, ok bool) {
	c := int(raw & tsDataMask)
	if c < tsCode[0].code || c > tsCode[len(tsCode)-1].code {
		return 0, false
	}
	for i := 1; i < len(tsCode); i++ {
		if c <= tsCode[i].code {
			lo, hi := tsCode[i-1], tsCode[i]
			span := hi.code - lo.code
			return lo.mC + (hi.mC-lo.mC)*(c-lo.code)/span, true
		}
	}
	return tsCode[len(tsCode)-1].mC, true
}

// Temp geeft de temperatuur van een kanaal (0 = CPU, 1 = GPU) in millicelsius.
// ok=false zolang de sensor niets geldigs levert.
func Temp(chn int) (mC int, ok bool) {
	if chn < 0 || chn > 1 {
		return 0, false
	}
	return codeToMilli(dev.Read32(tsData0 + uintptr(chn)*4))
}

// TempClockInfo geeft de klok- en GRF-registers van de sensor terug. GEMETEN
// NOODZAAK 06-08: de conversie liep niet (rauwe code 0 op beide kanalen) terwijl
// AUTO_CON onze schrijfactie wél droeg. Dan is de vraag of de klokken lopen en of
// de analoge sequentie in de GRF landde — en dat is met deze drie registers in
// één boot te zien in plaats van te gissen.
func TempClockInfo() (gate26, sel51, grfCon uint32) {
	return dev.Read32(CRUBase + cruCLKGATE26),
		dev.Read32(CRUBase + cruCLKSEL51),
		dev.Read32(GRFBase + grfTSADC)
}

// TempDump geeft de eerste 32 woorden van het TSADC-blok terug.
//
// Reden: na twee weggestreepte hypothesen (de afgeleide delers, en de
// parent-klok) heb ik geen onderbouwde derde. Dan is verder gissen duurder dan
// kijken. Als de conversie wél loopt maar de data ergens anders staat dan waar
// ik hem verwacht (0x20 + kanaal*4), dan valt een plausibele waarde — rond 2100
// voor een bord op kamertemperatuur — meteen op in deze dump. Staat er nergens
// zoiets, dan converteert het blok echt niet en is de analoge kant de verdachte.
func TempDump() [32]uint32 {
	var out [32]uint32
	for i := range out {
		out[i] = dev.Read32(tsadcBase + uintptr(i)*4)
	}
	return out
}

// TempRaw geeft de rauwe codes van beide kanalen, voor het meetinstrument: een
// code onder 1584 zegt "blok niet wakker" en dat is een ander probleem dan een
// verkeerde tabel.
func TempRaw() (cpu, gpu uint32, autoCon uint32) {
	return dev.Read32(tsData0) & tsDataMask,
		dev.Read32(tsData0+4) & tsDataMask,
		dev.Read32(tsAutoCon)
}
