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
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/board/licheerv"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/pcie"
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

func (machine) CoreID() int { return licheerv.CoreID() }

// MemTotal: de SG2002 op dit bordje heeft 256MB DDR3, door de FSBL
// geïnitialiseerd (zijn log meldt "DDR3-2G-QFN"). Er is geen device-tree om te
// bevragen — de FSBL is de firmware en die geeft ons niks mee — dus dit is
// board-kennis. Het RAM-plan zelf staat in board/licheerv/mem.go.
func (machine) MemTotal() uint64 { return 256 << 20 }

// CoreClass: twee ongelijke kernen. HOP draait op de 1GHz C906 ("big", waar de
// FSBL ons image start), het app-slot is de 700MHz C906L ("small"). Voor de
// affinity-attributen van de agent (node.cores.small = 1) is dat precies het
// onderscheid dat de leader nodig heeft.
func (machine) CoreClass(core int) string {
	if core == 0 {
		return "big"
	}
	return "small"
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
