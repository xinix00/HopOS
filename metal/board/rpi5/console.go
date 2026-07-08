package rpi5

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/pl011"
)

// printk stuurt één byte naar de debug-PL011 (de 3-pins JST-SH-connector;
// Raspberry Pi Debug Probe). Putc pollt begrensd — een dode/ongeklokte UART
// kost hooguit de poll, nooit de boot (zie metal/pl011).
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) { pl011.Putc(UART0Base, c) }
