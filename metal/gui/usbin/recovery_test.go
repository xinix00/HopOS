//go:build gui

package usbin

import (
	"testing"

	"github.com/xinix00/HopOS/metal/gui/driver/usb/hid"
)

func TestForgetControllerReleasesInputAndInvalidatesAllHandles(t *testing.T) {
	var got []hid.Event
	m := New(func(e hid.Event) { got = append(got, e) })
	p := &port{}
	p.kb.Decode([]byte{0, 0, 0x04, 0, 0, 0, 0, 0}, nil) // A ingedrukt
	p.ms.Decode([]byte{1, 0, 0}, nil)                   // linkermuisknop ingedrukt
	known := map[int]*port{1: p}

	// dev is bewust nil: recovery mag geen Detach op een handle doen dat door
	// de aanstaande HCRST ongeldig wordt.
	m.forgetController(known)

	if len(known) != 0 {
		t.Fatalf("oude Device-handles bleven bekend: %d", len(known))
	}
	want := []hid.Event{
		{Kind: hid.KeyUp, Code: 65},
		{Kind: hid.MouseUp, Code: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("release-events = %+v, wil %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("release-event %d = %+v, wil %+v", i, got[i], want[i])
		}
	}
	if evs := p.kb.Reset(nil); len(evs) != 0 {
		t.Fatalf("toetsenborddecoder behield state: %+v", evs)
	}
	if evs := p.ms.Reset(nil); len(evs) != 0 {
		t.Fatalf("muisdecoder behield state: %+v", evs)
	}
}

func TestEmitDropsBufferedEventsWithoutSink(t *testing.T) {
	m := New(nil)
	m.evs = append(m.evs, hid.Event{Kind: hid.KeyUp, Code: 65})
	m.emit()
	if len(m.evs) != 0 {
		t.Fatalf("eventbuffer zonder sink hield %d events vast", len(m.evs))
	}
}
