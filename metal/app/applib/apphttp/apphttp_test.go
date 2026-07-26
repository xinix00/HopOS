// Host-tests voor de handgerolde HTTP-parser. Dit pad leest netwerkdata van een
// server die wij niet schreven, dus het is precies het soort code dat tests
// verdient: elk faalpad moet luid falen i.p.v. een half antwoord door te geven.
package apphttp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lees haalt de hele body op en sluit hem.
func lees(t *testing.T, r *Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("body lezen: %v", err)
	}
	return b
}

func TestGewoneGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("methode %q, wil GET", r.Method)
		}
		if r.Host == "" {
			t.Error("geen Host-header meegestuurd")
		}
		w.Write([]byte("hallo"))
	}))
	defer srv.Close()

	resp, err := Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Length != 5 {
		t.Fatalf("Length %d, wil 5", resp.Length)
	}
	if got := lees(t, resp); string(got) != "hallo" {
		t.Fatalf("body %q, wil %q", got, "hallo")
	}
}

// De regressie die ik bijna shipte: de header-begrenzing mag de BODY niet
// afknijpen. Een image is megabytes groot, dus een payload ruim boven bufSize
// (8KB) én boven maxHeaderBytes (64KB) moet compleet doorkomen.
func TestGroteBodyWordtNietAfgekapt(t *testing.T) {
	want := bytes.Repeat([]byte("0123456789abcdef"), 20_000) // 320KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Expliciet, anders schakelt Go's server op deze omvang naar chunked —
		// en dat weigert deze client bewust (zie TestChunkedWeigert).
		w.Header().Set("Content-Length", fmt.Sprint(len(want)))
		w.Write(want)
	}))
	defer srv.Close()

	resp, err := Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Length != int64(len(want)) {
		t.Fatalf("Length %d, wil %d", resp.Length, len(want))
	}
	if got := lees(t, resp); !bytes.Equal(got, want) {
		t.Fatalf("body %d bytes, wil %d — afgekapt", len(got), len(want))
	}
}

func TestRedirectWordtGevolgd(t *testing.T) {
	var doel *httptest.Server
	doel = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/een":
			http.Redirect(w, r, doel.URL+"/twee", http.StatusFound) // absoluut
		case "/twee":
			http.Redirect(w, r, "/drie", http.StatusMovedPermanently) // relatief
		case "/drie":
			w.Write([]byte("aangekomen"))
		default:
			t.Errorf("onverwacht pad %q", r.URL.Path)
		}
	}))
	defer doel.Close()

	resp, err := Get(doel.URL + "/een")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := lees(t, resp); string(got) != "aangekomen" {
		t.Fatalf("body %q, wil %q", got, "aangekomen")
	}
}

func TestRedirectLusStopt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/rond", http.StatusFound)
	}))
	defer srv.Close()

	if _, err := Get(srv.URL); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("err = %v, wil een too-many-redirects-fout", err)
	}
}

// De kern van het pakket: https moet luid weigeren, want er is geen TLS
// gelinkt. Stil doorgaan zou een onbegrijpelijke verbindingsfout geven.
func TestHTTPSWeigertLuid(t *testing.T) {
	_, err := Get("https://example.com/app.elf")
	if err == nil {
		t.Fatal("https werd geaccepteerd")
	}
	if !strings.Contains(err.Error(), "only http://") || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("err = %v — de melding moet http-only én TLS noemen", err)
	}
}

func TestChunkedWeigert(t *testing.T) {
	// Handmatig antwoorden: httptest's ResponseWriter zet zelf Content-Length
	// op kleine bodies, dus chunked moet op de draad geforceerd worden.
	srv := rauweServer(t, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhallo\r\n0\r\n\r\n")
	if _, err := Get(srv); err == nil || !strings.Contains(err.Error(), "chunked") {
		t.Fatalf("err = %v, wil een chunked-fout", err)
	}
}

func TestZonderContentLengthWeigert(t *testing.T) {
	srv := rauweServer(t, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\nhallo")
	if _, err := Get(srv); err == nil || !strings.Contains(err.Error(), "no Content-Length") {
		t.Fatalf("err = %v, wil een ontbrekende-lengte-fout", err)
	}
}

// Twee verschillende lengtes is een smokkel-signaal, geen laatste-wint-geval.
func TestDubbeleContentLengthWeigert(t *testing.T) {
	srv := rauweServer(t, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 9\r\n\r\nhallo")
	if _, err := Get(srv); err == nil || !strings.Contains(err.Error(), "duplicate Content-Length") {
		t.Fatalf("err = %v, wil een dubbele-lengte-fout", err)
	}
}

func TestFoutstatusGeeftFout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "weg", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := Get(srv.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, wil een 404-fout", err)
	}
}

func TestKromgeStatusregelWeigert(t *testing.T) {
	srv := rauweServer(t, "ik ben geen http\r\n\r\n")
	if _, err := Get(srv); err == nil || !strings.Contains(err.Error(), "malformed status line") {
		t.Fatalf("err = %v, wil een malformed-status-fout", err)
	}
}

// Eén absurd lange headerregel mag geen ongebonden buffergroei geven.
func TestTeLangeHeaderregelWeigert(t *testing.T) {
	srv := rauweServer(t, "HTTP/1.1 200 OK\r\nX-Groot: "+strings.Repeat("A", bufSize+1)+"\r\nContent-Length: 1\r\n\r\nx")
	if _, err := Get(srv); err == nil || !strings.Contains(err.Error(), "header line exceeds") {
		t.Fatalf("err = %v, wil een te-lange-regel-fout", err)
	}
}

// Veel kleine regels passen elk in de buffer maar mogen samen niet ongebonden
// groeien — de cumulatieve grens (maxHeaderBytes).
func TestTeVeelHeaderbytesWeigert(t *testing.T) {
	var b strings.Builder
	b.WriteString("HTTP/1.1 200 OK\r\n")
	for i := 0; b.Len() < maxHeaderBytes+1024; i++ {
		fmt.Fprintf(&b, "X-Vul-%d: %s\r\n", i, strings.Repeat("v", 200))
	}
	b.WriteString("Content-Length: 1\r\n\r\nx")
	if _, err := Get(rauweServer(t, b.String())); err == nil || !strings.Contains(err.Error(), "headers exceed") {
		t.Fatalf("err = %v, wil een headers-te-groot-fout", err)
	}
}

func TestGeenHostWeigert(t *testing.T) {
	if _, err := Get("http:///app.elf"); err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("err = %v, wil een geen-host-fout", err)
	}
}

// rauweServer antwoordt op elke verbinding met exact deze bytes en geeft zijn
// URL — nodig waar net/http's server het antwoord te netjes zou maken.
func rauweServer(t *testing.T, antwoord string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				// Het verzoek eerst wegslikken tot de lege regel, zodat de
				// client zijn write kwijt kan.
				buf := make([]byte, 4096)
				c.Read(buf)
				io.WriteString(c, antwoord)
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}
