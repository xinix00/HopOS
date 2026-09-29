//go:build tamago && arm64

package apple

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// proxy.go — de mini-proxy: een nieuw image over de dockchannel binnenhalen en
// erin springen, zonder 1TR.
//
// WAAROM. Sinds wij zelf het bootobject zijn, kost élk nieuw image een fysieke
// reis naar de machine: kmutil draait alleen in 1TR, want macOS boot niet meer
// (dat volume boot ons). Meten is gratis geworden — elke power-on draait wat er
// staat — maar veranderen is duur. Dit is wat m1n1's proxy voor ons was, nu in
// ons eigen image: installeer dit één keer, en daarna reist elk volgend image
// over de kabel.
//
// HOE. De dockchannel is een FIFO in twee richtingen; de console gebruikte tot
// nu toe alleen de schrijfkant (console.go). De leeskant is DATA_RX_COUNT +
// DATA_RX8 (m1n1's dockchannel_uart.c), en daar leest Serve een piepklein
// protocol uit: een vaste kop van 32 bytes, en bij LOAD de bytes erachteraan.
//
//	magic  8   "HOPPRX01"
//	cmd    1   'L' load, 'J' jump, 'P' ping
//	pad    7   nul
//	addr   8   little-endian
//	len    8   little-endian
//
// De sprong gaat naar addr+0x800 — de stub-entry van het geladen image, exact
// waar iBoot ook aflevert — met x0 = de boot_args die wij van de firmware
// kregen. Het nieuwe image verplaatst zichzelf dan naar zijn linkadres en boot
// alsof het net geïnstalleerd was. Dat pad is al bewezen (AT=-test, 29-08).
//
// WAT DIT NIET IS. Geen beveiliging, geen hersteltruc: wie aan deze kabel zit,
// zit fysiek aan de machine en kan hem sowieso in DFU zetten. En het vervangt
// het bootobject NIET — na een power-cycle draait weer wat er geïnstalleerd
// staat. Dat is precies de bedoeling: de installatie blijft het anker, de proxy
// is de werkbank.

const (
	dockRX      = 0x401c // DATA_RX8 — de byte staat in bits 15:8
	dockRXCount = 0x402c // DATA_RX_COUNT

	proxyMagic  = "HOPPRX01"
	proxyHdrLen = 32
	proxyEntry  = 0x800 // stubEntry: waar de firmware een bootobject aflevert

	// Bovengrens op één LOAD. Ruim boven een agent-image (6MB) en ver onder de
	// scratch-regio, zodat een verminkte lengte niet het halve DRAM overschrijft.
	proxyMaxLen = 64 << 20

	// Hoeveel opeenvolgende BEURTEN zonder een byte we accepteren voordat we
	// een overdracht laten vervallen. Beurten, geen tijd: de aanroeper bepaalt
	// het tempo, en die weet zelf hoe vaak hij langskomt.
	proxyGiveUp = 400
)

// ProxyScratch is waar een binnengehaald image landt: de partitie-pool van dit
// board (PoolBase), dus geheugen dat ONS plan zelf uitdeelt.
//
// Het stond eerst op 2GB boven de DRAM-basis — hetzelfde adres als de
// AT=-test die de relocatie-stub bewees — en dat is precies het soort adres
// waarvan je aanneemt dat het vrij is omdat er niets van jou staat. Gemeten
// 30-08: een schrijf daarheen eindigde in een externe abort die pas op de
// eerstvolgende device-lees landde (ESR 0x02000000 met FAR = het RX-register
// van de dockchannel — de uitgestelde-abort-les uit de PCIe-bring-up, nu op
// het geheugenpad). Wat vrij lijkt in het DRAM is niet vrij; wat ons plan
// uitdeelt, is dat wel. De probe gebruikt de pool niet, dus daar staat een
// binnengehaald image niemand in de weg.
const ProxyScratch = PoolBase

// proxyBoot staat in proxy_arm64.s: MMU uit en springen. Keert nooit terug.
func proxyBoot(entry, x0 uint64)

var (
	proxyBuf  [4096]byte
	proxyHdr  [proxyHdrLen]byte
	proxyHave int

	// Wat de laatste LOAD neerzette: de sprong schoont precies dát op, en niet
	// een ruime gok over het halve DRAM.
	proxyLast, proxyLastLen uint64
	proxySum                uint32

	// De lopende overdracht. Een LOAD is GEEN lus die wacht tot alles binnen
	// is: hij is een toestand waar elke ProxyPoll een hap uit neemt.
	//
	// Dat is de derde vorm van dit stuk code en de eerste die blijft staan. De
	// vorige twee bleven zelf in een lus hangen tot de overdracht klaar was —
	// eerst spinnend, daarna met time.Sleep en runtime.Gosched ertussen — en
	// allebei namen ze de node mee: "semacquire not on the G stack", met de
	// opruim-goroutine van de runtime in het spoor (mcleanup.go). De les is
	// niet welke wachtvorm de juiste was, maar dat een bare-metal driver de
	// scheduler niet seconden mag bezetten. Wie de klok wil, gebruikt die van
	// de aanroeper: de hartslag van de probe pollt en slaapt al.
	rxBusy bool
	rxAddr uint64 // waar de volgende hap landt
	rxLeft uint64 // hoeveel er nog moet komen
	rxSum  uint32
	rxIdle int // opeenvolgende beurten zonder één byte
)

// rxCount/rxByte zijn de leeskant van de dockchannel. Vaste adressen, zelfde
// afweging als in console.go: dit moet werken vóórdat er iets anders werkt.
func rxCount() uint32 {
	if DockChannelBase == 0 {
		return 0
	}
	return dev.Read32(uintptr(DockChannelBase) + dockRXCount)
}

func rxByte() byte {
	return byte(dev.Read32(uintptr(DockChannelBase)+dockRX) >> 8)
}

// proxyRaw zet één teken op de dockchannel ZONDER de console-stack (conlog,
// fmt, de ring): geen allocatie, geen lock, geen enkele runtime-afspraak. Dat
// is precies waarom hij bestaat — de eerste versie van dit bestand stierf op
// "semacquire not on the G stack" en nam de node mee, en dan wil je merktekens
// die niet zelf van de runtime afhangen. Eén teken per stap, meer niet.
func proxyRaw(c byte) {
	if DockChannelBase == 0 {
		return
	}
	base := uintptr(DockChannelBase)
	for i := 0; i < 20000; i++ {
		if dev.Read32(base+dockTXFree) != 0 {
			dev.Write32(base+dockTX, uint32(c))
			return
		}
	}
}

// proxyWritable schrijft één woord op addr en leest het terug. Geen aanname,
// een proef: op dit silicium is een verboden schrijfactie stil en komt de
// rekening pas bij de eerstvolgende lees (zie ProxyScratch). Een overdracht van
// megabytes beginnen op geheugen dat we niet mogen hebben, kost de hele node —
// dus eerst één woord, en pas dan de rest.
func proxyWritable(addr uint64) bool {
	const probe = 0x5A5A_1234_ABCD_0007
	dev.Write64(uintptr(addr), probe)
	dev.MB()
	return dev.Read64(uintptr(addr)) == probe
}

// ProxyPoll kijkt of er een commando klaarstaat en voert het uit. Aanroepen in
// een lus die toch al draait (de hartslag van de probe); hij keert meteen terug
// als er niets ligt, dus hij kost niets als er niemand aan de kabel zit.
//
// Terugkeren doet hij ook ná een LOAD. Alleen JUMP keert nooit terug.
func ProxyPoll() {
	if rxBusy {
		proxyChunk()
		return
	}
	// Eén hap kop-zoeken per beurt is ruim: een kop is 32 bytes en de
	// aanroeper komt honderden keren per seconde langs.
	for n := 0; rxCount() > 0 && n < len(proxyBuf); n++ {
		c := rxByte()
		// Magic zoeken, byte voor byte. Klopt de volgende byte niet, dan
		// beginnen we opnieuw — maar wél met déze byte als mogelijke start,
		// anders mist een herhaalde 'H' de echte kop.
		if proxyHave < len(proxyMagic) {
			if c == proxyMagic[proxyHave] {
				proxyHdr[proxyHave] = c
				proxyHave++
			} else if c == proxyMagic[0] {
				proxyHdr[0] = c
				proxyHave = 1
			} else {
				proxyHave = 0
			}
			continue
		}
		proxyHdr[proxyHave] = c
		proxyHave++
		if proxyHave < proxyHdrLen {
			continue
		}
		proxyHave = 0
		proxyRun(proxyHdr[8], le64(proxyHdr[16:]), le64(proxyHdr[24:]))
		if rxBusy {
			return // de rest van deze kop is payload; die haalt proxyChunk op
		}
	}
}

// proxyChunk neemt één hap uit een lopende overdracht: hooguit één bufferlengte,
// altijd eindig, altijd terugkerend. Komt er een tijd lang niets, dan is de
// zender weg en vervalt de overdracht — anders blijft de node wachten op bytes
// die nooit komen.
func proxyChunk() {
	n := 0
	for n < len(proxyBuf) && rxLeft > 0 && rxCount() > 0 {
		proxyBuf[n] = rxByte()
		n++
		rxLeft--
	}
	if n == 0 {
		if rxIdle++; rxIdle > proxyGiveUp {
			rxBusy = false
			proxyRaw('?')
		}
		return
	}
	rxIdle = 0
	dev.Copy(uintptr(rxAddr), proxyBuf[:n])
	for _, b := range proxyBuf[:n] {
		rxSum = rxSum*31 + uint32(b)
	}
	rxAddr += uint64(n)
	proxyRaw('.')
	if rxLeft == 0 {
		rxBusy = false
		proxyLast, proxyLastLen, proxySum = rxAddr-proxyLastLen, proxyLastLen, rxSum
		proxyRaw('>')
		fmt.Printf("proxy: loaded %d bytes at %#x sum %#x\n", proxyLastLen, proxyLast, rxSum)
	}
}

func le64(b []byte) uint64 {
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func proxyRun(cmd byte, addr, length uint64) {
	switch cmd {
	case 'P':
		fmt.Printf("proxy: ready — scratch %#x, writable=%v\n",
			uint64(ProxyScratch), proxyWritable(ProxyScratch))
	case 'L':
		if addr == 0 {
			addr = ProxyScratch
		}
		// BEREIK-check, geen proefschrijfactie. De schrijf/lees-proef leek de
		// nette wacht, maar op dit silicium is een verboden schrijf STIL en
		// landt de abort pas op een latere lees (SError) — tegen de tijd dat
		// de proef "nee" zou zeggen, is de chaos al begonnen (gemeten 30-08:
		// de zender stuurde het oude scratch-adres in firmware-geheugen, en de
		// node stierf in de runtime vóór één eerlijke foutregel). Dus: alleen
		// adressen die aantoonbaar in ÓNZE pool liggen, al het andere is een
		// luide weigering zonder één schrijfactie.
		if addr < ProxyScratch || addr+length > ProxyScratch+proxyMaxLen {
			fmt.Printf("proxy: refusing %#x+%#x — outside the scratch window %#x+%#x\n",
				addr, length, uint64(ProxyScratch), uint64(proxyMaxLen))
			return
		}
		proxyLoad(addr, length)
	case 'J':
		if addr == 0 {
			addr = proxyLast
		}
		if addr == 0 {
			addr = ProxyScratch
		}
		n := proxyLastLen
		if addr != proxyLast || n == 0 {
			n = proxyMaxLen // gesprongen zonder eigen LOAD: veilig ruim vegen
		}
		fmt.Printf("proxy: jumping into the image at %#x (entry %#x)\n", addr, addr+proxyEntry)
		Chainload(addr, n)
	default:
		fmt.Printf("proxy: unknown command %q\n", string(rune(cmd)))
	}
}

// proxyLoad zet een overdracht op; het binnenhalen doet proxyChunk, hap voor
// hap. Zie de opmerking bij rxBusy voor waarom dit géén lus meer is.
func proxyLoad(addr, length uint64) {
	if length == 0 || length > proxyMaxLen {
		proxyRaw('!')
		return
	}
	rxBusy, rxAddr, rxLeft, rxSum, rxIdle = true, addr, length, 0, 0
	proxyLastLen = length
	proxyRaw('<')
}

// Chainload springt in een image dat op addr klaarligt: cache-onderhoud over
// de n bytes die er staan, en dan de machine teruggeven zoals de firmware hem
// aflevert — MMU uit, x0 = boot_args, binnenkomst op de stub-entry.
//
// Dit is het bring-up-pad: een image dat over de m1n1-proxykabel binnenkwam.
// Het is iets ANDERS dan de kern-flip (docs/kern-flip.md), en dat is geen
// dubbeling maar een ander soort sprong: hier gaat een compleet .img met de
// apple-bootstub vooraan naar zijn eigen linkadres, terwijl de flip een al
// geplaatste ELF in een geleend venster inspringt en zijn bewoners meeneemt.
// De eerste is gereedschap voor een bord dat nog niets kan, de tweede is hoe
// een draaiende node zichzelf vervangt.
//
// Keert nooit terug.
func Chainload(addr, n uint64) {
	// De console eerst leeg laten lopen: na de sprong is dit image weg.
	time.Sleep(50 * time.Millisecond)
	dev.CleanInv(uintptr(addr), uintptr(n))
	proxyBoot(addr+proxyEntry, FirmwareX0())
}
