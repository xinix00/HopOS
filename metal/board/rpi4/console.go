package rpi4

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/fb"
	"hop-os/metal/pl011"
)

// printk stuurt één byte naar de PL011 op GPIO14/15 en spiegelt naar de
// HDMI-log-console (metal/fb) zodra die actief is — het beeld-kanaal zonder
// debug-kabel. De bootloader configureerde de UART al (uart_2ndstage=1); de
// poke-logica en de PrimeCell-offsets wonen in metal/pl011.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	pl011.Putc(UART0Base, c)
	fb.Putc(c)
}
