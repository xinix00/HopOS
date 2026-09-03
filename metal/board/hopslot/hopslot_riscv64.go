//go:build riscv64

// De RISC-V64-helft van het generieke app-board. Zie hopslot.go voor waar dit
// pakket over gaat (de kooi ís het board); hier staat wat op deze architectuur
// anders is, en dat is precies twee dingen:
//
//   - **De tijdbasis is geen CSR.** ARM leest CNTFRQ_EL0 en berekent daaruit
//     zijn multiplier; RISC-V heeft rdtime maar geen register dat de frequentie
//     meldt — dat is board-kennis. Zolang HopOS één RISC-V-board heeft staat hij
//     hier als constante; komt er een tweede, dan hoort HOP hem op de
//     control-page te zetten (net als de klok-offset).
//   - **Geen cache-instructie in de init.** De kooi-stub van het slot heeft de
//     caches al aangezet (T-Head-CSR's) vóór hij ons binnensprong; een tweede
//     keer aanzetten is geen no-op maar een invalidatie midden in een draaiende
//     app.
package hopslot

import (
	"runtime/goos"
	_ "unsafe" // voor go:linkname

	"github.com/usbarmory/tamago/riscv64"

	"github.com/xinix00/HopOS/metal/v2/board/appboard"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	_ "github.com/xinix00/HopOS/metal/v2/cpu/slotstart" // levert cpuinit (-tags linkcpuinit)
)

// TimebaseHz is de frequentie van de TIME CSR (rdtime) — BOARD-kennis, niet
// arch-kennis: RISC-V heeft geen register waaruit je hem kunt lezen (waar ARM
// CNTFRQ_EL0 heeft), dus moet iemand het getal aanleveren.
//
// Dat "iemand" is de board-basis, via appboard.TimebaseHz. Dit pakket is het
// generieke app-board dat op élke architectuur hetzelfde hoort te zijn, dus een
// SG2002-frequentie hoorde hier niet als constante te staan — dat maakte hopslot
// stil board-specifiek.
//
// De terugval is de SG2002-waarde: vandaag het enige RISC-V-board, en een
// tijdbasis van nul is een deling door nul bij de eerste klok-lees.
const timebaseFallback = 25_000_000

func timebaseHz() uint64 {
	if hz := appboard.TimebaseHz; hz > 0 {
		return hz
	}
	return timebaseFallback
}

// RV64 is tamago's generieke RISC-V64-driver. Counter geeft nanoseconden, dus
// TimerMultiplier blijft 1.
var RV64 = &riscv64.CPU{
	Counter:         counterNs,
	TimerMultiplier: 1,
	TimerOffset:     1, // vereist vóór Init
}

// counterNs zet de TIME CSR om in nanoseconden.
func counterNs() uint64 { return rdtime() * (1_000_000_000 / timebaseHz()) }

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	// InitSupervisor en niet Init: een slot draait in S-mode zodra zijn hart dat
	// kan (de kooi-stub mret't erheen), en Init schrijft mtvec — een M-mode-CSR
	// die daar illegaal is. Waarom die verhuizing: zolang de app in M-mode zit is
	// er boven hem niets, en kan HOP het hart niet preempten en de kooi niet
	// omschakelen — één app per hart. Zie image/licheerv/stub-slot.
	//
	// Wat Init verder deed en InitSupervisor niet doet, doen we hier zelf: de
	// twee runtime-hooks. Dit pad werkt in BEIDE modi — stvec schrijven mag ook
	// vanuit M-mode — dus hetzelfde image draait op een hart met én zonder
	// supervisor-modus, en HOP hoeft er bij het plaatsen niets van te weten.
	goos.Exit = exitHalt
	goos.Idle = RV64.DefaultIdleGovernor
	RV64.InitSupervisor()
	idle.Enable() // ná de governor-hook hierboven
}

// exitHalt is de laatste terugval als de runtime afsluit: blijven staan en niets
// meer aanraken. Tamago's Init zette hier zijn eigen (niet-geëxporteerde)
// WFI-lus; die kunnen we niet aanwijzen, en een lus hier is precies wat
// applib.parkExit ook doet — met dezelfde reden: een slot hoort te wachten tot
// het ophoudt te bestaan, niet terug te keren in runtime-code die nog alloceert.
// In de praktijk overschrijft applib deze hook met het echte parkeer-contract.
func exitHalt(int32) {
	for {
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 { return RV64.GetTime() }

// rdtime leest de TIME CSR (cpu_riscv64.s).
func rdtime() uint64

// mhartid leest het hart-id (cpu_riscv64.s) — de terugval voor CoreID.
func mhartid() uint64

// CoreID geeft het eigen slotnummer: primair de slotHint die HOP bij Start in
// de image patcht (zie hopslot.go), met mhartid als terugval voor images die
// buiten slots om draaien.
func CoreID() int {
	if slotHint != 0 {
		return int(slotHint)
	}
	return int(mhartid())
}

type appBoard struct{}

func (appBoard) CoreID() int            { return CoreID() }
func (appBoard) SetTimerOffset(o int64) { RV64.TimerOffset = o }

func init() { appboard.Use(appBoard{}) }
