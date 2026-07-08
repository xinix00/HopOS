package rpi5

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/fbcons"
	"hop-os/metal/pl011"
)

// uartLive: zet true zodra er een debug-sessie is (Raspberry Pi Debug Probe
// op het JST-connectortje). Zonder sessie is de PL011 mogelijk ongeklokt en
// kan zelfs een READ van FR de bus laten stallen — daar helpt geen
// poll-limiet tegen, dus in blind-bedrijf raken we het blok simpelweg niet
// aan (boot-meting 2026-07-08: LED-keten stopte precies ná de eerste
// UART-toegang).
const uartLive = false

// printk stuurt één byte naar de debug-PL011 (alleen mét sessie, zie
// uartLive) en spiegelt naar de HDMI-console zodra die er is (fbcons.Init,
// zie probe5/docs/rpi5.md) — het debugkanaal zonder JST-kabeltje; vóór
// Init is de spiegel één bool-check.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	if uartLive {
		pl011.Putc(UART0Base, c)
	}
	fbcons.Putc(c)
}
