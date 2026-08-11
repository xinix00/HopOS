package vitals

// De netwerktests. HOP levert geen netstats (bewust: de switch telt niet),
// dus vitals meet zijn eigen draadverkeer — zelfde aanpak als surf's dash.
//
//	rx     download van een externe bron: doorvoer door de hele keten
//	       (app-netstack → hopswitch → gwnat → NIC → internet)
//	tx     client-gedreven: curl het /blob-endpoint en de handler klokt
//	       zijn eigen schrijfkant; het resultaat verschijnt als "tx"
//	storm  veel korte verbindingen naar de eigen gepubliceerde poort
//	       (hairpin): verbindingsopbouw onder druk, met latentiepercentielen
//	rtt    kale TCP-handshakes naar de gateway: de vloer van het interne pad

import (
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// runRx downloadt (een stuk van) een groot bestand en meet de doorvoer.
// Default is een publiek CDN-bestand — de node moet dus internet hebben; met
// ?url= kan het ook een buur op het LAN zijn.
func (s *Server) runRx(res *Result, q url.Values) {
	src := q.Get("url")
	if src == "" {
		src = s.cfg.RxURL
	}
	capBytes := int64(qInt(q, "mb", 32, 1, 1024)) << 20

	t0 := time.Now()
	resp, err := leanhttp.Get(src)
	if err != nil {
		res.Err = err.Error()
		return
	}
	defer resp.Body.Close()
	header := time.Since(t0)

	buf := make([]byte, 64<<10)
	var got int64
	t1 := time.Now()
	for got < capBytes {
		n, err := resp.Body.Read(buf)
		got += int64(n)
		if got%(8<<20) < int64(n) {
			s.setNote("rx %d/%d MB", got>>20, capBytes>>20)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			res.Err = err.Error()
			return
		}
	}
	el := time.Since(t1).Seconds()

	res.add("throughput", float64(got)/el/1e6, "MB/s")
	res.add("read", float64(got>>20), "MB")
	res.add("header", header.Seconds()*1e3, "ms")
	res.linef("%s (%d MB available, read %d MB)", src, resp.Length>>20, got>>20)
}

// runStorm vuurt veel korte GETs op de eigen /ping af, standaard via de
// gepubliceerde poort op het node-IP (hairpin): dan loopt élke verbinding
// door hopswitch en gwnat, precies het pad dat onder druk moet blijven staan.
// Kanttekening: client en server delen dit slot, dus de cijfers zijn een
// ondergrens — voor de zuivere meting storm je van buitenaf naar /ping.
func (s *Server) runStorm(res *Result, q url.Values) {
	target := q.Get("url")
	switch {
	case target != "":
	case s.cfg.Host != "":
		target = "http://" + net.JoinHostPort(s.cfg.Host, s.cfg.Port) + "/ping"
	default:
		target = "http://" + net.JoinHostPort(s.cfg.IP, s.cfg.Port) + "/ping"
	}
	workers := qInt(q, "n", 8, 1, 64)
	total := qInt(q, "reqs", 200, 10, 10000)

	var next, errs int64
	durs := make([]float64, 0, total)
	var mu sync.Mutex
	var wg sync.WaitGroup
	t0 := time.Now()
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k := atomic.AddInt64(&next, 1)
				if k > int64(total) {
					return
				}
				if k%50 == 0 {
					s.setNote("storm %d/%d", k, total)
				}
				t := time.Now()
				resp, err := leanhttp.Do(leanhttp.Call{URL: target, Timeout: 10 * time.Second})
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if err != nil {
					atomic.AddInt64(&errs, 1)
					continue
				}
				mu.Lock()
				durs = append(durs, time.Since(t).Seconds()*1e3)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	el := time.Since(t0).Seconds()

	res.add("rate", float64(len(durs))/el, "conn/s")
	res.add("p50", pct(durs, 50), "ms")
	res.add("p90", pct(durs, 90), "ms")
	res.add("p99", pct(durs, 99), "ms")
	res.add("errors", float64(errs), "")
	res.linef("%d requests, %d workers, target %s", total, workers, target)
	res.linef("each request is one full connection (dial + GET + close) — client and server share this slot")
}

// runRTT meet kale TCP-handshakes naar de gateway (de agent-API-poort van de
// eigen node): geen HTTP, geen handler — de vloer van het interne netwerkpad.
func (s *Server) runRTT(res *Result, q url.Values) {
	addr := q.Get("addr")
	if addr == "" {
		addr = s.cfg.HopAddr
	}
	count := qInt(q, "n", 100, 10, 2000)

	durs := make([]float64, 0, count)
	errs := 0
	for k := 0; k < count; k++ {
		t := time.Now()
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			errs++
			continue
		}
		durs = append(durs, time.Since(t).Seconds()*1e6)
		c.Close()
		time.Sleep(5 * time.Millisecond) // meten, niet fluten
	}

	res.add("p50", pct(durs, 50), "µs")
	res.add("p99", pct(durs, 99), "µs")
	res.add("max", pct(durs, 100), "µs")
	res.add("errors", float64(errs), "")
	res.linef("%d TCP dials to %s (connect + close, 5ms apart)", count, addr)
}

// blobChunk is het patroonblok dat /blob herhaalt; één keer vullen is genoeg,
// de inhoud doet er niet toe (niemand pakt hem uit).
var blobChunk = func() []byte {
	b := make([]byte, 256<<10)
	r := uint64(2463534242)
	for i := range b {
		r ^= r << 13
		r ^= r >> 7
		r ^= r << 17
		b[i] = byte(r)
	}
	return b
}()

// serveBlob streamt mb megabytes en klokt zijn eigen schrijfkant: de
// TX-tegenhanger van rx, gedreven door een client die je zelf kiest
// (curl -o /dev/null http://node:poort/blob?mb=64). Het resultaat wordt als
// "tx" opgeslagen alsof het een test-run was.
func (s *Server) serveBlob(w leanhttp.ResponseWriter, r *leanhttp.Request) {
	mb := qInt(r.Query(), "mb", 32, 1, 1024)
	total := mb << 20

	// Content-Length vooraf: dan schrijft leanhttp direct door (geen
	// buffering) en kan curl de voortgang tonen.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(total))

	res := &Result{Test: "tx", Started: time.Now()}
	t0 := time.Now()
	sent := 0
	for sent < total {
		n := len(blobChunk)
		if total-sent < n {
			n = total - sent
		}
		m, err := w.Write(blobChunk[:n])
		sent += m
		if err != nil {
			res.Err = "client went away: " + err.Error()
			break
		}
	}
	el := time.Since(t0).Seconds()

	res.Duration = el
	res.add("throughput", float64(sent)/el/1e6, "MB/s")
	res.add("sent", float64(sent>>20), "MB")
	res.linef("%d MB to %s", sent>>20, r.RemoteAddr)
	s.mu.Lock()
	s.results["tx"] = res
	s.mu.Unlock()
}

// pct is het p-de percentiel (p=100 → maximum); 0 zonder samples.
func pct(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	sorted := make([]float64, len(v))
	copy(sorted, v)
	sort.Float64s(sorted)
	i := len(sorted) * p / 100
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
