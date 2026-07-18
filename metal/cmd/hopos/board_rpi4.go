//go:build rpi4

// board_rpi4.go — de Raspberry Pi 4-kant van de agent-main: alleen wat écht
// rpi4-specifiek is — de board-registratie en de dvfs-mailbox-basis. De rest
// (RAM-declaratie, cmdline-config, watchdog) staat in board_raspi.go; het
// klokbeleid zelf in board/raspi/hop.StartDVFS. Netwerk = de geïntegreerde
// GENET v5 (metal/driver/nic/genet, P2 bewezen 2026-07-11).
package main

import (
	raspihop "hop-os/metal/board/raspi/hop"
	"hop-os/metal/board/rpi4"
	_ "hop-os/metal/board/rpi4/hop" // registreert het board (init); de basis levert de tamago-hooks
)

func init() {
	boardExtra = func() { raspihop.StartDVFS(uintptr(rpi4.VCMailBase)) }
}
