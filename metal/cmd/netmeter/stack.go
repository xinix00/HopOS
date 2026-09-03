package main

// stack.go — de stack-opbouw van de bank: leannet (xinix00/lean). De bank
// blijft het instrument waarmee een stackwissel verdiend wordt; de lat van de
// vorige kant staat in de geschiedenis (lneto: LicheeRV RX 8,84 MB/s zonder
// drops, kern-OOM bij 128KiB-buffers op een 64MB-raam).

import (
	"net"
	"net/netip"
	"time"

	"github.com/xinix00/HopOS/metal/v2/net/netdev"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/lean/leannet"
)

const stackName = "leannet"

// benchStackUp bouwt de bank-stack met een vast, ruim budget: de bank meet
// het pakket-pad, niet de krapte (die meet de OOM-reproductie op de node
// zelf). Drops en weigeringen zijn tellers op de stack; /report kan ze later
// tonen.
func benchStackUp(nic netdev.Device, nc board.NetConfig, mac string) (func([]byte) error, error) {
	pfx, err := netip.ParsePrefix(nc.CIDR)
	if err != nil {
		return nil, err
	}
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, err
	}
	cfg := leannet.Config{
		IP:     pfx.Addr().As4(),
		Prefix: pfx.Bits(),
		// Hetzelfde budget-profiel als de kern op dit board (hopnet: raam/8 —
		// op de LicheeRV 8MB van 64MB), zodat de bank meet wat een node doet en
		// niet een ruimere wereld. Cap per verbinding is Budget/4 = 2MB, dus
		// shift 5 (0xffff<<5 ≈ 2MB) is precies genoeg om dat te adverteren.
		Budget: 8 << 20,
		AdvWS:  5,
	}
	copy(cfg.MAC[:], hw)
	if gw, err := netip.ParseAddr(nc.GW); err == nil && gw.Is4() {
		cfg.GW = gw.As4()
	}
	st := leannet.NewStack(nic, cfg, uint32(time.Now().UnixNano()))
	net.SocketFunc = st.Socket
	return st.RecvInboundPacket, nil
}
