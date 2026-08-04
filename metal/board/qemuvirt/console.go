package qemuvirt

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/driver/conlog"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/pl011"
)

// printk stuurt één byte naar de PL011 en spiegelt naar de fb-log-console als
// die actief is (fb.Putc is een no-op zonder scherm — op virt normaal het
// geval). De poke-logica en de PrimeCell-offsets wonen in metal/driver/pl011.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	conlog.Put(c) // ook over het netwerk op te vragen (driver/conlog)
	pl011.Putc(UART0Base, c)
	fb.Putc(c)
}
