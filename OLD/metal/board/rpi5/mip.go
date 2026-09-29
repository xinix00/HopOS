package rpi5

import "github.com/xinix00/HopOS/metal/v2/dev"

// De MIP (MSI-X Interrupt Peripheral) van de BCM2712: een MSI-write van een
// PCIe-endpoint naar MIPMSIAddr wordt GIC-SPI MIPFirstSPI+data. De RP1 vuurt
// élke peripheral-interrupt zo af (zijn MSI-X-tabel wijst hierheen), dus de
// GEM-interrupt komt niet als INTx maar als vector door deze poort. Registers
// en init-volgorde naar Linux' drivers/irqchip/irq-bcm2712-mip.c en de DT
// (bcm2712.dtsi mip0: reg 0x10_0013_0000, msi-ranges GIC_SPI 128 ×64,
// dma-ranges PCIe 0xff_ffff_f000 → 0x10_0013_0000 — dat inbound-window zet
// ProbeNIC al, sinds de freeze-jacht van 13-07).
const (
	MIPBase     = 0x10_0013_0000
	MIPMSIAddr  = 0xff_ffff_f000 // het PCIe-adres dat een endpoint in zijn MSI-X-entry zet
	MIPFirstSPI = 128            // vector n → GIC SPI 128+n → INTID 160+n
	MIPVectors  = 64

	mipMaskLHost = 0x40
	mipMaskHHost = 0x50
	mipMaskLVPU  = 0x60
	mipMaskHVPU  = 0x70
	mipCfgLHost  = 0x20
	mipCfgHHost  = 0x30
)

// MIPInit zet alle 64 vectoren open naar de host (mip_probe: MASK_HOST 0,
// MASK_VPU ~0, CFG_HOST ~0).
func MIPInit() {
	dev.Write32(MIPBase+mipMaskLHost, 0)
	dev.Write32(MIPBase+mipMaskHHost, 0)
	dev.Write32(MIPBase+mipMaskLVPU, ^uint32(0))
	dev.Write32(MIPBase+mipMaskHVPU, ^uint32(0))
	dev.Write32(MIPBase+mipCfgLHost, ^uint32(0))
	dev.Write32(MIPBase+mipCfgHHost, ^uint32(0))
	dev.MB()
}

// MIPINTID geeft het GIC-INTID van MSI-vector v.
func MIPINTID(v int) int { return 32 + MIPFirstSPI + v }

// De RP1-kant van dezelfde weg (drivers/mfd/rp1.c): per peripheral-IRQ een
// MSIX_CFG-woord op RP1_PCIE_APBS_BASE (0x108000) + 0x8 + 4·irq, met de
// RP1-aliassen SET (+0x800) en CLR (+0xc00). ENABLE laat de MSI door,
// IACK_EN maakt hem level-achtig: ná één MSI wacht de RP1 tot de host IACK
// schrijft voordat hij (bij een nog staande lijn) opnieuw vuurt — precies
// het ack/rearm-ritme van de RX-pomp. RP1_INT_ETH = 6 (dt-bindings/mfd/rp1.h).
const (
	RP1IntETH = 6

	rp1MSIXCfg    = RP1Base + 0x108000 + 0x8
	rp1RegSet     = 0x800
	rp1MSIXEnable = 1 << 0
	rp1MSIXIACK   = 1 << 2
	rp1MSIXIACKEn = 1 << 3
)

// RP1MSIXEnable zet de MSI van RP1-IRQ irq aan, in IACK-modus.
func RP1MSIXEnable(irq int) {
	dev.Write32(rp1MSIXCfg+rp1RegSet+uintptr(4*irq), rp1MSIXEnable|rp1MSIXIACKEn)
	dev.MB()
}

// RP1MSIXAck laat de RP1 de lijn van irq opnieuw bekijken (rp1_chained_handle_irq).
func RP1MSIXAck(irq int) {
	dev.Write32(rp1MSIXCfg+rp1RegSet+uintptr(4*irq), rp1MSIXIACK)
	dev.MB()
}
