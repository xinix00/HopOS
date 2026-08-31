//go:build tamago && arm64

// pmgr.go — de power-manager: blokken aan- en uitzetten.
//
// Op elk ander HopOS-board heeft de firmware alles al aangezet wat wij nodig
// hebben. Op Apple silicon niet: iBoot laat de PCIe-controller uit staan, en
// wie hem wil gebruiken zet zelf zijn power-domein aan. Zolang m1n1 ertussen
// zat deed híj dat (`pmgr_adt_power_enable` uit `src/pmgr.c`); dit is dezelfde
// mechaniek, in Go, uit dezelfde bron: de ADT.
//
// De boom draagt drie dingen. `/arm-io/<blok>` heeft een `clock-gates`-lijst
// met device-ID's; `/arm-io/pmgr` heeft een `devices`-tabel die elk ID vertaalt
// naar (registerbank, offset, ouders); en `ps-regs` zegt waar die banken
// liggen. m1n1 kent daarnaast een `ps-groups`-vorm en smalle u8-ID's voor
// andere generaties; t8132 gebruikt ps-regs met u16-ID's (GEMETEN 29-08), dus
// die takken staan hier niet. Een blok aanzetten is dus: ID opzoeken, eerst zijn ouders
// aanzetten, dan hem — en dat laatste is niet vrijblijvend, een blok waarvan de
// klokvoorziening uit staat komt nooit in mode ACTIVE.
package apple

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/adt"
)

const (
	pmgrAutoEnable  = 1 << 28
	pmgrWasClkGated = 1 << 9
	pmgrWasPwrGated = 1 << 8
	pmgrPSTarget    = 0xF
	pmgrPSActive    = 0xF
	pmgrFlagVirtual = 0x10

	// Eén device-record in de `devices`-tabel is 48 bytes (m1n1 struct
	// pmgr_device, PACKED). Wij lezen er vijf velden uit:
	pmgrRecSize    = 48
	pmgrOffFlags   = 0  // u8
	pmgrOffParents = 4  // 2× u16
	pmgrOffAddr    = 10 // u8, << 3
	pmgrOffPSIdx   = 11 // u8 — index in ps-regs
	pmgrOffID      = 26 // u16
	pmgrOffName    = 32 // char[16], NUL-gevuld

	pmgrPollTimeout = 10 * time.Millisecond

	// Reset-bits van één device (m1n1 src/pmgr.c pmgr_reset_device).
	pmgrDevDisable = 1 << 10
	pmgrReset      = 1 << 31
	pmgrPSActual   = 0xF << 4
)

// pmgrTable is de uitgepakte tabel. Alle velden wijzen in de ADT zelf: dit is
// firmware-geheugen dat niet verandert, dus er valt niets te kopiëren.
type pmgrTable struct {
	t     adt.Tree
	chain []adt.Node // keten naar /arm-io/pmgr, voor de reg-vertaling
	ps    uintptr    // ps-regs
	psLen uint32
	devs  uintptr
	nDevs uint32
}

// pmgrOpen leest de tabel uit de boom.
func pmgrOpen() (pmgrTable, bool) {
	var p pmgrTable
	t, ok := ADT()
	if !ok {
		return p, false
	}
	p.t = t
	if p.chain, ok = t.PathTrace("/arm-io/pmgr"); !ok {
		return p, false
	}
	n := p.chain[len(p.chain)-1]
	if p.ps, p.psLen, ok = t.Prop(n, "ps-regs"); !ok || p.psLen == 0 {
		return p, false
	}
	var size uint32
	if p.devs, size, ok = t.Prop(n, "devices"); !ok || size < 2*pmgrRecSize {
		return p, false
	}
	p.nDevs = size / pmgrRecSize
	return p, true
}

// psreg geeft het adres van registerbank idx.
func (p pmgrTable) psreg(idx int) uintptr {
	if uint32(idx)*12+8 > p.psLen {
		return 0
	}
	regIdx := dev.Read32(p.ps + uintptr(idx)*12)
	regOff := dev.Read32(p.ps + uintptr(idx)*12 + 4)
	base, _, ok := p.t.RegAt(p.chain, int(regIdx))
	if !ok {
		return 0
	}
	return uintptr(base) + uintptr(regOff)
}

// rec geeft het adres van record i.
func (p pmgrTable) rec(i uint32) uintptr { return p.devs + uintptr(i)*pmgrRecSize }

func (p pmgrTable) id(r uintptr) uint16 { return dev.Read16(r + pmgrOffID) }

func (p pmgrTable) parents(r uintptr) (uint16, uint16) {
	return dev.Read16(r + pmgrOffParents), dev.Read16(r + pmgrOffParents + 2)
}

// addr geeft het statusregister van een device op een die.
func (p pmgrTable) addr(die int, r uintptr) uintptr {
	a := p.psreg(int(dev.Read8(r + pmgrOffPSIdx)))
	if a == 0 {
		return 0
	}
	return a + uintptr(die)*pmgrDieStep + uintptr(dev.Read8(r+pmgrOffAddr))<<3
}

// find zoekt het record van een device-ID.
func (p pmgrTable) find(id uint16) (uintptr, bool) {
	for i := uint32(0); i < p.nDevs; i++ {
		if r := p.rec(i); p.id(r) == id {
			return r, true
		}
	}
	return 0, false
}

// setMode schrijft de gewenste stand en wacht tot het blok hem meldt. De
// terugmelding staat in een ANDER veld dan het doel (PS_ACTUAL vs PS_TARGET) —
// dat is precies wat dit register bruikbaar maakt: het zegt niet wat je vroeg
// maar wat er gebeurde.
func pmgrSetMode(a uintptr, mode uint32) bool {
	v := dev.Read32(a)
	v &^= pmgrAutoEnable | pmgrWasClkGated | pmgrWasPwrGated | pmgrPSTarget
	dev.Write32(a, v|mode)
	deadline := time.Now().Add(pmgrPollTimeout)
	for {
		if dev.Read32(a)>>4&0xF == mode {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// setMode zet één device in de gewenste stand, ouders eerst bij aanzetten en
// laatst bij uitzetten. Virtuele devices hebben geen register — die bestaan
// alleen om ouders te groeperen.
func (p pmgrTable) setMode(die int, id uint16, mode uint32, depth int) bool {
	if id == 0 || depth > 8 {
		return false
	}
	r, ok := p.find(id)
	if !ok {
		return false
	}
	real := dev.Read8(r+pmgrOffFlags)&pmgrFlagVirtual == 0
	if mode == 0 && real {
		a := p.addr(die, r)
		if a == 0 || !pmgrSetMode(a, mode) {
			return false
		}
	}
	p0, p1 := p.parents(r)
	for _, par := range [2]uint16{p0, p1} {
		if par != 0 && !p.setMode(die, par, mode, depth+1) {
			return false
		}
	}
	if mode != 0 && real {
		a := p.addr(die, r)
		if a == 0 || !pmgrSetMode(a, mode) {
			return false
		}
	}
	return true
}

// PowerEnable zet het power-domein van een ADT-blok aan (`clock-gates`), met
// zijn ouders. Geeft het aantal devices dat aanging; 0 = de boom kende het blok
// niet of het lukte niet.
func PowerEnable(path string) int {
	p, ok := pmgrOpen()
	if !ok {
		return 0
	}
	n, ok := p.t.Path(path)
	if !ok {
		return 0
	}
	gates, size, ok := p.t.Prop(n, "clock-gates")
	if !ok || size == 0 {
		return 0
	}
	done := 0
	for i := uint32(0); i < size/4; i++ {
		v := dev.Read32(gates + uintptr(i)*4)
		if p.setMode(int(v>>28&0xF), uint16(v), pmgrPSActive, 0) {
			done++
		}
	}
	return done
}

// ResetNamed zet de genoemde devices door een echte reset heen: uitzetten,
// reset aan, even wachten, en in omgekeerde volgorde terug. Geeft het aantal
// devices dat gereset is.
//
// OP NAAM, en niet via de ADT-boom — dat is geen stijlkeuze maar een meting.
// De eerste versie hiervan liep over de `clock-gates` van een ADT-blok, zoals
// PowerEnable dat doet, en vond niets: `/arm-io/ans` HEEFT die eigenschap niet
// (gemeten 30-08 op ijzer: "no power domain found to reset"). m1n1 doet het
// daarom ook op naam (`pmgr_reset(die, "ANS")`), uit de devicetabel van de
// pmgr zelf. De naam staat op offset 32 van elk record, 16 bytes, NUL-gevuld.
//
// WAAROM DIT ER MOET ZIJN. Zolang m1n1 vóór ons draaide, deed híj dit: bij zijn
// eigen start trof hij iBoot's ANS "left powered" aan, praatte hem in slaap en
// resette dan zijn power-domein (nvme.c: nvme_ensure_shutdown). Onze
// NVMe-driver kreeg dus altijd een VERSE coprocessor aangereikt zonder dat wij
// dat wisten. Sinds wij zelf het bootobject zijn viel die stap weg en kwam
// CSTS.RDY nooit meer op 1 — ook niet na een echte power-cycle, want het is de
// staat die iBoot achterlaat en niet een restant van ons.
//
// Een device dat niet actief is slaan we over: resetten wat uit staat is
// zinloos, en m1n1 weigert het ook.
func ResetNamed(names ...string) int {
	p, ok := pmgrOpen()
	if !ok {
		return 0
	}
	done := 0
	for i := uint32(0); i < p.nDevs; i++ {
		r := p.rec(i)
		if !p.nameIn(r, names) {
			continue
		}
		// Een virtueel device is een alias zonder eigen register (zie
		// setMode): resetten heeft daar geen betekenis.
		if dev.Read8(r+pmgrOffFlags)&pmgrFlagVirtual != 0 {
			continue
		}
		a := p.addr(0, r)
		if a == 0 || dev.Read32(a)&pmgrPSActual != pmgrPSActive<<4 {
			continue
		}
		dev.Write32(a, dev.Read32(a)|pmgrDevDisable)
		dev.Write32(a, dev.Read32(a)|pmgrReset)
		time.Sleep(10 * time.Microsecond)
		dev.Write32(a, dev.Read32(a)&^uint32(pmgrReset))
		dev.Write32(a, dev.Read32(a)&^uint32(pmgrDevDisable))
		done++
	}
	return done
}

// nameIn zegt of de naam van dit record in de lijst staat. Vergelijken doen we
// byte voor byte uit de ADT: het veld is NUL-gevuld en er valt niets te
// alloceren op dit pad.
func (p pmgrTable) nameIn(r uintptr, names []string) bool {
	for _, want := range names {
		if len(want) > 15 {
			continue
		}
		ok := true
		for i := 0; i < len(want); i++ {
			if dev.Read8(r+pmgrOffName+uintptr(i)) != want[i] {
				ok = false
				break
			}
		}
		if ok && dev.Read8(r+pmgrOffName+uintptr(len(want))) == 0 {
			return true
		}
	}
	return false
}

// ResetANS reset de opslag-coprocessor. Drie namen, want ze verschillen per
// generatie: m1n1 probeert "ANS" en "ANS2" ("Some machines call this ANS, some
// ANS2...", src/nvme.c), maar op de M4 heet het tweede domein **ANS-V** —
// gemeten 30-08 met PMGRDump("ANS"), dat precies twee regels gaf:
//
//	ANS   ps=0x380700538 val=0xf0020ff
//	ANS-V ps=0x380700000 val=0x1f0
//
// Zoeken op de m1n1-namen alleen raakte er dus één, en dat verklaart waarom
// BOOT_STATUS na die "geslaagde" reset gewoon op OK bleef staan.
func ResetANS() int { return ResetNamed("ANS", "ANS2", "ANS-V") }

// PMGRDump geeft één regel per device waarvan de naam met prefix begint:
// naam, het ps-reg-adres en de waarde die er nu in staat. Puur voor de
// meetbank — "1 power domain(s) reset" zegt niets zolang je niet weet WELK
// domein dat was, en op de M4 bleef BOOT_STATUS na die ene reset gewoon staan
// (gemeten 30-08), wat betekent dat we de coprocessor-core niet raakten.
func PMGRDump(prefix string) []string {
	p, ok := pmgrOpen()
	if !ok {
		return nil
	}
	var out []string
	for i := uint32(0); i < p.nDevs; i++ {
		r := p.rec(i)
		name := make([]byte, 0, 16)
		for j := uintptr(0); j < 16; j++ {
			c := dev.Read8(r + pmgrOffName + j)
			if c == 0 {
				break
			}
			name = append(name, c)
		}
		if len(name) < len(prefix) || string(name[:len(prefix)]) != prefix {
			continue
		}
		a := p.addr(0, r)
		v := uint32(0)
		if a != 0 {
			v = dev.Read32(a)
		}
		out = append(out, fmt.Sprintf("%s ps=%#x val=%#x", name, uint64(a), v))
	}
	return out
}
