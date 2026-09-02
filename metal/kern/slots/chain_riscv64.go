//go:build tamago && riscv64

package slots

// ChainloadM springt in een nieuwe kern die in een geleend venster geplaatst
// is (de kern-flip, docs/kern-flip.md). Op deze architectuur draait HOP zelf
// in machine mode zonder MMU, dus dit is een kale sprong met de cache- en
// interrupt-hygiëne eromheen — zie chain_riscv64.s. Keert nooit terug.
//
// Hij woont in kern/slots en niet in kern/kernflip omdat de asm bij de laag
// hoort die de harts bezit; kernflip roept hem aan via dezelfde naad als
// el2.Chainload op ARM.
func ChainloadM(entry, x0arg uint64)
