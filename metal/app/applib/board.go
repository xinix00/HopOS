package applib

// Eén board voor elke app: hopslot, het generieke app-board. De import levert
// de tamago runtime-hooks (arch-timer, stille printk, MMIO-vrije RNG, de kale
// EL1-cpuinit) en de appboard-registratie (slot via de door HOP gepatchte
// slotHint). Onder stage-2 is de kooi het board — er is niets board-specifieks
// meer te linken, dus een app-binary draait ongewijzigd op QEMU, de Pi's en
// de Altra. Build-tags (rpi4/rpi5/uefi) doen voor app-images niets meer; ze
// bestaan alleen nog voor HOP-binaries (die linken drivers).
import _ "hop-os/metal/board/hopslot"
