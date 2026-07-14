//go:build !rpi5 && !rpi4 && !uefi

package main

// Default: QEMU -M virt. De board-import levert de tamago runtime-hooks; de
// loader-logica is board-oblivious (de hop-ABI is op elk board identiek).
import _ "hop-os/metal/board/qemuvirt"
