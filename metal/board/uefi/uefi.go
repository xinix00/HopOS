// Package uefi biedt HopOS-support voor élk AArch64-platform met
// UEFI-firmware en ACPI — de Ampere Altra (128 cores) voorop, met QEMU
// -M virt + EDK2 als proeftuin die exact dezelfde weg bewandelt (BOOTAA64.EFI
// op een FAT-medium). Waar de Pi's board-specifieke bring-up nodig hebben,
// is dit het universele pad: de firmware beschrijft het ijzer (ACPI), wij
// lezen het uit (metal/fw/acpi) — ontwerpprincipe "universeel boven
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
	"unsafe" // geheugenreads op firmware-adressen + go:linkname

	"github.com/usbarmory/tamago/arm64"

	"github.com/xinix00/HopOS/metal/v2/cpu/drbg"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/cpu/trng"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/conlog"
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"github.com/xinix00/HopOS/metal/v2/driver/ns16550"
	"github.com/xinix00/HopOS/metal/v2/driver/pl011"
	"github.com/xinix00/HopOS/metal/v2/fw/acpi"
	"github.com/xinix00/HopOS/metal/v2/fw/bootcfg"
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

// sctlrEL1 leest SCTLR_EL1 (cpu_arm64.s): bit 0 = MMU aan.
func sctlrEL1() uint64

// uefiSlots is de kandidatentabel van de stub, gepatcht door mkkernel -pe:
// [0]=aantal, [1]=stride in bytes tussen de payload-varianten in de geladen
// PE, [2..]=linkadres per variant (volgorde = voorkeursvolgorde; index 0 is
// de primaire variant, waarvan ook de PE-entry en de stub zelf draaien).
// In reloc-modus (mkkernel -reloc) is stride 0: er is één payload voor álle
// kandidaten en de stub herleidt de adressen zelf (zie uefiReloc).
var uefiSlots [18]uint64

// uefiReloc is de relocatie-descriptor van mkkernel -reloc: [0] = offset van
// de u32-offsettabel t.o.v. de payload-start in de geladen PE, [1] = aantal
// entries. Elke entry wijst een 8-byte-woord met een absoluut adres aan; de
// stub telt er (gekozen venster − linkbasis) bij op ná de kopie. [1] == 0 =
// klassieke multi-variant-PE, de stub slaat de lus dan over. Zo draagt één
// payload álle venster-kandidaten (74MB → ~12,5MB; docs/archief/pe-relocatie.md).
var uefiReloc [2]uint64

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
var hexLine [80]uint16

// gopInfo is het firmware-beeld, door de stub vóór ExitBootServices uit het
// Graphics Output Protocol gehaald (asm-contract): [0] = lineaire
// framebuffer-basis (0 = geen bruikbare GOP), [1] = hoogte<<32 | breedte,
// [2] = PixelFormat<<32 | pixels-per-scanlijn. 32bpp gegarandeerd (de stub
// filtert op PixelFormat 0/1: 0=RGB, 1=BGR). gopDesc() decodeert het.
var gopInfo [3]uint64

// cfgCap moet gelijk zijn aan CFG_CAP in init.s (asm kent geen Go-constanten).
const cfgCap = 0x4000 // 16KB — de gui-template is 8KB; met 4KB (t/m 17-09) verdween de tweede helft stil

// cfgBuf/cfgLen: hopos.cfg van de ESP-root, door de stub vóór ExitBootServices
// via het firmware-SimpleFileSystem gelezen (asm-contract, gopInfo-patroon:
// verhuist met de image mee). Géén FS-driver in HopOS — de firmware leest zijn
// eigen FAT (dezelfde weg waarlangs de PE geladen is), HopOS parseert alleen;
// exact het cmdline.txt-model van de Pi. Beheer = het tekstbestandje op de
// stick bewerken (elke computer mount FAT). cfgLen 0 = geen bestand → defaults.
// Ditzelfde bestand draagt straks ook de overige node-config (API-key e.d.).
var (
	cfgBuf [cfgCap]byte
	cfgLen uint64
)

// BootConfigAll geeft ALLE key=-waarden uit hopos.cfg, in volgorde (nil = geen).
// Eén `key=value` per regel, #-regels zijn commentaar; enkelvoudige config heeft
// één waarde, een herhaalde sleutel (hopos.init[]={...}) meerdere. cmd/hopos'
// bootParam pakt de eerste voor enkelvoudige sleutels.
//
// De lezing zelf staat in fw/bootcfg — dezelfde parser die de Pi op zijn
// hopos.cfg gebruikt en die het blob van de LicheeRV leest. Dat is de hele
// reden dat hij daar staat: één bestand hoort op élk board hetzelfde te
// betekenen.
func BootConfigAll(key string) []string {
	n := cfgLen
	if n > uint64(len(cfgBuf)) {
		n = uint64(len(cfgBuf)) // tegen een kapotte asm-lengte (contract-schending)
	}
	return bootcfg.All(string(cfgBuf[:n]), key)
}

// GOPFramebuffer geeft het firmware-beeld dat de stub bewaarde — de
// board.Board.Framebuffer-bron voor de hop-helft (hwinit1 deed fb.Init al
// voor de vroege console; de main mag opnieuw — Init is idempotent genoeg).
func GOPFramebuffer() (fb.Desc, bool) { return gopDesc(false) }

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
	w := uint64(uint32(gopInfo[1]))
	scan := gopInfo[2] & 0xffffffff
	// Firmware-input kruislings toetsen: het venster dat we straks beschrijven is
	// stride×height, dus een breedte die daar niet in past (scan < w) laat de
	// renderer per regel voorbij de bereikbare span schrijven. Ook 0-waarden
	// weigeren — dan is er geen beeld om te beschrijven.
	if w == 0 || h == 0 || scan < w {
		return fb.Desc{}, false
	}
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
		Width:  int(w),
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
}

// UEFI-memory-types (UEFI-spec 7.2). Ná ExitBootServices is niet alleen
// EfiConventionalMemory vrij: de firmware geeft óók haar boot-services-code
// en -data terug (de OS mag die dan hergebruiken). Op de Altra valt een groot
// deel van het vrije DRAM in EfiBootServicesData — echt RAM, alleen in het
// boot-snapshot nog als "boot-services" geboekt; alleen op conventional
// filteren zou dat onterecht weggooien (gemeten 14-07). Reserved/MMIO/ACPI/
// runtime blijven verboden.
const (
	EfiLoaderCode         = 1
	EfiLoaderData         = 2
	EfiBootServicesCode   = 3
	EfiBootServicesData   = 4
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

// pa48Limit is het absolute PA-plafond van de 48-bit-wereld die extendVA
// opzet (T0SZ=16): tot hier kan MapHigh mappen — voor MMIO én, sinds 15-07,
// voor het hoge DRAM van de slot-pool (Altra: ~300GB boven de vlakke 512GB).
const pa48Limit = uint64(1) << 48

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

// uart16550/uartShift: de registerlayout van de SPCR-console — PL011 (Altra,
// QEMU) of 16550 met een registerstap (DesignWare 8250 op de O6N, 32-bit-
// stride). Beide poke-lagen zijn pure functies zonder init, dus veilig als
// printk-hook.
var (
	uart16550 bool
	uartShift uint
)

// earlyUART is de PL011 waarop de asm ná ExitBootServices zijn "K2"/"K1"-
// markers zet en die hwinit1 meteen als console-spiegel neemt: een link-
// time constante van het board (early_o6n.go: de debug-header van de Orion;
// early_generic.go: 0 = geen). Vóór hwinit1 is er geen ACPI en dus geen
// SPCR — dit is de enige stem in dat gat, en de reden dat een stille boot
// nu in fases valt.
//
// uartMirror is een tweede PL011 waar printk óók naartoe schrijft: de
// debug-header van een bord waarvan de SPCR een andere UART noemt (O6N:
// UART2 op 0x040d0000). 0 = geen. Gezet via hopos.uart= (hwinit1) of door
// een board-pakket (MirrorConsole).
var uartMirror uintptr

// MirrorConsole laat printk vanaf nu ook naar de PL011 op base schrijven.
// Idempotent; base == de SPCR-UART is een no-op.
func MirrorConsole(base uintptr) {
	if base != 0 && base != uartBase {
		uartMirror = base
	}
}

// parseHex leest "0x..." of kale hex.
func parseHex(s string) (uint64, bool) {
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if s == "" || len(s) > 16 {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint64(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint64(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint64(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// acpiTables is de bij hwinit1 geparste tabellenset; Tables() geeft hem aan
// de main (nil = parse mislukt — de main meldt en stopt).
var acpiTables *acpi.Tables

// Tables geeft de bij boot geparste ACPI-tabellen.
func Tables() *acpi.Tables { return acpiTables }

// EarlyMark zet één teken op de vroege UART (asm, init.s): de meetlat van
// de boot vóór en tijdens hwinit1. No-op zonder earlyUART.
func EarlyMark(c byte)

// fwFacts: de firmware-feiten die de stub vóór ExitBootServices verzamelde,
// als blok in de carve (fwFactsOff, plan.go) zodat een GEFLIPTE kern — die
// zonder firmware binnenkomt (init.s fwentry, x1 = 0) — ze terugvindt. De
// koude boot publiceert ze in hwinit1, de geflipte boot herstelt ze daar,
// vóór alles wat ze nodig heeft (extendVA, ACPI, GOP, config). Het blok ligt
// buiten de Go-RAM (Device-gemapt): alleen 8-byte dev-toegang, geen memmove.
//
//	+0    magic "HOPFWFCT"   +8   sysTable      +16  imageHandle
//	+24   memmapSize          +32  memmapDesc    +40  memmapVer
//	+48   bootELVal           +56  gopInfo[3]    +80  cfgLen
//	+88   kooi-feiten (cage_origin.go, 4 woorden)
//	+0x1000 cfgBuf[:4K]        +0x2000 memmapBuf (memmapCap)
//	+0x42000 cfgBuf[4K:]       (de rest van een config > 4KB)
//
// De indeling ligt VAST: de stick-kern die vandaag draait publiceert zo, en
// een nieuwere geflipte kern moet dat blok kunnen lezen (17-09: een
// verschoven map-offset = een crash bij elke flip; 20-09: een "reparatie"
// die bootELVal naar +120 verhuisde las gopInfo van de stick-kern één woord
// te vroeg en stierf in de boot — bundel 49). Nieuwe woorden komen ALTIJD
// achteraan. Een langere config gaat in een staart die oude kernen nooit
// schrijven en nieuwe alleen lezen als cfgLen daarom vraagt.
const (
	fwFactsMagic = 0x5443465746504f48 // "HOPFWFCT"
	fwFactsCfg   = 0x1000
	fwFactsCfg1  = 0x1000 // eerste 4KB van de config
	fwFactsMap   = 0x2000
	fwFactsCfg2  = fwFactsMap + memmapCap // config voorbij 4KB
	fwFactsSize  = fwFactsCfg2 + cfgCap - fwFactsCfg1
)

func fwFactsPublish() {
	b := Base() + fwFactsOff
	persistentCage = reservedCarve(uint64(Base()), carveOff, carveSize, cageReservation{})
	dev.Write64(b+cageFactsOffset, cageFactsTag)
	dev.Write64(b+cageFactsOffset+8, cageFactsVersion)
	dev.Write64(b+cageFactsOffset+16, persistentCage.Base)
	dev.Write64(b+cageFactsOffset+24, persistentCage.Size)
	dev.Write64(b+8, sysTable)
	dev.Write64(b+16, imageHandle)
	dev.Write64(b+24, memmapSize)
	dev.Write64(b+32, memmapDesc)
	dev.Write64(b+40, memmapVer)
	dev.Write64(b+48, bootELVal)
	for i, v := range gopInfo {
		dev.Write64(b+56+uintptr(i)*8, v)
	}
	dev.Write64(b+80, cfgLen)
	devCopyOut(b+fwFactsCfg, cfgBuf[:fwFactsCfg1])
	devCopyOut(b+fwFactsMap, memmapBuf[:memmapSize])
	devCopyOut(b+fwFactsCfg2, cfgBuf[fwFactsCfg1:])
	dev.Write64(b, fwFactsMagic)
	dev.MB()
}

func fwFactsRestore() bool {
	b := Base() + fwFactsOff
	if dev.Read64(b) != fwFactsMagic {
		return false
	}
	var err error
	persistentCage, err = decodeCageReservation(dev.Read64(b+cageFactsOffset), dev.Read64(b+cageFactsOffset+8), dev.Read64(b+cageFactsOffset+16), dev.Read64(b+cageFactsOffset+24), carveSize)
	if err != nil {
		panic(err)
	}
	sysTable = dev.Read64(b + 8)
	imageHandle = dev.Read64(b + 16)
	memmapSize = dev.Read64(b + 24)
	memmapDesc = dev.Read64(b + 32)
	memmapVer = dev.Read64(b + 40)
	bootELVal = dev.Read64(b + 48)
	for i := range gopInfo {
		gopInfo[i] = dev.Read64(b + 56 + uintptr(i)*8)
	}
	cfgLen = dev.Read64(b + 80)
	if cfgLen > cfgCap {
		cfgLen = cfgCap
	}
	if memmapSize > memmapCap {
		memmapSize = memmapCap
	}
	devCopyIn(cfgBuf[:fwFactsCfg1], b+fwFactsCfg)
	devCopyIn(memmapBuf[:memmapSize], b+fwFactsMap)
	if cfgLen > fwFactsCfg1 {
		devCopyIn(cfgBuf[fwFactsCfg1:cfgLen], b+fwFactsCfg2)
	}
	return sysTable != 0
}

// FwFactsCopy (kernflip.BoardHandoff): het fwFacts-blok van dit venster naar
// de carve van het nieuwe venster — daar zoekt de geflipte kern (Base() is
// dan het nieuwe venster). Beide kanten Device-geheugen: per 8 bytes.
func FwFactsCopy(newBase uintptr) {
	src, dst := Base()+fwFactsOff, newBase+fwFactsOff
	size := uintptr(fwFactsSize)
	if !MapHigh(uint64(dst), uint64(size)) {
		printkStr("kernflip: firmware facts NOT copied (new window unreachable)\n")
		return
	}
	for off := uintptr(8); off < size; off += 8 { // magic als laatste
		dev.Write64(dst+off, dev.Read64(src+off))
	}
	dev.Write64(dst, dev.Read64(src))
	dev.MB()
}

func printkStr(s string) { earlySay(s) }

// devCopyOut/devCopyIn: per 8 bytes (het blok is Device-geheugen).
func devCopyOut(dst uintptr, src []byte) {
	for i := 0; i < len(src); i += 8 {
		var w uint64
		for k := 0; k < 8 && i+k < len(src); k++ {
			w |= uint64(src[i+k]) << (8 * k)
		}
		dev.Write64(dst+uintptr(i), w)
	}
}

func devCopyIn(dst []byte, src uintptr) {
	for i := 0; i < len(dst); i += 8 {
		w := dev.Read64(src + uintptr(i))
		for k := 0; k < 8 && i+k < len(dst); k++ {
			dst[i+k] = byte(w >> (8 * k))
		}
	}
}

//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	coldBoot := sysTable != 0 // capture before restoring the previous kernel's facts
	// Firmware-feiten: een koude boot legt ze in de carve, een geflipte kern
	// (sysTable nog 0, want geen stub) haalt ze daar vandaan. Vóór alles.
	if sysTable != 0 {
		fwFactsPublish()
	} else if fwFactsRestore() {
		earlySay("hwinit1: firmware facts restored from the carve (kernel flip)\n")
	}
	// De vroege UART is vanaf het eerste moment óók printk-bestemming: een
	// panic in de runtime-init hieronder wordt dan gehoord.
	if earlyUART != 0 {
		uartMirror = earlyUART
	}
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
	if earlyUART != 0 {
		earlySay("\nhwinit1: go runtime up, parsing ACPI\n")
	}
	if t, err := acpi.Parse(RSDP()); err == nil {
		acpiTables = t
		// MapHigh vóór gebruik: een onbereikbare UART laat de allereerste
		// printk faulten — en de panic verdrinkt dan in dezelfde printk
		// (Altra-meting 13-07: blauw scherm zonder tekst).
		// Op een bord met een vroege UART (O6N) is dát de console en blijft
		// de SPCR-UART onaangeraakt: die wijst naar UART0/UART3, waarvan de
		// SCP de klok dichthoudt — gemeten 09-09: elke write daarheen hield
		// de bus vast tot de SE-firmware de klok even terugzette.
		if c, err := t.Console(); err == nil && c.Base != 0 && (earlyUART == 0 || t.OEMID != EarlyUARTOEM) && MapHigh(c.Base, 0x1000) {
			uart16550, uartShift = c.Is16550(), c.Shift
			uartBase = uintptr(c.Base)
		}
		// hopos.uart=0xADDR in hopos.cfg: een PL011 die de SPCR níét noemt —
		// de O6N-firmware zet haar SPCR op UART0/UART3 terwijl de debug-header
		// UART2 (0x040d0000) is. Vóór de cfg-lezing is de stick-config al in
		// cfgBuf (de stub las hem vóór ExitBootServices), dus dit werkt vanaf
		// de allereerste printk. Wordt er al op geprint via de SPCR, dan
		// spiegelt printk naar beide.
		if v := BootConfigAll("hopos.uart"); len(v) > 0 {
			if base, ok := parseHex(v[0]); ok && base != 0 && MapHigh(base, 0x1000) {
				MirrorConsole(uintptr(base))
			}
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

	if earlyUART != 0 {
		if acpiTables != nil {
			earlySay("hwinit1: ACPI parsed, console on the header UART\n")
		} else {
			earlySay("hwinit1: ACPI PARSE FAILED\n")
		}
	}

	// Het firmware-beeld (GOP, door de stub bewaard): ná ExitBootServices
	// zwijgt de firmware-console, dus dit is hoe het scherm blijft praten —
	// beeld = firmware-buffer, geen driver.
	// A resident display may still own these pixels across FLIP. Main decides
	// whether to start a console after restoring framebuffer grant ownership.
	if d, ok := gopDesc(true); ok && coldBoot {
		fb.Init(d)
	}
	earlySay("hwinit1: done\n")
}

// earlySay print via de printk-hook — voor hwinit1, waar fmt nog niet mag.
func earlySay(s string) {
	for i := 0; i < len(s); i++ {
		printk(s[i])
	}
}

//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	conlog.Put(c) // ook over het netwerk op te vragen (driver/conlog)
	fb.Putc(c)    // beeld eerst: UART-pollen mag de schermdiagnose niet verbergen
	if uartBase != 0 {
		if uart16550 {
			ns16550.Putc(uartBase, uartShift, c)
		} else {
			pl011.Putc(uartBase, c)
		}
	}
	if uartMirror != 0 {
		pl011.Putc(uartMirror, c)
	} else if uartBase == 0 && earlyUART != 0 {
		// Vóór hwinit1 (runtime-init, een throw in rt0): de vroege UART is
		// dan de enige stem — anders sterft zo'n throw geluidloos.
		pl011.Putc(earlyUART, c)
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// De gedeelde SHA-256-DRBG (metal/cpu/drbg), hier geseed uit trng.Fill: de
// volle bronkeuze — FEAT_RNG (RNDR) op de O6N, SMCCC TRNG/DEN 0098 op de
// Altra — en anders de timing-jitter-terugval van het drbg-pakket.
//
// Bewust géén EFI_RNG_PROTOCOL: dat bleek eeuwig te kunnen blokkeren in
// firmware zonder werkende TRNG (gemeten 13-07, review #5). SMCCC TRNG loopt
// via een EL3-monitor en is met de EL3-check in metal/cpu/trng crash-veilig.

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	fill := trng.Fill
	if earlyUART != 0 {
		// O6N: RNDR is er nooit apart getimed (de stille boots van 09-09
		// bleken een dangling TTBR1, niet RNDR) — tot die meting de
		// jitter-seed, geen hardwarebron.
		fill = func([]byte) (string, bool) { return "", false }
	}
	drbg.Init(fill, ARM64.Counter)
}

// RNGSource geeft de gekozen entropiebron ("rndr", "smccc-trng" of "jitter")
// terug — voor de discovery-print en de boot-log.
func RNGSource() string { return drbg.Source() }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) { drbg.Read(b) }

// CoreID geeft de eigen core-index: de plek van het eigen MPIDR in de
// MADT-volgorde (dé platform-nummering; de Altra nummert via aff1/aff2).
// Vóór de ACPI-parse is alleen de boot-core actief → 0.
func CoreID() int { return coreIDFromMADT() }

// read64 leest een 8-byte little-endian woord op fysiek adres pa. Lokaal en
// simpel gehouden (geen metal/dev-import): dit zijn gewone geheugenreads.
func read64(pa uintptr) uint64 {
	return *(*uint64)(unsafe.Pointer(pa))
}
