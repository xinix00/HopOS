package acpi

import "testing"

// putU64/putU32: little-endian schrijven in de synthetische tabel.
func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
func putU32(b []byte, v uint32) {
	for i := 0; i < 4; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

// subspace bouwt één type-2-subspace (62 bytes) met herkenbare velden.
func subspace(typ uint8, shmem, doorbell, preserve, write uint64, lat uint32) []byte {
	e := make([]byte, 62)
	e[0], e[1] = typ, 62
	putU64(e[8:], shmem)
	putU64(e[16:], 0x100) // shmem-lengte
	e[24], e[25] = 0, 32  // GAS: SystemMemory, 32-bit register
	putU64(e[28:], doorbell)
	putU64(e[36:], preserve)
	putU64(e[44:], write)
	putU32(e[52:], lat)
	return e
}

// TestPCCFrom bewijst de subspace-walk: ordinale nummering (het DSDT-
// "pcc-channel"-contract), de veld-offsets uit de ACPI-spec, en nette
// afwijzing van ontbrekende indexen en extended types.
func TestPCCFrom(t *testing.T) {
	tbl := make([]byte, 48) // SDT-header(36) + flags(4) + reserved(8)
	tbl = append(tbl, subspace(2, 0x88600000, 0x100000540010, ^uint64(1), 1, 500)...)
	tbl = append(tbl, subspace(1, 0x88601000, 0x100000540020, 0, 0x53, 100)...)
	tbl = append(tbl, subspace(3, 0xdead, 0xbeef, 0, 0, 0)...) // extended: overslaan

	ss, ok := pccFrom(tbl, 0)
	if !ok || ss.ShmemBase != 0x88600000 || ss.Doorbell != 0x100000540010 ||
		ss.Preserve != ^uint64(1) || ss.Write != 1 || ss.LatencyUS != 500 || ss.DBWidth != 32 {
		t.Fatalf("subspace 0 verkeerd geparsed: %+v (ok=%v)", ss, ok)
	}
	if ss, ok := pccFrom(tbl, 1); !ok || ss.ShmemBase != 0x88601000 || ss.Write != 0x53 {
		t.Fatalf("subspace 1 verkeerd geparsed: %+v (ok=%v)", ss, ok)
	}
	if _, ok := pccFrom(tbl, 2); ok {
		t.Fatal("extended type (3) moet ok=false geven")
	}
	if _, ok := pccFrom(tbl, 9); ok {
		t.Fatal("niet-bestaande index moet ok=false geven")
	}
	if _, ok := pccFrom(nil, 0); ok {
		t.Fatal("ontbrekende tabel moet ok=false geven")
	}
}
