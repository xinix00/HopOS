//go:build riscv64 && gui

package slots

import "fmt"

// De RISC-V-kooi is PMP, en dat is geen paginatabel: de vensters worden bij het
// starten van het hart in één keer geprogrammeerd en de entries zijn LOCKED —
// definitief tot de hart-reset (zie cage_riscv64.go). Er valt dus niets bij te
// mappen in een kooi die draait, en dát is precies wat een surface-grant vraagt.
//
// Geen stille no-op maar een fout, want stil zou betekenen dat de app zijn
// pixels nergens heen stuurt en de display een leeg venster toont. Met deze
// fout valt de app netjes terug op de pixel-over-de-socket-weg, die gewoon
// blijft bestaan (hij is óók het pad voor apps op een ándere node).
//
// Wat het wél mogelijk zou maken: de vensters van een GUI-app vooraf reserveren
// bij het starten van het slot, zoals GrantHooks.Window dat voor de framebuffer
// doet. Dat kost een vaste reservering per slot en is pas de moeite als er een
// RISC-V-bord met een scherm in het veld staat.

func cageMapSurface(holder, blk int, pa uint64, blocks int) error {
	return fmt.Errorf("surface grant: the PMP cage cannot map a window into a running slot (locked entries)")
}

func cageUnmapSurface(holder, blk, blocks int) error { return nil }

func cageRemapSurfaceZero(holder, blk, blocks int, zeroPA uint64) error { return nil }
