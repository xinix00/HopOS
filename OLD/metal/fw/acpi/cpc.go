package acpi

import "fmt"

// CPC is het _CPC-object (Collaborative Processor Performance Control, ACPI
// 8.4.6.1) van één processor, voor zover HopOS het nodig heeft: de perf-
// grenzen in de eenheid van de firmware (op de O6N: MHz) en het register
// waarin een gewenste prestatie geschreven wordt. Bron: de DSDT-AML, zonder
// interpreter — een _CPC is een Name met een Package van integers en
// Register()-buffers, en dát patroon is deterministisch te lezen.
type CPC struct {
	UID        uint32 // ACPI processor UID (de _UID vóór dit _CPC) — koppelt aan MADT CPU.UID
	Highest    uint32 // [2] HighestPerformance
	Nominal    uint32 // [3] NominalPerformance
	LowestNL   uint32 // [4] LowestNonlinearPerformance
	Lowest     uint32 // [5] LowestPerformance
	DesiredReg uint64 // [7] DesiredPerformanceRegister: SystemMemory-adres (0 = geen/andere ruimte)
	DesiredBit uint8  // registerbreedte in bits (32 op de O6N)

	// De perf-schaal is abstract (ACPI 8.4.6.1.1): pas LowestFrequency [21]
	// en NominalFrequency [22] (MHz, revisie 3) maken er een klok van. 0 =
	// het package draagt ze niet.
	LowestMHz, NominalMHz uint32
	// [13] Reference- en [14] DeliveredPerformanceCounter: wat de hardware
	// wérkelijk levert (Linux' cppc_cpufreq leest ze voor scaling_cur_freq).
	// Alleen de ruimte en het adres, als diagnose: 0x7f = FFH (op Arm: de
	// AMU-systeemregisters), 0 = SystemMemory.
	DeliveredSpace, ReferenceSpace uint8
	DeliveredAddr, ReferenceAddr   uint64
}

// MHz rekent een perf-waarde om naar een klok via Nominal ↔ NominalMHz; 0
// als het package geen frequenties draagt.
func (c CPC) MHz(perf uint32) uint32 {
	if c.Nominal == 0 || c.NominalMHz == 0 {
		return 0
	}
	return uint32(uint64(perf) * uint64(c.NominalMHz) / uint64(c.Nominal))
}

// Perf is de omgekeerde van MHz: de perf-waarde bij een klok, naar beneden
// afgerond. Draagt het package geen frequenties, dan is er niets om te rekenen
// en is mhz al een perf-waarde (ok=false).
func (c CPC) Perf(mhz uint32) (perf uint32, ok bool) {
	if c.Nominal == 0 || c.NominalMHz == 0 {
		return mhz, false
	}
	return uint32(uint64(mhz) * uint64(c.Nominal) / uint64(c.NominalMHz)), true
}

// CPCs zoekt álle _CPC-objecten in de DSDT en SSDT's. Leeg = geen CPPC (of
// een vorm die we niet lezen: _CPC als Method). Foutloos per constructie:
// wat niet parsebaar is wordt overgeslagen, want dit is diagnose-invoer
// voor de klok en nooit een boot-blokker.
func (t *Tables) CPCs() []CPC {
	var out []CPC
	for _, sig := range []string{"DSDT", "SSDT"} {
		for _, b := range t.allTables(sig) {
			out = append(out, scanCPC(b)...)
		}
	}
	return out
}

// allTables geeft alle tabellen met deze signatuur (SSDT komt vaker voor).
func (t *Tables) allTables(sig string) [][]byte {
	var out [][]byte
	for _, e := range t.all {
		if e.sig == sig {
			if b := decodeAt(e.pa); b != nil {
				out = append(out, b)
			}
		}
	}
	return out
}

// AML-opcodes die hier voorkomen.
const (
	amlZero    = 0x00
	amlOne     = 0x01
	amlByte    = 0x0a
	amlWord    = 0x0b
	amlDWord   = 0x0c
	amlQWord   = 0x0e
	amlBuffer  = 0x11
	amlPackage = 0x12
	amlOnes    = 0xff

	gasDescriptor = 0x82 // Generic Register descriptor (large item)
	gasSystemMem  = 0x00
)

// scanCPC loopt de AML-bytes af op "_CPC" (voorafgegaan door NameOp 0x08)
// en parseert het Package erachter; de laatst geziene _UID-integer is de
// processor.
func scanCPC(b []byte) []CPC {
	var out []CPC
	uid, haveUID := uint32(0), false
	for i := 0; i+5 <= len(b); i++ {
		if b[i] != 0x08 { // NameOp
			continue
		}
		name := string(b[i+1 : i+5])
		switch name {
		case "_UID":
			if v, _, ok := amlInteger(b, i+5); ok {
				uid, haveUID = uint32(v), true
			}
		case "_CPC":
			if c, ok := parseCPCPackage(b, i+5); ok && haveUID {
				c.UID = uid
				out = append(out, c)
			}
		}
	}
	return out
}

// pkgLength decodeert een AML PkgLength op b[i:]: (lengte, aantal bytes van
// het lengteveld). De lengte telt het lengteveld zelf mee.
func pkgLength(b []byte, i int) (int, int, bool) {
	if i >= len(b) {
		return 0, 0, false
	}
	lead := b[i]
	n := int(lead>>6) + 1
	if i+n > len(b) {
		return 0, 0, false
	}
	if n == 1 {
		return int(lead & 0x3f), 1, true
	}
	l := int(lead & 0x0f)
	for k := 1; k < n; k++ {
		l |= int(b[i+k]) << (4 + 8*(k-1))
	}
	return l, n, true
}

// amlInteger leest een integer-constante op b[i:]: (waarde, breedte, ok).
func amlInteger(b []byte, i int) (uint64, int, bool) {
	if i >= len(b) {
		return 0, 0, false
	}
	switch b[i] {
	case amlZero:
		return 0, 1, true
	case amlOne:
		return 1, 1, true
	case amlOnes:
		return ^uint64(0), 1, true
	case amlByte:
		if i+2 <= len(b) {
			return uint64(b[i+1]), 2, true
		}
	case amlWord:
		if i+3 <= len(b) {
			return uint64(u16(b[i+1:])), 3, true
		}
	case amlDWord:
		if i+5 <= len(b) {
			return uint64(u32(b[i+1:])), 5, true
		}
	case amlQWord:
		if i+9 <= len(b) {
			return u64(b[i+1:]), 9, true
		}
	}
	return 0, 0, false
}

// cpcElem is één element van het _CPC-package: een integer óf een Register.
type cpcElem struct {
	isReg bool
	val   uint64 // integer
	space uint8  // register: GAS space id
	bits  uint8  // register: bit width
	addr  uint64 // register: adres
}

// parseCPCPackage leest het Package op b[i:] element voor element.
func parseCPCPackage(b []byte, i int) (CPC, bool) {
	if i >= len(b) || b[i] != amlPackage {
		return CPC{}, false
	}
	l, n, ok := pkgLength(b, i+1)
	if !ok || i+1+l > len(b) {
		return CPC{}, false
	}
	end := i + 1 + l
	p := i + 1 + n
	if p >= end {
		return CPC{}, false
	}
	count := int(b[p])
	p++
	var elems []cpcElem
	for k := 0; k < count && p < end; k++ {
		if v, w, ok := amlInteger(b, p); ok {
			elems = append(elems, cpcElem{val: v})
			p += w
			continue
		}
		if b[p] != amlBuffer {
			return CPC{}, false // iets anders (Method-call, naam): niet ons patroon
		}
		bl, bn, ok := pkgLength(b, p+1)
		if !ok || p+1+bl > end {
			return CPC{}, false
		}
		bufEnd := p + 1 + bl
		q := p + 1 + bn
		// BufferSize (TermArg: een integer-constante), dan de ruwe bytes.
		if _, w, ok := amlInteger(b, q); ok {
			q += w
		} else {
			return CPC{}, false
		}
		e := cpcElem{isReg: true}
		if q+14 <= bufEnd && b[q] == gasDescriptor && b[q+1] == 0x0c && b[q+2] == 0x00 {
			// Generic Register (ACPI 6.5 §6.4.3.7): +3 AddressSpaceID,
			// +4 BitWidth, +5 BitOffset, +6 AccessSize, +7..+14 Address.
			// Tot 17-09 stond hier q+6: de AccessSize (3 = dword) werd de
			// laagste byte van het adres — 0x0659009c werd 0x659009c03, en
			// de eerste read daarvan op de O6N een external abort.
			e.space = b[q+3]
			e.bits = b[q+4]
			e.addr = u64(b[q+7:])
		}
		elems = append(elems, e)
		p = bufEnd
	}
	// Package-indeling (ACPI 8.4.6.1.1): 0 NumEntries, 1 Revision, 2 Highest,
	// 3 Nominal, 4 LowestNonlinear, 5 Lowest, 6 GuaranteedReg, 7 DesiredReg.
	if len(elems) < 8 {
		return CPC{}, false
	}
	c := CPC{}
	pick := func(k int) uint32 {
		if elems[k].isReg {
			return 0 // een register i.p.v. een constante: niet ondersteund
		}
		return uint32(elems[k].val)
	}
	c.Highest, c.Nominal, c.LowestNL, c.Lowest = pick(2), pick(3), pick(4), pick(5)
	if d := elems[7]; d.isReg && d.space == gasSystemMem && d.addr != 0 {
		c.DesiredReg, c.DesiredBit = d.addr, d.bits
	}
	if len(elems) > 14 {
		c.ReferenceSpace, c.ReferenceAddr = elems[13].space, elems[13].addr
		c.DeliveredSpace, c.DeliveredAddr = elems[14].space, elems[14].addr
	}
	if len(elems) > 22 {
		c.LowestMHz, c.NominalMHz = pick(21), pick(22)
	}
	return c, true
}

// String is de diagnose-regel van één _CPC.
func (c CPC) String() string {
	return fmt.Sprintf("uid %d perf %d..%d (nominal %d = %d MHz, lowest %d MHz, nonlinear %d) desired@%#x/%d delivered %#x@%#x reference %#x@%#x",
		c.UID, c.Lowest, c.Highest, c.Nominal, c.NominalMHz, c.LowestMHz, c.LowestNL, c.DesiredReg, c.DesiredBit,
		c.DeliveredSpace, c.DeliveredAddr, c.ReferenceSpace, c.ReferenceAddr)
}
