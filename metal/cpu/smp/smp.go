// Package smp is de app-kant van HopOS' fase-5-ondersteuning: één app-runtime
// over meerdere cores, met een gedeelde heap. Het wordt door de OS-laag
// (applib.Init) gewired, niet door app-code — de app blijft oblivious en krijgt
// simpelweg N cores "as is" (parallelle goroutines via GOMAXPROCS).
//
// Mechanisme: HOP wijst N cores toe aan de app, laadt de image in de partitie
// van de primaire core en publiceert op de control-page hoeveel cores de app
// heeft plus het fysieke adres van de EL2 SMP-trampoline. Configure zet dan de
// runtime-hook goos.Task: telkens als de Go-scheduler een extra M nodig heeft
// (er is parallel werk voor een tweede/derde core), brengt task() de volgende
// core op via PSCI CPU_ON naar die trampoline. Die core deelt de stage-2-tabel
// van de primaire → dezelfde fysieke partitie → één gedeelde heap.
//
// De weak-memory-correctheid van de scheduler/GC/channels/sync erven we van
// upstream Go (linux/arm64 is productie-SMP op zwak geheugen); wat wij leveren
// is enkel het OS-primitief "start een OS-thread op een core", plus de
// coherentie-invariant dat de core in hetzelfde inner-shareable domein zit
// (PSCI/TF-A voegt 'm toe bij CPU_ON — op QEMU automatisch).

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
	primarySlot int    // slot (= core-index) van de primaire core
	lastCore    int    // hoogste core-index van de app (primair + secundairen)
	stubIPA     uint64 // app-IPA van de EL1-stub (ELR-doel na de trampoline)
	ttbr0       uint64 // gedeelde stage-1 L1-tabel (RamStart+0x4000, IPA)

	nextCore int    // volgende op te brengen secundaire core (onder bootLock)
	bootLock uint32 // spinlock: één core-boot tegelijk (één handoff-venster)
)

// Configure wired de goos.Task-hook en zet GOMAXPROCS op het aantal cores.
// Aangeroepen door applib.Init op de primaire core, vóór er parallel werk is.
// No-op bij cores ≤ 1 (dan blijft de runtime single-core, zoals altijd) — de
// aanroeper hoeft dus niet zelf op "SMP of niet" te vertakken.
//
//   - prim:  slotnummer van de primaire core (= board.CoreID())
//   - cores: totaal aantal cores voor deze app (door HOP op de control-page gezet)
//
// De EL2-trampoline (fysiek, door HOP gepubliceerd) en de EL1-stub (eigen IPA)
// haalt Configure zelf op — de app-kant blijft oblivious.
func Configure(prim, cores int) {
	if cores <= 1 {
		return
	}
	primarySlot = prim
	lastCore = prim + cores - 1
	nextCore = prim + 1
	// De EL1-stub is ons eigen symbool (cpu/el2 smp.s, in élk app-image
	// gelinkt) — op elk board hetzelfde, dus rechtstreeks en zonder board-omweg.
	stubIPA = el2.SMPStubPC()

	// Gedeelde stage-1 L1 = de tabel die de primaire in InitMMU bouwde, op
	// RamStart+0x4000 (IPA). De secundaire core zet zijn TTBR0_EL1 hierop → hij
	// deelt exact de VA→IPA-map van de primaire (en de stage-2 legt de IPA op
	// dezelfde partitie) = gedeelde heap.
	start, _ := runtime.MemRegion()
	ttbr0 = uint64(start) + 0x4000

	// goos.Idle laten we met rust: de primaire core zette 'm al op de
	// WFE-governor (metal/cpu/idle, via hwinit1). Die parkeert een idle core met WFE
	// en leunt op de ARM event-stream om ~elke ms weer te kijken — laag vermogen,
	// geen interrupt (dus geen botsing met de EL2-kill-route). De secundaire core
	// sloeg hwinit over, dus zijn per-core event-stream zet de SMP-stub aan
	// (CNTKCTL_EL1); daarmee wekt zijn WFE net zo goed.
	goos.Task = task
	runtime.GOMAXPROCS(cores)
}

// task is de goos.Task-hook: de runtime roept 'm aan (vanuit newosproc) als hij
// een extra OS-thread wil. De app mág geen cores opbrengen — de parkeer-
// mailboxen liggen buiten elke stage-2-map, precies zodat een app dat niet kan.
// Dus: leg de M-context op de control-page en vraag HOP via CtrlSMPReq de core
// te dispatchen (HOP kiest cold-PSCI of geparkeerd-SEV). Draait in
// scheduler-context (m.p kan nil zijn): géén allocatie, géén Go-parking — enkel
// atomics en device-stores; het wachten is een spin (HOP is een andere core).
func task(sp, mp, gp, fn unsafe.Pointer) {
	// Serialiseer op het enkele handoff-venster: één core-verzoek tegelijk.
	// nextCore staat daardoor onder de lock (geen atomic nodig).
	for !atomic.CompareAndSwapUint32(&bootLock, 0, 1) {
	}
	if nextCore > lastCore {
		// Meer Ms gevraagd dan toegewezen cores — met GOMAXPROCS==cores hoort dit
		// niet te gebeuren. Zichtbaar falen i.p.v. een core stelen of stil een M
		// laten stallen.
		atomic.StoreUint32(&bootLock, 0)
		panic("smp: runtime vroeg meer OS-threads dan toegewezen cores")
	}
	sec := nextCore
	nextCore++

	cp := layout.CtrlPage(primarySlot)
	dev.Write64(cp+layout.CtrlSMPSp, uint64(uintptr(sp)))
	dev.Write64(cp+layout.CtrlSMPMp, uint64(uintptr(mp)))
	dev.Write64(cp+layout.CtrlSMPG0, uint64(uintptr(gp)))
	dev.Write64(cp+layout.CtrlSMPFn, uint64(uintptr(fn)))
	dev.Write64(cp+layout.CtrlSMPStub, stubIPA)
	dev.Write64(cp+layout.CtrlSMPTtbr0, ttbr0)
	dev.MB() // handoff zichtbaar vóór het verzoek

	// Verzoek: HOP's servicer ziet CtrlSMPReq, dispatcht de core (naar de
	// SMP-trampoline, die de handoff hierboven oppikt) en zet 'm weer op 0.
	dev.Write64(cp+layout.CtrlSMPReq, uint64(sec))
	dev.MB()
	for dev.Read64(cp+layout.CtrlSMPReq) != 0 {
	}
	atomic.StoreUint32(&bootLock, 0)
}
