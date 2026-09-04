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
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/ring"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

const txBackpressure = 10 * time.Millisecond

var errTXRingFull = errors.New("appnet: TX ring bleef vol")

// Tellers van het transport, voor een app die ze wil tonen (vitals):
// TXWaits = Transmit trof de ring vol en wachtte; TXDrops = na txBackpressure
// alsnog opgegeven (de stack ziet een device-fout, TCP herstelt met RTO);
// PumpEarly = de pomp is vóór zijn timer gewekt (kick); PumpTimer = de timer
// liep af, PumpTimerData = ... en er lag al RX (een gemiste kick).
var TXWaits, TXDrops, PumpEarly, PumpTimer, PumpTimerData, PumpMissNs, PumpMissMaxNs atomic.Uint64

// LastRXNs: wanneer de pomp zijn laatste frame uit de ring haalde (UnixNano);
// een meetlat om een call-latentie te splitsen in vóór en ná de pomp.
var LastRXNs atomic.Int64

// LastTXNs: wanneer Transmit zijn laatste frame in de ring zette (UnixNano).
var LastTXNs atomic.Int64

// Counters geeft de tellers als map, voor een status-pagina.
func Counters() map[string]uint64 {
	m := map[string]uint64{
		"tx_waits": TXWaits.Load(), "tx_drops": TXDrops.Load(),
		"pump_early": PumpEarly.Load(), "pump_timer": PumpTimer.Load(), "pump_timer_data": PumpTimerData.Load(),
		"pump_miss_us": PumpMissNs.Load() / 1000, "pump_miss_max_us": PumpMissMaxNs.Load() / 1000,
	}
	if st := current; st != nil {
		ts := st.Stats()
		m["tcp_retrans"], m["tcp_fast_retrans"] = uint64(ts.TCPRetransmits), uint64(ts.TCPFastRetransmits)
		m["tcp_persist"], m["tcp_zero_window"] = uint64(ts.TCPPersistProbes), uint64(ts.TCPZeroWindows)
		m["drop_bad"], m["drop_short"], m["drop_noport"], m["drop_replyfull"] = uint64(ts.DropBadFrame), uint64(ts.DropShortFrame), uint64(ts.DropNoPort), uint64(ts.DropReplyFull)
	}
	return m
}

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

// Transmit zet één frame in de TX-ring. Een korte lokale burst krijgt
// backpressure in plaats van stil pakketverlies. De bovengrens houdt een
// verdwenen switch een gewone device-fout in plaats van een deadlock.
func (n *nic) Transmit(buf []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	deadline := time.Now().Add(txBackpressure)
	for {
		ok, notify := n.tx.WriteNotify(ring.TypeFrame, buf)
		if ok {
			if notify {
				dev.Notify()
			}
			if len(buf) > 64 { // geen kale ACK: de stempel hoort bij een verzoek
				LastTXNs.Store(time.Now().UnixNano())
			}
			return nil
		}
		// Maak de vol→ruimte-race level-triggered zonder architectuurkennis.
		dev.Notify()
		TXWaits.Add(1)
		if time.Now().After(deadline) {
			TXDrops.Add(1)
			return errTXRingFull
		}
		runtime.Gosched()
	}
}
