//go:build rpi5

// board_rpi5.go — de Raspberry Pi 5-kant van de agent-main: dezelfde
// HOP-agent-bytes, maar met de rpi5-runtime-hooks en de RAM-declaratie van
// het board (raw load op 0x80000, 128MB HOP-kern — zie pi5_main/mem_rpi5).
// Netwerk komt via board.ProbeNIC: PCIe-RC-training → RP1 → GEM → DHCP,
// allemaal HOP's eigen werk (P2, bewezen 2026-07-10).
package main

import (
	_ "unsafe"

	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi5" // registreert het board (init) + tamago-hooks
	"hop-os/metal/dev"
	"hop-os/metal/dvfs"
	"hop-os/metal/layout"
	"hop-os/metal/vcmail"
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = 0x00080000

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x08000000

// Klokbeleid (docs/plan-p2b-soak.md): klok volgt idle, via de firmware-
// mailbox van dit board. TickHz = CNTFRQ/65536 (het event-stream-tempo van
// de idle-teller, zie metal/idle: EVNTI=15 → periode 2^16 tellerticks).
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

// Node-identiteit (P2b/C5): eerst de boot-parameter hopos.node= uit
// cmdline.txt (configureren zonder rebuild), anders het board-serial.
func init() {
	nodeName = func() string {
		dtb := uintptr(dev.Read64(rpi5.DTBPtr))
		if n := raspi.BootParam(dtb, "hopos.node"); n != "" {
			return n
		}
		if s := raspi.SerialSuffix(dtb); s != "" {
			return "hopos-" + s
		}
		return ""
	}
}
