package licheerv

// CV181x TEMPSEN (0x030E0000) — on-die temperatuursensor, nodig om het
// effect van terugklokken te valideren (fanless case). Init-sequentie en
// formule uit de vendor-driver (linux_5.10/drivers/thermal/cv181x_thermal.c):
//
//	0x004: bit0 = en, bits[7:4] = channel-select
//	0x00c: bits[5:4] = chopsel, [7:6] = accsel, [15:8] = cyc_clkdiv
//	0x020: bits[12:0] = ch0 result
//	temp_m°C = result*1000*716/2048 − 273000
//
// Klok: clk_tempsen gate = REG_CLK_EN_0 bit 9 (osc 25MHz).

import "time"

const (
	tempBase = 0x030e0000

	tempCtl    = tempBase + 0x004
	tempCfg    = tempBase + 0x00c
	tempResult = tempBase + 0x020
	tempAuto   = tempBase + 0x064 // [23:0] auto_cycle, [31:24] auto_prediv

	// CV181x clock-blok: alleen de gate van de sensor-klok leeft hier nog
	// (de terugklok-tooling die de rest van dit blok kende is 04-08 gesloopt).
	clkBase    = 0x03002000
	clkEn0Reg  = clkBase + 0x000
	clkTempsen = 1 << 9
)

// TempInit zet de sensor aan (chop 1024T, acc 2048T, 0.5MHz cycle-clock,
// channel 0) — eerste meting is na ~enkele ms geldig.
func TempInit() {
	write32(clkEn0Reg, read32(clkEn0Reg)|clkTempsen)

	// chopsel=3, accsel=2, cyc_clkdiv=0x31 (25M/50 = 0.5MHz)
	cfg := read32(tempCfg)
	cfg = cfg&^uint32(0x30) | 0x3<<4
	cfg = cfg&^uint32(0xc0) | 0x2<<6
	cfg = cfg&^uint32(0xff00) | 0x31<<8
	write32(tempCfg, cfg)

	// De auto-cycle, en die ontbrak: zonder een periode doet de sensor ÉÉN
	// conversie bij enable en blijft het resultaatregister daarna staan. Dat is
	// precies wat we zagen (Derek, 11-08): per boot een geloofwaardige waarde
	// (52,1 / 48,9 / 40,5°C), en binnen één boot tien metingen lang exact
	// hetzelfde getal terwijl de node aan het downloaden was. Eén LSB is 0,35°C,
	// dus stilstand op die schaal is geen ruisonderdrukking maar een dood
	// register. Waarde uit de vendor-driver (cv181x_thermal.c, LicheeRV-Nano-
	// Build): 0x100000 in [23:0], prediv ongemoeid — op de 0,5MHz cycle-clock
	// (T=2µs) is dat een nieuwe meting per ~2s, ruim vaker dan wij printen.
	write32(tempAuto, read32(tempAuto)&^uint32(0xffffff)|0x100000)

	// channel 0 aan + enable
	write32(tempCtl, read32(tempCtl)&^uint32(0xf0)|0x1<<4|0x1)

	time.Sleep(10 * time.Millisecond)
}

// TempMilliC leest de die-temperatuur in milli-°C.
func TempMilliC() int {
	result := read32(tempResult) & 0x1fff
	return int(result)*1000*716/2048 - 273000
}
