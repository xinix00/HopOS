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
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"hop-os/metal/applib"
	"hop-os/metal/applib/appnet"
	"hop-os/metal/board"
	"hop-os/metal/checksum"
	"hop-os/metal/layout"
)

func main() {
	app := applib.Init()

	// Loggen loopt via de hop-ABI-ring naar de HOP-kern — niet rechtstreeks
	// naar de UART, zodat output van alle slots netjes gemultiplext wordt.
	app.Logf("Go-runtime leeft (%s), RAM %dMB @ %#x, klok=%s, BUCKET=%q ROLE=%q",
		runtime.Version(), app.RAMSize>>20, app.RAMStart,
		time.Now().UTC().Format("2006-01-02T15:04:05Z"), app.Env("BUCKET"), app.Env("ROLE"))

	// SMP (fase 5): één app, meerdere cores, gedeelde heap. De app deed hier
	// niets bijzonders — applib gaf hem GOMAXPROCS=N "as is". Deze rol bewijst
	// dat het echt parallel is.
	if app.Env("SMP") == "bench" {
		smpBench(app)
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

	// "Werk": periodiek een logregel; heartbeat en kill lopen via applib.
	for i := 1; ; i++ {
		time.Sleep(400 * time.Millisecond)
		app.Logf("werkje %d klaar", i)
	}
}

// exit geeft de laatste logregel de tijd om de ring uit te komen en stopt dan.
func exit(app *applib.App, code uint64) {
	time.Sleep(100 * time.Millisecond)
	app.Exit(code)
}

// smpSink houdt reken-resultaten levend zodat de compiler het werk niet weggooit.
var smpSink uint64

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
	// iteratie op welke core hij draaide. Zien we werk op de secundaire core(s),
	// dan verdeelt de runtime de goroutines écht over meerdere cores. Elke
	// goroutine yield't af en toe (Gosched) zodat de scheduler kan spreiden.
	var ran [12]atomic.Uint64
	var wg0 sync.WaitGroup
	const workers = 8
	for g := 0; g < workers; g++ {
		wg0.Add(1)
		go func() {
			defer wg0.Done()
			for i := 0; i < 2000; i++ {
				ran[board.Current().CoreID()%len(ran)].Add(1)
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
	for c := 1; c < len(ran); c++ {
		if v := ran[c].Load(); v > 0 {
			spread++
			app.Logf("SMP: core %d draaide %d iteraties", c, v)
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
