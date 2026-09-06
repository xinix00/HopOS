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

type unreadableBundle struct{ t *testing.T }

func (r unreadableBundle) Read([]byte) (int, error) {
	r.t.Fatal("invalid length read the body")
	return 0, io.EOF
}

func TestReadBundleLength(t *testing.T) {
	for _, n := range []int64{-1, 0, maxBundle + 1, 1 << 62} {
		if _, err := readBundle(unreadableBundle{t}, n); err == nil {
			t.Fatalf("accepted length %d", n)
		}
	}
	b, err := readBundle(bytes.NewReader([]byte("abc")), 3)
	if err != nil || string(b) != "abc" || cap(b) != 3 {
		t.Fatalf("read = %q, cap %d, err %v", b, cap(b), err)
	}
	if _, err = readBundle(bytes.NewReader([]byte("ab")), 3); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncation: %v", err)
	}
}

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

// A realistic LicheeRV bundle: compare transient allocation pressure without
// relying on the host GC schedule or claiming the board's OOM is diagnosed.
func BenchmarkBundleRead(b *testing.B) {
	payload := make([]byte, 6100000)
	for _, exact := range []bool{false, true} {
		name := "ReadAll"
		if exact {
			name = "ExactLength"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				var p []byte
				var err error
				if exact {
					p, err = readBundle(bytes.NewReader(payload), int64(len(payload)))
				} else {
					p, err = io.ReadAll(io.LimitReader(bytes.NewReader(payload), maxBundle+1))
				}
				if err != nil || !bytes.Equal(p, payload) {
					b.Fatal(err)
				}
			}
		})
	}
}
