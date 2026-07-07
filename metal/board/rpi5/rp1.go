package rpi5

// RP1-southbridge (RP1-peripherals-datasheet, RP-008370-DS): de BCM2712 ziet
// het RP1-peripheralvenster via PCIe op 0x1f_0000_0000 → RP1-adres
// 0x4000_0000 (BAR1). De VPU-firmware traint de PCIe-link al vóór onze
// kernel start (de bootloader gebruikt de RP1 zelf, o.a. voor netboot) —
// de probe verifieert dat door de sysinfo/CHIP_ID te lezen.
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
)
