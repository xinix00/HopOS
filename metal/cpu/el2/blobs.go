package el2

// De EL2-blobs: de stukken van dit pakket die een APP-CORE uitvoert, en die
// daarom niet in het kern-image mogen blijven wonen (docs/kern-flip.md). Ze
// worden bij de boot naar de plan-regio gekopieerd, zodat een kern-flip zijn
// eigen venster kan verlaten terwijl geyielde en geparkeerde cores gewoon
// doordraaien.
//
// Deze lijst is de ÉNE beschrijving van die verzameling. Drie plekken leunen
// erop en moeten het over exact dezelfde bytes in dezelfde volgorde eens zijn,
// anders vergelijken ze appels met peren:
//
//   - kern/stage2 kopieert ze en rekent er de som over (installSwitchCode);
//   - kern/kernflip rekent dezelfde som over de blobs ín een nieuwe
//     kern-bundel, om te weten of een flip met levende bewoners mag;
//   - de asm zelf (switch.s, el2.s, smp.s) levert de eindmarkers.
//
// Komt er ooit een vierde blob bij, dan is dít de plek — en dan volgen de
// andere twee vanzelf in plaats van half.
var BlobSymbols = [][2]string{
	{"el2entry", "el2entryEnd"},
	{"s2tramp", "s2trampEnd"},
	{"smpEL2Tramp", "smpEL2TrampEnd"},
}

// MaxBlobSize is de bovengrens per blob. Een groter bereik tussen een symbool
// en zijn eindmarker betekent dat de linker ze uit elkaar heeft getrokken
// (of dat een marker verschoven is), en dat hoort hard te vallen — niet
// stilzwijgend megabytes te kopiëren of te hashen.
const MaxBlobSize = 0x2000
