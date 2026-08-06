//go:build arm64 && gui

package slots

import "github.com/xinix00/HopOS/metal/kern/stage2"

// De ARM-kant van de surface-grant: stage-2 kan een venster in een DRAAIENDE
// kooi bij- en afmappen, dus hier is het een tabelwijziging plus een TLBI.
// Zie kern/stage2/surface.go voor de descriptor en de volgorde.

func cageMapSurface(holder, blk int, pa uint64, blocks int) error {
	return stage2.MapSurface(holder, blk, pa, blocks)
}

func cageUnmapSurface(holder, blk, blocks int) error {
	return stage2.UnmapSurface(holder, blk, blocks)
}

// cageRemapSurfaceZero wijst de blokken naar de nulregio in plaats van ze te
// ontmappen — zie de uitleg boven in surfgrant.go: de display kan de slice nog
// vast hebben, en dan is "niet gemapt" een fatale fault en "andermans RAM" een
// lek. Alle blokken naar hetzelfde 2MB, dus één regio dekt de hele node.
func cageRemapSurfaceZero(holder, blk, blocks int, zeroPA uint64) error {
	return stage2.RemapSurfaceZero(holder, blk, blocks, zeroPA)
}
