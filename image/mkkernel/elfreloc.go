// -elfreloc: de kern-flip-bundel (docs/kern-flip.md) — de kern-ELF zoals de
// linker hem maakte, met daarachter een HOPRELO1-staart die de relocatietabel
// draagt. Het artifact blijft dus gewoon een ELF (zelfde download- en
// plaatsingsweg als een app-image); de staart is wat een app niet nodig heeft
// en de kern wél: apps draaien achter een MMU-kooi die het linkadres
// vertaalt, de kern draait bare op identity en moet dus per woord herbaseerd
// worden naar het venster dat de vertrekkende kern uit de pool leent.
//
// De tabel komt uit dezelfde dubbel-build-diff als -pe (docs/archief/
// pe-relocatie.md): elke -elf is dezelfde build op een eigen -T, en elk
// 8-byte-woord dat verschilt moet exact de linkbasis-delta dragen — één woord
// dat dat niet doet is een gebroken toolchain-aanname en faalt HARD. De
// eerste -elf is de payload (canoniek linkadres, dus óók los bruikbaar als
// gewone kern); de rest is diff-bewijs.
//
// Staart-vorm (8-uitgelijnd achter de ELF-bytes, offsets in de platte
// beeld-ruimte t.o.v. linkLoad):
//
//	header:  magic "HOPRELO1" | u32 versie | u32 flipABI | u64 elfSize |
//	         u64 linkLoad | u64 flatSize | u64 entry | u64 count | count×u32
//	footer (laatste 16B van het bestand): u64 headerOff | magic
//
// De parser (kern/kernflip) vindt de staart via de footer, zodat de ELF-bytes
// onaangeroerd vooraan liggen. De node controleert het HELE bestand tegen de
// sha256 die in zijn platform-config staat (hopos.flip.sha256) —
// image/flip-bundle.sh print die som na het bouwen.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// relocMagic spelt "HOPRELO1" little-endian; kern/kernflip leest hetzelfde woord.
const relocMagic = 0x314F4C4552504F48

// writeELFReloc bouwt de flip-bundel uit ≥2 ELF-varianten. flipABI is de
// versie van het chainload/handoff-contract (kern/kernflip.ABI) — de
// ontvangende kern weigert een bundel met een andere versie.
func writeELFReloc(paths []string, outPath string, flipABI uint32) {
	if len(paths) < 2 {
		die("-elfreloc vergt minstens 2 varianten: één payload + minstens één schaduw voor het diff-bewijs")
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
	relocs := diffRelocs(ps)

	// De symbolen die kernflip bij plaatsing patcht moeten bestaan, en de
	// symboltabel dus aan boord zijn (-w zonder -s): hier falen is een
	// buildfout op de Mac, ontbreken op de node is een geweigerde flip.
	for _, naam := range []string{"runtime/goos.RamStart", "runtime/goos.RamSize"} {
		if _, size := ps[0].symbol(naam); size < 8 {
			die("symbool %s te klein voor de flip-patch", naam)
		}
	}

	elf, err := os.ReadFile(paths[0])
	if err != nil {
		die("%v", err)
	}
	headerOff := (uint64(len(elf)) + 7) &^ 7

	out := make([]byte, headerOff, headerOff+56+uint64(len(relocs))*4+16)
	copy(out, elf)
	var hdr [56]byte
	binary.LittleEndian.PutUint64(hdr[0:], relocMagic)
	binary.LittleEndian.PutUint32(hdr[8:], 1) // staart-versie
	binary.LittleEndian.PutUint32(hdr[12:], flipABI)
	binary.LittleEndian.PutUint64(hdr[16:], uint64(len(elf)))
	binary.LittleEndian.PutUint64(hdr[24:], ps[0].load)
	binary.LittleEndian.PutUint64(hdr[32:], uint64(len(ps[0].img)))
	binary.LittleEndian.PutUint64(hdr[40:], ps[0].load+ps[0].entryOff)
	binary.LittleEndian.PutUint64(hdr[48:], uint64(len(relocs)))
	out = append(out, hdr[:]...)
	for _, r := range relocs {
		out = binary.LittleEndian.AppendUint32(out, r)
	}
	if pad := (8 - uint64(len(out))&7) & 7; pad != 0 {
		out = append(out, make([]byte, pad)...)
	}
	out = binary.LittleEndian.AppendUint64(out, headerOff)
	out = binary.LittleEndian.AppendUint64(out, relocMagic)

	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		die("%v", err)
	}
	fmt.Printf("%s: %d bytes flip-bundel (ELF %d + %d relocs), payload @ %#x entry %#x, flip-ABI %d\n",
		outPath, len(out), len(elf), len(relocs), ps[0].load, ps[0].load+ps[0].entryOff, flipABI)
}

// diffRelocs is de diff over de MAAGDELIJKE platte images: elk afwijkend
// 8-byte-woord moet in élke variant exact de eigen linkbasis-delta dragen —
// gedeeld tussen -pe (writePEReloc) en -elfreloc.
func diffRelocs(ps []payload) []uint32 {
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
				die("reloc-diff @ %#x: %#x vs %#x is geen zuivere linkbasis-delta (%#x) — gebroken aanname (-buildid= vergeten? toolchain gewijzigd?); onderzoek (docs/archief/pe-relocatie.md)",
					off, w0, got, p.load-ps[0].load)
			}
		}
		relocs = append(relocs, uint32(off))
	}
	for _, p := range ps[1:] {
		if string(img0[n*8:]) != string(p.img[n*8:]) {
			die("staartbytes (niet-woord-uitgelijnd) verschillen tussen varianten")
		}
	}
	return relocs
}
