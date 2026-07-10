// Package qemuvirt biedt HopOS-support voor de QEMU `-M virt` arm64-machine
// (virtualization=on — HopOS eist EL2): PL011-console, generic timers, en
// PSCI (SMC-conduit, door QEMU geleverd).
//
// Dit is ons multikernel-referentiedoel in fase 1: in tegenstelling tot QEMU's
// imx8mp-evk (directe -kernel boot, geen TF-A) levert virt gegarandeerd PSCI,
// tot -smp 12, GICv3 — dezelfde bouwstenen als de Orion O6N (daar via TF-A/SMC).
//
// Alleen voor GOOS=tamago GOARCH=arm64. Het pakket levert de verplichte
// runtime/goos-hooks (zie tamago's goos-pakket): RamStart, RamStackOffset,
// Hwinit1, Printk, Nanotime, InitRNG, GetRandomData. RamSize komt uit de app.
package qemuvirt

import (
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/dev"
	"hop-os/metal/fdt"
	"hop-os/metal/idle"
	"hop-os/metal/layout"
)

// QEMU virt memory map (hw/arm/virt.c, stabiel gedocumenteerd).
const (
	UART0Base = 0x09000000 // PL011-poke via metal/pl011 (offsets/bit gedeeld)

	// revokeVecAsm moet byte-gelijk zijn aan #define REVOKE_VEC in cpuinit.s
	// (VBAR_EL2 van core 0 wordt vóór Go gezet); init() checkt de pariteit
	// met het plan hieronder.
	revokeVecAsm = 0xC2000800
)

// Het PA-plan van dit board — BEWUST verschoven t.o.v. de IPA-constanten
// (ctrl/ringen/tabellen op 0xC0000000+ terwijl apps ze op 0xB0000000 zien,
// en een pool in twee losse regio's): op QEMU is identity de valkuil die
// IPA/PA-verwisselingen verhult, dus maakt de proeftuin ze juist ongelijk —
// elke verwisseling knalt dan in de regressie i.p.v. pas op een board.
// Vereist -m 3G (image/qemu-run.sh): RAM tot 0x100000000.
func init() {
	layout.UsePlan(layout.Plan{
		CtrlPA:        0xC0000000,
		RingPA:        0xC1000000,
		Stage2PA:      0xC2000000,
		NetRingPA:     0xC3000000,
		BootScratchPA: 0xB0000000, // cpuinit-vast (BOOT_SCRATCH/DTB_PTR)
		Pool: []layout.Region{
			{Base: 0x50000000, Size: 0x60000000}, // 1,5GB — het klassieke venster
			{Base: 0xB1000000, Size: 0x0F000000}, // 240MB — bewijst multi-regio
		},
	})
	if uint64(layout.RevokeVecPA()) != revokeVecAsm {
		panic("qemuvirt: REVOKE_VEC in cpuinit.s wijkt af van het PA-plan")
	}
}

// ARM64 core-instantie (zelfde constructie als tamago's imx8mp).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

// RamStart en RamSize worden door elke image zelf gedefinieerd (HOP-kern en
// app-images hebben elk een eigen partitie — zie metal/layout); alleen de
// stack-offset is voor iedereen gelijk.
//
//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// hwinit1: post-World lagere-laag-init. QEMU zet CNTFRQ zelf, dus
// InitGenericTimers(0, 0) berekent alleen de TimerMultiplier.
//
// memTotal is het bij boot gedetecteerde DRAM (0 = onbekend). Vroeg parsen
// (hier, niet lui bij MemTotal): QEMU legt de DTB in laag RAM dat de runtime
// later hergebruikt, dus tegen main-tijd is hij weg.
var memTotal uint64

//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	ARM64.Init()
	ARM64.EnableCache()
	ARM64.InitGenericTimers(0, 0)
	idle.Enable() // ná Init (die zet de default governor)

	// Alleen de HOP-core (0) kreeg de DTB-pointer in x0 en heeft de scratch
	// leesbaar; app-cores (stage-2) mogen 0xB000_0008 niet aanraken.
	// De firmware geeft de DTB-pointer in x0 (cpuinit → layout.DTBPtr). Werkt
	// waar de firmware die conventie honoreert (TF-A op O6N, en te verifiëren
	// op de Pi); QEMU -kernel <ELF> zet x0 níét → detectie faalt en de
	// aanroeper valt terug op het statische slot-plan. Alleen core 0 heeft de
	// scratch leesbaar (app-cores: stage-2).
	if CoreID() == 0 {
		// layout.DTBPtr is het scratch-woord waarin cpuinit x0 legde; eerst
		// dereferencen naar het echte DTB-adres. Op QEMU -kernel is x0=0 →
		// Read64 geeft 0 → MemTotal(0) is (0,false) → nette fallback.
		if n, ok := fdt.MemTotal(uintptr(dev.Read64(layout.DTBPtr))); ok {
			memTotal = n
		}
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// mpidr leest MPIDR_EL1 (cpu_arm64.s).
func mpidr() uint64

// CoreID geeft de eigen core-index (MPIDR aff0; op virt 0..11). Voor een
// app-core is dit tevens het slotnummer — de enige identiteitsbron die
// onafhankelijk is van het linkadres (images zijn canoniek gelinkt en
// draaien via de stage-2-vertaling op elk slot).
func CoreID() int { return int(mpidr() & 0xFF) }
