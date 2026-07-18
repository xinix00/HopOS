// Package drbg is de gedeelde entropielaag achter runtime/goos.GetRandomData:
// een Hash-DRBG op SHA-256 (out_i = H(state ‖ ctr ‖ 0), state' = H(state ‖
// ctr ‖ 1)), geseed uit een hardware-TRNG en anders uit timing-jitter. Het
// recept stond byte-identiek dubbel (board/uefi en board/hopslot); hier staat
// het één keer — het bóárd kiest de bron en houdt de runtime-hooks
// (go:linkname kan niet in een gedeeld pakket wonen: welke boards meelinken
// verschilt per image, en de hook mag maar één keer bestaan):
//
//   - board/uefi: trng.Fill (FEAT_RNG, anders SMCCC-TRNG via EL3);
//   - board/hopslot: trng.FillCPU (alleen RNDR — een gekooide app praat nooit
//     met EL3: HCR_EL2.TSC trapt elke SMC uit de kooi);
//   - board/qemuvirt: trng.FillCPU (QEMU-TCG heeft geen FEAT_RNG → jitter).
//
// Jitter is op echt silicium een serieuze bron (cache/branch/DRAM-variatie in
// de meetlussen, het jitterentropy-principe); op QEMU/TCG is hij zwakker —
// dáár draait ook geen productie-TLS. Is er een hardwarebron, dan herzaait de
// DRBG zich elke reseedInterval bytes met een verse draw (voorwaartse
// onvoorspelbaarheid); jitter herzaait niet (te duur, de boot-seed volstaat).
package drbg

import (
	"crypto/sha256"
	"encoding/binary"
)

const reseedInterval = 1 << 20 // bytes tussen twee TRNG-herzaaiingen

var (
	state       [32]byte
	ctr         uint64
	source      = "jitter"
	sinceReseed uint64
	fill        func([]byte) (string, bool)
)

// Init seedt de DRBG. fillFn is de hardwarebron van het board (trng.Fill of
// trng.FillCPU; levert de bronnaam en of hij werkte), counter de arch-teller
// (CNTPCT, bv. ARM64.Counter) voor de jitter-terugval. Aanroepen vanuit de
// runtime/goos.InitRNG-hook van het board.
func Init(fillFn func([]byte) (string, bool), counter func() uint64) {
	fill = fillFn
	var seed [48]byte
	if src, ok := fill(seed[:]); ok {
		source = src
	} else {
		jitterSeed(seed[:], counter)
	}
	state = sha256.Sum256(seed[:])
}

// Source geeft de gekozen entropiebron ("rndr", "smccc-trng" of "jitter")
// terug — voor de discovery-print en de boot-log.
func Source() string { return source }

// Read vult b uit de DRBG — het werk achter runtime/goos.GetRandomData.
func Read(b []byte) {
	if source != "jitter" && sinceReseed >= reseedInterval {
		reseed()
	}
	var in [48]byte
	for len(b) > 0 {
		ctr++
		copy(in[:32], state[:])
		binary.LittleEndian.PutUint64(in[32:], ctr)
		in[40] = 0
		out := sha256.Sum256(in[:])
		in[40] = 1
		state = sha256.Sum256(in[:])
		n := copy(b, out[:])
		b = b[n:]
		sinceReseed += uint64(n)
	}
}

// jitterSeed vult dst uit timing-jitter: 512 hash-rondes waarvan de
// individuele DUUR (CNTPCT-delta per ronde) de entropie levert; de teller
// zelf gaat als monotone basis mee. De terugvaller als er geen hardware-TRNG
// is (QEMU virt, de Pi's in de kooi, Neoverse-N1/Altra zonder DEN 0098).
func jitterSeed(dst []byte, counter func() uint64) {
	var pool [48]byte
	var st [32]byte
	for i := 0; i < 512; i++ {
		binary.LittleEndian.PutUint64(pool[32:], counter())
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
	if _, ok := fill(fresh[:]); ok {
		var in [56]byte
		copy(in[:32], state[:])
		copy(in[32:], fresh[:])
		state = sha256.Sum256(in[:])
	}
	sinceReseed = 0
}
