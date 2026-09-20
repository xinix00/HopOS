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


// Moet gelijk zijn aan memmapCap in uefi.go (asm kent geen Go-constanten).
#define MEMMAP_CAP 0x40000
#define CFG_CAP 0x4000	// 16KB: de gui-template is 8KB; bij 4KB (t/m 17-09) verdween alles na byte 4096 stil

// Pariteit met board.go (Go-init checkt): de carve — het stuk tussen de
// Go-RAM (RamSize) en het einde van de claim — draagt het layout-plan
// (ctrl/ringen/stage-2/net-DMA), en REVOKE_OFF is waar cpuinit VBAR_EL2
// van de HOP-core heen zet (RamStart + offset; layout.TrapVecPA).
#define CARVE_SIZE 0x02000000
#define REVOKE_OFF 0x08900800

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
	B	·cpuinitEL1(SB)
el1hang:
	WFE
	B	el1hang
fwentry:
	// Kern-flip (kern/kernflip): de oude kern legt ons al gereloceerd op het
	// linkadres en springt hierheen met x1 = 0 (chain_arm64.s) — er is geen
	// firmware meer om aan te roepen, geen kopie te maken en geen venster te
	// kiezen. Rechtstreeks de generieke boot in; hwinit1 haalt de firmware-
	// feiten (SystemTable, memory map, GOP, hopos.cfg) uit de carve waar de
	// koude boot ze neerlegde (uefi.go fwFacts). Een echte UEFI-entry heeft
	// nooit x1 = 0: dat is *SystemTable.
	CBNZ	R1, fwreal
	MOVD	$·bootKernel(SB), R2
	JMP	(R2)
fwreal:
	MOVD	R0, R19			// ImageHandle
	MOVD	R1, R20			// *SystemTable

	// Werkruimte voor out-parameters op de (UEFI-)stack:
	//   0(RSP)=AllocatePages-adres  8(RSP)=MapSize  16(RSP)=MapKey
	//   24(RSP)=DescSize            32(RSP)=DescVer
	//   40..56(RSP)=GOP             64(RSP)=VarSize  72(RSP)=HopOSCores
	SUB	$80, RSP

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

	// Node-config uit hopos.cfg op de ESP-root, NU — vóór ExitBootServices
	// leest de FIRMWARE zijn eigen FAT (SimpleFileSystem, dezelfde weg
	// waarlangs deze PE geladen is); wij nemen alleen de bytes over. Géén
	// FS-driver in HopOS — zelfde model als cmdline.txt op de Pi: de firmware
	// leest, HopOS parseert (uefi.BootConfig, zelfde sleutels als de Pi).
	// Beheer = een tekstbestandje op de stick bewerken. Elke fout → 0 bytes →
	// Go-defaults. root/file in callee-saved regs (R22/R24 — pas later in
	// gebruik): de firmware-calls clobberen x9-x15. 72(RSP) = bytes gelezen.
	MOVD	$0, R0
	MOVD	R0, 72(RSP)
	MOVD	R19, R0			// eigen ImageHandle
	MOVD	$·liGUID(SB), R1	// EFI_LOADED_IMAGE_PROTOCOL
	MOVD	RSP, R2			// &iface (slot 0, scratch)
	MOVD	0x98(R21), R8		// HandleProtocol
	WORD	$0xd63f0100		// blr x8
	CBNZ	R0, nocfg
	MOVD	(RSP), R0		// *LoadedImage
	CBZ	R0, nocfg
	MOVD	0x18(R0), R0		// ->DeviceHandle (de ESP)
	CBZ	R0, nocfg
	MOVD	$·sfsGUID(SB), R1	// EFI_SIMPLE_FILE_SYSTEM_PROTOCOL
	MOVD	RSP, R2
	MOVD	0x98(R21), R8		// HandleProtocol
	WORD	$0xd63f0100		// blr x8
	CBNZ	R0, nocfg
	MOVD	(RSP), R0		// *SimpleFileSystem
	CBZ	R0, nocfg
	MOVD	RSP, R1			// &root
	MOVD	0x08(R0), R8		// ->OpenVolume
	WORD	$0xd63f0100		// blr x8
	CBNZ	R0, nocfg
	MOVD	(RSP), R22		// *root (EFI_FILE, callee-saved)
	CBZ	R22, nocfg
	MOVD	R22, R0
	MOVD	RSP, R1			// &file
	MOVD	$·cfgName(SB), R2	// L"hopos.cfg"
	MOVD	$1, R3			// EFI_FILE_MODE_READ
	MOVD	$0, R4
	MOVD	0x08(R22), R8		// root->Open
	WORD	$0xd63f0100		// blr x8
	CBNZ	R0, cfgroot		// geen bestand: alleen root sluiten
	MOVD	(RSP), R24		// *file (callee-saved)
	CBZ	R24, cfgroot
	MOVD	$CFG_CAP, R0
	MOVD	R0, 72(RSP)		// size in: capaciteit
	MOVD	R24, R0
	MOVD	RSP, R1
	ADD	$72, R1			// &size
	MOVD	$·cfgBuf(SB), R2	// B-kant buffer; verhuist straks mee
	MOVD	0x20(R24), R8		// file->Read
	WORD	$0xd63f0100		// blr x8
	CBZ	R0, cfgclose
	MOVD	$0, R1			// leesfout: niets gelezen
	MOVD	R1, 72(RSP)
cfgclose:
	MOVD	R24, R0
	MOVD	0x10(R24), R8		// file->Close (fouten negeren)
	WORD	$0xd63f0100		// blr x8
cfgroot:
	MOVD	R22, R0
	MOVD	0x10(R22), R8		// root->Close
	WORD	$0xd63f0100		// blr x8
	B	cfgdone
nocfg:
	MOVD	$0, R0
	MOVD	R0, 72(RSP)
cfgdone:

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

	// Diagnose op de firmware-console, nu het nog kan: "W<venster> G<GOP-
	// basis> P<pixelformaat<<32|scanlijn> C<cfg-bytes>" — de meting waarmee
	// een stille boot (O6N 09-09: banner en daarna niets) in fases valt.
	// OutputString mag hier nog: de MapKey wordt in de lus hieronder vers
	// opgehaald.
	MOVD	$·hexLine(SB), R2
	MOVD	$0x57, R3		// W
	MOVH	R3, (R2)
	ADD	$2, R2
	MOVD	R23, R0
	CALL	hexput(SB)
	MOVD	$0x20, R3
	MOVH	R3, (R2)
	MOVD	$0x47, R3		// G
	MOVH	R3, 2(R2)
	ADD	$4, R2
	MOVD	40(RSP), R0
	CALL	hexput(SB)
	MOVD	$0x20, R3
	MOVH	R3, (R2)
	MOVD	$0x50, R3		// P
	MOVH	R3, 2(R2)
	ADD	$4, R2
	MOVD	56(RSP), R0
	CALL	hexput(SB)
	MOVD	$0x20, R3
	MOVH	R3, (R2)
	MOVD	$0x43, R3		// C
	MOVH	R3, 2(R2)
	ADD	$4, R2
	MOVD	72(RSP), R0
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
	// "exit boot services" — het laatste dat de firmware-console ziet.
	MOVD	0x40(R20), R0
	MOVD	$·strExit(SB), R1
	MOVD	0x08(R0), R8
	WORD	$0xd63f0100		// blr x8

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

	// Relocatie (mkkernel -reloc): één payload voor álle vensters. De
	// u32-tabel (·uefiReloc: [0]=offset t.o.v. de payload-start, [1]=aantal)
	// wijst elk 8-byte-woord met een absoluut adres aan; de kopie draait op
	// L ≠ linkbasis T0, dus: woord += L − T0. Count 0 = klassieke
	// multi-variant-PE: overslaan. Vóór het cache-onderhoud hieronder,
	// zodat de gepatchte woorden mee de lijn af gaan.
	MOVD	$·uefiReloc(SB), R0
	MOVD	8(R0), R6		// aantal entries
	CBZ	R6, relocdone
	MOVD	(R0), R3		// tabel-offset t.o.v. payload-start
	MOVD	runtime∕goos·RamStart(SB), R2	// T0 (B-kant, PC-relatief)
	ADD	R22, R2, R1		// payload1B (B-kant payload-basis)
	ADD	R1, R3			// &tabel (B-kant)
	SUB	R2, R23, R5		// delta = L − T0
relocnext:
	MOVWU.P	4(R3), R1		// volgende u32-offset
	ADD	R23, R1			// &woord in de kopie (L-kant)
	MOVD	(R1), R2
	ADD	R5, R2
	MOVD	R2, (R1)
	SUB	$1, R6
	CBNZ	R6, relocnext
relocdone:

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
	MOVD	$·cfgLen(SB), R0	// hopos.cfg: gelezen lengte (0 = geen bestand)
	MOVD	72(RSP), R1
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
	// hopos.cfg-bytes: zelfde verhaal als de memory-map — de stub las de
	// B-kant-buffer, de kopie bracht een lege mee.
	MOVD	$·cfgBuf(SB), R0	// bron (B-kant)
	ADD	R5, R0, R1		// doel (L-kant)
	MOVD	72(RSP), R2
	ADD	$7, R2
	BIC	$7, R2
	ADD	R0, R2			// bronEinde
cfgcopy:
	CMP	R2, R0
	BGE	cfgcdone
	MOVD.P	8(R0), R3
	MOVD.P	R3, 8(R1)
	B	cfgcopy
cfgcdone:
	ADD	$80, RSP

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

// bootKernel draait op het LINKADRES met MMU uit — vanaf hier is dit de
// generieke boot (cpu/el2/boot.h), met bootKernel als BOOT_ENTRY (cpuinit is
// hier de UEFI-ingang: de loader hierboven). Geen BOOT_SCRATCH: het boot-EL
// gaat naar een Go-global (bootELVal). Geen TRAP_VEC als constante: de
// trap-vectoren liggen op RamStart+REVOKE_OFF, dus rekent el2UEFI ze uit. De
// WERKELIJKE waarden gaan naar Go-globals zodat plan.go de asm/Go-pariteit
// écht kan checken (review #9: de oude check was een tautologie). MMU is uit
// en de lijnen zijn ge-civac'd — deze stores zijn coherent.
#include "../../cpu/el2/sysreg.h"
#include "../../cpu/el2/drop.h"

#define BOOT_ENTRY  ·bootKernel
#define BOARD_EL2   BL ·el2UEFI(SB)
#define BOARD_EL1   BL ·el1UEFI(SB)
#include "../../cpu/el2/boot.h"

// el2UEFI: VBAR_EL2 van de HOP-core → de revoke-vectoren (RamStart+REVOKE_OFF
// = layout.TrapVecPA). stage2.InitVectors vult ze na boot; de hard-kill-HVC
// uit stage2.Revoke landt daar. Plus de pariteitswaarden voor plan.go.
TEXT ·el2UEFI(SB),NOSPLIT|NOFRAME,$0
	MOVD	runtime∕goos·RamStart(SB), R0
	MOVD	$REVOKE_OFF, R1
	ADD	R1, R0
	// Boot-trapvectoren in de revoke-pagina, tot stage2.InitVectors de
	// echte erin zet: elke EL2-exception vóór die tijd (een trap uit EL1,
	// een EL2-fout) was een stille hang op een lege pagina; nu print hij
	// "E2 esr elr far" op de vroege UART (trapPrint) en parkeert. De blob
	// (·trapVecEL2, 4KB incl. handler) is positie-onafhankelijk: hij vindt
	// zijn UART en de printer via woorden die wij hier in entry 0 zetten
	// (+0x70 UART, +0x68 printer) en via MRS VBAR_EL2.
	MOVD	$·trapVecEL2(SB), R2
	ADD	$0x7ff, R2
	AND	$~0x7ff, R2		// de 2KB-uitgelijnde tabel in de blob
	MOVD	R0, R3
	MOVD	$0x1000, R4
vecopy:
	LDP.P	16(R2), (R5, R6)
	STP.P	(R5, R6), 16(R3)
	SUBS	$16, R4
	BNE	vecopy
	MOVD	·earlyUART(SB), R2
	MOVD	R2, 0x70(R0)
	MOVD	$·trapPrint(SB), R2
	MOVD	R2, 0x68(R0)
	MOVD	R0, R2
	ADD	$0x1000, R0, R3
veclean:
	WORD	$0xd50b7e22		// dc civac, x2
	ADD	$64, R2
	CMP	R3, R2
	BLT	veclean
	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd508751f		// ic iallu
	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd5033fdf		// isb
	WORD	$0xd51cc000		// msr vbar_el2, x0
	MOVD	R0, ·vbarEL2Val(SB)

	// EL2 saneren zoals Linux' init_el2_state (arch/arm64/include/asm/
	// el2_setup.h) het doet vóór de drop naar EL1: elk trap-controle-
	// register op een bekende "geen traps"-waarde, per feature bewaakt met
	// het ID-register. TF-A zet de klassieke set (HCR/CPTR/CNTHCTL/MDCR/
	// HSTR/VPIDR); de Armv9-registers (FGT, HCRX, MPAM) resetten naar een
	// UNKNOWN waarde en zijn op de O6N (A720: FGT, MPAM) nooit bewezen
	// schoon. Een verdwaalde EL1→EL2-trap vóór stage2 zijn vectoren zette,
	// was daar een stille hang (09-09).
	MOVD	$0, R0
	WORD	$0xd51c1120		// msr mdcr_el2, x0   (geen debug/PMU-traps)
	WORD	$0xd51c1160		// msr hstr_el2, x0   (geen CP15-traps)
	// FEAT_FGT: ID_AA64MMFR0_EL1[59:56]
	WORD	$0xd5380701		// mrs x1, id_aa64mmfr0_el1
	LSR	$56, R1
	AND	$0xf, R1
	CBZ	R1, nofgt
	WORD	$0xd51c1180		// msr hfgrtr_el2, x0
	WORD	$0xd51c11a0		// msr hfgwtr_el2, x0
	WORD	$0xd51c11c0		// msr hfgitr_el2, x0
	WORD	$0xd51c3180		// msr hdfgrtr_el2, x0
	WORD	$0xd51c31a0		// msr hdfgwtr_el2, x0
nofgt:
	// FEAT_HCX: ID_AA64MMFR1_EL1[43:40]
	WORD	$0xd5380721		// mrs x1, id_aa64mmfr1_el1
	LSR	$40, R1
	AND	$0xf, R1
	CBZ	R1, nohcx
	WORD	$0xd51c1240		// msr hcrx_el2, x0
nohcx:
	// FEAT_MPAM: ID_AA64PFR0_EL1[43:40]
	WORD	$0xd5380401		// mrs x1, id_aa64pfr0_el1
	LSR	$40, R1
	AND	$0xf, R1
	CBZ	R1, nompam
	WORD	$0xd51ca500		// msr mpam2_el2, x0   (geen MPAM-traps uit EL1)
	WORD	$0xd51ca400		// msr mpamhcr_el2, x0
nompam:
	// GICv3-systeemregisters uit EL1 toestaan: ICC_SRE_EL2 = SRE|DFB|DIB|Enable,
	// ICH_HCR_EL2 = 0 (ID_AA64PFR0_EL1[27:24] = GIC).
	WORD	$0xd5380401		// mrs x1, id_aa64pfr0_el1
	LSR	$24, R1
	AND	$0xf, R1
	CBZ	R1, nogic
	MOVD	$0xf, R1
	WORD	$0xd51cc9a1		// msr icc_sre_el2, x1
	ISB	$15
	WORD	$0xd51ccb00		// msr ich_hcr_el2, x0
nogic:
	// VPIDR/VMPIDR = MIDR/MPIDR: wat EL1 als eigen identiteit ziet.
	WORD	$0xd5380001		// mrs x1, midr_el1
	WORD	$0xd51c0001		// msr vpidr_el2, x1
	WORD	$0xd53800a1		// mrs x1, mpidr_el1
	WORD	$0xd51c00a1		// msr vmpidr_el2, x1
	ISB	$15
	MOVD	$CARVE_SIZE, R1
	MOVD	R1, ·carveSizeAsm(SB)
	MOVD	$MEMMAP_CAP, R1
	MOVD	R1, ·memmapCapAsm(SB)
	MOVD	$0x32, R0		// "K2": kern op het linkadres, EL2, MMU uit
	B	earlyMark(SB)

// el1UEFI: het boot-EL (x10, boot.h) naar de Go-global — op élke route, ook
// een EL1-boot: de probe MELDT dan BootEL()<2, de echte kern weigert erop.
TEXT ·el1UEFI(SB),NOSPLIT|NOFRAME,$0
	MOVD	R10, ·bootELVal(SB)
	// CPACR_EL1.FPEN=0b11: FP/SIMD vrij vóór de eerste Go-instructie. De
	// runtime raakt V-registers en floats al in rt0 (runtime·check,
	// memmove), tamago's fp_enable komt pas in Hwinit0 — en FPEN reset naar
	// een UNKNOWN waarde: op N1/A76/A55 toevallig open, op de A720 niet
	// bewezen. Een FP-trap vóór de vectoren staan is een stille hang.
	MOVD	$0x300000, R0
	WORD	$0xd5181040		// msr cpacr_el1, x0
	// VBAR_EL1 naar de boot-trapvectoren tot tamago (hwinit1) de zijne
	// zet: een EL1-exception in rt0/Hwinit0 print dan "E1 esr elr far".
	MOVD	$·trapVecEL1(SB), R0
	ADD	$0x7ff, R0
	AND	$~0x7ff, R0		// 2KB-uitgelijnd (PCALIGN in de blob)
	WORD	$0xd518c000		// msr vbar_el1, x0
	ISB	$15
	MOVD	R30, R7			// LR bewaren over de mark
	MOVD	$0x31, R0		// "K1": na de drop, vóór rt0
	BL	earlyMark(SB)
	MOVD	R7, R30
	RET

// trapVecEL1: 16 vectoren (2KB-uitgelijnd, 0x80 per entry) die elk naar
// trapEL1 springen: ESR/ELR/FAR van EL1 ophalen en printen. In-image, dus
// gewone symboolreferenties.
TEXT ·trapVecEL1(SB),NOSPLIT|NOFRAME,$0
	PCALIGN	$2048
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
	B	trapEL1
	PCALIGN	$128
trapEL1:
	MOVD	$0, R26			// EL1-trap: nooit terugkeren
	MOVD	·earlyUART(SB), R1
	MRS	ESR_EL1, R2
	MRS	ELR_EL1, R3
	MRS	FAR_EL1, R4
	MOVD	$0x31, R0
	B	·trapPrint(SB)

// trapVecEL2: zelfde tabel, maar gekopieerd naar de revoke-pagina en dus
// positie-onafhankelijk: de UART en de printer komen uit entry 0 (+0x70,
// +0x68), gevonden via VBAR_EL2. De handler staat ín de blob (na de 16
// entries, binnen de 4KB die el2UEFI kopieert).
TEXT ·trapVecEL2(SB),NOSPLIT|NOFRAME,$0
	PCALIGN	$2048
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
	B	trapEL2
	PCALIGN	$128
trapEL2:
	WORD	$0xd53cc005		// mrs x5, vbar_el2
	MOVD	0x70(R5), R1		// UART
	MOVD	0x68(R5), R5		// printer (absoluut)
	WORD	$0xd53c5202		// mrs x2, esr_el2
	WORD	$0xd53c4023		// mrs x3, elr_el2
	WORD	$0xd53c6004		// mrs x4, far_el2
	MOVD	$0x32, R0		// '2'
	MOVD	$0, R26			// default: parkeren na de print
	LSR	$26, R2, R6
	CMP	$0x16, R6		// EC 0x16 = HVC uit AArch64: de zelftest → terug
	BNE	trapgo
	MOVD	$1, R26
trapgo:
	JMP	(R5)

// trapPrint: "E<R0> esr=<R2> elr=<R3> far=<R4>\r\n" op PL011 R1, dan
// parkeren. Bladcode zonder stack; earlyPutc klobbert R3/R4 dus de waarden
// gaan eerst naar R20-R22 (hier is niets meer te bewaren).
TEXT ·trapPrint(SB),NOSPLIT|NOFRAME,$0
	CBZ	R1, tphang
	MOVD	R0, R19
	MOVD	R2, R20
	MOVD	R3, R21
	MOVD	R4, R22
	MOVD	$0x0d, R2
	CALL	earlyPutc(SB)
	MOVD	$0x0a, R2
	CALL	earlyPutc(SB)
	MOVD	$0x45, R2		// E
	CALL	earlyPutc(SB)
	MOVD	R19, R2
	CALL	earlyPutc(SB)
	MOVD	$0x20, R2
	CALL	earlyPutc(SB)
	MOVD	R20, R0
	CALL	hexuart(SB)
	MOVD	$0x20, R2
	CALL	earlyPutc(SB)
	MOVD	R21, R0
	CALL	hexuart(SB)
	MOVD	$0x20, R2
	CALL	earlyPutc(SB)
	MOVD	R22, R0
	CALL	hexuart(SB)
	MOVD	$0x0d, R2
	CALL	earlyPutc(SB)
	MOVD	$0x0a, R2
	CALL	earlyPutc(SB)
	CBZ	R26, tphang
	ERET				// zelftest (HVC): terug naar EL1, ELR wijst voorbij de HVC
tphang:
	WFE
	B	tphang

// hexuart: R0 als 16 hexcijfers naar PL011 R1. R23 = teller, R24 = waarde;
// earlyPutc klobbert R3/R4; LR in R25.
TEXT hexuart(SB),NOSPLIT|NOFRAME,$0
	MOVD	R30, R25
	MOVD	R0, R24
	MOVD	$60, R23
hxdig:
	LSR	R23, R24, R2
	AND	$0xf, R2
	CMP	$10, R2
	BLT	hxnum
	ADD	$55, R2
	B	hxput
hxnum:
	ADD	$48, R2
hxput:
	CALL	earlyPutc(SB)
	SUBS	$4, R23
	BGE	hxdig
	MOVD	R25, R30
	RET

// earlyMark schrijft "K<R0>\r\n" naar de vroege PL011 (·earlyUART: een
// link-time constante van het board, 0 = geen), met MMU uit en zonder stack:
// de enige stem tussen ExitBootServices en hwinit1. Begrensde poll op
// FR.TXFF; een dode UART kost hooguit de lus. Bladfunctie: R0-R5 scratch,
// keert terug naar de aanroeper van el2UEFI/el1UEFI (B, geen BL).
TEXT earlyMark(SB),NOSPLIT|NOFRAME,$0
	MOVD	·earlyUART(SB), R1
	CBZ	R1, markdone
	MOVD	R30, R6			// LR van boot.h bewaren: de CALLs hieronder klobberen hem
	MOVD	$0x4b, R2		// K
	CALL	earlyPutc(SB)
	MOVD	R0, R2
	CALL	earlyPutc(SB)
	MOVD	$0x0d, R2
	CALL	earlyPutc(SB)
	MOVD	$0x0a, R2
	CALL	earlyPutc(SB)
	MOVD	R6, R30
markdone:
	RET

// EarlyMark(c byte): één teken naar de vroege UART vanuit Go — de meetlat
// door hwinit1 heen die niet van printk/conlog/mirror afhangt. NOFRAME +
// LR in R6, zelfde recept als earlyMark.
TEXT ·EarlyMark(SB),NOSPLIT|NOFRAME,$0-1
	MOVD	·earlyUART(SB), R1
	CBZ	R1, emdone
	MOVBU	c+0(FP), R2
	MOVD	R30, R6
	CALL	earlyPutc(SB)
	MOVD	R6, R30
emdone:
	RET

// earlyPutc: R2 → PL011 op R1 (DR +0, FR +0x18 bit 5 = TXFF). R3/R4 scratch.
TEXT earlyPutc(SB),NOSPLIT|NOFRAME,$0
	MOVD	$0x100000, R4
putcwait:
	MOVWU	0x18(R1), R3
	TBZ	$5, R3, putcgo
	SUBS	$1, R4
	BNE	putcwait
	RET				// dood: niets schrijven
putcgo:
	MOVW	R2, (R1)
	RET

// EFI_GRAPHICS_OUTPUT_PROTOCOL_GUID (9042a9de-23dc-4a38-96fb-7aded080516a)
// zoals hij in geheugen ligt, als twee LE-woorden.
GLOBL	·gopGUID(SB),RODATA,$16
DATA	·gopGUID+0(SB)/8,$0x4a3823dc9042a9de
DATA	·gopGUID+8(SB)/8,$0x6a5180d0de7afb96

// EFI_LOADED_IMAGE_PROTOCOL_GUID (5b1b31a1-9562-11d2-8e3f-00a0c969723b).
GLOBL	·liGUID(SB),RODATA,$16
DATA	·liGUID+0(SB)/8,$0x11d295625b1b31a1
DATA	·liGUID+8(SB)/8,$0x3b7269c9a0003f8e

// EFI_SIMPLE_FILE_SYSTEM_PROTOCOL_GUID (964e5b22-6459-11d2-8e39-00a0c969723b).
GLOBL	·sfsGUID(SB),RODATA,$16
DATA	·sfsGUID+0(SB)/8,$0x11d26459964e5b22
DATA	·sfsGUID+8(SB)/8,$0x3b7269c9a000398e

// L"hopos.cfg" (UCS-2, NUL-getermineerd, 8-byte-gepad).
GLOBL	·cfgName(SB),RODATA,$24
DATA	·cfgName+0(SB)/8,$0x006f0070006f0068
DATA	·cfgName+8(SB)/8,$0x00660063002e0073
DATA	·cfgName+16(SB)/8,$0x0000000000000067


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
// "exit boot services\r\n"
GLOBL	·strExit(SB),RODATA,$48
DATA	·strExit+0(SB)/8,$0x0074006900780065	// e x i t
DATA	·strExit+8(SB)/8,$0x006f006f00620020	//   b o o
DATA	·strExit+16(SB)/8,$0x0073002000740074	// t   s
DATA	·strExit+24(SB)/8,$0x0076007200650073	// e r v
DATA	·strExit+32(SB)/8,$0x0073006500630069	// i c e s
DATA	·strExit+40(SB)/8,$0x00000000000a000d	// \r \n NUL
// "RAM WINDOW BUSY\r\n" — AllocatePages(RamStart) faalde.
GLOBL	·strAllocFail(SB),RODATA,$40
DATA	·strAllocFail+0(SB)/8,$0x0020004d00410052	// R A M (spatie)
DATA	·strAllocFail+8(SB)/8,$0x0044004e00490057	// W I N D
DATA	·strAllocFail+16(SB)/8,$0x004200200057004f	// O W (spatie) B
DATA	·strAllocFail+24(SB)/8,$0x000d005900530055	// U S Y \r
DATA	·strAllocFail+32(SB)/8,$0x000000000000000a	// \n NUL
