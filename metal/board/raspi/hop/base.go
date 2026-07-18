// Package hop is de gedeelde HOP-bedrading van de raspi-SoC-laag: Base
// draagt alles wat rpi4/hop en rpi5/hop identiek deden (boot-EL, DTB-RAM,
// timers, PSCI-plumbing, stage-2-trampolines, lease/Net, framebuffer-
// discovery, console-stempel). Vóór 18-07 stond dit twee keer — twee plekken
// die synchroon moesten blijven op precies de naden (PSCI, timers, lease)
// waar drift stil misgaat.
//
// Het board levert alleen wat écht per SoC-versie verschilt: de
// MPIDR-nummering (CoreID/Target: A72 = aff0, A76 = aff1), de
// framebuffer-adressen, en ProbeNIC (GENET vs brcmpcie→RP1→GEM) — die blijft
// een board-methode, niet een Base-veld.
package hop

import (
	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/raspi/vcfb"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/cpu/psci"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/pcie"
	"hop-os/metal/fw/fdt"
	"hop-os/metal/net/dhcp"
)

// Base is de gedeelde board.Board-helft; rpi4/hop en rpi5/hop embedden hem in
// hun machine en vullen de naden.
type Base struct {
	CoreIDFn   func() int               // eigen MPIDR → core-index
	Target     func(core uint64) uint64 // core-index → MPIDR-target
	DTBPtr     uintptr                  // scratch-woord met de DTB-pointer (vcfb)
	VCMailBase uintptr                  // VideoCore-mailbox (vcfb-terugval)
}

func (b Base) BootEL() int { return int(raspi.BootEL()) }
func (b Base) CoreID() int { return b.CoreIDFn() }

// MemTotal leest de DTB die de firmware in x0 meegaf (cpuinit.s → DTBPtr) en
// telt het /memory-node op. 0 = niet gevonden. DTBPtr is het scratch-woord
// waarin cpuinit x0 legde, dus eerst dereferencen: het woord bevat het
// DTB-adres (raspi.DTB doet dat).
func (b Base) MemTotal() uint64 {
	if n, ok := fdt.MemTotal(raspi.DTB()); ok {
		return n
	}
	return 0
}

// CoreClass: beide Pi's zijn homogeen (4× A72 resp. 4× A76) — alle slots
// big-class ("big" = de beste klasse die dít board heeft; een A72 is trager
// dan een A76, maar dat is een node-keuze, geen slot-keuze).
func (b Base) CoreClass(i int) string { return "big" }

func (b Base) TimerOffset() int64     { return raspi.ARM64.TimerOffset }
func (b Base) SetTimerOffset(o int64) { raspi.ARM64.TimerOffset = o }
func (b Base) SetWallTime(ns int64)   { raspi.ARM64.SetTime(ns) }

// PSCI via de gedeelde wrappers (metal/cpu/psci; TF-A/armstub op EL3,
// conduit SMC) — hier wordt alleen de core-index naar het MPIDR-target
// vertaald (b.Target). Board-eigenaardigheden (stock-armstub zonder PSCI op
// de Pi 4, ALREADY_ON-gedrag op de Pi 5) staan bij de boards zelf.
func (b Base) CPUOn(core, entry, ctx uint64) int64 {
	return psci.On(b.Target(core), entry, ctx)
}
func (b Base) CPUOff() int64 { return psci.Off() }
func (b Base) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(psci.AffinityInfo(b.Target(core)))
}
func (b Base) PSCIVersion() (major, minor uint16) { return psci.Version() }

// Stage-2/SMP: de trampolines zijn board-neutraal en data-gedreven (gedeeld
// metal/cpu/el2 — geen GIC, geen MPIDR, geen ingebakken adressen; de
// hard-kill loopt via stage2.Revoke). Het board levert het PA-plan
// (rpi4.go/rpi5.go); hier is het één-op-één doorgeven.
func (b Base) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (b Base) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// Lease bewaart wat het board-ProbeNIC via DHCP ophaalde; Net/DHCPLease lezen
// hem. hopnet.Up roept ProbeNIC vóór Net() aan (die volgorde is het
// contract). Package-var is veilig: er linkt precies één Pi-board per binary.
var Lease dhcp.Lease

// Net geeft de DHCP-lease omgezet naar board.NetConfig (gedeelde omzetting
// in metal/board).
func (b Base) Net() board.NetConfig { return board.NetFromLease(Lease) }

// DHCPLease geeft de door ProbeNIC verkregen lease (board.LeaseHolder), zodat
// hopnet er na de stack-bring-up dhcp.KeepAlive op start. false vóór een
// echte ACK (dan is er niets te vernieuwen).
func (b Base) DHCPLease() (dhcp.Lease, bool) { return Lease, Lease.Acquired }

// PCIe: geen toewijsbaar venster op de Pi's — de Pi 4-lane zit vast aan de
// VL805-USB, de Pi 5-RP1 wordt volledig door ProbeNIC/brcmpcie gebracht.
func (b Base) PCIe() pcie.Window { return pcie.Window{} }

// Framebuffer: DTB-simplefb met mailbox-terugval — de gedeelde Pi-discovery
// (zie board/raspi/vcfb voor het meetverhaal); het board levert de adressen.
func (b Base) Framebuffer() (fb.Desc, bool) {
	return vcfb.FramebufferVC(b.DTBPtr, b.VCMailBase)
}

// EnableTimestamps zet de per-regel-console-stempel aan (optionele interface,
// door cmd/hopos ná de boot-banner aangeroepen). Zie board/raspi/console_ts.go.
func (b Base) EnableTimestamps() { raspi.LogTimestamps(true) }
