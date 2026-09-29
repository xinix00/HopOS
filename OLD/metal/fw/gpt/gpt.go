// Package gpt leest een GUID-partitietabel — genoeg ervan om één vraag te
// beantwoorden: welk stuk van deze schijf is niet van iemand anders?
//
// Op elk HopOS-bord tot nu toe was de schijf van ons alleen en was die vraag
// flauw. Op een machine die we DELEN met het besturingssysteem van de eigenaar
// (een Mac mini: macOS op dezelfde SSD, plus Recovery) is het de enige vraag die
// telt. hopfs krijgt daarom een venster (hopfs.NewRange) en dat venster komt
// hiervandaan — uit wat de schijf zélf zegt, niet uit een hardgecodeerd getal
// dat op één machine klopte.
//
// Bewust minimaal: we lezen de header en de entries, en verder niets. Geen
// schrijven, geen reparatie, geen backup-header. Wie de tabel wil veranderen
// doet dat met het gereedschap van zijn eigen OS.
package gpt

import (
	"encoding/binary"
	"fmt"
)

// Part is één partitie: waar hij begint en eindigt (inclusief), plus zijn naam
// voor de mens die het logboek leest.
type Part struct {
	First, Last uint64
	Name        string
}

// Table is wat we van de tabel nodig hebben.
type Table struct {
	FirstUsable, LastUsable uint64
	Parts                   []Part
}

// ReadFunc leest één blok van blockSize bytes op lba. Zo hoeft dit pakket niets
// van NVMe (of van welke schijf dan ook) te weten.
type ReadFunc func(lba uint64, p []byte) error

// Read leest de tabel van LBA 1.
func Read(read ReadFunc, blockSize int) (Table, error) {
	var t Table
	hdr := make([]byte, blockSize)
	if err := read(1, hdr); err != nil {
		return t, fmt.Errorf("gpt: header: %w", err)
	}
	if string(hdr[0:8]) != "EFI PART" {
		return t, fmt.Errorf("gpt: no signature on LBA 1 (%q) — unpartitioned, or a different block size", hdr[0:8])
	}
	t.FirstUsable = binary.LittleEndian.Uint64(hdr[40:])
	t.LastUsable = binary.LittleEndian.Uint64(hdr[48:])
	entryLBA := binary.LittleEndian.Uint64(hdr[72:])
	numEntries := binary.LittleEndian.Uint32(hdr[80:])
	entrySize := binary.LittleEndian.Uint32(hdr[84:])
	if entrySize < 128 || entrySize > uint32(blockSize) {
		return t, fmt.Errorf("gpt: entry size %d does not fit a %d-byte block", entrySize, blockSize)
	}
	perBlock := uint32(blockSize) / entrySize

	buf := make([]byte, blockSize)
	for i := uint32(0); i < numEntries; i++ {
		if i%perBlock == 0 {
			if err := read(entryLBA+uint64(i/perBlock), buf); err != nil {
				return t, fmt.Errorf("gpt: entries: %w", err)
			}
		}
		e := buf[(i%perBlock)*entrySize:]
		if allZero(e[0:16]) {
			continue // lege sleuf: geen partitie
		}
		p := Part{
			First: binary.LittleEndian.Uint64(e[32:]),
			Last:  binary.LittleEndian.Uint64(e[40:]),
			Name:  utf16Name(e[56 : 56+72]),
		}
		if p.Last < p.First {
			return t, fmt.Errorf("gpt: entry %d ends (%d) before it starts (%d)", i, p.Last, p.First)
		}
		t.Parts = append(t.Parts, p)
	}
	return t, nil
}

// LargestGap geeft het grootste aaneengesloten stuk bruikbare schijf waar geen
// partitie op staat: eerste LBA en aantal LBA's. count=0 = er is niets vrij.
//
// Gaten TUSSEN partities tellen mee, en dat is geen detail: wie zijn macOS
// krimpt, krijgt zijn ruimte precies daar — tussen de container en de
// Recovery-partitie die erachter blijft staan (gemeten 30-08 op de M4).
func LargestGap(t Table) (first, count uint64) {
	// Loop de bruikbare regio langs en sla telkens naar het eind van de
	// partitie die de huidige positie overlapt. Geen sortering nodig: bij elke
	// stap zoeken we de partitie die hier begint of overheen ligt.
	pos := t.FirstUsable
	for pos <= t.LastUsable {
		next := t.LastUsable + 1 // waar het gat eindigt als er niets meer komt
		blocked := false
		for _, p := range t.Parts {
			if p.First <= pos && pos <= p.Last {
				pos = p.Last + 1 // we staan ín een partitie: erlangs
				blocked = true
				break
			}
			if p.First > pos && p.First < next {
				next = p.First // dichtstbijzijnde partitie vóór ons
			}
		}
		if blocked {
			continue
		}
		if n := next - pos; n > count {
			first, count = pos, n
		}
		pos = next
	}
	return first, count
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// utf16Name pakt de UTF-16LE-naam uit; alleen ASCII overleeft, want dit gaat
// naar een console die niet meer kan.
func utf16Name(b []byte) string {
	out := make([]byte, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		if c < 0x20 || c > 0x7e {
			c = '?'
		}
		out = append(out, byte(c))
	}
	return string(out)
}
