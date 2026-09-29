// CPU-init van QEMU -M virt,virtualization=on: de generieke boot
// (cpu/el2/boot.h) met de twee adressen die dit board eigen heeft. QEMU levert
// af op EL2 met x0 = DTB; boot.h legt EL en x0 op de scratch, zet VBAR_EL2 op
// de trap-vectoren, HCR = RW, en dropt naar EL1 (cpu/el2/drop.h). Geen haken:
// dit board heeft geen banner, geen SMPEN en geen loader.

//go:build linkcpuinit

#include "textflag.h"
#include "../../cpu/el2/sysreg.h"
#include "../../cpu/el2/drop.h"

#define BOOT_SCRATCH 0xB0000000	// = layout.BootScratch (+8 = DTB, layout.DTBPtr)
#define TRAP_VEC     0xC2000800	// = layout.TrapVecPA() van het qemuvirt-plan
				// (pariteit gecheckt in qemuvirt.go init)

#include "../../cpu/el2/boot.h"
