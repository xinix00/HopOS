// CPU-init voor de Pi 5 (BCM2712): de gedeelde Pi-boot (board/raspi/
// cpuinit_body.h, op cpu/el2/boot.h) met alleen het board-eigene: de
// UART-basis. De firmware levert ons op EL2 af (TF-A/armstub op EL3).

//go:build linkcpuinit

#include "textflag.h"

#define UART_DR 0x107d001000
#define UART_FR 0x107d001018

#include "../raspi/cpuinit_body.h"
