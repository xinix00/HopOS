// Package apphttp is een minimale HTTP/1.1-GET-client voor apps die alleen
// plain HTTP praten: een LAN-fileserver lezen, een buur-app op het interne net
// (10.100.0.0/24), of de node-API op HOPOS_HOST:8080.
//
// Bewust een apart pakket naast applib en appnet, om dezelfde reden als appnet:
// alleen wie het importeert betaalt ervoor. En hier is die rekening groot.
// `net/http` linkt onvoorwaardelijk crypto/tls mee, en dat is in een app-image
// duurder dan de hele netstack (gemeten 26-07 op app/hello):
//
//	applib alleen ............ 1,71 MB
//	+ appnet (gVisor) ........ 4,70 MB
//	+ net/http ............... 7,99 MB   ← meer dan de netstack zelf
//
// Van wat net/http toevoegt is ~54% TLS/PKI (crypto/tls + x/crypto + x509 +
// math/big + asn1), geen HTTP. Een app die dit pakket i.p.v. net/http gebruikt
// houdt dus ~2,8MB over — puur door niet te linken wat hij niet nodig heeft.
//
// NIET voor de apploader: die haalt artifacts óók van https-URL's (GitHub-
// release-assets, `hop apply https://…/app.elf`) en heeft daarvoor de
// x509-rootbundel aan boord — gemeten 20-07. Die blijft op net/http. Dit pakket
// is voor apps die zélf weten dat hun verkeer plain http is.
//
// Wat dit NIET doet, en waarom dat mag:
//
//   - geen https. Een https-URL faalt LUID met een duidelijke melding — nooit
//     stil, want dit pakket bestaat juist om geen TLS te linken.
//   - geen chunked transfer. Content-Length is verplicht (StageImage en elke
//     andere bekende afnemer eisen de lengte toch al vooraf).
//   - geen keep-alive, geen connection pool: één GET per verbinding
//     (Connection: close).
//   - geen read-deadline op de body: een trage server mag een groot bestand
//     langzaam leveren. Een hángende server is niet fataal — applib's kill-flag
//     loopt op een eigen goroutine en blijft het vangnet.
//
// Redirects worden wél gevolgd (bounded): dat is het enige dat je anders t.o.v.
// net/http zou inleveren, en het is vijftien regels.
//
// De headerparser leest netwerkdata van een server die wij niet schreven, dus
// hij is dubbel begrensd: per regel (readLine, via de bufio-buffer) én
// cumulatief (maxHeaderBytes). De body erna staat vrij — die lengte kondigt
// Content-Length aan en StageImage toetst hem.
//
// Vereist een opgebrachte netstack (appnet.Up): dit gaat via net.Dial.
package apphttp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// dialTimeout begrenst het opzetten van de verbinding: een onbereikbare
	// host mag geen job-start gijzelen. De download zelf staat bewust vrij.
	dialTimeout = 10 * time.Second

	// maxRedirects volgt hetzelfde plafond als net/http's default.
	maxRedirects = 10

	// bufSize is tevens de maximale lengte van één headerregel: readLine leest
	// via ReadSlice, dus een regel die niet in de buffer past is een fout i.p.v.
	// ongebonden geheugengroei. Ruim voor elke echte header.
	bufSize = 8 << 10

	// maxHeaderBytes is de cumulatieve grens: veel kleine regels passen elk
	// binnen bufSize maar mogen samen niet ongebonden groeien.
	maxHeaderBytes = 64 << 10
)

// Response is één geslaagde GET. Body is de ongelezen responsebody; sluit hem
// (dat sluit de verbinding). Length is de aangekondigde Content-Length.
type Response struct {
	Body   io.ReadCloser
	Length int64
}

// Get doet één HTTP/1.1 GET en geeft de body als stream plus zijn lengte.
// Volgt redirects (3xx met Location) tot maxRedirects.
func Get(raw string) (*Response, error) {
	loc := raw
	for range maxRedirects + 1 {
		resp, next, err := get(loc)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return resp, nil
		}
		// Redirect: de body van het 3xx-antwoord interesseert ons niet, maar de
		// verbinding moet dicht vóór we de volgende opzetten (Connection: close
		// belooft de server hetzelfde, maar wij houden onze fd's in eigen hand).
		resp.Body.Close()
		base, err := url.Parse(loc)
		if err != nil {
			return nil, fmt.Errorf("apphttp: bad URL %q: %w", loc, err)
		}
		ref, err := url.Parse(next)
		if err != nil {
			return nil, fmt.Errorf("apphttp: bad Location %q: %w", next, err)
		}
		loc = base.ResolveReference(ref).String() // een relatieve Location mag
	}
	return nil, fmt.Errorf("apphttp: too many redirects (>%d) starting at %s", maxRedirects, raw)
}

// get doet één ronde. De tweede returnwaarde is de Location van een 3xx (leeg
// bij een gewoon antwoord): dán moet de aanroeper de body sluiten en doorgaan.
func get(raw string) (_ *Response, location string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("apphttp: bad URL %q: %w", raw, err)
	}
	// Luid, niet stil: dit pakket bestaat juist om TLS niet te linken, dus een
	// https-URL is een configuratiefout die je op de console hoort te zien.
	if u.Scheme != "http" {
		return nil, "", fmt.Errorf("apphttp: only http:// is supported, got %q — "+
			"this app links no TLS (use a plain-http artifact URL on the LAN, or build the app with net/http)", u.Scheme)
	}
	if u.Host == "" {
		return nil, "", fmt.Errorf("apphttp: URL %q has no host", raw)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(addr, "80")
	}

	conn, err := net.DialTimeout("tcp4", addr, dialTimeout)
	if err != nil {
		return nil, "", fmt.Errorf("apphttp: dial %s: %w", addr, err)
	}
	// Elk faalpad hierna sluit de verbinding; het succespad geeft hem als Body
	// aan de aanroeper mee.
	handedOff := false
	defer func() {
		if !handedOff {
			conn.Close()
		}
	}()

	// Accept-Encoding: identity — wij pakken niets uit, dus vraag het ook niet.
	// Host zonder poort-default, net als net/http.
	if _, err := fmt.Fprintf(conn,
		"GET %s HTTP/1.1\r\nHost: %s\r\nAccept-Encoding: identity\r\nConnection: close\r\n\r\n",
		u.RequestURI(), u.Host); err != nil {
		return nil, "", fmt.Errorf("apphttp: write request: %w", err)
	}

	br := bufio.NewReaderSize(conn, bufSize)
	budget := maxHeaderBytes

	status, err := readLine(br, &budget)
	if err != nil {
		return nil, "", fmt.Errorf("apphttp: read status line: %w", err)
	}
	code, err := statusCode(status)
	if err != nil {
		return nil, "", err
	}

	var length int64 = -1
	var loc string
	var encoded bool
	for {
		line, err := readLine(br, &budget)
		if err != nil {
			return nil, "", fmt.Errorf("apphttp: read headers: %w", err)
		}
		if line == "" {
			break // lege regel: einde headers
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			return nil, "", fmt.Errorf("apphttp: malformed header %q", line)
		}
		v = strings.TrimSpace(v)
		switch {
		case strings.EqualFold(k, "Content-Length"):
			// Een tweede, andere lengte is een smokkel-signaal, geen
			// laatste-wint-geval: falen.
			if length >= 0 {
				return nil, "", fmt.Errorf("apphttp: duplicate Content-Length")
			}
			if length, err = strconv.ParseInt(v, 10, 64); err != nil || length < 0 {
				return nil, "", fmt.Errorf("apphttp: bad Content-Length %q", v)
			}
		case strings.EqualFold(k, "Location"):
			loc = v
		case strings.EqualFold(k, "Transfer-Encoding"):
			encoded = !strings.EqualFold(v, "identity")
		}
	}

	if code >= 300 && code < 400 {
		if loc == "" {
			return nil, "", fmt.Errorf("apphttp: HTTP %d without Location", code)
		}
		handedOff = true // de aanroeper sluit deze body vóór de volgende ronde
		return &Response{Body: body{br, conn}, Length: length}, loc, nil
	}
	if code != 200 {
		return nil, "", fmt.Errorf("apphttp: HTTP %s", status)
	}
	// Chunked kan dit pakket niet, en een image zónder lengte kon de apploader
	// nooit stagen (hij weigerde ContentLength <= 0). Dus: luid falen.
	if encoded {
		return nil, "", fmt.Errorf("apphttp: chunked/encoded transfer is not supported — serve the artifact with a Content-Length")
	}
	if length < 0 {
		return nil, "", fmt.Errorf("apphttp: no Content-Length in response")
	}
	handedOff = true
	return &Response{Body: body{br, conn}, Length: length}, "", nil
}

// readLine leest één CRLF-regel en trekt hem van het budget af. ReadSlice
// begrenst de regel op de buffergrootte (ErrBufferFull = te lang) i.p.v.
// ongebonden te groeien zoals ReadString zou doen.
func readLine(br *bufio.Reader, budget *int) (string, error) {
	raw, err := br.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return "", fmt.Errorf("header line exceeds %d bytes", bufSize)
	}
	if err != nil {
		return "", err
	}
	if *budget -= len(raw); *budget < 0 {
		return "", fmt.Errorf("headers exceed %d bytes", maxHeaderBytes)
	}
	// raw wijst in de bufio-buffer en is na de volgende read ongeldig — de
	// string-conversie hieronder kopieert, dus dat is hier afgehandeld.
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// statusCode pelt de code uit "HTTP/1.1 200 OK".
func statusCode(line string) (int, error) {
	proto, rest, found := strings.Cut(line, " ")
	if !found || !strings.HasPrefix(proto, "HTTP/") {
		return 0, fmt.Errorf("apphttp: malformed status line %q", line)
	}
	num, _, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(num)
	if err != nil || code < 100 || code > 599 {
		return 0, fmt.Errorf("apphttp: malformed status line %q", line)
	}
	return code, nil
}

// body koppelt de gebufferde reader aan de verbinding, zodat Close beide
// afsluit: de bufio kan al body-bytes vooruit gelezen hebben, dus de body moet
// dóór die reader gelezen worden en niet rechtstreeks van de conn.
type body struct {
	r io.Reader
	c net.Conn
}

func (b body) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b body) Close() error               { return b.c.Close() }
