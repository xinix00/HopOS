//go:build arm64

package hopslot

import "github.com/usbarmory/tamago/arm64"

// esrEL1/farEL1/elrEL1: zie exc_arm64.s.
func esrEL1() uint64
func farEL1() uint64
func elrEL1() uint64

// reportException is wat een gekooide app bij een EL1-exception meldt vóór
// tamago hem afmaakt: ESR, FAR en ELR. Zonder dit is élke exception "EL1
// exception" plus een PC, en dat onderscheidt een undefined instruction niet
// van een data abort of een SError — GEMETEN 02-09 op de M4: drie boot-cycli
// om te zien dat een MSR naar een IMP-DEF-register faultte, en nog steeds
// niet wáárom. Geen allocatie: dit draait op de system stack, midden in de
// exception.
func reportException(pc uintptr) {
	esr, far, elr := esrEL1(), farEL1(), elrEL1()
	print("EL1 exception: esr=", esr, " ec=", esr>>26, " far=", far, " elr=", elr, " pc=", pc, "\n")
	arm64.DefaultExceptionHandler(pc)
}
