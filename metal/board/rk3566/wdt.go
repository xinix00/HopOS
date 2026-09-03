package rk3566

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// De hardware-watchdog van de RK3566: een standaard Synopsys DesignWare WDT.
// Zelfde filosofie als op de Pi en de LicheeRV — HOP-leven = node-leven, een
// node die zijn levensteken niet meer haalt cyclet zichzelf naar een verse boot
// — en met dezelfde les van 02-08 erbij: dit bestand is alleen het MECHANISME
// (wapenen, aaien), en de vraag "leeft de node écht?" ligt bij de canary in de
// main. Een onvoorwaardelijke aaier had de gemeten doofheid van die dag
// (nieuwe verbindingen dood, alle HOP-lussen kerngezond) niet gezien.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13 drivers/watchdog/dw_wdt.c voor de
// registers en de TOP-tabel, rk356x-base.dtsi voor adres en klok, en
// clk-rk3568.c voor de klokgate.
//
// TWEE DINGEN ZIJN HIER NIET UIT DE BRON TE BEWIJZEN, en ze staan allebei
// eerlijk in de code in plaats van als aanname te verdwijnen:
//
//  1. Of dit IP-core met de VASTE TOP-tabel gesynthetiseerd is. Linux gebruikt
//     onvoorwaardelijk `1 << (16+i)` (dw_wdt_fix_tops), en de rk356x-DTS zet
//     geen eigen `snps,watchdog-tops` — dus als dit silicium andere TOPs heeft,
//     heeft Linux het zélf ook mis. WatchdogArm meet daarom de echte waarde
//     in plaats van hem te melden: na een kick staat de teller op TOP, dus
//     CCVR gedeeld door de tclk ís de timeout. Wat de log zegt is gemeten.
//  2. Of een afgelopen teller de HELE node reset of maar een deel. Mainline
//     heeft geen Rockchip-glue voor dit blok (geen reset-scope-bit, geen
//     GRF-veld), dus dit is met code niet te beantwoorden — alleen met een
//     boot. Zie WatchdogResetTest.
//
// Precedent dat waarschuwt: op de LicheeRV was een kale DW-enable NIET genoeg
// (de reset-routering in het RTC-domein moest er eerst bij), en dat kostte een
// nacht waarin de canary stopte met aaien en de node gewoon dood bleef staan.
// Daarom wapent de agent hier pas als punt 2 op ijzer bewezen is.
const (
	wdtBase = 0xFE600000 // rk356x-base.dtsi: watchdog@fe600000

	wdtCR     = wdtBase + 0x00 // control: bit0 enable, bit1 response mode
	wdtTORR   = wdtBase + 0x04 // timeout range: TOP [3:0], TOP_INIT [7:4]
	wdtCCVR   = wdtBase + 0x08 // current counter value (read-only, loopt af)
	wdtCRR    = wdtBase + 0x0C // counter restart: 0x76 = aaien
	wdtParams = wdtBase + 0xF4 // COMP_PARAMS_1: bit6 = USE_FIX_TOP

	wdtEnable   = 1 << 0 // eenmaal aan kán hij niet meer uit — by design
	wdtKick     = 0x76   // het vaste DW-restart-wachtwoord
	wdtUseFixed = 1 << 6 // COMP_PARAMS_1: vaste TOP-tabel gesynthetiseerd

	// TOP 15 = 2^31 tclk-cycli. De tclk is TCLK_WDT_NS, een gate rechtstreeks
	// op xin24m (clk-rk3568.c) — dus 24MHz exact, zonder deler, en daarmee
	// ~89,5s. Ruim boven elke canary-periode; de tabel is grof (elke stap
	// verdubbelt), dus er is geen fijnere keuze in de buurt.
	wdtTop  = 15
	wdtTclk = 24_000_000

	// Klokgate: CLKGATE_CON(26), bit 13 = pclk (APB), bit 14 = tclk (de teller).
	cruCLKGATE26  = 0x300 + 26*4 // = 0x368
	gatePCLKWDTNS = 13
	gateTCLKWDTNS = 14
)

// WatchdogClockOn opent de twee klokgates van de watchdog. pclk staat na de
// bootketen vermoedelijk al open (zoals bij het GMAC), maar de tclk is de
// teller zelf — zonder die klok loopt hij niet af en is een gewapende watchdog
// een gewapend niets.
func WatchdogClockOn() {
	dev.Write32(CRUBase+cruCLKGATE26,
		hiword(0, 1, gatePCLKWDTNS)|hiword(0, 1, gateTCLKWDTNS))
	dev.MB()
}

// WatchdogArm wapent de watchdog en geeft de GEMETEN timeout terug (in
// seconden), plus of dit silicium de vaste TOP-tabel meldt. Onomkeerbaar: de
// DW-WDT kent geen uit-knop — dat is de garantie, niet een gebrek — dus wie
// wapent, belooft te aaien.
//
// De volgorde is die van dw_wdt_arm_system_reset: eerst de timeout, dan een
// kick zodat de teller met de nieuwe TOP begint, dan pas enable.
func WatchdogArm() (seconds float64, fixedTops bool) {
	// WatchdogProbeTop doet het zetten, kicken en meten; hier komt alleen de
	// enable erbij. Eén plek voor die sequentie, zodat de gemeten timeout in de
	// probe en de gewapende timeout niet uit elkaar kunnen lopen.
	seconds, fixedTops = WatchdogProbeTop()
	dev.Write32(wdtCR, wdtEnable) // bit1 (response = eerst IRQ) blijft 0: directe reset
	dev.MB()
	return seconds, fixedTops
}

// WatchdogProbeTop meet de ECHTE timeout bij TOP 15 zonder de watchdog te
// wapenen: TORR zetten, kicken, CCVR lezen. De teller laadt bij een kick met de
// werkelijke TOP van dit silicium, dus dit is een meting en geen tabelwaarde.
//
// Geeft (0, fixed) terug als de teller niet laadt zolang de enable-bit uit staat
// — dan is de timeout pas op het moment van wapenen te meten, en dat zegt de
// aanroeper dan ook zo.
func WatchdogProbeTop() (seconds float64, fixedTops bool) {
	WatchdogClockOn()
	params := dev.Read32(wdtParams)
	dev.Write32(wdtTORR, wdtTop|wdtTop<<4)
	dev.Write32(wdtCRR, wdtKick)
	dev.MB()
	counts := dev.Read32(wdtCCVR)
	return float64(counts) / wdtTclk, params&wdtUseFixed != 0
}

// WatchdogPet zet de teller terug op vol. Aanroepen zolang — en alléén zolang —
// de node zijn levensteken haalt. Eén woord, geen read-modify-write.
func WatchdogPet() { dev.Write32(wdtCRR, wdtKick) }

// WatchdogInfo leest de stand terug voor het meetinstrument: draait hij, welke
// TOP staat erin, hoe ver is de teller.
func WatchdogInfo() (cr, torr, ccvr, params uint32) {
	return dev.Read32(wdtCR), dev.Read32(wdtTORR),
		dev.Read32(wdtCCVR), dev.Read32(wdtParams)
}

// WatchdogResetTest is de enige manier om de vraag te beantwoorden die de
// Linux-bron niet beantwoordt: reset een afgelopen teller de HELE node?
//
// Het wapent met de KORTSTE TOP (~2,7ms bij 24MHz) en aait niet. Herstart het
// bord binnen een fractie van een seconde, dan is de scope de node en mag de
// agent de watchdog gaan gebruiken. Gebeurt er niets, dan is een gewapende
// watchdog hier een lege belofte en moeten we de CRU-globalreset gebruiken.
//
// BEWUST alleen achter een expliciete config-sleutel (hopos.wdtest=1): dit
// commando maakt van het bord een herstartlus, en dat wil je kiezen en niet
// overkomen.
func WatchdogResetTest() {
	WatchdogClockOn()
	fmt.Println("watchdog: RESET-SCOPE TEST — arming the shortest timeout and NOT petting. " +
		"If the board reboots within a second, a watchdog timeout resets the whole node.")
	time.Sleep(200 * time.Millisecond) // de UART leeg laten lopen vóór de reset
	dev.Write32(wdtTORR, 0)            // TOP 0 = 2^16 cycli ≈ 2,7ms
	dev.Write32(wdtCRR, wdtKick)
	dev.Write32(wdtCR, wdtEnable)
	dev.MB()
	for {
		time.Sleep(time.Second)
		fmt.Println("watchdog: still alive after the timeout should have fired — " +
			"a timeout does NOT reset this node from here (use the CRU global reset instead)")
	}
}

// GlobalReset herstart de hele SoC via de CRU. Dit is het pad dat GEGARANDEERD
// de node reset (clk-rk3568.c registreert dezelfde write als zijn
// restart-handler met priority 128), maar het vereist dat er nog iets draait om
// hem te schrijven — dus is het een reboot-primitief en géén watchdog.
func GlobalReset() {
	dev.Write32(CRUBase+0xD4, 0xFDB9) // RK3568_GLB_SRST_FST
	dev.MB()
	for {
		time.Sleep(time.Second) // de reset komt; hier nooit uit
	}
}
