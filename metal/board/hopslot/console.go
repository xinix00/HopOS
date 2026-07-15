package hopslot

import (
	_ "unsafe" // voor go:linkname
)

// printk is stil: een gekooide core heeft geen UART-MMIO in zijn stage-2-map
// (een poke zou een cage-fault zijn), en app-logs lopen toch via de
// hop-ABI-ring (applib.Logf). Runtime-output (panics) valt dus weg — dezelfde
// keuze als de Pi-boards en uefi al maken op app-cores, hier zonder guard
// omdat dit board alléén op app-cores draait.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {}
