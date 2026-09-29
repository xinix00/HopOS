// Package ns16550 is de minimale poke-laag voor een 16550-compatibele UART
// (DesignWare 8250 op de Orion O6N/Cix P1, en menig ander SoC): de
// tegenhanger van driver/pl011 voor de UEFI-console. De ACPI SPCR zegt welk
// van de twee een platform heeft (Interface Type) én met welke registerstap
// (GAS access size: DW-8250-blokken liggen op 32-bit-stride). Alleen THR en
// LSR worden aangeraakt; de firmware heeft baudrate en lijn al gezet.
//
// Alleen voor GOOS=tamago GOARCH=arm64 (device-MMIO via metal/dev).
package ns16550

import "github.com/xinix00/HopOS/metal/v2/dev"

const (
	thr     = 0      // transmit holding register (registerindex)
	lsr     = 5      // line status register (registerindex)
	lsrTHRE = 1 << 5 // THR leeg
)

// dead: zelfde vangrail als pl011 — een UART die nooit THRE meldt (dood,
// ongeklokt, all-ones) mag printk niet eeuwig gijzelen.
var dead bool

// Putc stuurt één byte naar de 16550 op base met registerstap 1<<shift
// (0 = byte-stride, 2 = 32-bit-stride zoals DesignWare). Begrensde poll op
// THRE (~1M reads ≫ 16 tekens @ 115200); daarna is de UART dood verklaard.
func Putc(base uintptr, shift uint, c byte) {
	if dead {
		return
	}
	rd, wr := dev.Read32, dev.Write32
	if shift == 0 {
		// Byte-stride: een 32-bit-read op LSR (offset 5) is ongealigneerd
		// en faultt op device-geheugen — dan per byte.
		rd = func(a uintptr) uint32 { return uint32(dev.Read8(a)) }
		wr = func(a uintptr, v uint32) { dev.Write8(a, uint8(v)) }
	}
	for i := 0; rd(base+lsr<<shift)&lsrTHRE == 0; i++ {
		if i > 1<<20 {
			dead = true
			return
		}
	}
	wr(base+thr<<shift, uint32(c))
}
