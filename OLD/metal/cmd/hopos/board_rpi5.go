//go:build rpi5

// board_rpi5.go — de Raspberry Pi 5-kant van de agent-main: alleen wat écht
// rpi5-specifiek is — de board-registratie en de dvfs-mailbox-basis. De rest
// (RAM-declaratie, cmdline-config, watchdog) staat in board_raspi.go; het
// klokbeleid zelf in board/raspi/hop.StartDVFS. Netwerk komt via
// board.ProbeNIC (PCIe-RC-training → RP1 → GEM → DHCP, P2 bewezen 2026-07-10).
package main

import (
	raspihop "github.com/xinix00/HopOS/metal/v2/board/raspi/hop"
	"github.com/xinix00/HopOS/metal/v2/board/rpi5"
	_ "github.com/xinix00/HopOS/metal/v2/board/rpi5/hop" // registreert het board (init); de basis levert de tamago-hooks
)

func init() {
	boardExtra = func() { raspihop.StartDVFS(uintptr(rpi5.VCMailBase)) }
}
