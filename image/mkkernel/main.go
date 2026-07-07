// mkkernel zet een tamago-ELF om naar het raw arm64 "Image"-formaat dat de
// Raspberry Pi-firmware (en elke Linux-capabele bootloader) laadt: platte
// bytes op het laadadres, met de 64-byte arm64 Image-header vooraan en een
// branch naar het ELF-entrypoint in het eerste instructiewoord.
//
// Gebruik: go run ./image/mkkernel -elf metal/probe5.elf -o kernel_2712.img -load 0x200000
//
// De ELF moet zó gelinkt zijn dat alle PT_LOAD-segmenten op of boven
// load+64 liggen (de header-ruimte); voor HopOS-images geldt
// TEXT_START = load + 0x10000.
package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mkkernel: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	elfPath := flag.String("elf", "", "invoer-ELF (tamago-image)")
	outPath := flag.String("o", "kernel_2712.img", "uitvoerbestand")
	loadAddr := flag.Uint64("load", 0x200000, "laadadres (kernel_address; arm64-default van de Pi-firmware)")
	flag.Parse()
	if *elfPath == "" {
		die("-elf is verplicht")
	}

	f, err := elf.Open(*elfPath)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()

	load := *loadAddr
	var fileEnd, memEnd uint64
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Paddr < load+64 {
			die("segment %#x ligt onder load+64 (%#x): geen ruimte voor de Image-header", p.Paddr, load+64)
		}
		if end := p.Paddr + p.Filesz - load; end > fileEnd {
			fileEnd = end
		}
		if end := p.Paddr + p.Memsz - load; end > memEnd {
			memEnd = end
		}
	}
	if fileEnd == 0 {
		die("geen PT_LOAD-segmenten")
	}
	if f.Entry < load+64 || f.Entry >= load+fileEnd || f.Entry%4 != 0 {
		die("entry %#x ongeldig voor load %#x", f.Entry, load)
	}

	img := make([]byte, fileEnd)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		if n, err := p.ReadAt(img[p.Paddr-load:p.Paddr-load+p.Filesz], 0); err != nil || uint64(n) != p.Filesz {
			die("segment %#x lezen: %d/%d, %v", p.Paddr, n, p.Filesz, err)
		}
	}

	// arm64 Image-header (Linux Documentation/arch/arm64/booting.rst):
	// code0 = branch naar de entry, magic "ARM\x64", 4K-pages, LE.
	binary.LittleEndian.PutUint32(img[0:], 0x14000000|uint32((f.Entry-load)/4)&0x03FFFFFF) // code0: b entry
	binary.LittleEndian.PutUint32(img[4:], 0)                                              // code1
	binary.LittleEndian.PutUint64(img[8:], 0)                                              // text_offset
	binary.LittleEndian.PutUint64(img[16:], memEnd)                                        // image_size (incl. BSS)
	binary.LittleEndian.PutUint64(img[24:], 0b010)                                         // flags: LE, 4K
	binary.LittleEndian.PutUint32(img[56:], 0x644d5241)                                    // magic "ARM\x64"

	if err := os.WriteFile(*outPath, img, 0o644); err != nil {
		die("%v", err)
	}
	fmt.Printf("%s: %d bytes (image_size %#x, entry %#x @ load %#x)\n",
		*outPath, len(img), memEnd, f.Entry, load)
}
