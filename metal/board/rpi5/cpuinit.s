// EL2-capabele CPU-init voor de Pi 5 — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit). De firmware levert ons op EL2 af (TF-A/armstub op EL3).
// Het instructiepad zélf is gedeeld met de Pi 4 (board/raspi/cpuinit_body.h,
// ge#include'd hieronder); hier staat alleen het BCM2712-eigene: de UART-basis
// en — als enige board — de A76-SMPEN-write (CPUECTLR_EL1, S3_0_C15_C1_4 bit6),
// die het gedeelde body via #define RPI5 inschakelt. De boot-EL-scratch is een
// RAM-adres onder de kernel (BOOT_SCRATCH), niet een device-page — de Pi heeft
// ons ctrl-page-plan nog niet.

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0x7F000
#define DTB_PTR      0x7F008
#define UART_DR 0x107d001000
#define UART_FR 0x107d001018
#define RPI5

#include "../raspi/cpuinit_body.h"
