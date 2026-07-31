//go:build tamago && riscv64

package applib

import (
	"fmt"
	"unsafe"
)

// selfPlace bestaat op deze architectuur (nog) niet, en dat is een bewuste
// keuze met een nette terugval: de aanroeper laat CtrlPlaceEntry dan 0 en HOP
// plaatst de image vanaf de staging (applib.go, dezelfde route als een
// "exotische image" op ARM).
//
// Waarom niet: zelfplaatsing bestaat omdat de app op ARM zijn segmenten binnen
// zijn éigen kooi moet schuiven — HOP kan daar niet bij zonder de stage-2-map
// van dat slot te openen. Op RISC-V is dat andersom: HOP draait in M-mode
// zónder eigen PMP-beperking en mag de partitie gewoon schrijven (hij zet er
// het slot-image ook al neer), terwijl de gekooide app juist niets buiten zijn
// vensters kan. HOP-plaatsing is hier dus de simpele én de veilige route.
//
// Zodra dit wél nodig is (bijvoorbeeld een slot dat zijn eigen image bijwerkt)
// hoort hier een rv64-instructie-encoder naast abi/a64 — het stubje zelf is
// dezelfde reken-logica uit abi/place, die is arch-neutraal.
func (a *App) selfPlace(stageAddr uintptr, imgSize int64) (uint64, error) {
	_ = unsafe.Pointer(stageAddr)
	return 0, fmt.Errorf("self-placement not implemented on riscv64 (HOP places from staging)")
}
