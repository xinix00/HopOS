package qemuvirt

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/dev"
)

// printk stuurt één byte naar de PL011 (poll op TX-FIFO-full). MMIO-toegang via
// metal/dev — dezelfde gealigneerde word-ops als de rest van het board (gic.go).
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	for dev.Read32(uartFR)&frTXFF != 0 {
	}
	dev.Write32(uartDR, uint32(c))
}
