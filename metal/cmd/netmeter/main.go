// netmeter is de meetbank voor de netstack van de kern (QEMU virt). Tot de
// flip van 09-08 was dit een A/B-bank (gVisor vs lneto, twee builds); de
// A/B-cijfers die de flip onderbouwden staan in het geheugen en de
// gvisor-kant staat in git history. Wat blijft is de bank zelf: RX/TX-
// plafonds, allocaties per fase, en de storm-workloads — ook bruikbaar op
// ijzer (de LicheeRV-RX-jacht).
//
// Wat QEMU hier wél zuiver meet: het pakket-pad (CPU-plafond, allocaties,
// GC-druk) en de correctheid (SHA256 van elke transfer). Wat QEMU NIET kan
// meten: WAN-venstergedrag — slirp termineert TCP op de host, dus de
// guest-RTT is altijd microseconden. Die kanttekening hoort bij elk getal
// uit deze bank.
//
// Bouwen (handmatig, buiten de gate):
//   -tags "linkcpuinit nodefaultstack"
//
// Fasen (markers op de console, Engels zoals alle console-output):
//   pull-local   GET http://10.0.2.2:8099/blob64  — RX-plafond, slirp-lokaal
//   storm-keep   300 GETs /small over één verbinding — request-pad/allocaties
//   storm-conn   100 GETs /small, verbinding per request — SYN/accept-churn
//   pull-github  echte HTTPS-download (TLS + redirect naar CDN) — correctheid
//   serve        32MiB op :80; de host trekt en klokt (hostfwd 28080)
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"time"

	_ "unsafe"

	// TLS-wortels: zonde deze bundel faalt elke https-fetch (geen OS, geen
	// system-CA-store) — zelfde regel als in cmd/hopos.
	_ "golang.org/x/crypto/x509roots/fallback"

	gnet "github.com/usbarmory/go-net"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	_ "github.com/xinix00/HopOS/metal/board/qemuvirt/hop" // registreert het board (init) + runtime-hooks
	"github.com/xinix00/HopOS/metal/cpu/memlimit"
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize

const (
	hostBase  = "http://10.0.2.2:8099" // slirp-alias van de Mac
	githubURL = "https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/display.elf"
	serveSize = 32 << 20
)

// newStack: de stack-maten van de bank — groot venster (wscale) zoals de
// kern zelf (net/hopnet).
func newStack() gnet.Stack {
	cfg := gnet.DefaultLnetoStackConfig()
	cfg.TCPBufferSize = 256 << 10
	cfg.TCPQueueSize = 64
	cfg.MaxActiveTCPPorts = 64
	cfg.MaxListenerConns = 16
	return gnet.NewLnetoStack(cfg)
}

func main() {
	memlimit.Arm()
	fmt.Printf("NETMETER lneto %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	nic, hw, err := board.Current().ProbeNIC()

	if err != nil || nic == nil {
		fmt.Printf("NETMETER_FAIL nic: %v\n", err)
		park()
	}
	nc := board.Current().Net()

	iface := &gnet.Interface{NetworkDevice: nic, Stack: newStack()}
	if err := iface.Init(nc.CIDR, hw.String(), nc.GW); err != nil {
		fmt.Printf("NETMETER_FAIL netstack init: %v\n", err)
		park()
	}
	iface.HandleStackErr = func(err error, tx bool) {
		fmt.Printf("netstack (tx=%v): %v\n", tx, err)
	}

	net.SetDefaultNS([]string{nc.DNS})
	net.SocketFunc = iface.Stack.Socket

	// RX-lus in de hopnet-vorm: pollen met microslaap.
	go func() {
		buf := make([]byte, gnet.MTU+gnet.EthernetMaximumSize)
		for {
			n, err := nic.Receive(buf)
			if n == 0 || err != nil {
				time.Sleep(300 * time.Microsecond)
				continue
			}
			if err := iface.Stack.RecvInboundPacket(buf[:n]); err != nil && iface.HandleStackErr != nil {
				iface.HandleStackErr(err, false)
			}
		}
	}()
	fmt.Printf("NETMETER net up: %s (gw %s)\n", nc.IP, nc.GW)

	// Wandtijd uit de Date-header van de host: zonder klok faalt elke
	// TLS-validatie (epoch 0 < notBefore). Seconde-grof is zat voor x509.
	if resp, err := http.Get(hostBase + "/small"); err == nil {
		if t, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
			board.Current().SetWallTime(t.UnixNano())
			fmt.Printf("NETMETER clock set: %s\n", t.Format(time.RFC3339))
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	phase("pull-local", func() (int64, string, error) { return pull(hostBase+"/blob64", 180*time.Second) })
	phase("storm-keep", func() (int64, string, error) { return storm(300, true) })
	phase("storm-conn", func() (int64, string, error) { return storm(400, false) })
	phase("pull-github", func() (int64, string, error) { return pull(githubURL, 300*time.Second) })
	serve()
}

// park houdt de bank in leven na een fatale fout, zodat de console leesbaar
// blijft (zelfde reden als cmd/hopos: geen shell om op terug te vallen).
func park() {
	for {
		time.Sleep(time.Hour)
	}
}

// snap is de memstats-foto vóór een fase; het verschil erna is de prijs van
// die fase in allocaties/GC — dát is het "lean"-getal naast de MB/s.
type snap struct {
	alloc, mallocs, pause uint64
	gc                    uint32
}

func take() snap {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return snap{alloc: ms.TotalAlloc, mallocs: ms.Mallocs, pause: ms.PauseTotalNs, gc: ms.NumGC}
}

func phase(name string, f func() (int64, string, error)) {
	before := take()
	t0 := time.Now()
	n, sha, err := f()
	dur := time.Since(t0)
	after := take()
	if err != nil {
		fmt.Printf("NETMETER_FAIL phase=%s after=%v err=%v\n", name, dur, err)
		return
	}
	mb := float64(n) / (1 << 20)
	fmt.Printf("NETMETER phase=%s bytes=%d ms=%d MBps=%.2f sha=%s allocMB=%.1f mallocs=%d gc=%d pauseMs=%.1f goroutines=%d\n",
		name, n, dur.Milliseconds(), mb/dur.Seconds(), sha,
		float64(after.alloc-before.alloc)/(1<<20), after.mallocs-before.mallocs,
		after.gc-before.gc, float64(after.pause-before.pause)/1e6, runtime.NumGoroutine())
}

// pull haalt url binnen en hasht onderweg: het MB/s-getal en het bewijs van
// byte-correctheid komen uit dezelfde lus.
func pull(url string, timeout time.Duration) (int64, string, error) {
	cl := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, "", fmt.Errorf("status %s", resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return n, "", err
	}
	return n, fmt.Sprintf("%x", h.Sum(nil))[:16], nil
}

// storm doet n kleine GETs; keepalive=false forceert een verse verbinding per
// request (SYN/accept/FIN-churn — poort-recycling is hier deel van de meting).
// Faalt een request, dan is de vervolgvraag nét zo belangrijk als het
// faalpunt: herstelt de stack als de churn stopt? De probe wacht 30s en doet
// er dan nog vijf — "failed at N, recovered" vs "wedged" scheelt een categorie.
func storm(n int, keepalive bool) (int64, string, error) {
	tr := &http.Transport{DisableKeepAlives: !keepalive}
	cl := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	var total int64
	one := func(i int) error {
		resp, err := cl.Get(hostBase + "/small")
		if err != nil {
			return fmt.Errorf("request %d: %w", i, err)
		}
		m, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("request %d body: %w", i, err)
		}
		total += m
		return nil
	}
	for i := 0; i < n; i++ {
		if err := one(i); err != nil {
			fmt.Printf("NETMETER storm stalled: %v — probing recovery in 30s\n", err)
			time.Sleep(30 * time.Second)
			for j := 0; j < 5; j++ {
				if perr := one(n + j); perr != nil {
					return total, "", fmt.Errorf("failed at %d, NOT recovered: %w", i, perr)
				}
			}
			return total, fmt.Sprintf("failed at %d, recovered after 30s", i), nil
		}
	}
	return total, fmt.Sprintf("%d reqs", n), nil
}

// serve zet 32MiB deterministische bytes op :80 en blijft staan; de host doet
// de klok (curl via hostfwd) — de TX-kant van de stack, gemeten van buitenaf.
func serve() {
	buf := make([]byte, serveSize)
	x := uint64(0x48504f53) // "HPOS" — vaste seed: elke build serveert dezelfde bytes
	for i := range buf {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		buf[i] = byte(x)
	}
	sum := sha256.Sum256(buf)
	fmt.Printf("NETMETER serve sha=%x bytes=%d\n", sum[:8], len(buf))

	http.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(buf)))
		w.Write(buf)
	})
	fmt.Println("NETMETER_SERVE_READY :80")
	if err := http.ListenAndServe(":80", nil); err != nil {
		fmt.Printf("NETMETER_FAIL serve: %v\n", err)
	}
	park()
}
