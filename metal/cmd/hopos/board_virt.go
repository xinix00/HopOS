//go:build !rpi5 && !rpi4 && !uefi

// board_virt.go — de QEMU-virt-kant van de agent-main: board-registratie en
// de RAM-declaratie van de HOP-kern (het enige dat per board verschilt; de
// main zelf is boardvrij).
package main

import (
	_ "unsafe"

	"hop-os/metal/abi/layout"
	_ "hop-os/metal/board/qemuvirt" // registreert het board (init) + tamago-hooks
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize
