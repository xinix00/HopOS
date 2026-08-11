package vitals

// Host-smoke: het pakket is bewust host-buildbaar, dus de server kan hier
// gewoon op een loopback-poort draaien. Dit test de bedrading (routing, de
// run-dispatcher, state-JSON), niet de meetwaarden — die zijn pas op ijzer
// interessant.

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

func TestSmoke(t *testing.T) {
	s := NewServer(Config{Version: "test", Arch: "host", Port: "0"})
	s.Start()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go leanhttp.Serve(l, s.Handle)
	base := "http://" + l.Addr().String()

	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := leanhttp.Do(leanhttp.Call{URL: base + path, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// De pagina en de state-API staan.
	if code, b := get("/"); code != 200 || !strings.Contains(string(b), "HopOS") {
		t.Fatalf("page: code %d", code)
	}
	var state struct {
		Running string             `json:"running"`
		Results map[string]*Result `json:"results"`
		Tests   []struct{ Name string }
	}
	if code, b := get("/api/state"); code != 200 {
		t.Fatalf("state: code %d", code)
	} else if err := json.Unmarshal(b, &state); err != nil {
		t.Fatalf("state: %v", err)
	} else if len(state.Tests) == 0 {
		t.Fatal("state: no tests listed")
	}

	// Eén echte run door de dispatcher heen; gc met 1s is de snelste.
	if code, b := get("/api/run?test=gc&secs=1"); code != 200 {
		t.Fatalf("run gc: code %d: %s", code, b)
	}
	// Een tweede run moet botsen zolang de eerste loopt.
	if code, _ := get("/api/run?test=timer"); code != 409 {
		t.Fatalf("second run: expected 409, got %d", code)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, b := get("/api/state")
		if err := json.Unmarshal(b, &state); err != nil {
			t.Fatal(err)
		}
		if state.Running == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gc test did not finish in 15s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	res := state.Results["gc"]
	if res == nil || res.Err != "" || len(res.Metrics) == 0 {
		t.Fatalf("gc result: %+v", res)
	}

	// Onbekende test = 404.
	if code, _ := get("/api/run?test=nope"); code != 404 {
		t.Fatalf("unknown test: expected 404, got %d", code)
	}

	// /blob levert exact de beloofde bytes en laat een tx-resultaat achter.
	if code, b := get("/blob?mb=1"); code != 200 || len(b) != 1<<20 {
		t.Fatalf("blob: code %d, %d bytes", code, len(b))
	}
	if _, b := get("/api/state"); !strings.Contains(string(b), "\"tx\"") {
		t.Fatal("blob left no tx result")
	}
}
