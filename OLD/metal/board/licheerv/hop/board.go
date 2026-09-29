// Package hop is de HOP-bedrading van het LicheeRV Nano-board: de volledige
// board.Board-implementatie. Alleen HOP-kant-binaries (cmd/) importeren deze
// helft; app-images importeren uitsluitend de basis (board/licheerv:
// runtime-hooks + appboard-contract) en linken zo nooit tegen de driverstack —
// dezelfde bronsplitsing als bij de ARM-boards.
//
// Het arch-specifieke deel van het contract (harts starten/killen via het
// reset-blok) staat in hart.go; de rest van dit bestand is de arch-neutrale
// Common-helft.
package hop

import (
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/licheerv"
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
)

// machine is de board-implementatie voor de Sipeed LicheeRV Nano (SG2002).
type machine struct{}

// init registreert dit board. Elke HOP-binary importeert deze hop-helft
// (cmd/hopos, -tags licheerv), dus board.Current() is meteen geldig.
func init() { board.Use(machine{}) }

// Conformiteit compile-time bewezen: zonder deze regel leunt het Board-
// contract puur op board.Use() at runtime en wordt een gemiste methode pas op
// het bord zichtbaar.
var _ board.Board = machine{}

// En de M-mode-switcher-kant: de comparator-afspraak per hart (hart.go).
var _ board.HartTimerer = machine{}

func (machine) CoreID() int { return licheerv.CoreID() }

// TempMilliC (board.Thermometer): de on-die TEMPSEN — rng.go zet hem bij boot
// aan (TempInit) voor zijn eigen minuut-thermometer, dus hier alleen lezen.
func (machine) TempMilliC() int { return licheerv.TempMilliC() }

// MemTotal: de SG2002 op dit bordje heeft 256MB DDR3, door de FSBL
// geïnitialiseerd (zijn log meldt "DDR3-2G-QFN"). Er is geen device-tree om te
// bevragen — de FSBL is de firmware en die geeft ons niks mee — dus dit is
// board-kennis. Het RAM-plan zelf staat in board/licheerv/mem.go.
func (machine) MemTotal() uint64 { return 256 << 20 }

// CoreClass: twee ongelijke kernen — een C906 op 1GHz ("big") en een C906L op
// 700MHz ("small"). CoreClass volgt het globale principe: HOP woont op HopHart, en de klasse
// van een logische core is dus een sommetje, geen rol-verhaal. Voor de
// affinity-attributen van de agent (node.cores.big = 1) is dit precies het
// onderscheid dat de leader nodig heeft — en meteen het antwoord op "wat
// koopt de wissel ons": een app-core die 43% sneller klokt.
func (machine) CoreClass(core int) string {
	onSmall := licheerv.HopHart == hartC906L // woont HOP op de kleine, dan zijn de apps groot
	if (core == 0) == onSmall {              // logische core 0 = HOP zelf
		return "small"
	}
	return "big"
}

// Tijd: de generieke-timer-offset leeft in de basis-helft (die de runtime-hook
// Nanotime bedient), zodat HOP en app dezelfde bron delen. Op dit board komt
// de teller uit de TIME CSR (rdtime) — de c900-CLINT heeft géén mtime-register
// (gemeten 30-07: elke read is een bus-fout), dus rdtime ís de klok.
func (machine) TimerOffset() int64       { return licheerv.TimerOffset() }
func (machine) SetTimerOffset(off int64) { licheerv.SetTimerOffset(off) }
func (machine) SetWallTime(ns int64)     { licheerv.SetWallTime(ns) }

// Het netwerk (ProbeNIC, Net, DHCPLease) staat in net.go, de SoC-glue van de
// ePHY in ephy.go.

// PCIe: de SG2002 heeft geen bruikbare PCIe-root voor ons — leeg venster.
func (machine) PCIe() pcie.Window { return pcie.Window{} }

// Framebuffer: dit bordje heeft geen firmware-framebuffer (de SG2002 heeft
// wel een display-controller, maar de FSBL zet er geen beeld op en er is geen
// GOP/simple-framebuffer om te vinden). De console is de UART.
func (machine) Framebuffer() (fb.Desc, bool) { return fb.Desc{}, false }
