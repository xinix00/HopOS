//go:build tamago

// Package memlimit geeft élke Go-wereld op HopOS — HOP zelf, de apploader,
// elke app — automatisch een geheugenplafond dat past bij het raam waarin hij
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
// allocator-groeistap te dekken die de zachte limiet overschrijdt. Empirisch —
// het is de waarde waarmee HopOS sinds 02-08 draait en waaronder de staging
// aantoonbaar sneuvelt (zie ArmBelow).
const arenaSlack = 4 << 20

// anchor dwingt de meet-allocatie het echte heap in (escape-analyse mag hem
// niet op een stack leggen — al ligt ook die op tamago binnen het raam).
var anchor *byte

// base is de heapbodem, bij de eerste Arm vastgelegd. Nul zolang Arm niet liep.
var base uintptr

// Arm zet het plafond. Zo vroeg mogelijk aanroepen — de agent-main en
// applib.Init doen dat al; een nieuwe main hoort dit als eerste te doen.
// Faalt de berekening (raam onbekend of absurd klein), dan doet Arm bewust
// niets: geen limiet is de oude, bekende toestand — een verzonnen limiet is
// een nieuwe manier om stuk te gaan.
func Arm() {
	// Het anker bij de EERSTE meting ís de heapbasis: de sbrk-allocator groeit
	// alleen omhoog vanaf einde image+BSS, dus wie vroeg meet meet de bodem.
	// Die onthouden we, want een LATERE meting geeft een willekeurige plek in
	// de al bevolkte heap — en dan is er geen bodem meer te vinden.
	anchor = new(byte)
	base = uintptr(unsafe.Pointer(anchor))
	arm(uintptr(ramStart)+uintptr(ramSize)-uintptr(ramStackOffset), base, true, arenaSlack)
}

// ArmBelow doet hetzelfde als Arm, maar met een LAGERE muur: de heap mag nooit
// voorbij top groeien. Nodig zodra er iets ánders dan de runtime in het raam
// woont, en dat is precies één geval — de apploader die een image tegen de
// bovenkant van zijn partitie stageert (applib.StageImage).
//
// WAAROM DIT MOET BESTAAN (gemeten Pi 5, 06-08). Zonder dit rekent Arm zijn
// muur op RamStart+RamSize−RamStackOffset, en RamStackOffset is 0x100: dus
// letterlijk de bovenkant. De loader kreeg daarmee een plafond in een raam
// waarvan de bovenste megabytes al bezet waren, groeide er tijdens de
// HTTPS-download doorheen, en Go NULT een verse span — HOP las daarna
// `bad magic number '[0 0 0 0]'` op de ELF-header. Geen kapotte download, geen
// te kleine partitie: een heap die mocht wat niet kon.
//
// Hier rekenen we met de ONTHOUDEN basis en tellen we m.Sys NIET mee: alle
// runtime-geheugen leeft in [basis, muur), dus dat is het budget — punt.
//
// De MARGE blijft groot, en dat is de les van 06-08. Ik had hem naar 1MB
// gebracht met het argument "de basis is nu exact, dus de meetfout-marge mag
// weg". Dat argument klopt en de conclusie niet: SetMemoryLimit is een ZACHTE
// limiet (Go mag eroverheen als hij niet genoeg kan vrijmaken) en de allocator
// groeit in stappen die pas ná de toets gezet worden. Onder een muur die een
// ECHT adres is — hier de staging met de gedownloade image erin — moet de marge
// dus minstens één zo'n stap dekken, anders springt één arena-groei eroverheen
// en nult Go de verse span dwars door de image.
//
// GEMETEN in QEMU met per-256KB-hashes over de staging: met 1MB marge werden
// elf van de blokken ná de download genuld; met arenaSlack geen enkel. De
// apploader plaatste daarna een lege app, die core spinde op 100% zonder ooit
// te yielden, en zijn buurman kon er niet meer bij — "core never yielded".
//
// Geeft het gezette plafond terug, en ok=false als er onder top niets te
// verdelen valt (dan is er niets gezet en moet de aanroeper luid falen — een
// verzonnen limiet is een nieuwe manier om stuk te gaan).
func ArmBelow(top uintptr) (limit uint64, ok bool) {
	if base != 0 {
		return arm(top, base, false, arenaSlack)
	}
	anchor = new(byte)
	return arm(top, uintptr(unsafe.Pointer(anchor)), true, arenaSlack)
}

// arm is de gedeelde rekensom. addSys hoort bij een VERS gemeten anker: dan is
// heap0 de huidige uitdeelplek en telt "al gepakt" (m.Sys) mee naast "nog te
// pakken" (wall−heap0). Met de onthouden basis is [heap0, wall) al het hele
// verhaal en zou m.Sys dubbeltellen.
func arm(wall, heap0 uintptr, addSys bool, minSlack uint64) (limit uint64, ok bool) {
	if ramSize == 0 || heap0 <= uintptr(ramStart) || heap0 >= wall {
		return 0, false
	}
	budget := uint64(wall - heap0)
	if addSys {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		budget += m.Sys
	}
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
