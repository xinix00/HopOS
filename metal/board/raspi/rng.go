package raspi

import _ "unsafe" // voor go:linkname

// PLACEHOLDER-entropie voor de bring-up: xorshift64* geseed met de cycle
// counter — gedeeld door rpi4 en rpi5 (voorheen een identieke kopie per board).
// NIET cryptografisch: vóór er TLS/keys op de Pi draaien (P2, agent) moet dit
// de BCM2711/2712-hardware-RNG (RNG200) of een jitter-DRBG worden — zie
// docs/rpi5.md resp. docs/rpi4.md. board/qemuvirt houdt bewust zijn eigen kopie
// (andere ARM64-instantie, O6N-ontwikkeldoel).

var rngState uint64

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	rngState = ARM64.Counter() | 1
}

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) {
	x := rngState
	for i := range b {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		b[i] = byte((x * 0x2545F4914F6CDD1D) >> 56)
	}
	rngState = x
}
