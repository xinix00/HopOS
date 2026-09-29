//go:build tamago && riscv64

// De RISC-V64-helft van de node-kant van SMP — en die is leeg, want op dit
// board heeft HOP één hart. De SG2002 heeft twee ongelijke kernen: de C906
// draait HOP, de C906L is het app-slot. Er is dus niets om bij te schakelen.
//
// De API blijft identiek zodat de main arch-neutraal blijft: ConfigureNode is
// een no-op en NodeStarted meldt eerlijk één core. Zodra HopOS een RISC-V-board
// met meerdere HOP-harts krijgt, hoort hier de echte handoff (op ARM: CPU_ON
// naar een EL2-trampoline; hier: het reset-blok + een eigen stub).
package smp

// ConfigureNode: niets bij te schakelen — HOP draait op één hart.
func ConfigureNode(cores int, dispatch func(core int, entry, ctx uint64)) {}

// NodeStarted geeft het aantal HOP-cores dat draait: altijd die ene.
func NodeStarted() int { return 1 }

// Configure is de app-kant: op ARM vraagt een app hiermee extra cores op (via
// de EL2 SMP-trampoline en een gedeelde heap). Op dit board bestaat dat niet —
// er is één app-hart, dus een slot heeft nooit een tweede core om bij te
// schakelen. No-op zodat applib arch-neutraal blijft.
func Configure(prim, cores int, ctrl uintptr) {}
