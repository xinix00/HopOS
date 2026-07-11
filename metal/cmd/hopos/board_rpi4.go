//go:build rpi4

// board_rpi4.go — de Raspberry Pi 4-kant van de agent-main: dezelfde
// HOP-agent-bytes, met de rpi4-runtime-hooks en de RAM-declaratie van het
// board (raw load op 0x80000, 128MB HOP-kern — zie pi4_main/mem_rpi4).
// Netwerk = de geïntegreerde GENET v5 (metal/genet, P2 bewezen 2026-07-11).
package main

import (
	_ "unsafe"

	_ "hop-os/metal/board/rpi4" // registreert het board (init) + tamago-hooks
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = 0x00080000

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x08000000
