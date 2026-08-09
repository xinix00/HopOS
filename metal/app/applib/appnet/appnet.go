// Package appnet geeft een app zijn eigen netstack (per-slot netwerk) over de
// frame-ringen naar HOP's L2-switch (metal/net/hopswitch). Na Up werken
// net.Listen en net.Dial gewoon — op het interne net (10.100.0.0/24) praat een
// app rechtstreeks met andere apps en met HOP, zonder dat er ooit een
// TCP-stack op core 0 tussen zit.
//
// Bewust een apart pakket naast applib: alleen apps die netwerk willen linken
// de netstack mee; wie het niet importeert houdt een kleine image.
//
// Eén backend: lneto via go-net (up_lneto.go) — sinds de flip van 09-08.
// De geschiedenis in het kort: gVisor was de bewezen maar forse backend
// (~2,7MB van elk app-image, 340k allocaties per 64MiB op het RX-pad); een
// eerdere lneto-poging (26-07) sneuvelde omdat twee stacks náást elkaar de
// dure toestand was én x/xnet echte gaten had. Die gaten zijn 09-08 gedicht
// in de bron zelf (window scaling, deadline-gedreven dials, sequentiële
// poorten — zie ~/Git/lneto branch hopos) en de flip is gemeten op de
// netmeter-bank: RX 26→61MB/s met 0 GC-druk. Eén backend, geen tags —
// behalve `nodefaultstack` bij het BOUWEN, anders linkt go-net's
// Interface-fallback gvisor alsnog mee.
package appnet

import (
	"sync"

	"github.com/xinix00/HopOS/metal/abi/ring"
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
