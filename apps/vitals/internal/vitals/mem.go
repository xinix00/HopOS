package vitals

// De geheugentests. Op een nieuw board is dít waar een verkeerde
// DRAM-controller-config (timing, breedte, frequentie) als eerste zichtbaar
// wordt: bandbreedte ver onder spec, of een latentietrap die niet bij de
// cache-hiërarchie past.

import (
	"net/url"
	"runtime"
	"time"
)

// bwBuf kiest de buffermaat: ruim boven elke L2 (dan meet je DRAM), maar met
// twee buffers ruim binnen de partitie — memlimit heeft de GC al op de
// partitiegrens gezet, dus hier past bescheidenheid.
func (s *Server) bwBuf() int {
	size := 16 << 20
	if m := int(s.cfg.RAMSize / 8); m < size && m > 0 {
		size = m
	}
	return size
}

// runMemBW meet STREAM-achtig: copy (grote memmove) en triad (gemengd
// lezen/rekenen/schrijven op float64). Bytes geteld zoals STREAM dat doet:
// elke gelezen én geschreven byte telt.
func (s *Server) runMemBW(res *Result, q url.Values) {
	size := s.bwBuf()
	res.add("buffer", float64(size>>20), "MB")

	// copy: 1x lezen + 1x schrijven per pass.
	src := make([]byte, size)
	dst := make([]byte, size)
	for i := range src {
		src[i] = byte(i)
	}
	passes := 0
	t0 := time.Now()
	for time.Since(t0) < 700*time.Millisecond {
		copy(dst, src)
		passes++
		runtime.Gosched()
	}
	el := time.Since(t0).Seconds()
	res.add("copy", float64(passes)*float64(size)*2/el/1e9, "GB/s")
	res.linef("copy: %d passes of %d MB in %.2fs", passes, size>>20, el)
	src, dst = nil, nil

	// triad: a[i] = b[i] + 3*c[i] — 2 reads + 1 write van 8 bytes per element.
	n := size / 8
	a := make([]float64, n)
	b := make([]float64, n)
	c := make([]float64, n)
	for i := range b {
		b[i] = float64(i)
		c[i] = float64(n - i)
	}
	passes = 0
	t0 = time.Now()
	for time.Since(t0) < 700*time.Millisecond {
		for i := 0; i < n; i++ {
			a[i] = b[i] + 3*c[i]
		}
		passes++
		runtime.Gosched()
	}
	el = time.Since(t0).Seconds()
	sink += uint64(a[n-1])
	res.add("triad", float64(passes)*float64(n)*24/el/1e9, "GB/s")
	res.linef("triad: %d passes of %d elements in %.2fs", passes, n, el)
}

// runMemLat meet de laadlatentie met een pointer-chase: elke load hangt van
// de vorige af, dus prefetchers en out-of-order helpen niet. Oplopende
// working-sets tekenen de cache-trap: L1, L2, en daarboven het echte DRAM.
func (s *Server) runMemLat(res *Result, q url.Values) {
	sizes := []int{32 << 10, 128 << 10, 512 << 10, 2 << 20, 8 << 20}
	maxSize := int(s.cfg.RAMSize / 8)

	var lastNS float64
	for _, size := range sizes {
		if maxSize > 0 && size > maxSize {
			break
		}
		n := size / 8 // één woord per cacheline zou strikter zijn; dit meet mee in de praktijkmix
		p := make([]uint32, n)
		for i := range p {
			p[i] = uint32(i)
		}
		// Sattolo: één cykel door alle elementen, in LCG-volgorde — geen
		// math/rand nodig en deterministisch tussen runs.
		r := uint64(88172645463325252)
		for i := n - 1; i > 0; i-- {
			r = r*6364136223846793005 + 1442695040888963407
			j := int(r % uint64(i))
			p[i], p[j] = p[j], p[i]
		}

		// Kalibreer het aantal stappen op ~80ms meettijd.
		steps := 1 << 16
		var el time.Duration
		var cur uint32
		for {
			t0 := time.Now()
			for k := 0; k < steps; k++ {
				cur = p[cur]
			}
			el = time.Since(t0)
			if el > 80*time.Millisecond || steps >= 1<<26 {
				break
			}
			steps *= 4
			runtime.Gosched()
		}
		sink += uint64(cur)
		lastNS = float64(el.Nanoseconds()) / float64(steps)
		res.linef("%6d KB: %6.1f ns/load (%d steps)", size>>10, lastNS, steps)
		runtime.Gosched()
	}
	res.add("furthest", lastNS, "ns/load")
}
