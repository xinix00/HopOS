package apple

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/cpu/drbg"
	"github.com/xinix00/HopOS/metal/cpu/trng"
)

// De gedeelde SHA-256-DRBG (metal/cpu/drbg), gezaaid uit timing-jitter — en
// alleen daaruit. Dezelfde regel als op de Radxa (board/rk3566/rng.go, daar
// gemeten): in de pre-main hooks hoort géén MMIO naar een blok waarvan de
// klok niet bewezen open is, en runtime/goos.InitRNG draait vóór main, vóór
// de eerste print. Apple heeft een hardware-RNG in de SEP (niet vanuit EL2
// bereikbaar zonder zijn protocol) en trng.FillCPU probeert RNDR zelf als het
// silicium FEAT_RNG meldt. Een echte entropiebron is werk voor ná de probe;
// tot die tijd is dit een node om te meten, niet om sleutels op te maken.

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() { drbg.Init(trng.FillCPU, ARM64.Counter) }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) { drbg.Read(b) }
