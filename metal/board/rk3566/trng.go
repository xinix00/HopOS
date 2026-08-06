package rk3566

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// Het hardware-TRNG van de RK3566: een los blok (géén crypto-engine — de
// rk356x-dtsi heeft helemaal geen crypto-knooppunt). Dit is de échte
// entropiebron waar de placeholder in rng.go om vroeg.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13 drivers/char/hw_random/rockchip-rng.c
// (compatible "rockchip,rk3568-rng", precies wat rk356x-base.dtsi op dit adres
// zet) plus clk-rk3568.c voor klokken en reset. De driver verwijst zelf naar
// RK3568 TRM-Part2 §5.4.1 voor de registerindeling.
//
// EEN EERLIJKE WAARSCHUWING VOORAF, want die hoort in de code en niet in een
// commit-bericht: GÉÉN ENKEL mainline-board zet `status = "okay"` op deze node —
// niet de Radxa Zero 3/3E, niet quartz64, niet rock-3a. Dit recept komt dus uit
// een driver die op dit silicium in mainline nooit draait. Alles hieronder kan
// dus kloppen en toch niets doen, en daarom is Fill zo gebouwd dat een blok dat
// niets levert eerlijk `ok=false` teruggeeft in plaats van nullen door te geven
// voor entropie. Dat verschil is het verschil tussen een zwakke sleutel en een
// zichtbare fout.
//
// En wat dit blok NIET heeft: een self-test of health-check. De driver zegt zelf
// dat de ruwe output ongeveer 87,5% van de FIFS-140-2-tests haalt (kwaliteit
// 900), dus dit is een SEED en geen stroom — hij hoort achter de SHA-256-DRBG
// (metal/cpu/drbg), precies waar rng.go hem hangt.
const (
	trngBase = 0xFE388000 // rk356x-base.dtsi: rng@fe388000

	trngCtl    = trngBase + 0x400 // hiword-masked: (mask<<16) | waarde
	trngSample = trngBase + 0x404 // sample count, plain write
	trngDout   = trngBase + 0x410 // 8 woorden = 32 bytes

	trngStart  = 1 << 0 // per-ronde trigger; valt zelf terug op 0 als klaar
	trngEnable = 1 << 1
	trngLen256 = 3 << 4 // 0=64b, 1=128b, 2=192b, 3=256b
	trngRing0  = 0 << 2 // oscillator-ring-snelheid
	trngCtlAll = 0xFFFF // het volle maskerveld

	// 1000 samples per ronde: de waarde waar de driver zijn
	// kwaliteitsinschatting op baseert. Lager is sneller en slechter.
	trngSampleCnt = 1000

	trngBytesPerRound = 32

	// Klokgate CLKGATE_CON(9): bit 10 = hclk, bit 11 = core. Beide staan in
	// Linux als CLK_IGNORE_UNUSED, dus vermoedelijk al open na de bootketen —
	// maar "vermoedelijk" is precies waarom we ze toch zetten.
	cruCLKGATE9  = 0x300 + 9*4 // = 0x324
	gateHCLKTrng = 10
	gateCLKTrng  = 11

	// SRST_TRNG_NS = 109 → bank 6, bit 13.
	cruSOFTRST6 = 0x400 + 6*4 // = 0x418
	srstTrngNS  = 13
)

// trngInit opent de klokken, pulst de reset en zet het blok in de aan-stand.
// De ENABLE- en LEN-bits blijven daarna staan; START is de per-ronde-trigger.
func trngInit() {
	dev.Write32(CRUBase+cruCLKGATE9,
		hiword(0, 1, gateHCLKTrng)|hiword(0, 1, gateCLKTrng))
	dev.MB()

	dev.Write32(CRUBase+cruSOFTRST6, hiword(1, 1, srstTrngNS))
	dev.MB()
	time.Sleep(5 * time.Microsecond) // de driver neemt 2µs
	dev.Write32(CRUBase+cruSOFTRST6, hiword(0, 1, srstTrngNS))
	dev.MB()

	dev.Write32(trngSample, trngSampleCnt)
	dev.Write32(trngCtl, trngCtlAll<<16|trngLen256|trngRing0|trngEnable)
	dev.MB()
}

// trngStop wist de controlestand en zet de oscillator-ring uit. We draaien hem
// niet permanent: dit blok levert de boot-seed en daarna reseedt de DRBG zich
// uit zijn eigen staat.
func trngStop() {
	dev.Write32(trngCtl, trngCtlAll<<16)
	dev.MB()
}

// trngRound haalt één ronde van 32 bytes op. false bij een blok dat de
// START-bit niet laat vallen — begrensd, want een dood TRNG mag de boot niet
// ophouden (zelfde regel als bij de MDIO- en DMA-wachtlussen).
func trngRound(dst []byte) bool {
	// Alléén de START-bit maskeren: met een vol masker zouden we ENABLE en LEN
	// wissen, en dan draait de volgende ronde met een ander formaat.
	dev.Write32(trngCtl, trngStart<<16|trngStart)
	dev.MB()

	deadline := time.Now().Add(10 * time.Millisecond)
	for dev.Read32(trngCtl)&trngStart != 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Microsecond)
	}

	for i := 0; i < len(dst); i += 4 {
		w := dev.Read32(trngDout + uintptr(i))
		for b := 0; b < 4 && i+b < len(dst); b++ {
			dst[i+b] = byte(w >> (8 * b))
		}
	}
	return true
}

// Fill vult dst met hardware-entropie. Vorm gelijk aan cpu/trng.Fill, zodat
// rng.go hem als bron kan doorgeven aan de DRBG.
//
// ok=false als het blok niet reageert OF als het alleen nullen geeft. Die
// tweede controle is geen paranoia: een blok waarvan de klok dicht staat leest
// als nul, en nul-entropie die als geldig doorgaat is de gevaarlijkste
// uitkomst die dit bestand kan hebben.
func Fill(dst []byte) (source string, ok bool) {
	trngInit()
	defer trngStop()

	var nonzero bool
	for off := 0; off < len(dst); off += trngBytesPerRound {
		end := off + trngBytesPerRound
		if end > len(dst) {
			end = len(dst)
		}
		if !trngRound(dst[off:end]) {
			return "rk3566-trng (timeout)", false
		}
		for _, b := range dst[off:end] {
			if b != 0 {
				nonzero = true
			}
		}
	}
	if !nonzero {
		return "rk3566-trng (all zero)", false
	}
	return "rk3566-trng", true
}
