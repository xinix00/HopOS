package rpi5

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/board/raspi"
)

// printk stuurt één byte naar de debug-PL011 (de 3-pins JST-SH-connector;
// Raspberry Pi Debug Probe). Alle logica — app-core-filter, timestamp-prefix,
// fb-spiegel — is gedeeld met de Pi 4: raspi.Printk.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) { raspi.Printk(UART0Base, c) }
