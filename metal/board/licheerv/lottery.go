// De boot-hart-loterij: "HopOS woont op HopHart" als boekhouding op het
// enige moment dat er niets te verplaatsen valt — vóór alles, in de kernel-cpuinit
// (hop/cpuinit_riscv64.s, -tags linkcpuinit). Het volledige verhaal en het parkeer/adoptie-
// contract: zie die asm en abi/layout/hopcore.go (het
// globale principe). De FSBL start dit image op de grote core; is HopHart
// een ander hart, dan start de loterij dát hart op onze eigen entry en
// parkeert de boot-core zichzelf tot HOP hem adopteert als app-hart
// (AdoptParked, aangeroepen door hop.HartOn).
package licheerv

import "github.com/xinix00/HopOS/metal/abi/layout"

// HopHart is layout.HopCore vertaald naar dit silicium: 0 = C906B (1GHz,
// de firmware-core), 1 = C906L (700MHz). Eén globale knop; dit board
// leest hem alleen maar.
const HopHart = layout.HopCore

// Het loterij-blok op de boot-scratch (naast het hartprobe-blok, +0..+40).
const (
	LotteryProgress uintptr = 64 // 1 = boot-core geparkeerd, wacht op adoptie
	LotteryAdoptPC  uintptr = 72 // adoptie-PC (eenmalig; kern/slots cageInit)

	// LotteryHopAlive: HopHart schrijft hier 1 zodra zíjn loterij-pass
	// draait ("ik leef, parkeer gerust door"). Blijft dit uit, dan redt de
	// geparkeerde core zichzelf na de timeout en boot hij door als HOP in
	// de oude rolverdeling — een mislukte wissel is dan een console-regel,
	// geen baksteen.
	LotteryHopAlive uintptr = 88

	// LotteryParkArg: het argument dat de adoptie meegeeft. De parkeerlus
	// springt met X11 = dit woord naar de adoptie-PC (cpuinit_riscv64.s,
	// adoptie:) — voor de boot-intrek van de switcher (cpu/mmode parkenter)
	// is dat het sched-blok van deze core; een kooi-stub-entry negeert X11.
	// HOP schrijft dit woord vóór het adoptie-PC-woord (adoptParked).
	LotteryParkArg uintptr = 96
)
