package qemuvirt

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/pl011"
)

// printk stuurt één byte naar de PL011. De poke-logica en de PrimeCell-offsets
// wonen in metal/pl011 (gedeeld met rpi4/rpi5).
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) { pl011.Putc(UART0Base, c) }
