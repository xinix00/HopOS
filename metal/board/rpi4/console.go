package rpi4

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/board/raspi"
)

// printk stuurt één byte naar de PL011 op GPIO14/15 (de bootloader
// configureerde die al: uart_2ndstage=1). Alle logica — app-core-filter,
// timestamp-prefix, fb-spiegel — is gedeeld met de Pi 5: raspi.Printk.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) { raspi.Printk(UART0Base, c) }
