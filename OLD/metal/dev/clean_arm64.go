//go:build tamago && arm64

package dev

// Clean schrijft [addr, addr+size) uit de cache naar het geheugen zonder de
// regels weg te gooien: DC CVAC per cache-regel + DSB ISH (dev_arm64.s). De
// goedkope helft van CleanInv, voor een woord dat een lezer zonder cache moet
// zien terwijl de regel zelf geldig mag blijven (share_arm64.go). riscv64 heeft
// er geen aparte variant van (clean_riscv64.go).
func Clean(addr, size uintptr)
