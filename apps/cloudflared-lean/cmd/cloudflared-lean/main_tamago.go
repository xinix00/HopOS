//go:build tamago

package main

import (
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/origin"
	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"
)

// De slot-variant. Anders dan apps/cloudflared linkt deze GEEN root-bundel voor
// de systeem-trust-store: de edge bewijst zich met Cloudflare's eigen CA's en
// die zijn ingebakken (internal/tunnel/cfroots.pem). Er is hier dus geen
// x509.SystemCertPool die leeg terugvalt en geen 300kB rootbundel voor drie
// certificaten die we exact kennen.
func main() {
	app := applib.Init() // eerste regel: READY + heartbeat + kill-vlag

	// Een panic als één leesbare regel, reden als laatste — dat is de regel die
	// HOP op de node-console echoot (last="…"). Zelfde vorm als de andere apps.
	defer func() {
		if r := recover(); r != nil {
			for _, line := range strings.Split(strings.TrimRight(string(debug.Stack()), "\n"), "\n") {
				app.Logf("  %s", strings.ReplaceAll(line, "\t", "    "))
			}
			app.Logf("cloudflared-lean: PANIC: %v", r)
			app.Exit(2)
		}
	}()

	ip, err := appnet.Up(app) // eigen TCP/IP-stack, eigen IP
	if err != nil {
		app.Logf("cloudflared-lean: net: %v", err)
		app.Exit(1)
	}
	app.Logf("cloudflared-lean %s: slot at %s, dialing out to the Cloudflare edge", version, ip)

	// De kill-vlag van HOP is de enige echte exit; stop blijft dus open en de
	// tunnel loopt tot het slot wordt opgeruimd.
	stop := make(chan struct{})
	if err := run(app.Env, app.Logf, "hopos_"+runtime.GOARCH, origin.Proxy, stop); err != nil {
		app.Logf("cloudflared-lean: %v", err)
		app.Exit(1)
	}
	// Run keert alleen terug als er niets meer te proberen valt (een ingetrokken
	// token bijvoorbeeld). Netjes stoppen is dan eerlijker dan blijven hangen:
	// HOP ziet de exit en het staat in de status.
	app.Logf("cloudflared-lean: no connection left to try; stopping")
	app.Exit(1)
}
