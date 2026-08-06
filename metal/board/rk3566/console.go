package rk3566

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/driver/conlog"
)

// DW APB UART (16550-compatibel, reg-shift=2 → registers op 4-byte stride) —
// dezelfde IP-familie als de LicheeRV-console, andere basis. De bootloader
// heeft baudrate/formaat al gezet; wij pollen LSR en schrijven THR.
const (
	uartTHR = 0x00      // transmit holding register
	uartLSR = 0x05 << 2 // line status register
	lsrTHRE = 1 << 5    // transmit holding register empty
)

// Géén lock — zelfde afweging als board/licheerv/console.go: een console mag
// de node nooit kunnen ophouden, en de enige gemeten "console-race" daar bleek
// buiten het OS te liggen.
//
// De bestemmingen zelf staan in conlog.Route, want die lijst is geen
// board-kennis en stond hiervoor vijf keer los — dit board vergat de
// framebuffer, met als symptoom dat er op de monitor wél een bunny-header stond
// en géén logregel.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	// ALLEEN de HOP-core bezit de UART en de framebuffer. Een app-core draait
	// onder stage-2 en heeft die MMIO niet in zijn kooi; een runtime-print daar
	// (bijvoorbeeld een throw) zou een cage-fault worden die de échte oorzaak
	// maskeert. App-cores laten hun runtime-output dus vallen — hun eigen logs
	// lopen via de hop-ABI-ring.
	if dev.MPIDR()&0xFFFFFF != 0 {
		return
	}

	// De bestemmingen (ring → lijn → glas) staan één keer, in conlog.Route. Wat
	// dit board eraan toevoegt is alleen zijn eigen manier om een byte op de
	// lijn te zetten.
	conlog.Route(c, uartPut)
}

// uartPut schrijft één byte naar de DW-APB-UART. Pollt op THRE — begrensd door
// het feit dat de bootloader de UART al aan had; een ongeklokte UART zou hier
// blijven hangen, en dat is precies waarom de ring vóór de lijn komt.
func uartPut(c byte) {
	for dev.Read32(UART2Base+uartLSR)&lsrTHRE == 0 {
		// wait for TX ready
	}
	dev.Write32(UART2Base+uartTHR, uint32(c))
}
