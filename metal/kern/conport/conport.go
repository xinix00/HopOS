// Package conport serveert de console van de node op een TCP-poort: verbind en
// je ziet hetzelfde als op de seriële lijn, eerst de geschiedenis en daarna live
// verder. Bewust úit tenzij de config hem aanzet.
//
// Waarom dit bestaat: op een headless board is de UART de enige plek waar HOP
// zijn eigen redenering kwijt kan, en dat kanaal is niet te vertrouwen. Zit er
// geen kabel aan, dan bestaat de reden waarom een job niet start nergens; zit er
// wél een kabel aan, dan verliest de lijn bytes op 115200 — gemeten 31-07 kwam
// een misa-waarde binnen als "rv128 …0094112d", alleen leesbaar doordat de
// extensieletters de hex bevestigden. Een node hoort te kunnen vertellen wat er
// net gebeurde zonder dat iemand er fysiek naast zit.
//
// Waarom een poort en geen API-endpoint: dit is de console van HopOS, niet van
// HOP. Hij moet er zijn vóórdat de agent staat en blijven als die valt, en hij
// hoort niet achter dezelfde auth te zitten als het job-vlak — het is een kabel,
// geen API. Vandaar ook rauwe TCP en geen HTTP: `nc node 5555` is precies wat een
// seriële terminal was.
//
// En waarom uitzetbaar: deze poort geeft élke lezer de volledige console van de
// node, inclusief alles wat HOP over zijn jobs zegt. Dat is diagnose-gemak op een
// bank-netwerk en een lek daarbuiten. Geen default, dus: geen `hopos.console`
// in de config = geen poort.
package conport

import (
	"fmt"
	"net"
	"time"

	"hop-os/metal/driver/conlog"
)

// pollInterval is hoe vaak een verbonden lezer naar nieuwe bytes kijkt. Geen
// signalering vanuit conlog.Put: dat pad loopt uit printk (ook uit het
// panic-pad) en mag niets doen dat kan blokkeren of alloceren. 100ms is voor
// meelezen met een boot niet te merken.
const pollInterval = 100 * time.Millisecond

// Serve start de console-poort en keert meteen terug; de listener draait in een
// eigen goroutine. port 0 = uit (en dat is de default: zonder config-sleutel
// wordt Serve niet eens aangeroepen).
//
// Een fout bij het openen is GEEN reden om de node te laten falen: de console is
// gemak, geen functie. Hij meldt het en de node draait door.
func Serve(port int) {
	if port <= 0 {
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		fmt.Printf("console: port %d unavailable (%v) — serial line only\n", port, err)
		return
	}
	fmt.Printf("console: also on tcp/%d — HOPOS_CONPORT_UP\n", port)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				// De listener is weg (netstack omlaag): stoppen, niet rondtollen.
				fmt.Printf("console: accept on tcp/%d failed (%v) — port closed\n", port, err)
				return
			}
			go stream(c)
		}
	}()
}

// stream geeft één lezer eerst de bewaarde console en daarna wat er bij komt.
// Meerdere lezers mogen tegelijk — ze hebben elk hun eigen positie, en anders
// dan bij een tty verdelen ze de bytestroom niet onderling (dát was de valkuil
// bij de seriële lijn: twee cat-processen op dezelfde tty splitsten de bytes).
func stream(c net.Conn) {
	defer c.Close()

	// Beginnen bij het oudste dat nog bewaard is, niet bij nul: dan krijgt een
	// lezer die na de boot verbindt alsnog de hele geschiedenis die er ís.
	seen := conlog.Dropped()
	for {
		data, next := conlog.Since(seen)
		if len(data) > 0 {
			if _, err := c.Write(data); err != nil {
				return // lezer weg
			}
			seen = next
		}
		time.Sleep(pollInterval)
	}
}
