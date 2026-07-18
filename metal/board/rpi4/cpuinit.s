// EL2-capabele CPU-init voor de Pi 4 — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit). Met TF-A als armstub (verplicht op dit board, zie
// docs/archief/rpi4.md) levert de firmware ons op EL2 af. Het instructiepad zélf is
// gedeeld met de Pi 5 (board/raspi/cpuinit_body.h, ge#include'd hieronder);
// hier staat alleen het BCM2711-eigene: de UART-basis en de bewuste weglating
// van de A76-SMPEN-write.
//
// A72-verschil t.o.v. de Pi 5 (A76): GEEN CPUECTLR-write hier — de A76-
// encoding (S3_0_C15_C1_4) is op de A72 een ánder register (A72: SMPEN =
// S3_1_C15_C2_1 bit 6) en TF-A's cortex_a72-reset-handler zet SMPEN al.
// Verkeerde encoding = UNDEF-risico; weglaten is reference-conform — dus géén
// #define RPI5 hieronder (die zou de SMPEN-write in het gedeelde body inschakelen).

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0x7F000
#define DTB_PTR      0x7F008
#define UART_DR 0xFE201000
#define UART_FR 0xFE201018

#include "../raspi/cpuinit_body.h"
