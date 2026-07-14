// UEFI-entry voor HopOS — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit, zie de package-doc voor de vier boot-stappen).
//
// De firmware laadt de PE (mkkernel -pe) op een WILLEKEURIG adres en roept
// cpuinit als gewone AAPCS64-functie aan (UEFI op AArch64 = standaard-ABI):
// x0 = ImageHandle, x1 = *SystemTable; MMU aan, identity-mapped, op de
// UEFI-stack. Alle code hier tot aan de sprong is dus positie-onafhankelijk:
// symboolreferenties via MOVD $·sym(SB) zijn ADRP+ADD (PC-relatief) en wijzen
// naar de geladen (B-)kant; de "slide" B−L komt uit het verschil met het
// absolute linkadres dat als DATA-woord in de image ligt (·bootKernelVA).
//
// Callee-saved-registers door de hele stub (UEFI-calls bewaren ze, AAPCS64):
//   R19 = ImageHandle   R22 = slide (geladen − gelinkt)
//   R20 = *SystemTable  R23 = RamStart (linkbasis L)
//   R21 = *BootServices R24 = imagegrootte in bytes (runtime.end − RamStart)
//                       R26 = ExitBootServices-pogingenteller
//
// UEFI-tabeloffsets (UEFI-spec 2.x, 64-bit): SystemTable: ConOut=0x40,
// BootServices=0x60. BootServices: AllocatePages=0x28, GetMemoryMap=0x38,
// ExitBootServices=0xe8. SIMPLE_TEXT_OUTPUT: OutputString=0x08.

//go:build linkcpuinit

#include "textflag.h"

// Moet gelijk zijn aan memmapCap in uefi.go (asm kent geen Go-constanten).
#define MEMMAP_CAP 0x40000

// Pariteit met board.go (Go-init checkt): de carve — het stuk tussen de
// Go-RAM (RamSize) en het einde van de claim — draagt het layout-plan
// (ctrl/ringen/stage-2/net-DMA), en REVOKE_OFF is waar cpuinit VBAR_EL2
// van de HOP-core heen zet (RamStart + offset; layout.RevokeVecPA).
#define CARVE_SIZE 0x12000000
#define REVOKE_OFF 0x10900800

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	// EL-discriminator: de firmware roept ons als UEFI-app op EL2 aan;
	// een APP-CORE onder stage-2 entreert dit zelfde symbool op EL1 (de
	// el2-trampoline ERET't naar de image-entry) — dáár is geen firmware
	// en geen SystemTable, dus direct de runtime in (gemeten 13-07 avond:
	// zonder deze check deed de app-stub ConOut-calls op wilde pointers →
	// stage-2-fault → parkeer-lus bij elke jobstart).
	MRS	CurrentEL, R2
	LSR	$2, R2, R2
	AND	$0b11, R2, R2
	CMP	$1, R2
	BNE	fwentry
	// EL1 hoort een app-core onder stage-2 te zijn: dan draaien we op het
	// linkadres (HOP kopieerde de image daarheen) en is de slide 0. Een
	// ECHTE EL1-firmware-entry (gehoste UEFI zonder EL2) laadt ons op een
	// willekeurig adres — zonder claim/relocatie doorstarten zou wilde
	// stores in firmware-geheugen geven (review #12): parkeren.
	MOVD	$·bootKernel(SB), R2	// PC-relatief: geladen adres
	MOVD	·bootKernelVA(SB), R3	// absoluut: linkadres
	CMP	R2, R3
	BNE	el1hang
	B	·uefiEL1(SB)
el1hang:
	WFE
	B	el1hang
fwentry:
	MOVD	R0, R19			// ImageHandle
	MOVD	R1, R20			// *SystemTable

	// Werkruimte voor out-parameters op de (UEFI-)stack:
	//   0(RSP)=AllocatePages-adres  8(RSP)=MapSize  16(RSP)=MapKey
	//   24(RSP)=DescSize            32(RSP)=DescVer
	SUB	$64, RSP

	// Eerste levensteken op de firmware-console: ConOut->OutputString.
	// Fouten negeren (headless firmware bestaat; de echte console volgt
	// via ACPI SPCR).
	MOVD	0x40(R20), R0		// ConOut
	MOVD	$·strBanner(SB), R1
	MOVD	0x08(R0), R8		// OutputString
	WORD	$0xd63f0100		// blr x8

	MOVD	0x60(R20), R21		// *BootServices

	// GOP: het firmware-beeld opvragen zolang boot services leven — beeld
	// = firmware-buffer (fb.Init op de Go-kant), geen driver: het HopOS-
	// principe. Alleen 32bpp lineair (PixelFormat 0/1); BltOnly of geen
	// GOP → fbBase blijft 0 en het scherm blijft gewoon uit.
	// Stack-slots: 40(RSP)=fbBase, 48(RSP)=hoogte<<32|breedte,
	// 56(RSP)=PixelFormat<<32 | pixels-per-scanlijn.
	MOVD	$0, R0
	MOVD	R0, 40(RSP)
	MOVD	$·gopGUID(SB), R0
	MOVD	$0, R1
	MOVD	RSP, R2			// slot 0 tijdelijk als **Interface
	MOVD	0x140(R21), R8		// LocateProtocol
	WORD	$0xd63f0100		// blr x8
	CBNZ	R0, nogop
	MOVD	(RSP), R0		// *GOP
	CBZ	R0, nogop
	MOVD	24(R0), R0		// ->Mode
	CBZ	R0, nogop
	MOVD	8(R0), R1		// ->Info
	CBZ	R1, nogop
	MOVWU	12(R1), R4		// PixelFormat (0=RGB, 1=BGR); >1 = niet-lineair
	CMP	$2, R4
	BGE	nogop
	MOVD	24(R0), R3		// FrameBufferBase
	MOVD	R3, 40(RSP)
	MOVWU	4(R1), R2		// HorizontalResolution
	MOVWU	8(R1), R3		// VerticalResolution
	LSL	$32, R3
	ORR	R3, R2
	MOVD	R2, 48(RSP)
	MOVWU	32(R1), R2		// PixelsPerScanLine (past in 32 bits)
	LSL	$32, R4			// PixelFormat in de hoge helft ernaast
	ORR	R4, R2
	MOVD	R2, 56(RSP)
nogop:

	// GEEN EFI_RNG_PROTOCOL-call meer: GetRNG bleek te kunnen blokkeren in
	// een eeuwige entropie-poll (QEMU/EDK2 zonder werkende TRNG, gemeten
	// 13-07 avond — PC danste in firmware-code en de boot stond stil).
	// De DRBG (uefi.go) seedt daarom uit timing-jitter (initRNG); een
	// hardware-TRNG-pad (begrensde EFI-call of SMCCC TRNG) blijft backlog.

	// slide = geladen − gelinkt (·bootKernelVA is een absoluut DATA-woord).
	MOVD	$·bootKernel(SB), R0	// PC-relatief: geladen adres
	MOVD	·bootKernelVA(SB), R2	// absoluut: linkadres
	SUB	R2, R0, R22

	// Imagegrootte (runtime.end = einde BSS, gelinkt; RamStart is door
	// mkkernel -pe gepatcht naar het linkadres van déze variant — 0
	// betekent: niet door mkkernel verpakt, dan is booten zinloos).
	MOVD	runtime∕goos·RamStart(SB), R23
	CBZ	R23, hang
	MOVD	·imageEndVA(SB), R24
	SUB	R23, R24

	// Eerst de memory-map ophalen (buffer = onze eigen BSS): faalt de claim
	// hieronder, dan is de kaart al binnen en dumpt allocfail de vrije
	// regio's — één boot levert dan meteen het juiste venster op (gemeten
	// nodig op de Altra, 13-07: 0x90000000 was daar bezet).
	MOVD	$MEMMAP_CAP, R0
	MOVD	R0, 8(RSP)		// MapSize in: capaciteit
	MOVD	RSP, R0
	ADD	$8, R0			// &MapSize
	MOVD	$·memmapBuf(SB), R1
	MOVD	RSP, R2
	ADD	$16, R2			// &MapKey
	MOVD	RSP, R3
	ADD	$24, R3			// &DescSize
	MOVD	RSP, R4
	ADD	$32, R4			// &DescVer
	MOVD	0x38(R21), R8		// GetMemoryMap
	WORD	$0xd63f0100		// blr x8
	CBZ	R0, mapok
	MOVD	$0, R1			// map mislukt: dump straks niets
	MOVD	R1, 8(RSP)
mapok:

	// De kandidatenlus — het universele hart: de PE draagt meerdere
	// identieke varianten, elk gelinkt op een eigen venster (·uefiSlots,
	// gepatcht door mkkernel -pe). AllocatePages(AllocateAddress) is de
	// vraag "is dit venster vrij op dít bord?" — de eerste die slaagt
	// wint. Alles bezet → allocfail dumpt de vrije regio's als meting.
	MOVD	$0, R25			// k = variant-index
slottry:
	MOVD	$·uefiSlots(SB), R0
	MOVD	(R0), R1		// aantal kandidaten
	CMP	R1, R25
	BGE	allocfail
	ADD	$16, R0			// naar loads[0]
	LSL	$3, R25, R2
	ADD	R2, R0
	MOVD	(R0), R23		// kandidaat-linkadres
	MOVD	R23, (RSP)		// in/out: het gewenste adres
	MOVD	$2, R0			// AllocateAddress
	MOVD	$2, R1			// EfiLoaderData
	MOVD	runtime∕goos·RamSize(SB), R2
	MOVD	$CARVE_SIZE, R3
	ADD	R3, R2			// claim = Go-RAM + carve (layout-plan)
	LSR	$12, R2			// bytes → 4KB-pagina's
	MOVD	RSP, R3			// &mem
	MOVD	0x28(R21), R8		// AllocatePages
	WORD	$0xd63f0100		// blr x8
	CBZ	R0, claimed
	ADD	$1, R25
	B	slottry
claimed:

	// GetMemoryMap + ExitBootServices. De MapKey moet vers zijn: elke
	// allocatie ertussen maakt hem ongeldig, vandaar de lus (spec-recept).
	MOVD	$8, R26
ebstry:
	MOVD	$MEMMAP_CAP, R0
	MOVD	R0, 8(RSP)		// MapSize (in: capaciteit, uit: gebruikt)
	MOVD	RSP, R0
	ADD	$8, R0			// &MapSize
	MOVD	$·memmapBuf(SB), R1	// buffer (B-kant; verhuist mee)
	MOVD	RSP, R2
	ADD	$16, R2			// &MapKey
	MOVD	RSP, R3
	ADD	$24, R3			// &DescSize
	MOVD	RSP, R4
	ADD	$32, R4			// &DescVer
	MOVD	0x38(R21), R8		// GetMemoryMap
	WORD	$0xd63f0100		// blr x8

	MOVD	R19, R0			// ImageHandle
	MOVD	16(RSP), R1		// MapKey
	MOVD	0xe8(R21), R8		// ExitBootServices
	WORD	$0xd63f0100		// blr x8
	CBZ	R0, ebsok
	SUB	$1, R26
	CBNZ	R26, ebstry
	B	hang			// firmware weigert los te laten

ebsok:
	// Vanaf hier is het geheugen van ons; interrupts dicht vóór we de
	// firmware-vectoren onbruikbaar maken (MMU uit).
	WORD	$0xd5034fdf		// msr daifset, #0xf

	// De kopie: variant k (code+data+BSS-nullen) van zijn plek in de
	// geladen PE naar zijn linkadres. Bron = payload1B + k×stride, waarbij
	// payload1B = L1 + slide (L1 = de gepatchte RamStart van variant 0,
	// B-kant gelezen — PC-relatief). 16 bytes per slag; de linker houdt
	// runtime.end praktisch uitgelijnd en de partitie erachter is al van
	// ons — overschieten is onschadelijk.
	MOVD	runtime∕goos·RamStart(SB), R1
	ADD	R22, R1			// payload1B
	MOVD	$·uefiSlots(SB), R0
	MOVD	8(R0), R2		// stride tussen varianten in de PE
	MUL	R25, R2, R2		// k × stride
	ADD	R2, R1			// src = variant k, B-kant
	MOVD	R23, R0			// dst = gekozen L
	ADD	R24, R23, R2		// dstEnd
copy:
	LDP.P	16(R1), (R3, R4)
	STP.P	(R3, R4), 16(R0)
	CMP	R2, R0
	BLT	copy

	// De variantkopie is maagdelijk: ImageHandle/SystemTable en de
	// memory-map-vondst moeten alsnog naar de L-kant. delta (R5) vertaalt
	// een B-kant-symbooladres (PC-relatief, payload 1) naar de gekozen
	// L-kant: target = &sym + delta.
	MOVD	runtime∕goos·RamStart(SB), R1
	ADD	R22, R1			// payload1B
	SUB	R1, R23, R5		// delta = L − payload1B
	MOVD	$·imageHandle(SB), R0
	MOVD	R19, (R0)(R5)
	MOVD	$·sysTable(SB), R0
	MOVD	R20, (R0)(R5)
	MOVD	$·memmapSize(SB), R0
	MOVD	8(RSP), R1
	MOVD	R1, (R0)(R5)
	MOVD	$·memmapDesc(SB), R0
	MOVD	24(RSP), R1
	MOVD	R1, (R0)(R5)
	MOVD	$·memmapVer(SB), R0
	MOVD	32(RSP), R1
	MOVD	R1, (R0)(R5)
	MOVD	$·gopInfo(SB), R0
	MOVD	40(RSP), R1
	MOVD	R1, (R0)(R5)
	MOVD	$·gopInfo+8(SB), R0
	MOVD	48(RSP), R1
	MOVD	R1, (R0)(R5)
	MOVD	$·gopInfo+16(SB), R0
	MOVD	56(RSP), R1
	MOVD	R1, (R0)(R5)
	// De memory-map zelf: GetMemoryMap schreef de B-kant-buffer van
	// payload 1; de kopie bracht een lege mee. 8-byte-lus, lengte afgerond.
	MOVD	$·memmapBuf(SB), R0	// bron (B-kant)
	ADD	R5, R0, R1		// doel (L-kant)
	MOVD	8(RSP), R2
	ADD	$7, R2
	BIC	$7, R2
	ADD	R0, R2			// bronEinde
bufcopy:
	CMP	R2, R0
	BGE	bufdone
	MOVD.P	8(R0), R3
	MOVD.P	R3, 8(R1)
	B	bufcopy
bufdone:
	ADD	$64, RSP

	// Cache-onderhoud: de kopie is met MMU/cache aan geschreven; straks
	// leest de core met MMU uit (ongecached) en daarna cachet tamago weer.
	// Clean+invalidate per 64B (kleinste lijn op A76/N1-klasse), dan de
	// I-cache: de bestemming bevat code.
	MOVD	R23, R0
	ADD	R24, R23, R1
clean:
	WORD	$0xd50b7e20		// dc civac, x0
	ADD	$64, R0
	CMP	R1, R0
	BLT	clean
	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd508751f		// ic iallu
	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd5033fdf		// isb

	// MMU/caches uit op het huidige EL (firmware-EL: EL2 op servers, de
	// tamago-runtime bouwt zijn eigen vertaling op). Identity-mapped, dus
	// de PC blijft geldig over de overgang heen.
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	CMP	$2, R0
	BNE	mmuoff1
	WORD	$0xd53c1000		// mrs x0, sctlr_el2
	BIC	$1<<0, R0		// M
	BIC	$1<<2, R0		// C
	BIC	$1<<12, R0		// I
	WORD	$0xd51c1000		// msr sctlr_el2, x0
	B	mmuoffdone
mmuoff1:
	MRS	SCTLR_EL1, R0
	BIC	$1<<0, R0
	BIC	$1<<2, R0
	BIC	$1<<12, R0
	MSR	R0, SCTLR_EL1
mmuoffdone:
	WORD	$0xd5033fdf		// isb

	// Naar de L-kant van de gekozen variant: B-kant-adres + delta.
	MOVD	$·bootKernel(SB), R0
	ADD	R5, R0
	JMP	(R0)

allocfail:
	// RAM-venster bezet: melden en dan de vrije regio's (type 7 =
	// EfiConventionalMemory, ≥32MB) als "start einde"-hexparen printen —
	// de meting waarmee de volgende build zijn venster kiest (Altra 13-07:
	// 0x90000000 bleek bezet, dit maakt van die verrassing één boot werk).
	MOVD	0x40(R20), R0
	MOVD	$·strAllocFail(SB), R1
	MOVD	0x08(R0), R8
	WORD	$0xd63f0100		// blr x8

	MOVD	$·memmapBuf(SB), R24	// descriptor-cursor
	MOVD	8(RSP), R25
	ADD	R24, R25		// einde van de kaart
	MOVD	24(RSP), R26		// descriptor-stride (firmware-bepaald)
	CBZ	R26, hang		// geen kaart (GetMemoryMap faalde)
regloop:
	ADD	R26, R24, R0
	CMP	R25, R0			// cursor+stride voorbij het einde?
	BGT	hang
	MOVWU	(R24), R0		// Type
	CMP	$7, R0
	BNE	regnext
	MOVD	24(R24), R1		// NumberOfPages
	LSL	$12, R1			// → bytes
	CMP	$0x2000000, R1		// <32MB is ruis voor dit doel
	BLT	regnext
	MOVD	8(R24), R0		// PhysicalStart
	ADD	R0, R1			// R1 = einde
	MOVD	$·hexLine(SB), R2
	CALL	hexput(SB)		// R0 → 16 hexchars op (R2), R2 += 32
	MOVD	$0x20, R3
	MOVH	R3, (R2)		// spatie
	ADD	$2, R2
	MOVD	R1, R0
	CALL	hexput(SB)
	MOVD	$0x0d, R3
	MOVH	R3, (R2)		// \r
	MOVD	$0x0a, R3
	MOVH	R3, 2(R2)		// \n
	MOVD	$0, R3
	MOVH	R3, 4(R2)		// NUL
	MOVD	0x40(R20), R0
	MOVD	$·hexLine(SB), R1
	MOVD	0x08(R0), R8
	WORD	$0xd63f0100		// blr x8
regnext:
	ADD	R26, R24
	B	regloop
hang:
	WFE
	B	hang

// hexput schrijft R0 als 16 UCS-2-hexcijfers op (R2) en schuift R2 32 bytes
// op. Bladfunctie (geen stack, geen calls; alleen R9/R10 als scratch, R0
// blijft staan) — aanroepbaar met BL vanuit de dump-lus.
TEXT hexput(SB),NOSPLIT|NOFRAME,$0
	MOVD	$60, R9
hexdig:
	LSR	R9, R0, R10
	AND	$0xf, R10
	CMP	$10, R10
	BLT	hexnum
	ADD	$55, R10		// 'A' - 10
	B	hexsto
hexnum:
	ADD	$48, R10		// '0'
hexsto:
	MOVH	R10, (R2)
	ADD	$2, R2
	SUBS	$4, R9
	BGE	hexdig
	RET

// bootKernel draait op het LINKADRES met MMU uit — vanaf hier is dit het
// qemuvirt-cpuinit-recept: EL vaststellen, EL2 configureren, drop naar EL1,
// tamago-runtime. (Stage-2/VBAR_EL2 volgt in de slots-fase, zoals op de Pi.)
TEXT ·bootKernel(SB),NOSPLIT|NOFRAME,$0
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	MOVD	R0, ·bootELVal(SB)	// MMU uit; ongecached, coherent na civac

	CMP	$2, R0
	BEQ	el2
	// EL1-boot: doorstarten zodat de probe het kan MELDEN (BootEL()<2);
	// de echte kern weigert op deze waarde.
	B	·uefiEL1(SB)

el2:
	// VBAR_EL2 van de HOP-core → de revoke-vectoren (RamStart+REVOKE_OFF =
	// layout.RevokeVecPA). stage2.InitVectors vult ze na boot; de
	// hard-kill-HVC uit stage2.Revoke landt daar. De WERKELIJKE waarden
	// gaan naar Go-globals zodat board.go-init de asm/Go-pariteit écht kan
	// checken (review #9: de oude check was een tautologie). MMU is uit en
	// de lijnen zijn ge-civac'd — deze stores zijn coherent, als bootELVal.
	MOVD	runtime∕goos·RamStart(SB), R0
	MOVD	$REVOKE_OFF, R1
	ADD	R1, R0
	WORD	$0xd51cc000		// msr vbar_el2, x0
	MOVD	R0, ·vbarEL2Val(SB)
	MOVD	$CARVE_SIZE, R1
	MOVD	R1, ·carveSizeAsm(SB)
	MOVD	$MEMMAP_CAP, R1
	MOVD	R1, ·memmapCapAsm(SB)

	// HCR_EL2: RW(31)=1 — EL1 draait AArch64 (wist ook E2H, mocht de
	// firmware VHE aan hebben gehad).
	MOVD	$1<<31, R0
	WORD	$0xd51c1100		// msr hcr_el2, x0

	// CNTHCTL_EL2: EL1PCTEN|EL1PCEN — timer/counter vrij voor EL1.
	WORD	$0xd53ce100		// mrs x0, cnthctl_el2
	ORR	$0b11, R0, R0
	WORD	$0xd51ce100		// msr cnthctl_el2, x0
	MOVD	$0, R0
	WORD	$0xd51ce060		// msr cntvoff_el2, x0

	// SPSR_EL2: EL1h, DAIF gemaskeerd.
	MOVD	$0, R0
	ORR	$0b1111<<6, R0
	ORR	$0b0101<<0, R0
	WORD	$0xd51c4000		// msr spsr_el2, x0

	MOVD	$·uefiEL1(SB), R0
	WORD	$0xd51c4020		// msr elr_el2, x0
	ISB	$15
	ERET

TEXT ·uefiEL1(SB),NOSPLIT|NOFRAME,$0
	// SCTLR_EL1: alignment-check en MMU uit (tamago zet ze zelf weer aan).
	MRS	SCTLR_EL1, R0
	BIC	$1<<1, R0
	BIC	$1<<0, R0
	MSR	R0, SCTLR_EL1
	ISB	$15

	// Stack aan het einde van de eigen RAM-declaratie.
	MOVD	runtime∕goos·RamStart(SB), R1
	MOVD	R1, RSP
	MOVD	runtime∕goos·RamSize(SB), R1
	MOVD	runtime∕goos·RamStackOffset(SB), R2
	ADD	R1, RSP
	SUB	R2, RSP

	B	_rt0_tamago_start(SB)

// EFI_GRAPHICS_OUTPUT_PROTOCOL_GUID (9042a9de-23dc-4a38-96fb-7aded080516a)
// zoals hij in geheugen ligt, als twee LE-woorden.
GLOBL	·gopGUID(SB),RODATA,$16
DATA	·gopGUID+0(SB)/8,$0x4a3823dc9042a9de
DATA	·gopGUID+8(SB)/8,$0x6a5180d0de7afb96


// Absolute linkadressen als data: het anker waarmee de stub zijn slide meet
// (·bootKernelVA) en het einde van de image inclusief BSS (runtime.end).
GLOBL	·bootKernelVA(SB),RODATA,$8
DATA	·bootKernelVA+0(SB)/8,$·bootKernel(SB)
GLOBL	·imageEndVA(SB),RODATA,$8
DATA	·imageEndVA+0(SB)/8,$runtime·end(SB)

// UCS-2-strings voor de firmware-console (UEFI spreekt CHAR16).
// "HopOS: UEFI stub\r\n"
GLOBL	·strBanner(SB),RODATA,$40
DATA	·strBanner+0(SB)/8,$0x004f0070006f0048	// H o p O
DATA	·strBanner+8(SB)/8,$0x00550020003a0053	// S :   U
DATA	·strBanner+16(SB)/8,$0x0020004900460045	// E F I (spatie)
DATA	·strBanner+24(SB)/8,$0x0062007500740073	// s t u b
DATA	·strBanner+32(SB)/8,$0x0000000a000d		// \r \n NUL
// "RAM WINDOW BUSY\r\n" — AllocatePages(RamStart) faalde.
GLOBL	·strAllocFail(SB),RODATA,$40
DATA	·strAllocFail+0(SB)/8,$0x0020004d00410052	// R A M (spatie)
DATA	·strAllocFail+8(SB)/8,$0x0044004e00490057	// W I N D
DATA	·strAllocFail+16(SB)/8,$0x004200200057004f	// O W (spatie) B
DATA	·strAllocFail+24(SB)/8,$0x000d005900530055	// U S Y \r
DATA	·strAllocFail+32(SB)/8,$0x000000000000000a	// \n NUL
