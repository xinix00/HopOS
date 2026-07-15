//go:build rpi4

package main

// Pi 4-variant van de apploader (image/rpi4-agent.sh bakt 'm in de node): de
// board-import levert de tamago runtime-hooks. Zelfde loader, zelfde canonieke
// link — alleen de hooks verschillen per board. MMIO-hooks zijn op een
// app-core onder stage-2 verboden terrein — de board-laag guardt ze op de
// HOP-core.
import _ "hop-os/metal/board/rpi4"
