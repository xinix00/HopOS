// Package pl011 is de minimale poke-laag voor de ARM PrimeCell PL011 UART —
// hetzelfde registerblok dat op QEMU virt (0x09000000), de Pi 4 (0xFE201000)
// en de Pi 5 (0x107d001000) zit. Alleen de base verschilt per board; de
// registeroffsets en de TX-FIFO-full-bit zijn PrimeCell-standaard en horen dus
// één keer te leven i.p.v. per board hergedefinieerd — anders corrigeer je een
// verkeerde offset straks maar in één board.
//
// Alleen voor GOOS=tamago GOARCH=arm64 (device-MMIO via metal/dev).
package pl011

import "hop-os/metal/dev"

const (
	dr     = 0x00   // data register
	fr     = 0x18   // flag register
	frTXFF = 1 << 5 // TX FIFO full
)

// dead: de TX-FIFO-vol-vlag bleef ooit voorbij de poll-grens staan — een
// ongeklokte/dode PL011 (bv. de Pi 5-debug-UART zonder sessie) leest all-ones
// en zou printk anders eeuwig gijzelen. Eén UART per board, dus één vlag.
var dead bool

// Putc stuurt één byte naar de PL011 op base en pollt begrensd tot de
// TX-FIFO ruimte heeft. Een pure functie met een compile-time base — veilig
// als printk-hook vóór init(), zonder runtime-parametrisatie. De firmware
// heeft de UART al geconfigureerd (uart_2ndstage=1 op de Pi); wij raken
// alleen DR en FR aan. De poll-grens (~1M reads ≫ 16 tekens @ 115200) haalt
// een dode UART uit het printpad i.p.v. de boot te laten hangen (op een board
// zonder bereikbare UART is de LED, of straks de netwerk-log, het kanaal).
func Putc(base uintptr, c byte) {
	if dead {
		return
	}
	for i := 0; dev.Read32(base+fr)&frTXFF != 0; i++ {
		if i > 1<<20 {
			dead = true
			return
		}
	}
	dev.Write32(base+dr, uint32(c))
}
