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

import "time"

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
