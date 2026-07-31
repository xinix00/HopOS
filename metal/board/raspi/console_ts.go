package raspi

// Uniforme per-regel-timestamp op de console. De HOP-agent logde al met
// "YYYY/MM/DD HH:MM:SS" (Go's log-pakket) maar onze eigen regels (dvfs/net/
// clock/…) via fmt niet — rommelig (Derek, 2026-07-11). ConsoleByte zet er
// aan het begin van elke regel één uniforme stempel voor; main zet tegelijk
// het log-pakket op vlaggen 0 zodat er nooit een dubbele stempel komt.
//
// Alleen de HOP-core roept printk aan (zie board/*/console.go), dus deze
// state is single-threaded — geen lock nodig. Alloc-vrij: de stempel wordt
// cijfer voor cijfer naar de sink geschreven, geen tijd-formatteringsbuffer.

import (
	"time"

	"hop-os/metal/driver/conlog"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/pl011"
)

// Printk is de console-byte van álle Pi-boards: de rpi4- en rpi5-hooks waren
// byte-gelijk, alleen hun UART-basis verschilt. Naar de PL011 én — zodra die
// er is — naar de fb-log-console: het beeld-kanaal voor een node zónder
// debug-kabel.
//
// ALLEEN de HOP-core (MPIDR-affiniteit 0) bezit de UART/fb. Een app-core draait
// onder stage-2 en heeft die MMIO niet in zijn kooi — een runtime-print (bv. een
// throw) zou daar een cage-fault worden die de échte oorzaak maskeert. App-cores
// laten hun runtime-output dus vallen; hun eigen logs lopen via de
// hop-ABI-ring. Masker 0xFFFFFF dekt A72-aff0 én A76-aff1.
//
// Via ConsoleByte, zodat er (indien aan) één uniforme "dd-MM HH:mm"-prefix per
// regel op UART én fb komt. Putc pollt begrensd — een dode/ongeklokte UART kost
// hooguit de poll, nooit de boot (zie metal/driver/pl011).
func Printk(uartBase uintptr, c byte) {
	if MPIDR()&0xFFFFFF != 0 {
		return // app-core: geen toegang tot de UART (kooi)
	}
	ConsoleByte(c, func(b byte) {
		// Eerst de ring: een node zonder debug-kabel moet zijn eigen
		// console alsnog over het netwerk kunnen geven (zie driver/conlog).
		conlog.Put(b)
		pl011.Putc(uartBase, b)
		fb.Putc(b)
	})
}

var (
	logTS     bool // timestamps aan? (na de boot-banner aangezet)
	lineStart = true
)

// LogTimestamps zet de per-regel-stempel aan of uit. Main zet 'm AAN ná de
// boot-banner, zodat de bunny schoon blijft.
func LogTimestamps(on bool) { logTS = on }

// ConsoleByte schrijft c naar sink, met — indien aan — een klein
// "dd-MM HH:mm "-prefix aan het begin van elke regel (Derek: kort is genoeg).
func ConsoleByte(c byte, sink func(byte)) {
	if logTS && lineStart && c != '\n' && c != '\r' {
		writeStamp(sink)
	}
	lineStart = c == '\n'
	sink(c)
}

func d2(sink func(byte), n int) { sink(byte('0' + n/10%10)); sink(byte('0' + n%10)) }

func writeStamp(sink func(byte)) {
	t := time.Now().UTC()
	_, mo, d := t.Date()
	h, mi, _ := t.Clock()
	d2(sink, d)
	sink('-')
	d2(sink, int(mo))
	sink(' ')
	d2(sink, h)
	sink(':')
	d2(sink, mi)
	sink(' ')
}
