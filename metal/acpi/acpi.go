// Package acpi parseert de ACPI-tabellen die UEFI-firmware achterlaat — de
// hardware-beschrijving van servers (Ampere Altra) en van QEMU virt onder
// EDK2. Waar de Pi's een DTB meegeven (metal/fdt), is dit het ACPI-equivalent:
// RSDP → XSDT → MADT (cores), MCFG (PCIe-ECAM), SPCR (console-UART), FADT
// (PSCI-conduit). Alleen lezen, geen AML-interpreter: de statische tabellen
// dekken alles wat HopOS voor discovery nodig heeft.
//
// De tabellen liggen in door de firmware gereserveerd geheugen
// (EfiACPIReclaimMemory) buiten onze RAM-declaratie — tamago mapt dat
// device/ongecached, dus gewone reads werken. Alle multi-byte-velden zijn
// little-endian (ACPI-spec).
package acpi

import (
	"fmt"

	"hop-os/metal/dev"
)

// mem kopieert length bytes op fysiek adres pa naar een eigen buffer. De
// tabellen liggen buiten onze RAM-declaratie en zijn dus device-gemapt
// (nGnRnE): élke access moet uitgelijnd zijn — Go's memmove/string-ops op een
// directe slice geven een alignment fault (gemeten 2026-07-13, EL1 exception
// in slicebytetostring). Dus: uitgelijnde 32-bit reads (4 bytes per read
// i.p.v. 1), daarna parsen op de RAM-kopie. ACPI-tabellen zijn klein (KB's).
func mem(pa uintptr, length int) []byte {
	out := make([]byte, length)
	i := 0
	// Kop tot de eerste 4-byte-grens.
	for i < length && (pa+uintptr(i))&3 != 0 {
		p := pa + uintptr(i)
		out[i] = byte(dev.Read32(p&^3) >> (8 * (p & 3)))
		i++
	}
	// Volle woorden.
	for ; i+4 <= length; i += 4 {
		w := dev.Read32(pa + uintptr(i))
		out[i], out[i+1] = byte(w), byte(w>>8)
		out[i+2], out[i+3] = byte(w>>16), byte(w>>24)
	}
	// Staart.
	for ; i < length; i++ {
		p := pa + uintptr(i)
		out[i] = byte(dev.Read32(p&^3) >> (8 * (p & 3)))
	}
	return out
}

func u16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func u32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func u64(b []byte) uint64 { return uint64(u32(b)) | uint64(u32(b[4:]))<<32 }

// checksum telt alle bytes op — ACPI-checksums moeten op 0 uitkomen.
func checksum(b []byte) byte {
	var s byte
	for _, c := range b {
		s += c
	}
	return s
}

// Tables is het resultaat van Parse: de XSDT-inhoud, geïndexeerd op
// signature. Eén tabel per signature volstaat voor onze doelen (meerdere
// SSDT's bestaan, maar die zijn AML en slaan we over).
type Tables struct {
	Revision int                // RSDP-revisie (≥2 = ACPI 2.0+, XSDT aanwezig)
	OEMID    string             // uit de XSDT-header ("QEMU", "Ampere(R)", ...)
	tables   map[string]uintptr // signature → fysiek adres van de tabelheader
	cache    map[string][]byte  // gedecodeerde tabel per signature (device-reads
	// zijn duur: elke MCFG/MADT werd anders 2× per boot uit device-mem gehaald)
	Sigs []string // alle signatures in XSDT-volgorde (voor de dump)
}

// Parse leest de RSDP (fysiek adres, uit de EFI-configuratietabel) en walkt
// de XSDT. Checksums worden gecontroleerd: een corrupte pointer hier betekent
// dat al het vervolg giswerk is — dan liever meteen een fout.
func Parse(rsdp uintptr) (*Tables, error) {
	if rsdp == 0 {
		return nil, fmt.Errorf("acpi: no RSDP")
	}
	b := mem(rsdp, 36)
	if string(b[0:8]) != "RSD PTR " {
		return nil, fmt.Errorf("acpi: RSDP signature missing at %#x", rsdp)
	}
	if checksum(b[:20]) != 0 {
		return nil, fmt.Errorf("acpi: RSDP checksum failed (ACPI 1.0 part)")
	}
	rev := int(b[15])
	if rev < 2 {
		return nil, fmt.Errorf("acpi: RSDP revision %d — no XSDT (ACPI 1.0 firmware)", rev)
	}
	if checksum(b[:36]) != 0 {
		return nil, fmt.Errorf("acpi: RSDP checksum failed (ACPI 2.0 part)")
	}
	xsdt := uintptr(u64(b[24:]))
	if xsdt == 0 {
		return nil, fmt.Errorf("acpi: XSDT address is 0")
	}

	h := mem(xsdt, 36)
	if string(h[0:4]) != "XSDT" {
		return nil, fmt.Errorf("acpi: XSDT signature missing at %#x", xsdt)
	}
	length := int(u32(h[4:]))
	if length < 36 || length > 1<<22 {
		return nil, fmt.Errorf("acpi: implausible XSDT length %d", length)
	}
	full := mem(xsdt, length)
	if checksum(full) != 0 {
		return nil, fmt.Errorf("acpi: XSDT checksum failed")
	}

	t := &Tables{
		Revision: rev,
		OEMID:    trim(h[10:16]),
		tables:   map[string]uintptr{},
		cache:    map[string][]byte{},
	}
	for off := 36; off+8 <= length; off += 8 {
		pa := uintptr(u64(full[off:]))
		if pa == 0 {
			continue
		}
		sig := string(mem(pa, 4))
		t.Sigs = append(t.Sigs, sig)
		if _, dup := t.tables[sig]; !dup {
			t.tables[sig] = pa
		}
	}
	return t, nil
}

// trim knipt NUL's en spaties van een vaste-breedte ACPI-string.
func trim(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == 0 || b[end-1] == ' ') {
		end--
	}
	return string(b[:end])
}

// table geeft de volledige tabel-bytes voor een signature (checksum
// gecontroleerd), of nil als hij ontbreekt.
func (t *Tables) table(sig string) []byte {
	if b, ok := t.cache[sig]; ok {
		return b // gememoiseerd (nil-resultaat wordt óók gecachet)
	}
	b := t.decode(sig)
	t.cache[sig] = b
	return b
}

func (t *Tables) decode(sig string) []byte {
	pa, ok := t.tables[sig]
	if !ok {
		return nil
	}
	length := int(u32(mem(pa, 8)[4:]))
	// Length is firmware-input: te klein = geen geldige SDT, te groot =
	// een corrupte waarde die make() in een boot-OOM laat lopen (review
	// #11) — beide stil fout laten zijn ("liever meteen een fout" geldt
	// voor Parse; hier is nil het contract voor "onbruikbaar").
	if length < 36 || length > 1<<22 {
		return nil
	}
	b := mem(pa, length)
	if checksum(b) != 0 {
		return nil
	}
	return b
}

// CPU is één GICC-entry uit de MADT: een core zoals de firmware hem kent.
type CPU struct {
	UID     uint32 // ACPI processor UID
	MPIDR   uint64 // affiniteitsroute voor PSCI CPU_ON
	Enabled bool   // GICC-flags bit 0
}

// MADT geeft de cores (GICC-entries) plus het GICD-basisadres (0 = geen
// GICv3-distributor gevonden). Dit is de bron voor "hoeveel cores heeft dit
// ijzer" — op de Altra 128, op QEMU wat -smp zegt.
func (t *Tables) MADT() (cpus []CPU, gicd uint64, err error) {
	b := t.table("APIC")
	if b == nil {
		return nil, 0, fmt.Errorf("acpi: no MADT")
	}
	for off := 44; off+2 <= len(b); {
		typ, l := b[off], int(b[off+1])
		if l < 2 || off+l > len(b) {
			return nil, 0, fmt.Errorf("acpi: broken MADT entry at offset %d", off)
		}
		e := b[off : off+l]
		switch typ {
		case 0x0b: // GICC (ACPI 6.x: 80 bytes; MPIDR op offset 68)
			if l >= 76 {
				cpus = append(cpus, CPU{
					UID:     u32(e[8:]),
					MPIDR:   u64(e[68:]),
					Enabled: u32(e[12:])&1 != 0,
				})
			}
		case 0x0c: // GICD (24 bytes: GicId@4, PhysicalBaseAddress@8, versie@20)
			if l >= 16 {
				gicd = u64(e[8:])
			}
		}
		off += l
	}
	return cpus, gicd, nil
}

// ECAM is één MCFG-entry: een PCIe-configuratievenster.
type ECAM struct {
	Base     uint64
	Segment  uint16
	StartBus uint8
	EndBus   uint8
}

// MCFG geeft de PCIe-ECAM-vensters. De Altra heeft er meerdere (segments);
// QEMU virt één.
func (t *Tables) MCFG() ([]ECAM, error) {
	b := t.table("MCFG")
	if b == nil {
		return nil, fmt.Errorf("acpi: no MCFG")
	}
	var out []ECAM
	for off := 44; off+16 <= len(b); off += 16 {
		out = append(out, ECAM{
			Base:     u64(b[off:]),
			Segment:  u16(b[off+8:]),
			StartBus: b[off+10],
			EndBus:   b[off+11],
		})
	}
	return out, nil
}

// SPCR geeft de console-UART: MMIO-basis en interfacetype. Types die wij
// kunnen drijven met metal/pl011: 0x03 (PL011) en 0x0e (SBSA generic UART —
// een PL011-subset, zelfde DR/FR-offsets). De Altra en QEMU melden beide een
// van deze twee.
func (t *Tables) SPCR() (base uint64, ifType uint8, err error) {
	b := t.table("SPCR")
	if b == nil {
		return 0, 0, fmt.Errorf("acpi: no SPCR")
	}
	if len(b) < 52 {
		return 0, 0, fmt.Errorf("acpi: SPCR too short (%d)", len(b))
	}
	// Generic Address Structure op offset 40: space(1) width(1) off(1)
	// access(1) address(8).
	return u64(b[44:]), b[36], nil
}

// Watchdog geeft de SBSA Generic Watchdog uit de GTDT (platform-timer-
// structuur type 1): het refresh-frame (WRR) en control-frame (WCS/WOR).
// found=false: geen watchdog in de tabel (QEMU virt heeft er geen; de
// Altra wel — servers zijn hier braaf SBSA).
func (t *Tables) Watchdog() (refresh, control uint64, found bool) {
	b := t.table("GTDT")
	if b == nil || len(b) < 96 {
		return 0, 0, false
	}
	count := int(u32(b[88:]))
	off := int(u32(b[92:]))
	// Length beslaat bytes off+1..off+2 — de guard moet dus off+3 dekken
	// (review #8; de MADT-variant heeft een 1-byte-length en was al goed).
	for i := 0; i < count && off+3 <= len(b); i++ {
		typ, l := b[off], int(u16(b[off+1:]))
		if l < 4 || off+l > len(b) {
			return 0, 0, false
		}
		// Flags bit 2 = secure timer: die frames zijn vanuit NS-EL1
		// RAZ/WI of aborten — overslaan en doorzoeken (review #13,
		// Linux sbsa_gwdt doet hetzelfde).
		if typ == 1 && l >= 28 && u32(b[off+24:])&4 == 0 {
			return u64(b[off+4:]), u64(b[off+12:]), true
		}
		off += l
	}
	return 0, 0, false
}

// PSCI geeft de FADT ARM-boot-flags: is er PSCI, en is de conduit HVC (anders
// SMC). Zonder FADT of te korte FADT (pre-ACPI 5.1): geen PSCI-informatie.
func (t *Tables) PSCI() (compliant, useHVC bool, err error) {
	b := t.table("FACP")
	if b == nil {
		return false, false, fmt.Errorf("acpi: no FADT")
	}
	if len(b) < 131 {
		return false, false, fmt.Errorf("acpi: FADT too short for ARM_BOOT_ARCH (%d)", len(b))
	}
	flags := u16(b[129:])
	return flags&1 != 0, flags&2 != 0, nil
}
