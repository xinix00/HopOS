//go:build tamago

// welcome is de app die een verse HopOS-node een gezicht geeft: één
// zelfstandige pagina op de gepubliceerde poort, zodat een headless node niet
// leeg aanvoelt maar vertelt dat hij leeft en wat er op hem draait. Hij staat
// in de standaard-hopos.init[] van de headless release.
//
// Alle zichtbare tekst (pagina én logs) is Engels — het is een publieke pagina.
//
// Config komt uit de jobspec-env; HOP zet ze allemaal zelf:
//
//	ER_PORT_HTTP  de gepubliceerde poort (uit ports:{http:...}); bind DIE
//	HOPOS_HOST    het node-IP waarop die poort van buiten open staat
//	HOP_DNS       de cluster-resolver (alleen om te tonen; niet gebruikt)
//
// Jobspec (zoals in image/hopos-headless.cfg) — één regel voor beide
// architecturen, de node kiest zijn eigen artifact via match op node.arch:
//
//	hopos.init[]={"name":"welcome","driver":"hop","artifacts":[
//	  {"url":"https://github.com/xinix00/hop/releases/download/rolling-release/welcome-arm64-tamago.elf","match":{"node.arch":"arm64"}},
//	  {"url":"https://github.com/xinix00/hop/releases/download/rolling-release/welcome-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}],
//	 "memory_limit":67108864,
//	 "ports":{"http":80}}
package main

import (
	"runtime"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/app/applib"
	"github.com/xinix00/HopOS/metal/v2/app/applib/appnet"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/lean/leanhttp"

	"github.com/xinix00/HopOS/apps/welcome/internal/welcome"
)

var version = "dev" // -ldflags "-X main.version=vX.Y.Z"

func main() {
	app := applib.Init() // eerste regel: READY + heartbeat + kill-flag

	ip, err := appnet.Up(app) // eigen TCP/IP-stack, eigen IP
	if err != nil {
		app.Logf("welcome: net: %v", err)
		app.Exit(1)
	}

	// De poort die HOP publiceerde, nooit een hardgecodeerde 80: draait dit
	// image zonder publicatie (handmatig testen), dan is 8080 de terugval.
	port := app.Env("ER_PORT_HTTP")
	if port == "" {
		port = "8080"
	}

	srv := welcome.NewServer(welcome.Node{
		Host:    app.Env("HOPOS_HOST"),
		IP:      ip,
		Port:    port,
		DNS:     app.Env("HOP_DNS"),
		Slot:    app.Slot,
		Cores:   runtime.NumCPU(), // SMP zet HopOS zelf; hier alleen tellen
		RAMSize: app.RAMSize,      // de partitie die HOP gaf: harde grens
		Arch:    runtime.GOARCH,   // arm64 óf riscv64
		Runtime: runtime.Version(),
		Version: version,
	}, coreState(app))

	app.Logf("%s", welcome.Banner(srv.Node()))
	app.Logf("http: %v", leanhttp.ListenAndServe(":"+port, srv.Handle))
	app.Exit(1) // een service die stopt met serveren is een crash, by design
}

// coreState leest CtrlShared van de eigen control-page: 1 zodra er een tweede
// levende bewoner op deze fysieke core zit (sharegroup), 0 als deze app hem
// alleen heeft. HOP is de enige schrijver en werkt het bij zolang de app leeft,
// dus dit wordt bij élke opvraging opnieuw gelezen en niet één keer onthouden.
//
// Rechtstreeks van de control-page en niet via applib, want applib gebruikt dit
// woord alleen zelf (de idle-governor yieldt erop) en biedt er geen accessor
// voor. Het is wel gewoon de ABI: abi/layout documenteert CtrlShared als
// HOP → app. Met dev.Pull erbij, want op een board waar HOP en dit hart niet
// coherent zijn staat de verse waarde anders alleen in HOP's cache.
func coreState(app *applib.App) func() welcome.CoreState {
	// Vast IPA sinds slot-ABI 8: het control-blok woont niet meer in de
	// partitie van de app maar in HOP's systeempot, op een adres dat per slot
	// vastligt (layout.SlotControl) — RAMStart/RAMSize zeggen er niets meer over.
	addr := layout.SlotControl(app.Slot) + layout.CtrlShared
	return func() welcome.CoreState {
		dev.Pull(addr, 8)
		if dev.Read64(addr) == 1 {
			return welcome.CoreShared
		}
		return welcome.CoreDedicated
	}
}
