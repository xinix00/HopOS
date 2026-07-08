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

// Putc stuurt één byte naar de PL011 op base en pollt tot de TX-FIFO ruimte
// heeft. Een pure functie met een compile-time base — veilig als printk-hook
// vóór init(), zonder runtime-parametrisatie. De firmware heeft de UART al
// geconfigureerd (uart_2ndstage=1 op de Pi); wij raken alleen DR en FR aan.
func Putc(base uintptr, c byte) {
	for dev.Read32(base+fr)&frTXFF != 0 {
	}
	dev.Write32(base+dr, uint32(c))
}
