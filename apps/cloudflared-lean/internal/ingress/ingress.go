// Package ingress is de routeertabel die Cloudflare naar ons duwt.
//
// Waarom geen mux: leanhttp's Mux routeert op pad, kiest de meest specifieke
// route onafhankelijk van registratie-orde, en staat vast zodra Serve loopt.
// Cloudflare doet precies de andere drie dingen — hij routeert eerst op
// hostname (exact of *.suffix), zijn pad is een REGEX, hij neemt de EERSTE
// passende regel, en zijn config komt binnen terwijl we draaien. Vier
// verschillen, dus een eigen tabel; hij is kleiner dan het verschil zou zijn.
//
// De tabel wisselt in zijn geheel achter één pointer. Geen slot in het hete
// pad, en nooit een halve tabel: een verzoek ziet de oude of de nieuwe, nooit
// iets ertussenin.
package ingress

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
)

// Rule is één regel uit de config: waarheen met wat.
type Rule struct {
	Hostname string
	Path     *regexp.Regexp // nil = elk pad
	Service  string         // "http://10.100.0.2:7080", "http_status:404", ...
}

// Matches volgt Cloudflare's eigen regel (ingress/rule.go): een lege hostname
// of "*" matcht alles, "*.example.com" matcht op achtervoegsel, en het pad
// matcht als er geen regex is of als de regex ergens in het pad past.
func (r *Rule) Matches(hostname, path string) bool {
	host := false
	switch {
	case r.Hostname == "" || r.Hostname == "*":
		host = true
	case strings.HasPrefix(r.Hostname, "*."):
		host = strings.HasSuffix(hostname, r.Hostname[1:])
	default:
		host = strings.EqualFold(r.Hostname, hostname)
	}
	if !host {
		return false
	}
	return r.Path == nil || r.Path.MatchString(path)
}

// Table is een momentopname van de regels plus de versie waarmee ze kwamen.
type Table struct {
	Version int32
	Rules   []Rule
}

// Set houdt de levende tabel vast.
type Set struct {
	current atomic.Pointer[Table]
}

// New maakt een set met één regel: alles naar fallback. Zo werkt de tunnel al
// vóór de eerste config-push — anders staat er een gat tussen "verbonden" en
// "geconfigureerd" waarin bezoekers een fout krijgen.
func New(fallback string) *Set {
	s := &Set{}
	s.current.Store(&Table{Version: 0, Rules: []Rule{{Service: fallback}}})
	return s
}

// Table geeft de huidige tabel.
func (s *Set) Table() *Table { return s.current.Load() }

// remoteConfig is de vorm die de edge stuurt (dezelfde als cloudflared's
// newRemoteConfig: een ingress-lijst met hostname, path en service).
type remoteConfig struct {
	Ingress []struct {
		Hostname string `json:"hostname"`
		Path     string `json:"path"`
		Service  string `json:"service"`
	} `json:"ingress"`
}

// Update neemt een config-push aan. Ouder of gelijk aan wat we hebben wordt
// genegeerd — dat is Cloudflare's eigen regel, en het houdt een late push van
// een andere verbinding uit onze tabel.
func (s *Set) Update(version int32, raw []byte) (applied int32, err error) {
	now := s.current.Load()
	if version <= now.Version {
		return now.Version, nil
	}
	var doc remoteConfig
	if err := json.Unmarshal(raw, &doc); err != nil {
		return now.Version, fmt.Errorf("configuration is not readable: %w", err)
	}
	rules := make([]Rule, 0, len(doc.Ingress))
	for i, in := range doc.Ingress {
		r := Rule{Hostname: in.Hostname, Service: in.Service}
		if in.Path != "" {
			re, err := regexp.Compile(in.Path)
			if err != nil {
				// Niet de hele tabel weggooien om één regel: de edge krijgt de
				// fout te horen en houdt de oude tabel. Half toepassen zou
				// betekenen dat het dashboard iets anders zegt dan er draait.
				return now.Version, fmt.Errorf("rule %d has an unusable path %q: %w", i, in.Path, err)
			}
			r.Path = re
		}
		if r.Service == "" {
			return now.Version, fmt.Errorf("rule %d has no service", i)
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return now.Version, fmt.Errorf("configuration carries no ingress rules")
	}
	s.current.Store(&Table{Version: version, Rules: rules})
	return version, nil
}

// Route zoekt de dienst voor een verzoek. found is false als geen regel past;
// dat kan alleen als de laatste regel geen catch-all is.
func (s *Set) Route(hostname, path string) (service string, found bool) {
	t := s.current.Load()
	for i := range t.Rules {
		if t.Rules[i].Matches(hostname, path) {
			return t.Rules[i].Service, true
		}
	}
	return "", false
}

// Describe geeft de tabel als leesbare regels, voor de log bij het opstarten en
// na elke push. Zonder dit is "de tunnel draait" niet te onderscheiden van "de
// tunnel draait en stuurt alles naar de verkeerde plek".
func (s *Set) Describe() []string {
	t := s.current.Load()
	out := make([]string, 0, len(t.Rules))
	for _, r := range t.Rules {
		host := r.Hostname
		if host == "" {
			host = "*"
		}
		path := ""
		if r.Path != nil {
			path = " path " + r.Path.String()
		}
		out = append(out, fmt.Sprintf("%s%s -> %s", host, path, r.Service))
	}
	return out
}
