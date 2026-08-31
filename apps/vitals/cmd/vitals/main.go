//go:build tamago

// vitals is de board-dokter: één app die de vitale functies van een node meet
// en benchmarkt — idle-gedrag, CPU-doorvoer (incl. volgehouden last + thermiek),
// geheugenbandbreedte en -latentie, netwerkdoorvoer en scheduler-jitter. Voor
// bring-up en diagnose van nieuwe boards: draai vitals, druk op "run all", en
// vergelijk het rapport met een gezond board.
//
// Alle zichtbare tekst (pagina én logs) is Engels — het is een publieke pagina.
//
// Config komt uit de jobspec-env; alles heeft een werkende default:
//
//	ER_PORT_HTTP   de gepubliceerde poort (uit ports:{http:...}); fallback 8080
//	HOPOS_HOST     het node-IP (HOP zet hem altijd); doel van de storm-test
//	HOP_ADDR       agent-API voor temperatuur (default 10.100.0.1:8080)
//	HOP_KEY        cluster-key voor de agent-API; leeg = geen temperatuur
//	VITALS_RX_URL  bron voor de download-test (plain http; default cachefly)
//
// Jobspec (poort 8090, naast welcome's 80):
//
//	hopos.init[]={"name":"vitals","driver":"hop","artifacts":[
//	  {"url":"https://github.com/xinix00/hop/releases/download/rolling-release/vitals-arm64-tamago.elf","match":{"node.arch":"arm64"}},
//	  {"url":"https://github.com/xinix00/hop/releases/download/rolling-release/vitals-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}],
//	 "memory_limit":134217728,
//	 "cpu_shares":2048,
//	 "ports":{"http":8090}}
//
// cpu_shares ≥ 2048 (2 cores) maakt de smp-test zinvol; met 1 core slaat hij
// zichzelf over. Geen sharegroup: benchmarkcijfers horen van een dedicated
// core te komen. memory_limit 128MB geeft de geheugentests ruimte om boven de
// caches uit te meten.
package main

import (
	"runtime"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/lean/leanhttp"

	"github.com/xinix00/HopOS/apps/vitals/internal/vitals"
)

var version = "dev" // -ldflags "-X main.version=vX.Y.Z"

func main() {
	app := applib.Init() // eerste regel: READY + heartbeat + kill-flag

	ip, err := appnet.Up(app) // eigen TCP/IP-stack, eigen IP
	if err != nil {
		app.Logf("vitals: net: %v", err)
		app.Exit(1)
	}

	port := app.Env("ER_PORT_HTTP")
	if port == "" {
		port = "8080"
	}

	// De control-page is het app↔HOP-oppervlak; vitals leest er zijn eigen
	// telemetriewoorden van terug (CtrlIdle/CtrlWakes publiceert applib, maar
	// biedt er geen accessor voor — HOP leest CtrlWakes bewust niet, dus déze
	// app is de afnemer). Met dev.Pull erbij: op een board waar HOP en dit
	// hart niet coherent zijn staat de verse waarde anders alleen in een cache.
	ctrl := layout.CtrlPageAt(app.RAMStart, app.RAMSize)
	ctrlRead := func(off uint64) uint64 {
		a := ctrl + uintptr(off)
		dev.Pull(a, 8)
		return dev.Read64(a)
	}

	// VITALS_NOCTRL=1: de control-page NIET lezen. Nodig op een board waar die
	// lees faultt — op de Mac mini M4 sloeg `dev.Pull`+`Read64` op de
	// control-page de app om bij het opstarten (31-08, exit 2 in idle.go),
	// waardoor ÉLKE andere test onbereikbaar werd. De idle-cijfers vallen dan
	// weg (die komen nergens anders vandaan), maar cpu/membw/memlat/burn/gc/
	// timer/rx/tx zijn dan wél te meten. Een dokter die niet binnenkomt omdat
	// één instrument stuk is, is een slechte dokter.
	if app.Env("VITALS_NOCTRL") == "1" {
		ctrlRead = nil
		app.Logf("vitals: control page disabled (VITALS_NOCTRL=1) — no idle figures")
	}

	srv := vitals.NewServer(vitals.Config{
		Version: version,
		Arch:    runtime.GOARCH,
		Runtime: runtime.Version(),
		Slot:    app.Slot,
		RAMSize: app.RAMSize,
		IP:      ip,
		Host:    app.Env("HOPOS_HOST"),
		Port:    port,

		Logf:      app.Logf,
		CtrlRead:  ctrlRead,
		CounterHz: idle.CounterHz,
		Offsets: vitals.Offsets{
			Idle:      layout.CtrlIdle,
			Wakes:     layout.CtrlWakes,
			Cores:     layout.CtrlCores,
			Shared:    layout.CtrlShared,
			Heartbeat: layout.CtrlHeartbeat,
			MemSys:    layout.CtrlMemSys,
		},

		HopAddr: app.Env("HOP_ADDR"),
		HopKey:  app.Env("HOP_KEY"),
		RxURL:   app.Env("VITALS_RX_URL"),
	})
	srv.Start()

	app.Logf("vitals %s: serving on %s:%s (slot %d, %s, %d core(s))",
		version, ip, port, app.Slot, runtime.GOARCH, runtime.NumCPU())
	app.Logf("http: %v", leanhttp.ListenAndServe(":"+port, srv.Handle))
	app.Exit(1) // een service die stopt met serveren is een crash, by design
}
