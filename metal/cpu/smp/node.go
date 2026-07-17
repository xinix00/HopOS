// node.go: de node-kant van HopOS' multicore-support. Waar Configure (smp.go)
// een APP meerdere cores geeft (via HOP's CtrlSMPReq-dispatch, onder een
// stage-2-kooi), geeft ConfigureNode de HOP-node-runtime zélf meerdere cores.
// Derek's model: "HOP is ook maar een app die we in cores gooien" — dus zelfde
// machinerie (goos.Task + GOMAXPROCS + dezelfde gedeelde EL2-trampoline uit
// el2/smp.s), met twee verschillen die de node ís (geen aparte codepaden):
//
//   - De node dispatcht zijn EIGEN cores: er is geen HOP-boven-HOP die op
//     CtrlSMPReq luistert. ConfigureNode krijgt daarom een dispatch-callback
//     (PSCI CPU_ON) van de kern-laag ingespoten — zo hoeft dit cpu-pakket het
//     board niet te importeren (laag-schoon), maar loopt de bring-up wél via
//     precies board.CPUOn, net als een app-core.
//   - De node draait zonder kooi (EL1, stage-2 uit): de handoff zet
//     CtrlS2Table=0, waarop de gedeelde trampoline het node-profiel kiest (HCR
//     zonder VM/TSC), en CtrlVecPA=RevokeVecPA (dezelfde EL2-vectoren als core 0
//     uit bootKernel). De gereserveerde slots (1..cores-1) lenen hun control-page
//     als handoff-scratch; apps komen daar nooit (slotmgr schuift ze weg).
//
// De secundaire node-cores delen de stage-1 van core 0 (nodeTtbr0 =
// RamStart+0x4000, identity → dezelfde node-heap), net zoals app-SMP-cores de
// stage-1 van hun primaire delen. Go's scheduler spreidt de node-goroutines
// (switch, leader, plaatsing) daarna zelf over de cores — het Go-idee.

//go:build tamago && arm64

package smp

import (
	"runtime"
	"runtime/goos"
	"sync/atomic"
	"unsafe"

	"hop-os/metal/abi/layout"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/dev"
)

var (
	nodeLastCore int    // hoogste node-core-index (cores 1..nodeLastCore, naast core 0)
	nodeNextCore int    // volgende op te brengen node-core (onder nodeBootLock)
	nodeBootLock uint32 // spinlock: één core-boot tegelijk
	nodeStub     uint64 // adres van de EL1-stub (node-image, identity → PA)
	nodeTramp    uint64 // adres van de EL2 SMP-trampoline (PSCI CPU_ON-entry)

	// nodeDispatch brengt een core op via PSCI CPU_ON: entry = de trampoline,
	// ctx = het handoff-adres (x0 bij de trampoline). Ingespoten door de kern
	// (main.go) zodat dit pakket het board niet hoeft te importeren.
	nodeDispatch func(core int, entry, ctx uint64)

	// nodeStarted telt hoeveel node-cores de runtime via nodeTask heeft
	// gedispatcht — diagnostiek (main.go logt 'm) om te bewijzen dat de extra
	// cores écht opgevraagd zijn.
	nodeStarted uint32
)

// NodeStarted geeft het aantal via nodeTask gedispatchte node-cores.
func NodeStarted() int { return int(atomic.LoadUint32(&nodeStarted)) }

// In regs_arm64.s: de actieve EL1-config van déze (de dispatchende primaire)
// core — de secundaire erft die 1-op-1, in het app-pad (smp.go) én het
// node-pad. Bewust géén afgeleide waarden (RamStart+0x4000 e.d.): de
// UEFI-basis kan de MMU-wereld al hebben uitgebreid (mmu48/extendVA → 48-bit
// L0 op een ander adres, andere TCR) en een core met de oude 39-bit-view kan
// de hoge Altra-periferie (UART/SBSA-watchdog op 16TB) niet vertalen:
// fault → watchdog-reset (gemeten 17-07, debug-kabel).
func readTTBR0() uint64
func readTCR() uint64
func readMAIR() uint64
func readVBAR() uint64

// ConfigureNode geeft de HOP-node-runtime `cores` cores (core 0 telt mee) en
// zet GOMAXPROCS zodat Go de node-goroutines over de cores spreidt. No-op bij
// cores ≤ 1 (dan blijft de node single-core, zoals altijd). Aanroepen op core 0
// ná de EL2-vectoren (stage2.InitVectors) en vóór de zware node-goroutines.
// dispatch is board.CPUOn (geïnjecteerd). De extra cores komen lazy op zodra de
// Go-scheduler een M nodig heeft (net als bij een app).
func ConfigureNode(cores int, dispatch func(core int, entry, ctx uint64)) {
	if cores <= 1 {
		return
	}
	nodeDispatch = dispatch
	nodeLastCore = cores - 1
	nodeNextCore = 1
	// De ÁCTIEVE adreswereld van core 0 (gedeeld regime, smp.go) — niet een
	// aanname daarover: TTBR0 kan mmu48's L0 zijn (48-bit, Altra). Eén keer
	// lezen volstaat: MapHigh muteert de tabél, niet deze registers, en de
	// node-cores delen VMID 0 met core 0 zodat broadcast-TLBI's hen ook
	// bereiken (smp.s).
	readRegime()
	nodeStub = el2.SMPStubPC()     // gedeelde EL1-stub (node-image → PA)
	nodeTramp = el2.S2SMPTrampPC() // gedeelde EL2-trampoline (CPU_ON-entry)
	goos.Task = nodeTask
	runtime.GOMAXPROCS(cores)
}

// nodeTask is de goos.Task-hook voor de node: de Go-runtime roept 'm aan als hij
// een extra M wil. Legt de M-context op de control-page van een gereserveerd slot
// neer (handoff-scratch), zet CtrlS2Table=0 (node-profiel: geen kooi) en
// dispatcht de core direct via PSCI. Draait in scheduler-context (m.p kan nil
// zijn): geen allocatie, geen Go-parking — enkel atomics en device-stores.
func nodeTask(sp, mp, gp, fn unsafe.Pointer) {
	for !atomic.CompareAndSwapUint32(&nodeBootLock, 0, 1) {
	}
	if nodeNextCore > nodeLastCore {
		atomic.StoreUint32(&nodeBootLock, 0)
		panic("smp: node runtime requested more OS threads than assigned cores")
	}
	c := nodeNextCore
	nodeNextCore++
	atomic.AddUint32(&nodeStarted, 1)

	// Handoff-scratch: de control-page van gereserveerd slot c (apps komen daar
	// nooit). Het gedeelde deel via writeHandoff (smp.go); daarbovenop de
	// node-profiel-velden.
	cp := layout.CtrlPagePA(c)
	writeHandoff(cp, sp, mp, gp, fn, nodeStub)
	dev.Write64(cp+layout.CtrlS2Table, 0)                          // node-profiel: geen stage-2-kooi
	dev.Write64(cp+layout.CtrlVecPA, uint64(layout.RevokeVecPA())) // EL2-vectoren (revoke), als core 0
	dev.Write64(cp+layout.CtrlSlot, 0)                             // VMID 0 = die van core 0 (TLBI-broadcast)
	dev.Write64(cp+layout.CtrlSMPMbox, 0)                          // node-cores parkeren niet
	dev.MB() // handoff zichtbaar vóór de dispatch

	nodeDispatch(c, nodeTramp, uint64(cp))
	atomic.StoreUint32(&nodeBootLock, 0)
}
