// CPU-init van het generieke app-board (bouw met -tags linkcpuinit, zoals
// alle images): de generieke boot (cpu/el2/boot.h) zonder één define. Het
// kanonieke pad is EL1: HOP's EL2-trampoline (cpu/el2, s2tramp) heeft stage-2,
// timers en een schone SCTLR al geregeld en ERET't naar cpuinitEL1 — dan rest
// alleen: SCTLR opschonen, stack uit de (door HOP gepatchte) RAM-declaratie, en
// door naar de tamago-runtime. Géén BOOT_SCRATCH en géén TRAP_VEC: een
// gekooide core mag geen MMIO, scratch of firmware aanraken — en heeft het
// ook niet nodig.
//
// EL2 is het dev-pad (QEMU -kernel app.elf, buiten HopOS om): dezelfde drop
// als elke boot, alleen zonder scratch (een app-image heeft geen
// scratch-contract).

//go:build linkcpuinit

#include "textflag.h"
#include "../../cpu/el2/sysreg.h"
#include "../../cpu/el2/drop.h"
#include "../../cpu/el2/boot.h"
