// init_o6n.s — de UEFI-boot (init_body.h) voor de Orion O6N: VHE (nVHE-EL1 stierf er binnen 0,5 s, 17-09). De EL2-lay-out
// bepaalt het board via zijn build-tag, niet een -asmflags-vlag (20-09).

//go:build linkcpuinit && o6n

#include "textflag.h"
#define VHE

#include "init_body.h"
