// el2_nvhe.s — de EL2-code van dit board in één vertaaleenheid. Welke variant van
// de switcher en welke EL2-lay-out (VHE of nVHE) erin zit, bepaalt het BOARD
// via zijn build-tag, niet een -asmflags-vlag in een bouwscript (Derek,
// 20-09: "het board bepaalt wat erin wordt gesteld"). De drie lichamen
// (el2_body.h, smp_body.h, switch_body.h) zijn gedeeld; sysreg.h kiest op
// VHE de register-encoderingen, switch_body.h op APPLE_IPI het slaap- en
// wekpad (zonder: WFE, gewekt door de event stream of HOP's kick).
//
// Alle andere borden (QEMU virt en EDK2, Pi 4/5, RK3566, Altra): nVHE, de
// switcher slaapt in WFE, gewekt door de event stream of HOP's SGI-kick.

//go:build tamago && arm64 && !apple && !o6n

#include "textflag.h"
#include "hygiene.h"
#include "sysreg.h"
#include "drop.h"

#include "el2_body.h"
#include "smp_body.h"
#include "switch_body.h"
