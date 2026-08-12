// Package appnet geeft een app zijn eigen netstack (per-slot netwerk) over de
// frame-ringen naar HOP's L2-switch (metal/net/hopswitch). Na Up werken
// net.Listen en net.Dial gewoon — op het interne net (10.100.0.0/24) praat een
// app rechtstreeks met andere apps en met HOP, zonder dat er ooit een
// TCP-stack op core 0 tussen zit.
//
// Bewust een apart pakket naast applib: alleen apps die netwerk willen linken
// de netstack mee; wie het niet importeert houdt een kleine image.
//
// De stack is leannet (xinix00/lean), sinds 12-08; de opbouw staat in up.go
// en de afwegingen in lean/leannet/DESIGN.md. Geen build-tags, geen backends.
//
// De geschiedenis in het kort, want die verklaart waarom dit pakket zo dun is:
// gVisor was de bewezen maar forse backend (~2,7MB van elk app-image, 340k
// allocaties per 64MiB op het RX-pad); lneto (09-08) haalde dat weg maar bleek
// bugs te dragen die wij niet konden repareren zonder fork-onderhoud (29
// bevindingen, 11-08). Elke wissel raakte alleen up.go, want alles eromheen
// hangt aan het twee-methode-device (metal/net/netdev).
package appnet

import (
	"sync"

	"github.com/xinix00/HopOS/metal/abi/ring"
)

// nic is het netdev.Device over de eigen frame-ringen.
type nic struct {
	mu sync.Mutex // Transmit kan uit meerdere goroutines komen; ring is SPSC
	tx *ring.Ring // app → switch (wij producer)
	rx *ring.Ring // switch → app (wij consumer)
}

// Receive levert één frame uit de RX-ring (0 = niets; de RX-lus pollt).
// Uitsluitend door de RX-lus aangeroepen — één consumer. ReadInto leest het
// frame rechtstreeks in buf: geen allocatie én geen extra kopie per frame
// (buf is ruim MTU-groot, dus elk doorgezet Ethernet-frame past).
func (n *nic) Receive(buf []byte) (int, error) {
	typ, m, ok := n.rx.ReadInto(buf)
	if !ok || typ != ring.TypeFrame {
		return 0, nil
	}
	return m, nil
}

// Transmit zet één frame in de TX-ring; vol = drop (TCP herstelt).
func (n *nic) Transmit(buf []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tx.Write(ring.TypeFrame, buf)
	return nil
}
