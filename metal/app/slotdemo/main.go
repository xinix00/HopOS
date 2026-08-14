// slotdemo is een app-image voor een RISC-V app-slot: een gewone tamago-app
// die in een PMP-kooi op het app-hart draait (kern/cage) en verder van
// niets weet — dát is het punt van een kooi.
//
// Wat hij doet is wat een echte HopOS-app doet: rekenen (heap, scheduler en
// timers doen dus mee) en zijn voortgang op de control page zetten, zodat HOP
// kan zien dat hij leeft zonder ook maar één byte van de app te vertrouwen.
//
// Bouwen: -tags linkramsize (board/licheerv/mem_slot.go legt RAM op de
// app-partitie) en linken ín die partitie; image/licheerv-agent.sh maakt er
// met de kooi-stub één slot-blob van.
//
// Bewust GEEN console-output: de UART is granted MMIO die we met HOP delen, en
// twee schrijvers op één 16550 geven rommel (gemeten 30-07). De control page
// is het kanaal; de stub zet één controlebyte neer.
package main

import (
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/board/licheerv"
)

// De control page: velden 0-5 zijn van de stub (zie image/licheerv/stub-slot),
// 6 en 7 van de app.
const (
	beatField = 6 // hartslag: loopt op zolang de app draait
	workField = 7 // waar hij is met rekenen
)

// put zet een woord op de control page zó dat HOP het écht ziet. De app draait
// met caches aan, dus de regel moet naar DRAM gecleand worden: een fence
// alleen laat hem write-back in de eigen D$ staan (gemeten 30-07 — HOP las dan
// stale nullen). Zelfde contract als de ring-buffers op ARM: expliciet
// cache-onderhoud rond elke gedeelde regel, want de clusters zijn niet
// coherent.
func put(field int, v uint64) {
	addr := uintptr(licheerv.StubMbox + uint64(field*8))
	*(*uint64)(unsafe.Pointer(addr)) = v
	licheerv.CacheClean(addr, 8)
}

func main() {
	primes := make([]int, 0, 4096)

	for n, beat := 2, uint64(0); ; n++ {
		isPrime := true
		for _, p := range primes {
			if p*p > n {
				break
			}
			if n%p == 0 {
				isPrime = false
				break
			}
		}
		if isPrime {
			if len(primes) < cap(primes) {
				primes = append(primes, n)
			}
			if len(primes)%64 == 0 {
				beat++
				put(beatField, beat)
				put(workField, uint64(n))
				time.Sleep(10 * time.Millisecond) // coöperatief afgeven
			}
		}
		if n > 1<<30 {
			n = 1
		}
	}
}
