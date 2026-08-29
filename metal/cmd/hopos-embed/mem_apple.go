//go:build apple

package main

import (
	"fmt"
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/board/apple"
)

// De RAM-declaratie van de HOP-kern op Apple: hetzelfde venster als de agent
// (cmd/hopos/board_apple.go) — 256MB vanaf 1TiB+4GB.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = apple.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = apple.HopRAMSize

func init() {
	// Zie cmd/hopos/board_apple.go: het DRAM via 2MB-blokken bereikbaar maken
	// (geen 1GB-blokken boven 2^40 op dit silicium) vóór het plan, want de
	// pool, m1n1's spin-table en de struct-regio liggen buiten het HOP-venster.
	if p, ok := apple.Params(); ok {
		n := apple.MapDRAM(p.DRAMBase, p.DRAMSize)
		fmt.Printf("mmu: %d GB of DRAM remapped to 2MB blocks\n", n)
	}
	apple.SetupPlan()
}
