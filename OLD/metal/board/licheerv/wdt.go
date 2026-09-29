package licheerv

// De hardware-watchdog van de CV181x: een standaard Synopsys DesignWare WDT
// (vendor-DT: snps,dw-wdt @ 0x03010000, klok = de vaste 25MHz pclk). Dezelfde
// filosofie als op de Pi (board/raspi/watchdog.go) — HOP-leven = node-leven,
// een defecte node cyclet zichzelf naar een verse boot — maar met één les
// erbij van 02-08: het levensteken is hier NIET "draait de scheduler" maar
// wordt door de aanroeper bepaald. De gemeten doofheid (nieuwe verbindingen
// en ICMP dood, gevestigde flows en al HOP's lussen kerngezond) was voor een
// onvoorwaardelijke aaier onzichtbaar geweest; daarom is dit bestand alleen
// het mechanisme — wapenen en aaien — en ligt de vraag "leeft de node écht?"
// bij wie er verstand van heeft (de canary in cmd/hopos/board_licheerv.go).
//
// DW-WDT-recept (Synopsys databook; het blok zelf is door de hart-1-probe
// bewezen bereikbaar vóór iemand dit aanroept):
//
//	CR   (0x00)  bit0 = enable (eenmaal aan kan hij niet meer uit — by design),
//	             bit1 = response mode: 0 = direct systeemreset
//	TORR (0x04)  timeout-range: timeout = 2^(16+top) pclk-cycli; top én
//	             top_init allebei 15 → 2^31/25MHz ≈ 86s
//	CCVR (0x08)  actuele tellerstand (read-only)
//	CRR  (0x0C)  0x76 schrijven = teller terug op vol ("aaien")

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	wdtBase = 0x03010000
	wdtCR   = wdtBase + 0x00
	wdtTORR = wdtBase + 0x04
	wdtCRR  = wdtBase + 0x0C

	wdtKick = 0x76 // het vaste DW-restart-wachtwoord
	wdtTop  = 15   // 2^31 cycli op 25MHz ≈ 86s — ruim boven de canary-periode
)

// WatchdogArm wapent de watchdog op ~86s. Onomkeerbaar: de DW-WDT kent geen
// uit-knop (dat is de garantie, geen gebrek) — wie wapent, belooft te aaien.
// De aanroeper doet dat pas nadat de node één keer bewezen gezond was, zodat
// een kapotte bring-up blijft staan voor de operator in plaats van te cyclen.
//
// Het recept is verbatim de enable-helft van de FSBL's eigen __system_reset
// (vendor: fsbl/plat/cv180x/platform.c) — want een kale DW-enable bleek niet
// genoeg: op 02-08 stopte de canary met aaien en bleef de node gewoon dood.
// De TOP_WDT-afloop reset de chip alleen als de RTC-routering hem draagt én
// de puls lang genoeg is (CR=0x11, vendor-pulslengte; onze eerdere 0x1 was
// vermoedelijk te kort om de chip-reset te latchen). Alles hieronder is een
// enable, geen trigger: de reset komt pas als de teller ooit afloopt.
func WatchdogArm() {
	dev.Write32(0x050260E0, 0x0001)     // rtc_core: watchdog reset enable
	dev.Write32(0x050260C8, 0x0001)     // rtc_core: power cycle enable
	time.Sleep(100 * time.Microsecond)  // de FSBL wacht hier ook (rtc-domein is traag)
	dev.Write32(0x050250AC, 0x00000000) // rtcsys_rstn_src_sel: WDT → hele rtcsys
	dev.Write32(0x05025004, 0x0000AB18) // RTC_CTRL0 unlock
	dev.Write32(0x05025008, 0x00400040) // rtc_ctrl: watchdog reset enable

	dev.Write32(wdtTORR, wdtTop|wdtTop<<4)
	dev.Write32(wdtBase+0x1C, 0x20) // vendor-glue van de TOP_WDT (verbatim)
	dev.Write32(wdtCRR, wdtKick)
	dev.Write32(wdtCR, 0x11) // enable + vendor-pulslengte, response = directe reset
	fmt.Println("watchdog: hardware reset armed (~86s, FSBL reset routing) — a node that stops proving liveness now self-reboots")
}

// WatchdogProbe is stap 8 van de hart-probe (hartprobe_riscv64.s), maar dan
// vanaf het aanroepende hart zelf: CCVR aanraken (de lees die bus-fault als
// het blok dood is), TORR schrijven en teruglezen. TORR is inert zolang
// CR.enable uit staat, en de geschreven waarde is exact wat WatchdogArm er
// straks tóch in zet. Vanaf HOP's eigen hart is dit een gok die de node kan
// kosten (HOP overleeft een bus-fout niet, de probe-les) — de aanroeper hoort
// dat af te wegen én vooraf luid te melden, zodat een dode console de dader
// aanwijst (hop/hartprobe.go doet beide).
func WatchdogProbe() bool {
	_ = dev.Read32(wdtBase + 0x08) // WDT_CCVR — de aanraking
	dev.Write32(wdtTORR, wdtTop|wdtTop<<4)
	return dev.Read32(wdtTORR) == wdtTop|wdtTop<<4
}

// WatchdogPet zet de teller terug op vol. Aanroepen zolang — en alléén
// zolang — de node zijn levensteken haalt.
func WatchdogPet() { dev.Write32(wdtCRR, wdtKick) }
