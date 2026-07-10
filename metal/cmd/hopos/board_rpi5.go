//go:build rpi5

// board_rpi5.go — de Raspberry Pi 5-kant van de agent-main: dezelfde
// HOP-agent-bytes, maar met de rpi5-runtime-hooks en de RAM-declaratie van
// het board (raw load op 0x80000, 128MB HOP-kern — zie pi5_main/mem_rpi5).
// Netwerk komt via board.ProbeNIC: PCIe-RC-training → RP1 → GEM → DHCP,
// allemaal HOP's eigen werk (P2, bewezen 2026-07-10).
package main

import (
	_ "unsafe"

	_ "hop-os/metal/board/rpi5" // registreert het board (init) + tamago-hooks
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = 0x00080000

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x08000000
