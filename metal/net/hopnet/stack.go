package hopnet

// stack.go — de netstack van de kern: leannet (xinix00/lean). Geen
// buffer-maten per wereld: één budget, afgeleid uit het RAM-raam zoals
// memlimit dat doet, en de stack deelt het zelf in — groeien bij gebruik,
// floor per verbinding, luid weigeren als de pot leeg is.
//
// Deze naad (stackUp levert de RX-voeding, hangt zelf net.SocketFunc) is wat
// twee stackwissels goedkoop maakte: gVisor → lneto (09-08) → leannet
// (12-08). Wie een derde overweegt, splitst dit bestand opnieuw achter een
// build-tag en meet op de netmeter-bank; de vorige kant staat in git history.
// Ontwerp en afwegingen: lean/leannet/DESIGN.md.

import (
	"fmt"
	"net"
	"net/netip"
	"time"
	_ "unsafe" // go:linkname naar het RAM-raam, zelfde bron als cpu/memlimit

	"github.com/xinix00/lean/leannet"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board"
)

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint

// stackUp bouwt de netstack over het device, hangt hem in Go's net-package en
// geeft de RX-voeding terug (rxLoop voert daar de frames in).
func stackUp(loc *locdev, nc board.NetConfig, mac string) (func([]byte) error, error) {
	pfx, err := netip.ParsePrefix(nc.CIDR)
	if err != nil {
		return nil, fmt.Errorf("netstack init: bad CIDR %q: %w", nc.CIDR, err)
	}
	cfg := leannet.Config{
		IP:     pfx.Addr().As4(),
		Prefix: pfx.Bits(),
		Budget: netBudget(),
		// Per verbinding niet meer buffer (rx + tx samen) dan één slot-frame-
		// ring: het venster dat HOP een app adverteert is wat die app in één
		// burst mag sturen, en dat moet in zijn TX-ring (960KB) passen — anders
		// dropt zijn Transmit en herstelt TCP het met retransmits. De ringen
		// verdubbelen binnen dit plafond, dus rx stopt onder de helft. Zelfde
		// grens als appnet aan de andere kant.
		MaxBufPerConn: layout.NetRingDataCap,
	}
	copy(cfg.MAC[:], loc.mac[:])
	if gw, err := netip.ParseAddr(nc.GW); err == nil && gw.Is4() {
		cfg.GW = gw.As4()
	}
	cfg.AdvWS = wsShiftFor(cfg.Budget / 4)

	// De ISS-seed hoeft niet kryptografisch, wel per boot anders — de klok
	// ná SNTP bestaat hier nog niet, dus de boot-tijd is wat er is.
	st := leannet.NewStack(loc, cfg, uint32(time.Now().UnixNano()))
	current = st

	// Netstack in Go's standaard net-package hangen: hierna werken
	// net.Listen, net/http en DNS voor alle HOP-kern-code. ICMP-echo zit er
	// standaard in (de ping-route van de node-watchdog); efemere poorten
	// ≥ 49152, per constructie disjunct van hopswitch.MasqEnd.
	net.SocketFunc = st.Socket
	return st.RecvInboundPacket, nil
}

// netBudget leidt de buffer-pot af uit het RAM-raam: 1/8 van het venster,
// geklemd. Op de LicheeRV (64MB) is dat 8MB; op een Altra-server met een
// groot raam loopt hij tot 64MB — "hier heb je 40MB, buffer een shit load"
// zonder dat iemand een getal per board kiest.
func netBudget() int {
	b := int(ramSize) / 8
	if b < 2<<20 {
		b = 2 << 20
	}
	if b > 64<<20 {
		b = 64 << 20
	}
	return b
}

// wsShiftFor geeft de kleinste window-scale-shift waarmee een venster van
// maxBuf bytes te adverteren is (RFC 7323, plafond shift 14).
func wsShiftFor(maxBuf int) uint8 {
	var shift uint8
	for shift < 14 && 0xffff<<shift < maxBuf {
		shift++
	}
	return shift
}

var current *leannet.Stack

// Stats: de tellers van HOP's eigen stack (telemetrie; nul vóór Up).
func Stats() leannet.Stats {
	if current == nil {
		return leannet.Stats{}
	}
	return current.Stats()
}
