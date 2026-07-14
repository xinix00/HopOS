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
//
// Met -pe ontstaat een UEFI-applicatie (BOOTAA64.EFI, zie pe.go) en is -elf
// HERHAALBAAR: elke ELF is dezelfde build gelinkt op een eigen
// kandidaat-venster (load = TEXT − 0x10000, uit de ELF afgeleid). De stub
// (metal/board/uefi/init.s) kiest bij boot de eerste kandidaat die volgens
// de firmware vrij is — zo boot één bestand universeel, ongeacht welk RAM
// een bord al bezet heeft.
package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mkkernel: "+format+"\n", args...)
	os.Exit(1)
}

// multiFlag maakt -elf herhaalbaar (alleen zinvol met -pe).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// payload is één platte variant: de image-bytes (t/m memEnd, BSS-nullen
// inbegrepen) plus zijn herkomst.
type payload struct {
	img      []byte
	f        *elf.File
	load     uint64
	entryOff uint64
}

// flatten leest een tamago-ELF en bouwt de platte image vanaf load. Tot en
// met memEnd (niet fileEnd): de BSS moet als expliciete nullen in het
// bestand — een raw image heeft geen loader die p_memsz zeroet, en de
// runtime op DRAM-restanten laten starten gaf de "zwervende dood in
// schedinit" (garbage mutex → semasleep → wilde deref; gemeten 2026-07-09,
// ESR 0x96000004 @ runtime.semasleep). Kost ~170KB extra.
func flatten(path string, load uint64) payload {
	f, err := elf.Open(path)
	if err != nil {
		die("%v", err)
	}

	var fileEnd, memEnd uint64
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Paddr < load+64 {
			die("%s: segment %#x ligt onder load+64 (%#x): geen ruimte voor de Image-header", path, p.Paddr, load+64)
		}
		if end := p.Paddr + p.Filesz - load; end > fileEnd {
			fileEnd = end
		}
		if end := p.Paddr + p.Memsz - load; end > memEnd {
			memEnd = end
		}
	}
	if fileEnd == 0 {
		die("%s: geen PT_LOAD-segmenten", path)
	}
	if f.Entry < load+64 || f.Entry >= load+fileEnd || f.Entry%4 != 0 {
		die("%s: entry %#x ongeldig voor load %#x", path, f.Entry, load)
	}

	img := make([]byte, memEnd)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		if n, err := p.ReadAt(img[p.Paddr-load:p.Paddr-load+p.Filesz], 0); err != nil || uint64(n) != p.Filesz {
			die("%s: segment %#x lezen: %d/%d, %v", path, p.Paddr, n, p.Filesz, err)
		}
	}
	return payload{img: img, f: f, load: load, entryOff: f.Entry - load}
}

// deriveLoad leidt het laadadres af uit de HopOS-linkconventie
// (TEXT = load + 0x10000, eerste segment binnen die eerste 64KB): het
// laagste PT_LOAD-adres afgerond op 64KB.
func deriveLoad(path string) uint64 {
	f, err := elf.Open(path)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	low := ^uint64(0)
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Paddr < low {
			low = p.Paddr
		}
	}
	if low == ^uint64(0) {
		die("%s: geen PT_LOAD-segmenten", path)
	}
	return low &^ 0xFFFF
}

// patch64 schrijft val op de plek van symbool name in de platte image.
func (p *payload) patch64(name string, val uint64) {
	off, size := p.symbol(name)
	if size < 8 {
		die("symbool %s te klein (%d)", name, size)
	}
	binary.LittleEndian.PutUint64(p.img[off:], val)
}

// symbol zoekt een symbool op en geeft (offset-in-image, grootte). De ELF
// mag dus niet met -s gelinkt zijn (HopOS-conventie: alleen -w).
func (p *payload) symbol(name string) (off, size uint64) {
	syms, err := p.f.Symbols()
	if err != nil {
		die("%s: symboltabel: %v (gelinkt met -s?)", name, err)
	}
	for _, s := range syms {
		if s.Name == name {
			return s.Value - p.load, s.Size
		}
	}
	die("symbool %s niet gevonden", name)
	return 0, 0
}

func main() {
	var elfPaths multiFlag
	flag.Var(&elfPaths, "elf", "invoer-ELF (tamago-image); met -pe herhaalbaar: elke ELF is een venster-variant")
	outPath := flag.String("o", "kernel_2712.img", "uitvoerbestand")
	loadAddr := flag.Uint64("load", 0x200000, "laadadres (kernel_address; arm64-default van de Pi-firmware; bij -pe genegeerd — dan uit de ELF afgeleid)")
	raw := flag.Bool("raw", false, "geen arm64 Image-header: alleen de code0-branch, géén ARM\\x64-magic — de firmware behandelt het bestand dan als raw binary en springt blind naar kernel_address (het Circle-recept; boot-meting 2026-07-08: het Image-pad mét magic weigerde onze kernel zonder enig levensteken, het raw-pad is op de Pi 5 bewezen)")
	pe := flag.Bool("pe", false, "verpak als AArch64 PE/COFF UEFI-applicatie (BOOTAA64.EFI) met venster-varianten — zie de package-doc")
	flag.Parse()
	if len(elfPaths) == 0 {
		die("-elf is verplicht")
	}

	if *pe {
		writePE(elfPaths, *outPath)
		return
	}
	if len(elfPaths) > 1 {
		die("meerdere -elf's is alleen zinvol met -pe")
	}

	p := flatten(elfPaths[0], *loadAddr)
	img, memEnd := p.img, uint64(len(p.img))

	// code0: branch naar de entry — in beide modi het eerste instructiewoord.
	binary.LittleEndian.PutUint32(img[0:], 0x14000000|uint32(p.entryOff/4)&0x03FFFFFF) // code0: b entry
	if !*raw {
		// arm64 Image-header (Linux Documentation/arch/arm64/booting.rst):
		// magic "ARM\x64", 4K-pages, LE. Zonder -raw; zie de -raw-flag.
		binary.LittleEndian.PutUint32(img[4:], 0)           // code1
		binary.LittleEndian.PutUint64(img[8:], 0)           // text_offset
		binary.LittleEndian.PutUint64(img[16:], memEnd)     // image_size (incl. BSS)
		binary.LittleEndian.PutUint64(img[24:], 0b010)      // flags: LE, 4K
		binary.LittleEndian.PutUint32(img[56:], 0x644d5241) // magic "ARM\x64"
	}

	if err := os.WriteFile(*outPath, img, 0o644); err != nil {
		die("%v", err)
	}
	fmt.Printf("%s: %d bytes (image_size %#x, entry %#x @ load %#x)\n",
		*outPath, len(img), memEnd, p.f.Entry, p.load)
}

// writePE bouwt de UEFI-applicatie uit één of meer venster-varianten:
// laadadressen afleiden, RamStart per variant patchen, de kandidatentabel
// (uefiSlots) in variant 0 vullen, en alles in één PE verpakken.
func writePE(paths []string, outPath string) {
	var ps []payload
	for _, path := range paths {
		ps = append(ps, flatten(path, deriveLoad(path)))
	}

	// De varianten zijn dezelfde build op een ander linkadres: gelijke
	// groottes en entry-offsets zijn de sanity-check dat er niet per
	// ongeluk verschillende programma's in één PE belanden.
	for _, p := range ps[1:] {
		if len(p.img) != len(ps[0].img) || p.entryOff != ps[0].entryOff {
			die("varianten verschillen (grootte %d vs %d, entry %#x vs %#x) — zelfde build op ander -T vereist",
				len(p.img), len(ps[0].img), p.entryOff, ps[0].entryOff)
		}
	}
	stride := (uint64(len(ps[0].img)) + 0xfff) &^ 0xfff

	// RamStart per variant = zijn eigen linkadres: de bron van waarheid
	// voor de runtime ("waar draai ik") én voor de stub (variant 0 = L1).
	for i := range ps {
		ps[i].patch64("runtime/goos.RamStart", ps[i].load)
	}
	// De kandidatentabel in variant 0: aantal, stride, laadadressen.
	if _, size := ps[0].symbol("hop-os/metal/board/uefi.uefiSlots"); size < uint64(2+len(ps))*8 {
		die("uefiSlots te klein voor %d varianten", len(ps))
	}
	off, _ := ps[0].symbol("hop-os/metal/board/uefi.uefiSlots")
	binary.LittleEndian.PutUint64(ps[0].img[off:], uint64(len(ps)))
	binary.LittleEndian.PutUint64(ps[0].img[off+8:], stride)
	for i, p := range ps {
		binary.LittleEndian.PutUint64(ps[0].img[off+16+uint64(i)*8:], p.load)
	}

	var extras [][]byte
	for _, p := range ps[1:] {
		extras = append(extras, p.img)
	}
	out := wrapPE(ps[0].img, ps[0].f, ps[0].load, ps[0].entryOff, extras, uint32(stride))
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		die("%v", err)
	}
	var loads []string
	for _, p := range ps {
		loads = append(loads, fmt.Sprintf("%#x", p.load))
	}
	fmt.Printf("%s: %d bytes PE/COFF, %d venster-variant(en): %s (entry-RVA %#x)\n",
		outPath, len(out), len(ps), strings.Join(loads, " "), peHeaderSize+ps[0].entryOff)
}
