// Package thead is het CPU-PROFIEL van de XuanTie C906 — de middelste van drie
// lagen waarin de RISC-V-kant van HopOS uiteenvalt:
//
//	ISA-generiek RISC-V   wat de spec garandeert: privilege-modes, de PMP-
//	                      codering (TOR/NAPOT), het Sv39-tabelformaat, satp,
//	                      sfence.vma, ecall/mret. Staat in kern/cage,
//	                      cpu/slotstart en cpu/mmode.
//	CPU-profiel (dit)     wat DIT silicium erbij of anders doet: hoeveel
//	                      PMP-entries er zijn, hoe breed het fysieke adres is,
//	                      welke korrel PMP aanhoudt, welke PTE-bits de
//	                      cache-attributen dragen, en hoe je een cacheregel
//	                      wegveegt.
//	board-glue            wat aan het BORDJE hangt: tijdbasis, reset-blok,
//	                      MMIO-adressen, DRAM-indeling. Staat in board/licheerv.
//
// Waarom dit een eigen laag is en niet een handvol constanten in kern/cage: geen
// van de getallen hieronder volgt uit "riscv64". Ze volgen uit deze kern, en een
// tweede RISC-V-board (SBI, of met H-extensie) heeft andere. Zolang ze in de
// isolatiecode zelf stonden, was "riscv64" in deze boom stilzwijgend een synoniem
// voor "C906" — en dan ontdek je bij het tweede board welke aannames er zaten in
// plaats van ze te kunnen lezen.
//
// Het is bewust een BUILD-TIME seam en geen runtime-capability-framework: één
// binary draait op één architectuur, dus de vraag "welke CPU?" is bij het bouwen
// al beslist. Een tweede profiel is een tweede pakket met dezelfde namen.
//
// Bronnen: XuanTie C906 User Manual (de th.*-instructies en de mxstatus/mcor/
// mhcr/mhint-CSR's) en de vendor-kernel in lab/licheerv/vendor — met name
// linux_5.10/arch/riscv/mm/cacheflush.c (de cache-encodings) en
// include/asm/pgtable-bits.h (_PAGE_BUF/_PAGE_CACHE).
package thead

// De PMP-eigenschappen van deze kern. Alle drie zijn implementation-defined in de
// RISC-V-spec, dus ze horen hier en niet in de codering die ze gebruikt.
const (
	// PMPEntries is hoeveel PMP-entries we zeker hebben. De C906-spec zegt 8;
	// T-Head levert mogelijk 16, maar dat is niet gemeten en we rekenen met wat
	// vaststaat. Elk TOR-venster kost er twee (onder- én bovengrens).
	PMPEntries = 8

	// PABits is de fysieke adresbreedte die een deny-all moet dekken.
	PABits = 40
)

// De uitgebreide PTE-attributen. Bit 61/62 bestaan NIET in de RISC-V-spec: het
// zijn T-Head-uitbreidingen die actief worden zodra MAEE in mxstatus aanstaat (en
// dat zet de kooi-stub). Staan ze op nul, dan is de pagina device-achtig —
// strong-ordered en ongebufferd — en faalt een atomic erop.
//
// GEMETEN 31-07: zonder deze twee sloeg de app om in "Store/AMO access fault"
// (mcause 7) op zijn eigen stack, in runtime.check, bij de eerste atomic van de
// Go-runtime. Normaal RAM krijgt ze dus allebei, precies zoals PAGE_KERNEL in de
// vendor-kernel; MMIO krijgt ze niet — daar wil je juist device-semantiek.
const (
	PTEBufferable = 1 << 61
	PTECacheable  = 1 << 62
)
