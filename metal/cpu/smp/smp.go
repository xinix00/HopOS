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

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/cpu/el2"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/dev"
)

var (
	primaryCtrl uintptr // control page van de primaire — de handoff-scratch. Komt
	// van de aanroeper (applib) i.p.v. uit een slotnummer: de control page ligt in
	// de staart van de eigen partitie (layout: de slot-ABI), en die basis kent
	// alleen de app zelf (RamStart/RamSize).
	lastCore int    // hoogste core-index van de app (primair + secundairen)
	stubIPA  uint64 // app-IPA van de EL1-stub (ELR-doel na de trampoline)

	nextCore int    // volgende op te brengen secundaire core (onder bootLock)
	bootLock uint32 // spinlock: één core-boot tegelijk (één handoff-venster)
)

// regime is het geërfde EL1-vertaalregime {ttbr0, tcr, mair, vbar} van de
// dispatchende primaire — één keer gelezen van de levende registers
// (Configure of ConfigureNode; per binary draait er precies één), door beide
// handoff-schrijvers gedeeld. Van de bron gelezen, niet afgeleid: de node-
// primaire kan mmu48's 48-bit-wereld draaien (Altra), de app-primaire draait
// tamago's InitMMU-waarden — de secundaire erft wat er wérkelijk staat.
var regime struct{ ttbr0, tcr, mair, vbar uint64 }

func readRegime() {
	regime.ttbr0 = readTTBR0()
	regime.tcr = readTCR()
	regime.mair = readMAIR()
	regime.vbar = readVBAR()
}

// writeHandoff schrijft de M-context + het regime op een control-page — het
// gedeelde deel van de handoff naar de EL2-trampoline/EL1-stub. cp is de basis
// zoals de schrijver hem ziet (IPA voor een app, PA voor de node); stub is het
// ELR-doel in de eigen adresruimte.
func writeHandoff(cp uintptr, sp, mp, gp, fn unsafe.Pointer, stub uint64) {
	dev.Write64(cp+layout.CtrlSMPSp, uint64(uintptr(sp)))
	dev.Write64(cp+layout.CtrlSMPMp, uint64(uintptr(mp)))
	dev.Write64(cp+layout.CtrlSMPG0, uint64(uintptr(gp)))
	dev.Write64(cp+layout.CtrlSMPFn, uint64(uintptr(fn)))
	dev.Write64(cp+layout.CtrlSMPStub, stub)
	dev.Write64(cp+layout.CtrlSMPTtbr0, regime.ttbr0)
	dev.Write64(cp+layout.CtrlSMPTcr, regime.tcr)
	dev.Write64(cp+layout.CtrlSMPMair, regime.mair)
	dev.Write64(cp+layout.CtrlSMPVbar, regime.vbar)
	// Naar DRAM, niet alleen naar onze cache: de lezer is de EL2-trampoline
	// van de nieuwe core, en die leest met de MMU UIT — dus langs élke cache
	// heen. Een cacheable store van deze core is voor een andere core
	// coherent, maar niet voor een niet-cacheable lees; op QEMU (geen
	// cachemodel) viel dat nooit op, op de M4 leest de tweede core dan de
	// vorige inhoud van de page: garbage als sp/g0/ttbr0 (02-09). Eén veeg
	// over het hele SMP-venster van de control-page (0x40..0xFF: Vbar op
	// 0x78, Sp..Ttbr0 op 0x88..0xB0, Tcr 0xD8, Mair 0xF8).
	dev.CleanInv(cp+0x40, 0xC0)
}

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
func Configure(prim, cores int, ctrl uintptr) {
	if cores <= 1 {
		return
	}
	primaryCtrl = ctrl
	lastCore = prim + cores - 1
	nextCore = prim + 1
	// De EL1-stub is ons eigen symbool (cpu/el2 smp.s, in élk app-image
	// gelinkt) — op elk board hetzelfde, dus rechtstreeks en zonder board-omweg.
	stubIPA = el2.SMPStubPC()

	// Het EL1-vertaalregime van de primaire, gelezen van de levende registers —
	// de secundaire erft het 1-op-1 (de stub zet ze blind), dus hij deelt exact
	// de VA→IPA-map van de primaire (en de stage-2 legt de IPA op dezelfde
	// partitie) = gedeelde heap.
	readRegime()

	// goos.Idle laten we met rust: de primaire core zette 'm al op de
	// WFE-governor (metal/cpu/idle, via hwinit1). Die parkeert een idle core met WFE
	// en leunt op de ARM event-stream om ~elke ms weer te kijken — laag vermogen,
	// geen interrupt (dus geen botsing met de EL2-kill-route). De secundaire core
	// sloeg hwinit over, dus zijn per-core event-stream zet de SMP-stub aan
	// (CNTKCTL_EL1); daarmee wekt zijn WFE net zo goed.
	goos.Task = task
	runtime.GOMAXPROCS(cores)
	// De idle-governor mag een core nu hoogstens één ms laten slapen: op EL2
	// is hij voor de runtime onbereikbaar, en met een tweede core is er
	// iemand die op hem wacht (idle.wakeAt).
	idle.MultiCore(true)
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

	cp := primaryCtrl
	writeHandoff(cp, sp, mp, gp, fn, stubIPA)
	dev.MB() // handoff zichtbaar vóór het verzoek

	// Verzoek: HOP's servicer ziet CtrlSMPReq, dispatcht de core (naar de
	// SMP-trampoline, die de handoff hierboven oppikt) en zet 'm weer op 0.
	dev.Write64(cp+layout.CtrlSMPReq, uint64(sec))
	dev.MB()
	for dev.Read64(cp+layout.CtrlSMPReq) != 0 {
	}
	atomic.StoreUint32(&bootLock, 0)
}
