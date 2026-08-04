// Package place is de éne bron van waarheid voor het plaatsingsplan van een
// app-image: welke ELF-segmenten waarheen (IPA), welke symbolen gepatcht met
// welke waarden, en álle validatie-invarianten daaromheen. Twee uitvoerders
// delen het plan:
//
//   - HOP's plaatsing (kern/slots): device-side op core 0 — de bootstrap die
//     de apploader plaatst (Start) en het vangnet voor images zonder
//     zelfplaatsing (StartStaged legacy);
//   - de zelfplaatsing (app/applib/selfplace.go): de loader genereert uit
//     het plan het stubje dat op de eigen core de segmenten schuift.
//
// Vóór dit pakket leefde de validatie dubbel (kern/slots én applib) — een
// ABI-kritisch pad hoort niet op twee plekken te kunnen divergeren. Alles
// hier is puur reken- en leeswerk op io.ReaderAt: geen dev-toegang, dus
// host-testbaar.
package place

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
)

// De gepatchte symbolen — het contract met tamago's runtime (RamStart/
// RamSize; vereist -w zónder -s: de symboltabel moet aan boord blijven) en
// het optionele slot-hint-symbool (hopslot- en oudere uefi-app-images).
const (
	SymRAMStart    = "runtime/goos.RamStart"
	SymRAMSize     = "runtime/goos.RamSize"
	SymSlotHint    = "github.com/xinix00/HopOS/metal/board/uefi.slotHint"
	SymSlotHintGen = "github.com/xinix00/HopOS/metal/board/hopslot.slotHint"

	// SymABI draagt de versie van de slot-ABI: de indeling van de partitie-staart
	// (control page, hop-ABI-ringen, frame-ringen) waar een app zijn adressen uit
	// rekent. Build leest de wáárde uit het image en vergelijkt hem met de versie
	// die de aanroeper meegeeft. Een app die op het verkeerde adres zijn control
	// page zoekt is anders een stille misread, en die klasse fouten kost dagen.
	SymABI = "github.com/xinix00/HopOS/metal/app/applib.abiVersion"
)

// Seg is één te plaatsen PT_LOAD: Off in de image → Dst (IPA), Filesz
// kopiëren, de rest tot Memsz nullen (BSS).
type Seg struct {
	Dst, Off, Filesz, Memsz uint64
}

// Patch is één 64-bit symboolwaarde op zijn (IPA-)adres.
type Patch struct {
	Addr, Val uint64
}

// Plan is het gevalideerde plaatsingsplan van één image.
type Plan struct {
	Entry   uint64 // app-entry (IPA, binnen het opgegeven linkvenster)
	Segs    []Seg
	Patches []Patch
}

// Build parseert en valideert een image tegen het linkvenster
// [linkBase, linkBase+appRAM) — het canonieke contract: de aanroeper kent de
// basis (de loader: zijn eigen gepatchte RAMStart; de kern: SlotBase(1)).
// Segmenten moeten bovendien tussen loOff en topOff blijven (offsets vanaf
// linkBase): boven de ruimte die de architectuur vooraan reserveert (RISC-V zet
// daar de kooi-stub die de app binnenlaat; op ARM is loOff nul), en onder de
// onderkant van de staging (kern) of het stub-venster (loader) — de kopieerbron
// moet de kopie overleven. slot is de slotHint-patchwaarde, abi de
// slot-ABI-versie die deze HopOS spreekt (layout.ABIVersion) — het image moet
// dezelfde melden. Elke afwijking is een fout: een plan is compleet geldig of
// bestaat niet. Bewust géén abi/layout-import (abi-pakketten zijn vlak): alles
// komt als parameter binnen.
func Build(r io.ReaderAt, imgSize int64, linkBase, appRAM, loOff, topOff uint64, slot int, abi uint64) (*Plan, error) {
	f, err := elf.NewFile(r)
	if err != nil {
		return nil, fmt.Errorf("elf parse: %w", err)
	}

	if f.Entry < linkBase+loOff || f.Entry >= linkBase+appRAM {
		return nil, fmt.Errorf("entry %#x outside link window %#x+%#x", f.Entry, linkBase, appRAM)
	}
	p := &Plan{Entry: f.Entry}

	for _, ph := range f.Progs {
		if ph.Type != elf.PT_LOAD {
			continue
		}
		// Headervelden zijn input (de image komt van het netwerk):
		// overflow-veilig begrenzen, binnen het linkvenster, binnen de image,
		// en onder het plafond.
		if ph.Filesz > ph.Memsz || ph.Memsz > appRAM ||
			ph.Paddr < linkBase+loOff || ph.Paddr > linkBase+appRAM-ph.Memsz {
			return nil, fmt.Errorf("segment %#x+%#x (file %#x) outside link range %#x+%#x",
				ph.Paddr, ph.Memsz, ph.Filesz, linkBase, appRAM)
		}
		if ph.Off > uint64(imgSize) || ph.Filesz > uint64(imgSize)-ph.Off {
			return nil, fmt.Errorf("segment file-offset %#x+%#x outside image (%d bytes)",
				ph.Off, ph.Filesz, imgSize)
		}
		if ph.Paddr+ph.Memsz > linkBase+topOff {
			return nil, fmt.Errorf("segment %#x+%#x reaches into the staging window (top %#x)",
				ph.Paddr, ph.Memsz, linkBase+topOff)
		}
		p.Segs = append(p.Segs, Seg{Dst: ph.Paddr, Off: ph.Off, Filesz: ph.Filesz, Memsz: ph.Memsz})
	}
	if len(p.Segs) == 0 {
		return nil, fmt.Errorf("no PT_LOAD segments")
	}

	syms, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("symbols (image built with -s?): %w", err)
	}
	ramPatched := 0
	imgABI, abiFound := uint64(0), false
	for _, s := range syms {
		if s.Name == SymABI {
			v, err := readU64(r, f, s.Value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", SymABI, err)
			}
			imgABI, abiFound = v, true
		}
		switch s.Name {
		case SymRAMStart, SymRAMSize:
			if s.Value%8 != 0 || s.Value < linkBase || s.Value > linkBase+appRAM-8 {
				return nil, fmt.Errorf("symbol %s (%#x) outside link range", s.Name, s.Value)
			}
			v := linkBase
			if s.Name == SymRAMSize {
				v = appRAM
			}
			p.Patches = append(p.Patches, Patch{Addr: s.Value, Val: v})
			ramPatched++
		case SymSlotHint, SymSlotHintGen:
			// Optioneel en additief: images zonder het symbool merken er
			// niets van; een vreemde waarde wordt stil overgeslagen (zelfde
			// semantiek als altijd).
			if s.Value%8 == 0 && s.Value >= linkBase && s.Value <= linkBase+appRAM-8 {
				p.Patches = append(p.Patches, Patch{Addr: s.Value, Val: uint64(slot)})
			}
		}
	}
	if ramPatched != 2 {
		return nil, fmt.Errorf("RAM symbols not found (%d/2)", ramPatched)
	}
	if !abiFound {
		return nil, fmt.Errorf("image predates the versioned slot ABI (no %s) — rebuild it against this HopOS (ABI %d)", SymABI, abi)
	}
	if imgABI != abi {
		return nil, fmt.Errorf("image speaks slot ABI %d, this HopOS speaks %d — rebuild it", imgABI, abi)
	}
	return p, nil
}

// readU64 leest het 64-bit woord dat op adres va in het image staat: welk
// PT_LOAD-segment het draagt, en waar dat in het bestand ligt. Nodig omdat een
// symboltabel adréssen geeft, geen inhoud — en de ABI-versie is inhoud.
func readU64(r io.ReaderAt, f *elf.File, va uint64) (uint64, error) {
	for _, ph := range f.Progs {
		if ph.Type != elf.PT_LOAD || va < ph.Paddr || va+8 > ph.Paddr+ph.Filesz {
			continue
		}
		var b [8]byte
		if _, err := r.ReadAt(b[:], int64(ph.Off+va-ph.Paddr)); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(b[:]), nil
	}
	return 0, fmt.Errorf("adres %#x valt in geen enkel geladen segment", va)
}
