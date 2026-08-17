package licheerv

import (
	"github.com/xinix00/HopOS/metal/board/appboard"
	"github.com/xinix00/HopOS/metal/cpu/idle"
)

func init() {
	// De tijdbasis is board-kennis: RISC-V heeft geen CNTFRQ-equivalent, dus moet
	// dit bordje zijn eigen getal aanleveren aan de lagen die eronder liggen (het
	// generieke app-board en de idle-teller). Zonder deze twee regels zouden die
	// een SG2002-frequentie als constante moeten dragen, en dan is "generiek" een
	// naam en geen eigenschap.
	appboard.TimebaseHz = RTCCLK
	idle.UseCounterHz(RTCCLK)
}

// CoreID geeft de eigen hart-index. De C906 (HOP) is hart 0, de C906L (het
// app-slot) hart 1 — mhartid van dit silicium is niet betrouwbaar als
// slotnummer, dus we leiden het af uit waar we draaien: een app-image is
// gelinkt in de app-partitie (SlotBase+), HOP eronder. Eén vergelijking, geen
// CSR-afhankelijkheid.
func CoreID() int {
	if pc() >= SlotBase {
		return 1
	}
	return 0
}

// pc geeft het huidige programmateller-adres (zie appboard_riscv64.s).
func pc() uint64

// TimerOffset/SetTimerOffset/SetWallTime: de klok-offset (wall-ns bij
// tellerstand nul) is gedeeld met de app via de control page. De teller zelf
// is de TIME CSR (rdtime): de c900-CLINT heeft géén mtime-register (gemeten
// 30-07), dus rdtime is op dit board de enige tijdbron — en tegelijk precies
// het pad dat een gekooide app óók heeft (geen CLINT-venster nodig).
func TimerOffset() int64 { return RV64.TimerOffset }

func SetTimerOffset(off int64) { RV64.TimerOffset = off }

// SetWallTime zet de offset zó dat "nu" op ns uitkomt.
func SetWallTime(ns int64) { RV64.SetTime(ns) }
