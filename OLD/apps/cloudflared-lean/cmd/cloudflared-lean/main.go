// cloudflared-lean maakt een node achter NAT publiek bereikbaar via Cloudflare
// Tunnel — op onze eigen fundamenten.
//
// Waar apps/cloudflared cloudflared's eigen CLI in een slot draait (30MB beeld,
// hun hele dependency-boom), praat dit programma het tunnelprotocol zelf:
//
//	leantls          TLS 1.3 naar de edge (SNI h2.cftunnel.com, geen ALPN)
//	internal/h2      HTTP/2 — als SERVER, want de edge is de client
//	internal/capnp   de Cap'n Proto-registratie, drie vaste berichten
//	internal/ingress de routeertabel die Cloudflare naar ons duwt
//	leanhttp         de poot naar de lokale dienst
//
// Config uit de jobspec-env, met cloudflared's eigen namen zodat hun
// documentatie blijft gelden:
//
//	TUNNEL_TOKEN       verplicht: de named tunnel uit het dashboard
//	TUNNEL_URL         waar verkeer heen gaat vóór de eerste config-push
//	                   (default http://$HOPOS_HOST — de welcome-pagina)
//	TUNNEL_CONNECTIONS aantal edge-verbindingen (default 4, zoals cloudflared)
//
// De ingress (welke hostname naar welke dienst) komt van Cloudflare, niet uit
// deze env: dat is precies het punt van een remote-managed tunnel.
package main

import (
	"fmt"

	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/tunnel"
)

// version wordt door de build gezet (-X main.version).
var version = "dev"

// run is de gedeelde start: host en slot leveren hun eigen env, log en proxy.
// Zo is alles behalve de vervoerslaag host-getest.
func run(env func(string) string, logf func(string, ...any), arch string,
	proxy tunnel.Proxy, stop <-chan struct{}) error {

	raw := env("TUNNEL_TOKEN")
	if raw == "" {
		return fmt.Errorf("TUNNEL_TOKEN is empty: this tunnel needs a named tunnel from the Cloudflare dashboard")
	}
	tok, err := tunnel.ParseToken(raw)
	if err != nil {
		return err
	}
	fallback := env("TUNNEL_URL")
	if fallback == "" {
		if host := env("HOPOS_HOST"); host != "" {
			fallback = "http://" + host
		} else {
			fallback = "http://127.0.0.1:80"
		}
	}
	conns := 4
	if v := env("TUNNEL_CONNECTIONS"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &conns); err != nil || n != 1 || conns < 1 || conns > 8 {
			return fmt.Errorf("TUNNEL_CONNECTIONS must be 1..8, got %q", v)
		}
	}

	t, err := tunnel.New(tunnel.Options{
		Token:       tok,
		Fallback:    fallback,
		Version:     version,
		Arch:        arch,
		Connections: conns,
		Logf:        logf,
		Proxy:       proxy,
	})
	if err != nil {
		return err
	}
	logf("cloudflared-lean %s: tunnel %x, fallback %s", version, tok.TunnelID[:4], fallback)
	return t.Run(stop)
}
