// Package slotstart draagt de CPU-init van een app-slot: het eerste dat draait
// als de kooi het hart aan de app geeft, vóór de Go-runtime.
//
// Waarom een eigen pakket en niet bij het board: op RISC-V linkt niet elk
// app-image hetzelfde board. De generieke apps gaan via board/hopslot, de
// board-eigen demo via board/licheerv — en die twee kunnen niet samen in één
// image, want ze registreren elk een appboard (appboard.Use). De
// opstart-assembly is voor beide identiek, dus staat hij hier: één definitie,
// en elk board importeert hem blanco.
//
// De arm64-helft woont nog wél bij het board (board/hopslot/cpuinit_arm64.s):
// daar is er één app-board, dus geen reden om iets te verplaatsen.
//
// Alleen van betekenis met -tags linkcpuinit — dan levert tamago zijn eigen
// cpuinit niet en moet het image er zelf een hebben. Zonder die tag is dit een
// leeg pakket.
package slotstart
