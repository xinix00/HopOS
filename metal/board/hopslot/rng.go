package hopslot

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/cpu/drbg"
	"hop-os/metal/cpu/trng"
)

// De gedeelde SHA-256-DRBG (metal/cpu/drbg), MMIO-vrij en dus
// board-onafhankelijk: geseed uit trng.FillCPU (RNDR-instructie waar het
// silicium FEAT_RNG heeft) en anders timing-jitter uit de arch-counter.
// Bewust NIET trng.Fill: dat valt terug op SMCCC-TRNG — een firmware-SMC, en
// een gekooide app praat nooit met EL3 (HCR_EL2.TSC trapt elke SMC uit de
// kooi; op de Altra, zonder FEAT_RNG, zou de allereerste seed de app vellen).

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() { drbg.Init(trng.FillCPU, ARM64.Counter) }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) { drbg.Read(b) }
