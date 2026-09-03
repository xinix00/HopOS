//go:build apple

package main

import (
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/v2/board/apple"
)

// De RAM-declaratie van de HOP-kern op Apple: hetzelfde venster als de agent
// (cmd/hopos/board_apple.go) — 256MB vanaf 1TiB+4GB.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = apple.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = apple.HopRAMSize

func init() {
	apple.SetupPlan()
}
