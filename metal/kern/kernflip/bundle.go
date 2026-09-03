// Package kernflip is de kern-flip (docs/kern-flip.md): HopOS onder zichzelf
// updaten zonder reboot. De vertrekkende kern leent een venster uit de
// app-pool, plaatst daar de nieuwe kern-ELF (zelfde plaatsingsrekensom als een
// app-image), herbaseert hem met de HOPRELO1-tabel uit de bundel-staart
// (mkkernel -elfreloc), legt een handoff-blob klaar en springt erin. De nieuwe
// kern doorloopt zijn volledige boot-pad, alsof de firmware hem daar aflever-
// de — alleen de plek verschilt.
//
// Dit bestand is de pure bundel-kant: parsen en valideren, host-testbaar.
// De uitvoerende kant (Flip) staat in flip_<arch>.go.
package kernflip

import (
	"encoding/binary"
	"fmt"
)

// ABI is de versie van het flip-contract: de staart-vorm, het handoff-blob en
// de chainload-entryconditie samen. De bundel draagt hem (mkkernel -flipabi);
// een mismatch is een geweigerde flip — dan is de gewone reboot-update de weg.
const ABI = 2

// relocMagic spelt "HOPRELO1" (little-endian) — zelfde woord als mkkernel.
const relocMagic = 0x314F4C4552504F48

// Bundle is een gevalideerde flip-bundel: de kern-ELF plus zijn
// relocatietabel (offsets in de platte beeld-ruimte, t.o.v. LinkLoad).
type Bundle struct {
	ELF      []byte // de onaangeroerde kern-ELF (met symboltabel)
	FlipABI  uint32
	LinkLoad uint64 // linkbasis van de payload (TEXT − 0x10000)
	FlatSize uint64 // platte beeldgrootte incl. BSS (memEnd)
	Entry    uint64 // entrypoint, absoluut op LinkLoad
	Relocs   []byte // count × u32 LE offsets, 8-uitgelijnd binnen FlatSize
}

// RelocCount geeft het aantal tabel-entries.
func (b *Bundle) RelocCount() int { return len(b.Relocs) / 4 }

// ParseBundle valideert een bundel-bestand (ELF + HOPRELO1-staart). Elke
// afwijking is een fout: een bundel is compleet geldig of bestaat niet — de
// bytes komen van het net en dit pad springt er straks in.
func ParseBundle(b []byte) (*Bundle, error) {
	const (
		hdrLen = 56
		ftrLen = 16
	)
	if len(b) < hdrLen+ftrLen+64 {
		return nil, fmt.Errorf("bundel te klein (%d bytes)", len(b))
	}
	// Footer: laatste 16 bytes = {headerOff, magic}.
	ftr := b[len(b)-ftrLen:]
	if binary.LittleEndian.Uint64(ftr[8:]) != relocMagic {
		return nil, fmt.Errorf("geen HOPRELO1-staart — is dit een kale ELF i.p.v. een flip-bundel (mkkernel -elfreloc)?")
	}
	hdrOff := binary.LittleEndian.Uint64(ftr[:8])
	if hdrOff%8 != 0 || hdrOff+hdrLen > uint64(len(b)-ftrLen) {
		return nil, fmt.Errorf("staart-header buiten het bestand (%#x)", hdrOff)
	}
	hdr := b[hdrOff : hdrOff+hdrLen]
	if binary.LittleEndian.Uint64(hdr[0:]) != relocMagic {
		return nil, fmt.Errorf("staart-magic klopt niet op %#x", hdrOff)
	}
	if v := binary.LittleEndian.Uint32(hdr[8:]); v != 1 {
		return nil, fmt.Errorf("staart-versie %d, deze kern kent 1", v)
	}
	bun := &Bundle{
		FlipABI:  binary.LittleEndian.Uint32(hdr[12:]),
		LinkLoad: binary.LittleEndian.Uint64(hdr[24:]),
		FlatSize: binary.LittleEndian.Uint64(hdr[32:]),
		Entry:    binary.LittleEndian.Uint64(hdr[40:]),
	}
	elfSize := binary.LittleEndian.Uint64(hdr[16:])
	count := binary.LittleEndian.Uint64(hdr[48:])
	if elfSize == 0 || elfSize > hdrOff {
		return nil, fmt.Errorf("elfSize %d buiten de bundel (staart op %#x)", elfSize, hdrOff)
	}
	if count > (uint64(len(b)-ftrLen)-hdrOff-hdrLen)/4 {
		return nil, fmt.Errorf("relocatietabel (%d entries) buiten het bestand", count)
	}
	bun.ELF = b[:elfSize]
	bun.Relocs = b[hdrOff+hdrLen : hdrOff+hdrLen+count*4]

	if bun.FlatSize == 0 || bun.FlatSize > 1<<32 {
		return nil, fmt.Errorf("platte beeldgrootte %#x is onzin", bun.FlatSize)
	}
	if bun.LinkLoad%0x10000 != 0 {
		return nil, fmt.Errorf("linkbasis %#x niet 64KB-uitgelijnd", bun.LinkLoad)
	}
	if bun.Entry < bun.LinkLoad+64 || bun.Entry >= bun.LinkLoad+bun.FlatSize || bun.Entry%4 != 0 {
		return nil, fmt.Errorf("entry %#x buiten de payload (%#x+%#x)", bun.Entry, bun.LinkLoad, bun.FlatSize)
	}
	for i := 0; i < int(count); i++ {
		off := binary.LittleEndian.Uint32(bun.Relocs[i*4:])
		if off%8 != 0 || uint64(off)+8 > bun.FlatSize {
			return nil, fmt.Errorf("reloc-offset %#x buiten het platte beeld (%#x)", off, bun.FlatSize)
		}
	}
	return bun, nil
}
