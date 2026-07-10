//go:build rpi4

package main

// Pi 4-variant van de app-image (image/rpi4-hopos.sh, -tags rpi4): zelfde app,
// zelfde canonieke link (slot-1-IPA), alleen de runtime-hooks komen van het
// rpi4-board. MMIO-hooks (printk) zijn op een app-core onder stage-2 verboden
// terrein — de board-laag guardt ze op de HOP-core.
import _ "hop-os/metal/board/rpi4"
