//go:build !(tamago && arm64)

package memattr

// NormalNC is buiten arm64 een no-op, en dat is geen gat maar een gevolg: op de
// andere architectuur die HopOS draait is DRAM al normaal geheugen.
//
// RISC-V: de slot-PTE's dragen expliciet de T-Head-attributen Cacheable én
// Bufferable (cpu/thead.PTECacheable/PTEBufferable, gezet in kern/cage —
// zónder die twee faultte zelfs een atomic op de eigen stack, gemeten 31-07).
// Er is daar dus geen venster dat per ongeluk device-semantiek krijgt: wij
// benoemen het attribuut per mapping, terwijl tamago's arm64-identity-map álles
// buiten de RAM-declaratie ongevraagd Device-nGnRnE maakt. Dát verschil is de
// hele reden dat dit pakket bestaat.
//
// Zou een RISC-V-board ooit een framebuffer krijgen (board/licheerv geeft nu
// bewust géén fb.Desc af), dan is de write-combine-variant daar: Bufferable
// laten staan en Cacheable eraf — of PBMT=NC op een core met Svpbmt. Dan hoort
// hier een echte implementatie, niet eerder: ongebruikte MMU-code die nooit op
// silicium liep is een belofte, geen fundament.
func NormalNC(va, size uintptr) error { return nil }

// NormalRO idem voor het leesvenster van een surface-grant. Op RISC-V komt het
// niet voor: de PMP-kooi kan geen venster in een draaiend hart bijmappen, dus
// kern/slots weigert de grant daar al (surfgrant_riscv64.go) en de GUI valt
// terug op pixels over de socket.
func NormalRO(va, size uintptr) error { return nil }
