package slots

import (
	"bytes"
	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
	"github.com/xinix00/HopOS/metal/v2/abi/systemapi"
	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
	"github.com/xinix00/HopOS/metal/v2/kern/hopfs"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

type systemTestAddr string

func (a systemTestAddr) Network() string { return "tcp" }
func (a systemTestAddr) String() string  { return string(a) }

func TestSlotFromRemote(t *testing.T) {
	tests := []struct {
		addr string
		slot int
		ok   bool
	}{
		{"10.100.0.2:1234", 1, true},
		{"10.100.0.17:65535", 16, true},
		{"10.100.0.1:1234", 0, false}, // HOP zelf is nooit een app.
		{"10.100.1.2:1234", 0, false},
		{"192.0.2.2:1234", 0, false},
		{"[::1]:1234", 0, false},
		{"geen-adres", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got, ok := slotFromRemote(systemTestAddr(tt.addr))
			if got != tt.slot || ok != tt.ok {
				t.Fatalf("slotFromRemote(%q) = (%d, %v), want (%d, %v)", tt.addr, got, ok, tt.slot, tt.ok)
			}
		})
	}
}

// A log-only app must not reserve the maximum file-transfer buffers. Keep
// the wire test here: this is the connection path that exhausted the RV node.
func TestSystemConnSmallThenLargeFrames(t *testing.T) {
	oldFS := fsys
	fsys = nil
	defer func() { fsys = oldFS }()
	s := &servicer{slot: 2, stop: make(chan struct{}), logs: make(chan string, 1)}
	svcMu.Lock()
	old := servicers[2]
	servicers[2] = s
	svcMu.Unlock()
	defer func() {
		svcMu.Lock()
		if old == nil {
			delete(servicers, 2)
		} else {
			servicers[2] = old
		}
		svcMu.Unlock()
	}()
	server, client := net.Pipe()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan struct{})
	defer func() { client.Close(); server.Close(); <-done }()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	go func() { defer close(done); serveSystemConn(server, s) }()
	sendLog := func(msg string) {
		t.Helper()
		if err := systemapi.WriteFrame(client, systemapi.KindLog, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-s.logs:
			if got != msg {
				t.Fatal("log payload changed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("log not delivered")
		}
	}
	sendLog("ready")
	request := hopabi.EncodeReq(hopabi.Req{Op: hopabi.OpRead, N: systemapi.MaxIOChunk, Path: "/file"})
	if err := systemapi.WriteFrame(client, systemapi.KindCall, request); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := systemapi.ReadFrame(client)
	if err != nil || kind != systemapi.KindResult {
		t.Fatalf("response kind %d: %v", kind, err)
	}
	response, err := hopabi.DecodeResp(payload)
	if err != nil || response.Status != hopabi.StatusError {
		t.Fatalf("storage-less read: %+v, %v", response, err)
	}
	runtime.ReadMemStats(&after)
	if used := after.TotalAlloc - before.TotalAlloc; used > 512<<10 {
		t.Fatalf("small system traffic allocated %d bytes; maximum bulk buffers must stay lazy", used)
	}
	sendLog(strings.Repeat("x", systemapi.MaxPayload))
	sendLog("small again")
}

// Sparse files exercise real hopfs reads without touching a physical disk.
func TestSystemReadReusesRequestBuffer(t *testing.T) {
	old := fsys
	fsys = hopfs.New(&nvme.Controller{Blocks: 4096, BlockSize: 512, MaxTransfer: 1 << 20})
	defer func() { fsys = old }()
	if err := fsys.Truncate("/app/file", systemapi.MaxIOChunk); err != nil {
		t.Fatal(err)
	}
	s := &servicer{root: "/app"}
	var work []byte
	var base *byte
	for i, n := range []int{systemapi.MaxIOChunk, 17, systemapi.MaxIOChunk, 0} {
		request := hopabi.EncodeReq(hopabi.Req{Op: hopabi.OpRead, Path: "/file", N: uint64(n), Seq: uint32(i + 1)})
		if cap(work) < len(request) {
			work = make([]byte, len(request))
		}
		copy(work[:len(request)], request)
		response := s.handleWithLimit(work[:len(request)], systemapi.MaxIOChunk, &work)
		got, err := hopabi.DecodeResp(response)
		if err != nil || got.Status != hopabi.StatusOK || got.Seq != uint32(i+1) || got.Size != uint64(n) || !bytes.Equal(got.Data, make([]byte, n)) {
			t.Fatalf("read %d: size=%d seq=%d status=%d err=%v", n, got.Size, got.Seq, got.Status, err)
		}
		if &response[0] != &work[0] {
			t.Fatal("response is outside the connection work buffer")
		}
		if i == 0 {
			base = &work[0]
		} else if &work[0] != base {
			t.Fatal("same or smaller read replaced the reusable buffer")
		}
	}
}
