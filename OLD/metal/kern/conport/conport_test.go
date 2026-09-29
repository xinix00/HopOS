package conport

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestIdleConsoleDisconnectReleasesStream(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		stream(server)
		close(done)
	}()
	// Drain any existing console history, then disconnect without requiring
	// another console write to release the reader.
	go io.Copy(io.Discard, client)
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle console retained its disconnected reader")
	}
}
