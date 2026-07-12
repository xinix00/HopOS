package raspi

import (
	"hop-os/metal/board"
	"hop-os/metal/dhcp"
)

// NetFromLease zet een DHCP-lease om in het board.NetConfig dat HOP verwacht.
// Geen resolver in de lease → de gateway als DNS (thuisrouters resolven
// vrijwel altijd zelf); poort 53. Gedeeld door rpi4/rpi5 (board.Net()).
func NetFromLease(l dhcp.Lease) board.NetConfig {
	dns := l.DNSString()
	if dns == "0.0.0.0" {
		dns = l.GWString()
	}
	return board.NetConfig{
		IP:   l.IPString(),
		CIDR: l.CIDR(),
		GW:   l.GWString(),
		DNS:  dns + ":53",
	}
}
