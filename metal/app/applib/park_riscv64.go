//go:build tamago && riscv64

package applib

import "github.com/xinix00/HopOS/metal/cpu/idle"

// parkExit geeft het hart aan HOP terug. De status (Exited of Staged) staat al
// op de control page; wat hier nog moet gebeuren is één ding: ophouden te
// bestaan zónder iemand op te houden.
//
// Dat is de exit-trap — ONvoorwaardelijk, precies zoals ARM's hvc #0. HOP's
// switcher (cpu/mmode) meldt deze bewoner dood (CtxDead) en roteert weg: een
// buurman draait door, en HOP's fase 2 ziet de dood waar hij op wacht. De
// eerste vorm hiervan trapte alleen als CtrlShared aanstond, en dat was fout op
// de manier die alleen op ijzer opvalt: de laatste levende bewoner van een hart
// heeft CtrlShared 0, spinde dus hier, en HOP's StartStaged wachtte zijn twee
// seconden vol op een CtxDead die nooit kwam (gemeten 31-07: "place staged
// slot 2: loader still live on shared core 1" — na een geslaagde staging).
//
// Draait dit hart de app in M-mode (geen supervisor-modus in misa), dan is de
// mtvec nog van de kooi-stub en parkeert díe het hart op de trap — functioneel
// hetzelfde einde: stil wachten tot HOP het hart reset. Keert nooit terug.
func parkExit() {
	for {
		idle.ExitTrap()
	}
}
