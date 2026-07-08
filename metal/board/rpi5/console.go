package rpi5

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/pl011"
)

// printk stuurt één byte naar de debug-PL011. De firmware heeft de UART al
// geconfigureerd (uart_2ndstage=1); de poke-logica en de PrimeCell-offsets
// wonen in metal/pl011 (gedeeld met rpi4/qemuvirt).
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) { pl011.Putc(UART0Base, c) }
