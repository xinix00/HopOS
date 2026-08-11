//go:build tamago

// Package memlimit geeft élke Go-wereld op HopOS — HOP zelf, elke app —
// automatisch een geheugenplafond dat past bij het raam waarin hij
// draait. Niemand hoort hierover na te denken (Derek, 02-08): geen getal per
// board, geen getal per job, en een nieuw board of een kleinere partitie is
// vanzelf goed.
//
// Waarom dit bestaat (gemeten 02-08, LicheeRV): Go's default GC-beleid
// verdubbelt zijn heap-doel per ronde en kent geen muur. In een klein vast
// raam eindigt dat als "fatal error: out of memory" — en die haalt alleen een
// UART, want de TCP-console sterft mee: van buiten een volledige stille
// node-dood. Drie zwarte dozen vingen hem identiek: alloc ~34,9MB, Sys
// 39.780KB, pas drie GC's op ~56s — Sys + image was exact het raam, en de
// arena-groei voor ronde vier was de doodsteek. Het was nooit een lek; het
// was ontbrekende informatie. SetMemoryLimit ís die informatie: de runtime
// GC't vanzelf harder naarmate hij het plafond nadert en komt er nooit
// doorheen — een app die even meer wil dan mag, wordt afgeremd in plaats van
// ter dood gebracht.
//
// Alles komt uit wat de wereld al over zichzelf weet:
//
//	muur   = RamStart + RamSize − RamStackOffset   (runtime/goos — per board
//	         gelinkt, per app door HOP gepatcht: de RAM-declaratie)
//	heap0  ≈ het adres van een vroege heap-allocatie. tamago's allocator is
//	         een sbrk die alleen omhoog groeit vanaf einde image+BSS
//	         (runtime/mem_sbrk.go: initBloc → firstmoduledata.end), dus elk
//	         vroeg heap-adres markeert die basis — hooguit een paar honderd
//	         KB te hoog, en die fout maakt het plafond alleen maar LAGER.
//	         (runtime.bloc zelf is sinds go1.23 geen geldig linkname-doel.)
//	budget = (muur − heap0) + MemStats.Sys         (nog te pakken + al gepakt)
//	limiet = budget − slack, slack = max(10% van budget, 4MB)
//
// 10% is Go's eigen richtlijn voor headroom onder een memory limit (5-10%), en
// "conservatief is niet hetzelfde als wat er kán" (Derek, 02-08). De vloer van
// 4MB is geen smaak maar de meetfout: het anker kan tot één arena-chunk onder de
// werkelijke claim-top liggen, en de slack moet die onscherpte áltijd dekken.
//
// **Eén regel voor élke wereld, kern én app** — de marge hoort bij het raam, niet
// bij wie erin woont. Was de fractie 1/16 (≈6%), dan gaf dat op de LicheeRV een
// limiet van 47MB op 47MB bruikbare ruimte: nul marge, en dood.
//
// Naast de marge staat het TEMPO (zie arm()): in een smal raam is Go's
// verdubbel-regel de echte oorzaak, en die is met GOGC te sturen. Dat is
// belangrijk, want het alternatief was allocaties kleiner maken en dat kost
// doorvoer op élk board — ook op de boards die geheugen genóeg hebben.
//
// Let op: deze doc noemde ~44MB bij een image van ~13MB, maar de bank meet nu
// image+bss = 17MB — het image is met de netstack-flip ~4MB gegroeid en dat komt
// rechtstreeks van de heap af.
package memlimit

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"unsafe"
	_ "unsafe" // go:linkname naar de goos-ramen hieronder
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint

// arenaSlack is de ondergrens van de marge onder de muur: genoeg om één
// allocator-groeistap te dekken die de zachte limiet overschrijdt.
// SetMemoryLimit is ZACHT (Go mag eroverheen als hij niet genoeg kan
// vrijmaken) en de allocator groeit in stappen die pas ná de toets gezet
// worden — een kleinere marge liet de heap aantoonbaar door een echte muur
// springen (gemeten 06-08, per-256KB-hashes: 1MB marge = elf genulde blokken;
// deze waarde = nul). Empirisch de waarde waarmee HopOS sinds 02-08 draait.
const arenaSlack = 4 << 20

// tightWindow is de grens waaronder Go's verdubbel-regel niet meer past, en
// tightGCPercent het tempo dat daar wél werkt. Zie de toelichting in arm(): 128MB
// omdat élk board dat HopOS draait daaronder een HOP-raam van 64MB of minder
// heeft (LicheeRV, en de app-partities), en daarboven verdubbelen efficiënt is.
const (
	tightWindow    = 128 << 20
	tightGCPercent = 25
)

// anchor dwingt de meet-allocatie het echte heap in (escape-analyse mag hem
// niet op een stack leggen — al ligt ook die op tamago binnen het raam).
var anchor *byte

// Arm zet het plafond. Zo vroeg mogelijk aanroepen — de agent-main en
// applib.Init doen dat al; een nieuwe main hoort dit als eerste te doen.
// Faalt de berekening (raam onbekend of absurd klein), dan doet Arm bewust
// niets: geen limiet is de oude, bekende toestand — een verzonnen limiet is
// een nieuwe manier om stuk te gaan.
func Arm() {
	// Het anker bij een vroege meting ís de heapbasis: de sbrk-allocator
	// groeit alleen omhoog vanaf einde image+BSS.
	anchor = new(byte)
	base := uintptr(unsafe.Pointer(anchor))
	arm(uintptr(ramStart)+uintptr(ramSize)-uintptr(ramStackOffset), base, arenaSlack)
}

// arm is de rekensom: heap0 is de zojuist gemeten uitdeelplek, dus "al
// gepakt" (m.Sys) telt mee naast "nog te pakken" (wall−heap0).
func arm(wall, heap0 uintptr, minSlack uint64) (limit uint64, ok bool) {
	if ramSize == 0 || heap0 <= uintptr(ramStart) || heap0 >= wall {
		return 0, false
	}
	budget := uint64(wall - heap0)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	budget += m.Sys
	// 10% is Go's eigen richtlijn voor headroom onder een memory limit; de vloer
	// eronder dekt wat niet meeschaalt (één arena-groeistap plus de meetfout van
	// het anker). Eén regel voor élke Go-wereld op HopOS, kern én app: de marge
	// hoort bij het RAAM, niet bij wie erin woont (Derek, 11-08). Heeft een
	// wereld méér marge nodig, dan alloceert hij te grof — dat hoort daar
	// gefixt te worden en niet hier.
	slack := budget / 10
	if slack < minSlack {
		slack = minSlack
	}
	if slack >= budget {
		return 0, false
	}
	limit = budget - slack
	debug.SetMemoryLimit(int64(limit))

	// En dan het tempo, want een limiet alleen is niet genoeg. Go's default is
	// "ruim op als de heap VERDUBBELT" — een relatieve regel, terwijl ons plafond
	// absoluut en dichtbij is. Bij een live set van 20MB wacht hij dus tot 40MB,
	// en dat duurt bij echte last ruim een minuut: de GC zit niet lui te wezen,
	// hij volgt zijn regel (Derek, 11-08). In een smal raam is die regel gewoon
	// verkeerd: verdubbelen past er niet in.
	//
	// GEMETEN op de QEMU-bank, HOP's LicheeRV-raam met 128KiB TCP-buffers en de
	// last die een board doodde: GOGC 100 → piek 41,6MB en OOM na 151s (3 GC's);
	// GOGC 50 → piek 36,3MB, overleeft maar met bijna geen lucht; GOGC 25 → piek
	// 30,6MB, Sys 32,4MB, 15MB over.
	//
	// En die extra rondes kosten hier niets: 22 GC's in 215s met 3,7ms
	// stop-the-world totaal, dus 168µs per ronde (0,002% van de tijd). Dat komt
	// doordat GC-werk met levende POINTERS schaalt en niet met bytes — deze heap
	// is vooral []byte-buffers, en die staan pointer-vrij in noscan-spans. Draait
	// er ooit een wereld met veel kleine pointer-rijke objecten, dan gaat die
	// vlieger niet op en is dit getal opnieuw een meting waard.
	//
	// Alleen bij een smal raam, want daarboven is verdubbelen efficiënt en is de
	// extra GC-tijd weggegooid. De grens leest het venster zelf uit — geen getal
	// per board, geen getal per job.
	gogc := 100
	if limit < tightWindow {
		gogc = tightGCPercent
		debug.SetGCPercent(gogc)
	}
	// Eén regel zelfconfiguratie in de console-historie, net als de
	// netwerk-identiteit: als een node ooit tóch tegen zijn plafond aan
	// GC-stormt, zijn dít de twee getallen die de operator wil kennen.
	fmt.Printf("mem: Go memory limit %dMB, GOGC %d (window %dMB, image+bss %dMB)\n",
		limit>>20, gogc, uintptr(ramSize)>>20, (heap0-uintptr(ramStart))>>20)
	return limit, true
}
