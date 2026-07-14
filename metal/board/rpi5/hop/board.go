// Package hop is de HOP-bedrading van het rpi5-board: de volledige
// board.Board-implementatie mét drivers (brcmpcie → RP1 → GEM → DHCP,
// framebuffer). Alleen HOP-kant-binaries (cmd/) importeren deze helft;
// app-images importeren uitsluitend de basis (board/rpi5: runtime-hooks +
// appboard-contract) en linken zo nooit tegen de driverstack.
//
// Fase-P-status: boot/PSCI/console/timers zijn er; de rest is expliciet
// afwezig tot de bijbehorende fase — een aanroep ervan is een bug, geen
// stille fallback.
package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/raspi/vcfb"
	"hop-os/metal/board/rpi5"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/driver/brcmpcie"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/pcie"
	"hop-os/metal/driver/nic/gem"
	"hop-os/metal/fw/fdt"
	"hop-os/metal/net/dhcp"
)

// machine is de board-implementatie voor de Raspberry Pi 5 (BCM2712).
type machine struct{}

// init registreert dit board: elke HOP-binary voor de Pi 5 importeert deze
// hop-helft (cmd/hopos/board_rpi5.go); de basis registreerde het app-contract
// (appboard) al in háár init.
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(raspi.BootEL()) }
func (machine) CoreID() int { return rpi5.CoreID() }

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
// hier wordt alleen de core-index naar het A76-MPIDR-target vertaald (aff1,
// rpi5.Target). LET OP (meetpunt probe): de standaard Pi-armstub zet
// secundaire cores mogelijk al "aan" (CPU_ON → ALREADY_ON) — dan vervangen we
// hem door een zelfgebouwde upstream-TF-A bl31.bin (armstub= in config.txt),
// die cores netjes geparkeerd houdt tot CPU_ON. Zie docs/rpi5.md.
func (machine) CPUOn(core, entry, ctx uint64) int64 {
	return raspi.CPUOn(rpi5.Target(core), entry, ctx)
}
func (machine) CPUOff() int64 { return raspi.CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(raspi.AffinityInfo(rpi5.Target(core)))
}
func (machine) PSCIVersion() (major, minor uint16) { return raspi.PSCIVersion() }

// Stage-2/SMP: de trampolines zijn board-neutraal en data-gedreven (gedeeld
// metal/cpu/el2 — geen GIC, geen MPIDR, geen ingebakken adressen; de hard-kill
// loopt via stage2.Revoke). Dit board levert het PA-plan (rpi5.go) en
// VBAR_EL2 → REVOKE_VEC in cpuinit; de rest is hier één-op-één doorgeven.
// Fase-P1-acceptatie = het isolatie/hard-kill/SMP-bewijs op het board zelf
// (metal/pi5_main.go).
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// lease bewaart wat ProbeNIC via DHCP ophaalde; Net() leest hem. hopnet.Up
// roept ProbeNIC vóór Net() aan (die volgorde is het contract).
var lease dhcp.Lease

// theNIC is de door ProbeNIC gebouwde GEM (voor NetQuiesce); nil vóór P2-init.
var theNIC *gem.Net

// NetQuiesce (board.NetQuiescer): RX-DMA tijdelijk stil rond de
// slot-vensters — de C1-erratum-workaround. Vóór netwerk-init een no-op.
func (machine) NetQuiesce(off bool) {
	if theNIC != nil {
		theNIC.Quiesce(off)
	}
}

// LifecyclePace (board.LifecyclePacer): minimale adempauze tussen
// slot-lifecycles op dit board — C1-erratum-demper naast NetQuiesce.
func (machine) LifecyclePace() time.Duration { return 500 * time.Millisecond }

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
		Base:     uintptr(rpi5.PCIe2Base),
		SWInit:   uintptr(rpi5.PCIeSWInit),
		SWInitID: rpi5.PCIe2SWInit,
		Gen:      2,
		Out:      brcmpcie.OutWin{CPU: rpi5.RP1Base, PCIe: 0, Size: 0x1000_0000},
		In: []brcmpcie.InWin{
			{PCIe: 0, CPU: rpi5.RP1Base, Size: 0x40_0000},        // RP1-loopback (BAR1)
			{PCIe: 0x10_0000_0000, CPU: 0, Size: 0x10_0000_0000}, // al het DRAM
			// MIP0: het MSI-X-doel van de RP1 (bcm2712.dtsi dma-ranges: PCIe
			// 0xff_ffff_f000 → 0x10_0013_0000, 4KB). De RP1 vuurt peripheral-
			// IRQ's als MSI-writes op dít adres af; zonder window slaat zo'n
			// write op een ongemapt PCIe-adres — kansgedreven fabric-gif midden
			// in de RX-stroom (freeze-jacht 13-07, referentie-agent delta #2).
			{PCIe: 0xff_ffff_f000, CPU: 0x10_0013_0000, Size: 0x1000},
		},
	}
	// BAR's: vaste toewijzing (groottes gemeten met probe6: 16KB/4MB/64KB).
	// BAR1 MOET op PCIe 0x0 (RP1's eigen DMA bereikt zijn peripherals via de
	// loopback door het inbound-window hierboven).
	if err := rc.BringUp(brcmpcie.BringConfig{
		Rescal: uintptr(rpi5.PCIeRescal),
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
	rpi5.RP1GPIOOut(32, false)
	time.Sleep(10 * time.Millisecond)
	rpi5.RP1GPIOOut(32, true)
	time.Sleep(50 * time.Millisecond)

	nic := &gem.Net{
		Base:   uintptr(rpi5.RP1EthBase),
		BusOff: 0x10_0000_0000, // RP1-masters → PCIe → RC-inbound → DRAM 0
		MAC:    raspi.MACFromSerial(raspi.DTB(), 0x05),
	}
	theNIC = nic // voor NetQuiesce (slots-Start-venster, freeze-jacht 13-07)
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
// via de gedeelde omzetting in metal/board (identiek aan de Pi 4).
func (machine) Net() board.NetConfig { return board.NetFromLease(lease) }

// DHCPLease geeft de door ProbeNIC verkregen lease (board.LeaseHolder), zodat
// hopnet er na de stack-bring-up dhcp.KeepAlive op start. false vóór een echte
// ACK (dan is er niets te vernieuwen).
func (machine) DHCPLease() (dhcp.Lease, bool) { return lease, lease.Acquired }

// PCIe: fase P2 — de RP1 hangt aan de BCM2712-PCIe; het adresplan volgt bij
// de RP1-bring-up.
func (machine) PCIe() pcie.Window { return pcie.Window{} }

// Framebuffer: DTB-simplefb met mailbox-terugval — de gedeelde Pi-discovery
// (zie board/raspi/vcfb voor het meetverhaal); hier alleen de boardadressen.
func (machine) Framebuffer() (fb.Desc, bool) {
	return vcfb.FramebufferVC(rpi5.DTBPtr, uintptr(rpi5.VCMailBase))
}

// EnableTimestamps zet de per-regel-console-stempel aan (optionele interface,
// door cmd/hopos ná de boot-banner aangeroepen). Zie board/raspi/console_ts.go.
func (machine) EnableTimestamps() { raspi.LogTimestamps(true) }
