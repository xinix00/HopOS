// De referentie-app voor fase 1: een eigen Go-runtime die HOP-OS in een
// slot laadt en op een eigen core start. Via applib meldt hij zich READY,
// stuurt heartbeats en gehoorzaamt de kill-flag. Canoniek gelinkt
// (TEXT_START = SlotBase(1)+0x10000, zie image/qemu-run.sh) — de stage-2-map
// legt hem op de partitie van elk slot; de RAM-declaratie wordt door HopOS
// bij het laden gepatcht (job.MemoryLimit).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/abi/checksum"
	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"
	"github.com/xinix00/HopOS/metal/board/appboard"
)

func main() {
	app := applib.Init()

	// Loggen loopt via de hop-ABI-ring naar de HOP-kern — niet rechtstreeks
	// naar de UART, zodat output van alle slots netjes gemultiplext wordt.
	app.Logf("runtime up (%s), RAM %d MB @ %#x, clock=%s, BUCKET=%q ROLE=%q",
		runtime.Version(), app.RAMSize>>20, app.RAMStart,
		time.Now().UTC().Format("2006-01-02T15:04:05Z"), app.Env("BUCKET"), app.Env("ROLE"))

	// SMP (fase 5): één app, meerdere cores, gedeelde heap. De app deed hier
	// niets bijzonders — applib gaf hem GOMAXPROCS=N "as is". Deze rol bewijst
	// dat het echt parallel is.
	if app.Env("SMP") == "bench" {
		smpBench(app)
	}

	// Crash-rol (verbrande-core-test 20-07): een échte Go-panic. Zonder de
	// goos.Exit-hook eindigde die in tamago's DAIFSet+WFI — een lijk dat
	// zelfs de stage-2-revoke niet meer voelt; de core was weg tot de
	// powercycle. Mét de hook parkeert hij netjes (exitcode 2) en moet een
	// volgende plaatsing op dezelfde core gewoon slagen — dát is de proef.
	if app.Env("PANIC") != "" {
		time.Sleep(2 * time.Second) // eerst READY+logs laten landen
		panic("appspike: PANIC-rol — bewuste crash voor de park-proef")
	}

	// Soak-rol (P2b, docs/archief/plan-p2b-soak.md): permanent CPU branden + heap
	// churnen op alle cores, met een telemetrieregel per minuut. De
	// heartbeat loopt vanzelf (applib), kill werkt gewoon — dit is de
	// "zware taak" voor de 24-uurs-soak; hij triggert continu de
	// dvfs-druk-flank.
	if app.Env("BURN") != "" {
		burn(app)
	}

	// Isolatietest: grijp bewust buiten de eigen kooi. Onder stage-2 hoort
	// de load te faulten → EL2-vector → CPU_OFF; de tweede logregel mag
	// nooit verschijnen.
	if app.Env("PROBE") == "hop" {
		app.Logf("PROBE: lees HOP-geheugen @ %#x — de MMU-kooi hoort dit te stoppen", uint64(layout.HopRAMStart))
		time.Sleep(100 * time.Millisecond) // logregel eerst de ring uit
		v := *(*uint64)(unsafe.Pointer(uintptr(layout.HopRAMStart)))
		app.Logf("PROBE: gelekt: %#x — GEEN isolatie!", v)
	}

	// Isolatietest 2: praat bewust met de firmware — arch-eigen, dus achter een
	// naad (smp_arm64.go / smp_riscv64.go).
	if app.Env("PROBE") == "smc" {
		firmwareProbe(app)
	}

	// Object-store-demo (de persistente laag naast hopfs): zie store_demo.go.
	if app.Env("STOREDEMO") == "roundtrip" {
		storeDemo(app) // keert niet terug (exit)
	}

	// Volumes-demo (het storage-model van het plan): elke rol bewijst een
	// stuk van de keten. Exitcodes dragen het resultaat naar HOP.
	switch app.Env("FSDEMO") {
	case "writer":
		// Schrijf de gedeelde dataset in het gemounte /data, en een privé-
		// bestand in de eigen root (die geen andere task ooit ziet).
		data := make([]byte, 100<<10)
		for i := range data {
			data[i] = byte(i*13 + 7)
		}
		if err := app.WriteFile("/data/db.bin", data); err != nil {
			exitf(app, 1, "FSDEMO writer: %v", err)
		}
		if err := app.WriteFile("/prive.txt", []byte("alleen van slot-eigenaar")); err != nil {
			exitf(app, 1, "FSDEMO writer: prive: %v", err)
		}
		exitf(app, 0, "FSDEMO writer: /data/db.bin (%d bytes) + eigen /prive.txt geschreven", len(data))

	case "reader":
		// Lees de gedeelde dataset en exit met de checksum; bewijs en passant
		// dat andermans privé-bestand en een '..'-escape onzichtbaar zijn.
		b, err := app.ReadFile("/data/db.bin")
		if err != nil {
			exitf(app, 1, "FSDEMO reader: %v", err)
		}
		if _, err := app.ReadFile("/prive.txt"); err == nil {
			exitf(app, 2, "FSDEMO reader: LEK — andermans prive-bestand zichtbaar")
		}
		if _, err := app.ReadFile("/../.tasks/slot1/prive.txt"); err == nil {
			exitf(app, 3, "FSDEMO reader: LEK — '..'-escape werkt")
		}
		sum := checksum.FNV64(b)
		exitf(app, sum, "FSDEMO reader: %d bytes, checksum %#x", len(b), sum)

	case "denied":
		// Zonder mount bestaat /data voor deze task simpelweg niet.
		if _, err := app.ReadFile("/data/db.bin"); err == nil {
			exitf(app, 1, "FSDEMO denied: LEK — /data zichtbaar zonder mount")
		}
		exitf(app, 0, "FSDEMO denied: /data onzichtbaar zonder mount — goed")

	case "fetch":
		// Downloaden doet de APP, op zijn eigen core en met zijn eigen netstack —
		// HOP deed dit vroeger via een fetch-op in de ABI, en dat betekende dat de
		// vertrouwde kern op core 0 een door de app opgegeven URL opende. De bytes
		// gaan hier via de gewone write-weg het volume in; dat is precies wat een
		// echte job (de apploader!) ook doet.
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "FSDEMO fetch: %v", err)
		}
		body, err := httpGet(app.Env("FETCH_URL"))
		if err != nil {
			exitf(app, 1, "FSDEMO fetch: %v", err)
		}
		if err := app.WriteFile("/data/hello.txt", body); err != nil {
			exitf(app, 1, "FSDEMO fetch: schrijven: %v", err)
		}
		b, err := app.ReadFile("/data/hello.txt")
		if err != nil {
			exitf(app, 1, "FSDEMO fetch: teruglezen: %v", err)
		}
		exitf(app, 0, "FSDEMO fetch: %d bytes: %q", len(b), string(b[:min(len(b), 40)]))
	}

	// Netdemo (per-slot netwerk): elke rol draait een eigen netstack over de
	// frame-ringen; de switch bij HOP schuift alleen Ethernet-frames.
	switch app.Env("NETDEMO") {
	case "listen":
		// Echo-server: beantwoord elke regel met "pong <regel>". Serveert
		// tot HOP het slot stopt. Poort uit HOP's ER_PORT_*-conventie
		// (zelfde nummer als de gepubliceerde node-poort), default 8080.
		ip, err := appnet.Up(app)
		if err != nil {
			exitf(app, 1, "NETDEMO listen: %v", err)
		}
		port := app.Env("ER_PORT_HTTP")
		if port == "" {
			port = "8080"
		}
		l, err := net.Listen("tcp4", ":"+port)
		if err != nil {
			exitf(app, 1, "NETDEMO listen: %v", err)
		}
		app.Logf("NETDEMO listen: eigen stack op %s, poort :%s open", ip, port)
		for {
			conn, err := l.Accept()
			if err != nil {
				exitf(app, 1, "NETDEMO listen: accept: %v", err)
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				app.Logf("NETDEMO listen: %q van %s", line[:len(line)-1], c.RemoteAddr())
				c.Write([]byte("pong " + line))
			}(conn)
		}

	case "dial":
		// Client: ping naar NET_DIAL (een andere app), verifieer de pong.
		ip, err := appnet.Up(app)
		if err != nil {
			exitf(app, 1, "NETDEMO dial: %v", err)
		}
		conn, err := net.Dial("tcp4", app.Env("NET_DIAL"))
		if err != nil {
			exitf(app, 1, "NETDEMO dial: %v", err)
		}
		if _, err := conn.Write([]byte("ping van " + ip + "\n")); err != nil {
			exitf(app, 1, "NETDEMO dial: write: %v", err)
		}
		resp, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil || resp != "pong ping van "+ip+"\n" {
			exitf(app, 1, "NETDEMO dial: onverwacht antwoord %q (%v)", resp, err)
		}
		exitf(app, 0, "NETDEMO dial: %s → %s: pong ontvangen — app↔app zonder HOP-TCP", ip, app.Env("NET_DIAL"))

	case "node":
		// "Mijn node": de agent-API op het vaste interne adres (10.100.0.1 —
		// hetzelfde op élke node, Dereks besluit 20-07). Sinds de netstack-flip
		// (09-08) is dat adres geen tweede kern-NIC meer maar een statische
		// 1:1-vertaling op de gateway-naad (hopswitch.GwToHost/GwFromHost);
		// deze GET bewijst dat pad end-to-end: app-stack → switch → vertaling
		// → kern-stack → agent → en het antwoord helemaal terug.
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "NETDEMO node: %v", err)
		}
		body, err := httpGet("http://" + layout.IP4Str(layout.HostIP4()) + ":8080/health")
		if err != nil {
			exitf(app, 1, "NETDEMO node: %v", err)
		}
		// Succes = blijven staan (een job is een service): de taak blijft
		// "running" en dát is het leesbare bewijs; een exit zou als crash
		// tellen en herstarts triggeren.
		app.Logf("NETDEMO node: agent answered %q via the internal gateway address", string(body[:min(len(body), 40)]))
		for {
			time.Sleep(time.Hour)
		}

	case "out":
		// Uitgaand naar buiten: één DNS-query (UDP) naar de node-resolver die
		// HOP als HOP_DNS meegaf. HOP masquerade't de query (slot-IP:poort →
		// node-IP:node-poort) de externe NIC uit en het antwoord terug — een
		// respóns bewijst de hele round-trip, ongeacht wat erin staat. Dít is
		// het pad dat straks cloudflared/servers naar buiten gebruiken.
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "NETDEMO out: %v", err)
		}
		dns := app.Env("HOP_DNS")
		if dns == "" {
			exitf(app, 1, "NETDEMO out: geen HOP_DNS meegegeven")
		}
		conn, err := net.Dial("udp4", dns)
		if err != nil {
			exitf(app, 1, "NETDEMO out: dial %s: %v", dns, err)
		}
		defer conn.Close()
		// Minimale DNS A-query voor "a.root-servers.net" (id 0x1234, RD).
		query := []byte{
			0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x01, 'a', 0x0c, 'r', 'o', 'o', 't', '-', 's', 'e', 'r', 'v', 'e', 'r', 's',
			0x03, 'n', 'e', 't', 0x00, 0x00, 0x01, 0x00, 0x01,
		}
		if _, err := conn.Write(query); err != nil {
			exitf(app, 1, "NETDEMO out: write: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp := make([]byte, 512)
		n, err := conn.Read(resp)
		if err != nil || n < 12 || resp[0] != 0x12 || resp[1] != 0x34 {
			exitf(app, 1, "NETDEMO out: geen bruikbaar DNS-antwoord (n=%d, %v)", n, err)
		}
		exitf(app, 0, "NETDEMO out: DNS-antwoord van %s (%d bytes) — uitgaande masquerade werkt", dns, n)
	}

	// Hanger: een lege lus zonder preemptiepunt monopoliseert de core — de
	// heartbeat-goroutine komt nooit meer aan bod en de kill-flag wordt
	// genegeerd. Precies de hang waarvoor HOP's hard-kill-SGI bestaat.
	if app.Env("HANG") == "spin" {
		app.Logf("HANG: spin zonder preemptiepunt — alleen een hard-kill helpt nog")
		time.Sleep(100 * time.Millisecond) // logregel eerst de ring uit
		for {
		}
	}

	// Standaard-rol: een long-running service — de werklast voor de brede
	// SMP-test (N taken → N slots → N cores, elk hier). Elke ronde een korte
	// rekenburst gevolgd door een korte pauze: de pauze is het yield-punt
	// (heartbeat en kill-flag krijgen de core) én houdt een zwaar overboekte
	// QEMU bij. Af en toe — niet elke ronde, anders overstemmen 127 apps de
	// console — een levensteken met het slotnummer (in een slot is CoreID het
	// door HOP gepatchte slotHint), het rondetal (bewijst dat hij écht itereert)
	// en de uptime. Geen exit: een HOP-app is een service.
	start := time.Now()
	next := start.Add(12 * time.Second)
	var acc, rounds uint64
	for {
		for k := 0; k < 1<<18; k++ { // korte rekenburst (~honderden µs)
			acc = acc*6364136223846793005 + uint64(k)
		}
		smpSink = acc
		rounds++
		if now := time.Now(); now.After(next) {
			app.Logf("service alive: slot %d, %d rounds, up %s",
				appboard.Current().CoreID(), rounds, time.Since(start).Round(time.Second))
			next = now.Add(12 * time.Second)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// exit geeft de laatste logregel de tijd om de ring uit te komen en stopt dan.
func exit(app *applib.App, code uint64) {
	time.Sleep(100 * time.Millisecond)
	app.Exit(code)
}

// fatalf logt de fout en stopt met deze code — het vaste faalpaar van elke
// demo-stap (stond ~40× uitgeschreven als Logf+exit).
func exitf(app *applib.App, code uint64, format string, args ...any) {
	app.Logf(format, args...)
	exit(app, code)
}

// httpGet haalt één URL op over de EIGEN netstack van dit slot (appnet.Up moet
// gelopen hebben) en geeft de body. Begrensd, want dit is scratch-opslag en geen
// datalake — precies de grens die HOP's oude fetch-op ook had, nu op de plek waar
// de bytes daadwerkelijk landen.
func httpGet(url string) ([]byte, error) {
	const maxBody = 8 << 20
	if url == "" {
		return nil, errors.New("geen FETCH_URL meegegeven")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("GET %s: body > %dMB", url, maxBody>>20)
	}
	return b, nil
}

// smpSink houdt reken-resultaten levend zodat de compiler het werk niet weggooit.
var smpSink uint64

// burn is de soak-werklast (P2b): alle cores rekenen + heap-churn (GC-druk),
// maar in een RITME van 10 min werk / 5 min rust i.p.v. dauerlast (Derek,
// 2026-07-11). Zo test de soak de dvfs-TERUGKLOK op zijn ontwerp-premisse —
// een app die LEEFT maar idlet moet terugklokken — niet alleen het opklokken.
// Tijdens rust slapen de workers, de idle-governor tikt door → dvfs ziet
// idle → klokt naar de vloer. Keert nooit terug; HOP's kill/stop beëindigt.
func burn(app *applib.App) {
	n := runtime.GOMAXPROCS(0)
	const workSecs, restSecs = 600, 300
	app.Logf("BURN: soak load on %d core(s), RAM %d MB — %ds work / %ds rest cycle",
		n, app.RAMSize>>20, workSecs, restSecs)

	// ZELF-TIMEND: geen aparte controller-goroutine (die op een 1-core slot
	// door de rekenlus gestarfd kan worden). Elke worker leidt z'n fase af
	// uit de wandklok en rekent in korte bursts met een yield ertussen —
	// tijdens werk brandt de core, tijdens rust slaapt hij (core idle →
	// dvfs klokt terug). Zo hangt er niets van een niet-geschedulede
	// goroutine af. Gemeten 2026-07-12: dit was de betrouwbare vorm.
	const cycle = workSecs + restSecs
	var iters uint64
	for c := 0; c < n; c++ {
		go func() {
			var acc uint64
			for {
				if time.Now().Unix()%cycle >= workSecs { // rust-venster
					time.Sleep(200 * time.Millisecond)
					continue
				}
				for k := 0; k < 1<<19; k++ { // ~0,3ms rekenburst
					acc = acc*6364136223846793005 + uint64(k)
				}
				smpSink = acc
				atomic.AddUint64(&iters, 1)
				runtime.Gosched() // afgeven: telemetrie/heartbeat krijgen de core
			}
		}()
	}

	var ms runtime.MemStats
	inWork := true
	for {
		time.Sleep(10 * time.Second)
		nowWork := time.Now().Unix()%cycle < workSecs
		if nowWork != inWork { // fase-overgang loggen (dvfs-bewijs)
			inWork = nowWork
			if inWork {
				app.Logf("BURN: work phase — cores busy, dvfs should clock up")
			} else {
				app.Logf("BURN: rest phase — cores idle, dvfs should clock down")
			}
		}
		runtime.ReadMemStats(&ms)
		app.Logf("BURN: %dM bursts, GC=%d, heap=%d KB, phase=%s, clock=%s",
			atomic.LoadUint64(&iters), ms.NumGC, ms.HeapAlloc>>10,
			map[bool]string{true: "work", false: "rest"}[inWork],
			time.Now().UTC().Format("15:04:05Z"))
	}
}
