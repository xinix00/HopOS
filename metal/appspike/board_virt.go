//go:build !rpi5

package main

// De board-import levert de tamago runtime-hooks (Printk, Hwinit1, timers).
// De app-logica zelf is board-oblivious — de hop-ABI (control-page + ringen op
// hun IPA's) is op elk board identiek; alleen de hooks verschillen. Default:
// QEMU -M virt (image/qemu-run.sh); de Pi-variant zit achter de rpi5-tag.
import _ "hop-os/metal/board/qemuvirt"
