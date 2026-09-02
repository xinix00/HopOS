// CPU-init voor Apple silicon: de generieke boot (cpu/el2/boot.h) met de drie
// haken die dit platform afdwingt. Alles wat hier staat is wat iBoot en m1n1
// écht anders maken; de vijftien instructies ertussen zijn die van elk board.
//
//   - BOARD_EARLY (earlyApple): de hop en het parkeerprotocol. HopOS hoort op
//     een zuinige core; de firmware levert af op een snelle. Het param-blok
//     wijst de zuinige aan, en zijn wij die niet, dan starten we hem op deze
//     entry en parkeren wij ons voor adoptie als app-core. Daarna de schone
//     lei (TLBI/IC: een geadopteerde core draaide net nog met m1n1's MMU),
//     MPIDR op de scratch, de EL2-vectortabel die élke EL2-exceptie op de
//     dockchannel meldt (ESR/ELR/FAR) en parkeert — tamago's EL1-vectoren
//     dekken EL2 niet — en één regel banner vóór welke regel Go dan ook.
//   - BOARD_EL2 (el2Apple): HCR_EL2 en CNTHCTL_EL2 TERUGGELEZEN op de scratch.
//     Apple's EL2 heeft E2H vast op 1 (geen FEAT_E2H0), en de probe meet dat
//     hier in plaats van het aan te nemen.
//   - BOARD_EL1 (el1Apple): een kale EL1-dumper op VBAR_EL1 vóór tamago zijn
//     eigen vectoren zet — na een kern-flip wees VBAR_EL1 nog naar de vorige
//     kern en viel een vroege fault diens lijk in (GEMETEN 01/02-09).
//
// De EL2-vectortabel wordt in RAM gebouwd (apple.RevokeVec, 16 × {ldr x16,#8;
// br x16; .quad handler}), want een 2KB-uitgelijnd symbool is in Go-asm geen
// zekerheid. Het adres is tegelijk HOP's TrapVecPA: stage2.InitVectors plugt
// er later alleen de HVC-revoke-handler in en laat de foutmelders staan.
//
// TGE blijft 0 (met TGE=1 bestaat EL1 niet), IMO/AMO blijven 0: fysieke
// interrupts richten zich op EL1, waar DAIF ze maskeert — HopOS pollt.
//
// VHE is hier geen keuze maar een feit: de drop (drop.h) schrijft SCTLR_EL1
// onder E2H=1 als sctlr_el12, en dat kiest sysreg.h bij het BOUWEN
// (-asmflags all=-D=VHE, in apple-m4.sh, flip-bundle.sh en de gate). Zonder
// die define zou dezelfde encodering SCTLR_EL2 raken — dus faalt de build
// hieronder met opzet, met de naam van het ontbrekende als foutmelding.

//go:build linkcpuinit

#include "textflag.h"
#include "../../cpu/el2/sysreg.h"
#include "../../cpu/el2/drop.h"

#ifndef VHE
APPLE_CPUINIT_NEEDS_ASMFLAGS_D_VHE
#endif

#define BOOT_SCRATCH    0x1010000E000	// = apple.BootScratch (pariteit: apple.go); +8 = x0
#define HCR_SCRATCH     0x1010000E010	// = apple.HCRScratch
#define CNTHCTL_SCRATCH 0x1010000E018	// = apple.CNTHCTLScratch
#define MPIDR_SCRATCH   0x1010000E020	// = apple.MPIDRScratch
#define PARAM_HOPMPIDR  0x1010000E120	// = apple.ParamBase + paramHopMPIDR
#define PARAM_HOPREL    0x1010000E128	// = apple.ParamBase + paramHopRelease
#define HOP_ALIVE       0x1010000E028	// = apple.HopAlive
#define HOP_PARKPC      0x1010000E030	// = apple.HopParkPC
#define HOP_PARKARG     0x1010000E038	// = apple.HopParkArg
#define HOP_PARKFOR     0x1010000E040	// = apple.HopParkFor
#define EL2_VECTORS     0x101100F0000	// = apple.EL2Vectors = apple.RevokeVec (pariteit: SetupPlan)
#define EL1_VECTORS     0x101100F0800	// el1fault-tabel, direct achter de EL2-tabel (2KB; vrij t/m FlipScratch 0xF8000)
#define DOCK_FALLBACK   0x388128000	// = apple.DockChannelBase

#define TRAP_VEC        EL2_VECTORS	// boot.h zet VBAR_EL2 hierheen — ná earlyApple, die de tabel bouwt
#define BOARD_EARLY     BL ·earlyApple(SB)
#define BOARD_EL2       BL ·el2Apple(SB)
#define BOARD_EL1       BL ·el1Apple(SB)
#include "../../cpu/el2/boot.h"

// earlyApple: vóór alles, op het boot-EL, MMU uit. x9 = x0 bij binnenkomst
// (boot.h), x30 = de terugweg; de banner BL't, dus LR in x22.
TEXT ·earlyApple(SB),NOSPLIT|NOFRAME,$0
	MOVD	R30, R22
	MRS	MPIDR_EL1, R2

	// ── De hop ──────────────────────────────────────────────────────────────
	// Dit staat vóór élke geheugenschrijf, en dat is geen stijl maar noodzaak:
	// m1n1 ROEPT een vrijgegeven core AAN als functie, met zijn eigen MMU nog
	// levend — anders dan de boot-core, die hij vlak vóór de sprong afbreekt.
	// Alles wat die core met caches aan schrijft blijft in zijn cache hangen,
	// onzichtbaar voor de ander die met MMU uit meeleest. Dus: eerst de MMU van
	// deze core uit, dan pas schrijven.
	//
	// Het RELEASE-adres is de bewaker, niet het MPIDR: cpu0's MPIDR is op dit
	// silicium letterlijk 0.
	MOVD	$PARAM_HOPREL, R1
	MOVD	(R1), R6
	CBZ	R6, nohop
	MOVD	$PARAM_HOPMPIDR, R1
	MOVD	(R1), R3
	AND	$0xFFFFFF, R2, R4
	AND	$0xFFFFFF, R3, R3
	CMP	R3, R4
	BNE	dohop

	// Wij ZIJN de zuinige core. Levensteken zetten, mét die ene cacheregel naar
	// geheugen: onze MMU staat nog aan (m1n1 riep ons als functie aan), de
	// wachtende core leest met de MMU uit. Géén cache-onderhoud op set/way —
	// dat raakt gedeelde niveaus terwijl de andere core nog loopt.
	MOVD	$HOP_ALIVE, R1
	MOVD	$1, R5
	MOVD	R5, (R1)
	WORD	$0xd50b7c21		// dc civac, x1
	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd503209f		// sev

	// En dan de MMU van deze core uit, zodat de rest van cpuinit in dezelfde
	// wereld draait als op de boot-core: geen vertaling, geen caches.
	WORD	$0xd53c1005		// mrs x5, sctlr_el2
	BIC	$1<<0, R5		// M — vertaling
	BIC	$1<<2, R5		// C — data-cache
	BIC	$1<<12, R5		// I — instructie-cache
	WORD	$0xd51c1005		// msr sctlr_el2, x5
	ISB	$15
	B	nohop

dohop:
	// m1n1's spin-table: args[0..3] op target+8, het target-woord op target+0.
	// Volgorde is de zijne: args, dsb, target, sev (pariteit: params.go Release).
	MOVD	R9, 8(R6)		// x0 doorgeven zoals wij hem kregen
	MOVD	ZR, 16(R6)
	MOVD	ZR, 24(R6)
	MOVD	ZR, 32(R6)
	WORD	$0xd5033f9f		// dsb sy
	MOVD	$cpuinit(SB), R5
	MOVD	R5, (R6)
	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd503209f		// sev

	// Wachten op zijn levensteken. Komt het niet, dan redden we onszelf en
	// booten we door als HOP op deze core: een mislukte wissel hoort een regel
	// op de console te zijn, geen baksteen.
	MOVD	$HOP_ALIVE, R1
	MOVD	$0x4000000, R7
waitalive:
	MOVD	(R1), R5
	CBNZ	R5, park
	SUB	$1, R7
	CBNZ	R7, waitalive
	B	nohop			// zelfredding

park:
	// Geparkeerd tot HOP ons adopteert. Hij schrijft eerst het argument, dan de
	// entry, en als laatste vóór wie het bedoeld is. Wij komen daar aan zoals
	// élke door m1n1 vrijgegeven core: EL2, x0 = het argument.
	//
	// Het adreswoord is niet optioneel. Wie hier alleen op de entry wacht
	// vertrekt met het werk van de éérste core die HOP daarna start, wie die
	// ook is — en dan draaien er twee cores op dezelfde app-entry. Zelfde
	// protocol als stubReset in bootstub.s, inclusief het ontvangstbewijs.
	MRS	MPIDR_EL1, R3
	AND	$0xFFFF, R3, R3		// aff1:aff0 — ons adres in de brievenbus
	MOVD	$HOP_PARKFOR, R4
	MOVD	$HOP_PARKPC, R1
parkloop:
	WFE
	MOVD	(R4), R5
	CMP	R3, R5
	BNE	parkloop
	MOVD	(R1), R6
	CBZ	R6, parkloop
	MOVD	$HOP_PARKARG, R2
	MOVD	(R2), R0
	// De entry staat veilig in R6 vóór we bevestigen: zodra het bewijs er ligt
	// mag HOP de bus opnieuw vullen voor de volgende core.
	MOVD	$-1, R5
	MOVD	R5, (R4)
	WORD	$0xd5033f9f		// dsb sy
	JMP	(R6)

nohop:
	// Schone lei voor de vertaling. De boot-core krijgt die gratis — m1n1 doet
	// vlak voor de sprong een volledige mmu_shutdown — maar een core die wij
	// uit zijn spin-table adopteren niet: die draaide zojuist nog MET m1n1's
	// MMU en houdt zijn vertalingen in de TLB. tamago zet daarna onze tabellen
	// aan en gaat uit van een lege TLB, dus dan wint m1n1's oude 1GB-blok voor
	// het lage DRAM en geeft élke lees daar een "address size fault, level 0" —
	// terwijl tabellen, TTBR0, TCR en SCTLR aantoonbaar goed staan (GEMETEN
	// 29-08 op de gehopte core, met een softwarewandeling ernaast).
	//
	// Hier, en niet alleen in de hop-tak: het kost niets op een core die al
	// schoon is, en elke manier waarop een core hier binnenkomt heeft hetzelfde
	// nodig.
	WORD	$0xd508871f	// tlbi vmalle1 — m1n1's EL1&0-vertalingen
	WORD	$0xd50c871f	// tlbi alle2   — en zijn eigen
	WORD	$0xd508751f	// ic iallu
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd5033fdf	// isb

	MOVD	$MPIDR_SCRATCH, R1
	MRS	MPIDR_EL1, R2
	MOVD	R2, (R1)

	// EL2-vectortabel bouwen: 16 ingangen van 0x80 bytes, elk
	//   ldr x16, #8      (0x58000050)
	//   br  x16          (0xd61f0200)
	//   .quad el2fault
	// Vóór boot.h VBAR_EL2 erheen zet (TRAP_VEC), dus hier en niet in el2Apple.
	MOVD	$EL2_VECTORS, R1
	MOVD	$el2fault(SB), R2
	MOVD	$16, R3
	MOVD	$0x58000050, R4
	MOVD	$0xd61f0200, R5
build:
	MOVW	R4, (R1)
	MOVW	R5, 4(R1)
	MOVD	R2, 8(R1)
	ADD	$0x80, R1
	SUB	$1, R3
	CBNZ	R3, build
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd508751f	// ic iallu — de tabel is code, net geschreven
	WORD	$0xd5033fdf	// isb

	// Eerste licht: één regel op de UART vóór de sprong naar EL1 en dus vóór
	// élke regel Go. Op nieuw silicium is dit het verschil tussen "hij deed
	// niets" en "hij kwam tot hier, met dít in x0" — en dat verschil kost
	// anders een bootcyclus om te achterhalen. Het param-blok mag ontbreken:
	// zodra wij het bootobject zijn is dat de normale toestand.
	MOVD	$DOCK_FALLBACK, R10
	MOVD	$·msgBoot(SB), R11
	MOVD	$9, R15
	BL	puts(SB)
	MOVD	R9, R0
	BL	puthex(SB)
	MOVD	$·msgNL(SB), R11
	MOVD	$2, R15
	BL	puts(SB)

	MOVD	R22, R30
	RET

// el2Apple: op EL2, ná boot.h's HCR = RW. De teruggelezen waarden gaan op de
// scratch: de probe meet zo of we nVHE of VHE draaien (E2H blijft 1 zonder
// FEAT_E2H0), en of de EL1-tellerbits in beide lay-outs aankwamen. De drop
// zet CNTHCTL daarna nog eens — idempotent, en de meting is dan al gedaan.
//
// NIET PROBEREN: Apple's poort voor de timer-FIQ (s3_5_c15_c1_3, m1n1's
// SYS_IMP_APL_VM_TMR_FIQ_ENA_EL2) is op t8132 VERGRENDELD — een enkele mrs
// geeft `!!EL2 ESR=0000000002000000` (undefined), gemeten 29-08. m1n1 wist het
// al: features_m4 mist apple_sysregs_unlocked, met een XXX erboven. Gevolg: op
// een core die de firmware niet zelf configureerde bereikt de timer-FIQ de
// core niet, en WFI slaapt daar voor eeuwig terwijl de timer wél afgaat. Het
// board meet dat (apple.TimerWakes) in plaats van het aan te nemen.
TEXT ·el2Apple(SB),NOSPLIT|NOFRAME,$0
	ISB	$15
	WORD	$0xd53c1100	// mrs x0, hcr_el2
	MOVD	$HCR_SCRATCH, R1
	MOVD	R0, (R1)

	// CNTHCTL_EL2: EL1-toegang tot fysieke teller en timer, in beide lay-outs:
	//   E2H=0: bit0 EL1PCTEN, bit1 EL1PCEN
	//   E2H=1: bit10 EL1PCTEN, bit11 EL1PTEN (bits 0/1 zijn dan EL0-toegang)
	WORD	$0xd53ce100	// mrs x0, cnthctl_el2
	ORR	$0b11, R0, R0
	ORR	$0b11<<10, R0, R0
	WORD	$0xd51ce100	// msr cnthctl_el2, x0
	WORD	$0xd53ce100	// mrs x0, cnthctl_el2
	MOVD	$CNTHCTL_SCRATCH, R1
	MOVD	R0, (R1)
	RET

// el1Apple: op EL1, vóór SCTLR en de stack. EERST de EL1-vectoren op onze
// eigen kale dumper: vóór deze regel wijst VBAR_EL1 naar wat de vórige
// bewoner van deze core achterliet — na een kern-flip de tamago-vectoren van
// de OUDE kern, en een vroege fault viel daardoor diens lijk in: een hybride
// trace met oude g-nummers die de echte ESR/ELR/FAR opat (GEMETEN 01/02-09,
// flips 6-8). De tabel hergebruikt el1fault op elke 0x80; tamago zet er bij
// zijn runtime-init zijn eigen vectoren overheen — dit dekt precies het gat
// daarvóór, op élke boot-route (iBoot, m1n1 én flip).
TEXT ·el1Apple(SB),NOSPLIT|NOFRAME,$0
	MOVD	$EL1_VECTORS, R1
	MOVD	$el1fault(SB), R2
	MOVD	$16, R3
	MOVD	$0x58000050, R4
	MOVD	$0xd61f0200, R5
buildel1:
	MOVW	R4, (R1)
	MOVW	R5, 4(R1)
	MOVD	R2, 8(R1)
	ADD	$0x80, R1
	SUB	$1, R3
	CBNZ	R3, buildel1
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd508751f	// ic iallu — de tabel is code
	WORD	$0xd5033fdf	// isb
	MOVD	$EL1_VECTORS, R0
	WORD	$0xd518c000	// msr vbar_el1, x0
	RET

// el1fault: de EL1-tegenhanger van el2fault — zelfde vorm, EL1-registers.
// Bestaansreden: het venster tussen de drop naar EL1 en tamago's eigen
// vectorinstallatie. Meldt en parkeert; el2fault's helpers doen het werk.
TEXT el1fault(SB),NOSPLIT|NOFRAME,$0
	MOVD	$DOCK_FALLBACK, R10
	MOVD	$·msgESR(SB), R11
	MOVD	$12, R15
	BL	puts(SB)
	WORD	$0xd5385200	// mrs x0, esr_el1
	BL	puthex(SB)
	MOVD	$·msgELR(SB), R11
	MOVD	$5, R15
	BL	puts(SB)
	WORD	$0xd5384020	// mrs x0, elr_el1
	BL	puthex(SB)
	MOVD	$·msgFAR(SB), R11
	MOVD	$5, R15
	BL	puts(SB)
	WORD	$0xd5386000	// mrs x0, far_el1
	BL	puthex(SB)
	MOVD	$·msgNL(SB), R11
	MOVD	$2, R15
	BL	puts(SB)
el1park:
	WFE
	B	el1park

// el2fault: de landingsplaats van élke EL2-exceptie — er horen er geen te
// komen. Meldt ESR/ELR/FAR op de dockchannel, rechtstreeks (geen runtime, geen
// stack), en parkeert. De dockchannel-basis komt uit het param-blok; is die 0,
// dan alleen parkeren.
//
// Registerafspraak van de helpers hieronder: R10 = UART-basis, R11 = string,
// R15 = lengte, R12 = byte, R0 = getal; R20/R21 bewaren LR over de nesting.
TEXT el2fault(SB),NOSPLIT|NOFRAME,$0
	// De dockchannel-basis als constante: dit blok draait vóór élke lezer, en
	// een foutmelder die van een param-blok afhangt zwijgt juist wanneer het
	// misgaat.
	MOVD	$DOCK_FALLBACK, R10
	MOVD	$·msgESR(SB), R11
	MOVD	$12, R15
	BL	puts(SB)
	WORD	$0xd53c5200	// mrs x0, esr_el2
	BL	puthex(SB)
	MOVD	$·msgELR(SB), R11
	MOVD	$5, R15
	BL	puts(SB)
	WORD	$0xd53c4020	// mrs x0, elr_el2
	BL	puthex(SB)
	MOVD	$·msgFAR(SB), R11
	MOVD	$5, R15
	BL	puts(SB)
	WORD	$0xd53c6000	// mrs x0, far_el2
	BL	puthex(SB)
	MOVD	$·msgHPF(SB), R11
	MOVD	$7, R15
	BL	puts(SB)
	WORD	$0xd53c6080	// mrs x0, hpfar_el2 — ≠0 alleen bij een stage-2-fault
	BL	puthex(SB)
	MOVD	$·msgSPSR(SB), R11
	MOVD	$6, R15
	BL	puts(SB)
	WORD	$0xd53c4000	// mrs x0, spsr_el2 — M[3:0] zegt vanaf welk EL
	BL	puthex(SB)
	MOVD	$·msgHCR(SB), R11
	MOVD	$5, R15
	BL	puts(SB)
	WORD	$0xd53c1100	// mrs x0, hcr_el2 — bit 0 (VM) zegt of stage 2 leeft,
				// en dus of een EL1-abort hier HOORT te landen
	BL	puthex(SB)
	MOVD	$·msgNL(SB), R11
	MOVD	$2, R15
	BL	puts(SB)
hang:
	WFE
	B	hang

// putc: R12 → dockchannel op R10. Begrensde poll op TX_FREE (+0x4014), dan
// TX8 (+0x4004). Clobbert R13, R14.
TEXT putc(SB),NOSPLIT|NOFRAME,$0
	MOVD	$100000, R14
wait:
	MOVWU	0x4014(R10), R13
	CBNZ	R13, go
	SUB	$1, R14
	CBNZ	R14, wait
	RET			// FIFO blijft vol: byte laten vallen
go:
	MOVW	R12, 0x4004(R10)
	RET

// puts: R15 bytes vanaf R11.
TEXT puts(SB),NOSPLIT|NOFRAME,$0
	MOVD	R30, R20
next:
	CBZ	R15, done
	MOVBU	(R11), R12
	ADD	$1, R11
	SUB	$1, R15
	BL	putc(SB)
	B	next
done:
	MOVD	R20, R30
	RET

// puthex: R0 als 16 hexcijfers.
TEXT puthex(SB),NOSPLIT|NOFRAME,$0
	MOVD	R30, R21
	MOVD	$16, R16
digit:
	LSR	$60, R0, R12
	AND	$0xF, R12, R12
	CMP	$10, R12
	BLT	num
	ADD	$87, R12, R12	// 'a' - 10
	B	emit
num:
	ADD	$48, R12, R12	// '0'
emit:
	BL	putc(SB)
	LSL	$4, R0, R0
	SUB	$1, R16
	CBNZ	R16, digit
	MOVD	R21, R30
	RET

DATA	·msgESR+0(SB)/8, $"\r\n!!EL2 "
DATA	·msgESR+8(SB)/4, $"ESR="
GLOBL	·msgESR(SB), RODATA|NOPTR, $12
DATA	·msgELR+0(SB)/5, $" ELR="
GLOBL	·msgELR(SB), RODATA|NOPTR, $5
DATA	·msgFAR+0(SB)/5, $" FAR="
GLOBL	·msgFAR(SB), RODATA|NOPTR, $5
DATA	·msgHPF+0(SB)/7, $" HPFAR="
GLOBL	·msgHPF(SB), RODATA|NOPTR, $7
DATA	·msgSPSR+0(SB)/6, $" SPSR="
GLOBL	·msgSPSR(SB), RODATA|NOPTR, $6
DATA	·msgHCR+0(SB)/5, $" HCR="
GLOBL	·msgHCR(SB), RODATA|NOPTR, $5
DATA	·msgNL+0(SB)/2, $"\r\n"
GLOBL	·msgNL(SB), RODATA|NOPTR, $2
DATA	·msgBoot+0(SB)/8, $"\r\nhopos "
DATA	·msgBoot+8(SB)/1, $"x"
GLOBL	·msgBoot(SB), RODATA|NOPTR, $9

