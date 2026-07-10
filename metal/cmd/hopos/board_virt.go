//go:build !rpi5

// board_virt.go — de QEMU-virt-kant van de agent-main: board-registratie en
// de RAM-declaratie van de HOP-kern (het enige dat per board verschilt; de
// main zelf is boardvrij).
package main

import (
	_ "unsafe"

	_ "hop-os/metal/board/qemuvirt" // registreert het board (init) + tamago-hooks
	"hop-os/metal/layout"
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize
