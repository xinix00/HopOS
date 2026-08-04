// Package appnet geeft een app zijn eigen netstack (per-slot netwerk) over de
// frame-ringen naar HOP's L2-switch (metal/net/hopswitch). Na Up werken
// net.Listen en net.Dial gewoon — op het interne net (10.100.0.0/24) praat een
// app rechtstreeks met andere apps en met HOP, zonder dat er ooit een
// TCP-stack op core 0 tussen zit.
//
// Bewust een apart pakket naast applib: alleen apps die netwerk willen linken
// de netstack mee; wie het niet importeert houdt een kleine image.
//
// Eén backend: gVisor via go-net (up_gvisor.go) — bewezen, maar fors (~4,3MB
// per app-image). Er stond hier tot 26-07 een tweede, lichtere backend
// (soypat/lneto via x/xnet, achter `-tags lnetonet`): ~2,7MB kleiner, maar elf
// dagen opt-in zonder ooit default te worden. Twee netstacks naast elkaar was
// de dure toestand — dubbele frame-constanten (lneto mócht go-net niet
// importeren), een dependency op een niet-uitgebrachte commit, extra
// gate-builds, en elke wijziging aan dit contract twee keer. De flip werd
// bovendien tegengehouden door een echt gat: x/xnet had geen close-all, dus
// peers van zo'n app vielen terug op hun eigen read-deadline (30s bij de
// display) i.p.v. een directe RST. Terughalen kan uit git history.
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
