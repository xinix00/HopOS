// Package netboot haalt de volgende kern van het net en springt erin.
//
// DIT IS BRING-UP-GEREEDSCHAP, GEEN UPDATE-MECHANISME. Lees dat eerst, want
// het bepaalt wat hier wel en niet in hoort.
//
// Waarom het bestaat: op een board waar wij zelf het bootobject zijn (Apple
// silicon — kmutil registreert ons image in de LocalPolicy) kost élke wijziging
// een fysieke reis naar de machine. 1TR is een GUI, en macOS boot niet meer
// omdat dat volume óns start. Meten werd gratis en veranderen duur, en dat is
// de verkeerde kant op als je aan het porten bent.
//
// Waarom het NIET de update-weg voor een vloot is (Derek, 30-08): één verkeerde
// URL of één slecht image en élke node trekt hem binnen. Dat is precies de
// centrale storing waar een node-OS niet van afhankelijk mag zijn — een vloot
// die zichzelf automatisch van het net vernieuwt, is een vloot die je in één
// keer kwijt kunt zijn. Een echte update-weg hoort per node te worden gewild,
// stapsgewijs uitgerold en terug te draaien; niets van dat alles staat hier.
//
// Vandaar: standaard UIT (geen URL = niets gebeurt), één URL uit de
// platform-config van dié node, handtekening verplicht, en bij twijfel gewoon
// doorbooten op wat er geïnstalleerd staat. Zet het aan op je meetbank, niet
// op productie.
//
// HET VERTROUWENSMODEL. Een kern van het net is code met alle rechten, dus hij
// wordt geverifieerd vóór de sprong: ed25519 over de hele image, met de
// publieke sleutel uit de config (of ingebakken). Faalt dat, dan springen we
// niet — en zeggen we luid waarom. Dat is dezelfde afspraak als bij de
// release-images (tools/release.sh tekent SHA256SUMS), alleen dan op de node.
package netboot

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// Config is wat de platform-config over netboot zegt.
type Config struct {
	// URL van het image (mkkernel-uitvoer voor dit board). Leeg = uit.
	URL string
	// PubKey is de ed25519-sleutel die de handtekening moet dragen,
	// base64 (raw, 32 bytes). Leeg = we springen NIET; ongetekend booten van
	// het net is precies hoe je een node kwijtraakt.
	PubKey string
	// SigURL is waar de handtekening staat; leeg = URL + ".sig".
	SigURL string
	// Max is de bovengrens op wat we binnenhalen (0 = 64MB). Een kern die
	// groter is dan dit hoort een fout te zijn, geen volgelopen geheugen.
	Max int64
}

// Fetch haalt image en handtekening op en geeft de bytes terug als de
// handtekening klopt. Elke fout is hier een reden om NIET te springen.
func Fetch(cfg Config) ([]byte, error) {
	if cfg.URL == "" {
		return nil, nil // uit; geen fout
	}
	if cfg.PubKey == "" {
		return nil, fmt.Errorf("netboot: %s configured without a public key — refusing to boot unsigned code", cfg.URL)
	}
	key, err := base64.StdEncoding.DecodeString(cfg.PubKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("netboot: public key is not 32 raw bytes in base64 (%d bytes, %v)", len(key), err)
	}
	max := cfg.Max
	if max <= 0 {
		max = 64 << 20
	}
	sigURL := cfg.SigURL
	if sigURL == "" {
		sigURL = cfg.URL + ".sig"
	}

	// Handtekening eerst: klein, en als die er niet is hoeven we de kern niet
	// eens op te halen.
	sig, err := get(sigURL, ed25519.SignatureSize)
	if err != nil {
		return nil, fmt.Errorf("netboot: signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("netboot: signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	img, err := get(cfg.URL, max)
	if err != nil {
		return nil, fmt.Errorf("netboot: image: %w", err)
	}
	if !ed25519.Verify(key, img, sig) {
		return nil, fmt.Errorf("netboot: signature does not match %s (%d bytes) — not booting it", cfg.URL, len(img))
	}
	return img, nil
}

// get haalt één URL op, met een harde bovengrens op wat we accepteren.
func get(url string, max int64) ([]byte, error) {
	// Eén verbinding per fetch en meteen weer opruimen: dit pad draait één
	// keer bij de boot, en een netstack-budget vasthouden voor een client die
	// klaar is, is precies wat een node met 64MB niet moet doen.
	cl := &leanhttp.Client{IdleTimeout: 5 * time.Second}
	defer cl.CloseIdle()
	resp, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, max)
	}
	return b, nil
}
