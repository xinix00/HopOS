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
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/v2/driver/conlog"
)

// pollInterval is hoe vaak een verbonden lezer naar nieuwe bytes kijkt. Geen
// signalering vanuit conlog.Put: dat pad loopt uit printk (ook uit het
// panic-pad) en mag niets doen dat kan blokkeren of alloceren. 100ms is voor
// meelezen met een boot niet te merken.
const pollInterval = 100 * time.Millisecond

// maxReaders begrenst het aantal gelijktijdige lezers. De listener-pool van de
// netstack is eindig (MaxListenerConns, nu 8 per listener), en die is GEDEELD
// met niets: raakt hij leeg, dan is deze poort dood — juist het kanaal dat je
// dan nodig hebt. Een dubbele marge houden we vrij zodat één stuk gelopen
// client nooit de laatste plek kan pakken.
const maxReaders = 4

var readers atomic.Int32

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
			if n := readers.Add(1); int(n) > maxReaders {
				// Vol: dat zeggen en sluiten, in plaats van de verbinding stil
				// vasthouden. Een lezer die dit ziet weet dat hij moet wachten,
				// en de pool houdt plek over voor de rest van de node.
				fmt.Fprintf(c, "console: %d readers already attached, try again later\n", maxReaders)
				c.Close()
				readers.Add(-1)
				continue
			}
			go func() {
				defer readers.Add(-1)
				stream(c)
			}()
		}
	}()
}

// stream geeft één lezer eerst de bewaarde console en daarna wat er bij komt.
// Meerdere lezers mogen tegelijk — ze hebben elk hun eigen positie, en anders
// dan bij een tty verdelen ze de bytestroom niet onderling (dát was de valkuil
// bij de seriële lijn: twee cat-processen op dezelfde tty splitsten de bytes).
func stream(c net.Conn) {
	defer c.Close()

	// LEZEN, ook al stuurt een console-lezer niets: zonder read-kant merken we
	// een weggelopen client alléén als er iets te schrijven is. Bij een stille
	// console (die-temp is één regel per minuut) blijft zo'n verbinding dus
	// eeuwig hangen mét zijn slot in de listener-pool — en na 8 daarvan is de
	// poort voorgoed dood. GEMETEN 11-08 (Derek): negen browsertabs op
	// http://node:5555 legden de console om. Een browser stuurt hier een
	// HTTP-request, snapt het rauwe antwoord niet en breekt af; die FIN kwam
	// nooit aan omdat niemand las. io.Discard, want wat een client stuurt is
	// per definitie niet voor ons — het enige dat telt is het EINDE ervan.
	go func() {
		io.Copy(io.Discard, c)
		c.Close() // EOF of fout = client weg; de Write hieronder faalt nu meteen
	}()

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
