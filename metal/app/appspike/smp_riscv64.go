//go:build riscv64

package main

// smp_riscv64.go — de ARM-kant van de referentie-app op RISC-V: weigeren met
// een reden, niet weglaten. Zo bouwt appspike op élke arch die HopOS draagt, en
// dat is precies wat je op een board wilt hebben staan als er iets níet werkt:
// de netdemo-tweeling (NETDEMO listen/dial) is het instrument waarmee je meet
// of twee slots elkaar bereiken, en die is architectuur-vrij.
//
// Wat hier weigert:
//
//   - firmwareProbe: op ARM is een SMC uit een kooi een isolatiebreuk die EL2
//     hoort te trappen. Op deze RISC-V-boards draait de kooi zelf in M-mode
//     (PMP-isolatie, zie kern/cage) en is een ecall juist de LEGITIEME weg naar
//     HOP (yield/exit). Dezelfde proef bestaat hier dus niet; hem stil laten
//     slagen zou een isolatiebewijs suggereren dat er niet is.
//   - smpBench: één app op meerdere harts loopt op deze boards niet (er is één
//     app-hart), en zonder een core-onderscheider is er niets te bewijzen.

import "github.com/xinix00/HopOS/metal/app/applib"

func firmwareProbe(app *applib.App) {
	exitf(app, 1, "PROBE=smc: not applicable on riscv64 -- a cage runs in M-mode here, "+
		"and an ecall is the legitimate path to HOP (yield/exit), not an isolation breach")
}

func smpBench(app *applib.App) {
	exitf(app, 1, "SMP=bench: this board runs one app hart -- nothing to prove")
}
