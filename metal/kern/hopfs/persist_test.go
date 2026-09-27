package hopfs

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Een schijf van 64 MiB in 512B-LBA's: 16384 hopfs-blokken, plekken van 256
// blokken, dus een metagebied van 2 MiB.
func persistDisk() *recordingDisk {
	return &recordingDisk{blockSize: 512, data: make([]byte, 64<<20)}
}

func mountT(t *testing.T, d *recordingDisk, fresh bool) (*FS, string) {
	t.Helper()
	f, msg := mount(d, 0, uint64(len(d.data))/d.blockSize, d.blockSize, 128<<10, fresh)
	if !f.Persistent() {
		t.Fatalf("mount not persistent: %s", msg)
	}
	return f, msg
}

func pattern(n int, seed byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7) + seed
	}
	return p
}

func mustRead(t *testing.T, f *FS, path string, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	n, err := f.ReadAt(path, 0, got)
	if err != nil || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("%s: read %d bytes, err %v, equal %v", path, n, err, bytes.Equal(got[:n], want[:n]))
	}
}

func TestPersistRoundTrip(t *testing.T) {
	d := persistDisk()
	f, msg := mountT(t, d, false)
	if !strings.Contains(msg, "no saved tree") {
		t.Fatalf("empty disk: %q", msg)
	}
	film, sub := pattern(3<<20+123, 1), pattern(5000, 9)
	if err := f.WriteAt("media/Films/A/a.mkv", 0, film); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAt("media/Films/A/a.srt", 0, sub); err != nil {
		t.Fatal(err)
	}
	if err := f.MkdirAll("media/Backups"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("media/sparse", 10<<20); err != nil { // een gat zonder blokken
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	g, msg := mountT(t, d, false)
	if !strings.Contains(msg, "generation 1 restored") {
		t.Fatalf("remount: %q", msg)
	}
	mustRead(t, g, "media/Films/A/a.mkv", film)
	mustRead(t, g, "media/Films/A/a.srt", sub)
	if size, dir, err := g.Stat("media/Backups"); err != nil || !dir || size != 0 {
		t.Fatalf("Backups: %d %v %v", size, dir, err)
	}
	if size, _, err := g.Stat("media/sparse"); err != nil || size != 10<<20 {
		t.Fatalf("sparse: %d %v", size, err)
	}
	if g.nodes != f.nodes || g.index != f.index {
		t.Fatalf("counters: nodes %d/%d index %d/%d", g.nodes, f.nodes, g.index, f.index)
	}
	// Nieuw werk na de remount mag geen blok van de oude bestanden raken.
	if err := g.WriteAt("media/new.bin", 0, pattern(2<<20, 5)); err != nil {
		t.Fatal(err)
	}
	mustRead(t, g, "media/Films/A/a.mkv", film)
}

func TestPersistTornSlotFallsBack(t *testing.T) {
	d := persistDisk()
	f, _ := mountT(t, d, false)
	one := pattern(8192, 1)
	f.WriteAt("one", 0, one)
	f.Commit() // generatie 1, plek 0
	f.WriteAt("two", 0, pattern(8192, 2))
	f.Commit() // generatie 2, plek 1
	if f.last != 1 || f.gen != 2 {
		t.Fatalf("slots: last %d gen %d", f.last, f.gen)
	}
	// Stroom weg midden in plek 1: één byte van de boom kapot.
	off := (uint64(f.slot)+1)*BlockSize + 3
	d.data[off] ^= 0xff
	g, msg := mountT(t, d, false)
	if !strings.Contains(msg, "generation 1 restored from slot 0") {
		t.Fatalf("fallback: %q", msg)
	}
	mustRead(t, g, "one", one)
	if _, _, err := g.Stat("two"); !IsNotExist(err) {
		t.Fatalf("two should not exist in generation 1: %v", err)
	}
	// De volgende commit overschrijft de kapotte plek, niet de goede.
	g.WriteAt("three", 0, pattern(100, 3))
	if err := g.Commit(); err != nil || g.last != 1 || g.gen != 2 {
		t.Fatalf("commit after fallback: err %v last %d gen %d", err, g.last, g.gen)
	}
}

// Het veiligheidsregeltje: vrijgegeven blokken wachten op een commit. Anders
// wijst de boom op schijf na een stroomuitval naar andermans data.
func TestPersistDeferredFree(t *testing.T) {
	d := persistDisk()
	f, _ := mountT(t, d, false)
	old := pattern(1<<20, 4)
	f.WriteAt("old", 0, old)
	f.Commit()
	oldRun := f.root.children["old"].extents[0]
	f.Remove("old")
	f.WriteAt("new", 0, pattern(4<<20, 6))
	for _, e := range f.root.children["new"].extents {
		if e.physical < oldRun.physical+oldRun.count && oldRun.physical < e.physical+e.count {
			t.Fatalf("new reused old's blocks before a commit: %+v vs %+v", e, oldRun)
		}
	}
	// "Stroom weg" vóór de commit: de schijf-boom kent old nog, heel.
	g, _ := mountT(t, d, false)
	mustRead(t, g, "old", old)
	// Na de commit mogen ze wél terug in de vrije lijst (de allocator pakt
	// eerst de bump-regio, dus hergebruik zelf is niet het criterium).
	if err := f.Commit(); err != nil || len(f.pending) != 0 {
		t.Fatalf("commit: %v pending %d", err, len(f.pending))
	}
	if len(f.free) == 0 || f.free[0].start != oldRun.physical || f.free[0].count < oldRun.count {
		t.Fatalf("freed run not on the free list after commit: %+v vs %+v", f.free, oldRun)
	}
}

func TestPersistOtherWindowIgnored(t *testing.T) {
	d := persistDisk()
	f, _ := mountT(t, d, false)
	f.WriteAt("x", 0, pattern(4096, 1))
	f.Commit()
	// Zelfde schijf, kleiner venster: dezelfde plekken, andere geometrie.
	g, msg := mount(d, 0, uint64(len(d.data))/d.blockSize/2, d.blockSize, 128<<10, false)
	if !strings.Contains(msg, "no saved tree") || g.nodes != 0 {
		t.Fatalf("other window: %q nodes %d", msg, g.nodes)
	}
}

func TestPersistFreshClears(t *testing.T) {
	d := persistDisk()
	f, _ := mountT(t, d, false)
	f.WriteAt("x", 0, pattern(4096, 1))
	f.Commit()
	f.WriteAt("y", 0, pattern(4096, 2))
	f.Commit() // beide plekken beschreven
	if _, msg := mountT(t, d, true); !strings.Contains(msg, "starting empty, both") {
		t.Fatalf("fresh: %q", msg)
	}
	if g, msg := mountT(t, d, false); !strings.Contains(msg, "no saved tree") || g.nodes != 0 {
		t.Fatalf("after fresh: %q", msg)
	}
}

func TestPersistFreezeHoldsWriters(t *testing.T) {
	d := persistDisk()
	f, _ := mountT(t, d, false)
	f.WriteAt("before", 0, pattern(4096, 1))
	thaw, _, err := f.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.WriteAt("after", 0, pattern(4096, 2)) }()
	select {
	case <-done:
		t.Fatal("a write got through a frozen FS")
	case <-time.After(50 * time.Millisecond):
	}
	// Wat de nieuwe kern ziet: "before" wel, "after" niet.
	g, _ := mountT(t, d, false)
	if _, _, err := g.Stat("before"); err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, _, err := g.Stat("after"); !IsNotExist(err) {
		t.Fatalf("after: %v", err)
	}
	thaw()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPersistRejectsDoubleOwner(t *testing.T) {
	d := persistDisk()
	f, _ := mountT(t, d, false)
	f.WriteAt("a", 0, pattern(4096, 1))
	f.WriteAt("b", 0, pattern(4096, 2))
	// Een boom waarin twee bestanden hetzelfde blok claimen mag nooit geladen worden.
	f.root.children["b"].extents[0].physical = f.root.children["a"].extents[0].physical
	f.dirty = true
	f.Commit()
	g, msg := mountT(t, d, false)
	if !strings.Contains(msg, "owned twice") || g.nodes != 0 {
		t.Fatalf("double owner: %q", msg)
	}
}
