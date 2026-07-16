// Package hop is de HOP-bedrading van het uefi-board — de brug waardoor
// cmd/hopos (agent, leader, slots, stage-2-isolatie, NAT) op élk UEFI/ACPI-
// platform draait, de Ampere Altra voorop. Alles wat een Pi-board uit
// boardkennis haalt, komt hier uit wat de firmware al vertelde: cores uit de
// MADT, RAM uit de memory-map, PCIe uit de MCFG, beeld uit GOP, en CPU_ON via
// PSCI (conduit uit de FADT — SMC, de HopOS-invariant).
//
// Alleen HOP-kant-binaries (cmd/) importeren deze helft; app-images
// importeren uitsluitend de basis (board/uefi: runtime-hooks, PA-plan met
// app-guard, appboard-contract) en linken zo nooit tegen igb/pcie/dhcp.
package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/board/uefi"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/cpu/psci"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/nic/igb"
	"hop-os/metal/driver/pcie"
	"hop-os/metal/net/dhcp"
)

// machine is de board-implementatie voor UEFI/ACPI-platforms.
type machine struct{}

// init registreert dit board; het PA-plan zette de basis al (board/uefi
// plan.go, met de app-guard), het app-contract idem (appboard.go).
func init() { board.Use(machine{}) }

// SelfPlannedPool meldt dat dit board zijn slot-pool al op de gemeten vrije
// RAM heeft geplukt (basis-init, usablePool) — de main slaat dan de
// RequiredRAM-check over (die op statische qemuvirt-adressen leunt; hier
// zinloos).
func (machine) SelfPlannedPool() bool { return true }

func (machine) BootEL() int { return uefi.BootEL() }

// CoreID: eigen MPIDR opzoeken in de MADT-volgorde — dé core-nummering van
// dit platform (zie uefi.CoreID/coreIDFromMADT in de basis).
func (machine) CoreID() int { return uefi.CoreID() }

// MemTotal: het conventionele RAM uit de boot-memory-map plus de eigen
// claim (die stond op het moment van het snapshot als LoaderData geboekt).
func (machine) MemTotal() uint64 { return uefi.MemTotal() }

// CoreClass: de Altra (en QEMU-N1) is homogeen — alles is "big".
func (machine) CoreClass(i int) string { return "big" }

func (machine) TimerOffset() int64     { return uefi.ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { uefi.ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { uefi.ARM64.SetTime(ns) }

// PSCI: SMC-conduit (FADT bevestigt; HopOS-invariant). De core-index wordt
// via de MADT naar het MPIDR-target vertaald.
const (
	psciVersion  = 0x8400_0000
	psciCPUOff   = 0x8400_0002
	psciCPUOnSMC = 0xC400_0003 // SMC64
	psciAffInfo  = 0xC400_0004 // SMC64
)

func (machine) CPUOn(core, entry, ctx uint64) int64 {
	cpus := uefi.MADTCPUs()
	if int(core) >= len(cpus) {
		return -2 // PSCI INVALID_PARAMS
	}
	return int64(psci.SMC(psciCPUOnSMC, cpus[core].MPIDR, entry, ctx))
}
func (machine) CPUOff() int64 { return int64(psci.SMC(psciCPUOff, 0, 0, 0)) }
func (machine) AffinityInfo(core uint64) board.PowerState {
	cpus := uefi.MADTCPUs()
	if int(core) >= len(cpus) {
		return board.PowerState(-2)
	}
	return board.PowerState(psci.SMC(psciAffInfo, cpus[core].MPIDR, 0, 0))
}
func (machine) PSCIVersion() (major, minor uint16) {
	v := psci.SMC(psciVersion, 0, 0, 0)
	return uint16(v >> 16), uint16(v)
}

// ExpectedAppCores (board.CoreCountHinter): MADT-cores minus de HOP-core.
// Op de Altra 127 — slots begrenst zelf op MaxSlots/pool.
func (machine) ExpectedAppCores() int {
	if n := len(uefi.MADTCPUs()); n > 0 {
		return n - 1
	}
	return 0
}

// Stage-2/SMP: board-neutraal (metal/cpu/el2), identiek aan qemuvirt.
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// lease bewaart wat ProbeNIC via DHCP ophaalde (board.LeaseHolder-contract).
var lease dhcp.Lease

// eachECAM roept fn aan voor elk bereikbaar (via MapHigh) MCFG-segment tot
// fn true geeft; meldt of er een treffer was. Eén plek voor de MCFG→ECAM-
// walk die ProbeNIC en PCIe delen. Geen ACPI/MCFG → geen treffer.
func eachECAM(fn func(win pcie.Window, startBus int) bool) bool {
	t := uefi.Tables()
	if t == nil {
		return false
	}
	ecams, err := t.MCFG()
	if err != nil {
		return false
	}
	for _, e := range ecams {
		base, size := uefi.ECAMWindow(e)
		if !uefi.MapHigh(base, size) {
			// Diagnose op het scherm (geen serieel op de Altra): het
			// hoge-map-pad is op QEMU nooit geraakt, dus dít is de meting.
			ext, tcr, pr, used, max := uefi.VAStatus()
			fmt.Printf("net: ECAM %#x unreachable [%s] l0idx=%d vaExt=%v tcr=%#x parange=%d slots=%d/%d\n",
				base, uefi.MapFailReason(base), base>>39, ext, tcr, pr, used, max)
			continue
		}
		if fn(pcie.Window{ECAMBase: uintptr(e.Base)}, int(e.StartBus)) {
			return true // treffer: laat dit segment gemapt (fn gebruikt het nog)
		}
		uefi.UnmapHigh(base, size) // geen treffer: blokken teruggeven zodat de pool niet volloopt
	}
	return false
}

// ProbeNIC: MCFG → hiërarchie-scan → eerste igb-familielid → reset/link →
// ringen in het NetDMA-plan → DHCP. Hoge ECAM's/BAR's gaan door MapHigh
// (Altra: boven de vlakke 512GB, gemeten 13-07).
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	var d *pcie.Device
	eachECAM(func(win pcie.Window, startBus int) bool {
		for _, c := range pcie.ScanConfigured(win, startBus) {
			if c.VendorID == 0x8086 && igb.Supported(c.DeviceID) {
				d = c
				return true
			}
		}
		return false
	})
	if d == nil {
		return nil, nil, nil // geen igb-NIC (of geen ACPI/MCFG); headless
	}

	bar := d.BAR(0)
	if bar == 0 || !uefi.MapHigh(bar, 0x20000) {
		return nil, nil, fmt.Errorf("igb: BAR0 %#x unreachable", bar)
	}
	d.Enable()
	nic := &igb.Net{Base: uintptr(bar)}
	if err := nic.Reset(); err != nil {
		return nil, nil, err
	}
	speed, fd, err := nic.LinkUp(8 * time.Second)
	if err != nil {
		return nil, nil, err
	}
	fmt.Printf("net: igb %04x:%04x link %dMbps full-duplex=%v MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		d.VendorID, d.DeviceID, speed, fd,
		nic.MAC[0], nic.MAC[1], nic.MAC[2], nic.MAC[3], nic.MAC[4], nic.MAC[5])
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize); err != nil {
		return nil, nil, err
	}
	// Drop-wachter (netdoorvoer-jacht 15-07): de clear-on-read-tellers elke
	// 10s peilen en alleen printen als er iets te melden is — structureel
	// missed/nobuf onder last = de RX-drain is de fles, niet de lijn.
	go func() {
		for {
			time.Sleep(10 * time.Second)
			if m, nb := nic.Stats(); m|nb != 0 {
				fmt.Printf("igb: dropped frames: missed=%d nobuf=%d (last 10s)\n", m, nb)
			}
		}
	}()
	l, err := dhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	lease = l
	return nic, net.HardwareAddr(nic.MAC[:]), nil
}

// Net geeft de DHCP-lease als NetConfig (gedeelde omzetting in metal/board).
func (machine) Net() board.NetConfig { return board.NetFromLease(lease) }

// DHCPLease (board.LeaseHolder): hopnet start er de renewal op.
func (machine) DHCPLease() (dhcp.Lease, bool) { return lease, lease.Acquired }

// PCIe: het eerste bereikbare MCFG-segment als ECAM-venster (NVMe-fase;
// MMIOBase blijft 0 — BAR's zijn op UEFI-platforms al door de firmware
// toegewezen, HOP hoeft niets uit te delen).
func (machine) PCIe() pcie.Window {
	var win pcie.Window
	eachECAM(func(w pcie.Window, _ int) bool {
		win = w
		return true // eerste bereikbare segment volstaat
	})
	return win
}

// Framebuffer: het GOP-beeld dat de stub bewaarde (basis, uefi.GOPFramebuffer).
func (machine) Framebuffer() (fb.Desc, bool) { return uefi.GOPFramebuffer() }
