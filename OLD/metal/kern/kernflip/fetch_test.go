package kernflip

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestFetchBundleIntegrity(t *testing.T) {
	payload := []byte("complete kernel bundle")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name   string
		status int
		length int
		body   []byte
		hash   string
		ok     bool
	}{
		{"complete", 200, len(payload), payload, want, true},
		{"truncated", 200, len(payload), payload[:len(payload)-1], want, false},
		{"wrong hash", 200, len(payload), payload, hex.EncodeToString(make([]byte, 32)), false},
		{"oversized declaration", 200, maxBundle + 1, nil, want, false},
		{"empty", 200, 0, nil, want, false},
		{"http failure", 404, len(payload), payload, want, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(tc.length))
				w.WriteHeader(tc.status)
				w.Write(tc.body)
			}))
			defer srv.Close()
			got, err := fetchBundle(srv.URL, tc.hash)
			if (err == nil) != tc.ok {
				t.Fatalf("success=%v err=%v", tc.ok, err)
			}
			if tc.ok && !bytes.Equal(got, payload) {
				t.Fatal("payload differs")
			}
		})
	}
}

func fetchBundle(url, want string) ([]byte, error) {
	var b bytes.Buffer
	_, _, err := fetchBundleInto(url, want, func(n int64) (io.Writer, error) { b.Grow(int(n)); return &b, nil })
	return b.Bytes(), err
}

type boundedBundleSink struct {
	bytes    int
	maxWrite int
}

func (s *boundedBundleSink) Write(p []byte) (int, error) {
	s.bytes += len(p)
	s.maxWrite = max(s.maxWrite, len(p))
	return len(p), nil
}
func TestFetchBundleStreamsWithBoundedWrites(t *testing.T) {
	payload := bytes.Repeat([]byte("pool"), 1537534)
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
	defer srv.Close()
	var sink boundedBundleSink
	n, _, err := fetchBundleInto(srv.URL, hex.EncodeToString(sum[:]), func(length int64) (io.Writer, error) {
		if length != int64(len(payload)) {
			t.Fatal(length)
		}
		return &sink, nil
	})
	if err != nil || n != int64(len(payload)) || sink.bytes != len(payload) || sink.maxWrite > 32<<10 {
		t.Fatalf("n=%d sink=%+v err=%v", n, sink, err)
	}
}

type shortBundleSink struct{}

func (shortBundleSink) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestFetchBundleStorageFailure(t *testing.T) {
	payload := []byte("pool storage")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
	defer srv.Close()
	denied := errors.New("no partition")
	_, _, err := fetchBundleInto(srv.URL, hex.EncodeToString(sum[:]), func(int64) (io.Writer, error) { return nil, denied })
	if !errors.Is(err, denied) {
		t.Fatalf("reservation: %v", err)
	}
	_, _, err = fetchBundleInto(srv.URL, hex.EncodeToString(sum[:]), func(int64) (io.Writer, error) { return shortBundleSink{}, nil })
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
}
