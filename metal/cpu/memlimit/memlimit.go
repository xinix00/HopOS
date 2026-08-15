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
//	basis  = einde image+BSS: runtime.DataRegion() (firstmoduledata.enoptrbss)
//	         — exact waar tamago's sbrk-allocator begint (mem_sbrk.go:
//	         initBloc → firstmoduledata.end, luttele bytes hoger). Gemeten,
//	         niet geschat, en zonder één allocatie.
//	budget = muur − basis                          (de hele arena)
//	limiet = budget − slack, slack = max(10% van budget, 4MB)
//
// Dit verving op 15-08 een meting via een anker-allocatie. Die was twee keer
// fout: een klein anker landt in een bestaande span tot megabytes onder de
// sbrk-top en de som (muur−anker)+Sys telde dat stuk dubbel — limiet 34MB op
// een arena van ~30MB, drie sbrk-OOM's ondanks limiet (de GC joeg op een
// doel dat fysiek niet bestaat). En de verbeterde vorm (een 4MB-probe, één
// groeichunk, meet de top wél exact) was zelf de moord op kleine vensters:
// virtual@20MB heeft na image+BSS maar ~8MB arena, en 4MB probe bovenop 4MB
// init-heap = "cannot allocate 4194304-byte block (4030464 in use)" — en dat
// vóór de exit-haak, dus als stille gijzeling van de gedeelde hart (QEMU-
// gereproduceerd). DataRegion heeft geen van beide problemen.
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
	"runtime/metrics"
	"time"
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

// Arm zet het plafond. Zo vroeg mogelijk aanroepen — de agent-main en
// applib.Init doen dat al; een nieuwe main hoort dit als eerste te doen.
// Faalt de berekening (raam onbekend of absurd klein), dan doet Arm bewust
// niets: geen limiet is de oude, bekende toestand — een verzonnen limiet is
// een nieuwe manier om stuk te gaan.
func Arm() {
	// De sbrk-basis komt uit de runtime zelf: einde image+BSS. Niets meten
	// met allocaties — zie de pakketdoc voor de twee manieren waarop dat
	// op 15-08 fout bleek (dubbeltelling én een 4MB-probe die kleine
	// vensters zelf om zeep hielp).
	_, dataEnd := runtime.DataRegion()
	arm(uintptr(ramStart)+uintptr(ramSize)-uintptr(ramStackOffset), uintptr(dataEnd), arenaSlack)
}

// arm is de rekensom: basis = einde image+BSS (de sbrk-start), dus
// muur − basis ís de hele arena — niets erbij op te tellen.
func arm(wall, base uintptr, minSlack uint64) (limit uint64, ok bool) {
	if ramSize == 0 || base <= uintptr(ramStart) || base >= wall {
		return 0, false
	}
	budget := uint64(wall - base)
	// 10% is Go's eigen richtlijn voor headroom onder een memory limit; de vloer
	// eronder dekt wat niet meeschaalt (één arena-groeistap, plus de luttele
	// bytes tussen enoptrbss en de echte sbrk-start). Eén regel voor élke
	// Go-wereld op HopOS, kern én app: de marge
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
		limit>>20, gogc, uintptr(ramSize)>>20, (base-uintptr(ramStart))>>20)
	go watch(limit)
	return limit, true
}

// Het venster en de twee-slagen-regel van de thrash-wachter. Vijf seconden is
// ruim boven élke gezonde GC-cadans (gemeten 11-08: 22 GC's in 215s onder
// echte last), en twee opeenvolgende hete vensters filteren een eenmalige
// piek eruit: pas tien seconden aaneengesloten kap-raken is een vonnis.
const (
	thrashWindow  = 5 * time.Second
	thrashStrikes = 2
)

// watch maakt een GC-doodspiraal LUID. Zit de live heap tegen het plafond,
// dan GC't de runtime continu zonder ooit iets terug te winnen: 100% compute,
// nul voortgang, nul logs — en op een coöperatief gedeelde core ook nul
// yields, dus de buren verhongeren mee (gemeten 14/15-08, LicheeRV: een
// 20MB-venster met een TLS-lading gijzelde zo een hele hart, urenlang). De
// runtime kent geen "geef op": de limiet is zacht en hij blijft proberen.
//
// De diagnose komt van de runtime zélf: de GC-CPU-limiter slaat aan zodra de
// GC boven de helft van de CPU uitkomt — precies de spiraal-conditie, en
// onhaalbaar voor gezonde churn (kleine live set = goedkope cycli, hoe hard
// er ook gealloceerd wordt). Slaat hij thrashStrikes vensters op rij aan,
// dan is doorgaan zinloos en is een luide dood de dienst aan de operator:
// de panic haalt de console/het task-log, de monitor herstart de taak, en
// een verkeerd gedimensioneerde job wordt een leesbare crash-loop in plaats
// van een stille gijzeling. Voor HOP zelf geldt hetzelfde vonnis bewust:
// HOP-leven = node-leven, de watchdog maakt de herstart af.
func watch(limit uint64) {
	samples := []metrics.Sample{
		{Name: "/gc/limiter/last-enabled:gc-cycle"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	hot := 0
	var prevCycles uint64
	for {
		time.Sleep(thrashWindow)
		metrics.Read(samples)
		limiterLast := samples[0].Value.Uint64()
		cycles := samples[1].Value.Uint64()
		// Limiter aan in een cyclus van DIT venster = de GC zat boven zijn
		// CPU-kap terwijl wij sliepen. (Eerste venster: prevCycles=0, dus
		// een limiter-melding uit de boot telt meteen mee — dat is goed,
		// want de spiraal begint daar het vaakst.)
		if limiterLast > 0 && limiterLast > prevCycles {
			hot++
		} else {
			hot = 0
		}
		prevCycles = cycles
		if hot >= thrashStrikes {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			msg := fmt.Sprintf("HOPOS_GC_THRASH: GC ate >50%% CPU for %s straight — live heap %dMB against a %dMB memory limit (%d GC cycles); this window is too small for this workload",
				time.Duration(thrashStrikes)*thrashWindow, m.HeapAlloc>>20, limit>>20, cycles)
			// Zonder dump panicen: een volle goroutine-dump is groter dan
			// de log-ring en drukt de kop — de diagnose — eruit (gemeten
			// 15-08: de operator hield alleen "mgc.go:1695" over). De
			// stack is hier toch betekenisloos (dit is de wachter, niet de
			// dader); de regel mét getallen is het hele verhaal.
			fmt.Printf("%s\n", msg)
			debug.SetTraceback("none")
			panic(msg)
		}
	}
}
