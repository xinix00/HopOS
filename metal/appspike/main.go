// De referentie-app voor fase 1: een eigen Go-runtime die HOP-OS in een
// slot laadt en op een eigen core start. Via applib meldt hij zich READY,
// stuurt heartbeats en gehoorzaamt de kill-flag. Per slot gelinkt
// (TEXT_START = SlotBase+0x10000, zie image/qemu-virt-run.sh); de
// RAM-declaratie wordt door HopOS bij het laden gepatcht (job.MemoryLimit).
package main

import (
	"bufio"
	"hash/fnv"
	"net"
	"runtime"
	"time"
	"unsafe"

	"hop-os/metal/applib"
	"hop-os/metal/applib/appnet"
	_ "hop-os/metal/board/qemuvirt"
	"hop-os/metal/layout"
)

func main() {
	app := applib.Init()

	// Loggen loopt via de hop-ABI-ring naar de HOP-kern — niet rechtstreeks
	// naar de UART, zodat output van alle slots netjes gemultiplext wordt.
	app.Logf("Go-runtime leeft (%s), RAM %dMB @ %#x, klok=%s, BUCKET=%q ROLE=%q",
		runtime.Version(), app.RAMSize>>20, app.RAMStart,
		time.Now().UTC().Format("2006-01-02T15:04:05Z"), app.Env("BUCKET"), app.Env("ROLE"))

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
		sum := fnv64(b)
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

// fnv64 is FNV-1a (hash/fnv); HOP rekent met dezelfde stdlib-som over hetzelfde
// bestand — één bron van waarheid, geen hand-getypte constanten die kunnen
// afwijken tussen app en HOP.
func fnv64(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}
