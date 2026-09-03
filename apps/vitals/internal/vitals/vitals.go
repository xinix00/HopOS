// Package vitals meet de vitale functies van een HopOS-node: idle-gedrag,
// CPU-doorvoer, geheugenbandbreedte/-latentie, netwerkdoorvoer en
// scheduler-jitter. Eén test tegelijk (benchmarks die elkaar verdringen meten
// niets), resultaten blijven in geheugen staan tot de volgende run.
//
// Dit pakket is bewust host-buildbaar (geen metal-imports): alles wat van het
// board komt — de control-page-woorden, de tellerfrequentie — wordt door de
// tamago-main als functie geïnjecteerd. Zo draaien de rekentests ook in een
// gewone `go test` op de Mac.
//
// Alle output (pagina, JSON, logregels) is Engels; commentaar is Nederlands.
package vitals

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// Offsets zijn de control-page-offsets die de tamago-main uit abi/layout
// meegeeft — hier geen layout-import, dan blijft het pakket host-buildbaar en
// is er precies één bron voor de ABI.
type Offsets struct {
	Idle, Wakes, Cores, Shared, Heartbeat, MemSys uint64
}

// Config is alles wat de server van buiten nodig heeft. CtrlRead en CounterHz
// mogen nil zijn (host-build): dan rapporteert de idle-kant "n/a" en werkt de
// rest gewoon.
type Config struct {
	Version string
	Arch    string
	Runtime string
	Slot    int
	RAMSize uint64
	IP      string
	Host    string
	Port    string

	Logf      func(format string, args ...any)
	CtrlRead  func(off uint64) uint64
	CounterHz func() uint64
	Offsets   Offsets

	HopAddr string // agent-API (temperatuur); default 10.100.0.1:8080
	HopKey  string // "" = geen agent-API, temperatuur blijft n/a
	RxURL   string // bron van de download-test; default een plain-http CDN
	FS      FS     // bestandslaag voor de disk-test (applib.App); nil = test slaat over
}

// defaultRxURL is een publiek 100MB-bestand over plain http (leanhttp linkt
// bewust geen TLS). De test leest er standaard 32MB van; override via
// VITALS_RX_URL of ?url= per run.
const defaultRxURL = "http://cachefly.cachefly.net/100mb.test"

// Metric is één meetwaarde in een testresultaat.
type Metric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// Result is de uitkomst van één test-run. Eenmaal opgeslagen wordt hij niet
// meer gewijzigd; Lines is het per-stap-detail (voor de uitklap op de pagina
// en het rapport).
type Result struct {
	Test     string    `json:"test"`
	Started  time.Time `json:"started"`
	Duration float64   `json:"duration_s"`
	Metrics  []Metric  `json:"metrics"`
	Lines    []string  `json:"lines,omitempty"`
	Err      string    `json:"error,omitempty"`
}

func (r *Result) add(name string, value float64, unit string) {
	r.Metrics = append(r.Metrics, Metric{name, value, unit})
}

func (r *Result) linef(format string, args ...any) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, args...))
}

// tests is de vaste volgorde: zo staan ze op de pagina en zo loopt "all".
var tests = []struct{ Name, Desc string }{
	{"cpu", "single-core throughput"},
	{"ramp", "clock ramp-up under sudden load"},
	{"smp", "multi-core scaling"},
	{"burn", "sustained load + thermal"},
	{"membw", "memory bandwidth"},
	{"memlat", "memory latency"},
	{"rx", "download throughput"},
	{"disk", "NVMe through the system-call path"},
	{"storm", "connection storm (via published port)"},
	{"rtt", "TCP round-trips to the gateway"},
	{"gc", "allocation rate + GC pauses"},
	{"timer", "sleep jitter"},
}

// Server is de vitals-app: HTTP-handler, testdispatcher en idle-sampler.
type Server struct {
	cfg     Config
	started time.Time

	mu      sync.Mutex
	results map[string]*Result
	running string // "" = niets bezig; anders de testnaam (of "all")
	note    string // voortgangsregel van de lopende test

	up   upBurst // lopende upload-reeks van /sink (zie disk.go)
	idle idleSampler
	temp tempCache
}

// NewServer bouwt de server; Start() begint het passieve meten.
func NewServer(cfg Config) *Server {
	if cfg.HopAddr == "" {
		cfg.HopAddr = "10.100.0.1:8080"
	}
	if cfg.RxURL == "" {
		cfg.RxURL = defaultRxURL
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{
		cfg:     cfg,
		started: time.Now(),
		results: map[string]*Result{},
	}
}

// Start begint de idle-sampler: die loopt altijd, want idle-gedrag wil je
// juist zien terwijl er verder niets gebeurt.
func (s *Server) Start() {
	s.idle.start(s.cfg)
}

// setNote publiceert één regel voortgang van de lopende test.
func (s *Server) setNote(format string, args ...any) {
	s.mu.Lock()
	s.note = fmt.Sprintf(format, args...)
	s.mu.Unlock()
}

// startTest claimt de run-slot en draait de test asynchroon. Eén tegelijk:
// twee benchmarks door elkaar meten allebei niets.
func (s *Server) startTest(name string, q url.Values) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != "" {
		return fmt.Errorf("test %q is still running", s.running)
	}
	s.running, s.note = name, "starting"
	go func() {
		if name == "all" {
			for _, t := range tests {
				s.setNote("running %s", t.Name)
				res := s.run(t.Name, q)
				s.mu.Lock()
				s.results[t.Name] = res
				s.mu.Unlock()
			}
		} else {
			res := s.run(name, q)
			s.mu.Lock()
			s.results[name] = res
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.running, s.note = "", ""
		s.mu.Unlock()
	}()
	return nil
}

// run voert één test uit en vangt de uitkomst in een Result.
func (s *Server) run(name string, q url.Values) *Result {
	res := &Result{Test: name, Started: time.Now()}
	s.cfg.Logf("vitals: test %s started", name)
	switch name {
	case "cpu":
		s.runCPU(res, q)
	case "ramp":
		s.runRamp(res, q)
	case "smp":
		s.runSMP(res, q)
	case "burn":
		s.runBurn(res, q)
	case "membw":
		s.runMemBW(res, q)
	case "memlat":
		s.runMemLat(res, q)
	case "rx":
		s.runRx(res, q)
	case "disk":
		s.runDisk(res, q)
	case "storm":
		s.runStorm(res, q)
	case "rtt":
		s.runRTT(res, q)
	case "gc":
		s.runGC(res, q)
	case "timer":
		s.runTimer(res, q)
	default:
		res.Err = "unknown test"
	}
	res.Duration = time.Since(res.Started).Seconds()
	if res.Err != "" {
		s.cfg.Logf("vitals: test %s failed after %.1fs: %s", name, res.Duration, res.Err)
	} else {
		s.cfg.Logf("vitals: test %s done in %.1fs", name, res.Duration)
	}
	return res
}

// qInt leest een query-getal met default en grenzen.
func qInt(q url.Values, key string, def, min, max int) int {
	v, err := strconv.Atoi(q.Get(key))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// Handle is de leanhttp-handler: routeren op pad, zoals het pakket het wil.
func (s *Server) Handle(w leanhttp.ResponseWriter, r *leanhttp.Request) {
	switch r.Path {
	case "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	case "/api/state":
		s.writeState(w)
	case "/api/run":
		name := r.Query().Get("test")
		known := name == "all"
		for _, t := range tests {
			known = known || t.Name == name
		}
		if !known {
			leanhttp.Error(w, "unknown test "+strconv.Quote(name), leanhttp.StatusNotFound)
			return
		}
		if err := s.startTest(name, r.Query()); err != nil {
			leanhttp.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"started\":%q}\n", name)
	case "/ping":
		// Doelwit van de storm-test: zo klein mogelijk, meet de weg, niet
		// de handler.
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "pong\n")
	case "/blob":
		s.serveBlob(w, r)
	case "/sink":
		s.serveSink(w, r)
	default:
		leanhttp.Error(w, "not found", leanhttp.StatusNotFound)
	}
}

// writeState schrijft de volledige toestand als JSON: node-info, live
// idle-cijfers, lopende test en alle resultaten.
func (s *Server) writeState(w leanhttp.ResponseWriter) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	type testInfo struct {
		Name string `json:"name"`
		Desc string `json:"desc"`
	}
	state := struct {
		Node    map[string]any     `json:"node"`
		Idle    idleWindow         `json:"idle"`
		Temp    int                `json:"temp_milli_c"`
		Running string             `json:"running"`
		Note    string             `json:"note"`
		Tests   []testInfo         `json:"tests"`
		Results map[string]*Result `json:"results"`
	}{
		Node: map[string]any{
			"version":  s.cfg.Version,
			"arch":     s.cfg.Arch,
			"runtime":  s.cfg.Runtime,
			"slot":     s.cfg.Slot,
			"ram_mb":   s.cfg.RAMSize >> 20,
			"ip":       s.cfg.IP,
			"host":     s.cfg.Host,
			"port":     s.cfg.Port,
			"cores":    runtime.NumCPU(),
			"shared":   s.ctrl(s.cfg.Offsets.Shared) == 1,
			"uptime_s": int(time.Since(s.started).Seconds()),
			"heap_kb":  ms.HeapAlloc >> 10,
			"sys_mb":   ms.Sys >> 20,
			"num_gc":   ms.NumGC,
		},
		Idle:    s.idle.window(60 * time.Second),
		Temp:    s.temp.get(s.cfg),
		Results: map[string]*Result{},
	}
	for _, t := range tests {
		state.Tests = append(state.Tests, testInfo{t.Name, t.Desc})
	}
	s.mu.Lock()
	state.Running, state.Note = s.running, s.note
	for k, v := range s.results {
		state.Results[k] = v
	}
	s.mu.Unlock()

	b, err := json.Marshal(state)
	if err != nil {
		leanhttp.Error(w, err.Error(), leanhttp.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// ctrl leest één control-page-woord; 0 als er geen board is (host-build).
func (s *Server) ctrl(off uint64) uint64 {
	if s.cfg.CtrlRead == nil {
		return 0
	}
	return s.cfg.CtrlRead(off)
}
