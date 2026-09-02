// Package hop is de HOP-bedrading van het uefi-board — de brug waardoor
// cmd/hopos (agent, leader, slots, stage-2-isolatie, NAT) op élk UEFI/ACPI-
// platform draait, de Ampere Altra voorop. Alles wat een Pi-board uit
// boardkennis haalt, komt hier uit wat de firmware al vertelde: cores uit de
// MADT, RAM uit de memory-map, PCIe uit de MCFG, beeld uit GOP, en CPU_ON via
// PSCI (conduit uit de FADT — SMC, de HopOS-invariant).
//
// Alleen HOP-kant-binaries (cmd/) importeren deze helft; app-images
// importeren uitsluitend de basis (board/uefi: runtime-hooks, PA-plan met
// app-guard, appboard-contract) en linken zo nooit tegen igb/pcie/leandhcp.
package hop

import (
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/net/netdev"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/board/uefi"
	"github.com/xinix00/HopOS/metal/cpu/psci"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/nic/igb"
	"github.com/xinix00/HopOS/metal/driver/pcie"
	"github.com/xinix00/lean/leandhcp"
)

// machine is de board-implementatie voor UEFI/ACPI-platforms.
type machine struct{}

// init registreert dit board; het PA-plan zette de basis al (board/uefi
// plan.go, met de app-guard), het app-contract idem (appboard.go).
func init() { board.Use(machine{}) }

// Conformiteit compile-time bewezen: zonder deze regel leunt het Board-
// contract puur op board.Use() at runtime en wordt een gemiste methode pas
// op het bord zichtbaar (Derek, 18-07).
var _ board.Board = machine{}

// SelfPlannedPool meldt dat dit board zijn slot-pool al op de gemeten vrije
// RAM heeft geplukt (basis-init, usablePool) — de main slaat dan de
// RequiredRAM-check over (die op statische qemuvirt-adressen leunt; hier
// zinloos).
func (machine) SelfPlannedPool() bool { return true }

// Privilege/Firmware: EL2-boot vereist; de PSCI-provider zit onder ons (FADT
// bevestigt de SMC-conduit — de HopOS-invariant).
func (machine) Privilege() error { return board.RequireEL2(uefi.BootEL()) }
func (machine) Firmware() string { return psci.Line(uefi.BootEL()) }

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

// Cores: PSCI via de gedeelde wrappers (metal/cpu/psci). De core-index wordt
// via de MADT naar het MPIDR-target vertaald. De app-lijst komt uit de MADT
// zelf en niet uit een PSCI-probe: op sommige silicium meldt AFFINITY_INFO
// INVALID_PARAMS voor bestaande cores, en dan adverteerde HOP nul slots. Op de
// Altra 127 — slots begrenst zelf op MaxSlots/pool. Geen Reset: een
// ingetrokken core parkeert zichzelf in de EL2-lus.
func (machine) Cores() board.Cores {
	return board.Cores{
		App: func() []int {
			var app []int
			for c := 1; c < len(uefi.MADTCPUs()); c++ {
				app = append(app, c)
			}
			return app
		},
		Start: func(c int, entry, arg uint64) error {
			cpus := uefi.MADTCPUs()
			if c < 0 || c >= len(cpus) {
				return fmt.Errorf("uefi: no core %d in the MADT", c)
			}
			return psci.On(cpus[c].MPIDR, entry, arg)
		},
		State: func(c int) board.PowerState {
			cpus := uefi.MADTCPUs()
			if c < 0 || c >= len(cpus) {
				return board.PowerState(psci.INVALID_PARAMS)
			}
			return board.PowerState(psci.AffinityInfo(cpus[c].MPIDR))
		},
	}
}

// lease bewaart wat ProbeNIC via DHCP ophaalde (board.LeaseHolder-contract).
var lease leandhcp.Lease

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
func (machine) ProbeNIC() (netdev.Device, net.HardwareAddr, error) {
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
	// Frame-buffers Normal-WB mappen (descriptors blijven device): de dure
	// ongecachte 1500B-reads — het gemeten netdoorvoer-dak (17-07) — worden
	// cache-snelheid; de driver doet de DC-hygiëne rond de DMA (igb.go).
	// Weigert MapNormal, dan draait alles gewoon ongecached door.
	if base, size := nic.BufRegion(); uefi.MapNormal(base, size) {
		fmt.Println("net: igb frame buffers write-back cached (descriptors stay uncached)")
	} else {
		fmt.Println("net: igb frame buffers remain uncached (MapNormal declined)")
	}
	l, err := leandhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	lease = l
	return nic, net.HardwareAddr(nic.MAC[:]), nil
}

// Net geeft de DHCP-lease als NetConfig (gedeelde omzetting in metal/board).
func (machine) Net() board.NetConfig { return board.NetFromLease(lease) }

// DHCPLease (board.LeaseHolder): hopnet start er de renewal op.
func (machine) DHCPLease() (leandhcp.Lease, bool) { return lease, lease.Acquired }

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
