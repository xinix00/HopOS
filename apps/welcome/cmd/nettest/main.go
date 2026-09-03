//go:build tamago

// nettest is een bench-instrument, geen release-app: hij bewijst de twee
// richtingen van het slot-netpad los van elkaar, zonder TLS of klok.
//
//   - UITGAAND: elke 5s een kale http-GET naar NETTEST_URL (default: de
//     bench-httpd op de gateway van het Mac-deelnet). Slaagt die, dan werkt
//     slot-TX → switch → NAT-masquerade → uplink én de terugweg de RX-ring in.
//   - INKOMEND: dezelfde luisteraar-vorm als welcome op de gepubliceerde
//     poort; antwoordt hij van buiten, dan werkt publish/DNAT → RX-ring → app.
//
// Ontstaan 17-08 (boot 9, LicheeRV): welcome draaide gezond op de geadopteerde
// grote core (logs, telemetrie, kill — de hopabi-ring dus), maar :80 bleef
// doof. welcome doet zelf nooit uitgaand verkeer, dus of de nétringen werkten
// was daarmee onbewezen; dit instrument splitst dat in twee losse metingen.
//
// Bouwen: als elke riscv64-app (tools/apps-release.sh):
//
//	cd apps/welcome && GOWORK=off GOTOOLCHAIN=local GOOS=tamago \
//	  GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 $HOME/tamago-go/bin/go \
//	  build -tags "linkramsize linkcpuinit" -trimpath \
//	  -ldflags "-w -T 0x88010000 -R 0x1000" -o nettest-riscv64-tamago.elf ./cmd/nettest
package main

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/v2/app/applib"
	"github.com/xinix00/HopOS/metal/v2/app/applib/appnet"
	"github.com/xinix00/lean/leanhttp"
)

func main() {
	app := applib.Init()

	ip, err := appnet.Up(app)
	if err != nil {
		app.Logf("nettest: net: %v", err)
		app.Exit(1)
	}
	app.Logf("nettest: up as %s (slot %d)", ip, app.Slot)

	// De netstack-tellers elke 5s: beweegt RX bij een curl van buiten, dan
	// bereikt de SYN de app-stack; beweegt er niets, dan sterft hij node-zijdig.
	go appnet.WatchStats(app.Logf, 5*time.Second)

	// mDNS-teller: het LAN vloeit continu multicast de RX-ring in
	// (multicastInbound floodt naar elk slot). Tikt deze teller dóór terwijl
	// de dials al dood zijn, dan leeft het RX-pad (rxLoop → deliverLocked →
	// app-ring) bewijsbaar en is de dood geïsoleerd tot de TX-drain.
	go func() {
		if err := appnet.JoinMulticast(net.IPv4(224, 0, 0, 251)); err != nil {
			app.Logf("nettest: mdns join: %v", err)
			return
		}
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 5353})
		if err != nil {
			app.Logf("nettest: mdns listen: %v", err)
			return
		}
		buf := make([]byte, 2048)
		total, lastN := 0, 0
		go func() {
			for {
				time.Sleep(5 * time.Second)
				app.Logf("nettest: mdns rx +%d (total %d)", total-lastN, total)
				lastN = total
			}
		}()
		for {
			if _, _, err := c.ReadFromUDP(buf); err != nil {
				app.Logf("nettest: mdns read: %v", err)
				return
			}
			total++
		}
	}()

	target := app.Env("NETTEST_URL")
	if target == "" {
		target = "http://192.168.99.1:8000/ping"
	}

	// Twee sondes om en om: extern (uplink + NAT) en de gateway (drainGateway,
	// zelfde switch-lus maar geen uplink). Wélke van de twee sterft zegt in
	// welke helft van het switch-pad de dood zit.
	gw := "http://10.100.0.1:8080/health"
	go func() {
		for i := 1; ; i++ {
			for _, url := range []string{target, gw} {
				resp, err := leanhttp.Get(url)
				if err != nil {
					app.Logf("nettest: GET #%d %s: %v", i, url, err)
				} else {
					n, _ := io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					app.Logf("nettest: GET #%d %s: %s, %d bytes", i, url, resp.Status, n)
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()

	port := app.Env("ER_PORT_HTTP")
	if port == "" {
		port = "80"
	}
	app.Logf("nettest: listening on :%s", port)
	err = leanhttp.ListenAndServe(":"+port, func(w leanhttp.ResponseWriter, r *leanhttp.Request) {
		app.Logf("nettest: served %s %s", r.Method, r.Path)
		fmt.Fprintf(w, "nettest alive, ip %s, %s\n", ip, time.Now().UTC().Format(time.RFC3339))
	})
	app.Logf("nettest: http server: %v", err)
	app.Exit(1)
}
