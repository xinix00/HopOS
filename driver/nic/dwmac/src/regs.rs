//! Het registerblok van de DWMAC1000: de MAC op de basis en de DMA op
//! basis + 0x1000, op de offsets uit `designware.h` (vendor U-Boot) en
//! Linux' `dwmac1000.h`/`dwmac_dma.h`.

use core::mem::{offset_of, size_of};
use dev::Reg;

/// De MAC- en DMA-registers vanaf de basis.
#[repr(C)]
pub(crate) struct Regs {
    /// MAC_CONFIG.
    pub(crate) conf: Reg<u32>,
    /// MAC_FRAME_FILTER.
    pub(crate) filter: Reg<u32>,
    _r0: [u32; 2],
    /// GMII_ADDR: het MDIO-commando.
    pub(crate) gmii_addr: Reg<u32>,
    /// GMII_DATA.
    pub(crate) gmii_data: Reg<u32>,
    _r1: [u32; 2],
    /// VERSION: snpsver in [7:0]; 0x1037 op de SG2002 (30-07).
    pub(crate) version: Reg<u32>,
    _r2: [u32; 6],
    /// GMAC_INT_MASK: de interrupts van de MAC zelf (RGMII, PCS, PMT,
    /// timestamp, LPI); 1 = dicht.
    pub(crate) int_mask: Reg<u32>,
    /// MAC_ADDR0_HI: bytes 4 en 5.
    pub(crate) addr0_hi: Reg<u32>,
    /// MAC_ADDR0_LO: bytes 0 tot en met 3.
    pub(crate) addr0_lo: Reg<u32>,
    _r3: [u32; 49],
    /// MMC_RX_INTR_MASK (MMC op 0x100 bij de 3.x, Linux `MMC_GMAC3_X_OFFSET`).
    pub(crate) mmc_rx_mask: Reg<u32>,
    /// MMC_TX_INTR_MASK.
    pub(crate) mmc_tx_mask: Reg<u32>,
    _r3b: [u32; 59],
    /// MMC_RX_IPC_INTR_MASK.
    pub(crate) mmc_ipc_mask: Reg<u32>,
    _r3c: [u32; 895],
    /// DMA_BUS_MODE.
    pub(crate) bus_mode: Reg<u32>,
    /// DMA_XMT_POLL_DEMAND: elke schrijf laat de TX-DMA de ring opnieuw
    /// lezen.
    pub(crate) tx_poll: Reg<u32>,
    /// DMA_RCV_POLL_DEMAND.
    pub(crate) rx_poll: Reg<u32>,
    /// DMA_RCV_BASE_ADDR: de RX-ring.
    pub(crate) rx_list: Reg<u32>,
    /// DMA_TX_BASE_ADDR: de TX-ring.
    pub(crate) tx_list: Reg<u32>,
    /// DMA_STATUS (W1C).
    pub(crate) status: Reg<u32>,
    /// DMA_CONTROL (operation mode).
    pub(crate) op_mode: Reg<u32>,
    /// DMA_INTR_ENA: welke DMA-status de lijn (sbd_intr) mag hoog zetten.
    pub(crate) intr_ena: Reg<u32>,
    /// DMA_MISSED_FRAME_CTR: leest én wist.
    pub(crate) missed: Reg<u32>,
    _r5: [u32; 9],
    /// DMA_CUR_TX_DESC_ADDR.
    pub(crate) cur_tx_desc: Reg<u32>,
    /// DMA_CUR_RX_DESC_ADDR.
    pub(crate) cur_rx_desc: Reg<u32>,
}

/// De grootte van het blok tot en met de huidige RX-descriptor.
pub(crate) const REGS_SIZE: usize = size_of::<Regs>();

const _: () = {
    assert!(offset_of!(Regs, conf) == 0x0000);
    assert!(offset_of!(Regs, filter) == 0x0004);
    assert!(offset_of!(Regs, gmii_addr) == 0x0010);
    assert!(offset_of!(Regs, gmii_data) == 0x0014);
    assert!(offset_of!(Regs, version) == 0x0020);
    assert!(offset_of!(Regs, int_mask) == 0x003C);
    assert!(offset_of!(Regs, addr0_hi) == 0x0040);
    assert!(offset_of!(Regs, addr0_lo) == 0x0044);
    assert!(offset_of!(Regs, mmc_rx_mask) == 0x010C);
    assert!(offset_of!(Regs, mmc_tx_mask) == 0x0110);
    assert!(offset_of!(Regs, mmc_ipc_mask) == 0x0200);
    assert!(offset_of!(Regs, bus_mode) == 0x1000);
    assert!(offset_of!(Regs, tx_poll) == 0x1004);
    assert!(offset_of!(Regs, rx_poll) == 0x1008);
    assert!(offset_of!(Regs, rx_list) == 0x100C);
    assert!(offset_of!(Regs, tx_list) == 0x1010);
    assert!(offset_of!(Regs, status) == 0x1014);
    assert!(offset_of!(Regs, op_mode) == 0x1018);
    assert!(offset_of!(Regs, intr_ena) == 0x101C);
    assert!(offset_of!(Regs, missed) == 0x1020);
    assert!(offset_of!(Regs, cur_tx_desc) == 0x1048);
    assert!(offset_of!(Regs, cur_rx_desc) == 0x104C);
    assert!(REGS_SIZE == 0x1050);
};

// De MDIO-velden in GMII_ADDR (designware.h; Linux stmmac: addr_shift 11,
// reg_shift 6, clk_csr_shift 2). Andere posities dan de DWMAC4: daarom een
// eigen crate per generatie.
/// De machine is bezig; zelf gezet om een transactie te starten.
pub(crate) const MII_BUSY: u32 = 1 << 0;
/// Schrijven in plaats van lezen.
pub(crate) const MII_WRITE: u32 = 1 << 1;
/// De CSR-klokrange in [5:2].
pub(crate) const MII_CSR_SHIFT: u32 = 2;
/// Het register in [10:6].
pub(crate) const MII_REG_SHIFT: u32 = 6;
/// Het PHY-adres in [15:11].
pub(crate) const MII_ADDR_SHIFT: u32 = 11;

/// De CSR-klokrange voor de MDC-deler: eth_csrclk staat op de SG2002 vast
/// op 250 MHz (dts), dus de range 250-300 MHz = 0b0101 (Linux
/// `STMMAC_CSR_250_300M`). In GMII_ADDR geschoven is dat de 0x14 die de
/// Go-driver hardcodeerde.
pub const CSR_250_300M: u32 = 0x5;
