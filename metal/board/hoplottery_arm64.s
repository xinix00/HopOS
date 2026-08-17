// VOORSTEL (16-08, nog niet bedraad): de boot-hart-loterij voor ARM — de
// PSCI-broer van board/licheerv/hop/lottery_riscv64.s. Waar riscv64 een
// vendor-reset-vector moet poken, is dit op ARM één standaard-SMC:
// CPU_ON(HopCore, onze eigen entry). Daarna parkeert het boot-hart zichzelf
// op het adoptie-woord (zelfde contract: scratch +64/+72/+80) tot HOP —
// levend op HopCore — er de parkeer-entry van de EL2-switch in schrijft.
// Geen wfe-afhankelijkheid: een spinlus, het duurt hooguit de bootseconden.
//
// Bedrading (per board, later): scratch-adres als immediate zoals de
// riscv64-loterij dat doet, en de aanroep vóór rt0. HopCore == 0 (de
// default, abi/layout/hopcore.go) maakt dit een no-op — geen board merkt er
// iets van tot hij de knop omzet.

//go:build tamago

#include "textflag.h"

#define PSCI_CPU_ON 0xC4000003 // SMC64: x1=doel-MPIDR, x2=entry, x3=context

// func hopLotteryArm64()
TEXT ·hopLotteryArm64(SB), NOSPLIT|NOFRAME, $0
	// HopCore 0 = no-op: de firmware-core ís de woonplaats. De vergelijking
	// met MPIDR_EL1.Aff0 volgt bij de bedrading; dit voorstel legt het
	// PSCI-recept en het parkeer-contract vast.
	MOVD	$0, R0		// board.HopCore (spiegel; init-check bij bedrading)
	CBZ	R0, klaar

	// PSCI CPU_ON: start HopCore op onze eigen entry.
	MOVD	$PSCI_CPU_ON, R0
	MOVD	$1, R1		// doel-MPIDR (Aff0 = HopCore; bedrading vult in)
	MOVD	$_rt0_arm64_tamago(SB), R2
	MOVD	$0, R3
	SMC	$0

	// Parkeren op het adoptie-woord — scratch per board (immediate bij de
	// bedrading, zoals de riscv64-loterij zijn boot-scratch draagt).
	// +72 = adoptie-PC: 0 = wachten, 1 = afgeblazen (terug als HOP), anders
	// erin springen als kersvers app-hart.
klaar:
	RET
