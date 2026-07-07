package qemuvirt

import (
	_ "unsafe"
)

// PLACEHOLDER-entropie voor QEMU-ontwikkeling: xorshift64* geseed met de
// cycle counter. NIET cryptografisch — op echt ijzer (O6N) moet hier een
// hardware-RNG of jitter-entropy DRBG komen vóór er TLS/keys op draaien.

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
