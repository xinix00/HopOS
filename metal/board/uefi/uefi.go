// Package uefi biedt HopOS-support voor élk AArch64-platform met
// UEFI-firmware en ACPI — de Ampere Altra (128 cores) voorop, met QEMU
// -M virt + EDK2 als proeftuin die exact dezelfde weg bewandelt (BOOTAA64.EFI
// op een FAT-medium). Waar de Pi's board-specifieke bring-up nodig hebben,
// is dit het universele pad: de firmware beschrijft het ijzer (ACPI), wij
// lezen het uit (metal/acpi) — ontwerpprincipe "universeel boven
// board-specifiek".
//
// De boot-keten (init.s):
//
//  1. de firmware laadt ons als PE/COFF (mkkernel -pe) op een willekeurig
//     adres en roept cpuinit aan als UEFI-app (x0=ImageHandle,
//     x1=SystemTable, MMU aan, identity-mapped, EL2 op servers);
//  2. de stub claimt onze RAM-partitie (AllocatePages op RamStart — bewijst
//     dat het venster vrij is), haalt de memory-map op, ExitBootServices;
//  3. kopieert de hele image naar het linkadres (de "slide" weg), cache-clean,
//     MMU uit, en springt naar bootKernel op het linkadres;
//  4. bootKernel = het qemuvirt-recept: EL2-registers, drop naar EL1,
//     _rt0_tamago_start.
//
// SystemTable/memory-map/boot-EL overleven als Go-globals: de stub schrijft
// ze vóór de kopie (ze verhuizen mee), Go leest ze na de boot. Console komt
// uit de ACPI SPCR-tabel (PL011/SBSA-UART) — vóór die parse is printk een
// no-op, dus géén output tussen rt0 en hwinit1 (bewuste keuze: er is geen
// universeel UART-adres vóór ACPI).
//
// Alleen voor GOOS=tamago GOARCH=arm64, bouwen met -tags "uefi linkcpuinit".
package uefi

import (
	"crypto/sha256"
	"encoding/binary"
	"unsafe" // geheugenreads op firmware-adressen + go:linkname

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/acpi"
	"hop-os/metal/fb"
	"hop-os/metal/idle"
	"hop-os/metal/pl011"
	"hop-os/metal/trng"
)

// KernelSize: de grootte van de RAM-partitie van de HOP-kern. Het
// STARTADRES is niet één constante meer maar wordt bij boot ONTDEKT: de
// image (mkkernel -pe) draagt meerdere identiek gecompileerde varianten,
// elk gelinkt op een eigen kandidaat-venster (image/uefi-run.sh), en de
// stub kiest met AllocatePages de eerste kandidaat die op dit platform
// vrij is — universeel, geen herbouw per bord (Altra-meting 13-07:
// 0x90000000 bezet; dit maakt dat soort verrassingen onzichtbaar). Zijn
// álle kandidaten bezet, dan print de stub "RAM WINDOW BUSY" plus de vrije
// regio's (voeg een kandidaat toe en herbouw).
const KernelSize = GoRAMSize + carveSize // Go-RAM + plan-carve (zie board.go)

// De RAM-declaratie (runtime/goos.RamStart/RamSize) is eigendom van de
// MAIN (probeuefi, cmd/hopos/board_uefi, appspike/mem) — net als bij de
// andere boards, en nodig omdat app-images hun éígen canonieke declaratie
// hebben. HOP-mains zetten RamStart op 0 en GoRAMSize als grootte;
// mkkernel -pe patcht RamStart per venster-variant. Dit board leest het
// symbool via een asm-accessor (cpu_arm64.s) — referentie, geen definitie.

// GoRAMSize is de Go-RAM van een HOP-kern-main op dit board: de stub
// claimt GoRAMSize + de carve (het PA-plan-gebied, zie board.go/init.s).
const GoRAMSize = carveOff

// Base geeft het startadres van de eigen RAM-partitie (HOP-kern: het
// stub-gekozen venster; app-image: de canonieke slot-basis).
func Base() uintptr { return uintptr(ramStartAsm()) }

// ramStartAsm leest runtime/goos.RamStart (cpu_arm64.s).
func ramStartAsm() uint64

// uefiSlots is de kandidatentabel van de stub, gepatcht door mkkernel -pe:
// [0]=aantal, [1]=stride in bytes tussen de payload-varianten in de geladen
// PE, [2..]=linkadres per variant (volgorde = voorkeursvolgorde; index 0 is
// de primaire variant, waarvan ook de PE-entry en de stub zelf draaien).
var uefiSlots [18]uint64

// Door de stub (init.s) NA de variantkopie op de gekozen L-kant geschreven —
// zie de package-doc. Namen zijn deel van het asm-contract.
var (
	imageHandle uint64 // EFI_HANDLE van onze image
	sysTable    uint64 // *EFI_SYSTEM_TABLE (blijft geldig na ExitBootServices)
	memmapSize  uint64 // gebruikte bytes in memmapBuf
	memmapDesc  uint64 // descriptor-grootte (firmware-bepaald, ≠ sizeof!)
	memmapVer   uint64 // descriptor-versie
	bootELVal   uint64 // CurrentEL bij bootKernel (2 = EL2: de HopOS-eis)
)

// memmapCap moet gelijk zijn aan MEMMAP_CAP in init.s (asm kent geen Go-
// constanten). 256KB ≈ 5000 descriptors — ruim voor een 700GB-server.
const memmapCap = 0x40000

// memmapBuf ontvangt de UEFI-memory-map (GetMemoryMap in de stub). Als
// Go-global ligt hij binnen de image: hij verhuist mee met de kopie en de
// runtime schrijft er nooit overheen.
var memmapBuf [memmapCap]byte

// hexLine is de regelbuffer van de stub voor de vrije-regio-dump bij "RAM
// WINDOW BUSY" (UCS-2: 2×16 hexcijfers + spatie + \r\n + NUL). uint16 dwingt
// de 2-byte-uitlijning af die OutputString-tekst nodig heeft.
var hexLine [40]uint16

// gopInfo is het firmware-beeld, door de stub vóór ExitBootServices uit het
// Graphics Output Protocol gehaald (asm-contract): [0] = lineaire
// framebuffer-basis (0 = geen bruikbare GOP), [1] = hoogte<<32 | breedte,
// [2] = PixelFormat<<32 | pixels-per-scanlijn. 32bpp gegarandeerd (de stub
// filtert op PixelFormat 0/1: 0=RGB, 1=BGR). gopDesc() decodeert het.
var gopInfo [3]uint64

// gopDesc decodeert gopInfo tot een fb.Desc. mapHigh=true (hwinit1) mapt het
// venster actief via MapHigh; false (Framebuffer, ná die map) checkt alleen
// Reachable. false-retour = geen bruikbaar/bereikbaar beeld. Eén decode-plek
// voor beide aanroepers.
func gopDesc(mapHigh bool) (fb.Desc, bool) {
	base := gopInfo[0]
	if base == 0 {
		return fb.Desc{}, false
	}
	h := gopInfo[1] >> 32
	scan := gopInfo[2] & 0xffffffff
	span := scan * h * 4
	ok := Reachable(base, span)
	if mapHigh {
		ok = MapHigh(base, span)
	}
	if !ok {
		return fb.Desc{}, false
	}
	return fb.Desc{
		Base:   uintptr(base),
		Width:  int(uint32(gopInfo[1])),
		Height: int(h),
		Stride: int(scan) * 4,
		BPP:    32,
		SwapRB: gopInfo[2]>>32 == 0, // PixelFormat 0 = RGB → R/B ruilen
	}, true
}

// BootEL geeft het EL waarop de firmware ons afleverde (gemeten in
// bootKernel). HopOS eist 2: zonder EL2 geen stage-2-kooi.
func BootEL() int { return int(bootELVal) }

// SystemTable geeft de EFI_SYSTEM_TABLE-pointer (voor runtime services en de
// configuratietabel; boot services zijn na ExitBootServices ongeldig).
func SystemTable() uintptr { return uintptr(sysTable) }

// efiACPIGUID is EFI_ACPI_20_TABLE_GUID (8868e871-e4f1-11d3-bc22-0080c73c8881)
// zoals hij in het geheugen ligt: Data1/2/3 little-endian + Data4 als bytes,
// gelezen als twee LE-uint64's.
const (
	efiACPIGUIDLo = 0x11d3e4f18868e871
	efiACPIGUIDHi = 0x81883cc7800022bc
)

// RSDP zoekt de ACPI 2.0 RSDP in de EFI-configuratietabel (SystemTable+0x68:
// aantal, +0x70: array van {GUID(16), VendorTable(8)}). 0 = geen ACPI 2.0 —
// dan is dit platform voor HopOS onbruikbaar (de aanroeper meldt dat).
func RSDP() uintptr {
	st := SystemTable()
	if st == 0 {
		return 0
	}
	n := int(read64(st + 0x68))
	tbl := uintptr(read64(st + 0x70))
	if tbl == 0 || n <= 0 || n > 4096 {
		return 0
	}
	for i := 0; i < n; i++ {
		e := tbl + uintptr(i)*24
		if read64(e) == efiACPIGUIDLo && read64(e+8) == efiACPIGUIDHi {
			return uintptr(read64(e + 16))
		}
	}
	return 0
}

// MemDesc is één EFI_MEMORY_DESCRIPTOR uit de bij boot gesnapshotte
// memory-map — de RAM-waarheid van dit platform (het ACPI-equivalent van het
// DTB /memory-node).
type MemDesc struct {
	Type  uint32
	Start uint64
	Pages uint64 // 4KB-pagina's
	Attr  uint64
}

// UEFI-memory-types (UEFI-spec 7.2). Ná ExitBootServices is niet alleen
// EfiConventionalMemory vrij: de firmware geeft óók haar boot-services-code
// en -data terug (de OS mag die dan hergebruiken). Op de Altra ligt onze
// slot-pool (0xB8000000) in EfiBootServicesData — echt RAM, alleen in het
// boot-snapshot nog als "boot-services" geboekt; alleen op conventional
// filteren paniekte daar onterecht (gemeten 14-07). Reserved/MMIO/ACPI/
// runtime blijven verboden.
const (
	EfiLoaderCode       = 1
	EfiLoaderData       = 2
	EfiBootServicesCode = 3
	EfiBootServicesData = 4
	EfiConventionalMemory = 7
)

// usableRAM meldt of een memory-type ná ExitBootServices vrij RAM is voor de
// OS. Boot-services- en loader-regio's tellen mee (spec 7.2; Linux' efi doet
// hetzelfde) — reserved, runtime-services, ACPI en MMIO niet.
func usableRAM(t uint32) bool {
	switch t {
	case EfiLoaderCode, EfiLoaderData, EfiBootServicesCode, EfiBootServicesData, EfiConventionalMemory:
		return true
	}
	return false
}

// vaLimit is tamago's MMU-bereik: 39-bit VA (TCR_EL1 T0SZ=25) = 512GB, flat
// gemapt. Alles daarbóven bestaat voor de kern niet — een access fault er
// meteen op. Serversilicium legt periferie soms hoger (Altra: de SoC-UART
// op 0x1000_0260_0000, gemeten 13-07: blauw scherm zonder tekst doordat de
// eerste printk-read faultte en de panic via dezelfde printk verzoop).
// Reachable is dáárom de poortwachter vóór elk firmware-geleverd adres;
// de echte fix (48-bit VA in tamago's InitMMU) staat op de backlog.
const vaLimit = 1 << 39

// Reachable meldt of [base, base+size) binnen het MMU-bereik van de kern
// valt: onder de vlakke 512GB, of in een via MapHigh gemapt hoog blok —
// check vóór gebruik van elk adres dat de firmware aanlevert (SPCR-UART,
// ECAM, BAR's, framebuffer). MapHigh is de actieve tegenhanger.
func Reachable(base, size uint64) bool {
	if base+size < base {
		return false
	}
	if base+size <= vaLimit {
		return true
	}
	return reachableHigh(base, size)
}

// MemoryMap decodeert de door de stub bewaarde UEFI-memory-map.
func MemoryMap() []MemDesc {
	var out []MemDesc
	if memmapDesc == 0 {
		return nil
	}
	for off := uint64(0); off+memmapDesc <= memmapSize; off += memmapDesc {
		base := uintptr(unsafe.Pointer(&memmapBuf[0])) + uintptr(off)
		out = append(out, MemDesc{
			Type:  uint32(read64(base)), // Type(u32)+pad — lage 32 bits
			Start: read64(base + 8),
			Pages: read64(base + 24),
			Attr:  read64(base + 32),
		})
	}
	return out
}

// IsUsableRAM meldt of [base, base+size) volledig in ná-ExitBootServices
// vrij RAM valt — over aaneengesloten bruikbare regio's heen (de pool of een
// DMA-venster mag een boot-services- én een conventional-regio raken). De
// check vóór een stuk RAM buiten de eigen partitie in gebruik gaat: "vrij"
// moet gemeten zijn, niet aangenomen.
func IsUsableRAM(base, size uint64) bool {
	return UsableRun(base, size) >= size
}

// UsableRun geeft hoeveel bytes vanaf base aaneengesloten vrij RAM zijn
// (geklemd op want): loop de cursor door opeenvolgende bruikbare descriptors
// tot er een gat/verboden type komt. Robuust tegen fragmentatie én tegen een
// ongesorteerde map (descAt zoekt de dekkende descriptor per stap).
func UsableRun(base, want uint64) uint64 {
	cursor := base
	for cursor < base+want {
		d, ok := descAt(cursor)
		if !ok || !usableRAM(d.Type) {
			break
		}
		cursor = d.Start + d.Pages*4096
	}
	if cursor <= base {
		return 0
	}
	if run := cursor - base; run < want {
		return run
	}
	return want
}

// descAt geeft de memory-map-descriptor die pa bevat.
func descAt(pa uint64) (MemDesc, bool) {
	for _, d := range MemoryMap() {
		if pa >= d.Start && pa < d.Start+d.Pages*4096 {
			return d, true
		}
	}
	return MemDesc{}, false
}

// MemTotal telt het conventionele RAM in de memory-map op (plus onze eigen
// partitie, die de stub vóór het snapshot claimde en dus als LoaderData
// geboekt staat).
func MemTotal() uint64 {
	var n uint64
	for _, d := range MemoryMap() {
		if d.Type == EfiConventionalMemory {
			n += d.Pages * 4096
		}
	}
	return n + KernelSize
}

// ARM64 is de tamago-CPU-instantie (levert ook runtime/goos.Hwinit0 — de
// vroege MMU-init — via het arm64-package zelf).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// uartBase is de SPCR-console zodra hwinit1 hem gevonden heeft; 0 = printk
// is een no-op. Er bestaat geen universeel UART-adres vóór de ACPI-parse,
// dus een panic tussen rt0 en hwinit1 is stil — bij zo'n verdenking: zet
// hier tijdelijk het platform-UART-adres in (QEMU virt: 0x09000000; zo is
// de acpi-alignment-bug van 13-07 gevonden).
var uartBase uintptr

// acpiTables is de bij hwinit1 geparste tabellenset; Tables() geeft hem aan
// de main (nil = parse mislukt — de main meldt en stopt).
var acpiTables *acpi.Tables

// Tables geeft de bij boot geparste ACPI-tabellen.
func Tables() *acpi.Tables { return acpiTables }

//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	ARM64.Init()
	ARM64.EnableCache()
	ARM64.InitGenericTimers(0, 0) // CNTFRQ is door de firmware gezet
	idle.Enable()

	// Console-discovery: EFI-configuratietabel → RSDP → XSDT → SPCR. Puur
	// geheugen lezen (de tabellen liggen buiten onze RAM-declaratie en zijn
	// dus device-gemapt — ongecached, coherent). Faalt een stap, dan blijft
	// printk stil; de main kan via Tables()==nil alsnog de diagnose stellen
	// zodra er ooit een ander kanaal is.
	// De 48-bit-uitbreiding (mmu48.go) vóór alles wat firmware-adressen
	// aanraakt: Altra legt UART/ECAM boven tamago's vlakke 512GB. ALLEEN
	// op de HOP-kern (SystemTable gevuld door de stub): een app-image
	// heeft geen firmware-adressen nodig én zijn tabeladres (de carve)
	// ligt buiten de slotpartitie — daar schrijven = stage-2-fault
	// (gemeten 13-07 avond: crashlus bij de eerste job).
	if sysTable != 0 {
		extendVA()
	}

	if t, err := acpi.Parse(RSDP()); err == nil {
		acpiTables = t
		// MapHigh vóór gebruik: een onbereikbare UART laat de allereerste
		// printk faulten — en de panic verdrinkt dan in dezelfde printk
		// (Altra-meting 13-07: blauw scherm zonder tekst).
		if base, _, err := t.SPCR(); err == nil && base != 0 && MapHigh(base, 0x1000) {
			uartBase = uintptr(base)
		}
		// De core-lijst voor CPUOn/CoreID (board.go): MADT-volgorde is de
		// platform-nummering. Disabled cores eruit (review #14): CPU_ON op
		// zo'n MPIDR faalt, en ExpectedAppCores zou anders de accurate
		// PSCI-telling overschrijven met een te hoog getal.
		if cpus, _, err := t.MADT(); err == nil {
			for _, c := range cpus {
				if c.Enabled {
					madtCPUs = append(madtCPUs, c)
				}
			}
		}
	}

	// Het firmware-beeld (GOP, door de stub bewaard): ná ExitBootServices
	// zwijgt de firmware-console, dus dit is hoe het scherm blijft praten —
	// beeld = firmware-buffer, geen driver.
	if d, ok := gopDesc(true); ok {
		fb.Init(d)
	}
}

//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	if uartBase != 0 {
		pl011.Putc(uartBase, c)
	}
	fb.Putc(c) // no-op zonder scherm
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// Hash-DRBG op SHA-256: out_i = H(state ‖ ctr ‖ 0), state' = H(state ‖ ctr
// ‖ 1). De seed komt bij voorkeur uit een hardware-TRNG (metal/trng: FEAT_RNG
// op de O6N, SMCCC TRNG/DEN 0098 op de Altra); ontbreekt die, dan valt initRNG
// terug op timing-jitter (jitterSeed). Jitter is op echt silicium een serieuze
// bron (cache/branch/DRAM-variatie in de meetlussen, het jitterentropy-
// principe); op QEMU/TCG is hij zwakker — dáár draait ook geen productie-TLS.
// Is er een hardwarebron, dan herzaait de DRBG zich elke reseedInterval bytes
// met een verse draw (voorwaartse onvoorspelbaarheid); jitter herzaait niet
// (te duur, en de boot-seed volstaat). rngSource legt vast wat het werd.
//
// Bewust géén EFI_RNG_PROTOCOL: dat bleek eeuwig te kunnen blokkeren in
// firmware zonder werkende TRNG (gemeten 13-07, review #5). SMCCC TRNG loopt
// via een EL3-monitor en is met de EL3-check in metal/trng crash-veilig.
const reseedInterval = 1 << 20 // bytes tussen twee TRNG-herzaaiingen

var (
	drbgState   [32]byte
	drbgCtr     uint64
	rngSource   = "jitter"
	sinceReseed uint64
)

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	var seed [48]byte
	if src, ok := trng.Fill(seed[:]); ok {
		rngSource = src
	} else {
		jitterSeed(seed[:])
	}
	drbgState = sha256.Sum256(seed[:])
}

// jitterSeed vult dst uit timing-jitter: 512 hash-rondes waarvan de
// individuele DUUR (CNTPCT-delta per ronde) de entropie levert; de teller
// zelf gaat als monotone basis mee. De terugvaller als er geen hardware-TRNG
// is (Neoverse-N1/Altra zonder DEN 0098, of QEMU virt).
func jitterSeed(dst []byte) {
	var pool [48]byte
	var st [32]byte
	for i := 0; i < 512; i++ {
		binary.LittleEndian.PutUint64(pool[32:], ARM64.Counter())
		binary.LittleEndian.PutUint64(pool[40:], uint64(i))
		copy(pool[:32], st[:])
		st = sha256.Sum256(pool[:])
	}
	for len(dst) > 0 {
		dst = dst[copy(dst, st[:]):]
		st = sha256.Sum256(st[:])
	}
}

// reseed mengt een verse hardware-draw in de DRBG-state: state' =
// H(state ‖ fresh). Faalt de bron even, dan blijft de oude state staan (nog
// steeds veilig) en proberen we bij de volgende drempel opnieuw.
func reseed() {
	var fresh [24]byte
	if _, ok := trng.Fill(fresh[:]); ok {
		var in [56]byte
		copy(in[:32], drbgState[:])
		copy(in[32:], fresh[:])
		drbgState = sha256.Sum256(in[:])
	}
	sinceReseed = 0
}

// RNGSource geeft de gekozen entropiebron ("rndr", "smccc-trng" of "jitter")
// terug — voor de discovery-print en de boot-log.
func RNGSource() string { return rngSource }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) {
	if rngSource != "jitter" && sinceReseed >= reseedInterval {
		reseed()
	}
	var in [48]byte
	for len(b) > 0 {
		drbgCtr++
		copy(in[:32], drbgState[:])
		binary.LittleEndian.PutUint64(in[32:], drbgCtr)
		in[40] = 0
		out := sha256.Sum256(in[:])
		in[40] = 1
		drbgState = sha256.Sum256(in[:])
		n := copy(b, out[:])
		b = b[n:]
		sinceReseed += uint64(n)
	}
}

// mpidr leest MPIDR_EL1 (cpu_arm64.s).
func mpidr() uint64

// CoreID geeft de eigen core-index: de plek van het eigen MPIDR in de
// MADT-volgorde (dé platform-nummering; de Altra nummert via aff1/aff2).
// Vóór de ACPI-parse is alleen de boot-core actief → 0.
func CoreID() int { return coreIDFromMADT() }

// read64 leest een 8-byte little-endian woord op fysiek adres pa. Lokaal en
// simpel gehouden (geen metal/dev-import): dit zijn gewone geheugenreads.
func read64(pa uintptr) uint64 {
	return *(*uint64)(unsafe.Pointer(pa))
}
