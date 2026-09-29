package cage

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/cpu/thead"
)

// De tweede helft van een kooi: verplaatsen.
//
// Een kooi doet twee dingen — begrenzen en verplaatsen — en beide architecturen
// doen precies dat, alleen met ander silicium:
//
//	                begrenzen              verplaatsen
//	ARM             stage-2-tabel          dezelfde stage-2-tabel
//	RISC-V (C906)   PMP-whitelist (cage.go)  deze tabel
//
// Op ARM valt het samen: één tabel doet allebei, en daarom stond `cageRelocates`
// daar altijd op true. Hier zijn het twee mechanismen, en dat is de enige reden
// dat het hier in twee bestanden staat.
//
// Waarvoor verplaatsen dient, en het is één ding: **één artifact per
// architectuur**. Zonder verplaatsing draait een app op het fysieke adres waarop
// hij gelinkt is, dus zou elk slot zijn eigen build nodig hebben — en kan er in
// de praktijk maar één slot bestaan. Mét verplaatsing ziet élk slot zichzelf op
// hetzelfde linkadres en vertaalt de kooi dat naar de partitie die het slot
// werkelijk kreeg. Exact wat de stage-2-map op ARM al doet.
//
// Wat verplaatsen NIET is: de invariant. Die blijft de whitelist (cage.go). Dat
// onderscheid is hier scherper dan op ARM, want een app draait in de laag ónder
// HOP en mag daar zijn eigen map-register schrijven — hij kan zijn adresruimte
// dus hertekenen. Dat is veilig omdat de hardware-walker zelf aan de whitelist
// onderworpen is: hertekenen bereikt nooit iets buiten de kooi, en schaadt dus
// alleen de app zelf.
//
// Gevolg daarvan: de tabel mág ín de partitie staan, waar de app hem kan
// overschrijven. Dat moet zelfs — een tabel buiten de kooi zou de walk laten
// faulten.

const (
	// PageSize/BlockSize zijn de twee korrels die deze tabel gebruikt. Een blok
	// van 2MB per entry houdt de tabel op twee pagina's in plaats van
	// duizenden; een partitie is tientallen MB's, dus fijner mappen levert niets
	// en kost alleen tabel. Daarom moet een partitie op BlockSize uitgelijnd
	// zijn — Relocate weigert het anders in plaats van stil een halve pagina te
	// mappen.
	PageSize  = 4096
	BlockSize = 2 << 20

	// Entries per tabelniveau: 512 × 8 bytes = precies één pagina.
	mapEntries = 512

	// Vlaggen van één tabel-entry (RISC-V privileged spec, tabel 4.4).
	entValid = 1 << 0
	entRead  = 1 << 1
	entWrite = 1 << 2
	entExec  = 1 << 3
	// De U-bit (1<<4) staat hier niet: een app draait in supervisor-modus, niet in
	// user-modus, dus geen enkele slot-PTE hoort user-bereikbaar te zijn.
	// De G-bit (1<<5) evenmin, en dat is hier geen detail: G belooft de hardware
	// dat de mapping in ÉLKE adresruimte hetzelfde betekent, waarna hij haar over
	// een satp-wissel mag laten staan. Elk slot mapt exact het tegendeel —
	// hetzelfde linkadres naar een ándere partitie — dus is G op een slot-PTE
	// volgens de privileged spec een softwarefout. Er is geen venster dat hem
	// verdient: app-RAM, ABI-staart en granted MMIO zijn alle drie per slot.
	entSeen  = 1 << 6 // "accessed"
	entDirty = 1 << 7

	// De cache-attributen komen uit het CPU-PROFIEL (cpu/thead), niet uit de
	// RISC-V-spec: bit 61/62 bestaan daar niet. Dit bestand kent het Sv39-formaat,
	// het profiel kent het silicium.
	entBuf   = thead.PTEBufferable
	entCache = thead.PTECacheable

	// Seen/Dirty vooraf zetten: veel implementaties (de C906 hoort erbij) faulten
	// liever dan dat ze die bits zelf bijwerken, en HopOS heeft geen
	// pagina-vervanging waarvoor ze zouden dienen. Zo kan er nooit een fault zijn
	// op geheugen dat de app gewoon mag hebben.
	entTouched = entSeen | entDirty

	// mapMode is de modus-waarde bovenin het map-register (Sv39: drie niveaus,
	// 39 adresbits).
	mapMode = 8 << 60

	// Adresbits die deze modus dekt. Een linkadres daarbuiten is een bug in het
	// PA-plan van een board, niet iets om stil af te kappen.
	mapBits = 39
)

// MapWindow is één verplaatsing: waar de app het ziet (Link) → waar het echt
// staat (Phys). Dezelfde woorden als de rest van de kooi-naad gebruikt
// (linkBaseFor, linkBase in kern/slots) — bewust niet "virtueel/fysiek", want
// dat is arch-jargon voor hetzelfde idee.
type MapWindow struct {
	Link, Phys, Size uint64
	R, W, X          bool

	// Device: dit venster is MMIO, geen RAM. Dan blijven de cache-attributen uit
	// (zie entCache/entBuf) — hetzelfde onderscheid dat ARM met device- versus
	// normal-memory maakt.
	Device bool
}

// MapPlan is de adresruimte van één slot.
type MapPlan struct {
	// TableBase is waar de tabel zelf komt te staan (fysiek, PageSize-uitgelijnd).
	// Er is één pagina voor de wortel plus één per gemapte gigabyte.
	TableBase uint64
	Windows   []MapWindow
}

// Reloc is de uitkomst: de bytes die HOP op TableBase zet, en de waarde die de
// arch-entry in zijn map-register schrijft vóór hij de app binnenlaat — satp op
// RISC-V, VTTBR_EL2 op ARM. Eén begrip, twee registers.
type Reloc struct {
	Bytes []byte
	Root  uint64
}

// Relocate rekent de tabel uit. Alle validatie zit hier, in Go, op de host
// getest — dezelfde arbeidsdeling als Encode: de arch-entry schrijft alleen een
// register weg en neemt geen enkele beslissing.
func Relocate(p MapPlan) (*Reloc, error) {
	if p.TableBase%PageSize != 0 {
		return nil, fmt.Errorf("cage: tabelbasis %#x niet op %d uitgelijnd", p.TableBase, PageSize)
	}
	if len(p.Windows) == 0 {
		return nil, fmt.Errorf("cage: leeg map-plan — een slot zonder mapping kan niet draaien")
	}

	root := make([]uint64, mapEntries)
	var mid [][]uint64     // de niveaus onder de wortel, in aanmaakvolgorde
	at := map[uint64]int{} // giga-index → positie in mid

	for i, w := range p.Windows {
		if w.Size == 0 {
			return nil, fmt.Errorf("cage: map window %d has zero length", i)
		}
		if w.Link%BlockSize != 0 || w.Phys%BlockSize != 0 || w.Size%BlockSize != 0 {
			return nil, fmt.Errorf("cage: map window %d (link %#x phys %#x len %#x) not %dMB aligned",
				i, w.Link, w.Phys, w.Size, BlockSize>>20)
		}
		if !w.R && !w.W && !w.X {
			return nil, fmt.Errorf("cage: map window %d has no permissions", i)
		}
		// Schrijven zonder lezen is een gereserveerde combinatie: de hardware mág
		// zo'n entry weigeren. Liever hier hard falen dan een tabel die op één
		// silicium werkt en op het volgende faultt.
		if w.W && !w.R {
			return nil, fmt.Errorf("cage: map window %d is W without R — reserved encoding", i)
		}

		flags := uint64(entValid | entTouched)
		if !w.Device {
			flags |= entBuf | entCache
		}
		if w.R {
			flags |= entRead
		}
		if w.W {
			flags |= entWrite
		}
		if w.X {
			flags |= entExec
		}

		for off := uint64(0); off < w.Size; off += BlockSize {
			link, phys := w.Link+off, w.Phys+off
			if link>>mapBits != 0 {
				return nil, fmt.Errorf("cage: link address %#x is outside the %d address bits of this mode",
					link, mapBits)
			}
			gi := (link >> 30) & (mapEntries - 1)
			bi := (link >> 21) & (mapEntries - 1)

			pos, ok := at[gi]
			if !ok {
				mid = append(mid, make([]uint64, mapEntries))
				pos = len(mid) - 1
				at[gi] = pos
				// Een verwijzende entry heeft ALLEEN Valid: geen R/W/X, want dat
				// zou hem juist tot blad maken.
				next := p.TableBase + uint64(1+pos)*PageSize
				root[gi] = (next>>12)<<10 | entValid
			}
			if mid[pos][bi] != 0 {
				return nil, fmt.Errorf("cage: link address %#x is already mapped — overlapping windows", link)
			}
			mid[pos][bi] = (phys>>12)<<10 | flags
		}
	}

	// Uitschrijven: wortel eerst, dan de niveaus in aanmaakvolgorde — precies de
	// volgorde die de verwijzende entries hierboven aannemen.
	out := make([]byte, (1+len(mid))*PageSize)
	put := func(page int, t []uint64) {
		for i, v := range t {
			o := page*PageSize + i*8
			for b := range 8 {
				out[o+b] = byte(v >> (8 * b))
			}
		}
	}
	put(0, root)
	for i, t := range mid {
		put(1+i, t)
	}

	return &Reloc{Bytes: out, Root: mapMode | (p.TableBase >> 12)}, nil
}
