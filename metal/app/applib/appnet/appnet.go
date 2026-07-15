// Package appnet geeft een app zijn eigen netstack (per-slot netwerk) over de
// frame-ringen naar HOP's L2-switch (metal/net/hopswitch). Na Up werken
// net.Listen en net.Dial gewoon — op het interne net (10.100.0.0/24) praat een
// app rechtstreeks met andere apps en met HOP, zonder dat er ooit een
// TCP-stack op core 0 tussen zit.
//
// Bewust een apart pakket naast applib: alleen apps die netwerk willen linken
// de netstack mee; wie het niet importeert houdt een kleine image.
//
// Er zijn twee backends achter dezelfde Up (build-tag, geen API-verschil):
//
//   - default: gVisor via go-net (up_gvisor.go) — bewezen, maar fors
//     (~4,3MB per app-image);
//   - -tags lnetonet: soypat/lneto via x/xnet (up_lneto.go) — ~2,7MB
//     kleiner, maar jong; wordt pas default als hij NETDEMO + soak
//     overleeft. Bewust rechtstreeks op x/xnet en niet op go-net's
//     LnetoStack: go-net importeert gvisor, en package-inits worden altijd
//     meegelinkt — via go-net blijft gVisor dus in de binary (gemeten
//     15-07: 5,54MB i.p.v. 3,88MB).
package appnet

import (
	"sync"

	"hop-os/metal/abi/ring"
)

// nic is het NetworkDevice over de eigen frame-ringen — gedeeld door beide
// backends.
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
