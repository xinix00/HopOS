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
	"hash/fnv"
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
// fetchBundleInto streams to storage reserved by the caller after the length
// is checked. Its working memory is fixed, independent of the bundle size.
func fetchBundleInto(url, want string, reserve func(int64) (io.Writer, error)) (int64, uint64, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	if len(want) != 64 {
		return 0, 0, fmt.Errorf("kernflip: expected a 64-character SHA-256")
	}
	if _, err := hex.DecodeString(want); err != nil {
		return 0, 0, fmt.Errorf("kernflip: invalid SHA-256: %w", err)
	}
	cl := &leanhttp.Client{IdleTimeout: 5 * time.Second}
	defer cl.CloseIdle()
	resp, err := cl.Get(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("kernflip: HTTP %d", resp.StatusCode)
	}
	length := resp.Length
	if length <= 0 || length > maxBundle {
		return 0, 0, fmt.Errorf("bundle length %d outside 1..%d bytes", length, maxBundle)
	}
	dst, err := reserve(length)
	if err != nil {
		return 0, 0, err
	}
	sha := sha256.New()
	content := fnv.New64a()
	buf := make([]byte, 32<<10)
	for left := length; left > 0; {
		n := int64(len(buf))
		if left < n {
			n = left
		}
		if _, err := io.ReadFull(resp.Body, buf[:n]); err != nil {
			return 0, 0, err
		}
		written, err := dst.Write(buf[:n])
		if err != nil {
			return 0, 0, err
		}
		if int64(written) != n {
			return 0, 0, io.ErrShortWrite
		}
		sha.Write(buf[:n])
		content.Write(buf[:n])
		left -= n
	}
	if got := hex.EncodeToString(sha.Sum(nil)); got != want {
		return 0, 0, fmt.Errorf("kernflip: SHA-256 mismatch: got %s, want %s", got, want)
	}
	return length, content.Sum64(), nil
}
