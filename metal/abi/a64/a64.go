// Package a64 bevat de minimale AArch64-instructie-encoders die HopOS'
// codegeneratoren delen: de EL2-vectorgenerator (kern/stage2) en de
// zelfplaats-stub (app/applib). Eén bron van waarheid per encoding (ARM ARM
// C6.2) — vóór dit pakket leefden movz/movk op twee plekken.
package a64

// Movz codeert movz Xd, #imm16, lsl #shift (shift ∈ {0,16,32,48}).
func Movz(rd, imm16, shift uint32) uint32 {
	return 0xD2800000 | (shift/16)<<21 | (imm16&0xFFFF)<<5 | rd&0x1F
}

// Movk codeert movk Xd, #imm16, lsl #shift.
func Movk(rd, imm16, shift uint32) uint32 {
	return 0xF2800000 | (shift/16)<<21 | (imm16&0xFFFF)<<5 | rd&0x1F
}

// StrX codeert str Xt, [Xn, #off] (off veelvoud van 8, 64-bit).
func StrX(rt, rn, off uint32) uint32 {
	return 0xF9000000 | (off/8)<<10 | (rn&0x1F)<<5 | rt&0x1F
}

// Mov64 genereert movz+movk('s) die de volledige 64-bit constante v in
// x<rd> laden; nulhelften worden overgeslagen.
func Mov64(code []uint32, rd uint32, v uint64) []uint32 {
	code = append(code, Movz(rd, uint32(v&0xFFFF), 0))
	for sh := uint32(16); sh < 64; sh += 16 {
		if part := uint32(v >> sh & 0xFFFF); part != 0 {
			code = append(code, Movk(rd, part, sh))
		}
	}
	return code
}

// Gedeelde vaste woorden (barrières/cache-onderhoud).
const (
	DSBSY   = 0xD5033F9F // dsb sy
	ISB     = 0xD5033FDF // isb
	ICIALLU = 0xD508751F // ic iallu
)
