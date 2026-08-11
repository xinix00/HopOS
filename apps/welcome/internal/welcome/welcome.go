// Package welcome serveert de enige pagina die een verse HopOS-node laat zien
// dat hij leeft: één zelfstandige HTML-pagina op de gepubliceerde poort, plus
// een klein /api/status dat de pagina zelf pollt en een /healthz voor wie
// alleen "ok" wil horen.
//
// Alles wat de pagina toont weet de app van zichzelf: zijn eigen runtime, zijn
// eigen netstack, en de env die HOP hem bij de start meegaf (HOPOS_HOST,
// HOP_DNS, ER_PORT_*). Er wordt bewust NIETS aan de agent-API gevraagd: dat
// zou HMAC met de cluster-key vereisen, en een welkomstpagina die zonder key
// niks toont is precies de frictie die dit ding moet wegnemen. Node-naam en
// cluster-naam staan dus niet op de pagina — die bestaan hier niet, en verzinnen
// is erger dan weglaten.
//
// De pagina en de statushandler zijn hier host-testbaar: apphttp is gewoon Go
// (het is applib dat tamago-only is), dus de routering, de HTML en de JSON
// gaan door echte tests, en alleen de main hangt aan de tamago-gate.
package welcome

import (
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// CoreState zegt of deze app de fysieke core voor zichzelf heeft. HopOS geeft
// standaard hele cores, maar met een sharegroup kan HOP meerdere kooien op één
// core zetten als je die dichtheid wilt — dan is "een hele core" een leugen, en
// die staat niet op een pagina die de node moet uitleggen.
//
// HOP houdt het bij in CtrlShared en werkt het BIJ zolang de app leeft (een
// buur die erbij komt of weggaat zet het om), dus dit is geen opstartwaarde maar
// een levend cijfer: het staat in /api/status en loopt mee.
type CoreState int

const (
	CoreUnknown   CoreState = iota // niet uit te lezen (host-tests, oudere node)
	CoreDedicated                  // deze app is de enige bewoner van zijn core
	CoreShared                     // er woont minstens één ander slot op deze core
)

func (c CoreState) String() string {
	switch c {
	case CoreDedicated:
		return "dedicated"
	case CoreShared:
		return "shared"
	default:
		return "unknown"
	}
}

// Node is wat de app zonder iemand iets te vragen over zichzelf weet. Leeg mag:
// een veld dat HOP niet meegaf laat de pagina weg in plaats van te gokken.
type Node struct {
	Host string // HOPOS_HOST — het node-IP waarop deze poort gepubliceerd is
	IP   string // eigen IP op het interne net, uit appnet.Up
	Port string // ER_PORT_HTTP — de poort die HOP publiceerde
	DNS  string // HOP_DNS (leeg = niet meegegeven)

	// Slot is het KOOInummer: HOP patcht het als slotHint in het image bij Start
	// (board/hopslot valt alleen terug op MPIDR als die hint ontbreekt, en op
	// servers is MPIDR geen slotnummer). Het is dus NIET de index van de fysieke
	// core — welke core de kooi kreeg weet een app niet, en met een sharegroup
	// zitten er meerdere kooien op één core. Of deze app die core alleen heeft:
	// zie CoreState.
	Slot int

	Cores   int    // cores van deze app (SMP zet HopOS zelf)
	RAMSize uint64 // de partitie die HOP gaf — harde grens, geen quota
	Arch    string // runtime.GOARCH: arm64 óf riscv64
	Runtime string // runtime.Version(): tamago/…
	Version string // versie van dit image
}

// Server bedient de drie paden: de pagina, de status-JSON en /healthz.
type Server struct {
	node  Node
	start time.Time
	now   func() time.Time // testhaak; nil = time.Now
	core  func() CoreState // leest CtrlShared; nil = CoreUnknown

	views atomic.Uint64 // pagina-opvragingen ("/")
	reqs  atomic.Uint64 // alle behandelde verzoeken behalve /healthz
}

// NewServer zet de uptime-klok op nu: dat is het moment waarop applib.Init()
// READY meldde, niet dat waarop de node aanging — de pagina zegt dat ook zo.
//
// core wordt bij elke opvraging opnieuw gelezen (HOP werkt CtrlShared bij
// terwijl de app draait); nil betekent "niet uit te lezen" en dan claimt de
// pagina niets over de core.
func NewServer(n Node, core func() CoreState) *Server {
	if n.Host == "" {
		n.Host = n.IP // niet gepubliceerd: dan is het interne IP het enige adres
	}
	return &Server{node: n, start: time.Now(), core: core}
}

// Node geeft de node-gegevens van deze server, met de HOPOS_HOST-terugval er
// al in verwerkt.
func (s *Server) Node() Node { return s.node }

// Banner is de regel die main logt en die de pagina als voorbeeld-uitvoer van
// `hop logs welcome` laat zien. Eén bron voor beide: wat je op de pagina leest
// is letterlijk wat er in de logs staat.
func Banner(n Node) string {
	unit := "cores"
	if n.Cores == 1 {
		unit = "core"
	}
	return fmt.Sprintf("welcome %s: slot %d, %d %s, %s RAM, serving %s",
		n.Version, n.Slot, n.Cores, unit, fmtBytes(n.RAMSize), URL(n))
}

// URL is het adres waarop deze app van buiten én van de node zelf te bereiken
// is: het node-IP met de gepubliceerde poort (poort 80 blijft impliciet).
func URL(n Node) string {
	if n.Port == "" || n.Port == "80" {
		return "http://" + n.Host
	}
	return "http://" + n.Host + ":" + n.Port
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Status is de momentopname die /api/status teruggeeft: alleen getallen, zodat
// de pagina ze zelf kan opmaken en er geen escaping aan te pas komt.
type Status struct {
	UptimeSeconds int64
	HeapBytes     uint64
	HeapObjects   uint64
	Goroutines    int
	Views         uint64
	Requests      uint64
	Slot          int
	Cores         int
	RAMBytes      uint64
	Core          CoreState
}

// Status leest de levende cijfers: uptime van de app zelf, de heap uit de
// Go-runtime (die op deze core hét OS is) en de eigen tellers.
func (s *Server) Status() Status {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	core := CoreUnknown
	if s.core != nil {
		core = s.core()
	}
	return Status{
		Core:          core,
		UptimeSeconds: int64(s.clock().Sub(s.start) / time.Second),
		HeapBytes:     m.HeapAlloc,
		HeapObjects:   m.HeapObjects,
		Goroutines:    runtime.NumGoroutine(),
		Views:         s.views.Load(),
		Requests:      s.reqs.Load(),
		Slot:          s.node.Slot,
		Cores:         s.node.Cores,
		RAMBytes:      s.node.RAMSize,
	}
}

// Handle is de hele mux: drie paden, de rest 404. Routeren op r.Path doet een
// apphttp-handler zelf — een mux is een switch, en die kun je beter zien.
func (s *Server) Handle(w leanhttp.ResponseWriter, r *leanhttp.Request) {
	// /healthz telt niet mee: een uptime-check die elke seconde langskomt
	// hoort de bezoekersteller niet op te blazen.
	if r.Path == "/healthz" {
		s.respond(w, r, "text/plain; charset=utf-8", []byte("ok\n"))
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		leanhttp.Error(w, "method not allowed", leanhttp.StatusMethodNotAllowed)
		return
	}
	switch r.Path {
	case "/":
		s.reqs.Add(1)
		s.views.Add(1)
		s.respond(w, r, "text/html; charset=utf-8", Page(s.node, s.Status()))
	case "/api/status":
		s.reqs.Add(1)
		w.Header().Set("Cache-Control", "no-store")
		s.respond(w, r, "application/json", StatusJSON(s.Status()))
	default:
		s.reqs.Add(1)
		leanhttp.Error(w, "not found — this node serves / and /api/status\n", leanhttp.StatusNotFound)
	}
}

// respond schrijft het antwoord. Op een HEAD gaat alleen de kop de deur uit:
// apphttp leidt Content-Length af uit wat de handler schrijft, dus dat is een
// eerlijke lege 200 in plaats van een verzonnen lengte.
func (s *Server) respond(w leanhttp.ResponseWriter, r *leanhttp.Request, ctype string, body []byte) {
	w.Header().Set("Content-Type", ctype)
	if r.Method == "HEAD" {
		w.WriteHeader(leanhttp.StatusOK)
		return
	}
	w.Write(body)
}

// StatusJSON schrijft het antwoord van /api/status met de hand. encoding/json
// zou reflect meelinken voor negen getallen en één woord, en een app-image
// betaalt elke byte in zijn RAM-partitie. Escapen hoeft niet: alles is een
// integer behalve de core-status, en die is een vaste enum (%q eromheen).
func StatusJSON(st Status) []byte {
	return fmt.Appendf(nil, `{"uptime_seconds":%d,"heap_bytes":%d,"heap_objects":%d,`+
		`"goroutines":%d,"views":%d,"requests":%d,"slot":%d,"cores":%d,"ram_bytes":%d,`+
		`"core":%q}`,
		st.UptimeSeconds, st.HeapBytes, st.HeapObjects, st.Goroutines,
		st.Views, st.Requests, st.Slot, st.Cores, st.RAMBytes, st.Core)
}

// fmtBytes maakt van een getal een leesbare grootte. Basis 1024 met de korte
// labels: 67108864 wordt "64.0 MB", precies zoals de jobspec het meegaf. De JS
// op de pagina doet exact hetzelfde, zodat de eerste render en de eerste poll
// niet van vorm verschillen.
func fmtBytes(n uint64) string {
	switch f := float64(n); {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", f/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// fmtDuration schrijft een uptime uit; ook hier heeft de JS een tweelingbroer.
func fmtDuration(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	d, h, m, s := sec/86400, sec%86400/3600, sec%3600/60, sec%60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %02dh %02dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// fmtCount groepeert duizenden met een dunne komma, zoals de JS
// (toLocaleString) op de pagina.
func fmtCount(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
