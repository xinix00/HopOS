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

	"hop-os/metal/board"
	"hop-os/metal/board/uefi"
	"hop-os/metal/cpu/psci"
	"hop-os/metal/cpu/trng"
	"hop-os/metal/driver/nic/igb"
	"hop-os/metal/driver/pcie"
	"hop-os/metal/net/dhcp"
)

// RAM-declaratie: RamStart wordt door mkkernel -pe per venster-variant
// gepatcht (0 = onverpakt, de stub weigert dan); de stub claimt GoRAMSize
// plus de plan-carve — zie board/uefi.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = uefi.GoRAMSize

func main() {
	say("\nprobeuefi: %s on bare metal — UEFI/ACPI discovery\n", runtime.Version())
	say("boot EL: %d (HopOS requires 2: EL2 = the stage-2 cage)\n", uefi.BootEL())
	say("core: %d, RAM window %#x+%#x (stub-selected), SystemTable %#x\n",
		uefi.CoreID(), uefi.Base(), uefi.KernelSize, uefi.SystemTable())

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

	if base, ifType, err := t.SPCR(); err == nil {
		if uefi.Reachable(base, 0x1000) {
			say("SPCR: UART %#x type %#x (0x03=PL011, 0x0e=SBSA) — active (48-bit VA hook)\n", base, ifType)
		} else {
			say("SPCR: UART %#x unreachable (MapHigh failed) — serial OFF, screen is the console\n", base)
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
		say("MADT: %d cores (%d enabled), GICD %#x\n", len(cpus), on, gicd)
		show := len(cpus)
		if show > 8 {
			show = 8
		}
		for _, c := range cpus[:show] {
			say("  cpu uid=%d mpidr=%#x enabled=%v\n", c.UID, c.MPIDR, c.Enabled)
		}
		if len(cpus) > show {
			say("  ... and %d more\n", len(cpus)-show)
		}
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
	var nic *pcie.Device
	if ecams, err := t.MCFG(); err == nil {
		for _, e := range ecams {
			say("MCFG: segment %d bus %d-%d ECAM %#x\n", e.Segment, e.StartBus, e.EndBus, e.Base)
			if base, size := uefi.ECAMWindow(e); !uefi.MapHigh(base, size) {
				say("  ECAM unreachable (MapHigh failed) — segment skipped\n")
				continue
			}
			win := board.PCIeWindow{ECAMBase: uintptr(e.Base)}
			for _, d := range pcie.ScanConfigured(win, int(e.StartBus)) {
				tag := ""
				switch d.Class >> 16 {
				case 0x02:
					tag = "  <-- NETWORK"
				case 0x01:
					tag = "  <-- STORAGE"
				}
				say("  %v%s\n", d, tag)
				if nic == nil && d.VendorID == 0x8086 && igb.Supported(d.DeviceID) {
					nic = d
				}
			}
		}
	} else {
		say("MCFG: %v\n", err)
	}

	// De igb-proef: het volledige datapad in één meting — reset, MAC uit de
	// NVM, link, ringen, en dan DHCP de kabel op (probe6-recept: een lease
	// bewijst TX én RX én de bus-mastering in één keer).
	if nic != nil {
		igbProbe(nic)
	} else {
		say("igb: no known igb NIC found — driver test skipped\n")
	}

	say("\nprobeuefi: discovery complete — heartbeat every 30s\n")

	// Bescheiden hartslag: één regel per 30s, gewoon doorschrijvend op de
	// console (geen herhaalblokken — gemeten 13-07: een 1s-hartslag veegde
	// de discovery in ~1 minuut van het scherm, en het herhaal-experiment
	// erna beviel ook niet).
	for i := 1; ; i++ {
		time.Sleep(30 * time.Second)
		fmt.Printf("probeuefi: tick %d, clock %s\n", i, time.Now().UTC().Format("15:04:05"))
	}
}

// say — de discovery-regels (alias voor de leesbaarheid van de meetcode).
func say(format string, args ...any) {
	fmt.Printf(format, args...)
}

// igbProbe meet de hele driver op één NIC: BAR uit de firmware-config,
// reset/MAC/link, ringen in het NIC-DMA-venster, en DHCP als
// alles-in-één-bewijs. Elke stap kondigt zich aan vóór de actie
// (probe6-stijl: bevriest er iets, dan wijst de laatste regel de dader aan).
func igbProbe(d *pcie.Device) {
	bar := d.BAR(0)
	say("igb: %v — BAR0 %#x, enabling bus mastering\n", d, bar)
	if bar == 0 {
		say("igb: firmware assigned no BAR0 — stop\n")
		return
	}
	if !uefi.MapHigh(bar, 0x20000) {
		say("igb: BAR0 unreachable (MapHigh failed) — stop\n")
		return
	}
	d.Enable()

	nic := &igb.Net{Base: uintptr(bar)}
	say("igb: reset...\n")
	if err := nic.Reset(); err != nil {
		say("igb: %v\n", err)
		return
	}
	say("igb: MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		nic.MAC[0], nic.MAC[1], nic.MAC[2], nic.MAC[3], nic.MAC[4], nic.MAC[5])

	say("igb: link (SLU + PHY autoneg)...\n")
	speed, fd, err := nic.LinkUp(8 * time.Second)
	if err != nil {
		say("igb: %v\n", err)
		return
	}
	say("igb: link %dMbps full-duplex=%v\n", speed, fd)

	// NIC-DMA-venster: direct boven onze RAM-partitie — buiten de
	// RAM-declaratie (device-gemapt → ongecachet → coherent, de
	// HopOS-conventie), maar wél eerst tegen de UEFI-memory-map bewijzen
	// dat het conventioneel RAM is (de firmware claimde het niet voor ons).
	const dmaSize = 0x100000 // 1MB; de ringen+buffers vragen ~180KB
	dmaBase := uefi.Base() + uefi.KernelSize
	if !uefi.IsUsableRAM(uint64(dmaBase), dmaSize) {
		say("igb: %#x+%#x is geen vrij RAM volgens de memory-map — stop\n", dmaBase, dmaSize)
		return
	}
	say("igb: rings at %#x, enabling RX/TX...\n", dmaBase)
	if err := nic.Init(dmaBase, dmaSize); err != nil {
		say("igb: %v\n", err)
		return
	}

	say("igb: DHCP (proves TX+RX+DMA in one)...\n")
	lease, err := dhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		say("igb: %v\n", err)
		return
	}
	say("igb: LEASE %d.%d.%d.%d/%v gw %d.%d.%d.%d — driver path complete!\n",
		lease.IP[0], lease.IP[1], lease.IP[2], lease.IP[3], mask(lease.Mask),
		lease.GW[0], lease.GW[1], lease.GW[2], lease.GW[3])
}

// mask telt de prefixlengte van een dotted mask (alleen voor de print).
func mask(m [4]byte) int {
	n := 0
	for _, b := range m {
		for ; b&0x80 != 0; b <<= 1 {
			n++
		}
	}
	return n
}

// hang parkeert de probe na een fatale meting: de conclusie staat op de
// console, meer valt hier niet te halen.
func hang() {
	for {
		time.Sleep(time.Hour)
	}
}
