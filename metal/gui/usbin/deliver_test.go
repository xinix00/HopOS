//go:build gui

package usbin

import (
	"net"
	"testing"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/hid"
)

type inputTestBoard struct{ board.Board }

func (inputTestBoard) Framebuffer() (fb.Desc, bool) { return fb.Desc{}, false }

func TestInputListensOnNodeStackAndRejectsNonHolder(t *testing.T) {
	old := board.Current()
	board.Use(inputTestBoard{})
	defer board.Use(old)
	d, addr, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if addr != "10.100.0.1:7879" {
		t.Fatal("changed app-facing address", addr)
	}
	if a, ok := d.l.Addr().(*net.TCPAddr); !ok || !a.IP.IsUnspecified() {
		t.Fatal("listener binds an address absent from the node stack", d.l.Addr())
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:7879", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err = c.Read(b[:]); err == nil {
		t.Fatal("non-holder was accepted")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("non-holder connection was not closed")
	}
}

func TestIdleInputHeartbeatAndShutdown(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	d := &deliverer{q: make(chan hid.Event, queueDepth), conn: server}
	done := make(chan struct{})
	go func() { d.run(); close(done) }()
	client.SetReadDeadline(time.Now().Add(7 * time.Second))
	var b [1]byte
	n, err := client.Read(b[:])
	close(d.q)
	if err != nil || n != 1 || b[0] != '\n' {
		t.Fatalf("heartbeat %q: %v", b[:n], err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not stop")
	}
}
