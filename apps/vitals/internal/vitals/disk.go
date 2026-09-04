package vitals

// De opslagtests: de NVMe door het system-callpad (app-stack → slot-LAN → HOP
// → hopfs → NVMe), en de ontvangkant van het netwerk.
//
//	disk   schrijft en leest een bestand in chunks via het system-callcontract
//	       (1 MiB per call; ?kb= kiest kleiner), daarna 4 KiB-writes (het
//	       SQLite-patroon) en kale Stat-calls (de vloer van het pad zónder
//	       schijf). Zo is te zien of het LAN of de NVMe de rem is: alles boven
//	       de Stat-vloer is hopfs + schijf.
//	up     client-gedreven: `curl -T bestand http://node:poort/sink`; de
//	       handler klokt zijn leeskant en het resultaat verschijnt als "up",
//	       de tegenhanger van tx.

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// FS is de bestandslaag zoals disk hem nodig heeft — applib.App voldoet. Als
// interface zodat dit pakket host-buildbaar blijft (geen metal-import; de
// tamago-main geeft de app door).
type FS interface {
	Stat(path string) (uint64, error)
	ReadAt(path string, off uint64, n int) ([]byte, error)
	WriteAt(path string, off uint64, data []byte) (int, error)
	Remove(path string) error
}

// runDisk: schrijven, lezen, 4 KiB-writes en de Stat-vloer op één bestand in
// de eigen root (?path= kiest een ander, bijvoorbeeld een mount). Het bestand
// wordt na afloop weggehaald; hopfs is toch vluchtig.
func (s *Server) runDisk(res *Result, q url.Values) {
	fs := s.cfg.FS
	if fs == nil {
		res.Err = "no file layer (not running as a HopOS app)"
		return
	}
	mb := qInt(q, "mb", 64, 1, 1024)
	kb := qInt(q, "kb", 1024, 4, 1024)
	path := q.Get("path")
	if path == "" {
		path = "/vitals-disk.bin"
	}
	chunk := kb << 10
	total := mb << 20
	buf := make([]byte, chunk)
	for i := 0; i < len(buf); i += len(blobChunk) {
		copy(buf[i:], blobChunk)
	}
	defer fs.Remove(path)

	// ?hole=1: het bestand wordt niet geschreven maar alleen op lengte gezet
	// (één byte op het eind); de leesfase leest dan gaten, en die komen uit
	// HOP's RAM zonder één schijfblok. Dat isoleert het transport HOP → app.
	hole := q.Get("hole") == "1"
	wlat := make([]float64, 0, total/chunk+1)
	t0 := time.Now()
	if hole {
		if _, err := fs.WriteAt(path, uint64(total-1), buf[:1]); err != nil {
			res.Err = fmt.Sprintf("hole: %v", err)
			return
		}
	}
	for off := 0; off < total && !hole; off += chunk {
		n := chunk
		if total-off < n {
			n = total - off
		}
		t := time.Now()
		if _, err := fs.WriteAt(path, uint64(off), buf[:n]); err != nil {
			res.Err = fmt.Sprintf("write at %d: %v", off, err)
			return
		}
		wlat = append(wlat, time.Since(t).Seconds()*1e3)
		if off%(8<<20) == 0 {
			s.setNote("disk write %d/%d MB", off>>20, mb)
		}
	}
	wel := time.Since(t0).Seconds()

	// Lezen: zelfde chunks terug, lengte gecontroleerd.
	rlat := make([]float64, 0, len(wlat))
	t1 := time.Now()
	for off := 0; off < total; off += chunk {
		n := chunk
		if total-off < n {
			n = total - off
		}
		t := time.Now()
		got, err := fs.ReadAt(path, uint64(off), n)
		if err != nil {
			res.Err = fmt.Sprintf("read at %d: %v", off, err)
			return
		}
		if len(got) != n {
			res.Err = fmt.Sprintf("read at %d: %d bytes, want %d", off, len(got), n)
			return
		}
		// Inhoud vergelijken, niet alleen de lengte: een gecachte DMA-buffer
		// zonder invalidate geeft nette lengtes met de data van de vórige
		// transfer erin (T21, 03-09) — dat zie je alleen zo.
		if !hole && !bytes.Equal(got, buf[:n]) {
			res.Err = fmt.Sprintf("read at %d: content mismatch (first bad byte at %d)", off, firstDiff(got, buf[:n]))
			return
		}
		rlat = append(rlat, time.Since(t).Seconds()*1e3)
		if off%(8<<20) == 0 {
			s.setNote("disk read %d/%d MB", off>>20, mb)
		}
	}
	rel := time.Since(t1).Seconds()

	// 4 KiB-writes: het patroon van een embedded database (SQLite-pagina's),
	// 256 stuks = 1 MiB, over het begin van hetzelfde bestand.
	s.setNote("disk 4 KiB writes")
	const small, smallN = 4 << 10, 256
	slat := make([]float64, 0, smallN)
	t2 := time.Now()
	for k := 0; k < smallN; k++ {
		t := time.Now()
		if _, err := fs.WriteAt(path, uint64(k*small), buf[:small]); err != nil {
			res.Err = fmt.Sprintf("4 KiB write %d: %v", k, err)
			return
		}
		slat = append(slat, time.Since(t).Seconds()*1e6)
	}
	sel := time.Since(t2).Seconds()

	// De vloer: Stat raakt alleen hopfs' metadata in HOP's RAM — dit is het
	// system-callpad zelf (app-stack, slot-LAN, HOP's stack, servicer) zonder
	// één schijfblok.
	s.setNote("disk stat floor")
	const statN = 200
	flat := make([]float64, 0, statN)
	var size uint64
	for k := 0; k < statN; k++ {
		t := time.Now()
		sz, err := fs.Stat(path)
		if err != nil {
			res.Err = fmt.Sprintf("stat: %v", err)
			return
		}
		flat = append(flat, time.Since(t).Seconds()*1e6)
		size = sz
	}
	if size != uint64(total) {
		res.Err = fmt.Sprintf("file is %d bytes after the run, want %d", size, total)
		return
	}

	if hole {
		res.add("write", 0, "MB/s (skipped, hole=1)")
	} else {
		res.add("write", float64(total)/wel/1e6, "MB/s")
	}
	res.add("read", float64(total)/rel/1e6, "MB/s")
	res.add("write 4k", float64(small*smallN)/sel/1e6, "MB/s")
	res.add("call floor p50", pct(flat, 50), "µs")
	res.add("chunk", float64(kb), "KiB")
	res.linef("%d MB via %s in %d calls of %d KiB (write p50 %.1f ms, p99 %.1f ms; read p50 %.1f ms, p99 %.1f ms)",
		mb, path, len(rlat), kb, pct(wlat, 50), pct(wlat, 99), pct(rlat, 50), pct(rlat, 99))
	if hole {
		res.linef("hole=1: the file was never written, every read returned zeros from HOP's RAM — this is the transport HOP → app alone")
	}
	res.linef("4 KiB writes, the database pattern: %d calls, %.0f/s, p50 %.0f µs, p99 %.0f µs",
		smallN, float64(smallN)/sel, pct(slat, 50), pct(slat, 99))
	res.linef("system-call floor (stat, no disk block touched): p50 %.0f µs, p99 %.0f µs — everything above it is hopfs + NVMe",
		pct(flat, 50), pct(flat, 99))
	res.linef("path: app stack → slot LAN → HOP's stack → servicer → hopfs → NVMe, %d KiB per call", kb)
}

// upBurst telt uploads die elkaar snel opvolgen bij elkaar op. leanhttp
// begrenst een body bewust op 1 MiB (maxBodyBytes), dus een upload van
// betekenis komt als een reeks PUTs over één keep-alive-verbinding binnen
// (perf.sh doet dat); één request zegt dan niets, de reeks wél. Een gat van
// meer dan twee seconden begint een nieuwe reeks.
type upBurst struct {
	start, end time.Time
	bytes      int64
	reqs       int
}

// serveSink ontvangt een body en gooit hem weg. De ontvangkant van het
// netwerkpad, gedreven door een client die je zelf kiest — perf.sh, of met de
// hand: python/curl PUTs van hoogstens 1 MiB naar /sink. Het resultaat wordt
// als "up" opgeslagen, zoals /blob dat als "tx" doet, en beslaat de hele
// reeks: bytes gedeeld door de wandtijd van de eerste tot de laatste request,
// dus inclusief de gaten die de client laat vallen.
func (s *Server) serveSink(w leanhttp.ResponseWriter, r *leanhttp.Request) {
	if r.Method != "POST" && r.Method != "PUT" {
		leanhttp.Error(w, "send bodies of up to 1 MiB here: apps/vitals/perf.sh, or PUT /sink", 405)
		return
	}
	t0 := time.Now()
	buf := make([]byte, 64<<10)
	var got int64
	var readErr error
	for {
		n, err := r.Body.Read(buf)
		got += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	now := time.Now()
	s.mu.Lock()
	if s.up.reqs == 0 || t0.Sub(s.up.end) > 2*time.Second {
		s.up = upBurst{start: t0}
	}
	s.up.end, s.up.bytes, s.up.reqs = now, s.up.bytes+got, s.up.reqs+1
	b := s.up
	res := &Result{Test: "up", Started: b.start, Duration: b.end.Sub(b.start).Seconds()}
	if readErr != nil {
		res.Err = "client went away: " + readErr.Error()
	}
	res.add("throughput", float64(b.bytes)/res.Duration/1e6, "MB/s")
	res.add("received", float64(b.bytes)/(1<<20), "MB")
	res.add("requests", float64(b.reqs), "")
	res.linef("%d request(s) from %s, %d MB in %.2fs wall time (bodies of up to 1 MiB, leanhttp's limit)",
		b.reqs, r.RemoteAddr, b.bytes>>20, res.Duration)
	s.results["up"] = res
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"received\":%d,\"burst_mb\":%d,\"burst_seconds\":%.3f}\n", got, b.bytes>>20, b.end.Sub(b.start).Seconds())
}

// runSyscall zet de vloer van een system call naast een like-for-like
// HTTP-request naar HOP's agent over een keep-alive-verbinding: zelfde
// app-stack, zelfde slot-LAN, zelfde HOP-stack, alleen de dienst erachter
// verschilt. Zit het verschil in de servicer, dan zie je het hier; zit het in
// het wek-pad van een stille verbinding, dan zijn beide even traag.
func (s *Server) runSyscall(res *Result, q url.Values) {
	n := qInt(q, "n", 200, 10, 5000)
	if s.cfg.FS != nil {
		path := "/vitals-syscall.bin"
		if _, err := s.cfg.FS.WriteAt(path, 0, []byte{1}); err != nil {
			res.Err = fmt.Sprintf("prepare: %v", err)
			return
		}
		defer s.cfg.FS.Remove(path)
		// ?burst=N: eerst N writes van 4 KiB, ?gap=ms: dan zoveel ms stilte —
		// om te zien of een burst de verbinding tijdelijk traag achterlaat.
		if burst := qInt(q, "burst", 0, 0, 4096); burst > 0 {
			blk := make([]byte, 4<<10)
			for k := 0; k < burst; k++ {
				if _, err := s.cfg.FS.WriteAt(path, uint64(k)<<12, blk); err != nil {
					res.Err = fmt.Sprintf("burst write %d: %v", k, err)
					return
				}
			}
			time.Sleep(time.Duration(qInt(q, "gap", 0, 0, 5000)) * time.Millisecond)
		}
		lat := make([]float64, 0, n)
		for k := 0; k < n; k++ {
			t := time.Now()
			if _, err := s.cfg.FS.Stat(path); err != nil {
				res.Err = fmt.Sprintf("stat: %v", err)
				return
			}
			lat = append(lat, time.Since(t).Seconds()*1e6)
		}
		res.add("stat p50", pct(lat, 50), "µs")
		res.add("stat p99", pct(lat, 99), "µs")
	}
	cl := &leanhttp.Client{}
	target := "http://" + s.cfg.HopAddr + "/health"
	lat := make([]float64, 0, n)
	for k := 0; k < n; k++ {
		t := time.Now()
		resp, err := cl.Do(leanhttp.Call{URL: target, Timeout: 5 * time.Second})
		if err != nil {
			res.Err = fmt.Sprintf("GET %s: %v", target, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lat = append(lat, time.Since(t).Seconds()*1e6)
	}
	res.add("GET p50", pct(lat, 50), "µs")
	res.add("GET p99", pct(lat, 99), "µs")
	res.linef("%d stat calls on the persistent system connection vs %d keep-alive GET %s", n, n, target)
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return -1
}
