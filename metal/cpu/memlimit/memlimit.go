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
//	limiet = budget − slack (1/16 van het budget, minimaal 4MB). De vloer is
//	         geen smaak maar de meetfout: het anker kan tot één arena-chunk
//	         (4MB) onder de werkelijke claim-top liggen, en de slack moet
//	         die onscherpte áltijd dekken. Daarboven 6%: Go's eigen
//	         richtlijn voor memory-limit-headroom is 5-10%, en "conservatief
//	         is niet hetzelfde als wat er kán" (Derek, 02-08).
//
// Op de LicheeRV rolt hier ~44MB uit (raam 64MB, image ~13MB) — precies de
// waarde die het cycle-protocol op 02-08 als stabiel bewees (arm H: nul doden
// waar élke eerdere arm er ~50% had).
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
	slack := budget / 16
	if slack < minSlack {
		slack = minSlack
	}
	if slack >= budget {
		return 0, false
	}
	limit = budget - slack
	debug.SetMemoryLimit(int64(limit))
	// Eén regel zelfconfiguratie in de console-historie, net als de
	// netwerk-identiteit: als een node ooit tóch tegen zijn plafond aan
	// GC-stormt, is dít het getal dat de operator wil kennen.
	fmt.Printf("mem: Go memory limit %dMB (window %dMB, image+bss %dMB)\n",
		limit>>20, uintptr(ramSize)>>20, (heap0-uintptr(ramStart))>>20)
	return limit, true
}
