// init_generic.s — de UEFI-boot (init_body.h) voor Altra en de EDK2-machine: nVHE. De EL2-lay-out
// bepaalt het board via zijn build-tag, niet een -asmflags-vlag (20-09).

//go:build linkcpuinit && !o6n

#include "textflag.h"

#include "init_body.h"
