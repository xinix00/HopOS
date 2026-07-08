package raspi

// De park-code voor secundaire cores (probes): meld je met '0'+ctx op de
// UART, zet een levensteken op ParkCount + ctx*8 en parkeer in een WFE-lus.
// ctx komt binnen in x0 (PSCI CPU_ON). De movz/movk-reeksen worden hier
// gegenereerd in plaats van met de hand geassembleerd — de handversie in
// probe5 bleek het UART-adres 4 bits verschoven te laden.

// ParkCode bouwt de park-instructiereeks voor een board met het gegeven
// UART-DR-adres. Planten: woord voor woord (32-bit) op ParkBase schrijven
// (dev.Write32), dan CPU_ON met entry=ParkBase en de core-index als ctx.
// uartDR = 0 slaat het UART-teken over: op de Pi 5 zonder debug-sessie is
// elke toegang tot de (mogelijk ongeklokte) PL011 verdacht — een gestalde
// bus-access kent geen timeout en zou de core dood parkeren.
func ParkCode(uartDR uint64) []uint32 {
	var code []uint32
	if uartDR != 0 {
		code = loadAddr(1, uartDR) // x1 = UART DR
		code = append(code,
			0x1100C002, // add w2, w0, #0x30   ('0' + ctx)
			0xB9000022, // str w2, [x1]
		)
	}
	code = append(code, loadAddr(3, ParkCount)...) // x3 = ParkCount
	return append(code,
		0x8B000C63, // add x3, x3, x0, lsl #3   (+ ctx*8)
		0xD2800024, // movz x4, #1
		0xF9000064, // str x4, [x3]
		0xD503205F, // wfe
		0x17FFFFFF, // b .-4 (wfe-lus)
	)
}

// loadAddr geeft de movz+movk-reeks die register x<rd> met een (max 48-bit)
// adres laadt; movk's voor 16-bit-chunks die nul zijn vervallen.
func loadAddr(rd uint32, addr uint64) []uint32 {
	ins := []uint32{0xD2800000 | uint32(addr&0xFFFF)<<5 | rd} // movz xRd, #lo16
	for hw := uint32(1); hw <= 2; hw++ {
		if chunk := uint32(addr >> (16 * hw) & 0xFFFF); chunk != 0 {
			ins = append(ins, 0xF2800000|hw<<21|chunk<<5|rd) // movk xRd, #chunk, lsl #16*hw
		}
	}
	return ins
}
