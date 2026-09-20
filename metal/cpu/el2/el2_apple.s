// el2_apple.s — de EL2-code van dit board in één vertaaleenheid. Welke variant van
// de switcher en welke EL2-lay-out (VHE of nVHE) erin zit, bepaalt het BOARD
// via zijn build-tag, niet een -asmflags-vlag in een bouwscript (Derek,
// 20-09: "het board bepaalt wat erin wordt gesteld"). De drie lichamen
// (el2_body.h, smp_body.h, switch_body.h) zijn gedeeld; sysreg.h kiest op
// VHE de register-encoderingen, switch_body.h op APPLE_IPI het slaap- en
// wekpad (zonder: WFE, gewekt door de event stream of HOP's kick).
//
// Apple silicon: E2H staat vast op 1 (VHE) en HOP wekt een slapende
// app-core met de fast IPI (switch_body.h, APPLE_IPI).

//go:build tamago && arm64 && apple

#include "textflag.h"
#define VHE
#define APPLE_IPI
#include "hygiene.h"
#include "sysreg.h"
#include "drop.h"

#include "el2_body.h"
#include "smp_body.h"
#include "switch_body.h"
