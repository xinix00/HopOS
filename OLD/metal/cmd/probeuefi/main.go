// probeuefi — de UEFI/ACPI-discovery-probe: het eerste HopOS-levensteken op
// servers (Ampere Altra Dev Kit, 128 cores) en op QEMU -M virt met
// EDK2-firmware — beide via exact dezelfde weg: BOOTAA64.EFI op een
// FAT-medium (USB-stick op de Altra, vvfat in QEMU; image/uefi-run.sh).
//
// Wat hij bewijst, in oplopende waarde:
//
//  1. de PE-stub werkt: firmware-banner, AllocatePages op onze partitie,
//     ExitBootServices, relocatie naar het linkadres — dít is "werkende
//     UEFI-boot";
//  2. de tamago-runtime draait en de console komt uit ACPI SPCR — elke
//     verdere regel is bewijs;
//  3. de discovery: boot-EL (2 = stage-2-kooi mogelijk, de HopOS-eis),
//     UEFI-memory-map (RAM-totaal), MADT (cores + MPIDR's — op de Altra
//     hoort hier 128 te staan), MCFG (PCIe-ECAM + enumeratie: dáár hangen
//     de i210/X550-NIC's), SPCR, FADT/PSCI (conduit voor CPU_ON in fase 2).
//
// Elke stap kondigt zich aan vóór de mogelijk-fatale actie (probe6-stijl):
// bevriest de console, dan wijst de laatste regel de dader aan.
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe" // go:linkname (RAM-declaratie)

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/cpu/psci"
	"github.com/xinix00/HopOS/metal/v2/cpu/trng"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/HopOS/metal/v2/driver/scmi"
	"github.com/xinix00/HopOS/metal/v2/net/netdev"
)

// rxbuf: één frame, om tijdens de interruptmeting de ring leeg te houden.
var rxbuf [netdev.MTU + netdev.EthernetMaximumSize]byte

// RAM-declaratie: RamStart wordt door mkkernel -pe per venster-variant
// gepatcht (0 = onverpakt, de stub weigert dan); de stub claimt GoRAMSize
// plus de plan-carve — zie board/uefi.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = uefi.GoRAMSize

func main() {
	// De SBSA-watchdog overnemen en in leven houden (O6N 09-09: de firmware
	// laat hem gewapend achter; Linux' watchdog-core doet hetzelfde vanaf de
	// probe). Zonder dit sterft de probe na de firmware-timeout — stil.
	if desc, ok := uefi.WatchdogArm(12 * time.Second); ok {
		say("watchdog: armed and petted every 3s (%s)\n", desc)
		go func() {
			for {
				time.Sleep(3 * time.Second)
				uefi.WatchdogPet()
			}
		}()
	} else {
		say("watchdog: %s\n", desc)
	}
	say("\nprobeuefi: %s on bare metal — UEFI/ACPI discovery\n", runtime.Version())
	say("boot EL: %d (HopOS requires 2: EL2 = the stage-2 cage)\n", uefi.BootEL())
	say("core: MADT index %d, MPIDR %#x, RAM window %#x+%#x (stub-selected), SystemTable %#x\n",
		uefi.CoreID(), dev.MPIDR()&0xffffff, uefi.Base(), uefi.KernelSize, uefi.SystemTable())
	say("board: %s\n", board.Current().Firmware())

	// UEFI-memory-map: de RAM-waarheid (door de stub gesnapshot vóór
	// ExitBootServices).
	mm := uefi.MemoryMap()
	var conv, other uint64
	var regions int
	for _, d := range mm {
		if d.Type == uefi.EfiConventionalMemory {
			conv += d.Pages * 4096
			regions++
		} else {
			other += d.Pages * 4096
		}
	}
	say("memmap: %d descriptors, free RAM %d MB in %d regions, other %d MB\n",
		len(mm), conv>>20, regions, other>>20)
	// De grootste vrije regio's: waar de app-slots straks kunnen wonen.
	for _, d := range mm {
		if d.Type == uefi.EfiConventionalMemory && d.Pages >= 1<<16 { // ≥256MB
			say("  free: %#x + %d MB\n", d.Start, d.Pages*4096>>20)
		}
	}

	t := uefi.Tables()
	if t == nil {
		say("ACPI: PARSE FAILED — no RSDP/XSDT\n")
		hang()
	}
	say("ACPI: rev %d, OEM %q, tables: %v\n", t.Revision, t.OEMID, t.Sigs)

	if c, err := t.Console(); err == nil {
		if uefi.Reachable(c.Base, 0x1000) {
			say("SPCR: UART %#x type %#x (0x03=PL011, 0x0e=SBSA, 0x00/0x12=16550) stride 1<<%d — active (48-bit VA hook)\n", c.Base, c.IfType, c.Shift)
		} else {
			say("SPCR: UART %#x unreachable (MapHigh failed) — serial OFF, screen is the console\n", c.Base)
		}
	}

	// MADT: de cores. Op QEMU = -smp; op de Altra horen hier 128 GICC's.
	if cpus, gicd, err := t.MADT(); err == nil {
		on := 0
		for _, c := range cpus {
			if c.Enabled {
				on++
			}
		}
		gicr, gicrLen, its := t.GIC()
		say("MADT: %d cores (%d enabled), GICD %#x, GICR range %#x+%#x, ITS %#x\n", len(cpus), on, gicd, gicr, gicrLen, its)
		show := len(cpus)
		if show > 16 {
			show = 16
		}
		for i, c := range cpus[:show] {
			say("  [%d] uid=%d mpidr=%#x enabled=%v eff-class=%d gicr=%#x\n", i, c.UID, c.MPIDR, c.Enabled, c.EffClass, c.GICR)
		}
		if len(cpus) > show {
			say("  ... and %d more\n", len(cpus)-show)
		}
		// De app-cores zoals het board ze adverteert (zonder de eigen core),
		// met de klasse die de placement straks ziet.
		k := board.Current().Cores()
		app := k.App()
		say("app cores: %d — ", len(app))
		for i := range app {
			say("%d:%s ", app[i], board.Current().CoreClass(i+1))
		}
		say("\n")
	} else {
		say("MADT: %v\n", err)
	}

	// FADT: het PSCI-conduit — en meteen de proef op de som met een
	// PSCI_VERSION-call (fase 2 start hiermee cores).
	if ok, hvc, err := t.PSCI(); err == nil {
		say("FADT: PSCI=%v conduit=%s\n", ok, map[bool]string{true: "HVC", false: "SMC"}[hvc])
		if ok && !hvc {
			// PSCI_VERSION (0x84000000): major[31:16].minor[15:0] — de
			// eerste echte firmware-call, fase 2 (CPU_ON) leunt hierop.
			v := psci.SMC(0x84000000, 0, 0, 0)
			say("PSCI: version %d.%d (live SMC call works)\n", (v>>16)&0xffff, v&0xffff)
		}
	} else {
		say("FADT: %v\n", err)
	}

	// _CPC (CPPC) uit de DSDT-AML: de perf-grenzen en het desired-perf-
	// register per processor — op de O6N de SCMI-fastchannel waarmee de klok
	// gezet wordt. Geen _CPC = de klok is firmware-domein (Altra).
	if cpcs := t.CPCs(); len(cpcs) > 0 {
		say("CPPC: %d _CPC objects\n", len(cpcs))
		for _, c := range cpcs {
			say("  %v\n", c)
		}
	} else {
		say("CPPC: no _CPC in the DSDT/SSDTs (clock is firmware-managed)\n")
	}

	// SBSA-watchdog (GTDT): alleen melden; wapenen is agent-werk.
	if refresh, control, found := t.Watchdog(); found {
		say("GTDT: SBSA watchdog refresh %#x control %#x (WOR now %#x)\n", refresh, control,
			func() uint32 {
				if uefi.MapHigh(control, 0x1000) {
					return dev.Read32(uintptr(control) + 8)
				}
				return 0
			}())
	} else {
		say("GTDT: no SBSA watchdog\n")
	}

	// SCMI-sensoren (Cix P1): het AML-kanaal van de DSDT (PMMX, 0x065d0000).
	// Alleen op CIXTEK-firmware: een doorbell op een vreemd adres is geen
	// meting maar een gok.
	if t.OEMID == "CIXTEK" {
		scmiProbe()
	}

	// De thermometer van het board (board.Thermometer): wat de agent straks op
	// zijn heartbeat zet.
	if mC := board.TempMilliC(); mC != 0 {
		say("hwmon: board thermometer %d.%dC\n", mC/1000, mC%1000/100)
	} else {
		say("hwmon: no board thermometer\n")
	}

	// RNG: welke entropiebron heeft de node? De runtime koos er bij boot al
	// één (initRNG, uefi.RNGSource); hier meten we bovendien live of hij
	// bytes levert. rndr = FEAT_RNG-instructie (O6N), smccc-trng = firmware
	// DEN 0098 (Altra/TF-A), jitter = geen hardware → timing-DRBG.
	var sample [16]byte
	if src, ok := trng.Fill(sample[:]); ok {
		say("RNG: hardware source=%s (runtime uses %s) sample=%x\n", src, uefi.RNGSource(), sample)
	} else {
		say("RNG: no hardware TRNG (RNDR absent, SMCCC unsupported) — jitter-DRBG (runtime uses %s)\n", uefi.RNGSource())
	}

	// MCFG: PCIe-ECAM — enumereer de firmware-geconfigureerde hiërarchie van
	// elk segment (read-only: op een server hangen de NIC's achter
	// root-poorten, niet op bus 0). Op de Altra verschijnen hier de i210's.
	if ecams, err := t.MCFG(); err == nil {
		for _, e := range ecams {
			say("MCFG: segment %d bus %d-%d ECAM %#x\n", e.Segment, e.StartBus, e.EndBus, e.Base)
			if base, size := uefi.ECAMWindow(e); !uefi.MapHigh(base, size) {
				say("  ECAM unreachable (MapHigh failed) — segment skipped\n")
				continue
			}
			win := pcie.Window{ECAMBase: uintptr(e.Base)}
			for _, d := range pcie.ScanConfigured(win, int(e.StartBus)) {
				tag := ""
				switch d.Class >> 16 {
				case 0x02:
					tag = "  <-- NETWORK"
				case 0x01:
					tag = "  <-- STORAGE"
				}
				bars := ""
				if d.Class>>16 == 0x02 || d.Class>>16 == 0x01 {
					bars = fmt.Sprintf(" BAR0 %#x BAR2 %#x", d.BAR(0), d.BAR(2))
				}
				say("  %v%s%s\n", d, bars, tag)
			}
		}
	} else {
		say("MCFG: %v\n", err)
	}

	// NVMe: de opslag zoals de agent hem straks pakt (board.Disk: hiërarchie-
	// scan, BAR0, controller-init, identify) — leest de schijf niet.
	if bd, ok := board.Current().(interface {
		Disk() (*nvme.Controller, uint64, uint64, error)
	}); ok {
		say("nvme: probing (hierarchy scan, BAR0, admin queue, identify)...\n")
		if disk, first, count, err := bd.Disk(); err != nil {
			say("nvme: %v\n", err)
		} else {
			say("nvme: %q, %d MB, LBA %d..%d, block %d — controller path complete\n",
				disk.Model, count*disk.BlockSize>>20, first, first+count-1, disk.BlockSize)
		}
	}

	// De NIC-proef: het volledige datapad in één meting via het board zelf
	// (driver-tabel: igb of RTL8126) — reset, MAC, ringen, link, en dan DHCP
	// de kabel op (probe6-recept: een lease bewijst TX én RX én de
	// bus-mastering in één keer).
	say("net: probing (MCFG scan, driver, reset, rings, link, DHCP)...\n")
	if nic, mac, err := board.Current().ProbeNIC(); err != nil {
		say("net: %v\n", err)
	} else if nic == nil {
		say("net: no supported NIC found — driver test skipped\n")
	} else {
		nc := board.Current().Net()
		say("net: LEASE %s gw %s dns %s (MAC %v) — driver path complete!\n", nc.CIDR, nc.GW, nc.DNS, mac)
		// De interrupt: het board bedraadde hem in ProbeNIC (of zei waarom
		// niet). ARP/broadcast op een LAN vuurt hem binnen seconden; wij
		// wachten hoogstens 15s en melden wat er kwam.
		if w, ok := board.Current().(board.NICInterrupter); ok {
			say("irq: waiting up to 15s for the first NIC interrupt (LAN broadcast triggers it)...\n")
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) && irq.Fired() == 0 {
				w.WaitNIC(time.Second)
				for n, _ := nic.Receive(rxbuf[:]); n > 0; n, _ = nic.Receive(rxbuf[:]) {
					// ring leeg pompen: anders blijft een level-lijn staan
				}
			}
			say("irq: %d interrupt(s) claimed so far\n", irq.Fired())
		}
	}

	say("\nprobeuefi: discovery complete — heartbeat every 30s\n")

	// Bescheiden hartslag: één regel per 30s, gewoon doorschrijvend op de
	// console (geen herhaalblokken — gemeten 13-07: een 1s-hartslag veegde
	// de discovery in ~1 minuut van het scherm, en het herhaal-experiment
	// erna beviel ook niet).
	for i := 1; ; i++ {
		time.Sleep(30 * time.Second)
		fmt.Printf("probeuefi: tick %d, clock %s, irq fired %d\n", i, time.Now().UTC().Format("15:04:05"), irq.Fired())
	}
}

// say — de discovery-regels (alias voor de leesbaarheid van de meetcode).
func say(format string, args ...any) {
	fmt.Printf(format, args...)
}

// scmiProbe meet het SCMI-kanaal van de Cix-SCP: protocolversies, de
// sensorlijst en één lezing per sensor. Elke stap kondigt zich aan.
func scmiProbe() {
	const base = 0x065d0000 // Cix DSDT: device PMMX, OperationRegion MBXO
	if !uefi.MapHigh(base, 0x1000) {
		say("scmi: channel %#x unreachable\n", base)
		return
	}
	ch := &scmi.Channel{Base: base}
	say("scmi: channel %#x status %#x — asking BASE version...\n", base, dev.Read32(base+4))
	v, err := ch.Version(scmi.ProtoBase)
	if err != nil {
		say("scmi: %v\n", err)
		return
	}
	say("scmi: base protocol %d.%d\n", v>>16, v&0xffff)
	if v, err := ch.Version(scmi.ProtoSensor); err != nil {
		say("scmi: sensor protocol: %v\n", err)
		return
	} else {
		say("scmi: sensor protocol %d.%d\n", v>>16, v&0xffff)
	}
	sensors, err := ch.Sensors()
	if err != nil {
		say("scmi: sensor list: %v (got %d)\n", err, len(sensors))
	}
	for _, sn := range sensors {
		r, err := ch.Reading(sn.ID)
		if err != nil {
			say("  sensor %d %q type %d exp %d: %v\n", sn.ID, sn.Name, sn.Type, sn.Exponent, err)
			continue
		}
		if sn.Type == 2 {
			mC := sn.MilliC(r)
			say("  sensor %d %q: %d.%dC (raw %d)\n", sn.ID, sn.Name, mC/1000, mC%1000/100, r)
		} else {
			say("  sensor %d %q type %d: raw %d (x10^%d)\n", sn.ID, sn.Name, sn.Type, r, sn.Exponent)
		}
	}
}

// hang parkeert de probe na een fatale meting: de conclusie staat op de
// console, meer valt hier niet te halen.
func hang() {
	for {
		time.Sleep(time.Hour)
	}
}
