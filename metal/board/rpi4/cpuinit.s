// CPU-init voor de Pi 4 (BCM2711): de gedeelde Pi-boot (board/raspi/
// cpuinit_body.h, op cpu/el2/boot.h) met alleen het board-eigene: de
// UART-basis. Met TF-A als armstub (verplicht op dit board, zie
// docs/archief/rpi4.md) levert de firmware ons op EL2 af.

//go:build linkcpuinit

#include "textflag.h"

#define UART_DR 0xFE201000
#define UART_FR 0xFE201018

#include "../raspi/cpuinit_body.h"
