// CPU-init van de RK3566 (Radxa Zero 3): de generieke boot (cpu/el2/boot.h)
// met de twee adressen die dit board eigen heeft. U-Boot's `booti` levert af
// op EL2 met x0 = DTB — dezelfde conventie als QEMU-virt en de Pi-armstub,
// dus dezelfde vorm. Geen haken.
//
//   - de scratch ligt bínnen het eigen RAM-venster (zie rk3566.go: laag DRAM
//     op dit bord kan TrustZone-gefirewalld zijn, en een scratch die faultt
//     sterft vóór de eerste UART-byte);
//   - VBAR_EL2 van core 0 → de trap-vectoren, waar stage2.InitVectors na de
//     boot de HVC-revoke-handler in plugt (de hard-kill van een kooi). Moet
//     byte-gelijk zijn aan revokeVecPA in plan.go; die pariteit wordt bij
//     SetupPlan gecheckt.

//go:build linkcpuinit

#include "textflag.h"
#include "../../cpu/el2/sysreg.h"
#include "../../cpu/el2/drop.h"

#define BOOT_SCRATCH 0x0220F000	// = rk3566.BootScratch (pariteit: rk3566.go); +8 = DTB (rk3566.DTBPtr)
#define TRAP_VEC     0x062F0000	// = revokeVecPA (pariteit: plan.go, gecheckt)

#include "../../cpu/el2/boot.h"
