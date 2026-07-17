// mmu48.go — het 48-bit-VA-haakje bovenop tamago's MMU. tamago's InitMMU
// bouwt een vlakke 39-bit-wereld (512GB, L1-tabel op Base()+0x4000); dat is
// genoeg voor elk klein board, maar serversilicium legt periferie hoger: de
// Altra-UART woont op 0x1000_0260_0000 (16TB) en ECAM/BAR's kunnen hoog
// liggen. In plaats van tamago te vervangen schuiven we er één tabelniveau
// boven (dezelfde beweging als EL2/stage-2, die tamago ook niet kent):
//
//   - een eigen L0-tabel (Base()+0x9000) waarvan ingang 0 naar tamago's
//     bestaande L1 wijst — de lage 512GB blijven byte-voor-byte gelijk;
//   - TCR_EL1 naar T0SZ=16 (48-bit VA) + IPS naar wat het silicium kan;
//   - MapHigh hangt er per 512GB-regio een eigen L1 met 1GB-device-blokken
//     in, on demand (UART, ECAM, BAR — wat ACPI maar aanwijst).
//
// De omschakeling zelf gebeurt met de MMU heel even uit (switchMMU in
// cpu_arm64.s): tussen een TCR- en TTBR-write bestaat anders een venster
// waarin de core oud+nieuw kan mengen. Met MMU uit is er geen vertaling om
// te mengen — en de routine raakt in dat venster geen geheugen aan.
//
// Sinds 15-07 mapt usablePool via ditzelfde luik ook het hoge DRAM (Altra:
// de ~300GB bulk boven 512GB) — de slot-pool is daarmee de volle machine;
// alleen de kern zelf (Go-RAM + carve) blijft onder de grens leven.
package uefi

import (
	"unsafe"

	"hop-os/metal/dev"
)

const (
	// Tabeladressen: in het gat tussen tamago's tabellen en de image, en
	// BEWUST binnen de Go-RAM-regio — die is Normal-WB gemapt, net als
	// tamago's eigen tabellen, dus de table-walker (cacheable walks,
	// TCR IRGN/ORGN=WB) leest coherent wat wij schrijven. In de carve
	// (device-gemapt) waren de writes uncached en de walks cached: stale
	// lines op echt silicium (Dereks review #1; QEMU is cache-blind).
	// Wél voorbij +0x8000: daar ligt tamago's tekst-grens-L3
	// (l3pageTableStart + l3pageTableSize*8) — die overschrijven was de
	// stille exit(1) van 13-07. Het gat 0x9000..0x10000 is precies
	// 1 L0 + highL1Max pagina's.
	l0Off     = 0x9000 // onze L0 (4KB)
	highL1Off = 0xA000 // pool van L1-pagina's (hoge regio's)
	highL1Max = 6      // 0xA000..0xFFFF — tot de image (text op +0x10000)
	tamagoL1  = 0x4000 // tamago's L1 (InitMMU-conventie, arm64/mmu.go)

	// Descriptor-bits (zelfde encodering als tamago's mmu.go).
	tteTable  = 0b11
	tteBlock  = 0b01
	tteAF     = 1 << 10
	tteOuter  = 0b10 << 8
	deviceBlk = tteBlock | tteAF | tteOuter // attr-index 0 = device (MAIR)

	gb = 1 << 30
)

// highL1 administreert welke 512GB-regio (L0-index) welke poolpagina kreeg.
var highL1 [highL1Max]uint64

// vaExtended: is de 48-bit-wereld actief (extendVA gedraaid)?
var vaExtended bool

// extendVA schuift de L0 boven tamago's tabellen en zet TCR op 48-bit.
// Idempotent genoeg voor één aanroep uit hwinit1; vóór die tijd is de lage
// 512GB-wereld gewoon geldig.
func extendVA() {
	l0 := Base() + l0Off
	for i := uintptr(0); i < 4096; i += 8 {
		*(*uint64)(unsafe.Pointer(l0 + i)) = 0
	}
	*(*uint64)(unsafe.Pointer(l0)) = uint64(Base()+tamagoL1) | tteTable

	// IPS: het kleinste van 48-bit en wat ID_AA64MMFR0.PARange meldt.
	ips := mmfr0() & 0xF
	if ips > 5 {
		ips = 5 // 48-bit is genoeg; 52-bit vergt een ander granule-verhaal
	}
	tcr := readTCR()
	tcr = tcr&^uint64(0x3F) | 16         // T0SZ: 64-48
	tcr = tcr&^(uint64(7)<<32) | ips<<32 // IPS
	dev.MB()
	switchMMU(tcr, uint64(l0))
	vaExtended = true
}

// MapHigh zorgt dat [base, base+size) gemapt is als device-geheugen (per
// 1GB-blok). Adressen onder de 512GB zijn al vlak gemapt (true); boven de
// 48-bit-grens of bij een volle pool: false — de aanroeper meldt en
// degradeert. Dit is het luik waardoor hoge firmware-adressen (Altra: UART
// op 16TB, ECAM's boven 512GB — gemeten 13-07) alsnog bereikbaar worden.
func MapHigh(base, size uint64) bool {
	if size == 0 {
		return false // een leeg venster "mappen" is een aanroepersfout
	}
	for pa := base &^ uint64(gb-1); pa < base+size; pa += gb {
		if !mapGB(pa) {
			return false
		}
	}
	return true
}

// mapGB mapt één 1GB-blok.
func mapGB(pa uint64) bool {
	if pa < vaLimit {
		return true
	}
	if !vaExtended || pa >= 1<<48 {
		return false
	}
	l0idx := pa >> 39 // 512GB-regio
	var l1 uintptr
	for i := range highL1 {
		switch highL1[i] {
		case l0idx:
			l1 = Base() + highL1Off + uintptr(i)*4096
		case 0:
			// vrije poolpagina: claimen, schoonvegen, in de L0 hangen
			l1 = Base() + highL1Off + uintptr(i)*4096
			for off := uintptr(0); off < 4096; off += 8 {
				*(*uint64)(unsafe.Pointer(l1 + off)) = 0
			}
			highL1[i] = l0idx
			dev.MB()
			*(*uint64)(unsafe.Pointer(Base() + l0Off + uintptr(l0idx)*8)) = uint64(l1) | tteTable
		default:
			continue
		}
		break
	}
	if l1 == 0 {
		return false // pool op
	}
	l1idx := (pa >> 30) & 0x1FF
	*(*uint64)(unsafe.Pointer(l1 + uintptr(l1idx)*8)) = pa&^uint64(gb-1) | deviceBlk
	dev.MB()
	tlbiAll() // de ingang was invalid; conservatief vegen is goedkoop
	return true
}

// UnmapHigh geeft de 1GB-blokken van [base, base+size) terug (invalide L1-
// ingangen; TLB gewist). Voor scan-en-verwerp: een ECAM-segment zonder onze
// NIC hoeft niet gemapt te blijven — anders vult de kleine pool zich met
// segmenten die we niet gebruiken (de Altra heeft tot 8 PCIe-segmenten, elk in
// een eigen 512GB-regio; de pool heeft er 6). Onder de vlakke 512GB niets te
// doen.
//
// Precies-per-blok (review golf-2 #8): alléén de eigen 1GB-blokken worden
// geïnvalideerd, en de highL1-poolpagina + L0-ingang gaan pas terug wanneer die
// pagina géén geldige ingang meer heeft. Zo overleeft een ándere mapping in
// dezelfde 512GB-regio (de UART, de GOP-framebuffer, een buur-ECAM) het
// verwerpen van dit segment — de oude versie sloopte de hele regio en liet de
// eerstvolgende printk in een niet-gemapte console faulten (de stille
// "blue screen zonder tekst"-modus van 13-07).
func UnmapHigh(base, size uint64) {
	if size == 0 || base < vaLimit {
		return
	}
	for pa := base &^ uint64(gb-1); pa < base+size; pa += gb {
		l0idx := pa >> 39
		for i := range highL1 {
			if highL1[i] != l0idx {
				continue
			}
			l1 := Base() + highL1Off + uintptr(i)*4096
			l1idx := (pa >> 30) & 0x1FF
			*(*uint64)(unsafe.Pointer(l1 + uintptr(l1idx)*8)) = 0
			// L1-pagina helemaal leeg? Dan pas de regio (L0-ingang + poolslot)
			// teruggeven; anders blijft een buur-mapping in dezelfde regio staan.
			empty := true
			for off := uintptr(0); off < 4096; off += 8 {
				if *(*uint64)(unsafe.Pointer(l1 + off)) != 0 {
					empty = false
					break
				}
			}
			if empty {
				*(*uint64)(unsafe.Pointer(Base() + l0Off + uintptr(l0idx)*8)) = 0
				highL1[i] = 0
			}
			break
		}
	}
	dev.MB()
	tlbiAll()
}

// MapNormal hermapt [pa, pa+size) — 2MB-gealigneerd — van Device naar
// Normal-WB inner-shareable (XN) in tamago's eigen stage-1: het bereik moet in
// een GB liggen die al een L2-tabel heeft (de RAM-GB — daar wonen Go-RAM én
// carve, dus ook de NIC-DMA-regio). Dít is hoe de igb-frame-buffers cache-snel
// worden (netdoorvoer stap "cacheable", 17-07): de driver doet de expliciete
// hygiëne (dev.CleanInv rond de DMA), deze remap maakt de bulk-reads gecached.
// Alleen bestaande Device-BLOKKEN worden geflipt — RAM, tabellen of iets
// onverwachts aanraken weigert (false: de aanroeper meldt en draait ongecached
// door). Coherentie tussen de node-cores is hardware (inner-shareable); de
// TLB's gaan om via tlbiAll (VMID 0 gedeeld met de node-SMP-cores, smp.s).
func MapNormal(pa, size uintptr) bool {
	const blk = 2 << 20
	// Normal-WB inner-shareable 2MB-blok, execute-never: TTE_BLOCK | AF |
	// INNER_SH | attr-index 1 (MAIR: Normal WB) | XN — tamago's memoryAttributes.
	const normalBlk = 0x1<<0 | 1<<10 | 0x3<<8 | 1<<2 | 0x3<<53
	if pa == 0 || size == 0 || pa&(blk-1) != 0 || size&(blk-1) != 0 {
		return false
	}
	for a := pa; a < pa+size; a += blk {
		l1e := *(*uint64)(unsafe.Pointer(Base() + tamagoL1 + uintptr((uint64(a)>>30)&0x1FF)*8))
		if l1e&0b11 != 0b11 {
			return false // geen L2 onder deze GB: buiten de RAM-GB gevraagd
		}
		l2 := uintptr(l1e & 0x0000FFFFFFFFF000)
		ep := l2 + uintptr((uint64(a)>>21)&0x1FF)*8
		e := *(*uint64)(unsafe.Pointer(ep))
		if e&0b11 != 0b01 || (e>>2)&0x7 != 0 {
			return false // alleen een Device-blok (attr-index 0) flippen
		}
		*(*uint64)(unsafe.Pointer(ep)) = uint64(a) | normalBlk
	}
	dev.MB()
	tlbiAll()
	return true
}

// VAStatus rapporteert de 48-bit-MMU-toestand voor diagnose op het scherm
// (het hoge-map-pad wordt op QEMU nooit geraakt — alles ligt daar laag —
// dus de Altra is de eerste echte test; zonder serieel is dit hoe we zien
// waaróm een hoog adres onbereikbaar blijft). parange = ID_AA64MMFR0.PARange
// (5=48-bit, 6=52-bit); slotsUsed/slotsMax = de highL1-pool.
func VAStatus() (extended bool, tcr, parange uint64, slotsUsed, slotsMax int) {
	extended = vaExtended
	tcr = readTCR()
	parange = mmfr0() & 0xF
	for _, v := range highL1 {
		if v != 0 {
			slotsUsed++
		}
	}
	return extended, tcr, parange, slotsUsed, highL1Max
}

// MapFailReason geeft in één woord waaróm MapHigh(pa,·) zou falen — voor de
// diagnostische regel bij een overgeslagen ECAM/BAR.
func MapFailReason(pa uint64) string {
	switch {
	case !vaExtended:
		return "va-not-extended"
	case pa >= 1<<48:
		return "above-48bit"
	default:
		return "highL1-pool-full"
	}
}

// Reachable meldt of [base, base+size) voor de kern aanraakbaar is: onder de
// vlakke 512GB, of in een via MapHigh gemapt hoog blok. (MapHigh eerst
// aanroepen voor hoge adressen; Reachable is de passieve check.)
func reachableHigh(base, size uint64) bool {
	if !vaExtended {
		return false
	}
	for pa := base &^ uint64(gb-1); pa < base+size; pa += gb {
		l0idx := pa >> 39
		mapped := false
		for i := range highL1 {
			if highL1[i] == l0idx {
				l1e := *(*uint64)(unsafe.Pointer(Base() + highL1Off + uintptr(i)*4096 + uintptr((pa>>30)&0x1FF)*8))
				mapped = l1e&0b11 != 0
				break
			}
		}
		if !mapped {
			return false
		}
	}
	return true
}

// In cpu_arm64.s.
func readTCR() uint64
func mmfr0() uint64
func switchMMU(tcr, ttbr0 uint64)
func tlbiAll()
