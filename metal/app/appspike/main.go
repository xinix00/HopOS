// De referentie-app voor fase 1: een eigen Go-runtime die HOP-OS in een
// slot laadt en op een eigen core start. Via applib meldt hij zich READY,
// stuurt heartbeats en gehoorzaamt de kill-flag. Canoniek gelinkt
// (TEXT_START = SlotBase(1)+0x10000, zie image/qemu-run.sh) — de stage-2-map
// legt hem op de partitie van elk slot; de RAM-declaratie wordt door HopOS
// bij het laden gepatcht (job.MemoryLimit).
package main

import (
	"bufio"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"hop-os/metal/abi/checksum"
	"hop-os/metal/abi/layout"
	"hop-os/metal/app/applib"
	"hop-os/metal/app/applib/appnet"
	"hop-os/metal/board/appboard"
	"hop-os/metal/board/hopslot"
	"hop-os/metal/cpu/psci"
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

	// 1-core-prestatierol (bench.go): draait de app-core op volle N1-snelheid?
	if app.Env("BENCH") != "" {
		cpuBench(app)
	}

	// IJkgewicht voor de per-app CPU-meting (bench.go): DUTY=50 hoort op
	// ijzer als cpu≈50 in /tasks te staan (bewezen 18-07: 0/24/50/74/100).
	if v := app.Env("DUTY"); v != "" {
		pct, _ := strconv.Atoi(v) // fout → 0, cpuDuty klemt naar 1..100
		cpuDuty(app, pct)
	}

	// Soak-rol (P2b, docs/plan-p2b-soak.md): permanent CPU branden + heap
	// churnen op alle cores, met een telemetrieregel per minuut. De
	// heartbeat loopt vanzelf (applib), kill werkt gewoon — dit is de
	// "zware taak" voor de 24-uurs-soak; hij triggert continu de
	// dvfs-druk-flank.
	if app.Env("BURN") != "" {
		burn(app)
	}

	// Downloader-rol (freeze-jacht 13-07, idee Derek): sustained RX-DMA door
	// het VOLLE slot-pad (eigen netstack → switch → NAT-masquerade → GEM),
	// zonder job-churn en zonder core-0-buffering — streamen en weggooien.
	// Plain HTTP: test en passant of TLS-crypto een freeze-ingrediënt is.
	if url := app.Env("DOWNLOAD"); url != "" {
		download(app, url)
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

	// Isolatietest 2: praat bewust met de firmware. Een app heeft géén
	// legitieme SMC (zelfs SMP-bring-up loopt via HOP; exit is een HVC), dus
	// HCR_EL2.TSC hoort dit als EC=0x17 op de EL2-vector te trappen — de
	// tweede logregel mag nooit verschijnen.
	if app.Env("PROBE") == "smc" {
		app.Logf("PROBE: SMC PSCI_VERSION vanuit de kooi — EL2 hoort dit te trappen")
		time.Sleep(100 * time.Millisecond) // logregel eerst de ring uit
		v := psci.SMC(psci.VERSION, 0, 0, 0)
		app.Logf("PROBE: firmware antwoordde %#x — GEEN SMC-kooi!", v)
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
			app.Logf("FSDEMO writer: %v", err)
			exit(app, 1)
		}
		if err := app.WriteFile("/prive.txt", []byte("alleen van slot-eigenaar")); err != nil {
			app.Logf("FSDEMO writer: prive: %v", err)
			exit(app, 1)
		}
		app.Logf("FSDEMO writer: /data/db.bin (%d bytes) + eigen /prive.txt geschreven", len(data))
		exit(app, 0)

	case "reader":
		// Lees de gedeelde dataset en exit met de checksum; bewijs en passant
		// dat andermans privé-bestand en een '..'-escape onzichtbaar zijn.
		b, err := app.ReadFile("/data/db.bin")
		if err != nil {
			app.Logf("FSDEMO reader: %v", err)
			exit(app, 1)
		}
		if _, err := app.ReadFile("/prive.txt"); err == nil {
			app.Logf("FSDEMO reader: LEK — andermans prive-bestand zichtbaar")
			exit(app, 2)
		}
		if _, err := app.ReadFile("/../.tasks/slot1/prive.txt"); err == nil {
			app.Logf("FSDEMO reader: LEK — '..'-escape werkt")
			exit(app, 3)
		}
		sum := checksum.FNV64(b)
		app.Logf("FSDEMO reader: %d bytes, checksum %#x", len(b), sum)
		exit(app, sum)

	case "denied":
		// Zonder mount bestaat /data voor deze task simpelweg niet.
		if _, err := app.ReadFile("/data/db.bin"); err == nil {
			app.Logf("FSDEMO denied: LEK — /data zichtbaar zonder mount")
			exit(app, 1)
		}
		app.Logf("FSDEMO denied: /data onzichtbaar zonder mount — goed")
		exit(app, 0)

	case "fetch":
		// HOP downloadt voor ons; de bulk gaat buiten de ring om de storage in.
		n, err := app.Fetch(app.Env("FETCH_URL"), "/data/hello.txt")
		if err != nil {
			app.Logf("FSDEMO fetch: %v", err)
			exit(app, 1)
		}
		b, err := app.ReadFile("/data/hello.txt")
		if err != nil {
			app.Logf("FSDEMO fetch: teruglezen: %v", err)
			exit(app, 1)
		}
		app.Logf("FSDEMO fetch: %d bytes: %q", n, string(b[:min(len(b), 40)]))
		exit(app, 0)
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
			app.Logf("NETDEMO listen: %v", err)
			exit(app, 1)
		}
		port := app.Env("ER_PORT_HTTP")
		if port == "" {
			port = "8080"
		}
		l, err := net.Listen("tcp4", ":"+port)
		if err != nil {
			app.Logf("NETDEMO listen: %v", err)
			exit(app, 1)
		}
		app.Logf("NETDEMO listen: eigen stack op %s, poort :%s open", ip, port)
		for {
			conn, err := l.Accept()
			if err != nil {
				app.Logf("NETDEMO listen: accept: %v", err)
				exit(app, 1)
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
			app.Logf("NETDEMO dial: %v", err)
			exit(app, 1)
		}
		conn, err := net.Dial("tcp4", app.Env("NET_DIAL"))
		if err != nil {
			app.Logf("NETDEMO dial: %v", err)
			exit(app, 1)
		}
		if _, err := conn.Write([]byte("ping van " + ip + "\n")); err != nil {
			app.Logf("NETDEMO dial: write: %v", err)
			exit(app, 1)
		}
		resp, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil || resp != "pong ping van "+ip+"\n" {
			app.Logf("NETDEMO dial: onverwacht antwoord %q (%v)", resp, err)
			exit(app, 1)
		}
		app.Logf("NETDEMO dial: %s → %s: pong ontvangen — app↔app zonder HOP-TCP", ip, app.Env("NET_DIAL"))
		exit(app, 0)

	case "out":
		// Uitgaand naar buiten: één DNS-query (UDP) naar de node-resolver die
		// HOP als HOP_DNS meegaf. HOP masquerade't de query (slot-IP:poort →
		// node-IP:node-poort) de externe NIC uit en het antwoord terug — een
		// respóns bewijst de hele round-trip, ongeacht wat erin staat. Dít is
		// het pad dat straks cloudflared/servers naar buiten gebruiken.
		if _, err := appnet.Up(app); err != nil {
			app.Logf("NETDEMO out: %v", err)
			exit(app, 1)
		}
		dns := app.Env("HOP_DNS")
		if dns == "" {
			app.Logf("NETDEMO out: geen HOP_DNS meegegeven")
			exit(app, 1)
		}
		conn, err := net.Dial("udp4", dns)
		if err != nil {
			app.Logf("NETDEMO out: dial %s: %v", dns, err)
			exit(app, 1)
		}
		defer conn.Close()
		// Minimale DNS A-query voor "a.root-servers.net" (id 0x1234, RD).
		query := []byte{
			0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x01, 'a', 0x0c, 'r', 'o', 'o', 't', '-', 's', 'e', 'r', 'v', 'e', 'r', 's',
			0x03, 'n', 'e', 't', 0x00, 0x00, 0x01, 0x00, 0x01,
		}
		if _, err := conn.Write(query); err != nil {
			app.Logf("NETDEMO out: write: %v", err)
			exit(app, 1)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp := make([]byte, 512)
		n, err := conn.Read(resp)
		if err != nil || n < 12 || resp[0] != 0x12 || resp[1] != 0x34 {
			app.Logf("NETDEMO out: geen bruikbaar DNS-antwoord (n=%d, %v)", n, err)
			exit(app, 1)
		}
		app.Logf("NETDEMO out: DNS-antwoord van %s (%d bytes) — uitgaande masquerade werkt", dns, n)
		exit(app, 0)
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

// download is de sustained-RX-DMA-rol (freeze-jacht 13-07): eindeloos een
// groot bestand streamen door de eigen netstack en de bytes weggooien — het
// volle slot-pad (appnet → switch → NAT → GEM) onder continue inbound-druk,
// zonder churn en zonder buffering. Plain HTTP houdt TLS buiten het
// experiment. Body.Read blokkeert (yield), dus heartbeat/kill lopen gewoon.
func download(app *applib.App, url string) {
	ip, err := appnet.Up(app)
	if err != nil {
		app.Logf("DOWNLOAD: netstack: %v", err)
		return
	}
	if d := app.Env("HOP_DNS"); d != "" {
		net.SetDefaultNS([]string{d})
	}
	app.Logf("DOWNLOAD: stack up on %s — streaming %s to /dev/null", ip, url)
	buf := make([]byte, 32<<10)
	var total, last uint64
	for round := 1; ; round++ {
		resp, err := http.Get(url)
		if err != nil {
			app.Logf("DOWNLOAD: %v — retry in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if round == 1 || resp.StatusCode != 200 {
			app.Logf("DOWNLOAD: HTTP %s, length %d", resp.Status, resp.ContentLength)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			time.Sleep(5 * time.Second)
			continue
		}
		var n uint64
		for {
			k, err := resp.Body.Read(buf)
			n += uint64(k)
			total += uint64(k)
			if total-last >= 100<<20 {
				last = total
				app.Logf("DOWNLOAD: %d MB total (round %d)", total>>20, round)
			}
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		app.Logf("DOWNLOAD: round %d done — %d MB this round, %d MB total", round, n>>20, total>>20)
	}
}

// smpBench bewijst fase 5: de app draait op meerdere cores met één gedeelde
// heap, en heeft daar zelf niets voor hoeven doen (applib zette GOMAXPROCS).
func smpBench(app *applib.App) {
	n := runtime.GOMAXPROCS(0)
	app.Logf("SMP: app ziet %d cores (GOMAXPROCS), RAM %dMB — app-code deed hier niets voor", n, app.RAMSize>>20)
	if n < 2 {
		app.Logf("SMP: minder dan 2 cores toegewezen — geen SMP")
		exit(app, 1)
	}

	// 1) Parallellisme-bewijs: N CPU-drukke goroutines tegelijk; elk telt per
	// iteratie op welke core hij draaide. Zien we werk op meerdere cores, dan
	// verdeelt de runtime de goroutines écht. De core-onderscheider is het
	// rauwe MPIDR (hopslot.MPIDR): per fysieke core gegarandeerd verschillend
	// op elk board — CoreID is hier onbruikbaar, want dat is de SLOT-identiteit
	// (slotHint) en die is voor alle cores van één app gelijk. Elke goroutine
	// yield't af en toe (Gosched) zodat de scheduler kan spreiden.
	var ran [12]atomic.Uint64
	var wg0 sync.WaitGroup
	const workers = 8
	for g := 0; g < workers; g++ {
		wg0.Add(1)
		go func() {
			defer wg0.Done()
			for i := 0; i < 2000; i++ {
				ran[hopslot.MPIDR()%uint64(len(ran))].Add(1)
				for j := 0; j < 20000; j++ {
				}
				if i%50 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	wg0.Wait()
	spread := 0
	for c := 0; c < len(ran); c++ {
		if v := ran[c].Load(); v > 0 {
			spread++
			app.Logf("SMP: core-bucket %d draaide %d iteraties", c, v)
		}
	}
	if spread < 2 {
		app.Logf("SMP: werk liep op %d core(s) — geen echt parallellisme", spread)
		exit(app, 2)
	}
	app.Logf("SMP: goroutines liepen parallel op %d cores — echte multi-core", spread)

	// 2) Gedeelde heap: twee goroutines vullen om-en-om dezelfde slice (één
	// adresruimte, nul berichten ertussen) en HOP zit er niet tussen. Verifieer.
	const N = 1 << 20
	shared := make([]uint32, N)
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < N; i += 2 {
				shared[i] = uint32(i)
			}
		}(g)
	}
	wg.Wait()
	var sum uint64
	for i := 0; i < N; i++ {
		if shared[i] != uint32(i) {
			app.Logf("SMP: gedeelde slice corrupt @ %d (=%d)", i, shared[i])
			exit(app, 3)
		}
		sum += uint64(shared[i])
	}
	app.Logf("SMP: gedeelde heap OK — %d elementen door twee cores beschreven (som %d)", N, sum)

	// 3) GC over de gedeelde heap: allocatie-druk op alle cores + een volledige
	// GC-cyclus; de stop-the-world moet elke core bereiken (ReadMemStats/GC
	// zouden anders hangen). Overleven = de coöperatieve STW werkt cross-core.
	gc0 := gcCount()
	var wg2 sync.WaitGroup
	for g := 0; g < n; g++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			var keep [][]byte
			for i := 0; i < 300; i++ {
				keep = append(keep, make([]byte, 4096))
				if len(keep) > 32 {
					keep = keep[16:]
				}
			}
			atomic.AddUint64(&smpSink, uint64(len(keep)))
		}()
	}
	wg2.Wait()
	runtime.GC()
	app.Logf("SMP: GC overleefd op de gedeelde heap (NumGC %d→%d) — cross-core STW werkt", gc0, gcCount())

	// 4) Speedup (informatief; onder emulatie variabel): zelfde werk serieel vs.
	// over n goroutines. Het rendezvous is het harde bewijs; dit is de maat.
	const W = 6_000_000
	t1 := time.Now()
	smpWork(W)
	d1 := time.Since(t1)
	t2 := time.Now()
	var wg3 sync.WaitGroup
	for g := 0; g < n; g++ {
		wg3.Add(1)
		go func() { defer wg3.Done(); smpWork(W / n) }()
	}
	wg3.Wait()
	d2 := time.Since(t2)
	app.Logf("SMP: werk serieel %v, parallel(%d) %v → %.2fx", d1, n, d2, float64(d1)/float64(d2))

	exit(app, 0)
}

//go:noinline
func smpWork(iters int) {
	var s uint64
	for i := 0; i < iters; i++ {
		s += uint64(i)*2654435761 ^ s>>13
	}
	atomic.AddUint64(&smpSink, s)
}

func gcCount() uint32 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.NumGC
}
