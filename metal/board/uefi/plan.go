// plan.go — het PA-plan van het uefi-board (layout.UsePlan) plus de
// MADT/asm-waarheden die basis en hop-helft delen. Het plan leeft in de
// "carve": het stuk tussen de Go-RAM (ramSize) en het einde van de stub-claim
// (CARVE_SIZE in init.s). Alle offsets zijn relatief aan Base() — de stub
// kiest het venster, het plan verhuist mee. De slot-pool ligt direct bóven de
// claim en wordt bij gebruik tegen de UEFI-memory-map geverifieerd (hwinit1).
//
// Dit staat in de BASIS (niet in board/uefi/hop): de sysTable-guard hieronder
// maakt het voor app-images een no-op, en er komt geen driver aan te pas —
// terwijl de HOP-kern het plan nodig heeft vóór main.
package uefi

import (
	"fmt"

	"hop-os/metal/abi/layout"
	"hop-os/metal/fw/acpi"
)

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

// init zet het PA-plan. ramStart is op dit punt al door mkkernel -pe gepatcht
// (DATA in de image), dus Base() is geldig in package-init — vóór main, vóór
// de runtime-hooks die het plan lezen. De board.Board-registratie zit in de
// hop-helft (board/uefi/hop); het app-contract in appboard.go.
func init() {
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

// ECAMWindow geeft het te mappen MMIO-venster van een MCFG-entry. De
// operanden éérst naar uint64: EndBus-StartBus+1 rekent anders in uint8 en
// wrapt bij bus 0-255 naar 0 (review #4 — QEMU levert precies die range).
// Gemapt wordt vanaf het eerste geadresseerde busnummer: onze config-reads
// gaan naar Base + bus<<20 met absolute busnummers.
func ECAMWindow(e acpi.ECAM) (base, size uint64) {
	return e.Base + uint64(e.StartBus)<<20, (uint64(e.EndBus) - uint64(e.StartBus) + 1) << 20
}

// madtCPUs is de core-lijst uit de MADT (alleen Enabled-cores), gevuld in
// hwinit1 (na de ACPI-parse): de bron voor CPUOn/AffinityInfo/CoreID.
var madtCPUs []acpi.CPU

// coreIDFromMADT: eigen MPIDR opzoeken in de MADT-volgorde — dé
// core-nummering van dit platform (de Altra nummert via aff1/aff2, aff0 is er
// altijd 0). Vóór de ACPI-parse (vroege runtime) is alleen core 0 actief: 0
// is dan het juiste antwoord. Zie CoreID (uefi.go).
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

// MADTCPUs geeft de core-lijst aan de hop-helft (PSCI-targets, core-telling).
func MADTCPUs() []acpi.CPU { return madtCPUs }

// slotHint wordt door slots.Start in een app-image gepatcht (symbool
// "hop-os/metal/board/uefi.slotHint"): het slotnummer van deze start.
// 0 = niet gepatcht (HOP-kern, of een image buiten slots om). Moet in dít
// pakket blijven wonen — de symboolnaam is deel van het laad-contract.
var slotHint uint64

// Door bootKernel (init.s) geschreven asm-waarheden voor de pariteitscheck
// in init(): het werkelijke VBAR_EL2-doel en de #define-waarden.
var (
	vbarEL2Val   uint64
	carveSizeAsm uint64
	memmapCapAsm uint64
)
