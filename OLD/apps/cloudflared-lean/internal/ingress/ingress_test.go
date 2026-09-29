package ingress

import "testing"

// De vier verschillen met een gewone mux, elk met een eigen toets. Dit is de
// reden dat dit pakket bestaat, dus dit is waar het bewijs hoort te staan.
func TestOrderedFirstMatchWins(t *testing.T) {
	s := New("http://val:80")
	_, err := s.Update(1, []byte(`{"ingress":[
		{"hostname":"demo.example.com","path":"^/api","service":"http://api:8080"},
		{"hostname":"demo.example.com","service":"http://web:80"},
		{"service":"http://val:80"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ host, path, want string }{
		// De eerste passende regel wint — niet de meest specifieke, en niet de
		// langste. Een mux zou hier anders kiezen.
		{"demo.example.com", "/api/v1/devices", "http://api:8080"},
		{"demo.example.com", "/", "http://web:80"},
		{"iets.anders.nl", "/api", "http://val:80"},
	} {
		got, ok := s.Route(c.host, c.path)
		if !ok || got != c.want {
			t.Errorf("%s%s -> %q (ok=%v), wil %q", c.host, c.path, got, ok, c.want)
		}
	}
}

func TestHostnameGlob(t *testing.T) {
	s := New("http://val:80")
	if _, err := s.Update(1, []byte(`{"ingress":[
		{"hostname":"*.example.com","service":"http://wild:80"},
		{"hostname":"exact.nl","service":"http://exact:80"},
		{"service":"http://val:80"}]}`)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ host, want string }{
		{"a.example.com", "http://wild:80"},
		{"diep.genest.example.com", "http://wild:80"},
		{"example.com", "http://val:80"}, // *.example.com dekt de basis NIET
		{"EXACT.NL", "http://exact:80"},  // hostnamen zijn niet kast-gevoelig
		{"nietexact.nl", "http://val:80"},
	} {
		got, _ := s.Route(c.host, "/")
		if got != c.want {
			t.Errorf("%s -> %q, wil %q", c.host, got, c.want)
		}
	}
}

// Het pad is een REGEX, geen voorvoegsel. Wie dat verwart routeert stil verkeerd.
func TestPathIsRegex(t *testing.T) {
	s := New("http://val:80")
	if _, err := s.Update(1, []byte(`{"ingress":[
		{"hostname":"h","path":"\\.(jpg|png)$","service":"http://beeld:80"},
		{"hostname":"h","service":"http://web:80"}]}`)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{"/camera/voordeur.jpg", "http://beeld:80"},
		{"/logo.png", "http://beeld:80"},
		{"/jpg/index.html", "http://web:80"}, // een voorvoegsel-mux zou hier fout gaan
	} {
		if got, _ := s.Route("h", c.path); got != c.want {
			t.Errorf("%s -> %q, wil %q", c.path, got, c.want)
		}
	}
}

// Ouder of gelijk wordt genegeerd: een late push van een andere verbinding mag
// een nieuwere tabel niet terugdraaien.
func TestVersionMonotonic(t *testing.T) {
	s := New("http://val:80")
	if _, err := s.Update(5, []byte(`{"ingress":[{"service":"http://nieuw:80"}]}`)); err != nil {
		t.Fatal(err)
	}
	applied, err := s.Update(3, []byte(`{"ingress":[{"service":"http://oud:80"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if applied != 5 {
		t.Errorf("toegepaste versie = %d, wil 5", applied)
	}
	if got, _ := s.Route("h", "/"); got != "http://nieuw:80" {
		t.Errorf("oudere push overschreef de tabel: %q", got)
	}
}

// Een kapotte regel laat de HELE oude tabel staan: half toepassen zou betekenen
// dat het dashboard iets anders zegt dan er draait.
func TestBadRuleKeepsOldTable(t *testing.T) {
	s := New("http://val:80")
	if _, err := s.Update(1, []byte(`{"ingress":[{"service":"http://goed:80"}]}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"ingress":[{"path":"(ongeldig","service":"http://x:80"}]}`,
		`{"ingress":[{"hostname":"h"}]}`, // geen dienst
		`{"ingress":[]}`,
		`niet json`,
	} {
		if _, err := s.Update(2, []byte(bad)); err == nil {
			t.Errorf("geen fout op %q", bad)
		}
		if got, _ := s.Route("h", "/"); got != "http://goed:80" {
			t.Errorf("tabel veranderde na een kapotte push: %q", got)
		}
		if v := s.Table().Version; v != 1 {
			t.Errorf("versie schoof op naar %d na een kapotte push", v)
		}
	}
}

// Vóór de eerste push draait de tunnel op de fallback: anders zit er een gat
// tussen "verbonden" en "geconfigureerd" waarin bezoekers een fout krijgen.
func TestFallbackBeforeFirstPush(t *testing.T) {
	s := New("http://stulp:80")
	got, ok := s.Route("wat.dan.ook", "/pad")
	if !ok || got != "http://stulp:80" {
		t.Errorf("fallback = %q (ok=%v)", got, ok)
	}
	if lines := s.Describe(); len(lines) != 1 || lines[0] != "* -> http://stulp:80" {
		t.Errorf("Describe = %v", lines)
	}
}
