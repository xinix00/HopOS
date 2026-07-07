package rpi4

import (
	_ "unsafe"

	"hop-os/metal/board/raspi"
)

// PLACEHOLDER-entropie voor de bring-up: xorshift64* geseed met de cycle
// counter — zelfde steiger als board/rpi5 en board/qemuvirt. NIET
// cryptografisch: vóór er TLS/keys op de Pi draaien (P2, agent) moet dit de
// hardware-RNG worden (RNG200 op RNG200Base — zelfde blok als de Pi 5, dus
// t.z.t. één gedeelde driver in board/raspi).

var rngState uint64

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	rngState = raspi.ARM64.Counter() | 1
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
