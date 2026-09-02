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
	"runtime/debug"
	"sort"
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

	// Meetbank voor de core-deling (BENCH, aangestuurd door schedbench.go in
	// de kern). Twee rollen die samen meten wat een hop tussen twee bewoners
	// van één core kost: "echo" antwoordt, "ping" klokt de round-trips. Beide
	// apps wonen op dezelfde fysieke core, dus elke round-trip is minstens
	// twee wissels — precies het getal dat we willen zien als de RX-slaapstand
	// verandert.
	switch app.Env("BENCH") {
	case "echo":
		// Stille echo-server: regel in, regel terug. Bewust GEEN log per regel
		// (dat is zelf een ring-write per round-trip en zou de meting zijn).
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "BENCH echo: %v", err)
		}
		l, err := net.Listen("tcp4", ":9000")
		if err != nil {
			exitf(app, 1, "BENCH echo: %v", err)
		}
		app.Logf("BENCH echo: ready on :9000")
		for {
			conn, err := l.Accept()
			if err != nil {
				exitf(app, 1, "BENCH echo: accept: %v", err)
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := c.Write([]byte(line)); err != nil {
						return
					}
				}
			}(conn)
		}

	case "ping":
		// Klok N round-trips over ÉÉN open verbinding (dus zonder handshake in
		// de meting) en rapporteer de verdeling. Daarna stil blijven: HOP meet
		// hierna het idle-tempo van dit paar.
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "BENCH ping: %v", err)
		}
		var conn net.Conn
		var err error
		for i := 0; i < 50; i++ { // de buurman mag nog opkomen
			conn, err = net.Dial("tcp4", app.Env("BENCH_PEER"))
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			exitf(app, 1, "BENCH ping: dial %s: %v", app.Env("BENCH_PEER"), err)
		}
		n := 200
		rtt := make([]time.Duration, 0, n)
		r := bufio.NewReader(conn)
		for i := 0; i < n; i++ {
			t0 := time.Now()
			if _, err := conn.Write([]byte("ping\n")); err != nil {
				exitf(app, 1, "BENCH ping: write: %v", err)
			}
			if _, err := r.ReadString('\n'); err != nil {
				exitf(app, 1, "BENCH ping: read: %v", err)
			}
			rtt = append(rtt, time.Since(t0))
		}
		sort.Slice(rtt, func(i, j int) bool { return rtt[i] < rtt[j] })
		app.Logf("BENCH_RTT rxpoll=%q n=%d min=%dus p50=%dus p90=%dus p99=%dus max=%dus",
			app.Env("RXPOLL"), n, rtt[0].Microseconds(), rtt[n/2].Microseconds(),
			rtt[n*9/10].Microseconds(), rtt[n*99/100].Microseconds(), rtt[n-1].Microseconds())

		// De tegenmeting: het EERSTE pakket ná stilte. Een adaptieve RX-slaap
		// koopt zijn wekken terug met precies dit getal — een seconde niets
		// doen, dan één round-trip over de al open verbinding (dus zonder
		// handshake: puur de wek-kosten van de buurman).
		cold := make([]time.Duration, 0, 15)
		for i := 0; i < cap(cold); i++ {
			time.Sleep(time.Second)
			t0 := time.Now()
			if _, err := conn.Write([]byte("cold\n")); err != nil {
				exitf(app, 1, "BENCH cold: write: %v", err)
			}
			if _, err := r.ReadString('\n'); err != nil {
				exitf(app, 1, "BENCH cold: read: %v", err)
			}
			cold = append(cold, time.Since(t0))
		}
		conn.Close()
		sort.Slice(cold, func(i, j int) bool { return cold[i] < cold[j] })
		m := len(cold)
		app.Logf("BENCH_COLD rxpoll=%q n=%d min=%dus p50=%dus p90=%dus max=%dus",
			app.Env("RXPOLL"), m, cold[0].Microseconds(), cold[m/2].Microseconds(),
			cold[m*9/10].Microseconds(), cold[m-1].Microseconds())
		for {
			time.Sleep(time.Hour) // stil blijven: nu meet HOP het idle-tempo
		}
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

	case "outkeep":
		// Dezelfde uitgaande masquerade, maar dan als LANGLEVENDE sessie: één
		// socket, elke paar seconden een query, en de app blijft leven. Dat is
		// het instrument voor de kern-flip (docs/kern-flip.md): de conntrack-
		// mapping van deze ene socket moet een kernwissel overleven, anders
		// vindt het antwoord de weg terug niet meer en valt deze app om. Precies
		// het gedrag van een cloudflared-tunnel, in tien regels.
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "NETDEMO outkeep: %v", err)
		}
		dns := app.Env("HOP_DNS")
		if dns == "" {
			exitf(app, 1, "NETDEMO outkeep: geen HOP_DNS meegegeven")
		}
		conn, err := net.Dial("udp4", dns)
		if err != nil {
			exitf(app, 1, "NETDEMO outkeep: dial %s: %v", dns, err)
		}
		defer conn.Close()
		query := []byte{
			0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x01, 'a', 0x0c, 'r', 'o', 'o', 't', '-', 's', 'e', 'r', 'v', 'e', 'r', 's',
			0x03, 'n', 'e', 't', 0x00, 0x00, 0x01, 0x00, 0x01,
		}
		resp := make([]byte, 512)
		// Een enkele verloren query is geen storing: de allereerste uitgaande
		// frames van een node kunnen sneuvelen terwijl hij zijn gateway-MAC nog
		// leert (ARP), en UDP mag sowieso pakketten verliezen. Pas een REEKS
		// stiltes betekent dat de weg terug echt weg is — dat is precies het
		// verschil dat de flip-regressie moet kunnen zien, en het is ook hoe een
		// echte tunnel zich hoort te gedragen.
		misses := 0
		for round := 1; ; round++ {
			if _, err := conn.Write(query); err != nil {
				exitf(app, 1, "NETDEMO outkeep: write in ronde %d: %v", round, err)
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := conn.Read(resp)
			if err != nil || n < 12 || resp[0] != 0x12 || resp[1] != 0x34 {
				if misses++; misses >= 4 {
					exitf(app, 1, "NETDEMO outkeep: %d rondes op rij zonder antwoord (laatste: n=%d, %v) — NAT-mapping weg?", misses, n, err)
				}
				app.Logf("NETDEMO outkeep: ronde %d zonder antwoord (%d op rij) — opnieuw", round, misses)
				continue
			}
			misses = 0
			// Optioneel: bewijs in dezelfde ronde dat het gemounte volume
			// bruikbaar is. Voor de kern-flip is dát de vraag — niet of de oude
			// inhoud er nog staat (hopfs is bewust vluchtig, ook over een
			// reboot), maar of de app na de wissel nog ergens KAN schrijven.
			// Ontbreekt het mount-punt, dan faalt de write en zegt deze app het.
			if path := app.Env("MOUNTCHECK"); path != "" {
				want := fmt.Sprintf("ronde %d", round)
				if err := app.WriteFile(path, []byte(want)); err != nil {
					exitf(app, 1, "MOUNTCHECK: schrijven naar %s in ronde %d: %v — mount weg?", path, round, err)
				}
				got, err := app.ReadFile(path)
				if err != nil || string(got) != want {
					exitf(app, 1, "MOUNTCHECK: %s las %q (%v), wil %q", path, got, err, want)
				}
			}
			app.Logf("NETDEMO outkeep: ronde %d, %d bytes terug — de mapping leeft", round, n)
			time.Sleep(2 * time.Second)
		}
	}

	// Multicast-demo (het matter/mDNS-pad, 15-08): listen joint de mDNS-groep
	// en logt elk datagram; send stuurt er elke seconde één naar de groep.
	// Samen bewijzen ze leannet-multicast + hopswitch-flood van slot naar
	// slot — precies wat matter-discovery op de node nodig heeft.
	switch app.Env("MCAST") {
	case "listen":
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "MCAST listen: %v", err)
		}
		if err := appnet.JoinMulticast(net.IPv4(224, 0, 0, 251)); err != nil {
			exitf(app, 1, "MCAST listen: join: %v", err)
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 5353})
		if err != nil {
			exitf(app, 1, "MCAST listen: %v", err)
		}
		app.Logf("MCAST listen: joined 224.0.0.251, port 5353 open")
		buf := make([]byte, 512)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				exitf(app, 1, "MCAST listen: read: %v", err)
			}
			app.Logf("MCAST listen: %q from %s", buf[:n], src)
		}
	case "send":
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "MCAST send: %v", err)
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
		if err != nil {
			exitf(app, 1, "MCAST send: %v", err)
		}
		dst := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
		for i := 0; ; i++ {
			if _, err := conn.WriteToUDP(fmt.Appendf(nil, "mdns-probe %d", i), dst); err != nil {
				app.Logf("MCAST send: %v", err)
			} else if i%5 == 0 {
				app.Logf("MCAST send: probe %d sent to 224.0.0.251:5353", i)
			}
			time.Sleep(time.Second)
		}
	}

	// IPv6-demo (het matter-pad, 18-08): listen opent een udp6-poort en
	// echoot; send leidt de link-local van een buurslot af uit diens
	// vaste MAC (fe80:: + EUI-64 van 02:00:00:00:00:0N) en stuurt er elke
	// seconde één datagram heen. Samen bewijzen ze de hele leanipv6-baan
	// op echte ringen: NDP over de 33:33-flood van de switch, unicast v6
	// slot↔slot, en UDP6 in beide richtingen — zonder join, want unicast.
	switch app.Env("V6") {
	case "listen":
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "V6 listen: %v", err)
		}
		// Addr-probe (18-08, unifi-jacht): wat rapporteert een wildcard-listener
		// als zijn eigen adres? Hoort het slot-IP met de echte poort te zijn —
		// ":0" betekent dat de shim het gevraagde adres teruggeeft i.p.v. het
		// gebonden adres, en dan is elke URL die een app eruit bouwt dood.
		if l, err := net.Listen("tcp", ":0"); err == nil {
			app.Logf("V6 ADDRPROBE tcp: %s", l.Addr())
			l.Close()
		} else {
			app.Logf("V6 ADDRPROBE tcp: %v", err)
		}
		conn, err := net.ListenUDP("udp6", &net.UDPAddr{Port: 7776})
		if err != nil {
			exitf(app, 1, "V6 listen: %v", err)
		}
		app.Logf("V6 listen: udp6 port 7776 open")
		buf := make([]byte, 512)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				exitf(app, 1, "V6 listen: read: %v", err)
			}
			app.Logf("V6 listen: %q from %s", buf[:n], src)
			if _, err := conn.WriteToUDP(append([]byte("echo:"), buf[:n]...), src); err != nil {
				app.Logf("V6 listen: echo: %v", err)
			}
		}
	case "send":
		if _, err := appnet.Up(app); err != nil {
			exitf(app, 1, "V6 send: %v", err)
		}
		slot := 0
		fmt.Sscanf(app.Env("V6PEER"), "%d", &slot)
		if slot < 1 {
			exitf(app, 1, "V6 send: set V6PEER to the listener's slot number")
		}
		peer := &net.UDPAddr{IP: net.IP{0xfe, 0x80, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0xff, 0xfe, 0, 0, byte(slot)}, Port: 7776}
		conn, err := net.DialUDP("udp6", nil, peer)
		if err != nil {
			exitf(app, 1, "V6 send: dial: %v", err)
		}
		go func() {
			buf := make([]byte, 512)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				app.Logf("V6 send: reply %q", buf[:n])
			}
		}()
		for i := 0; ; i++ {
			if _, err := conn.Write(fmt.Appendf(nil, "v6-probe %d", i)); err != nil {
				app.Logf("V6 send: %v", err)
			} else if i%5 == 0 {
				app.Logf("V6 send: probe %d sent to [%s]:7776", i, peer.IP)
			}
			time.Sleep(time.Second)
		}
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

	// GC-doodspiraal-rol (15-08): live heap tegen het memlimit-plafond pinnen
	// en dan churnen. Dít is de last die op ijzer een gedeelde hart gijzelde
	// (een TLS-app in een 20MB-venster); de memlimit-wachter hoort hem binnen
	// twee vensters LUID te maken (HOPOS_GC_THRASH) in plaats van stil te
	// laten malen. Keert nooit terug: het einde hoort die panic te zijn.
	if app.Env("THRASH") != "" {
		thrash(app)
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

// ball is de bouwsteen van thrash' live set: klein en met een pointer, zodat
// de GC-markering per cyclus de héle keten moet lopen — zoals de geparste
// x509-roots die op het ijzer de dader waren. Bytes-buffers zijn hier
// waardeloos: die liggen noscan en maken de GC juist goedkoop.
type ball struct {
	next *ball
	pad  [40]byte
}

// thrash pint een pointer-rijke live heap onder het memlimit-plafond en
// churnt dan door: de GC draait vanaf dat moment rug-aan-rug zonder ooit iets
// terug te winnen — 100% compute, nul voortgang, nul yields. Bewust op 3/5
// van de limiet en niet erop: dichterbij wint de sbrk-OOM het van het malen
// (gemeten 15-08, tweemaal: op 3/4 duwde de churn "in use" plus één
// 4MB-chunkverzoek door de arena-muur — floating garbage en fragmentatie
// eten de marge). Die OOM-dood is al luid; het stille malen erónder is wat
// de memlimit-wachter luid moet maken.
func thrash(app *applib.App) {
	limit := debug.SetMemoryLimit(-1) // -1 = alleen uitlezen, niets zetten
	app.Logf("THRASH: pinning a pointer-rich live heap against the %d MB memory limit, then churning", limit>>20)
	time.Sleep(100 * time.Millisecond) // logregel eerst de ring uit
	var head *ball
	var m runtime.MemStats
	for {
		runtime.ReadMemStats(&m)
		if int64(m.HeapAlloc) >= limit/2 {
			break
		}
		for i := 0; i < 4096; i++ {
			head = &ball{next: head}
		}
	}
	// De spiraal-stand: bijna geen headroom per cyclus terwijl élke cyclus
	// de hele pointer-keten markeert. Op het ijzer ontstond die vanzelf
	// (live tegen de limiet drukt de headroom naar nul); de rol zet de knop
	// expliciet — zelfde toestand, zonder de sbrk-muur te schampen. De
	// garbage moet KLEIN blijven: 64KB-blokken zijn large objects en hun
	// verse 4MB-chunks liepen tweemaal de arena-muur in (de OOM hierboven).
	debug.SetGCPercent(5)
	runtime.ReadMemStats(&m)
	app.Logf("THRASH: pinned, live %d MB, sys %d MB, gc %d — churning", m.HeapAlloc>>20, m.Sys>>20, m.NumGC)
	last := time.Now()
	for {
		// Kleine batch: het zwerfvuil tussen twee GC-afrondingen (die op
		// single-P alleen op het Gosched-punt landen) moet ruim onder het
		// niet-heap-gat naar de sbrk-muur blijven — 4096/batch liet op TCG
		// ~10MB zwerfvuil ophopen en OOM'de vóór de wachter kon vonnissen.
		for i := 0; i < 512; i++ {
			ballSink = &ball{next: head}
		}
		// Zonder schedulingspunt maakt de GC op een single-P tamago nooit
		// een cyclus af (gemeten 15-08: een kale alloc-lus at in <2s 8MB
		// naar de sbrk-muur, nul voltooide GC's — de vul-lus hierboven
		// werkte alleen doordat ReadMemStats per batch de wereld stopte).
		// Het echte werk dat op ijzer thrashte (x509-parse) zit vol
		// natuurlijke schedulingspunten; deze Gosched speelt die rol.
		runtime.Gosched()
		// Meetregel per ~2s: draait de GC (NumGC), en waar groeit het
		// geheugen heen — zonder dit was de churn-OOM blind.
		if time.Since(last) >= 2*time.Second {
			runtime.ReadMemStats(&m)
			app.Logf("THRASH: live %d MB, sys %d MB, gc %d, next %d MB, gcfrac %.3f",
				m.HeapAlloc>>20, m.Sys>>20, m.NumGC, m.NextGC>>20, m.GCCPUFraction)
			last = time.Now()
		}
	}
}

// ballSink houdt de churn-allocatie heap-echt (een lokale &ball{} blijft op
// de stack en alloceert dan niets).
var ballSink *ball

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
