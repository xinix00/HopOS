//go:build !(tamago && arm64)

package idle

import "sync/atomic"

var DoorIRQs, DoorIRQWoken atomic.Uint64

// ServeDoorIRQ: buiten arm64/tamago geen doorbell-interrupt — de governor
// blijft de weg (rxdoor.go), HOP kickt draaiende cores niet.
func ServeDoorIRQ() bool { return false }
