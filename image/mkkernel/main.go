// mkkernel zet een tamago-ELF om naar het raw arm64 "Image"-formaat dat de
// Raspberry Pi-firmware (en elke Linux-capabele bootloader) laadt: platte
// bytes op het laadadres, met de 64-byte arm64 Image-header vooraan en een
// branch naar het ELF-entrypoint in het eerste instructiewoord.
//
// Gebruik: go run ./image/mkkernel -elf metal/out/hopos5.elf -o kernel8.img -load 0x80000 -raw
//
// De ELF moet zó gelinkt zijn dat alle PT_LOAD-segmenten op of boven
// load+64 liggen (de header-ruimte); voor HopOS-images geldt
// TEXT_START = load + 0x10000.
//
// Met -pe ontstaat een UEFI-applicatie (BOOTAA64.EFI, zie pe.go) en is -elf
// HERHAALBAAR: elke ELF is dezelfde build gelinkt op een eigen
// kandidaat-venster (load = TEXT − 0x10000, uit de ELF afgeleid). Verpakt
// wordt één payload + relocatietabel (docs/pe-relocatie.md); de extra
// varianten dienen als diff-bewijs. De stub (metal/board/uefi/init.s) kiest
// bij boot de eerste kandidaat die volgens de firmware vrij is — zo boot één
// bestand universeel, ongeacht welk RAM een bord al bezet heeft.
package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
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
	loadAddr := flag.Uint64("load", 0x80000, "laadadres (de Pi-bootloader laadt raw images altijd op 0x80000; bij -pe genegeerd — dan uit de ELF afgeleid)")
	raw := flag.Bool("raw", false, "geen arm64 Image-header: alleen de code0-branch, géén ARM\\x64-magic — de firmware behandelt het bestand dan als raw binary en springt blind naar kernel_address (het Circle-recept; boot-meting 2026-07-08: het Image-pad mét magic weigerde onze kernel zonder enig levensteken, het raw-pad is op de Pi 5 bewezen)")
	pe := flag.Bool("pe", false, "verpak als AArch64 PE/COFF UEFI-applicatie (BOOTAA64.EFI): één payload + relocatietabel, de overige -elf-varianten als diff-bewijs (vereist -ldflags -buildid= op elke variant; zie docs/pe-relocatie.md)")
	flag.Parse()
	if len(elfPaths) == 0 {
		die("-elf is verplicht")
	}

	if *pe {
		writePEReloc(elfPaths, *outPath)
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

// writePEReloc bouwt de UEFI-applicatie uit ÉÉN payload + een relocatietabel
// (docs/pe-relocatie.md): de varianten zijn dezelfde build op verschillende
// linkadressen en verschillen dus uitsluitend in 8-byte-woorden die een
// absoluut adres dragen — die verschillen exact de linkbasis-delta. De diff
// levert de tabel én bewijst de aanname per build: één woord dat niet zuiver
// +delta is (of een staart-/groottemismatch) is een gebroken toolchain-
// aanname en faalt HARD — onderzoek dan (docs/pe-relocatie.md). De stub
// (init.s) kopieert de payload naar het gekozen venster en telt bij elk
// tabel-woord (venster − linkbasis) op. Winst: 6×12,3MB → 1×12,3MB + ~200KB,
// en extra kandidaten zijn voortaan 8 bytes i.p.v. een hele variant.
func writePEReloc(paths []string, outPath string) {
	if len(paths) < 2 {
		die("-pe vergt minstens 2 varianten: één payload + minstens één schaduw voor het diff-bewijs")
	}
	var ps []payload
	for _, path := range paths {
		ps = append(ps, flatten(path, deriveLoad(path)))
	}
	for _, p := range ps[1:] {
		if len(p.img) != len(ps[0].img) || p.entryOff != ps[0].entryOff {
			die("varianten verschillen (grootte %d vs %d, entry %#x vs %#x) — zelfde build op ander -T vereist",
				len(p.img), len(ps[0].img), p.entryOff, ps[0].entryOff)
		}
	}

	// De diff over de MAAGDELIJKE images (vóór elke patch): elk afwijkend
	// woord moet in élke variant exact de eigen linkbasis-delta dragen.
	img0 := ps[0].img
	n := len(img0) / 8
	var relocs []uint32
	for k := 0; k < n; k++ {
		off := k * 8
		w0 := binary.LittleEndian.Uint64(img0[off:])
		same := true
		for _, p := range ps[1:] {
			if binary.LittleEndian.Uint64(p.img[off:]) != w0 {
				same = false
				break
			}
		}
		if same {
			continue
		}
		for _, p := range ps[1:] {
			want := w0 + (p.load - ps[0].load) // uint64-wrap is de bedoeling
			if got := binary.LittleEndian.Uint64(p.img[off:]); got != want {
				die("reloc-diff @ %#x: %#x vs %#x is geen zuivere linkbasis-delta (%#x) — gebroken aanname (-buildid= vergeten? toolchain gewijzigd?); onderzoek (docs/pe-relocatie.md)",
					off, w0, got, p.load-ps[0].load)
			}
		}
		relocs = append(relocs, uint32(off))
	}
	for _, p := range ps[1:] {
		if !bytes.Equal(img0[n*8:], p.img[n*8:]) {
			die("staartbytes (niet-woord-uitgelijnd) verschillen tussen varianten")
		}
	}

	// RamStart: in de klassieke modus per variant het eigen linkadres; hier
	// waarde T0 in de payload + een tabel-entry, zodat de stub-relocatie er
	// vanzelf het gekozen venster van maakt. (Maagdelijk is hij 0 in élke
	// variant en dus nooit al in de tabel.)
	ps[0].patch64("runtime/goos.RamStart", ps[0].load)
	rsOff, _ := ps[0].symbol("runtime/goos.RamStart")
	if rsOff%8 != 0 {
		die("RamStart-offset %#x niet 8-uitgelijnd", rsOff)
	}
	relocs = append(relocs, uint32(rsOff))
	sort.Slice(relocs, func(i, j int) bool { return relocs[i] < relocs[j] })

	// Kandidatentabel: stride 0 — de stub-bron (payload + k×stride) is dan
	// voor élke kandidaat de ene payload. De bases blijven absoluut (ze
	// beschrijven vensters, geen payload-woorden) en staan bewust NIET in de
	// reloc-tabel.
	if _, size := ps[0].symbol("hop-os/metal/board/uefi.uefiSlots"); size < uint64(2+len(ps))*8 {
		die("uefiSlots te klein voor %d varianten", len(ps))
	}
	off, _ := ps[0].symbol("hop-os/metal/board/uefi.uefiSlots")
	binary.LittleEndian.PutUint64(ps[0].img[off:], uint64(len(ps)))
	binary.LittleEndian.PutUint64(ps[0].img[off+8:], 0) // stride 0 = reloc-modus
	for i, p := range ps {
		binary.LittleEndian.PutUint64(ps[0].img[off+16+uint64(i)*8:], p.load)
	}

	// De reloc-descriptor + de tabel zelf, pagina-rond ná de payload.
	tabOff := (uint64(len(img0)) + 0xfff) &^ 0xfff
	roff, rsize := ps[0].symbol("hop-os/metal/board/uefi.uefiReloc")
	if rsize < 16 {
		die("uefiReloc te klein (%d)", rsize)
	}
	binary.LittleEndian.PutUint64(ps[0].img[roff:], tabOff)
	binary.LittleEndian.PutUint64(ps[0].img[roff+8:], uint64(len(relocs)))
	tab := make([]byte, len(relocs)*4)
	for i, r := range relocs {
		binary.LittleEndian.PutUint32(tab[i*4:], r)
	}

	out := wrapPE(ps[0].img, ps[0].f, ps[0].load, ps[0].entryOff, tab, uint32(tabOff))
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		die("%v", err)
	}
	var loads []string
	for _, p := range ps {
		loads = append(loads, fmt.Sprintf("%#x", p.load))
	}
	fmt.Printf("%s: %d bytes PE/COFF (reloc: 1 payload, %d entries), %d venster-kandidaten: %s\n",
		outPath, len(out), len(relocs), len(ps), strings.Join(loads, " "))
}
