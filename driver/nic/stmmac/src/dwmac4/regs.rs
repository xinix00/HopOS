//! Het registerblok van de DWMAC4: de MAC, de MTL-laag en de DMA met
//! kanaal 0, op de offsets uit Linux' `dwmac4.h` en `dwmac4_dma.h`.

use core::mem::{offset_of, size_of};
use dev::Reg;

/// De MAC-, MTL- en DMA-registers vanaf de basis.
#[repr(C)]
pub struct Regs {
    /// GMAC_CONFIG.
    pub(crate) config: Reg<u32>,
    _r0: u32,
    /// GMAC_PACKET_FILTER.
    pub(crate) packet_filter: Reg<u32>,
    _r1: [u32; 37],
    /// GMAC_RXQ_CTRL0: welke RX-queues aan staan.
    pub(crate) rxq_ctrl0: Reg<u32>,
    _r2: [u32; 27],
    /// GMAC_VERSION: snpsver [7:0], userver [15:8].
    pub(crate) version: Reg<u32>,
    _r3: [u32; 3],
    /// GMAC_HW_FEATURE1: de FIFO-maten.
    pub(crate) hw_feature1: Reg<u32>,
    _r4: [u32; 55],
    /// GMAC_MDIO_ADDR.
    pub(crate) mdio_addr: Reg<u32>,
    /// GMAC_MDIO_DATA.
    pub(crate) mdio_data: Reg<u32>,
    _r5: [u32; 62],
    /// GMAC_ADDR_HIGH(0).
    pub(crate) addr0_hi: Reg<u32>,
    /// GMAC_ADDR_LOW(0).
    pub(crate) addr0_lo: Reg<u32>,
    _r6: [u32; 257],
    /// MMC_RX_INTR_MASK (MMC op 0x700 vanaf de 4.x, Linux `MMC_GMAC4_OFFSET`).
    pub(crate) mmc_rx_mask: Reg<u32>,
    /// MMC_TX_INTR_MASK.
    pub(crate) mmc_tx_mask: Reg<u32>,
    _r6b: [u32; 59],
    /// MMC_RX_IPC_INTR_MASK.
    pub(crate) mmc_ipc_mask: Reg<u32>,
    _r6c: [u32; 319],
    /// MTL_CHAN_TX_OP_MODE(0).
    pub(crate) mtl_tx_op_mode: Reg<u32>,
    _r7: u32,
    /// MTL_CHAN_TX_DEBUG(0), alleen voor de diagnose.
    pub(crate) mtl_tx_debug: Reg<u32>,
    _r8: [u32; 9],
    /// MTL_CHAN_RX_OP_MODE(0).
    pub(crate) mtl_rx_op_mode: Reg<u32>,
    _r9: u32,
    /// MTL_CHAN_RX_DEBUG(0).
    pub(crate) mtl_rx_debug: Reg<u32>,
    _r10: [u32; 177],
    /// DMA_BUS_MODE.
    pub(crate) dma_bus_mode: Reg<u32>,
    /// DMA_SYS_BUS_MODE: óók de AXI-config.
    pub(crate) dma_sys_bus_mode: Reg<u32>,
    /// DMA_STATUS.
    pub(crate) dma_status: Reg<u32>,
    /// DMA_DEBUG_STATUS_0.
    pub(crate) dma_debug0: Reg<u32>,
    _r11: [u32; 60],
    /// DMA-kanaal 0 (0x1100; +0x80 per kanaal).
    pub(crate) chan: Chan,
}

/// De registers van één DMA-kanaal.
#[repr(C)]
pub(crate) struct Chan {
    /// DMA_CHAN_CONTROL.
    pub(crate) control: Reg<u32>,
    /// DMA_CHAN_TX_CONTROL.
    pub(crate) tx_control: Reg<u32>,
    /// DMA_CHAN_RX_CONTROL.
    pub(crate) rx_control: Reg<u32>,
    _r0: u32,
    /// DMA_CHAN_TX_BASE_ADDR_HI.
    pub(crate) tx_base_hi: Reg<u32>,
    /// DMA_CHAN_TX_BASE_ADDR.
    pub(crate) tx_base: Reg<u32>,
    /// DMA_CHAN_RX_BASE_ADDR_HI.
    pub(crate) rx_base_hi: Reg<u32>,
    /// DMA_CHAN_RX_BASE_ADDR.
    pub(crate) rx_base: Reg<u32>,
    /// DMA_CHAN_TX_END_ADDR: de TX-tail.
    pub(crate) tx_end: Reg<u32>,
    _r1: u32,
    /// DMA_CHAN_RX_END_ADDR: de RX-tail.
    pub(crate) rx_end: Reg<u32>,
    /// DMA_CHAN_TX_RING_LEN: aantal min één.
    pub(crate) tx_ring_len: Reg<u32>,
    /// DMA_CHAN_RX_RING_LEN.
    pub(crate) rx_ring_len: Reg<u32>,
    /// DMA_CHAN_INTR_ENA.
    pub(crate) intr_ena: Reg<u32>,
    _r2: [u32; 3],
    /// DMA_CHAN_CUR_TX_DESC.
    pub(crate) cur_tx_desc: Reg<u32>,
    _r3: u32,
    /// DMA_CHAN_CUR_RX_DESC.
    pub(crate) cur_rx_desc: Reg<u32>,
    _r4: [u32; 4],
    /// DMA_CHAN_STATUS (W1C).
    pub(crate) status: Reg<u32>,
}

/// De grootte van het blok tot en met de kanaalstatus.
const REGS_SIZE: usize = size_of::<Regs>();

const _: () = {
    assert!(offset_of!(Regs, config) == 0x0000);
    assert!(offset_of!(Regs, packet_filter) == 0x0008);
    assert!(offset_of!(Regs, rxq_ctrl0) == 0x00A0);
    assert!(offset_of!(Regs, version) == 0x0110);
    assert!(offset_of!(Regs, hw_feature1) == 0x0120);
    assert!(offset_of!(Regs, mdio_addr) == 0x0200);
    assert!(offset_of!(Regs, mdio_data) == 0x0204);
    assert!(offset_of!(Regs, addr0_hi) == 0x0300);
    assert!(offset_of!(Regs, addr0_lo) == 0x0304);
    assert!(offset_of!(Regs, mmc_rx_mask) == 0x070C);
    assert!(offset_of!(Regs, mmc_tx_mask) == 0x0710);
    assert!(offset_of!(Regs, mmc_ipc_mask) == 0x0800);
    assert!(offset_of!(Regs, mtl_tx_op_mode) == 0x0D00);
    assert!(offset_of!(Regs, mtl_tx_debug) == 0x0D08);
    assert!(offset_of!(Regs, mtl_rx_op_mode) == 0x0D30);
    assert!(offset_of!(Regs, mtl_rx_debug) == 0x0D38);
    assert!(offset_of!(Regs, dma_bus_mode) == 0x1000);
    assert!(offset_of!(Regs, dma_sys_bus_mode) == 0x1004);
    assert!(offset_of!(Regs, dma_status) == 0x1008);
    assert!(offset_of!(Regs, dma_debug0) == 0x100C);
    assert!(offset_of!(Regs, chan) == 0x1100);
    assert!(offset_of!(Chan, control) == 0x00);
    assert!(offset_of!(Chan, tx_control) == 0x04);
    assert!(offset_of!(Chan, rx_control) == 0x08);
    assert!(offset_of!(Chan, tx_base_hi) == 0x10);
    assert!(offset_of!(Chan, tx_base) == 0x14);
    assert!(offset_of!(Chan, rx_base_hi) == 0x18);
    assert!(offset_of!(Chan, rx_base) == 0x1C);
    assert!(offset_of!(Chan, tx_end) == 0x20);
    assert!(offset_of!(Chan, rx_end) == 0x28);
    assert!(offset_of!(Chan, tx_ring_len) == 0x2C);
    assert!(offset_of!(Chan, rx_ring_len) == 0x30);
    assert!(offset_of!(Chan, intr_ena) == 0x34);
    assert!(offset_of!(Chan, cur_tx_desc) == 0x44);
    assert!(offset_of!(Chan, cur_rx_desc) == 0x4C);
    assert!(offset_of!(Chan, status) == 0x60);
    assert!(REGS_SIZE == 0x1164);
};

/// De CSR-klokrange voor de MDC-deler (`include/linux/stmmac.h`): de
/// "stmmaceth"-klok van de RK3566 is SCLK_GMAC1 op 125 MHz (clk-rk3568.c:
/// CLK_MAC1_2TOP van cpll_125m), dus 100-150 MHz.
pub const CSR_100_150M: u32 = 0x1;
