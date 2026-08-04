// De persistente storage-kant van de hop-ABI: de store-ops (OpStorePull/
// Push/List/Drop) kopiëren expliciet tussen de eigen map van de JOB in de
// object-store (apps/<cluster>/<job>/ — HOP dwingt de prefix af, de app kan
// er niet buiten) en het eigen hopfs-zicht van de task. Bewust een kopie op
// afroep en géén synchronisatie: een sync-daemon belooft persistentie die er
// tussen twee uploads niet is, en dat verlies-window zou onzichtbaar zijn.
// Pull bij start, push wanneer het bewaard moet zijn — persistentie is een
// daad van de app.
//
// De bytes lopen over HOP (core 0): dáár wonen de S3-credentials en de TLS
// al (dezelfde signer als de committed state). Dit is geen heropvoering van
// de gesloopte fetch-op — de app kiest geen URL en geen bucket, alleen een
// naam binnen zijn eigen map; het endpoint is operator-config. Wie bulk wil
// streamen hoort niet op dit pad thuis (de servicer van dit slot is er zo
// lang mee bezig; andere slots merken er niets van).
package slots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/xinix00/HopOS/metal/abi/hopabi"
)

// ObjectStore is wat de node-kant (cmd/hopos) aan slots geeft: de vier
// object-ops op de geconfigureerde bucket, met VOLLEDIGE keys — de
// prefix-opbouw (en dus de toegangsgrens) blijft hier in het slots-pakket.
// De ctx is de levenslijn van de servicer: evict cancelt hem, zodat een
// trage upload een Stop→Start nooit minutenlang ophoudt.
type ObjectStore interface {
	// Pull streamt het object naar w; found=false als het niet bestaat
	// (geen fout, en w is dan onaangeraakt).
	Pull(ctx context.Context, key string, w io.Writer) (n int64, found bool, err error)
	// Push uploadt size bytes uit r als het object; sha256Hex is de hex-hash
	// van precies die bytes (de signer tekent de payload-hash).
	Push(ctx context.Context, key string, size int64, sha256Hex string, r io.Reader) error
	// List geeft alle keys onder prefix; truncated=true als de cap van de
	// implementatie de lijst afkapte (de aanroeper maakt daar een luide fout
	// van — stilletjes inkorten leest als "dit is alles" terwijl het dat niet is).
	List(ctx context.Context, prefix string) (keys []string, truncated bool, err error)
	// Drop verwijdert het object (idempotent).
	Drop(ctx context.Context, key string) error
}

// store is de object-store van deze node (nil = niet geconfigureerd) en
// storeBase de naamruimte in de bucket ("apps/<cluster>" — naast de
// "leases/<cluster>" en "state/<cluster>" van de clusterstaat).
var (
	store     ObjectStore
	storeBase string
)

// UseStore koppelt de object-store (eenmalig bij boot, net als UseFS).
func UseStore(s ObjectStore, base string) {
	store = s
	storeBase = strings.TrimSuffix(base, "/")
}

// storeFS is het stukje hopfs dat de store-ops raken — als interface, zodat
// de pull/push-mechaniek op de host testbaar is zonder NVMe (*hopfs.FS
// voldoet er ongewijzigd aan).
type storeFS interface {
	Stat(path string) (uint64, bool, error)
	ReadAt(path string, off uint64, p []byte) (int, error)
	WriteAt(path string, off uint64, p []byte) error
	Truncate(path string, size uint64) error
}

// storeKey bouwt de volledige bucket-key voor pad cp ("/a/b", al door
// cleanAbs) binnen de eigen map van de job — dít is de toegangsgrens op de
// bucket, de spiegel van resolve() op hopfs.
func (s *servicer) storeKey(cp string) string {
	return storeBase + "/" + s.job + cp
}

// storeGate is de gedeelde poort van de vier store-ops: store aanwezig,
// task heeft een job-identiteit, pad is schoon. Geeft het schone pad terug.
func (s *servicer) storeGate(path string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("no object store configured on this node (boot with hopos.s3.*)")
	}
	if s.job == "" {
		return "", fmt.Errorf("task has no job identity — the object store is only available to jobs")
	}
	if strings.ContainsAny(s.job, "/\\") {
		return "", fmt.Errorf("job name %q cannot form a store namespace", s.job)
	}
	return cleanAbs(path)
}

// fsWriter schrijft een binnenstromend object naar een hopfs-pad, op
// oplopende offset. De eerste write kort het bestand éérst in
// (replace-semantiek, dezelfde als WriteFile aan de app-kant): een pull
// laat nooit de staart van een oudere, langere versie staan. Vóór de
// eerste byte blijft het bestaande bestand intact — een mislukte GET
// raakt lokaal niets aan. Een fout halverwege laat een korter bestand
// achter én een fout bij de app: POSIX-achtig, zoals alle hopfs-writes.
type fsWriter struct {
	fs     storeFS
	path   string
	off    uint64
	virgin bool
}

func (w *fsWriter) Write(p []byte) (int, error) {
	if w.virgin {
		if err := w.fs.Truncate(w.path, 0); err != nil {
			return 0, err
		}
		w.virgin = false
	}
	if err := w.fs.WriteAt(w.path, w.off, p); err != nil {
		return 0, err
	}
	w.off += uint64(len(p))
	return len(p), nil
}

// fsReader streamt een hopfs-bestand als io.Reader (voor de upload).
type fsReader struct {
	fs   storeFS
	path string
	off  uint64
}

func (r *fsReader) Read(p []byte) (int, error) {
	n, err := r.fs.ReadAt(r.path, r.off, p)
	r.off += uint64(n)
	if err != nil {
		return n, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// hashFile is de eerste pass van een push: de sha256 over precies size
// bytes. De bron is lokale NVMe, dus twee keer lezen is goedkoop — en de
// signer MOET de payload-hash vooraf kennen (bewust geen streaming-signing).
func hashFile(f storeFS, path string, size uint64) (string, error) {
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for off := uint64(0); off < size; {
		want := buf
		if rem := size - off; rem < uint64(len(buf)) {
			want = buf[:rem]
		}
		n, err := f.ReadAt(path, off, want)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", fmt.Errorf("file shrank during hashing (%d of %d bytes)", off, size)
		}
		h.Write(want[:n])
		off += uint64(n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// storePull haalt object <pad> uit de eigen map en vervangt er het lokale
// pad mee (mount-bewust via resolve, net als elke fs-op).
func (s *servicer) storePull(f storeFS, req hopabi.Req) []byte {
	cp, err := s.storeGate(req.Path)
	if err != nil {
		return fail(req, err)
	}
	rp, err := s.resolve(req.Path)
	if err != nil {
		return fail(req, err)
	}
	w := &fsWriter{fs: f, path: rp, virgin: true}
	n, found, err := store.Pull(s.ctx, s.storeKey(cp), w)
	if err != nil {
		return fail(req, err)
	}
	if !found {
		return failWith(req, hopabi.StatusNoEnt, "no such object: "+cp)
	}
	// Een leeg object heeft nooit een Write gedaan: de vervanging (en het
	// aanmaken van het bestand) alsnog uitvoeren.
	if w.virgin {
		if err := f.Truncate(rp, 0); err != nil {
			return fail(req, err)
		}
	}
	return ok(req, uint64(n), nil)
}

// storePush uploadt het lokale pad naar object <pad> in de eigen map. De
// inhoud is die van het hash-moment: schrijft de app er tegelijk doorheen,
// dan verwerpt de store de upload (hash/lengte-mismatch) — een gescheurde
// push is een luide fout, nooit stil een corrupt object.
func (s *servicer) storePush(f storeFS, req hopabi.Req) []byte {
	cp, err := s.storeGate(req.Path)
	if err != nil {
		return fail(req, err)
	}
	rp, err := s.resolve(req.Path)
	if err != nil {
		return fail(req, err)
	}
	size, isDir, err := f.Stat(rp)
	if err != nil {
		return fail(req, err)
	}
	if isDir {
		return fail(req, fmt.Errorf("%q is a directory", req.Path))
	}
	sum, err := hashFile(f, rp, size)
	if err != nil {
		return fail(req, err)
	}
	if err := store.Push(s.ctx, s.storeKey(cp), int64(size), sum, &fsReader{fs: f, path: rp}); err != nil {
		return fail(req, err)
	}
	return ok(req, size, nil)
}

// storeList geeft de keys in de eigen map onder pad-prefix <pad>, relatief
// aan de eigen map — zodat een naam uit List rechtstreeks aan Pull te
// voeren is. De match is de tekstuele prefix-match van de store zelf (het
// is een object-store, geen dir-boom): "db" matcht óók "dbx.json".
func (s *servicer) storeList(req hopabi.Req) []byte {
	cp, err := s.storeGate(req.Path)
	if err != nil {
		return fail(req, err)
	}
	own := storeBase + "/" + s.job + "/"
	prefix := own
	if cp != "/" {
		prefix = s.storeKey(cp)
	}
	keys, truncated, err := store.List(s.ctx, prefix)
	if err != nil {
		return fail(req, err)
	}
	if truncated {
		return fail(req, fmt.Errorf("list %q: too many objects for one response — use a narrower prefix", req.Path))
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = strings.TrimPrefix(k, own)
	}
	return listResp(req, names)
}

// storeDrop verwijdert object <pad> uit de eigen map (idempotent, zoals de
// store zelf).
func (s *servicer) storeDrop(req hopabi.Req) []byte {
	cp, err := s.storeGate(req.Path)
	if err != nil {
		return fail(req, err)
	}
	if err := store.Drop(s.ctx, s.storeKey(cp)); err != nil {
		return fail(req, err)
	}
	return ok(req, 0, nil)
}
