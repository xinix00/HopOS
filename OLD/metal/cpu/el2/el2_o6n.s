// el2_o6n.s — de EL2-code van dit board in één vertaaleenheid. Welke variant van
// de switcher en welke EL2-lay-out (VHE of nVHE) erin zit, bepaalt het BOARD
// via zijn build-tag, niet een -asmflags-vlag in een bouwscript (Derek,
// 20-09: "het board bepaalt wat erin wordt gesteld"). De drie lichamen
// (el2_body.h, smp_body.h, switch_body.h) zijn gedeeld; sysreg.h kiest op
// VHE de register-encoderingen, switch_body.h op APPLE_IPI het slaap- en
// wekpad (zonder: WFE, gewekt door de event stream of HOP's kick).
//
// Radxa Orion O6N: VHE (17-09: nVHE-EL1 stierf er binnen 0,5 s), de
// switcher slaapt in WFE en HOP's SGI-kick wekt die.

//go:build tamago && arm64 && o6n

#include "textflag.h"
#define VHE
#include "hygiene.h"
#include "sysreg.h"
#include "drop.h"

#include "el2_body.h"
#include "smp_body.h"
#include "switch_body.h"
