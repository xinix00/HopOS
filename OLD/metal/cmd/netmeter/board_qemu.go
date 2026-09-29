//go:build !licheerv

// board_qemu.go — de QEMU-virt-kant van de bank: board-registratie en de
// RAM-declaratie (het enige dat per board verschilt; de bank zelf is
// boardvrij). Zelfde snit als cmd/hopos/board_virt.go.
package main

import (
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	_ "github.com/xinix00/HopOS/metal/v2/board/qemuvirt/hop" // registreert het board (init) + runtime-hooks
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize

// serveSize: met 240MB HOP-RAM kan de serve-blob ruim — 32MiB geeft de host
// iets substantieels om te klokken.
const serveSize = 32 << 20
