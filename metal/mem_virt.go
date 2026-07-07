//go:build qemuvirt

package main

import (
	_ "unsafe"

	"hop-os/metal/layout"
)

// Geheugendeclaratie van de HOP-kern (core 0): alleen de eigen partitie.
// De slot-partities en de mailbox vallen hierbuiten en worden dus door de
// MMU als device/niet-gecached gemapt — precies wat het laden van app-images
// en de mailbox-communicatie coherent houdt zonder cache-onderhoud (QEMU;
// op echt ijzer herzien we dit met expliciete cache-maintenance).

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize // DMA-regio (layout.DMABase) valt hierbuiten
