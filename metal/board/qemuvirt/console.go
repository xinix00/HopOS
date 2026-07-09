package qemuvirt

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/fb"
	"hop-os/metal/pl011"
)

// printk stuurt één byte naar de PL011 en spiegelt naar de fb-log-console als
// die actief is (fb.Putc is een no-op zonder scherm — op virt normaal het
// geval). De poke-logica en de PrimeCell-offsets wonen in metal/pl011.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	pl011.Putc(UART0Base, c)
	fb.Putc(c)
}
