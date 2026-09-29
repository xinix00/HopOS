// Package cfd bouwt de opdrachtregel en de proces-env waarmee cloudflared's
// eigen tunnel-CLI in een HopOS-slot start. Bewust een apart pakket zonder
// cloudflared-import: zo is alles wat wij bepalen — welke modus, welke flags,
// welke defaults — host-getest, en hangt alleen de main aan de tamago-gate.
//
// Twee modi, afgeleid uit één ding: is er een token?
//
//	TUNNEL_TOKEN gezet  → named tunnel: `cloudflared tunnel run --token …`
//	                      De ingress (welke hostname naar welke service) komt
//	                      van het Cloudflare-dashboard, niet van hier.
//	geen token          → quick tunnel: `cloudflared tunnel --url …`
//	                      Gratis, anoniem, een trycloudflare.com-URL. Perfect
//	                      om een verse node één keer publiek te maken; de URL
//	                      verschijnt in `hop logs cloudflared`.
//
// De default-URL is http://<HOPOS_HOST> — de gepubliceerde poort 80 van deze
// node, oftewel: standaard tunnelt dit de welcome-pagina naar buiten.
package cfd

import (
	"errors"
	"fmt"
	"strings"
)

// Bridged zijn de cloudflared-env-namen die we uit de jobspec doorgeven aan de
// proces-env van de app. Nodig omdat de env van een slot NIET de proces-env is:
// applib leest hem van de control-page, dus `os.Getenv` ziet niks tenzij wij
// het overzetten. Alles hier is een echte cloudflared-env-naam (zie
// `cloudflared tunnel run --help`), dus de jobspec leest als hun documentatie.
//
// Uitbreiden = een naam toevoegen: applib kan zijn env niet opsommen (alleen
// opvragen), dus een allowlist is het enige dat kan. TestBridgedNamesAreReal…
// toetst elke naam hier tegen de flag-boom van de gepinde cloudflared, dus een
// verzonnen of hernoemde naam valt om in de tests en niet op een node. Flags
// zónder env-naam (--ha-connections, --no-tls-verify bijvoorbeeld) horen hier
// dus niet: die geef je mee via CFD_EXTRA_ARGS.
var Bridged = []string{
	"TUNNEL_TOKEN",
	"TUNNEL_URL",
	"TUNNEL_TRANSPORT_PROTOCOL",
	"TUNNEL_LOGLEVEL",
	"TUNNEL_REGION",
	"TUNNEL_EDGE_IP_VERSION",
	"TUNNEL_RETRIES",
	"TUNNEL_GRACE_PERIOD",
	"TUNNEL_METRICS",
	"TUNNEL_ORIGIN_SERVER_NAME",
	"NO_AUTOUPDATE",
}

// Config is wat de jobspec meegaf, met de defaults er al in.
type Config struct {
	Token    string // TUNNEL_TOKEN — leeg = quick tunnel
	URL      string // de lokale service die naar buiten gaat
	Protocol string // http2 (default) of quic
	LogLevel string // cloudflared's --loglevel
	Metrics  string // waar de metrics/readiness-server bindt
	Extra    []string
}

// Defaults die niet uit de env komen.
const (
	// DefaultProtocol is http2 en niet cloudflared's eigen default (quic):
	// http2 is TCP+TLS en loopt daarmee over hetzelfde pad als elke andere
	// uitgaande verbinding van een slot. QUIC is UDP met datagram-groottes en
	// socket-opties die door de eigen gVisor-netstack en HOP's masquerade heen
	// moeten — dat kan werken, maar het is niet gemeten. Zet
	// TUNNEL_TRANSPORT_PROTOCOL=quic om het te proberen.
	DefaultProtocol = "http2"
	DefaultLogLevel = "info"

	// DefaultMetricsPort is de poort van cloudflared's metrics/readiness-server.
	// Hij bindt op het eigen slot-IP, en dat moet expliciet: een slot heeft geen
	// loopback (de gVisor-stack krijgt één NIC met het slot-IP), dus
	// cloudflared's default "localhost:0" is onbindbaar — en de
	// virtual-variant "0.0.0.0:0" óók: die geeft "bad local address", want de
	// stack wil een concreet adres én een concrete poort. Gemeten in QEMU 31-07;
	// dit was de fout die het hele ding deed omvallen (exit code 1, en de reden
	// was onzichtbaar tot cli.ErrWriter naar de logring ging).
	//
	// 20241 is cloudflared's eigen eerste keuze uit GetMetricsKnownAddresses.
	DefaultMetricsPort = "20241"
)

// Load leest de config uit de slot-env. env is applib's App.Env; ip is het
// eigen IP uit appnet.Up (nodig omdat de metrics-server een concreet adres
// moet krijgen).
func Load(env func(string) string, ip string) Config {
	c := Config{
		Token:    strings.TrimSpace(env("TUNNEL_TOKEN")),
		URL:      strings.TrimSpace(env("TUNNEL_URL")),
		Protocol: strings.TrimSpace(env("TUNNEL_TRANSPORT_PROTOCOL")),
		LogLevel: strings.TrimSpace(env("TUNNEL_LOGLEVEL")),
		Metrics:  strings.TrimSpace(env("TUNNEL_METRICS")),
		Extra:    strings.Fields(env("CFD_EXTRA_ARGS")),
	}
	if c.Metrics == "" && ip != "" {
		// Publiceert de jobspec een poort onder de naam "metrics", dan zet HOP
		// ER_PORT_METRICS en is dat de poort die van buiten open staat — bind
		// dié, precies zoals elke andere HopOS-app doet. Anders 20241, en dan is
		// /ready en /metrics alleen op het interne net te halen.
		port := strings.TrimSpace(env("ER_PORT_METRICS"))
		if port == "" {
			port = DefaultMetricsPort
		}
		c.Metrics = ip + ":" + port
	}
	if c.Protocol == "" {
		c.Protocol = DefaultProtocol
	}
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	// Zonder expliciete URL: de gepubliceerde poort 80 op het node-IP. Dat is
	// het adres dat óók van binnenuit werkt (hairpin, HopOS ≥v1.5.4), dus in de
	// praktijk de welcome-pagina van deze node.
	if c.URL == "" {
		if host := strings.TrimSpace(env("HOPOS_HOST")); host != "" {
			c.URL = "http://" + host
		}
	}
	return c
}

// Named zegt of dit een named tunnel is (token) of een quick tunnel.
func (c Config) Named() bool { return c.Token != "" }

// Mode is de modusnaam voor logregels.
func (c Config) Mode() string {
	if c.Named() {
		return "named"
	}
	return "quick"
}

// ErrNoTarget: een quick tunnel zonder iets om naar te wijzen kan niet.
var ErrNoTarget = errors.New("no target: set TUNNEL_URL (or publish a port so HOPOS_HOST is set), or set TUNNEL_TOKEN for a named tunnel")

// Args bouwt de os.Args waarmee cloudflared's CLI gestart wordt.
//
// --no-autoupdate staat er altijd op: een slot-image dat zichzelf vervangt
// bestaat niet — een nieuwe versie is een nieuwe job met een nieuw artifact.
func (c Config) Args() ([]string, error) {
	switch c.Protocol {
	case "http2", "quic", "auto":
	default:
		return nil, fmt.Errorf("TUNNEL_TRANSPORT_PROTOCOL=%q: want http2, quic or auto", c.Protocol)
	}
	if !c.Named() && c.URL == "" {
		return nil, ErrNoTarget
	}

	args := []string{"cloudflared", "tunnel", "--no-autoupdate",
		"--loglevel", c.LogLevel, "--protocol", c.Protocol}
	if c.Metrics != "" {
		args = append(args, "--metrics", c.Metrics)
	}
	if c.Named() {
		// De ingress van een named tunnel komt uit het dashboard; een --url
		// erbij zou suggereren dat wij hem bepalen.
		args = append(args, "run", "--token", c.Token)
	} else {
		args = append(args, "--url", c.URL)
	}
	return append(args, c.Extra...), nil
}

// Banner is de regel die de app logt bij het starten. Zonder het token: dat is
// een credential, en logs zijn niet vertrouwelijk.
func (c Config) Banner(version, ip string) string {
	target := c.URL
	if c.Named() {
		target = "ingress from the Cloudflare dashboard"
	}
	return fmt.Sprintf("cloudflared %s: %s tunnel over %s, own IP %s, exposing %s",
		version, c.Mode(), c.Protocol, ip, target)
}

// Bridge zet de doorgegeven env-namen in de proces-env, zodat cloudflared's
// eigen env-flags werken. Geeft terug welke namen gezet zijn (voor de logregel)
// — nooit de waarden, want TUNNEL_TOKEN zit ertussen.
func Bridge(env func(string) string, set func(string, string) error) []string {
	var done []string
	for _, name := range Bridged {
		v := env(name)
		if v == "" {
			continue
		}
		if err := set(name, v); err == nil {
			done = append(done, name)
		}
	}
	return done
}
