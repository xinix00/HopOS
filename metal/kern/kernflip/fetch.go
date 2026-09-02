//go:build tamago

package kernflip

// De bundel binnenhalen en controleren.
//
// Eén controle: de SHA-256 die in de platform-config van DEZE node staat. Geen
// handtekening, en dat is een bewuste keuze (Derek, 01-09): de sleutel zou in
// dezelfde repo wonen als de release zelf, dus wie bij het een kan komen kan
// ook bij het ander — een handtekening dekt dan geen aanval die de som niet al
// dekt. Wat de som wél dekt is precies het gevaar dat er is: een webserver, CDN
// of mirror die iets anders serveert dan wat er gepubliceerd is.
//
// Het vertrouwensanker is dus het bootmedium: de som komt uit de config die jij
// op de kaart zet, naast de URL. Dat is dezelfde keten als de release-keten
// (tools/release.sh publiceert SHA256SUMS) en dezelfde eenvoud als de rest van
// dit systeem — geen sleutelbeheer, geen tweede bestand naast het image.
//
// Zonder som gebeurt er niets. Een kern van het net is code met alle rechten op
// deze machine; die haal je niet binnen op goed vertrouwen.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// maxBundle is de bovengrens op wat we binnenhalen. Een bundel die groter is
// dan dit hoort een fout te zijn, geen volgelopen geheugen.
const maxBundle = 64 << 20

// fetchBundle haalt url op en geeft de bytes terug als de SHA-256 klopt met
// want (hex, hoofdletterongevoelig). Elke fout is een reden om niet te
// springen.
func fetchBundle(url, want string) ([]byte, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return nil, fmt.Errorf("kernflip: %s configured without hopos.flip.sha256 — refusing to boot an unverified kernel", url)
	}
	if len(want) != 64 {
		return nil, fmt.Errorf("kernflip: hopos.flip.sha256 is %d characters, want 64 hex", len(want))
	}

	// Eén verbinding, meteen opgeruimd: dit pad draait één keer, en een
	// netstack-budget vasthouden voor een client die klaar is, is precies wat
	// een node met weinig RAM niet moet doen.
	cl := &leanhttp.Client{IdleTimeout: 5 * time.Second}
	defer cl.CloseIdle()
	resp, err := cl.Get(url)
	if err != nil {
		return nil, fmt.Errorf("kernflip: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("kernflip: %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBundle+1))
	if err != nil {
		return nil, fmt.Errorf("kernflip: %s: %w", url, err)
	}
	if len(b) > maxBundle {
		return nil, fmt.Errorf("kernflip: %s is larger than %d bytes", url, maxBundle)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("kernflip: %s is sha256 %s, config says %s — not booting it", url, got, want)
	}
	return b, nil
}
