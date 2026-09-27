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
	"fmt"
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
	// n= parallelle verbindingen, elk een eigen GET van capBytes/n. Eén app
	// met één socket haalt op een langzaam board niet de draad vol terwijl
	// zijn cores idle staan: het plafond is venster ÷ pijplijn-round-trip
	// per VERBINDING (L83 p56/p58). Met deze knop is te meten of "sneller"
	// gewoon "meer dan één verbinding" heet — wat elk bulkprotocol al doet
	// (S3-multipart, HTTP/2-streams, een backup in delen).
	conns := qInt(q, "n", 1, 1, 16)
	share := capBytes / int64(conns)
	// Wat er echt gevraagd wordt: bij n=3 blijft er anders een rest over die
	// geen verbinding haalt, en dan meldt de controle hieronder een te kort
	// antwoord dat er niet is.
	capBytes = share * int64(conns)
	rxProgress.Store(0)
	rxZeroReads.Store(0)
	done := make(chan struct{})
	go s.stallWatch("rx", &rxProgress, done)
	defer close(done)

	type result struct {
		got    int64
		header time.Duration
		err    string
	}
	var progress atomic.Int64
	out := make([]result, conns)
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := &out[i]
			h0 := time.Now()
			resp, err := leanhttp.GetCall(leanhttp.Call{URL: src, Header: leanhttp.Header{"User-Agent": "HopOS-vitals"}})
			if err != nil {
				r.err = err.Error()
				return
			}
			defer resp.Body.Close()
			r.header = time.Since(h0)
			buf := make([]byte, 64<<10)
			for r.got < share {
				want := int64(len(buf))
				if share-r.got < want {
					want = share - r.got
				}
				n, err := resp.Body.Read(buf[:want])
				r.got += int64(n)
				rxProgress.Store(progress.Add(int64(n)))
				if n == 0 && err == nil {
					rxZeroReads.Add(1)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					r.err = err.Error()
					return
				}
			}
		}(i)
	}
	wg.Wait()
	el := time.Since(t0).Seconds()

	var got int64
	var header time.Duration
	for i := range out {
		if out[i].err != "" {
			res.Err = out[i].err
			return
		}
		got += out[i].got
		if out[i].header > header {
			header = out[i].header
		}
	}
	if got < capBytes {
		res.Err = fmt.Sprintf("short benchmark response: read %d bytes, requested %d", got, capBytes)
		return
	}

	res.add("throughput", float64(got)/el/1e6, "MB/s")
	res.add("read", float64(got>>20), "MB")
	res.add("header", header.Seconds()*1e3, "ms")
	res.add("conns", float64(conns), "")
	res.linef("%s (%d connection(s), read %d MB)", src, conns, got>>20)
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
	txProgress.Store(0)
	done := make(chan struct{})
	go s.stallWatch("tx", &txProgress, done)
	defer close(done)
	t0 := time.Now()
	sent := 0
	for sent < total {
		n := len(blobChunk)
		if total-sent < n {
			n = total - sent
		}
		m, err := w.Write(blobChunk[:n])
		sent += m
		txProgress.Store(int64(sent))
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
