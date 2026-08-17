//go:build tamago

package board

// (verwijzing: abi/layout/hopcore.go)

// hopLotteryArm64 is de asm-kant (hoplottery_arm64.s): de PSCI-variant van
// de boot-hart-loterij — voorbereid, nog niet bedraad. Eén implementatie
// voor álle ARM-boards, want PSCI CPU_ON is daar de standaard-startknop
// (dezelfde die de app-cores al gebruiken). Contract als de riscv64-versie:
// draait als allereerste op het boot-hart; HopCore (abi/layout) == 0 maakt hem een no-op.
func hopLotteryArm64()
