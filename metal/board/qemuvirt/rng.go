package qemuvirt

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/cpu/drbg"
	"hop-os/metal/cpu/trng"
)

// De gedeelde SHA-256-DRBG (metal/cpu/drbg), zelfde bronkeuze als hopslot:
// trng.FillCPU (RNDR) waar de CPU FEAT_RNG heeft — QEMU-TCG heeft dat niet,
// dus in de praktijk de timing-jitter-seed. Vervangt de oude
// placeholder-xorshift (expliciet niet cryptografisch; deze wel, al blijft
// jitter op TCG zwakker dan op silicium — op QEMU draait geen productie-TLS).

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() { drbg.Init(trng.FillCPU, ARM64.Counter) }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) { drbg.Read(b) }
