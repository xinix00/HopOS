package rk3566

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/v2/cpu/drbg"
	"github.com/xinix00/HopOS/metal/v2/cpu/trng"
)

// De gedeelde SHA-256-DRBG (metal/cpu/drbg). De BOOT-seed komt uit
// timing-jitter; het hardware-TRNG (trng.go) komt er pas ná de boot bij, via
// een expliciete aanroep van UseHardwareRNG.
//
// DIE VOLGORDE IS GEMETEN EN NIET GEKOZEN, en het is de duurste fout van deze
// bring-up. Ik had hier eerst het TRNG rechtstreeks als seed-bron gehangen, en
// dat leek onschuldig: de driver heeft nette timeouts. Maar `initRNG` is de
// runtime/goos.InitRNG-hook, en de Go-runtime roept die aan VÓÓR main — vóór de
// eerste print, vóór élke andere init. Gevolg op ijzer (06-08): `Starting kernel
// ...` en daarna niets. Geen banner, geen paniek, geen enkel teken.
//
// Waarom een timeout daar niet helpt: een ongeklokt Rockchip-blok geeft geen
// abort maar HOUDT DE BUS VAST. Dan wordt de read zelf nooit afgerond en komt
// de code die op de klok wacht er niet eens aan toe. Precies de reden dat de
// GMAC-read in cmd/proberk3566 bewust als LAATSTE staat — ik had die les
// opgeschreven en hem hier alsnog overtreden.
//
// De regel die eruit volgt, en die breder geldt dan dit bestand: **in de
// pre-main hooks hoort geen enkele MMIO-toegang naar een blok waarvan de klok
// niet bewezen open is.** Jitter is daar goed genoeg (op echt silicium is het
// een serieuze bron: cache-, branch- en DRAM-variatie), en een betere bron mag
// altijd later.
//
// De Cortex-A55 heeft geen FEAT_RNG, dus trng.FillCPU is hier per definitie de
// jitter-bron en niet RNDR.

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() { drbg.Init(trng.FillCPU, ARM64.Counter) }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) { drbg.Read(b) }

// UseHardwareRNG herzaait de DRBG uit het hardware-TRNG. Aanroepen ná de boot,
// door wie heeft vastgesteld dat het blok antwoordt — de probe doet dat, en de
// agent mag het pas als die meting groen was.
//
// Geeft de bron terug die het écht deed: lukt het TRNG niet (geen antwoord, of
// alleen nullen omdat de klok dicht staat), dan blijft de jitter-seed staan en
// zegt de melding dat ook. Wat NIET mag is nul-entropie die als geldig doorgaat:
// dat is het verschil tussen een zwakke sleutel en een zichtbare fout.
func UseHardwareRNG() (source string, ok bool) {
	var probe [32]byte
	src, ok := Fill(probe[:])
	if !ok {
		return src, false
	}
	// Pas nú de DRBG omhangen: Init doet zelf een verse trekking, en die mag
	// alleen langs een bron die zojuist bewezen heeft te werken.
	drbg.Init(Fill, ARM64.Counter)
	return drbg.Source(), true
}
