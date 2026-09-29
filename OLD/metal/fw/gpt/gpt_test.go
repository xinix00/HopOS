package gpt

import (
	"encoding/binary"
	"testing"
)

// De echte tabel van de Mac mini M4, zoals de node hem 30-08 uitlas: drie
// partities met het vrijgemaakte gat ertussen. Dit is de tabel waar het om
// begonnen is, dus die hoort de test te zijn.
func m4Disk() (ReadFunc, int) {
	const bs = 4096
	hdr := make([]byte, bs)
	copy(hdr, "EFI PART")
	binary.LittleEndian.PutUint64(hdr[40:], 6)           // eerste bruikbare
	binary.LittleEndian.PutUint64(hdr[48:], 122_138_127) // laatste bruikbare
	binary.LittleEndian.PutUint64(hdr[72:], 2)           // entries op LBA 2
	binary.LittleEndian.PutUint32(hdr[80:], 128)
	binary.LittleEndian.PutUint32(hdr[84:], 128)

	entries := make([]byte, bs)
	put := func(i int, first, last uint64, name string) {
		e := entries[i*128:]
		e[0] = 1 // niet-lege type-GUID
		binary.LittleEndian.PutUint64(e[32:], first)
		binary.LittleEndian.PutUint64(e[40:], last)
		for j, r := range name {
			binary.LittleEndian.PutUint16(e[56+j*2:], uint16(r))
		}
	}
	put(0, 6, 128_005, "iBootSystemContainer")
	put(1, 128_006, 19_659_255, "")
	put(2, 120_827_419, 122_138_127, "RecoveryOSContainer")

	return func(lba uint64, p []byte) error {
		switch lba {
		case 1:
			copy(p, hdr)
		case 2:
			copy(p, entries)
		default:
			for i := range p {
				p[i] = 0
			}
		}
		return nil
	}, bs
}

func TestGatTussenPartitiesWordtGevonden(t *testing.T) {
	read, bs := m4Disk()
	tbl, err := Read(read, bs)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Parts) != 3 {
		t.Fatalf("%d partities gelezen, wil 3", len(tbl.Parts))
	}
	first, count := LargestGap(tbl)
	// Het gat ligt TUSSEN de gekrompen macOS-container en RecoveryOS — niet
	// achteraan. Een zoeker die alleen achter de laatste partitie kijkt, vindt
	// hier nul en dat was precies de val.
	if want := uint64(19_659_256); first != want {
		t.Fatalf("gat begint op %d, wil %d", first, want)
	}
	if want := uint64(120_827_419 - 19_659_256); count != want {
		t.Fatalf("gat is %d blokken, wil %d", count, want)
	}
	if gb := count * 4096 / 1e9; gb < 400 || gb > 420 {
		t.Fatalf("gat is %d GB, verwacht ~414", gb)
	}
}

func TestVolleSchijfGeeftGeenGat(t *testing.T) {
	const bs = 4096
	hdr := make([]byte, bs)
	copy(hdr, "EFI PART")
	binary.LittleEndian.PutUint64(hdr[40:], 6)
	binary.LittleEndian.PutUint64(hdr[48:], 1000)
	binary.LittleEndian.PutUint64(hdr[72:], 2)
	binary.LittleEndian.PutUint32(hdr[80:], 128)
	binary.LittleEndian.PutUint32(hdr[84:], 128)
	entries := make([]byte, bs)
	entries[0] = 1
	binary.LittleEndian.PutUint64(entries[32:], 6)
	binary.LittleEndian.PutUint64(entries[40:], 1000)

	read := ReadFunc(func(lba uint64, p []byte) error {
		switch lba {
		case 1:
			copy(p, hdr)
		case 2:
			copy(p, entries)
		}
		return nil
	})
	tbl, err := Read(read, bs)
	if err != nil {
		t.Fatal(err)
	}
	if _, count := LargestGap(tbl); count != 0 {
		t.Fatalf("volle schijf gaf %d vrije blokken, wil 0", count)
	}
}

func TestGeenGPTIsEenFoutEnGeenGok(t *testing.T) {
	read := ReadFunc(func(lba uint64, p []byte) error { return nil })
	if _, err := Read(read, 4096); err == nil {
		t.Fatal("een schijf zonder GPT hoort een fout te geven, geen lege tabel")
	}
}
