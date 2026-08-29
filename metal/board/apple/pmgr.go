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

	pmgrPollTimeout = 10 * time.Millisecond
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
