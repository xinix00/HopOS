//go:build rpi5

// board_rpi5.go — de Raspberry Pi 5-kant van de agent-main: alleen wat écht
// rpi5-specifiek is. Het board-neutrale deel (RAM-declaratie, cmdline-config,
// watchdog) staat in board_raspi.go; netwerk komt via board.ProbeNIC
// (PCIe-RC-training → RP1 → GEM → DHCP, P2 bewezen 2026-07-10).
package main

import (
	"hop-os/metal/abi/layout"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi5"
	_ "hop-os/metal/board/rpi5/hop" // registreert het board (init); de basis levert de tamago-hooks
	"hop-os/metal/driver/dvfs"
	"hop-os/metal/driver/vcmail"
)

// Klokbeleid (docs/plan-p2b-soak.md): klok volgt idle via de firmware-mailbox.
// Enige rpi5-specifieke waarde t.o.v. de Pi 4: de VCMail-basis. TickHz =
// CNTFRQ/65536 (event-stream-tempo van de idle-teller, metal/cpu/idle).
func init() {
	boardExtra = func() {
		dvfs.Start(dvfs.Config{
			Mbox:    &vcmail.Mbox{Base: uintptr(rpi5.VCMailBase), Buf: uintptr(raspi.VCMailBuf)},
			LowHz:   600_000_000,
			TickHz:  raspi.CNTFRQ() / 65536,
			Slots:   layout.MaxSlots,
			Verbose: true, // flanken loggen (soak-diagnose)
		})
	}
}
