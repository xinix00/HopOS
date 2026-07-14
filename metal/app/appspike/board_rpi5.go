//go:build rpi5

package main

// Pi 5-variant van de app-image (image/rpi5-hopos.sh, -tags rpi5): zelfde
// app, zelfde canonieke link (slot-1-IPA), alleen de runtime-hooks komen van
// het rpi5-board. LET OP: hooks die MMIO raken zijn op een app-core onder
// stage-2 verboden terrein — raspi.hwinit1 guardt zijn UART-marker daarom op
// de primaire core.
import _ "hop-os/metal/board/rpi5"
