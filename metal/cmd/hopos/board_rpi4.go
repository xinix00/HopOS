//go:build rpi4

// board_rpi4.go — de Raspberry Pi 4-kant van de agent-main: alleen wat écht
// rpi4-specifiek is. Het board-neutrale deel (RAM-declaratie, cmdline-config,
// watchdog) staat in board_raspi.go; netwerk = de geïntegreerde GENET v5
// (metal/driver/nic/genet, P2 bewezen 2026-07-11).
package main

import (
	"hop-os/metal/abi/layout"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi4"
	_ "hop-os/metal/board/rpi4/hop" // registreert het board (init); de basis levert de tamago-hooks
	"hop-os/metal/driver/dvfs"
	"hop-os/metal/driver/vcmail"
)

// Klokbeleid (docs/plan-p2b-soak.md): identiek aan de Pi 5, alleen de
// mailbox-basis verschilt. TickHz = CNTFRQ/65536 (event-stream-tempo).
func init() {
	boardExtra = func() {
		dvfs.Start(dvfs.Config{
			Mbox:    &vcmail.Mbox{Base: uintptr(rpi4.VCMailBase), Buf: uintptr(raspi.VCMailBuf)},
			LowHz:   600_000_000,
			TickHz:  raspi.CNTFRQ() / 65536,
			Slots:   layout.MaxSlots,
			Verbose: true, // flanken loggen (soak-diagnose)
		})
	}
}
