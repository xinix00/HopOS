package rpi4

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/board/raspi"
	"hop-os/metal/fb"
	"hop-os/metal/pl011"
)

// printk stuurt één byte naar de PL011 op GPIO14/15 en spiegelt naar de
// HDMI-log-console (metal/fb) zodra die actief is — het beeld-kanaal zonder
// debug-kabel. De bootloader configureerde de UART al (uart_2ndstage=1); de
// poke-logica en de PrimeCell-offsets wonen in metal/pl011.
//
// ALLEEN de HOP-core (MPIDR-affiniteit 0) bezit de UART/fb. Een app-core draait
// onder stage-2 en heeft die MMIO niet in zijn kooi — een runtime-print zou daar
// een cage-fault worden. App-cores laten hun runtime-output vallen (hun eigen
// logs lopen via de hop-ABI-ring). Masker 0xFFFFFF dekt A72-aff0 én A76-aff1.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	if raspi.MPIDR()&0xFFFFFF != 0 {
		return // app-core: geen toegang tot de UART (kooi)
	}
	pl011.Putc(UART0Base, c)
	fb.Putc(c)
}
