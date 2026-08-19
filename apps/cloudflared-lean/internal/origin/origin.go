// Package origin brengt een verzoek van de edge naar de lokale dienst.
//
// Eén implementatie voor host én slot: leanhttp praat op beide dezelfde HTTP/1.1
// (op een node over appnet, op een host over de gewone stack), dus hier hoort
// geen build-tag. Dat is ook waarom een test op de Mac iets zegt over de node.
//
// De edge-kant is HTTP/2 en dit is HTTP/1.1. Dat is precies de vertaling die een
// tunnel doet: multiplexen naar buiten, gewone verzoeken naar binnen.
package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/lean/leanh2"
	"github.com/xinix00/lean/leanhttp"
)

// maxBufferedBody is het plafond voor een verzoek-body zonder content-length.
// leanhttp chunkt geen uploads (BodyReader eist BodyLen), dus zo'n body moet
// eerst compleet zijn. Een node heeft 32MB, dus dit is bewust bescheiden: wie
// grote bestanden door de tunnel duwt, hoort een lengte mee te sturen.
const maxBufferedBody = 8 << 20

// originHeaderTimeout begrenst het wachten op de ANTWOORDKOPPEN van de lokale
// dienst, niet de overdracht daarna: een dode dienst mag geen stream vasthouden,
// een groot bestand mag zo lang duren als het duurt.
const originHeaderTimeout = 30 * time.Second

// hopByHop zijn de koppen die bij ONZE verbinding met de edge horen en niet bij
// het verzoek aan de oorsprong (RFC 9110 §7.6.1, plus cloudflared's eigen
// upgrade-koppen).
//
// leanhttp bezit daarnaast zelf de framing-koppen (Host, Content-Length,
// Connection, Transfer-Encoding, Expect) en weigert een aanroeper die ze zet —
// terecht, want anders zeggen de kop en het frame iets anders. Ze staan hier dus
// óók in: content-length komt terug als BodyLen, host als de URL.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true,
	"upgrade": true, "proxy-connection": true, "te": true, "trailer": true,
	"host": true, "content-length": true, "expect": true,
	"cf-cloudflared-proxy-connection-upgrade": true,
	"cf-cloudflared-proxy-src":                true,
}

// De Host-kop hoort die van de bezoeker te zijn, niet die van het slot-adres:
// een dienst met meer dan één hostnaam routeert erop, en een dienst die
// absolute URL's bouwt zet hem in zijn antwoorden.
//
// leanhttp bezit Host met opzet en leidt hem af uit de URL. Daarom draagt de
// URL hier de PUBLIEKE hostnaam en stuurt DialContext de verbinding naar het
// echte adres van de dienst — de naad die leanhttp daar zelf voor heeft. Eén
// gevolg: de verbindingspool zit in de URL-host, dus elke dienst krijgt zijn
// eigen Client. Anders zou een tweede regel op dezelfde hostnaam (zelfde host,
// ander pad, andere dienst) een verbinding van de eerste hergebruiken.
var (
	clientsMu sync.Mutex
	clients   = map[string]*leanhttp.Client{}
)

func clientFor(service string) (*leanhttp.Client, string, error) {
	addr, err := serviceAddr(service)
	if err != nil {
		return nil, "", err
	}
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c := clients[service]; c != nil {
		return c, addr, nil
	}
	c := &leanhttp.Client{}
	clients[service] = c
	return c, addr, nil
}

// serviceAddr haalt host:poort uit een dienst-URL uit de ingress-regel.
func serviceAddr(service string) (string, error) {
	rest, ok := strings.CutPrefix(service, "http://")
	if !ok {
		if strings.HasPrefix(service, "https://") {
			// Een oorsprong achter TLS vraagt leanhttps plus een keuze over
			// certificaatverificatie (Cloudflare's default is "verify niet" en
			// dat is geen keuze die hier stil gemaakt hoort te worden).
			return "", errors.New("https origins are not carried yet; use http:// in the ingress rule")
		}
		return "", fmt.Errorf("service %q is not an http:// address", service)
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", fmt.Errorf("service %q has no host", service)
	}
	if _, _, err := net.SplitHostPort(rest); err != nil {
		rest = net.JoinHostPort(rest, "80")
	}
	return rest, nil
}

// Proxy voert het verzoek uit en schrijft het antwoord terug als DATA-frames.
func Proxy(service string, req *leanh2.Request, res *leanh2.Response) error {
	client, addr, err := clientFor(service)
	if err != nil {
		return err
	}
	// De publieke hostnaam in de URL, het echte adres in de dialer.
	host := req.Authority
	if host == "" {
		host = req.Get("host")
	}
	if host == "" {
		host = addr
	}
	call := leanhttp.Call{
		Method: req.Method,
		URL:    "http://" + host + pathOf(service, req.Path),
		Header: leanhttp.Header{},
		// De oorsprong mag zelf beslissen hoe lang hij nadenkt; wij zetten alleen
		// een grens op het wachten op de KOPPEN, zodat een dode dienst niet een
		// stream vasthoudt. Een groot bestand mag daarna zo lang duren als nodig.
		HeaderTimeout: originHeaderTimeout,
		NoFollow:      true, // een omleiding is voor de bezoeker, niet voor ons
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp4", addr)
		},
	}
	for name, values := range req.Header {
		if hopByHop[name] {
			continue
		}
		// leanhttp's Header draagt één waarde per naam. Herhaalde velden vouwen
		// met komma's is de standaardvorm (RFC 9110 §5.2); cookies hebben hun
		// eigen scheiding.
		sep := ", "
		if name == "cookie" {
			sep = "; "
		}
		call.Header[name] = strings.Join(values, sep)
	}
	// Wie er echt aanklopt: de edge zet cf-connecting-ip, en dat is het enige
	// eerlijke antwoord — ons slot-IP zegt niemand iets.
	if ip := req.Get("cf-connecting-ip"); ip != "" {
		call.Header["x-forwarded-for"] = ip
		call.Header["x-forwarded-proto"] = "https"
	}

	if err := attachBody(&call, req); err != nil {
		return err
	}

	resp, err := client.Do(call)
	if err != nil {
		return fmt.Errorf("origin %s: %w", addr, err)
	}
	defer resp.Body.Close()

	out := map[string][]string{}
	for name, value := range resp.Header {
		lower := strings.ToLower(name)
		if hopByHop[lower] {
			continue
		}
		// content-length weglaten: wij hercoderen de body als frames, en het
		// stream-einde zegt waar hij ophoudt. Een verkeerde lengte is erger dan
		// geen lengte.
		if lower == "content-length" {
			continue
		}
		out[lower] = []string{value}
	}
	for _, c := range resp.SetCookie {
		out["set-cookie"] = append(out["set-cookie"], c)
	}
	if err := res.WriteHeader(resp.StatusCode, out); err != nil {
		return err
	}
	_, err = io.Copy(res, resp.Body)
	return err
}

// pathOf plakt een pad-voorvoegsel uit de dienst-URL vóór het verzoekpad. Een
// regel als http://10.0.0.5:8080/api betekent: alles hierheen, ónder /api.
func pathOf(service, path string) string {
	prefix := ""
	if rest, ok := strings.CutPrefix(service, "http://"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			prefix = strings.TrimSuffix(rest[i:], "/")
		}
	}
	if path == "" {
		path = "/"
	}
	return prefix + path
}

// attachBody hangt de verzoek-body aan de aanroep. Met een bekende lengte
// stroomt hij door; zonder lengte moet hij eerst compleet zijn omdat leanhttp
// geen chunked uploads doet.
func attachBody(call *leanhttp.Call, req *leanh2.Request) error {
	if req.Method == "GET" || req.Method == "HEAD" {
		return nil
	}
	if cl := req.Get("content-length"); cl != "" {
		n, err := parseLen(cl)
		if err != nil {
			return fmt.Errorf("request content-length %q: %w", cl, err)
		}
		if n == 0 {
			return nil
		}
		call.BodyReader = req.Body
		call.BodyLen = n
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(req.Body, maxBufferedBody+1))
	if err != nil {
		return fmt.Errorf("reading the request body: %w", err)
	}
	if len(buf) > maxBufferedBody {
		return fmt.Errorf("request body without content-length above the %d byte limit", maxBufferedBody)
	}
	if len(buf) > 0 {
		call.Body = buf
	}
	return nil
}

func parseLen(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(s[i]-'0')
		if n > 1<<40 {
			return 0, errors.New("too large")
		}
	}
	return n, nil
}
