package vitals

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTemperatureOpenAndAuthenticatedNode(t *testing.T) {
	for _, key := range []string{"", "test-key"} {
		t.Run(fmt.Sprintf("key-%t", key != ""), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := ""
				if key != "" {
					want = sign(key, "GET", "/v1/agents", nil)
				}
				if got := r.Header.Get("X-Hop-Auth"); got != want {
					t.Errorf("auth=%q want%q", got, want)
				}
				fmt.Fprint(w, `[{"endpoint":"http://192.168.1.122:8080","temp_milli_c":90000},{"endpoint":"http://192.168.1.12:8080","temp_milli_c":42000}]`)
			}))
			defer server.Close()
			cfg := Config{HopAddr: strings.TrimPrefix(server.URL, "http://"), HopKey: key, Host: "192.168.1.12"}
			var cache tempCache
			if got := cache.get(cfg); got != 42000 {
				t.Fatalf("temperature=%d", got)
			}
			cfg.Host = "192.168.1.99"
			if got := fetchTemp(cfg); got != 0 {
				t.Fatalf("unrelated temperature=%d", got)
			}
		})
	}
}

type unavailableFS struct {
	FS
	err error
}

func (f unavailableFS) Stat(string) (uint64, error) { return 0, f.err }

type failingWriteFS struct{ FS }

func (f failingWriteFS) WriteAt(string, uint64, []byte) (int, error) {
	return 0, errors.New("disk I/O failure")
}
func TestMissingStorageIsSkippedButIOFailureIsNot(t *testing.T) {
	for _, fsys := range []FS{nil, unavailableFS{err: errors.New("system call op 1: status 1: no storage layer on board")}} {
		s := NewServer(Config{FS: fsys})
		for _, run := range []func(*Result, url.Values){s.runDisk, s.runSQLite} {
			r := &Result{}
			run(r, nil)
			if r.Err != "" || r.Skipped == "" {
				t.Fatalf("missing storage: %+v", r)
			}
		}
	}
	for _, fsys := range []FS{unavailableFS{err: errors.New("device timeout")}, failingWriteFS{newMemFS()}} {
		s := NewServer(Config{FS: fsys})
		r := &Result{}
		s.runDisk(r, url.Values{"mb": {"1"}})
		if r.Err == "" || r.Skipped != "" {
			t.Fatalf("I/O failure hidden: %+v", r)
		}
	}
}
func TestSyscallWithoutStorageStillMeasuresHTTP(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); fmt.Fprint(w, "ok") }))
	defer server.Close()
	s := NewServer(Config{FS: unavailableFS{err: errors.New("system call op 1: status 1: no storage layer on board")}, HopAddr: strings.TrimPrefix(server.URL, "http://")})
	r := &Result{}
	s.runSyscall(r, url.Values{"n": {"10"}})
	if r.Err != "" || r.Skipped != "" || count.Load() != 10 || len(r.Metrics) != 2 {
		t.Fatalf("HTTP-only result count=%d %+v", count.Load(), r)
	}
	if !strings.Contains(strings.Join(r.Lines, " "), "stat phase skipped") {
		t.Fatalf("missing phase disclosure: %+v", r)
	}
}
func TestRXUserAgentAndShortResponse(t *testing.T) {
	for _, size := range []int{7, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() != "HopOS-vitals" {
					t.Errorf("user agent=%q", r.UserAgent())
				}
				w.Header().Set("Content-Length", fmt.Sprint(size))
				w.Write(make([]byte, size))
			}))
			defer server.Close()
			s := NewServer(Config{RxURL: server.URL})
			r := &Result{}
			s.runRx(r, url.Values{"mb": {"1"}})
			if size < 1<<20 {
				if r.Err == "" || len(r.Metrics) != 0 {
					t.Fatalf("short response credited: %+v", r)
				}
			} else if r.Err != "" || len(r.Metrics) == 0 {
				t.Fatalf("download failed: %+v", r)
			}
		})
	}
}
