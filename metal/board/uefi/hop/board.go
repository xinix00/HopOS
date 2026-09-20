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
	"github.com/xinix00/HopOS/metal/v2/driver/gicv3"
	"net"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/v2/net/netdev"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/cpu/psci"
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/igb"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/rtl8126"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/lean/leandhcp"
)

// Machine is de board-implementatie voor UEFI/ACPI-platforms. Geëxporteerd
// zodat een concreet UEFI-bord (board/o6n) hem kan embedden en alleen zijn
// eigen kennis overschrijft (clusterklassen, thermometer, NIC-interrupt).
type Machine struct{}

// init registreert dit board; het PA-plan zette de basis al (board/uefi
// plan.go, met de app-guard), het app-contract idem (appboard.go).
func init() { board.Use(Machine{}) }

// Conformiteit compile-time bewezen: zonder deze regel leunt het Board-
// contract puur op board.Use() at runtime en wordt een gemiste methode pas
// op het bord zichtbaar (Derek, 18-07).
var _ board.Board = Machine{}

// SelfPlannedPool meldt dat dit board zijn slot-pool al op de gemeten vrije
// RAM heeft geplukt (basis-init, usablePool) — de main slaat dan de
// RequiredRAM-check over (die op statische qemuvirt-adressen leunt; hier
// zinloos).
func (Machine) SelfPlannedPool() bool { return true }

// Privilege/Firmware: EL2-boot vereist; de PSCI-provider zit onder ons (FADT
// bevestigt de SMC-conduit — de HopOS-invariant).
func (Machine) Privilege() error { return board.RequireEL2(uefi.BootEL()) }
func (Machine) Firmware() string { return psci.Line(uefi.BootEL()) }

// CoreID: eigen MPIDR opzoeken in de MADT-volgorde — dé core-nummering van
// dit platform (zie uefi.CoreID/coreIDFromMADT in de basis).
func (Machine) CoreID() int { return uefi.CoreID() }

// MemTotal: het conventionele RAM uit de boot-memory-map plus de eigen
// claim (die stond op het moment van het snapshot als LoaderData geboekt).
func (Machine) MemTotal() uint64 { return uefi.MemTotal() }

// CoreClass: de Altra (en QEMU-N1) is homogeen — alles is "big".
func (Machine) CoreClass(i int) string { return "big" }

func (Machine) TimerOffset() int64     { return uefi.ARM64.TimerOffset }
func (Machine) SetTimerOffset(o int64) { uefi.ARM64.TimerOffset = o }
func (Machine) SetWallTime(ns int64)   { uefi.ARM64.SetTime(ns) }

// Cores: PSCI via de gedeelde wrappers (metal/cpu/psci). De core-index wordt
// via de MADT naar het MPIDR-target vertaald. De app-lijst komt uit de MADT
// zelf en niet uit een PSCI-probe: op sommige silicium meldt AFFINITY_INFO
// INVALID_PARAMS voor bestaande cores, en dan adverteerde HOP nul slots. Op de
// Altra 127 — slots begrenst zelf op MaxSlots/pool. Geen Reset: een
// ingetrokken core parkeert zichzelf in de EL2-lus.
//
// De eigen core is NIET per se MADT-index 0: de spec eist dat de boot-core
// vooraan staat, maar de O6N-firmware boot op een A720 (MPIDR 0xa00) terwijl
// haar MADT met 0x400 begint (dmesg + Madt.aslc, 09-09). Dus: alles behalve
// CoreID() — anders adverteert HOP zijn eigen core als slot en start hij er
// nooit één op.
func (Machine) Cores() board.Cores {
	return board.Cores{
		App: func() []int {
			var app []int
			own := uefi.CoreID()
			for c := 0; c < len(uefi.MADTCPUs()); c++ {
				if c != own {
					app = append(app, c)
				}
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
		// Het M4-model op een GIC (20-09): een app-core yieldt naar EL2 en
		// slaapt daar in WFI; HOP's wekker stuurt een SGI (kern/slots
		// waker.go) en de switcher ackt hem (cpu/el2/switch.s, GIC_IPI).
		// Zonder kick wachtte elke slapende app-core op de event-stream-tik:
		// app→HOP 2,3 ms en een hairpin van 32 ms op de Altra tegen 0,6 en
		// 2 ms op de M4, en node-naar-node 44 MB/s met iedereen idle.
		IdleMode: func(int) uint64 { return layout.IdleYield },
		Kick:     kickCore,
	}
}

var sgiPrepared sync.Map // core → true: redistributor klaar voor de kick-SGI

// kickCore wekt app-core c met de kick-SGI; de eerste keer maakt hij de
// redistributor van die core klaar.
func kickCore(c int) {
	cpus := uefi.MADTCPUs()
	if c < 0 || c >= len(cpus) {
		return
	}
	ctrl, err := controller()
	if err != nil {
		return
	}
	if _, ok := sgiPrepared.Load(c); !ok {
		gicr := uintptr(cpus[c].GICR)
		if gicr == 0 {
			_, gicrBase, gicrLen := gicrRange()
			if f, ok := gicv3.FindRedistributor(gicrBase, gicrLen, cpus[c].MPIDR); ok {
				gicr = uintptr(f)
			}
		}
		if gicr == 0 || !uefi.MapHigh(uint64(gicr), 0x20000) {
			fmt.Printf("irq: core %d: no redistributor for the kick SGI — that core wakes on its timer\n", c)
			sgiPrepared.Store(c, true)
			return
		}
		gicv3.PrepareSGI(gicr, gicv3.KickSGI)
		sgiPrepared.Store(c, true)
		_ = ctrl
	}
	gicv3.SendSGI(gicv3.KickSGI, cpus[c].MPIDR)
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

// nicDriver is één PCIe-NIC-driver die dit board kent: herkent hij (vendor,
// device), dan brengt Up hem op in het NetDMA-plan en geeft hij het device +
// zijn frame-bufferbereik terug (voor de Normal-WB-remap). Eén tabel voor
// álle UEFI-boards: een igb (Altra, QEMU) of een RTL8126 (Orion O6N) — welke
// het is, zegt de MCFG-scan, niet de build.
type nicDriver struct {
	name  string
	match func(vendor, device uint16) bool
	up    func(d *pcie.Device) (nic netdev.Device, mac [6]byte, bufBase, bufSize uintptr, err error)
}

var nicDrivers = []nicDriver{
	{"igb", func(v, d uint16) bool { return v == 0x8086 && igb.Supported(d) }, upIGB},
	{"rtl8126", rtl8126.Supported, upRTL8126},
}

// upIGB: BAR0 → reset/link → ringen (Altra-recept, sinds 13-07).
func upIGB(d *pcie.Device) (netdev.Device, [6]byte, uintptr, uintptr, error) {
	bar := d.BAR(0)
	if bar == 0 || !uefi.MapHigh(bar, 0x20000) {
		return nil, [6]byte{}, 0, 0, fmt.Errorf("igb: BAR0 %#x unreachable", bar)
	}
	d.Enable()
	nic := &igb.Net{Base: uintptr(bar)}
	if err := nic.Reset(); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	speed, fd, err := nic.LinkUp(8 * time.Second)
	if err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	fmt.Printf("net: igb %04x:%04x link %dMbps full-duplex=%v MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		d.VendorID, d.DeviceID, speed, fd,
		nic.MAC[0], nic.MAC[1], nic.MAC[2], nic.MAC[3], nic.MAC[4], nic.MAC[5])
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	base, size := nic.BufRegion()
	return nic, nic.MAC, base, size, nil
}

// upRTL8126: BAR2 (het MMIO-blok van de Realtek; BAR0 is de I/O-alias) →
// reset/MAC → ringen + MAC aan → PHY/autoneg. NBASE-T-autonegotiatie kan
// seconden duren, dus een ruimere link-wacht dan de igb.
func upRTL8126(d *pcie.Device) (netdev.Device, [6]byte, uintptr, uintptr, error) {
	bar := d.BAR(2)
	if bar == 0 || !uefi.MapHigh(bar, 0x10000) {
		return nil, [6]byte{}, 0, 0, fmt.Errorf("rtl8126: BAR2 %#x unreachable", bar)
	}
	d.Enable()
	nic := &rtl8126.Net{Base: uintptr(bar)}
	nicRTL = nic
	if err := nic.Reset(); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	speed, fd, err := nic.LinkUp(12 * time.Second)
	if err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	fmt.Printf("net: %s %04x:%04x xid %#x link %dMbps full-duplex=%v MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		nic.Name, d.VendorID, d.DeviceID, nic.XID, speed, fd,
		nic.MAC[0], nic.MAC[1], nic.MAC[2], nic.MAC[3], nic.MAC[4], nic.MAC[5])
	base, size := nic.BufRegion()
	return nic, nic.MAC, base, size, nil
}

// nicDev/nicStartBus/nicRTL: wat ProbeNIC vond — voor de interruptbedrading
// van een concreet bord (board/o6n/hop: de INTx-lijn hangt aan de root-port
// = het MCFG-segment waarin de NIC zat) en voor de IRQ-hooks van de driver.
var (
	nicDev      *pcie.Device
	nicStartBus int
	nicRTL      *rtl8126.Net
)

// NIC geeft het PCIe-device van de gekozen NIC en het startbusnummer van
// zijn MCFG-segment (de root-port); nil = geen NIC.
func NIC() (*pcie.Device, int) { return nicDev, nicStartBus }

// RTL geeft de Realtek-driver als de NIC er een is (nil = igb of geen).
func RTL() *rtl8126.Net { return nicRTL }

// ProbeNIC: MCFG → hiërarchie-scan → eerste NIC uit de drivertabel → op →
// DHCP. Hoge ECAM's/BAR's gaan door MapHigh (Altra: boven de vlakke 512GB,
// gemeten 13-07). Twee poorten (O6N): de eerste in busvolgorde wint; de
// tweede blijft ongebruikt tot er een reden is voor twee uplinks.
func (Machine) ProbeNIC() (netdev.Device, net.HardwareAddr, error) {
	var d *pcie.Device
	var drv nicDriver
	eachECAM(func(win pcie.Window, startBus int) bool {
		for _, c := range pcie.ScanConfigured(win, startBus) {
			for _, nd := range nicDrivers {
				if nd.match(c.VendorID, c.DeviceID) {
					d, drv = c, nd
					nicDev, nicStartBus = c, startBus
					return true
				}
			}
		}
		return false
	})
	if d == nil {
		return nil, nil, nil // geen bekende NIC (of geen ACPI/MCFG); headless
	}
	fmt.Printf("net: %s at %v (root port bus %#x)\n", drv.name, d, nicStartBus)
	nic, mac, bufBase, bufSize, err := drv.up(d)
	if err != nil {
		return nil, nil, err
	}
	// Frame-buffers Normal-WB mappen (descriptors blijven device): de dure
	// ongecachte 1500B-reads — het gemeten netdoorvoer-dak (17-07) — worden
	// cache-snelheid; de driver doet de DC-hygiëne rond de DMA. Weigert
	// MapNormal, dan draait alles gewoon ongecached door.
	if uefi.MapNormal(bufBase, bufSize) {
		fmt.Printf("net: %s frame buffers write-back cached (descriptors stay uncached)\n", drv.name)
	} else {
		fmt.Printf("net: %s frame buffers remain uncached (MapNormal declined)\n", drv.name)
	}
	l, err := leandhcp.Acquire(nic, mac, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	lease = l
	// De NIC-interrupt, ná een geslaagde probe (een retry maakt een nieuwe
	// driver-instantie; de bedrading hoort bij de laatste) en eenmalig.
	if ni, ok := nic.(NICInterrupt); ok && nicLine.ID == 0 {
		bus := nicStartBus
		irqOnce.Do(func() { setupNICIRQ(ni, bus) })
	}
	return nic, net.HardwareAddr(mac[:]), nil
}

var irqOnce sync.Once

var _ board.NICInterrupter = Machine{}

// Net geeft de DHCP-lease als NetConfig (gedeelde omzetting in metal/board).
func (Machine) Net() board.NetConfig { return board.NetFromLease(lease) }

// DHCPLease (board.LeaseHolder): hopnet start er de renewal op.
func (Machine) DHCPLease() (leandhcp.Lease, bool) { return lease, lease.Acquired }

// PCIe: het eerste bereikbare MCFG-segment als ECAM-venster (NVMe-fase;
// MMIOBase blijft 0 — BAR's zijn op UEFI-platforms al door de firmware
// toegewezen, HOP hoeft niets uit te delen).
func (Machine) PCIe() pcie.Window {
	var win pcie.Window
	eachECAM(func(w pcie.Window, _ int) bool {
		win = w
		return true // eerste bereikbare segment volstaat
	})
	return win
}

// Framebuffer: het GOP-beeld dat de stub bewaarde (basis, uefi.GOPFramebuffer).
func (Machine) Framebuffer() (fb.Desc, bool) { return uefi.GOPFramebuffer() }
