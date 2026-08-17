// VOORSTEL→BEDRAAD (16-08, Derek: "gewoon COREID — HOP altijd in coreX,
// het principe is globaal"): één universeel begrip voor waar HopOS woont,
// over beide arches, als PLAN-kennis — daarom hier in layout en niet in
// board (de board-basis mag board niet importeren, en waar HOP woont is
// geen board-gedrag maar een afspraak, net als de andere adressen hier).
//
// HopCore is de core waar HOP zelf leeft. 0 = de core waar de firmware ons
// startte (de default op élk board; elke loterij is dan een no-op). Anders
// verhuist de boot via de arch-loterij vóór vrijwel alles — riscv64:
// board/licheerv/lottery_riscv64.s (vendor-reset-vector, gehaakt in Init),
// arm64: board/hoplottery_arm64.s (PSCI, voorbereid voor de 12-corer).
//
// Waarom een getal en geen rol-vlag: "HOP woont op core N" dekt méér dan
// snelle-vs-zuinige cores — ook een DEFECTE core is dan configuratie.
// Later een hopcfg-sleutel (hopos.hopcore) voor per-kaart instellen.
package layout

// HopCore: zie boven. Const en geen var: de wissel gebeurt vóór de
// runtime, dus runtime-verstelbaar zou een leugen zijn.
// 1 (16-08, poging 2): de loterij woont nu in de kernel-cpuinit — caches
// uit, vers uit reset, vóór álles — precies het contract van de asm. De
// stranding van poging 1 (Init/Hwinit1, caches aan, cores niet coherent)
// staat in ledger r.53; de zelfredding is nu ook écht schoon, want op dat
// moment is er nog niets aangeraakt.
const HopCore = 1
