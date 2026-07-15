package hopslot

import (
	"crypto/sha256"
	"encoding/binary"
	_ "unsafe" // voor go:linkname

	"hop-os/metal/cpu/trng"
)

// Het uefi-recept (board/uefi/uefi.go), MMIO-vrij en dus board-onafhankelijk:
// een SHA-256-DRBG geseed uit trng.Fill (RNDR-instructie waar het silicium
// FEAT_RNG heeft; SMCCC-TRNG alleen mét EL3-monitor) en anders timing-jitter
// uit de arch-counter. Is er een hardwarebron, dan herzaait de DRBG zich elke
// reseedInterval bytes.

const reseedInterval = 1 << 20 // bytes tussen twee TRNG-herzaaiingen

var (
	drbgState   [32]byte
	drbgCtr     uint64
	rngSource   = "jitter"
	sinceReseed uint64
)

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	var seed [48]byte
	if src, ok := trng.Fill(seed[:]); ok {
		rngSource = src
	} else {
		jitterSeed(seed[:])
	}
	drbgState = sha256.Sum256(seed[:])
}

// jitterSeed vult dst uit timing-jitter: 512 hash-rondes waarvan de
// individuele DUUR (CNTPCT-delta per ronde) de entropie levert; de teller
// zelf gaat als monotone basis mee. De terugvaller als er geen hardware-TRNG
// is (QEMU virt, Pi's).
func jitterSeed(dst []byte) {
	var pool [48]byte
	var st [32]byte
	for i := 0; i < 512; i++ {
		binary.LittleEndian.PutUint64(pool[32:], ARM64.Counter())
		binary.LittleEndian.PutUint64(pool[40:], uint64(i))
		copy(pool[:32], st[:])
		st = sha256.Sum256(pool[:])
	}
	for len(dst) > 0 {
		dst = dst[copy(dst, st[:]):]
		st = sha256.Sum256(st[:])
	}
}

// reseed mengt een verse hardware-draw in de DRBG-state: state' =
// H(state ‖ fresh). Faalt de bron even, dan blijft de oude state staan (nog
// steeds veilig) en proberen we bij de volgende drempel opnieuw.
func reseed() {
	var fresh [24]byte
	if _, ok := trng.Fill(fresh[:]); ok {
		var in [56]byte
		copy(in[:32], drbgState[:])
		copy(in[32:], fresh[:])
		drbgState = sha256.Sum256(in[:])
	}
	sinceReseed = 0
}

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) {
	if rngSource != "jitter" && sinceReseed >= reseedInterval {
		reseed()
	}
	var in [48]byte
	for len(b) > 0 {
		drbgCtr++
		copy(in[:32], drbgState[:])
		binary.LittleEndian.PutUint64(in[32:], drbgCtr)
		in[40] = 0
		out := sha256.Sum256(in[:])
		in[40] = 1
		drbgState = sha256.Sum256(in[:])
		n := copy(b, out[:])
		b = b[n:]
		sinceReseed += uint64(n)
	}
}
