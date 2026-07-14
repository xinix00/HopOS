// board.go maakt van het uefi-package een volwaardig board.Board — de brug
// waardoor cmd/hopos (agent, leader, slots, stage-2-isolatie, NAT) op élk
// UEFI/ACPI-platform draait, de Ampere Altra voorop. Alles wat een Pi-board
// uit boardkennis haalt, komt hier uit wat de firmware al vertelde: cores
// uit de MADT, RAM uit de memory-map, PCIe uit de MCFG, beeld uit GOP, en
// CPU_ON via PSCI (conduit uit de FADT — SMC, de HopOS-invariant).
//
// Het PA-plan (layout.UsePlan) leeft in de "carve": het stuk tussen de
// Go-RAM (ramSize) en het einde van de stub-claim (CARVE_SIZE in init.s).
// Alle offsets zijn relatief aan Base() — de stub kiest het venster, het
// plan verhuist mee. De slot-pool ligt direct bóven de claim en wordt bij
// gebruik tegen de UEFI-memory-map geverifieerd (hwinit1).
package uefi

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/cpu/psci"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/nic/igb"
	"hop-os/metal/driver/pcie"
	"hop-os/metal/fw/acpi"
	"hop-os/metal/net/dhcp"
)

// machine is de board-implementatie voor UEFI/ACPI-platforms.
type machine struct{}

// Carve-offsets relatief aan Base() — pariteit met init.s (CARVE_SIZE,
// REVOKE_OFF) wordt in init() afgedwongen. ramSize (Go-RAM) eindigt op
// carveOff; de stub claimt t/m carveOff+carveSize.
// De carve draagt de per-slot fysieke regio's, gedimensioneerd voor
// layout.SlotCap (128) — niet voor de runtime-MaxSlots — zodat de
// stub-claim (init.s CARVE_SIZE, compile-time) hem altijd dekt; de node
// gebruikt er min(cores,128) van. De net-ringen liggen hier níét (meer): die
// leven in de staart van de eigen partitie van elk slot (zie kern/slots
// appRAMSize) en schalen zo mee met wat er draait — daarmee kromp de carve
// van 288MB naar 32MB. HOP-kern-RAM staat op 64MB — gemeten (14-07): idle
// 5MB Sys, 7-slot-storm-piek 14MB Sys bovenop een ~17.5MB statische image
// (code + de ingebakken apploader-blob). Sinds elke app zíjn eigen image
// downloadt draagt de kern geen fetch-golf meer, dus de oude 256MB-headroom
// verviel; 64MB is ~2× de gemeten piek en past ook op kleine devices.
// Totale claim (venster): 64 + 32 = 96MB.
const (
	carveOff  = 0x04000000 // = ramSize (Go-RAM 64MB)
	carveSize = 0x02000000 // CARVE_SIZE in init.s (32MB, dekt t/m scratch)

	ctrlOff    = carveOff + 0x000000  // (SlotCap+1)×4KB, 1MB gereserveerd
	ringOff    = carveOff + 0x100000  // SlotCap×64KB = 8MB
	stage2Off  = carveOff + 0x900000  // (SlotCap+1)×64KB ≈ 8MB
	revokeOff  = carveOff + 0x900800  // REVOKE_OFF in init.s (stage2 slot-0 +0x800)
	netDMAOff  = carveOff + 0x1200000 // NetDMASize (8MB)
	scratchOff = carveOff + 0x1A00000

	poolOff = carveOff + carveSize // einde van HOP's voetafdruk (kern-RAM + carve)

	pool2M = 2 << 20 // partitie-uitlijning (stage-2-blokken zijn 2MB)
)

// init registreert het board en zet het PA-plan. ramStart is op dit punt al
// door mkkernel -pe gepatcht (DATA in de image), dus Base() is geldig in
// package-init — vóór main, vóór de runtime-hooks die het plan lezen.
func init() {
	board.Use(machine{})

	b := uint64(Base())
	if b == 0 {
		// Niet via mkkernel -pe verpakt: de stub weigert dan al te booten
		// (CBZ op RamStart); hier alleen niet panicken in go vet e.d.
		return
	}

	// Alles hieronder is HOP-kern-werk: het PA-plan en de slot-pool. Een
	// app-image entreert cpuinit op EL1 (de el2-trampoline), slaat de
	// firmware-stub over en heeft dus géén memory-map — sysTable/memmapSize
	// blijven 0 (zelfde signaal dat extendVA gebruikt). Dan is er niets te
	// plannen: de app draait onder stage-2 op zijn canonieke IPA, HOP deelt
	// de pool uit. Zonder deze guard panicte usablePool() (lege map → 0 vrij)
	// bij élke jobstart, stil (uart/fb zijn in een app-image niet gezet).
	if sysTable == 0 {
		return
	}

	// Slots volgen de ontdekte cores — geen kunstmatige limiet, de fysieke
	// grens van dit ijzer (127 op de Altra, 3 op QEMU -smp 4). madtCPUs is
	// door hwinit1 al gevuld (die draait vóór package-init). SetMaxSlots
	// klemt op layout.SlotCap, waarvoor de carve fysiek is gereserveerd.
	if n := len(madtCPUs) - 1; n > 0 {
		layout.SetMaxSlots(n)
	}
	// De slot-pool ligt BUITEN de stub-claim: eerst tegen de UEFI-memory-map
	// bewijzen wat er werkelijk vrij is (review #6 — het comment beloofde
	// dit al, de check bestond niet). "Vrij" = ná-ExitBootServices bruikbaar,
	// dus óók boot-services-geheugen (Altra: de pool ligt in
	// EfiBootServicesData — echt RAM, gemeten 14-07). Niets bruikbaar =
	// harde, leesbare stop (de console leeft al: package-init draait ná
	// hwinit1).
	pool := usablePool()
	var poolBytes uint64
	for _, r := range pool {
		poolBytes += r.Size
	}
	if poolBytes < layout.SlotStride {
		panic(fmt.Sprintf("uefi: no usable RAM for the slot pool (memory map shows only %d MB free outside the HOP footprint)", poolBytes>>20))
	}
	fmt.Printf("uefi: slot pool %d MB free RAM in %d region(s) — all reachable DRAM, no artificial cap\n",
		poolBytes>>20, len(pool))

	layout.UsePlan(layout.Plan{
		CtrlPA:        b + ctrlOff,
		RingPA:        b + ringOff,
		Stage2PA:      b + stage2Off,
		RevokeVecPA:   b + revokeOff,
		NetDMAPA:      b + netDMAOff,
		BootScratchPA: b + scratchOff,
		Pool:          pool,
	})

	// Échte asm/Go-pariteit (review #9: de oude check vergeleek een waarde
	// met zichzelf): bootKernel schrijft wat de #defines in init.s WERKELIJK
	// waren (VBAR_EL2-doel, CARVE_SIZE, MEMMAP_CAP) — drift panict hier,
	// vóór er ooit een revoke-HVC naar een verkeerde pagina vectort.
	if vbarEL2Val != 0 {
		switch {
		case vbarEL2Val != b+revokeOff:
			panic("uefi: REVOKE_OFF in init.s wijkt af van het PA-plan (VBAR_EL2-drift)")
		case carveSizeAsm != carveSize:
			panic("uefi: CARVE_SIZE in init.s wijkt af van board.go")
		case memmapCapAsm != memmapCap:
			panic("uefi: MEMMAP_CAP in init.s wijkt af van memmapCap")
		}
	}
}

// usablePool verzamelt ÁLLE ná-ExitBootServices vrije, bereikbare RAM als
// slot-pool — geen artificiële limiet (Dereks principe: wat het OS ontdekt,
// moet HOP kunnen alloceren; net als de cores→slots). De vroegere 8GB-cap
// gooide de rest weg. Bronnen:
//
//   - élke bruikbare memory-map-regio (usableRAM: conventional + boot-services
//   - loader), niet alleen de ene run direct boven de claim — vrij RAM ligt
//     ook ónder onze venster-keuze (gemeten: 0x48000000+1664MB) en voorbij de
//     oude 8GB;
//   - minus HOP's eigen voetafdruk [Base, Base+poolOff) (kern-RAM + carve; die
//     staat als LoaderData in de map en zou anders als "vrij" meetellen);
//   - geklemd op het MMU-bereik (vaLimit = 512GB; DRAM daarboven vergt 48-bit
//     VA — backlog, bewust nu niet). 2MB-uitgelijnd voor de stage-2-blokken.
//
// De partitie-allocator (metal/kern/slots/partmem) doet first-fit met coalescing
// over meerdere regio's, dus losse stukken passen zó in het plan.
func usablePool() []layout.Region {
	b := uint64(Base())
	hopEnd := b + poolOff // HOP's kern-RAM + carve: verboden
	var regs []layout.Region
	add := func(start, end uint64) {
		if end > vaLimit { // buiten het MMU-bereik (geen 48-bit VA nu)
			end = vaLimit
		}
		start = (start + pool2M - 1) &^ uint64(pool2M-1) // base omhoog
		end &^= uint64(pool2M - 1)                       // end omlaag
		if end > start && end-start >= pool2M {
			regs = append(regs, layout.Region{Base: start, Size: end - start})
		}
	}
	for _, d := range MemoryMap() {
		if !usableRAM(d.Type) {
			continue
		}
		start, end := d.Start, d.Start+d.Pages*4096
		if start >= vaLimit {
			continue
		}
		switch {
		case end <= b || start >= hopEnd: // geen overlap met de voetafdruk
			add(start, end)
		default: // overlap: het deel eronder en/of erboven blijft pool
			if start < b {
				add(start, b)
			}
			if end > hopEnd {
				add(hopEnd, end)
			}
		}
	}
	return regs
}

// SelfPlannedPool meldt dat dit board zijn slot-pool al op de gemeten vrije
// RAM heeft geplukt (init, usablePool) — de main slaat dan de RequiredRAM-
// check over (die op statische qemuvirt-adressen leunt; hier zinloos).
func (machine) SelfPlannedPool() bool { return true }

func (machine) BootEL() int { return BootEL() }

// CoreID: eigen MPIDR opzoeken in de MADT-volgorde — dé core-nummering van
// dit platform (de Altra nummert via aff1/aff2, aff0 is er altijd 0).
// Vóór de ACPI-parse (vroege runtime) is alleen core 0 actief: 0 is dan
// het juiste antwoord.
func (machine) CoreID() int { return CoreID() }

// CoreID — zie machine.CoreID; ook door de probe en applib gebruikt.
func coreIDFromMADT() int {
	own := mpidr() & 0x00FFFFFF // aff0..aff2 (aff3 speelt hier niet)
	if len(madtCPUs) == 0 {
		// App-image-context: geen ACPI, dus geen MADT om in te zoeken.
		// HOP patcht daarom bij Start het slotnummer in de image
		// (slots.Start → slotHint) — MPIDR is op servers geen slotnummer
		// (Altra: aff0 altijd 0, review #2). De aff0-terugval blijft
		// alleen voor images die buiten slots.Start om draaien (QEMU-dev).
		if slotHint != 0 {
			return int(slotHint)
		}
		return int(own & 0xFF)
	}
	for i, c := range madtCPUs {
		if c.MPIDR&0x00FFFFFF == own {
			return i
		}
	}
	return 0
}

// MemTotal: het conventionele RAM uit de boot-memory-map plus de eigen
// claim (die stond op het moment van het snapshot als LoaderData geboekt).
func (machine) MemTotal() uint64 { return MemTotal() }

// CoreClass: de Altra (en QEMU-N1) is homogeen — alles is "big".
func (machine) CoreClass(i int) string { return "big" }

func (machine) TimerOffset() int64     { return ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { ARM64.SetTime(ns) }

// PSCI: SMC-conduit (FADT bevestigt; HopOS-invariant). De core-index wordt
// via de MADT naar het MPIDR-target vertaald.
const (
	psciVersion  = 0x8400_0000
	psciCPUOff   = 0x8400_0002
	psciCPUOnSMC = 0xC400_0003 // SMC64
	psciAffInfo  = 0xC400_0004 // SMC64
)

func (machine) CPUOn(core, entry, ctx uint64) int64 {
	if int(core) >= len(madtCPUs) {
		return -2 // PSCI INVALID_PARAMS
	}
	return int64(psci.SMC(psciCPUOnSMC, madtCPUs[core].MPIDR, entry, ctx))
}
func (machine) CPUOff() int64 { return int64(psci.SMC(psciCPUOff, 0, 0, 0)) }
func (machine) AffinityInfo(core uint64) board.PowerState {
	if int(core) >= len(madtCPUs) {
		return board.PowerState(-2)
	}
	return board.PowerState(psci.SMC(psciAffInfo, madtCPUs[core].MPIDR, 0, 0))
}
func (machine) PSCIVersion() (major, minor uint16) {
	v := psci.SMC(psciVersion, 0, 0, 0)
	return uint16(v >> 16), uint16(v)
}

// ExpectedAppCores (board.CoreCountHinter): MADT-cores minus de HOP-core.
// Op de Altra 127 — slots begrenst zelf op MaxSlots/pool.
func (machine) ExpectedAppCores() int {
	if len(madtCPUs) == 0 {
		return 0
	}
	return len(madtCPUs) - 1
}

// Stage-2/SMP: board-neutraal (metal/cpu/el2), identiek aan qemuvirt.
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }
func (machine) SMPStubPC() uint64    { return el2.SMPStubPC() }

// ECAMWindow geeft het te mappen MMIO-venster van een MCFG-entry. De
// operanden éérst naar uint64: EndBus-StartBus+1 rekent anders in uint8 en
// wrapt bij bus 0-255 naar 0 (review #4 — QEMU levert precies die range).
// Gemapt wordt vanaf het eerste geadresseerde busnummer: onze config-reads
// gaan naar Base + bus<<20 met absolute busnummers.
func ECAMWindow(e acpi.ECAM) (base, size uint64) {
	return e.Base + uint64(e.StartBus)<<20, (uint64(e.EndBus) - uint64(e.StartBus) + 1) << 20
}

// lease bewaart wat ProbeNIC via DHCP ophaalde (board.LeaseHolder-contract).
var lease dhcp.Lease

// eachECAM roept fn aan voor elk bereikbaar (via MapHigh) MCFG-segment tot
// fn true geeft; meldt of er een treffer was. Eén plek voor de MCFG→ECAM-
// walk die ProbeNIC en PCIe delen. Geen ACPI/MCFG → geen treffer.
func eachECAM(fn func(win board.PCIeWindow, startBus int) bool) bool {
	t := Tables()
	if t == nil {
		return false
	}
	ecams, err := t.MCFG()
	if err != nil {
		return false
	}
	for _, e := range ecams {
		base, size := ECAMWindow(e)
		if !MapHigh(base, size) {
			// Diagnose op het scherm (geen serieel op de Altra): het
			// hoge-map-pad is op QEMU nooit geraakt, dus dít is de meting.
			ext, tcr, pr, used, max := VAStatus()
			fmt.Printf("net: ECAM %#x unreachable [%s] l0idx=%d vaExt=%v tcr=%#x parange=%d slots=%d/%d\n",
				base, MapFailReason(base), base>>39, ext, tcr, pr, used, max)
			continue
		}
		if fn(board.PCIeWindow{ECAMBase: uintptr(e.Base)}, int(e.StartBus)) {
			return true // treffer: laat dit segment gemapt (fn gebruikt het nog)
		}
		UnmapHigh(base, size) // geen treffer: blokken teruggeven zodat de pool niet volloopt
	}
	return false
}

// ProbeNIC: MCFG → hiërarchie-scan → eerste igb-familielid → reset/link →
// ringen in het NetDMA-plan → DHCP. Hoge ECAM's/BAR's gaan door MapHigh
// (Altra: boven de vlakke 512GB, gemeten 13-07).
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	var d *pcie.Device
	eachECAM(func(win board.PCIeWindow, startBus int) bool {
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
	if bar == 0 || !MapHigh(bar, 0x20000) {
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
func (machine) PCIe() board.PCIeWindow {
	var win board.PCIeWindow
	eachECAM(func(w board.PCIeWindow, _ int) bool {
		win = w
		return true // eerste bereikbare segment volstaat
	})
	return win
}

// Framebuffer: het GOP-beeld dat de stub bewaarde (hwinit1 deed fb.Init al
// voor de vroege console; de main mag opnieuw — Init is idempotent genoeg).
func (machine) Framebuffer() (fb.Desc, bool) { return gopDesc(false) }

// madtCPUs is de core-lijst uit de MADT (alleen Enabled-cores), gevuld in
// hwinit1 (na de ACPI-parse): de bron voor CPUOn/AffinityInfo/CoreID.
var madtCPUs []acpi.CPU

// slotHint wordt door slots.Start in een app-image gepatcht (symbool
// "hop-os/metal/board/uefi.slotHint"): het slotnummer van deze start.
// 0 = niet gepatcht (HOP-kern, of een image buiten slots om).
var slotHint uint64

// Door bootKernel (init.s) geschreven asm-waarheden voor de pariteitscheck
// in init(): het werkelijke VBAR_EL2-doel en de #define-waarden.
var (
	vbarEL2Val   uint64
	carveSizeAsm uint64
	memmapCapAsm uint64
)
