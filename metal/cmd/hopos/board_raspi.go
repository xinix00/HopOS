//go:build rpi4 || rpi5

// board_raspi.go — de GEDEELDE Pi-kant van de agent-main (rpi4 én rpi5): alles
// wat board-neutraal is, één keer. De DTB-pointer komt uit raspi.DTB()
// (dev.Read64(BootScratch+8), identiek op beide Pi's — rpi4.DTBPtr == rpi5.DTBPtr),
// dus het lezen van cmdline.txt-parameters (hopos.node/cores/wd) hoeft niet per
// board herhaald te worden. De RAM-declaratie is op beide Pi's dezelfde raspi-
// constante. Wat écht verschilt (de dvfs-mailbox) staat in board_rpi5.go /
// board_rpi4.go.
package main

import (
	"strconv"
	"time"
	_ "unsafe" // go:linkname (RAM-declaratie)

	"hop-os/metal/board/raspi"
)

// RAM-declaratie: raw load op 0x80000, 128MB HOP-kern (mem_rpi*). Gelijk op
// beide Pi's, dus hier gedeeld.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = raspi.HopKernelStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = raspi.HopKernelSize

// init leest de node-config uit cmdline.txt — HopOS leest 'm (HOP-userspace kan
// er niet bij) en zet de board-hooks. Eén DTB-lees, drie parameters; hetzelfde
// patroon voor elke sleutel via raspi.BootParam. Draait vóór het board-specifieke
// boardExtra (init-volgorde op bestandsnaam: board_raspi < board_rpi*), zodat de
// watchdog — net als vroeger — zo vroeg mogelijk gewapend is.
func init() {
	dtb := raspi.DTB()

	// Hardware-watchdog vroeg (freeze-jacht 13-07): ook een hangende boot
	// reset-cyclet zichzelf tot een boot slaagt. Uit met hopos.wd=off (géén
	// rebuild): voor een JTAG-postmortem moet een bevroren node blijven stáán.
	if raspi.BootParam(dtb, "hopos.wd") != "off" {
		raspi.WatchdogStart(12 * time.Second)
	}

	// Node-identiteit (P2b/C5): hopos.node= uit cmdline, anders het board-serial;
	// twee nodes op één LAN mogen nooit allebei "hopos-1" heten.
	nodeName = func() string {
		if n := raspi.BootParam(dtb, "hopos.node"); n != "" {
			return n
		}
		if s := raspi.SerialSuffix(dtb); s != "" {
			return "hopos-" + s
		}
		return ""
	}

	// HOP-runtime-cores: hopos.cores= uit cmdline. Default 1 — geen verspilling
	// bij weinig apps; opt-in hoger als de flow er druk genoeg voor is.
	hopCores = func() int {
		if v := raspi.BootParam(dtb, "hopos.cores"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 {
				return n
			}
		}
		return 1
	}
}
