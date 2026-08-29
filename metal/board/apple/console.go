package apple

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/driver/conlog"
)

// Twee UARTs, allebei door m1n1 al geïnitialiseerd (klok, baud, pinnen), wij
// pollen en schrijven alleen:
//
//   - de dockchannel (aapl,dock-channels): een FIFO naar de Type-C-debugpoort.
//     Dit is wat m1n1 zelf als console gebruikt op de M2 en later. Registers
//     uit m1n1's dockchannel_uart.c: TX-vrije-plaatsen op +0x4014, byte in
//     op +0x4004.
//   - uart0 (uart-1,samsung, de S5L-UART): de klassieke debug-console die op
//     de M1 via de Type-C-seriële modus naar buiten kwam. UTRSTAT op +0x10
//     (bit 1 = TX-buffer leeg), UTXH op +0x20 — m1n1's uart.c.
//
// Welke van de twee op de laptop (/dev/cu.debug-console) aankomt is op de M4
// nog niet gemeten, dus dit board schrijft naar beide. Zodra dat bekend is
// valt er één af; tot die tijd kost het per byte één extra poll.
const (
	dockTX     = 0x4004 // DATA_TX8
	dockTXFree = 0x4014 // DATA_TX_FREE

	uartUTRSTAT = 0x10
	uartUTXH    = 0x20
	utrstatTXBE = 1 << 1

	// Bovengrens op de poll: een console mag de node nooit ophouden. Zit de
	// FIFO vol en blijft dat zo (niemand leest aan de andere kant), dan valt
	// de byte — de ring (conlog) houdt hem wél. Een lijn die stokt krijgt
	// daarna een klein budget (pollStalled) tot een byte weer wél past: zo
	// kost een lezerloze FIFO geen 2ms per byte, maar komt een lezer die
	// later aanhaakt (debugusb-mode aanzetten terwijl de node al draait) wél
	// weer beeld — een lijn permanent afschrijven bleek de verkeerde afslag.
	pollMax     = 20_000
	pollStalled = 256
)

// dockStalled/uartStalled: de vorige byte paste niet binnen de poll.
var dockStalled, uartStalled bool

// Géén lock — zelfde afweging als board/rk3566/console.go: een console mag de
// node nooit kunnen ophouden.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	// ALLEEN de boot-core (m1n1 boot ons op een P-core, niet op core 0)
	// bezit de UARTs. Een app-core onder stage-2 heeft die MMIO niet in zijn
	// kooi; zijn runtime-output zou een cage-fault worden die de echte oorzaak
	// maskeert.
	if dev.MPIDR()&0xFFFFFF != dev.Read64(MPIDRScratch)&0xFFFFFF {
		return
	}
	conlog.Route(c, uartPut)
}

// uartPut zet één byte op beide lijnen. '\n' wordt "\r\n": de ontvangende tty
// staat raw (m1n1 doet hetzelfde in dockchannel_uart_putchar).
func uartPut(c byte) {
	if c == '\n' {
		putBoth('\r')
	}
	putBoth(c)
}

func putBoth(c byte) {
	// Vaste adressen, en dat is een keuze. De ADT weet ze ook, maar die
	// uitlezen is een wandeling door een boom die zelf pas werkt als het DRAM
	// gemapt is — en dat mag niet aan de eerste byte console-uitvoer hangen.
	// Juist de eerste bytes zijn wat je wilt zien als er iets misgaat.
	dock, uart := uintptr(DockChannelBase), uintptr(UART0Base)
	if dock != 0 {
		dockStalled = !put(dock+dockTXFree, func(v uint32) bool { return v != 0 }, dock+dockTX, c, dockStalled)
	}
	if uart != 0 {
		uartStalled = !put(uart+uartUTRSTAT, func(v uint32) bool { return v&utrstatTXBE != 0 }, uart+uartUTXH, c, uartStalled)
	}
}

// put wacht (begrensd) tot ready(status) en schrijft dan c naar data. Het
// budget is klein als de vorige byte al niet paste. true = geschreven.
func put(status uintptr, ready func(uint32) bool, data uintptr, c byte, stalled bool) bool {
	budget := pollMax
	if stalled {
		budget = pollStalled
	}
	for i := 0; i < budget; i++ {
		if ready(dev.Read32(status)) {
			dev.Write32(data, uint32(c))
			return true
		}
	}
	return false
}
