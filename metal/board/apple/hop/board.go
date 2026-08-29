// Package hop is de HOP-bedrading van het Apple-board (Mac mini M4): de
// volledige board.Board-implementatie. Alleen HOP-kant-binaries (cmd/)
// importeren deze helft; app-images gebruiken het generieke app-board
// (board/hopslot) en linken zo nooit tegen boardcode — dezelfde bronsplitsing
// als bij de Pi's, de Radxa en de LicheeRV.
//
// Wat hier anders is dan op elk ander ARM-board: er is geen PSCI. m1n1 heeft
// de secundaire cores gestart en in zijn spin-table geparkeerd; apple.Release
// laat ze daar los (het cpu-release-addr-protocol) en dát is CPUOn. Een core
// die eenmaal van ons is komt nooit meer bij m1n1 terug — HopOS parkeert zijn
// cores toch al zelf (kern/slots: de EL2-parkeerlus), dus AffinityInfo is hier
// onze eigen boekhouding: Off tot de eerste Release, daarna On.
//
// Core-nummering. HopOS rekent met core 0 = de HOP-core en 1..N = app-cores,
// aaneengesloten. m1n1 boot op cpu 6 (een P-core) en nummert 0..9; de
// logische HopOS-index laat die boot-cpu weg: cpu < boot → core cpu+1,
// cpu > boot → core cpu. Op de M4 (boot 6): cores 1..6 = de E-cores cpu 0..5,
// cores 7..9 = de P-cores cpu 7..9. Apps merken hier niets van: hun slotnummer
// komt van de slotHint die HOP in het image patcht (board/hopslot).
//
// Netwerk: nog geen NIC (de Broadcom 57762 achter Apple's PCIe is het volgende
// werk); ProbeNIC meldt "geen NIC" en de agent draait headless — precies het
// pad dat de Radxa op 05-08 ook eerst liep.
package hop

import (
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/board/apple"
	"github.com/xinix00/HopOS/metal/cpu/el2"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/pcie"
)

// machine is de board-implementatie voor Apple silicon onder m1n1.
type machine struct{}

// init registreert dit board: elke HOP-binary voor dit bord importeert deze
// hop-helft (cmd/hopos/board_apple.go), dus board.Current() is meteen geldig.
func init() { board.Use(machine{}) }

// Conformiteit compile-time bewezen (Derek, 18-07): zonder deze regel leunt het
// Board-contract puur op board.Use() at runtime.
var _ board.Board = machine{}

// Optioneel: het aantal app-cores declareren — AffinityInfo is hier geen PSCI
// maar onze eigen boekhouding, en de kern mag daar niet op hoeven te vertrouwen
// voor zijn telling.
var _ board.CoreCountHinter = machine{}

// cpuOf vertaalt een logische HopOS-core (1..N) naar m1n1's cpu-index; -1 als
// hij niet bestaat. Core 0 is de boot-cpu.
func cpuOf(core int) int {
	p, ok := apple.Params()
	if !ok || core < 0 || core >= p.NCPU {
		return -1
	}
	if core == 0 {
		return p.BootCPU
	}
	if core-1 < p.BootCPU {
		return core - 1
	}
	return core
}

func (machine) CoreID() int      { return apple.CoreID() }
func (machine) BootEL() int      { return apple.BootEL() }
func (machine) MemTotal() uint64 { return apple.MemTotal() }

// ExpectedAppCores: alles behalve de boot-cpu (M4: 9).
func (machine) ExpectedAppCores() int {
	p, ok := apple.Params()
	if !ok {
		return 0
	}
	return p.NCPU - 1
}

// CoreClass: de E-cores ("sawtooth", cluster 0) zijn "small", de P-cores
// ("everest", cluster 1) "big". De clustergrens zit in apple.CoreID's tabel;
// hier via het MPIDR dat m1n1 per cpu rapporteerde (aff1 = cluster).
func (machine) CoreClass(core int) string {
	p, ok := apple.Params()
	cpu := cpuOf(core)
	if !ok || cpu < 0 {
		return "big"
	}
	if p.MPIDR[cpu]>>8&0xFF == 0 {
		return "small"
	}
	return "big"
}

func (machine) TimerOffset() int64       { return apple.ARM64.TimerOffset }
func (machine) SetTimerOffset(off int64) { apple.ARM64.TimerOffset = off }
func (machine) SetWallTime(ns int64)     { apple.ARM64.SetTime(ns) }

// CPUOn = de spin-table-release (bewezen op alle negen cores, 28-08). De
// return-codes volgen PSCI zodat de kern één pad houdt: 0 = ok,
// -2 (INVALID_PARAMS) = geen zo'n core, -4 (ALREADY_ON) = al losgelaten —
// een tweede Release zou een draaiende core midden in zijn werk kapen.
func (machine) CPUOn(core, entry, ctx uint64) int64 {
	cpu := cpuOf(int(core))
	if cpu < 0 || int(core) == 0 {
		return -2
	}
	if apple.Released(cpu) {
		return -4
	}
	if !apple.Release(cpu, entry, ctx) {
		return -2
	}
	return board.PSCISuccess
}

// AffinityInfo: eigen boekhouding (zie de pakketdoc). Buiten 0..N-1 een
// waarde buiten {On,Off,OnPending}, zodat kern/slots daar zijn topologie
// laat stoppen.
func (machine) AffinityInfo(core uint64) board.PowerState {
	cpu := cpuOf(int(core))
	switch {
	case cpu < 0:
		return board.PowerState(-2)
	case core == 0 || apple.Released(cpu):
		return board.PowerOn
	}
	return board.PowerOff
}

// PSCIVersion: er ís geen PSCI; 0.0 zegt dat eerlijk in de firmware-regel.
func (machine) PSCIVersion() (major, minor uint16) { return 0, 0 }

// Stage-2/SMP: de trampolines zijn board-neutraal (metal/cpu/el2), gebouwd
// met -D VHE voor dit board (E2H staat vast op 1 — zie cpu/el2/sysreg.h).
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// ProbeNIC, Net en DHCPLease staan in net.go — dat is de keten van PCIe-link
// tot DHCP-lease.

// PCIe: de ECAM en het MMIO-venster van Apple's rootpoort (ADT apcie).
func (machine) PCIe() pcie.Window { return Window() }

// Framebuffer: geen. De display-firmware (DCP) komt onder m1n1 op de M4 niet
// op ("dcp-iboot: failed to initialize disp0"), dus is iBoot's buffer een dummy
// zonder scanout; ernaartoe tekenen zou niemand zien.
func (machine) Framebuffer() (fb.Desc, bool) { return fb.Desc{}, false }
