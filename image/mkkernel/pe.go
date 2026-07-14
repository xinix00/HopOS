// PE/COFF-verpakking voor UEFI-boot (mkkernel -pe): de tamago-ELF wordt een
// AArch64 EFI-applicatie met échte secties — elk PT_LOAD-segment één
// PE-sectie met de eigen permissies (RX/RO/RW), zoals de Linux-arm64-kernel
// (efi-header.S) dat doet. Dat is geen cosmetiek: EDK2's image-protection
// mapt code-secties read-only-executable, en een alles-in-één-RWX-sectie
// betekent dat de stub zijn eigen globals niet kan schrijven (gemeten
// 2026-07-13: data abort op de eerste store, QEMU + EDK2-debugbuild).
// Strikte W^X-firmware (servers!) eist deze indeling sowieso.
//
// De relocatie-vraag lost de stub zelf op (metal/board/uefi/init.s is
// positie-onafhankelijk en verhuist de image naar het linkadres); voor
// loaders die zonder relocatie-informatie weigeren te verplaatsen ligt er
// een lege .reloc-sectie in (één blok met twee ABSOLUTE-padding-entries —
// de klassieke truc uit de x86-Linux-stub).
//
// Layout: RVA = 0x1000 + (paddr − load), en FileAlignment ==
// SectionAlignment == 0x1000 zodat RVA == bestandsoffset. De HopOS-linkstijl
// (-R 0x1000) legt de segmenten al pagina-rond aaneengesloten; de sprong van
// de firmware-lading naar het linkadres blijft daardoor één vlakke kopie.
package main

import (
	"debug/elf"
	"encoding/binary"
)

const (
	// peHeaderSize: alle headers passen ruim in één 4KB-pagina.
	peHeaderSize = 0x1000

	coffMachineARM64 = 0xaa64

	// Characteristics: EXECUTABLE_IMAGE | LINE_NUMS_STRIPPED |
	// DEBUG_STRIPPED (0x0002|0x0004|0x0200) — het Linux-arm64-trio; géén
	// RELOCS_STRIPPED: dat zou "laad mij alléén op ImageBase" betekenen.
	coffCharacteristics = 0x0206

	optMagicPE32Plus = 0x020b
	subsystemEFIApp  = 10

	// Sectie-characteristics.
	secCode  = 0x00000020
	secInitD = 0x00000040
	secExec  = 0x20000000
	secRead  = 0x40000000
	secWrite = 0x80000000
)

func put16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func put32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func put64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

// peSection is één sectie-in-wording: een venster op de platte image.
type peSection struct {
	name  string
	rva   uint32 // == bestandsoffset (FileAlignment == SectionAlignment)
	size  uint32 // pagina-rond; raw == virtual (BSS-nullen zitten in img)
	chars uint32
}

// wrapPE bouwt het PE-bestand uit de platte image (img = bytes vanaf load,
// incl. BSS-nullen) en de ELF-segmenten (voor de sectiegrenzen/permissies).
// entryOff is de ELF-entry relatief aan het laadadres. extras zijn de
// overige venster-varianten (zelfde build, ander linkadres): die komen als
// read-only data ná variant 0, elk op stride bytes — de stub kopieert de
// gekozen variant naar zijn linkadres, dus de loader hoeft ze alleen in het
// geheugen te zetten.
func wrapPE(img []byte, f *elf.File, load, entryOff uint64, extras [][]byte, stride uint32) []byte {
	round := func(v uint64) uint32 { return uint32((v + 0xfff) &^ 0xfff) }

	// Elk PT_LOAD één sectie. De segmenten liggen gesorteerd en pagina-rond
	// aaneengesloten (HopOS-linkstijl); namen zijn cosmetisch, de
	// characteristics zijn waar de loader op handelt.
	var secs []peSection
	var sizeOfCode, baseOfCode uint32
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Memsz == 0 {
			continue
		}
		s := peSection{
			rva:  peHeaderSize + uint32(p.Paddr-load),
			size: round(p.Memsz),
		}
		switch {
		case p.Flags&elf.PF_X != 0:
			s.name, s.chars = ".text", secCode|secExec|secRead
			if baseOfCode == 0 {
				baseOfCode = s.rva
			}
			sizeOfCode += s.size
		case p.Flags&elf.PF_W != 0:
			s.name, s.chars = ".data", secInitD|secRead|secWrite
		default:
			s.name, s.chars = ".rodata", secInitD|secRead
		}
		secs = append(secs, s)
	}
	last := secs[len(secs)-1]
	end := last.rva + last.size
	if e := peHeaderSize + round(uint64(len(img))); e > end {
		end = e
	}

	// De venster-varianten: één RO-sectie, aaneengesloten op stride vanaf
	// variant 0 (RVA peHeaderSize) — de stub rekent src = basis + k×stride.
	if len(extras) > 0 {
		payRVA := peHeaderSize + stride
		if payRVA < end {
			panic("mkkernel: stride overlapt de secties van variant 0")
		}
		secs = append(secs, peSection{".pay", payRVA, uint32(len(extras)) * stride, secInitD | secRead})
		end = payRVA + uint32(len(extras))*stride
	}

	// De lege .reloc als staart.
	relocRVA := end
	const relocSize = 12 // PageRVA(4) + BlockSize(4) + 2 ABSOLUTE-pad-entries
	secs = append(secs, peSection{".reloc", relocRVA, 0x1000, secInitD | secRead})
	imageSize := relocRVA + 0x1000

	out := make([]byte, imageSize)
	copy(out[peHeaderSize:], img)
	for i, e := range extras {
		copy(out[peHeaderSize+stride+uint32(i)*stride:], e)
	}

	// DOS-header: alleen de magic en de verwijzing naar de PE-header.
	out[0], out[1] = 'M', 'Z'
	put32(out, 0x3c, 0x40) // e_lfanew

	// PE-signature + COFF-header.
	pe := 0x40
	copy(out[pe:], "PE\x00\x00")
	put16(out, pe+4, coffMachineARM64)
	put16(out, pe+6, uint16(len(secs)))
	// TimeDateStamp/symbolen: 0 (reproduceerbaar).
	optSize := 112 + 6*8 // PE32+-basis + 6 datadirectories
	put16(out, pe+20, uint16(optSize))
	put16(out, pe+22, coffCharacteristics)

	// Optional header (PE32+).
	opt := pe + 24
	put16(out, opt+0, optMagicPE32Plus)
	put32(out, opt+4, sizeOfCode)
	put32(out, opt+16, peHeaderSize+uint32(entryOff)) // AddressOfEntryPoint
	put32(out, opt+20, baseOfCode)
	put64(out, opt+24, 0)            // ImageBase: geen voorkeur
	put32(out, opt+32, 0x1000)       // SectionAlignment
	put32(out, opt+36, 0x1000)       // FileAlignment
	put32(out, opt+56, imageSize)    // SizeOfImage
	put32(out, opt+60, peHeaderSize) // SizeOfHeaders
	put16(out, opt+68, subsystemEFIApp)
	put32(out, opt+108, 6) // NumberOfRvaAndSizes
	// Datadirectory 5 = Base Relocation Table → de lege .reloc.
	dir := opt + 112
	put32(out, dir+5*8, relocRVA)
	put32(out, dir+5*8+4, relocSize)

	// Sectieheaders (40 bytes elk), direct na de optional header.
	hdr := opt + optSize
	for _, s := range secs {
		copy(out[hdr:hdr+8], s.name)
		put32(out, hdr+8, s.size)  // VirtualSize
		put32(out, hdr+12, s.rva)  // VirtualAddress
		put32(out, hdr+16, s.size) // SizeOfRawData
		put32(out, hdr+20, s.rva)  // PointerToRawData (RVA == offset)
		put32(out, hdr+36, s.chars)
		hdr += 40
	}

	// Het .reloc-blok zelf: PageRVA=eerste sectie, BlockSize=12, twee
	// IMAGE_REL_BASED_ABSOLUTE-entries (type 0 = padding, geen effect).
	put32(out, int(relocRVA), peHeaderSize)
	put32(out, int(relocRVA)+4, relocSize)

	return out
}
