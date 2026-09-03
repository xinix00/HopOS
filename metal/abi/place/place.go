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
	"encoding/binary"
	"fmt"
	"io"

	"github.com/xinix00/lean/leanelf"
)

// De gepatchte symbolen — het contract met tamago's runtime (RamStart/
// RamSize; vereist -w zónder -s: de symboltabel moet aan boord blijven) en
// het optionele slot-hint-symbool (hopslot- en oudere uefi-app-images).
const (
	SymRAMStart    = "runtime/goos.RamStart"
	SymRAMSize     = "runtime/goos.RamSize"
	SymSlotHint    = "github.com/xinix00/HopOS/metal/v2/board/uefi.slotHint"
	SymSlotHintGen = "github.com/xinix00/HopOS/metal/v2/board/hopslot.slotHint"

	// SymABI draagt de versie van de slot-ABI: de indeling van de partitie-staart
	// (control page, hop-ABI-ringen, frame-ringen) waar een app zijn adressen uit
	// rekent. Build leest de wáárde uit het image en vergelijkt hem met de versie
	// die de aanroeper meegeeft. Een app die op het verkeerde adres zijn control
	// page zoekt is anders een stille misread, en die klasse fouten kost dagen.
	SymABI = "github.com/xinix00/HopOS/metal/v2/app/applib.abiVersion"
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
// dezelfde melden; 0 = geen stempel toetsen (een kern-image, zie onder). Elke
// afwijking is een fout: een plan is compleet geldig of bestaat niet. Bewust géén abi/layout-import (abi-pakketten zijn vlak): alles
// komt als parameter binnen.
func Build(r io.ReaderAt, imgSize int64, linkBase, appRAM, loOff, topOff uint64, slot int, abi uint64) (*Plan, error) {
	f, err := leanelf.Open(r, imgSize)
	if err != nil {
		return nil, fmt.Errorf("elf parse: %w", err)
	}

	if f.Entry < linkBase+loOff || f.Entry >= linkBase+appRAM {
		return nil, fmt.Errorf("entry %#x outside link window %#x+%#x", f.Entry, linkBase, appRAM)
	}
	p := &Plan{Entry: f.Entry}

	for _, ph := range f.Segments {
		if ph.Type != leanelf.PTLoad {
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

	// Vijf namen zoeken, niet de hele tabel bouwen: een app-image draagt
	// tienduizenden symbolen, en dit loopt op de kern.
	syms, err := f.Lookup(SymRAMStart, SymRAMSize, SymABI, SymSlotHint, SymSlotHintGen)
	if err != nil {
		return nil, fmt.Errorf("symbols (image built with -s?): %w", err)
	}
	for _, naam := range []string{SymRAMStart, SymRAMSize} {
		s, ok := syms[naam]
		if !ok {
			return nil, fmt.Errorf("RAM symbol %s not found", naam)
		}
		if s.Value%8 != 0 || s.Value < linkBase || s.Value > linkBase+appRAM-8 {
			return nil, fmt.Errorf("symbol %s (%#x) outside link range", naam, s.Value)
		}
		v := linkBase
		if naam == SymRAMSize {
			v = appRAM
		}
		p.Patches = append(p.Patches, Patch{Addr: s.Value, Val: v})
	}
	// De slot-hint is optioneel en additief: images zonder het symbool merken
	// er niets van, en een vreemde waarde wordt stil overgeslagen (zelfde
	// semantiek als altijd).
	for _, naam := range []string{SymSlotHint, SymSlotHintGen} {
		s, ok := syms[naam]
		if ok && s.Value%8 == 0 && s.Value >= linkBase && s.Value <= linkBase+appRAM-8 {
			p.Patches = append(p.Patches, Patch{Addr: s.Value, Val: uint64(slot)})
		}
	}

	// abi == 0: geen slot-ABI te toetsen. Dat is een KERN-image (kern/kernflip):
	// dezelfde ELF-vorm, dezelfde grenzen, dezelfde RAM-symbolen — alleen geen
	// applib erin, dus ook geen stempel. Een app-image toetst altijd.
	if abi == 0 {
		return p, nil
	}
	s, ok := syms[SymABI]
	if !ok {
		return nil, fmt.Errorf("image predates the versioned slot ABI (no %s) — rebuild it against this HopOS (ABI %d)", SymABI, abi)
	}
	imgABI, err := readU64(f, s.Value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SymABI, err)
	}
	if imgABI != abi {
		return nil, fmt.Errorf("image speaks slot ABI %d, this HopOS speaks %d — rebuild it", imgABI, abi)
	}
	return p, nil
}

// readU64 leest het 64-bit woord dat op adres addr in het image staat. Nodig
// omdat een symboltabel adréssen geeft, geen inhoud — en de ABI-versie is
// inhoud. Welk PT_LOAD dat adres draagt en waar dat in het bestand ligt, weet
// leanelf.
func readU64(f *leanelf.File, addr uint64) (uint64, error) {
	var b [8]byte
	if err := f.ReadAtPaddr(b[:], addr); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}
