package rpi5

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/board/raspi"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/pl011"
)

// printk stuurt één byte naar de debug-PL011 (de 3-pins JST-SH-connector;
// Raspberry Pi Debug Probe) en spiegelt naar de HDMI-log-console (metal/driver/fb)
// zodra die actief is — hét beeld-kanaal voor de Pi 5 zónder debug-kabeltje.
// Putc pollt begrensd — een dode/ongeklokte UART kost hooguit de poll, nooit
// de boot (zie metal/driver/pl011); fb.Putc is een no-op zonder scherm.
//
// ALLEEN de HOP-core (MPIDR-affiniteit 0) bezit de UART/fb. Een app-core draait
// onder stage-2 en heeft die MMIO niet in zijn kooi — een runtime-print (bv. een
// throw) zou daar een cage-fault worden die de échte oorzaak maskeert. App-cores
// laten hun runtime-output vallen (hun eigen logs lopen via de hop-ABI-ring).
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	if raspi.MPIDR()&0xFFFFFF != 0 {
		return // app-core: geen toegang tot de UART (kooi)
	}
	// Via de gedeelde stamper: die zet (indien aan) één uniforme
	// "dd-MM HH:mm"-prefix aan het begin van elke regel — op UART én fb.
	raspi.ConsoleByte(c, func(b byte) {
		pl011.Putc(UART0Base, b)
		fb.Putc(b)
	})
}
