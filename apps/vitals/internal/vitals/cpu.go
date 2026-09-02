package vitals

// De CPU-tests. De werklast is de LCG-stap uit appspike's soak (bewezen vorm):
// puur register-werk, geen geheugendruk, dus wat je meet is de kloksnelheid
// van het hart — precies wat je op een nieuw board wilt kalibreren. Elke
// burst is ~0,3 ms en eindigt in een Gosched: compute-lussen op HopOS geven
// coöperatief af (app-isolatie-principe), en zo blijven heartbeat en
// telemetrie lopen terwijl we branden.

import (
	"fmt"
	"net/url"
	"sort"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// sink houdt rekenresultaten levend zodat de compiler het werk niet weggooit.
var sink uint64

const burstSteps = 1 << 19 // ~0,3 ms per burst op ~1,5 GHz

//go:noinline
func lcgBurst(acc uint64) uint64 {
	for k := 0; k < burstSteps; k++ {
		acc = acc*6364136223846793005 + uint64(k)
	}
	return acc
}

// runCPU meet de single-core-doorvoer: LCG-stappen per seconde op één
// goroutine, mét de Gosched-tax die elke nette HopOS-app betaalt.
func (s *Server) runCPU(res *Result, q url.Values) {
	secs := qInt(q, "secs", 5, 1, 60)
	deadline := time.Now().Add(time.Duration(secs) * time.Second)

	var acc, bursts uint64
	t0 := time.Now()
	for time.Now().Before(deadline) {
		acc = lcgBurst(acc)
		bursts++
		runtime.Gosched()
	}
	sink = acc
	el := time.Since(t0).Seconds()

	steps := float64(bursts) * burstSteps
	res.add("rate", steps/el/1e6, "Msteps/s")
	res.add("burst", el / float64(bursts) * 1e6, "µs")
	res.linef("%d bursts of %d LCG steps in %.2fs (incl. one Gosched per burst)",
		bursts, burstSteps, el)
}

// runSMP meet de multi-core-schaal: hetzelfde werk serieel en verdeeld over
// alle cores. De speedup is de maat; het rendezvous (alle goroutines komen
// terug) is het bewijs dat alle harten echt meedoen. Eén core = overslaan.
func (s *Server) runSMP(res *Result, q url.Values) {
	n := runtime.GOMAXPROCS(0)
	res.add("cores", float64(n), "")
	if n < 2 {
		res.linef("only 1 core assigned — scaling test skipped (give the job cpu_shares >= 2048)")
		return
	}

	// Gedeelde heap: twee goroutines vullen om-en-om dezelfde slice. Gaat dit
	// mis, dan is elk ander cijfer hieronder betekenisloos.
	const N = 1 << 18
	shared := make([]uint32, N)
	var wg0 sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg0.Add(1)
		go func(g int) {
			defer wg0.Done()
			for i := g; i < N; i += 2 {
				shared[i] = uint32(i)
			}
		}(g)
	}
	wg0.Wait()
	for i := 0; i < N; i++ {
		if shared[i] != uint32(i) {
			res.Err = "shared heap corrupt — SMP is broken on this board"
			res.linef("slice mismatch at %d", i)
			return
		}
	}
	res.linef("shared heap verified: %d words written interleaved by 2 goroutines", N)

	// Serieel vs. parallel: W stappen op één goroutine, dan W/n op elk van n.
	const W = 64 << 20
	t1 := time.Now()
	var acc uint64
	for done := 0; done < W; done += burstSteps {
		acc = lcgBurst(acc)
		runtime.Gosched()
	}
	sink = acc
	d1 := time.Since(t1)

	t2 := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < n; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var a uint64
			for done := 0; done < W/n; done += burstSteps {
				a = lcgBurst(a)
				runtime.Gosched()
			}
			atomic.AddUint64(&sink, a)
		}()
	}
	wg.Wait()
	d2 := time.Since(t2)

	res.add("serial", d1.Seconds()*1e3, "ms")
	res.add("parallel", d2.Seconds()*1e3, "ms")
	res.add("speedup", float64(d1)/float64(d2), "x")
	res.linef("%dM LCG steps: serial %v, split over %d cores %v", W>>20, d1.Round(time.Millisecond), n, d2.Round(time.Millisecond))
}

// runBurn is de volgehouden-last-test: alle cores branden secs lang, met per
// seconde het tempo en (via de agent-API) de temperatuur ernaast. Thermal
// throttling of een dvfs-terugklok is dan een knik in de reeks — dé meting
// voor een nieuw board met een gokje van een heatsink.
func (s *Server) runBurn(res *Result, q url.Values) {
	secs := qInt(q, "secs", 120, 10, 600)
	n := runtime.GOMAXPROCS(0)

	var iters uint64
	var stop atomic.Bool
	for g := 0; g < n; g++ {
		go func() {
			var acc uint64
			for !stop.Load() {
				acc = lcgBurst(acc)
				atomic.AddUint64(&iters, 1)
				runtime.Gosched()
			}
			sink = acc
		}()
	}

	// De meetlus: per seconde het burst-tempo, elke 5s een temperatuur (de
	// cache dempt het API-verkeer toch al tot dat ritme).
	var first, last []float64 // eerste en laatste 10 samples
	maxTemp := 0
	prev := atomic.LoadUint64(&iters)
	for t := 1; t <= secs; t++ {
		time.Sleep(time.Second)
		cur := atomic.LoadUint64(&iters)
		rate := float64(cur-prev) * burstSteps / 1e6
		prev = cur

		temp := s.temp.get(s.cfg)
		if temp > maxTemp {
			maxTemp = temp
		}
		if temp > 0 {
			res.linef("t=%3ds  %8.1f Msteps/s  %5.1f C", t, rate, float64(temp)/1000)
		} else {
			res.linef("t=%3ds  %8.1f Msteps/s", t, rate)
		}
		if t > 1 && len(first) < 10 { // t=1 is een halve seconde opstart
			first = append(first, rate)
		}
		last = append(last, rate)
		if len(last) > 10 {
			last = last[1:]
		}
		if t%5 == 0 {
			s.setNote("burn %d/%ds, %.1f Msteps/s", t, secs, rate)
		}
	}
	stop.Store(true)

	res.add("cores", float64(n), "")
	res.add("rate_start", avg(first), "Msteps/s")
	res.add("rate_end", avg(last), "Msteps/s")
	if a := avg(first); a > 0 {
		res.add("degradation", (1-avg(last)/a)*100, "%")
	}
	if maxTemp > 0 {
		res.add("temp_max", float64(maxTemp)/1000, "C")
	} else {
		res.linef("no temperature available (set HOP_KEY to read it from the agent API)")
	}
}

func avg(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var t float64
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

// lcgBurstN: als lcgBurst, maar met een instelbaar aantal stappen — de
// ramp-meting wil bursts die klein genoeg zijn om binnen een 5ms-venster
// meerdere metingen te doen.
//
//go:noinline
func lcgBurstN(acc uint64, n int) uint64 {
	for k := 0; k < n; k++ {
		acc = acc*6364136223846793005 + uint64(k)
	}
	return acc
}

// runRamp meet hoe snel de kloksnelheid OPSCHAALT onder plotselinge last, in
// MHz over de tijd. Eerst bewust stil zijn (de hardware-governor — APSC op
// Apple — mag terugklokken), dan vol aan en de doorvoer per 5ms-venster
// vastleggen.
//
// De vertaling van doorvoer naar klok is geen aanname maar een ijking: de
// LCG-stap is een afhankelijke MADD-keten van exact 3,0 cycles op deze cores —
// GEMETEN 01-09 op de M4: 300,0 Msteps/s op 900 MHz én 723,8 op 2172 MHz,
// allebei binnen een half procent van klok/3. Dus MHz = Msteps/s × 3.
//
// Parameters: ?idle=ms (stilte vooraf, default 2000) en ?secs=n (meetduur).
// De uitkomst: startklok, eindklok, en de tijd tot 90% van de eindklok — dát
// getal is "hoe snel schaalt hij op".
func (s *Server) runRamp(res *Result, q url.Values) {
	idleMS := qInt(q, "idle", 2000, 0, 30000)
	secs := qInt(q, "secs", 2, 1, 10)
	time.Sleep(time.Duration(idleMS) * time.Millisecond)

	const win = 5 * time.Millisecond
	const chunk = 1 << 16 // ~90µs op volle klok: ≥50 metingen per venster
	var acc uint64
	var rates []float64 // Msteps/s per venster, in volgorde
	t0 := time.Now()
	deadline := t0.Add(time.Duration(secs) * time.Second)
	winStart, winSteps := t0, 0
	for time.Now().Before(deadline) {
		acc = lcgBurstN(acc, chunk)
		winSteps += chunk
		if now := time.Now(); now.Sub(winStart) >= win {
			rates = append(rates, float64(winSteps)/now.Sub(winStart).Seconds()/1e6)
			winStart, winSteps = now, 0
			runtime.Gosched() // compute geeft af (app-isolatie-principe)
		}
	}
	sink = acc
	if len(rates) < 4 {
		res.linef("too few samples (%d) — node too busy?", len(rates))
		return
	}

	// De eindklok: de mediaan van de tweede helft (dan staat hij er zeker).
	tail := append([]float64(nil), rates[len(rates)/2:]...)
	sort.Float64s(tail)
	top := tail[len(tail)/2]
	// De tijd tot 90% daarvan, en de klok waarmee we begonnen.
	rampMS := -1.0
	for i, r := range rates {
		if r >= 0.9*top {
			rampMS = float64(i) * win.Seconds() * 1000
			break
		}
	}
	const cyclesPerStep = 3.0
	res.add("mhz_start", rates[0]*cyclesPerStep, "MHz")
	res.add("mhz_top", top*cyclesPerStep, "MHz")
	res.add("ramp_ms", rampMS, "ms")
	res.linef("idle %dms, then full load: %.0f -> %.0f MHz, >=90%% after %.0f ms",
		idleMS, rates[0]*cyclesPerStep, top*cyclesPerStep, rampMS)
	// De eerste 20 vensters als tijdlijn — dáár zit het verhaal.
	n := len(rates)
	if n > 20 {
		n = 20
	}
	for i := 0; i < n; i += 4 {
		end := i + 4
		if end > n {
			end = n
		}
		line := ""
		for j := i; j < end; j++ {
			line += fmt.Sprintf("  t+%3dms %5.0f MHz", j*5, rates[j]*cyclesPerStep)
		}
		res.linef("%s", line)
	}
}
