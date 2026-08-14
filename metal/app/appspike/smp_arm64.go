//go:build arm64

package main

// smp_arm64.go — de twee rollen die ARM-registers en ARM-firmware nodig hebben:
// de SMP-bench (core-onderscheider = MPIDR) en de SMC-kooiproef (HCR_EL2.TSC).
// Ze staan hier apart zodat de referentie-app op élke arch bouwt die HopOS
// draagt; op riscv64 weigeren ze (smp_riscv64.go), en dat is een antwoord in
// plaats van een linkfout.

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/board/hopslot"
	"github.com/xinix00/HopOS/metal/cpu/psci"
)

// firmwareProbe is isolatietest 2: praat bewust met de firmware. Een app heeft
// géén legitieme SMC (zelfs SMP-bring-up loopt via HOP; exit is een HVC), dus
// HCR_EL2.TSC hoort dit als EC=0x17 op de EL2-vector te trappen — de tweede
// logregel mag nooit verschijnen.
func firmwareProbe(app *applib.App) {
	app.Logf("PROBE: SMC PSCI_VERSION vanuit de kooi — EL2 hoort dit te trappen")
	time.Sleep(100 * time.Millisecond) // logregel eerst de ring uit
	v := psci.SMC(psci.VERSION, 0, 0, 0)
	app.Logf("PROBE: firmware antwoordde %#x — GEEN SMC-kooi!", v)
}

// coreTag onderscheidt fysieke cores binnen één app: het rauwe MPIDR, per core
// gegarandeerd verschillend op elk ARM-board. CoreID is hier onbruikbaar, want
// dat is de SLOT-identiteit (slotHint) en die is voor alle cores van één app
// gelijk.
func coreTag() uint64 { return hopslot.MPIDR() }

// smpBench bewijst fase 5: de app draait op meerdere cores met één gedeelde
// heap, en heeft daar zelf niets voor hoeven doen (applib zette GOMAXPROCS).
func smpBench(app *applib.App) {
	n := runtime.GOMAXPROCS(0)
	app.Logf("SMP: app ziet %d cores (GOMAXPROCS), RAM %dMB — app-code deed hier niets voor", n, app.RAMSize>>20)
	if n < 2 {
		exitf(app, 1, "SMP: minder dan 2 cores toegewezen — geen SMP")
	}

	// 1) Parallellisme-bewijs: N CPU-drukke goroutines tegelijk; elk telt per
	// iteratie op welke core hij draaide. Zien we werk op meerdere cores, dan
	// verdeelt de runtime de goroutines écht. De core-onderscheider is het
	// rauwe MPIDR (coreTag, zie boven). Elke goroutine
	// yield't af en toe (Gosched) zodat de scheduler kan spreiden.
	var ran [12]atomic.Uint64
	var wg0 sync.WaitGroup
	const workers = 8
	for g := 0; g < workers; g++ {
		wg0.Add(1)
		go func() {
			defer wg0.Done()
			for i := 0; i < 2000; i++ {
				ran[coreTag()%uint64(len(ran))].Add(1)
				for j := 0; j < 20000; j++ {
				}
				if i%50 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	wg0.Wait()
	spread := 0
	for c := 0; c < len(ran); c++ {
		if v := ran[c].Load(); v > 0 {
			spread++
			app.Logf("SMP: core-bucket %d draaide %d iteraties", c, v)
		}
	}
	if spread < 2 {
		exitf(app, 2, "SMP: werk liep op %d core(s) — geen echt parallellisme", spread)
	}
	app.Logf("SMP: goroutines liepen parallel op %d cores — echte multi-core", spread)

	// 2) Gedeelde heap: twee goroutines vullen om-en-om dezelfde slice (één
	// adresruimte, nul berichten ertussen) en HOP zit er niet tussen. Verifieer.
	const N = 1 << 20
	shared := make([]uint32, N)
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < N; i += 2 {
				shared[i] = uint32(i)
			}
		}(g)
	}
	wg.Wait()
	var sum uint64
	for i := 0; i < N; i++ {
		if shared[i] != uint32(i) {
			exitf(app, 3, "SMP: gedeelde slice corrupt @ %d (=%d)", i, shared[i])
		}
		sum += uint64(shared[i])
	}
	app.Logf("SMP: gedeelde heap OK — %d elementen door twee cores beschreven (som %d)", N, sum)

	// 3) GC over de gedeelde heap: allocatie-druk op alle cores + een volledige
	// GC-cyclus; de stop-the-world moet elke core bereiken (ReadMemStats/GC
	// zouden anders hangen). Overleven = de coöperatieve STW werkt cross-core.
	gc0 := gcCount()
	var wg2 sync.WaitGroup
	for g := 0; g < n; g++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			var keep [][]byte
			for i := 0; i < 300; i++ {
				keep = append(keep, make([]byte, 4096))
				if len(keep) > 32 {
					keep = keep[16:]
				}
			}
			atomic.AddUint64(&smpSink, uint64(len(keep)))
		}()
	}
	wg2.Wait()
	runtime.GC()
	app.Logf("SMP: GC overleefd op de gedeelde heap (NumGC %d→%d) — cross-core STW werkt", gc0, gcCount())

	// 4) Speedup (informatief; onder emulatie variabel): zelfde werk serieel vs.
	// over n goroutines. Het rendezvous is het harde bewijs; dit is de maat.
	const W = 6_000_000
	t1 := time.Now()
	smpWork(W)
	d1 := time.Since(t1)
	t2 := time.Now()
	var wg3 sync.WaitGroup
	for g := 0; g < n; g++ {
		wg3.Add(1)
		go func() { defer wg3.Done(); smpWork(W / n) }()
	}
	wg3.Wait()
	d2 := time.Since(t2)
	app.Logf("SMP: werk serieel %v, parallel(%d) %v → %.2fx", d1, n, d2, float64(d1)/float64(d2))

	exit(app, 0)
}

//go:noinline
func smpWork(iters int) {
	var s uint64
	for i := 0; i < iters; i++ {
		s += uint64(i)*2654435761 ^ s>>13
	}
	atomic.AddUint64(&smpSink, s)
}

func gcCount() uint32 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.NumGC
}
