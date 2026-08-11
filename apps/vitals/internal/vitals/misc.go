package vitals

// De kleine metingen: GC/allocatie-gedrag en timer/scheduler-jitter. Klein
// maar board-gevoelig — de timerbron en de trap naar de slaapstand (WFE
// event-stream op arm64, CLINT-slaap op riscv64) zitten er allebei in.

import (
	"net/url"
	"runtime"
	"time"
)

// keepAlive houdt de gc-testallocaties vast tot na de meting.
var keepAlive [][]byte

// runGC meet het allocatietempo en wat GC daarvoor terugvraagt: cycli,
// totale pauzetijd en de langste pauze in het venster. Op een board met een
// krappe partitie zie je hier of memlimit de GC gezond houdt.
func (s *Server) runGC(res *Result, q url.Values) {
	secs := qInt(q, "secs", 3, 1, 30)

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const blob = 32 << 10
	allocs := 0
	keepAlive = keepAlive[:0]
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	t0 := time.Now()
	for time.Now().Before(deadline) {
		b := make([]byte, blob)
		b[0] = byte(allocs)
		keepAlive = append(keepAlive, b)
		if len(keepAlive) > 64 { // ~2MB levend, de rest is voer voor de GC
			keepAlive = keepAlive[32:]
		}
		allocs++
		if allocs%64 == 0 {
			runtime.Gosched()
		}
	}
	el := time.Since(t0).Seconds()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// De langste pauze uit de ring, alleen de cycli van dit venster.
	var maxPause uint64
	for gc := before.NumGC; gc < after.NumGC; gc++ {
		if p := after.PauseNs[gc%uint32(len(after.PauseNs))]; p > maxPause {
			maxPause = p
		}
	}

	res.add("alloc", float64(allocs)*blob/el/1e6, "MB/s")
	res.add("gc_cycles", float64(after.NumGC-before.NumGC), "")
	res.add("pause_total", float64(after.PauseTotalNs-before.PauseTotalNs)/1e6, "ms")
	res.add("pause_max", float64(maxPause)/1e6, "ms")
	res.linef("%d allocations of %d KB in %.2fs, heap now %d KB (sys %d MB)",
		allocs, blob>>10, el, after.HeapAlloc>>10, after.Sys>>20)
	keepAlive = nil
}

// runTimer meet de slaap-nauwkeurigheid: vraag N keer een korte slaap en kijk
// hoeveel te laat je wakker wordt. Dat overschot is de optelsom van timerbron,
// idle-governor en scheduler — op een gedeelde core zit ook de buurman erin.
func (s *Server) runTimer(res *Result, q url.Values) {
	targets := []time.Duration{time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond}
	const rounds = 40

	for _, target := range targets {
		over := make([]float64, 0, rounds)
		for k := 0; k < rounds; k++ {
			t0 := time.Now()
			time.Sleep(target)
			over = append(over, float64(time.Since(t0)-target)/1e3) // µs te laat
		}
		res.linef("sleep %4dms: median +%5.0f µs, p99 +%5.0f µs, max +%5.0f µs",
			target.Milliseconds(), pct(over, 50), pct(over, 99), pct(over, 100))
		if target == time.Millisecond {
			res.add("oversleep_1ms_p50", pct(over, 50), "µs")
			res.add("oversleep_1ms_p99", pct(over, 99), "µs")
		}
	}
	res.linef("oversleep = actual - requested; includes timer source, idle governor and scheduler")
}
