package rpi5

import "hop-os/metal/dev"

// RP1-southbridge (RP1-peripherals-datasheet, RP-008370-DS): de BCM2712 ziet
// het RP1-peripheralvenster via PCIe op 0x1f_0000_0000 → RP1-adres
// 0x4000_0000 (BAR1). GEMETEN 2026-07-10 (probe5/netprobe): de firmware
// traint de PCIe-link NIET — CHIP_ID las 0xdeaddead, PHYLINKUP=DL_ACTIVE=0.
// De bootloader laadt de RP1-firmware via I²C (vóór er PCIe bestaat) en
// gebruikt de link daarna niet; het OS traint hem zelf, elke boot — Linux
// ook. Dat doet metal/brcmpcie (probe6 verifieert de sequence).
//
// DMA-richting (belangrijk voor de GEM): RP1's 40-bit bus-masters (ethernet-
// DMA) sturen adressen 0x00_0000_0000..512G 1:1 als PCIe-upstream door
// (datasheet tabel 1, "PCIe Outbound direct mapped space"). Wat daar in het
// host-DRAM landt bepaalt het inbound-window van de BCM2712-root-complex —
// Linux' conventie is PCIe 0x10_0000_0000 → DRAM 0x0. De probe dumpt de
// RC-BAR-registers zodat we de echte offset kennen (gem.Net.BusOff).
const (
	// ARM-zicht op RP1: venster + peripherals (RP1-adres - 0x40000000).
	RP1Base    = 0x1f_0000_0000
	RP1SysInfo = RP1Base + 0x00000  // CHIP_ID (+0x0), PLATFORM (+0x4)
	RP1EthBase = RP1Base + 0x100000 // Cadence GEM registerblok
	RP1EthCfg  = RP1Base + 0x104000 // clocks/control om de GEM heen

	// eth_cfg-registers (datasheet §7.1).
	EthCfgControl = RP1EthCfg + 0x00
	EthCfgStatus  = RP1EthCfg + 0x04
	EthCfgClkGen  = RP1EthCfg + 0x14 // CLKGEN: volgt standaard de MAC-snelheid
	// CLKGEN-bits: 9 TXCLKDELEN, 8 DC50, 7 ENABLE (reset 1), 6 KILL,
	// 5:4 SPEED_FROM_MAC (RO), 3 SPEED_OVERRIDE_EN, 1:0 SPEED_OVERRIDE
	// (0=10M, 1=100M, 2=1000M). Zonder override volgt de klok de MAC.

	// BCM2712-kant: de PCIe-root-complex van de RP1-link (pcie2 in de
	// Linux-DT). De MISC-registers (brcmstb-conventie, +0x4000) dragen de
	// inbound-window-configuratie; de probe dumpt RC_BAR2_CONFIG_LO/HI om
	// de DMA-offset te leren.
	PCIe2Base      = 0x10_0012_0000
	RCBar2ConfigLo = PCIe2Base + 0x4034
	RCBar2ConfigHi = PCIe2Base + 0x4038

	// De gedeelde reset-infrastructuur van alle drie de PCIe-controllers
	// (metal/brcmpcie): RESCAL = het analoge kalibratieblok
	// (brcm,bcm7216-pcie-sata-rescal), één keer per boot; PCIeSWInit = de
	// brcm,brcmstb-reset SW_INIT-bank (bank = ID>>5, stride 0x18) met de
	// bridge-reset-ID's 42/43/44 voor pcie0/1/2 (uit de BCM2712-DT).
	PCIeRescal   = 0x10_0011_9500
	PCIeSWInit   = 0x10_0150_4318
	PCIe0SWInit  = 42
	PCIe1SWInit  = 43
	PCIe2SWInit  = 44

	// Outbound-CPU-vensters per controller (BCM2712-DT "ranges", 32-bit
	// non-prefetch-venster → PCIe-adres 0x0): pcie1 = de FFC (M.2/NVMe).
	PCIe1Window = 0x1b_0000_0000 // pcie2 (RP1) staat hierboven: RP1Base

	// RP1-GPIO-blokken (datasheet §3; bank 0 = pin 0-27, 1 = 28-33,
	// 2 = 34-53, bank-stride 0x4000). De ethernet-PHY-reset hangt aan
	// GPIO32 (actief-laag, bcm2712-rpi-5-b.dts: phy-reset-gpios).
	RP1IOBank0   = RP1Base + 0xd0000 // per pin: STATUS +0, CTRL +4 (stride 8)
	RP1RIOBank0  = RP1Base + 0xe0000 // OUT +0, OE +4; aliassen SET/CLR +0x2000/+0x3000
	RP1PadsBank0 = RP1Base + 0xf0000 // VOLTAGE +0, dan per pin +4 (stride 4)
)

// RP1GPIOOut zet één RP1-GPIO als software-output (funcsel 5 = sys_rio) op
// het gegeven niveau: eerst het niveau in het RIO-register, dán pas
// output-enable — geen glitch. Alleen bruikbaar bij een getrainde
// PCIe-link (metal/brcmpcie) met BAR1 op PCIe 0x0.
func RP1GPIOOut(pin int, high bool) {
	bank, off := 0, pin
	switch {
	case pin >= 34:
		bank, off = 2, pin-34
	case pin >= 28:
		bank, off = 1, pin-28
	}
	io := uintptr(RP1IOBank0 + bank*0x4000)
	rio := uintptr(RP1RIOBank0 + bank*0x4000)
	pads := uintptr(RP1PadsBank0 + bank*0x4000)

	// Pad: output-disable (bit 7) eraf, rest (drive/schmitt) laten staan.
	p := pads + 4 + uintptr(off)*4
	dev.Write32(p, dev.Read32(p)&^uint32(1<<7))
	dev.Write32(io+uintptr(off)*8+4, 5) // CTRL.FUNCSEL = sys_rio

	out := rio + 0x2000 // SET-alias van RIO_OUT
	if !high {
		out = rio + 0x3000 // CLR-alias
	}
	dev.Write32(out, 1<<off)
	dev.Write32(rio+0x2000+4, 1<<off) // RIO_OE aan (SET-alias)
	dev.MB()
}
