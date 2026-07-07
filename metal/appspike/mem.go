package main

import (
	_ "unsafe"

	"hop-os/metal/layout"
)

// Link-time defaults (slot 1): HopOS' slot-manager patcht RamStart en RamSize
// bij het laden naar de werkelijke slot-partitie en job.MemoryLimit. Deze
// waarden zijn dus alleen relevant als de image buiten HopOS om gestart wordt.

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.SlotsBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.SlotStride
