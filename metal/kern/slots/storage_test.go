package slots

// Host-tests voor de store-mechaniek (storage.go): de prefix-grens, de
// replace-semantiek van een pull (incl. het lege object en het 404-pad) en
// de hash-dan-streamen-volgorde van een push — op een in-memory fake van
// het hopfs-stukje (storeFS) en een fake ObjectStore. De echte NVMe/S3-
// kanten hebben hun eigen bewijs (hopfs op ijzer, hoplock's httptest-suite).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
)

// memFS is een minimale storeFS: hele bestanden in een map.
type memFS struct{ files map[string][]byte }

func newMemFS() *memFS { return &memFS{files: map[string][]byte{}} }

var errNoEntMem = fmt.Errorf("memfs: bestaat niet")

func (m *memFS) Stat(path string) (uint64, bool, error) {
	b, ok := m.files[path]
	if !ok {
		return 0, false, errNoEntMem
	}
	return uint64(len(b)), false, nil
}

func (m *memFS) ReadAt(path string, off uint64, p []byte) (int, error) {
	b, ok := m.files[path]
	if !ok {
		return 0, errNoEntMem
	}
	if off >= uint64(len(b)) {
		return 0, nil
	}
	return copy(p, b[off:]), nil
}

func (m *memFS) WriteAt(path string, off uint64, p []byte) error {
	b := m.files[path]
	if need := off + uint64(len(p)); uint64(len(b)) < need {
		nb := make([]byte, need)
		copy(nb, b)
		b = nb
	}
	copy(b[off:], p)
	m.files[path] = b
	return nil
}

func (m *memFS) Truncate(path string, size uint64) error {
	b := m.files[path]
	if uint64(len(b)) >= size {
		m.files[path] = b[:size]
		return nil
	}
	nb := make([]byte, size)
	copy(nb, b)
	m.files[path] = nb
	return nil
}

// memStore is een fake ObjectStore die keys, payloads en de aangeleverde
// push-hash vastlegt.
type memStore struct {
	objects  map[string][]byte
	pushHash map[string]string
	pushes   int
}

func newMemStore() *memStore {
	return &memStore{objects: map[string][]byte{}, pushHash: map[string]string{}}
}

func (s *memStore) Pull(_ context.Context, key string, w io.Writer) (int64, bool, error) {
	b, ok := s.objects[key]
	if !ok {
		return 0, false, nil
	}
	n, err := w.Write(b)
	return int64(n), true, err
}

func (s *memStore) Push(_ context.Context, key string, size int64, sha256Hex string, r io.Reader) error {
	s.pushes++
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size mismatch: got %d, want %d", len(b), size)
	}
	s.objects[key] = b
	s.pushHash[key] = sha256Hex
	return nil
}

func (s *memStore) List(_ context.Context, prefix string) ([]string, bool, error) {
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, false, nil
}

func (s *memStore) Drop(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

// storeEnv zet de package-globals (store, storeBase) voor één test en geeft
// een kale servicer terug — zonder ring of goroutine; de handler-logica
// (storeGate + resolve + storeKey) heeft daar niets van nodig.
func storeEnv(t *testing.T, st ObjectStore) *servicer {
	t.Helper()
	oldStore, oldBase := store, storeBase
	t.Cleanup(func() { store, storeBase = oldStore, oldBase })
	store, storeBase = st, "apps/testcluster"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &servicer{slot: 1, root: "/.tasks/slot1", job: "job1", ctx: ctx, cancel: cancel}
}

func decodeResp(t *testing.T, b []byte) hopabi.Resp {
	t.Helper()
	r, err := hopabi.DecodeResp(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStoreKeyPrefixIsEnforced(t *testing.T) {
	s := storeEnv(t, newMemStore())
	if got := s.storeKey("/config.json"); got != "apps/testcluster/job1/config.json" {
		t.Fatalf("storeKey = %q", got)
	}
	if got := s.storeKey("/db/users.json"); got != "apps/testcluster/job1/db/users.json" {
		t.Fatalf("storeKey diep pad = %q", got)
	}
}

func TestStoreGateRejectsEscapes(t *testing.T) {
	s := storeEnv(t, newMemStore())

	if _, err := s.storeGate("../other/x"); err == nil {
		t.Fatal("'..' door de poort")
	}
	s.job = ""
	if _, err := s.storeGate("/x"); err == nil {
		t.Fatal("lege job-identiteit door de poort")
	}
	s.job = "a/b"
	if _, err := s.storeGate("/x"); err == nil {
		t.Fatal("job met '/' door de poort (naamruimte-ontsnapping)")
	}
	s.job = "job1"
	store = nil
	if _, err := s.storeGate("/x"); err == nil {
		t.Fatal("nil store door de poort")
	}
}

// Een pull vervangt het lokale bestand volledig: ook als de oude inhoud
// lánger was, blijft er geen staart staan.
func TestStorePullReplacesLocalFile(t *testing.T) {
	st := newMemStore()
	st.objects["apps/testcluster/job1/f"] = []byte("nieuw")
	s := storeEnv(t, st)
	fs := newMemFS()
	fs.files["/.tasks/slot1/f"] = []byte("oude-veel-langere-inhoud")

	resp := decodeResp(t, s.storePull(fs, hopabi.Req{Op: hopabi.OpStorePull, Path: "f"}))
	if resp.Status != hopabi.StatusOK {
		t.Fatalf("pull faalt: %s", resp.Data)
	}
	if resp.Size != 5 {
		t.Fatalf("size %d, wil 5", resp.Size)
	}
	if got := string(fs.files["/.tasks/slot1/f"]); got != "nieuw" {
		t.Fatalf("lokaal bestand na pull: %q", got)
	}
}

// Een leeg object vervangt óók: het lokale bestand wordt 0 bytes (en wordt
// aangemaakt als het er nog niet was).
func TestStorePullEmptyObjectTruncates(t *testing.T) {
	st := newMemStore()
	st.objects["apps/testcluster/job1/leeg"] = nil
	s := storeEnv(t, st)
	fs := newMemFS()
	fs.files["/.tasks/slot1/leeg"] = []byte("weg hiermee")

	resp := decodeResp(t, s.storePull(fs, hopabi.Req{Op: hopabi.OpStorePull, Path: "leeg"}))
	if resp.Status != hopabi.StatusOK {
		t.Fatalf("pull faalt: %s", resp.Data)
	}
	if got, ok := fs.files["/.tasks/slot1/leeg"]; !ok || len(got) != 0 {
		t.Fatalf("lokaal bestand na lege pull: %q (ok=%v)", got, ok)
	}
}

// Een object dat er niet is: StatusNoEnt én het lokale bestand onaangeraakt.
func TestStorePullAbsentLeavesLocalIntact(t *testing.T) {
	s := storeEnv(t, newMemStore())
	fs := newMemFS()
	fs.files["/.tasks/slot1/f"] = []byte("blijf af")

	resp := decodeResp(t, s.storePull(fs, hopabi.Req{Op: hopabi.OpStorePull, Path: "f"}))
	if resp.Status != hopabi.StatusNoEnt {
		t.Fatalf("status %d, wil NoEnt", resp.Status)
	}
	if got := string(fs.files["/.tasks/slot1/f"]); got != "blijf af" {
		t.Fatalf("404-pull raakte het lokale bestand aan: %q", got)
	}
}

// Push levert het object onder de eigen key af, met de hash van precies de
// verstuurde bytes.
func TestStorePushRoundtrip(t *testing.T) {
	st := newMemStore()
	s := storeEnv(t, st)
	fs := newMemFS()
	content := bytes.Repeat([]byte("data!"), 40000) // 200KB: meer dan één hash-chunk
	fs.files["/.tasks/slot1/db/dump.bin"] = content

	resp := decodeResp(t, s.storePush(fs, hopabi.Req{Op: hopabi.OpStorePush, Path: "/db/dump.bin"}))
	if resp.Status != hopabi.StatusOK {
		t.Fatalf("push faalt: %s", resp.Data)
	}
	if resp.Size != uint64(len(content)) {
		t.Fatalf("size %d, wil %d", resp.Size, len(content))
	}
	key := "apps/testcluster/job1/db/dump.bin"
	if !bytes.Equal(st.objects[key], content) {
		t.Fatalf("object onder %q wijkt af (%d bytes)", key, len(st.objects[key]))
	}
	sum := sha256.Sum256(content)
	if st.pushHash[key] != hex.EncodeToString(sum[:]) {
		t.Fatalf("push-hash %s dekt de inhoud niet", st.pushHash[key])
	}
}

// De List-namen zijn relatief aan de eigen map (List → Pull werkt direct)
// en de map van een ándere job lekt er nooit in.
func TestStoreListNamesAreRelative(t *testing.T) {
	st := newMemStore()
	st.objects["apps/testcluster/job1/a.txt"] = []byte("x")
	st.objects["apps/testcluster/job1/db/u.json"] = []byte("y")
	st.objects["apps/testcluster/other/geheim.txt"] = []byte("z")
	s := storeEnv(t, st)

	resp := decodeResp(t, s.storeList(hopabi.Req{Op: hopabi.OpStoreList, Path: ""}))
	if resp.Status != hopabi.StatusOK {
		t.Fatalf("list faalt: %s", resp.Data)
	}
	names := strings.Split(string(resp.Data), "\n")
	if len(names) != 2 {
		t.Fatalf("names = %v (lekt de map van 'other'?)", names)
	}
	for _, n := range names {
		if n != "a.txt" && n != "db/u.json" {
			t.Fatalf("onverwachte naam %q", n)
		}
	}
}

func TestStoreDropScopedToOwnMap(t *testing.T) {
	st := newMemStore()
	st.objects["apps/testcluster/job1/x"] = []byte("1")
	st.objects["apps/testcluster/other/x"] = []byte("2")
	s := storeEnv(t, st)

	resp := decodeResp(t, s.storeDrop(hopabi.Req{Op: hopabi.OpStoreDrop, Path: "x"}))
	if resp.Status != hopabi.StatusOK {
		t.Fatalf("drop faalt: %s", resp.Data)
	}
	if _, ok := st.objects["apps/testcluster/job1/x"]; ok {
		t.Fatal("eigen object niet verwijderd")
	}
	if _, ok := st.objects["apps/testcluster/other/x"]; !ok {
		t.Fatal("drop raakte de map van een ander")
	}
}

func TestFsReaderStreamsWholeFile(t *testing.T) {
	fs := newMemFS()
	want := bytes.Repeat([]byte("0123456789"), 1000)
	fs.files["/big"] = want

	got, err := io.ReadAll(&fsReader{fs: fs, path: "/big"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("gelezen %d bytes, wil %d", len(got), len(want))
	}
}

func TestHashFileMatchesContent(t *testing.T) {
	fs := newMemFS()
	content := []byte("hello hop")
	fs.files["/f"] = content
	sum := sha256.Sum256(content)
	got, err := hashFile(context.Background(), fs, "/f", uint64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash %s dekt de inhoud niet", got)
	}
}

type cancelAfterReadFS struct {
	*memFS
	cancel context.CancelFunc
	reads  int
}

func (f *cancelAfterReadFS) ReadAt(path string, off uint64, p []byte) (int, error) {
	n, err := f.memFS.ReadAt(path, off, p)
	f.reads++
	if f.reads == 1 {
		f.cancel()
	}
	return n, err
}

// Stop/evict cancelt s.ctx. Ook als dat midden in de lokale hashpass gebeurt,
// mag StorePush niet de rest van het bestand lezen en de upload niet beginnen.
func TestStorePushCancellationStopsHashBeforeUpload(t *testing.T) {
	st := newMemStore()
	s := storeEnv(t, st)
	base := newMemFS()
	content := bytes.Repeat([]byte("x"), 3*(64<<10))
	base.files["/.tasks/slot1/big"] = content
	fs := &cancelAfterReadFS{memFS: base, cancel: s.cancel}

	resp := decodeResp(t, s.storePush(fs, hopabi.Req{Op: hopabi.OpStorePush, Path: "/big"}))
	if resp.Status == hopabi.StatusOK {
		t.Fatal("geannuleerde hashpass rapporteerde succes")
	}
	if !strings.Contains(string(resp.Data), context.Canceled.Error()) {
		t.Fatalf("fout = %q, wil context cancellation", resp.Data)
	}
	if fs.reads != 1 {
		t.Fatalf("na cancellation nog doorgelezen: %d reads, wil 1", fs.reads)
	}
	if st.pushes != 0 {
		t.Fatalf("upload begon ondanks geannuleerde hashpass: %d Push-calls", st.pushes)
	}
}
