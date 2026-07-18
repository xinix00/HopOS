// bench.go: de 1-core-prestatierol (BENCH=1) — "krijgt een app-core de volle
// N1?" (Derek, 18-07). Vier micro-kernels die samen klok, pijplijn, cache en
// DRAM bewijzen; elke ronde opnieuw gelogd, zodat een later aangehaakte
// log-lezer gewoon de volgende ronde vangt. Dezelfde kernels draaien als
// host-referentie op de Mac (scratchpad/benchhost) — de vergelijking plus de
// bekende N1-karakteristieken geven het oordeel "full performance of niet".
//
//   - addchain: 1 afhankelijke 64-bit add per iteratie ≈ 1 cycle/iter op elke
//     moderne arm64 → iters/s ≈ de kloksnelheid (de GHz-schatter);
//   - lcg: afhankelijke mul+add-keten → de multiplier-latentie (pijplijn);
//   - copy: 24MB→24MB kopieën → bandbreedte (bewijst D-cache áán en DRAM-tempo);
//   - chase: pointer-jacht door 24MB → geheugenlatentie in ns (DRAM/SLC).
package main

import (
	"runtime"
	"time"

	"hop-os/metal/app/applib"
)

// benchSink houdt de resultaten levend zodat de compiler de kernels niet
// wegoptimaliseert.
var benchSink uint64

func benchAddChain(iters uint64) float64 {
	t0 := time.Now()
	var x uint64
	for i := uint64(0); i < iters; i++ {
		x += i
	}
	benchSink += x
	return float64(iters) / time.Since(t0).Seconds()
}

func benchLCG(iters uint64) float64 {
	t0 := time.Now()
	x := uint64(88172645463325252)
	for i := uint64(0); i < iters; i++ {
		x = x*6364136223846793005 + 1442695040888963407
	}
	benchSink += x
	return float64(iters) / time.Since(t0).Seconds()
}

func benchCopy(src, dst []byte, rounds int) float64 {
	t0 := time.Now()
	for r := 0; r < rounds; r++ {
		copy(dst, src)
	}
	benchSink += uint64(dst[len(dst)-1])
	return float64(len(src)*rounds) / time.Since(t0).Seconds()
}

func benchChase(p []uint64, hops int) float64 {
	t0 := time.Now()
	idx := uint64(0)
	for i := 0; i < hops; i++ {
		idx = p[idx]
	}
	benchSink += idx
	return time.Since(t0).Seconds() * 1e9 / float64(hops)
}

// cpuDuty is het ijkgewicht voor de per-app CPU-meting (kern/slotmgr
// usage.go): DUTY=pct brandt pct% van elke 100ms en slaapt de rest — op
// ijzer hoort /tasks dan cpu≈pct te tonen (100 → 100, 50 → ~50). In
// QEMU-TCG is alleen de 100 betrouwbaar: WFE spint daar, dus elke slaap
// leest als "vol idle" (klemt op 0) — de lineariteit is een ijzer-meting.
// De Gosched per micro-burst houdt heartbeat/memwatch levend (coöperatief;
// telt níét als idle — alleen de echte idle-lus tikt).
func cpuDuty(app *applib.App, pct int) {
	pct = min(max(pct, 1), 100)
	const period = 100 * time.Millisecond
	busy := period * time.Duration(pct) / 100
	app.Logf("DUTY: %d%% duty-cycle (busy %v of every %v)", pct, busy, period)
	x := uint64(88172645463325252)
	for {
		for t0 := time.Now(); time.Since(t0) < busy; {
			for k := 0; k < 1<<14; k++ { // µs-burst, dan afgeven
				x = x*6364136223846793005 + 1442695040888963407
			}
			runtime.Gosched()
		}
		benchSink += x
		if rest := period - busy; rest > 0 {
			time.Sleep(rest)
		}
	}
}

// cpuBench draait de vier kernels in een eeuwige lus (heartbeat/kill lopen
// gewoon door via applib) en logt per ronde één samenvattingsregel.
func cpuBench(app *applib.App) {
	// Chase-permutatie: één cykel door 3M slots (24MB), Sattolo-shuffle met
	// een eigen LCG — deterministisch, geen rand-afhankelijkheid.
	const n = 3 << 20
	p := make([]uint64, n)
	for i := range p {
		p[i] = uint64(i)
	}
	seed := uint64(0x9E3779B97F4A7C15)
	for i := uint64(n - 1); i >= 1; i-- {
		seed = seed*6364136223846793005 + 1442695040888963407
		j := seed % i
		p[i], p[j] = p[j], p[i]
	}
	src := make([]byte, 24<<20)
	dst := make([]byte, 24<<20)
	for i := range src {
		src[i] = byte(i)
	}

	app.Logf("BENCH: single-core kernels (add-chain, lcg, copy 24MB, chase 24MB) — repeating")
	for round := 1; ; round++ {
		add := benchAddChain(1 << 30)
		lcg := benchLCG(1 << 28)
		cp := benchCopy(src, dst, 20)
		ch := benchChase(p, 5<<20)
		app.Logf("BENCH round %d: add-chain %.2f Giter/s (~clock GHz) | lcg %.0f Miter/s | copy %.2f GB/s | chase %.1f ns/hop",
			round, add/1e9, lcg/1e6, cp/1e9, ch)
	}
}
