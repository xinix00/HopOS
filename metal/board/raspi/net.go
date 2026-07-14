package raspi

import (
	"hop-os/metal/board"
	"hop-os/metal/net/dhcp"
)

// NetFromLease is de raspi-alias voor board.NetFromLease (rpi4/rpi5 roepen
// deze aan; de omzetting zelf woont gedeeld in metal/board).
func NetFromLease(l dhcp.Lease) board.NetConfig { return board.NetFromLease(l) }
