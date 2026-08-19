package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/edgeproto"
	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/ingress"
	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/origin"
	"github.com/xinix00/lean/leanh2"
)

// De koppen waarmee de edge zegt wat een stream is. Dit zijn cloudflared's
// eigen namen (connection/http2.go), dus hun documentatie geldt hier ook.
const (
	upgradeHeader     = "cf-cloudflared-proxy-connection-upgrade"
	upgradeControl    = "control-stream"
	upgradeConfig     = "update-configuration"
	upgradeWebsocket  = "websocket"
	tcpProxySrcHeader = "cf-cloudflared-proxy-src"
)

// Options is wat een tunnel nodig heeft.
type Options struct {
	Token    Token
	Fallback string // waar verkeer heen gaat vóór de eerste config-push
	Version  string
	Arch     string
	// Connections is hoeveel edge-verbindingen we aanhouden. Cloudflare's eigen
	// standaard is vier: twee regio's, twee verbindingen elk, zodat één
	// wegvallende edge-machine geen gat maakt.
	Connections int
	Logf        func(string, ...any)
	// Proxy voert een verzoek uit naar de lokale dienst. De tunnel weet niet hoe
	// HTTP naar buiten gaat — op een node is dat leanhttp over appnet, op een
	// host gewoon leanhttp. Dat scheidt het protocol van het vervoer.
	Proxy Proxy
}

// Proxy brengt één verzoek naar de lokale dienst en schrijft het antwoord terug.
type Proxy func(service string, req *leanh2.Request, res *leanh2.Response) error

// Tunnel houdt de verbindingen naar de edge aan.
type Tunnel struct {
	opt   Options
	rules *ingress.Set

	mu       sync.Mutex
	details  map[int]*ConnectionDetails // per verbindingsindex, als hij staat
	attempts map[int]uint8
}

// New maakt een tunnel; Run begint hem.
func New(opt Options) (*Tunnel, error) {
	if opt.Token.AccountTag == "" {
		return nil, errors.New("tunnel needs a token")
	}
	if opt.Connections <= 0 {
		opt.Connections = 4
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.Proxy == nil {
		return nil, errors.New("tunnel needs a proxy")
	}
	return &Tunnel{
		opt:      opt,
		rules:    ingress.New(opt.Fallback),
		details:  map[int]*ConnectionDetails{},
		attempts: map[int]uint8{},
	}, nil
}

// Rules geeft de routeertabel, voor een statuspagina of een log.
func (t *Tunnel) Rules() *ingress.Set { return t.rules }

// Run houdt alle verbindingen aan tot stop dichtgaat. Hij keert alleen terug als
// er niets meer te proberen valt.
func (t *Tunnel) Run(stop <-chan struct{}) error {
	addrs, err := EdgeAddrs(t.opt.Logf)
	if err != nil {
		return err
	}
	t.opt.Logf("cloudflared-lean: %d edge addresses, %d connections, fallback %s",
		len(addrs), t.opt.Connections, t.opt.Fallback)

	var wg sync.WaitGroup
	for i := 0; i < t.opt.Connections; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			t.keepConnected(index, addrs, stop)
		}(i)
	}
	wg.Wait()
	return nil
}

// keepConnected houdt één verbindingsindex bezet: opnieuw verbinden tot stop.
func (t *Tunnel) keepConnected(index int, addrs []string, stop <-chan struct{}) {
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		select {
		case <-stop:
			return
		default:
		}
		// Elke index begint bij een ander adres, en schuift door bij elke
		// poging: vier verbindingen op één edge-machine is geen HA.
		addr := addrs[(index+attempt)%len(addrs)]
		err := t.connectOnce(index, addr, stop)

		select {
		case <-stop:
			return
		default:
		}
		var refused *RegisterError
		switch {
		case errors.As(err, &refused) && !refused.ShouldRetry:
			// De edge zegt: niet opnieuw. Dat is een configuratiefout (een
			// ingetrokken token bijvoorbeeld) en die lost een herhaling niet op.
			t.opt.Logf("cloudflared-lean: connection %d given up: %v", index, err)
			return
		case errors.As(err, &refused) && refused.RetryAfter > 0:
			backoff = refused.RetryAfter
		}
		t.opt.Logf("cloudflared-lean: connection %d lost (%v); retrying in %s", index, err, backoff)
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// connectOnce doet één volledige verbinding: dial, h2, registreren, bedienen.
func (t *Tunnel) connectOnce(index int, addr string, stop <-chan struct{}) error {
	conn, err := DialEdge(addr, 15*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	registered := make(chan *ConnectionDetails, 1)
	h2conn := leanh2.NewConn(conn, func(req *leanh2.Request, res *leanh2.Response) {
		t.serve(index, req, res, registered)
	}, t.opt.Logf)

	// De verbinding sluiten als er gestopt wordt: dat breekt Serve af, en de
	// afmelding hieronder gaat er nog net voor langs.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			_ = h2conn.GoAway(0, "shutting down")
			conn.Close()
		case <-done:
		}
	}()

	serveErr := h2conn.Serve()
	t.mu.Lock()
	delete(t.details, index)
	t.mu.Unlock()
	return serveErr
}

// serve bedient één stream van de edge.
func (t *Tunnel) serve(index int, req *leanh2.Request, res *leanh2.Response, registered chan *ConnectionDetails) {
	switch strings.ToLower(req.Get(upgradeHeader)) {
	case upgradeControl:
		t.serveControl(index, req, res, registered)
	case upgradeConfig:
		t.serveConfigUpdate(req, res)
	default:
		if req.Get(tcpProxySrcHeader) != "" {
			// Een kale TCP-stroom (WARP of `cloudflared access`). Bewust niet
			// ondersteund in plaats van half: een 502 met een reden is
			// duidelijker dan een verbinding die stil niets doet.
			t.reply(res, 502, "this tunnel serves HTTP only")
			return
		}
		t.serveProxy(req, res)
	}
}

// serveControl handelt de control-stream af: 200 terug, dan de registratie, en
// daarna de stream open houden zolang de verbinding leeft.
func (t *Tunnel) serveControl(index int, req *leanh2.Request, res *leanh2.Response, registered chan *ConnectionDetails) {
	if err := res.WriteHeader(200, nil); err != nil {
		t.opt.Logf("cloudflared-lean: connection %d control stream: %v", index, err)
		return
	}
	t.mu.Lock()
	attempts := t.attempts[index]
	t.mu.Unlock()

	rw := &streamRW{r: req.Body, w: res}
	details, err := Register(rw, t.opt.Token, uint8(index), ClientInfo{
		ClientID: t.opt.Token.TunnelID,
		// Features zijn de namen waarmee cloudflared zegt wat hij kan. Wij
		// melden er bewust géén: elke naam hier is een belofte, en de edge
		// stuurt dan verkeer waarvan we het pad niet hebben.
		Features: nil,
		Version:  t.opt.Version,
		Arch:     t.opt.Arch,
	}, attempts)
	if err != nil {
		t.mu.Lock()
		if t.attempts[index] < 255 {
			t.attempts[index]++
		}
		t.mu.Unlock()
		t.opt.Logf("cloudflared-lean: connection %d not registered: %v", index, err)
		return
	}

	t.mu.Lock()
	t.details[index] = details
	t.attempts[index] = 0
	t.mu.Unlock()
	select {
	case registered <- details:
	default:
	}
	t.opt.Logf("cloudflared-lean: connection %d registered at %s (remotely managed: %v)",
		index, details.Location, details.RemotelyManaged)

	// De stream blijft open: de edge gebruikt hem voor latere RPC (een nette
	// afmelding bijvoorbeeld). We lezen door tot hij dichtgaat, want een
	// ongelezen stream vult vensters en dan valt de verbinding stil.
	for {
		if _, err := capnpDrain(rw); err != nil {
			return
		}
	}
}

// capnpDrain leest één RPC-bericht en gooit het weg. Onbekende aanroepen
// negeren is wat een client mag doen: de edge verwacht geen antwoord op iets
// dat hij niet aankondigde.
func capnpDrain(rw io.Reader) (int, error) {
	var head [8]byte
	if _, err := io.ReadFull(rw, head[:]); err != nil {
		return 0, err
	}
	words := int(uint32(head[4]) | uint32(head[5])<<8 | uint32(head[6])<<16 | uint32(head[7])<<24)
	if words < 0 || words > 1<<16 {
		return 0, errors.New("rpc message refused")
	}
	buf := make([]byte, words*8)
	if _, err := io.ReadFull(rw, buf); err != nil {
		return 0, err
	}
	return len(buf), nil
}

// serveConfigUpdate neemt een config-push aan.
func (t *Tunnel) serveConfigUpdate(req *leanh2.Request, res *leanh2.Response) {
	var body struct {
		Version int32           `json:"version"`
		Config  json.RawMessage `json:"config"`
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	answer := struct {
		LastAppliedVersion int32  `json:"lastAppliedVersion"`
		Err                string `json:"err,omitempty"`
	}{}
	if err != nil {
		answer.LastAppliedVersion = t.rules.Table().Version
		answer.Err = err.Error()
	} else {
		applied, uerr := t.rules.Update(body.Version, body.Config)
		answer.LastAppliedVersion = applied
		if uerr != nil {
			answer.Err = uerr.Error()
			t.opt.Logf("cloudflared-lean: configuration %d refused: %v", body.Version, uerr)
		} else {
			t.opt.Logf("cloudflared-lean: configuration %d applied:", applied)
			for _, line := range t.rules.Describe() {
				t.opt.Logf("cloudflared-lean:   %s", line)
			}
		}
	}
	out, _ := json.Marshal(answer)
	_ = res.WriteHeader(200, map[string][]string{"content-type": {"application/json"}})
	_, _ = res.Write(out)
}

// serveProxy brengt een bezoekersverzoek naar de lokale dienst.
func (t *Tunnel) serveProxy(req *leanh2.Request, res *leanh2.Response) {
	if strings.ToLower(req.Get(upgradeHeader)) == upgradeWebsocket {
		// Websockets vragen een tweerichtings-pomp op één stream. Het pad
		// bestaat (h2 kan het), maar zonder een echte test tegen een
		// websocket-oorsprong beloven we het niet.
		t.reply(res, 502, "this tunnel does not carry websockets yet")
		return
	}
	host := req.Authority
	if h := req.Get("host"); host == "" && h != "" {
		host = h
	}
	// De poort hoort niet bij de hostname in een ingress-regel.
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	service, ok := t.rules.Route(host, req.Path)
	if !ok {
		t.reply(res, 404, "no ingress rule matches this hostname and path")
		return
	}
	// De vaste "diensten" van Cloudflare: een status teruggeven zonder oorsprong.
	if strings.HasPrefix(service, "http_status:") {
		code := 404
		if n, err := atoi(service[len("http_status:"):]); err == nil {
			code = n
		}
		t.reply(res, code, "")
		return
	}
	if err := t.opt.Proxy(service, req, res); err != nil {
		t.opt.Logf("cloudflared-lean: stream %d to %s: %v", req.StreamID, service, err)
		if errors.Is(err, origin.ErrBodyTooLarge) {
			t.reply(res, 413, "this tunnel buffers request bodies; this one is too large")
			return
		}
		t.reply(res, 502, "the local service did not answer")
	}
}

// reply is een antwoord dat de TUNNEL maakt en niet de oorsprong — een 404
// omdat geen regel past, een 502 omdat de dienst niet opnam. De meta-kop zegt
// dat ook eerlijk (`cloudflared` i.p.v. `origin`), want de edge onderscheidt het
// in zijn eigen foutpagina's en statistiek.
func (t *Tunnel) reply(res *leanh2.Response, code int, msg string) {
	_ = res.WriteHeader(code, edgeproto.Headers(
		map[string][]string{"content-type": {"text/plain; charset=utf-8"}}, edgeproto.FromTunnel))
	if msg != "" {
		_, _ = res.Write([]byte(msg + "\n"))
	}
}

// Details geeft de gelande verbindingen, voor een statusregel.
func (t *Tunnel) Details() map[int]ConnectionDetails {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[int]ConnectionDetails, len(t.details))
	for i, d := range t.details {
		out[i] = *d
	}
	return out
}

// streamRW maakt van de twee helften van een h2-stream één ReadWriter, zodat de
// capnp-laag niets van HTTP hoeft te weten.
type streamRW struct {
	r io.Reader
	w *leanh2.Response
}

func (s *streamRW) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *streamRW) Write(p []byte) (int, error) { return s.w.Write(p) }

func atoi(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty number")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(s[i]-'0')
		if n > 599 {
			return 0, errors.New("status out of range")
		}
	}
	return n, nil
}
