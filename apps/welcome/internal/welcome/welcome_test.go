package welcome

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// testNode is een node zoals HOP hem meegeeft: gepubliceerd op poort 80 van het
// node-IP, één core, 64 MB partitie.
func testNode() Node {
	return Node{
		Host:    "10.0.0.7",
		IP:      "10.100.0.5",
		Port:    "80",
		DNS:     "10.100.0.1:5353",
		Slot:    3,
		Cores:   1,
		RAMSize: 67108864,
		Arch:    "arm64",
		Runtime: "tamago/go1.26.4",
		Version: "v0.20.3-release",
	}
}

func TestURLAndBanner(t *testing.T) {
	n := testNode()
	if got, want := URL(n), "http://10.0.0.7"; got != want {
		t.Errorf("URL op poort 80 = %q, wil %q (80 blijft impliciet)", got, want)
	}
	n.Port = "18080"
	if got, want := URL(n), "http://10.0.0.7:18080"; got != want {
		t.Errorf("URL = %q, wil %q", got, want)
	}
	if got, want := URL(Node{Host: "10.0.0.7", Port: ""}), "http://10.0.0.7"; got != want {
		t.Errorf("URL zonder poort = %q, wil %q", got, want)
	}

	// De banner is één bron voor de logregel én het voorbeeld op de pagina.
	if got, want := Banner(testNode()),
		"welcome v0.20.3-release: slot 3, 1 core, 64.0 MB RAM, serving http://10.0.0.7"; got != want {
		t.Errorf("Banner =\n  %q\nwil\n  %q", got, want)
	}
	multi := testNode()
	multi.Cores = 4
	if got := Banner(multi); !strings.Contains(got, "4 cores") {
		t.Errorf("Banner met 4 cores = %q, wil meervoud", got)
	}
}

func TestHostFallsBackToOwnIP(t *testing.T) {
	// Geen HOPOS_HOST (niet gepubliceerd): dan is het interne IP het enige
	// adres dat werkt, en dat moet de pagina noemen — niet een leeg veld.
	n := testNode()
	n.Host = ""
	s := NewServer(n, nil)
	if got := s.Node().Host; got != n.IP {
		t.Errorf("Host zonder HOPOS_HOST = %q, wil de eigen IP %q", got, n.IP)
	}
	if !strings.Contains(string(Page(s.Node(), s.Status())), n.IP) {
		t.Error("pagina noemt het eigen IP niet als er geen node-adres is")
	}
}

func TestFormatters(t *testing.T) {
	bytes := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2 kB"},
		{1572864, "1.5 MB"},
		{67108864, "64.0 MB"},
		{175 << 20, "175.0 MB"},
		{2 << 30, "2.00 GB"},
	}
	for _, c := range bytes {
		if got := fmtBytes(c.in); got != c.want {
			t.Errorf("fmtBytes(%d) = %q, wil %q", c.in, got, c.want)
		}
	}

	durs := []struct {
		in   int64
		want string
	}{
		{-1, "0s"},
		{0, "0s"},
		{9, "9s"},
		{72, "1m 12s"},
		{3723, "1h 02m 03s"},
		{90061, "1d 01h 01m"},
	}
	for _, c := range durs {
		if got := fmtDuration(c.in); got != c.want {
			t.Errorf("fmtDuration(%d) = %q, wil %q", c.in, got, c.want)
		}
	}

	counts := []struct {
		in   uint64
		want string
	}{{0, "0"}, {999, "999"}, {1000, "1,000"}, {1234567, "1,234,567"}}
	for _, c := range counts {
		if got := fmtCount(c.in); got != c.want {
			t.Errorf("fmtCount(%d) = %q, wil %q", c.in, got, c.want)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	got := string(StatusJSON(Status{
		UptimeSeconds: 61, HeapBytes: 1234567, HeapObjects: 4321,
		Goroutines: 6, Views: 2, Requests: 9, Slot: 3, Cores: 1, RAMBytes: 67108864,
		Core: CoreShared,
	}))
	want := `{"uptime_seconds":61,"heap_bytes":1234567,"heap_objects":4321,` +
		`"goroutines":6,"views":2,"requests":9,"slot":3,"cores":1,"ram_bytes":67108864,` +
		`"core":"shared"}`
	if got != want {
		t.Errorf("StatusJSON =\n  %s\nwil\n  %s", got, want)
	}
	// Zonder sampler (host, oudere node) mag er geen claim in staan.
	if unknown := string(StatusJSON(Status{})); !strings.Contains(unknown, `"core":"unknown"`) {
		t.Errorf("lege Status =\n  %s\nwil core=unknown", unknown)
	}
}

func TestCoreStateIsReadEveryTimeAndNeverAssumed(t *testing.T) {
	// HOP zet CtrlShared om terwijl de app draait (buur komt erbij, buur gaat
	// weg), dus de sampler moet elke keer opnieuw gelezen worden — niet één keer
	// bij het starten onthouden.
	state := CoreDedicated
	calls := 0
	s := NewServer(testNode(), func() CoreState { calls++; return state })

	if got := s.Status().Core; got != CoreDedicated {
		t.Errorf("Core = %v, wil dedicated", got)
	}
	state = CoreShared
	if got := s.Status().Core; got != CoreShared {
		t.Errorf("Core = %v na omzetten, wil shared — waarde werd gecachet", got)
	}
	if calls != 2 {
		t.Errorf("sampler %d× gelezen, wil 2×", calls)
	}

	// Zonder sampler claimt niets iets.
	if got := NewServer(testNode(), nil).Status().Core; got != CoreUnknown {
		t.Errorf("zonder sampler = %v, wil unknown", got)
	}
}

// TestPageNeverPromisesAWholeCore is de reden dat dit alles bestaat: hele cores
// zijn de default, geen wet. Zit er een buur op de core, dan mag die belofte
// nergens op de pagina staan — en ook met een eigen core is het een meting van
// dit moment, niet een eigenschap.
func TestPageNeverPromisesAWholeCore(t *testing.T) {
	shared := string(Page(testNode(), Status{Core: CoreShared}))
	for _, lie := range []string{
		"to itself",
		"nobody else on this core",
		"a whole core",
		"not a time slice",
	} {
		if strings.Contains(shared, lie) {
			t.Errorf("pagina belooft %q terwijl de core gedeeld wordt", lie)
		}
	}
	for _, want := range []string{"shares with at least one other slot", "sharing this core", ">shared<"} {
		if !strings.Contains(shared, want) {
			t.Errorf("pagina zegt niet dat de core gedeeld wordt (mist %q)", want)
		}
	}

	// Met een eigen core mag het er wél staan, maar als moment en niet als belofte.
	ded := string(Page(testNode(), Status{Core: CoreDedicated}))
	for _, want := range []string{"to itself at this moment", "nobody else on this core right now", ">dedicated<"} {
		if !strings.Contains(ded, want) {
			t.Errorf("pagina mist %q bij een dedicated core", want)
		}
	}
	if strings.Contains(ded, "sharing this core") {
		t.Error("pagina zegt dat de core gedeeld wordt terwijl hij dat niet is")
	}

	// Weten we het niet, dan geen van beide claims.
	unknown := string(Page(testNode(), Status{Core: CoreUnknown}))
	for _, lie := range []string{"to itself", "a whole core", "sharing this core", "nobody else on this core"} {
		if strings.Contains(unknown, lie) {
			t.Errorf("pagina claimt %q terwijl de core-status onbekend is", lie)
		}
	}

	// En in alle drie de gevallen staat erbij dat delen MAG maar niet MOET: HOP
	// stapelt alleen kooien op één core als een job daar met een sharegroup om
	// vraagt. Zonder die nuance leest "shared" als iets dat je overkomt.
	for _, page := range []string{shared, ded, unknown} {
		if !strings.Contains(page, "allowed but never imposed") {
			t.Error("pagina zegt niet dat delen mag maar niet moet")
		}
		if !strings.Contains(page, "sharegroup") {
			t.Error("pagina noemt het mechanisme (sharegroup) niet")
		}
	}
}

// TestPageNeverCallsTheSlotACoreIndex pint de tweede correctie vast: app.Slot is
// het KOOInummer (HOP patcht slotHint bij Start), niet de index van de fysieke
// core. Welke core een kooi kreeg weet een app niet, en met een sharegroup zitten
// er meerdere kooien op één core — dus mag de pagina die twee nooit gelijkstellen.
func TestPageNeverCallsTheSlotACoreIndex(t *testing.T) {
	for _, st := range []Status{{Core: CoreDedicated}, {Core: CoreShared}, {Core: CoreUnknown}} {
		page := string(Page(testNode(), st))
		for _, lie := range []string{
			"index of the core",
			"is the core index",
			"slot / core",
			"slot · core",
		} {
			if strings.Contains(page, lie) {
				t.Errorf("pagina stelt slot en core gelijk (%q) bij core=%v", lie, st.Core)
			}
		}
		if !strings.Contains(page, "the cage HOP started this app in") {
			t.Errorf("pagina legt het slotnummer niet uit als kooi (core=%v)", st.Core)
		}
	}
}

func TestStatusCountsUptimeFromReady(t *testing.T) {
	s := NewServer(testNode(), nil)
	s.now = func() time.Time { return s.start.Add(90 * time.Second) }
	if got := s.Status().UptimeSeconds; got != 90 {
		t.Errorf("uptime = %d, wil 90", got)
	}
}

func TestPageIsSelfContainedAndComplete(t *testing.T) {
	page := string(Page(testNode(), Status{UptimeSeconds: 61, HeapBytes: 1572864, Views: 2, Requests: 9}))

	// Geen enkele __TOKEN__ mag blijven staan: dat zou als kale tekst op de
	// pagina belanden.
	if i := strings.Index(page, "__"); i >= 0 {
		t.Errorf("onvervangen token in de pagina: %q", page[i:min(i+24, len(page))])
	}

	// Alles wat de pagina moet zeggen, staat erin.
	for _, want := range []string{
		"10.0.0.7",                 // node-adres
		"10.100.0.5",               // eigen IP
		"10.100.0.1:5353",          // HOP_DNS-regel
		">3<",                      // slot in een cel
		"64.0 MB",                  // RAM-partitie
		"arm64",                    // architectuur
		"tamago/go1.26.4",          // runtime
		"v0.20.3-release",          // versie
		"1m 01s",                   // uptime uit Status
		"1.5 MB",                   // heap uit Status
		"welcome-arm64-tamago.elf", // artifact-URL van dít image
		"/api/status",              // de poll
		"/healthz",
		"(\\(\\", // het konijn
		"🐇",      // favicon
	} {
		if !strings.Contains(page, want) {
			t.Errorf("pagina mist %q", want)
		}
	}

	// Zelfstandig: geen enkel extern verzoek. Links naar gethop.org mogen (die
	// klikt een mens op zijn laptop), maar niets wat de browser zelf ophaalt.
	for _, bad := range []string{"<script src", "<link rel=\"stylesheet\"", "@import", "url(http", "<img "} {
		if strings.Contains(page, bad) {
			t.Errorf("pagina haalt iets extern op: %q", bad)
		}
	}

	// Zonder HOP_DNS verdwijnt die regel in plaats van leeg te blijven staan.
	n := testNode()
	n.DNS = ""
	if strings.Contains(string(Page(n, Status{})), "cluster dns") {
		t.Error("dns-regel staat er terwijl HOP_DNS leeg is")
	}
}

func TestPageEscapesEnv(t *testing.T) {
	// HOPOS_HOST en HOP_DNS zijn tekst van buiten de app: die hoort niet in de
	// HTML-grammatica te kunnen stappen.
	n := testNode()
	n.Host = `1.2.3.4"><script>alert(1)</script>`
	page := string(Page(n, Status{}))
	if strings.Contains(page, "<script>alert(1)") {
		t.Error("node-adres komt ongeëscaped in de pagina")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("node-adres is niet geëscaped zichtbaar gemaakt")
	}
}

// TestServeEndToEnd zet de echte apphttp-server op een echte listener en praat
// er met de echte client tegen: routering, statuscodes, content-types en de
// tellers, precies zoals een browser op de node het zou zien.
func TestServeEndToEnd(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	s := NewServer(testNode(), nil)
	go leanhttp.Serve(l, s.Handle)
	base := "http://" + l.Addr().String()

	get := func(path string) (*leanhttp.Response, string) {
		t.Helper()
		resp, err := leanhttp.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s body: %v", path, err)
		}
		return resp, string(body)
	}

	// De pagina.
	resp, body := get("/")
	if resp.StatusCode != leanhttp.StatusOK {
		t.Errorf("GET / = %d, wil 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET / content-type = %q", ct)
	}
	if !strings.HasPrefix(body, "<!DOCTYPE html>") || !strings.Contains(body, "HopOS") {
		t.Errorf("GET / levert geen pagina (%d bytes)", len(body))
	}

	// De status-JSON: na één pagina-hit staat de teller op 1.
	resp, body = get("/api/status")
	if resp.StatusCode != leanhttp.StatusOK {
		t.Errorf("GET /api/status = %d, wil 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("status content-type = %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("status mag niet gecachet worden")
	}
	if !strings.Contains(body, `"views":1`) || !strings.Contains(body, `"slot":3`) {
		t.Errorf("status = %s", body)
	}

	// /healthz: kort, en het telt niet mee in de tellers.
	resp, body = get("/healthz")
	if resp.StatusCode != leanhttp.StatusOK || body != "ok\n" {
		t.Errorf("GET /healthz = %d %q, wil 200 \"ok\\n\"", resp.StatusCode, body)
	}
	if _, body = get("/api/status"); !strings.Contains(body, `"views":1`) {
		t.Errorf("healthz heeft de bezoekersteller aangeraakt: %s", body)
	}

	// Onbekend pad en verkeerde methode. Via Do, want leanhttp.Get maakt van een
	// 404 zelf een fout.
	miss, err := leanhttp.Do(leanhttp.Call{URL: base + "/favicon.ico"})
	if err != nil {
		t.Fatalf("GET /favicon.ico: %v", err)
	}
	miss.Body.Close()
	if miss.StatusCode != leanhttp.StatusNotFound {
		t.Errorf("GET /favicon.ico = %d, wil 404", miss.StatusCode)
	}
	post, err := leanhttp.Do(leanhttp.Call{Method: "POST", URL: base + "/", Body: []byte("x")})
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	post.Body.Close()
	if post.StatusCode != leanhttp.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, wil 405", post.StatusCode)
	}
	if got := post.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, wil \"GET, HEAD\"", got)
	}

	// HEAD: een kop zonder body (een uptime-check hoort geen 28 kB te trekken).
	head, err := leanhttp.Do(leanhttp.Call{Method: "HEAD", URL: base + "/"})
	if err != nil {
		t.Fatalf("HEAD /: %v", err)
	}
	defer head.Body.Close()
	if head.StatusCode != leanhttp.StatusOK {
		t.Errorf("HEAD / = %d, wil 200", head.StatusCode)
	}
	if hbody, _ := io.ReadAll(head.Body); len(hbody) != 0 {
		t.Errorf("HEAD / gaf %d body-bytes", len(hbody))
	}
}
