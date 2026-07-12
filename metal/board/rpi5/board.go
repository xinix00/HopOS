package rpi5

// board.go maakt van rpi5 een board.Board en registreert hem bij het laden.
// Fase-P-status: boot/PSCI/console/timers zijn er; de rest is expliciet
// afwezig tot de bijbehorende fase — een aanroep ervan is een bug, geen
// stille fallback.

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/brcmpcie"
	"hop-os/metal/dhcp"
	"hop-os/metal/el2"
	"hop-os/metal/fb"
	"hop-os/metal/fdt"
	"hop-os/metal/gem"
	"hop-os/metal/layout"
)

// machine is de board-implementatie voor de Raspberry Pi 5 (BCM2712).
type machine struct{}

// init registreert dit board: elke rpi5-binary importeert dit pakket al
// (verplicht, voor de tamago runtime-hooks).
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(raspi.BootEL()) }
func (machine) CoreID() int { return CoreID() }

// MemTotal leest de DTB die de firmware in x0 meegaf (cpuinit.s → DTBPtr) en
// telt het /memory-node op. 0 = niet gevonden. DTBPtr is het scratch-woord
// waarin cpuinit x0 legde, dus eerst dereferencen: het woord bevat het
// DTB-adres. LET OP: op het board te verifiëren (levert de Pi-firmware de
// DTB-pointer in x0 aan een raw kernel? zie docs/rpi5.md); de
// VideoCore-mailbox is de tweede bron (P2b).
func (machine) MemTotal() uint64 {
	if n, ok := fdt.MemTotal(raspi.DTB()); ok {
		return n
	}
	return 0
}

// CoreClass: de Pi 5 is homogeen (4× Cortex-A76) — per PLAN.md fase P zijn
// alle slots big-class.
func (machine) CoreClass(i int) string { return "big" }

func (machine) TimerOffset() int64     { return raspi.ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { raspi.ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { raspi.ARM64.SetTime(ns) }

// PSCI loopt via de gedeelde raspi-laag (TF-A/armstub op EL3, conduit SMC);
// hier wordt alleen de core-index naar het A76-MPIDR-target vertaald (aff1).
// LET OP (meetpunt probe): de standaard Pi-armstub zet secundaire cores
// mogelijk al "aan" (CPU_ON → ALREADY_ON) — dan vervangen we hem door een
// zelfgebouwde upstream-TF-A bl31.bin (armstub= in config.txt), die cores
// netjes geparkeerd houdt tot CPU_ON. Zie docs/rpi5.md.
func (machine) CPUOn(core, entry, ctx uint64) int64 { return raspi.CPUOn(target(core), entry, ctx) }
func (machine) CPUOff() int64                       { return raspi.CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(raspi.AffinityInfo(target(core)))
}
func (machine) PSCIVersion() (major, minor uint16) { return raspi.PSCIVersion() }

// Stage-2/SMP: de trampolines zijn board-neutraal en data-gedreven (gedeeld
// metal/el2 — geen GIC, geen MPIDR, geen ingebakken adressen; de hard-kill
// loopt via stage2.Revoke). Dit board levert het PA-plan (rpi5.go) en
// VBAR_EL2 → REVOKE_VEC in cpuinit; de rest is hier één-op-één doorgeven.
// Fase-P1-acceptatie = het isolatie/hard-kill/SMP-bewijs op het board zelf
// (metal/pi5_main.go).
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }
func (machine) SMPStubPC() uint64    { return el2.SMPStubPC() }

// lease bewaart wat ProbeNIC via DHCP ophaalde; Net() leest hem. hopnet.Up
// roept ProbeNIC vóór Net() aan (die volgorde is het contract).
var lease dhcp.Lease

// ProbeNIC brengt de hele netwerkketen op — elke stap boardvast bewezen met
// probe6 (2026-07-10, runs 2/4/5): RESCAL → pcie2-RC (54MHz-PLL!) →
// link-training (gen2 x4) → RP1-enumeratie (1de4:0001) → BAR's (BAR1 → PCIe
// 0x0, de DMA-loopback-eis) → PHY-reset via RP1-GPIO32 → autonegotiatie →
// GEM-init (DBW uit DCFG1) → DHCP-lease. De firmware doet hier niets van;
// vanaf de EEPROM-handoff is dit pad volledig van HOP.
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	// Het RP1-specifieke adresplan: de RC-basis/reset, het link-plafond en de
	// in/out-windows. De generieke bring-up-sequence (Rescal→Setup→StartLink→
	// OpenBridge→endpoint-check→BAR's→enable) woont nu in brcmpcie.RC.BringUp;
	// hier blijft alleen wat écht RP1 is.
	rc := &brcmpcie.RC{
		Base:     uintptr(PCIe2Base),
		SWInit:   uintptr(PCIeSWInit),
		SWInitID: PCIe2SWInit,
		Gen:      2,
		Out:      brcmpcie.OutWin{CPU: RP1Base, PCIe: 0, Size: 0x1000_0000},
		In: []brcmpcie.InWin{
			{PCIe: 0, CPU: RP1Base, Size: 0x40_0000},             // RP1-loopback (BAR1)
			{PCIe: 0x10_0000_0000, CPU: 0, Size: 0x10_0000_0000}, // al het DRAM
		},
	}
	// BAR's: vaste toewijzing (groottes gemeten met probe6: 16KB/4MB/64KB).
	// BAR1 MOET op PCIe 0x0 (RP1's eigen DMA bereikt zijn peripherals via de
	// loopback door het inbound-window hierboven).
	if err := rc.BringUp(brcmpcie.BringConfig{
		Rescal: uintptr(PCIeRescal),
		WantID: 0x1_1de4, // device 0x0001, vendor 0x1de4
		Bars: []brcmpcie.EPBar{
			{Off: 0x10, Val: 0x100_0000}, // BAR0
			{Off: 0x14, Val: 0x0},        // BAR1: peripheral-venster (PCIe 0x0, DMA-loopback)
			{Off: 0x18, Val: 0x101_0000}, // BAR2: SRAM
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("rp1: %w", err)
	}

	// De ethernet-PHY (BCM54213PE) hangt in reset aan RP1-GPIO32 (actief-
	// laag, 5ms — DT phy-reset-gpios; gemeten: zonder dit géén PHY op MDIO).
	RP1GPIOOut(32, false)
	time.Sleep(10 * time.Millisecond)
	RP1GPIOOut(32, true)
	time.Sleep(50 * time.Millisecond)

	nic := &gem.Net{
		Base:   uintptr(RP1EthBase),
		BusOff: 0x10_0000_0000, // RP1-masters → PCIe → RC-inbound → DRAM 0
		MAC:    raspi.MACFromSerial(raspi.DTB(), 0x05),
	}
	nic.MDIOEnable()
	addr, _, _, found := nic.PHYScan()
	if !found {
		return nil, nil, fmt.Errorf("rp1: geen PHY op de MDIO-bus")
	}
	speed, fd, err := nic.AutoNeg(addr, 8*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("rp1: %w", err)
	}
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize, speed, fd); err != nil {
		return nil, nil, err
	}

	l, err := dhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	lease = l
	return nic, net.HardwareAddr(nic.MAC[:]), nil
}

// Net geeft de DHCP-lease die ProbeNIC haalde, omgezet naar board.NetConfig
// via de gedeelde raspi-helper (identiek aan de Pi 4).
func (machine) Net() board.NetConfig { return raspi.NetFromLease(lease) }

// DHCPLease geeft de door ProbeNIC verkregen lease (board.LeaseHolder), zodat
// hopnet er na de stack-bring-up dhcp.KeepAlive op start. false vóór een echte
// ACK (dan is er niets te vernieuwen).
func (machine) DHCPLease() (dhcp.Lease, bool) { return lease, lease.Acquired }

// PCIe: fase P2 — de RP1 hangt aan de BCM2712-PCIe; het adresplan volgt bij
// de RP1-bring-up.
func (machine) PCIe() board.PCIeWindow { return board.PCIeWindow{} }

// Framebuffer: DTB-simplefb met mailbox-terugval — de gedeelde raspi-
// discovery (zie board/raspi/fb.go voor het meetverhaal); hier alleen de
// boardadressen.
func (machine) Framebuffer() (fb.Desc, bool) {
	return raspi.FramebufferVC(DTBPtr, uintptr(VCMailBase))
}

// EnableTimestamps zet de per-regel-console-stempel aan (optionele interface,
// door cmd/hopos ná de boot-banner aangeroepen). Zie board/raspi/console_ts.go.
func (machine) EnableTimestamps() { raspi.LogTimestamps(true) }
