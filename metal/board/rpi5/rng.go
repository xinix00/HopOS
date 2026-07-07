package rpi5

import (
	_ "unsafe"
)

// PLACEHOLDER-entropie voor de bring-up: xorshift64* geseed met de cycle
// counter — zelfde steiger als board/qemuvirt. NIET cryptografisch: vóór er
// TLS/keys op de Pi draaien (P2, agent) moet dit de BCM2712-hardware-RNG
// (RNG200) of een jitter-DRBG worden. Zie docs/rpi5.md.

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
