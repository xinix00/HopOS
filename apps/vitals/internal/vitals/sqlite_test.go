package vitals

import (
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// memFS is de FS-interface in het geheugen: genoeg om de VFS en de
// sqlite-test op de host te draaien.
type memFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMemFS() *memFS { return &memFS{files: map[string][]byte{}} }

func (m *memFS) Stat(path string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[path]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return uint64(len(b)), nil
}

func (m *memFS) ReadAt(path string, off uint64, n int) ([]byte, error) {
	buf := make([]byte, n)
	read, err := m.ReadInto(path, off, buf)
	return buf[:read], err
}

func (m *memFS) ReadInto(path string, off uint64, dst []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[path]
	if !ok {
		return 0, fs.ErrNotExist
	}
	if off >= uint64(len(b)) {
		return 0, nil
	}
	return copy(dst, b[off:]), nil
}

func (m *memFS) WriteAt(path string, off uint64, data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.files[path]
	if end := off + uint64(len(data)); end > uint64(len(b)) {
		b = append(b, make([]byte, end-uint64(len(b)))...)
	}
	copy(b[off:], data)
	m.files[path] = b
	return len(data), nil
}

func (m *memFS) Truncate(path string, size uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.files[path]
	if size > uint64(len(b)) {
		b = append(b, make([]byte, size-uint64(len(b)))...)
	}
	m.files[path] = b[:size]
	return nil
}

func (m *memFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[path]; !ok {
		return fs.ErrNotExist
	}
	delete(m.files, path)
	return nil
}

func TestSQLiteOnTheVFS(t *testing.T) {
	m := newMemFS()
	s := NewServer(Config{Version: "test", Arch: "host", Port: "0", FS: m})
	res := s.run("sqlite", url.Values{"mb": {"3"}, "trap": {"1"}, "rows": {"50"}})
	if res.Err != "" {
		t.Fatalf("sqlite: %s\n%s", res.Err, strings.Join(res.Lines, "\n"))
	}
	want := map[string]bool{"insert": false, "read": false, "commits/s": false, "excl insert": false, "wal insert": false, "wal commits/s": false, "wal-normal insert": false, "trap insert": false}
	for _, metric := range res.Metrics {
		if _, ok := want[metric.Name]; ok {
			want[metric.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("metric %s missing: %+v", name, res.Metrics)
		}
	}
	if len(m.files) != 0 {
		t.Fatalf("files left behind: %v", m.files)
	}
	t.Log(strings.Join(res.Lines, "\n"))
}

// De coalescer: aansluitende writes worden één run, een gat of de limiet
// breekt hem, en een read die wachtende bytes raakt ziet ze eerst landen.
func TestWriteCoalescer(t *testing.T) {
	var runs []string
	c := newWriteCoalescer(8, func(off int64, p []byte) error {
		runs = append(runs, string(rune('0'+off))+":"+string(p))
		return nil
	})
	_ = c.Write(0, []byte("ab"))
	_ = c.Write(2, []byte("cd"))
	if !c.Overlaps(3, 1) || c.Overlaps(4, 1) {
		t.Fatal("overlaps")
	}
	_ = c.Write(5, []byte("x"))        // gat → flush
	_ = c.Write(6, []byte("yyyyyyyy")) // over de limiet → flush, dan apart
	_ = c.Flush()
	if got := strings.Join(runs, " "); got != "0:abcd 5:x 6:yyyyyyyy" {
		t.Fatalf("runs = %s", got)
	}
}
