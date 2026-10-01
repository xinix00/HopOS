//! Het registerblok van de tg3 op BAR0 (Linux `tg3.h`) en de bitwaarden die
//! de driver schrijft, met een assertie per offset.
//!
//! De eerste 0x100 bytes van BAR0 spiegelen de PCI-config-space; daarom is
//! [`Config`] één type dat op twee plekken ligt: in de echte config-space
//! (ECAM, van het board) en aan het begin van [`Regs`]. De device-registers
//! zitten daarachter. Buiten dit blok ligt nog NIC-SRAM, bereikbaar via het
//! geheugenvenster in [`Config`]; die adressen staan onderaan als getallen,
//! want het is geen MMIO maar een tweede adresruimte.

use core::mem::{offset_of, size_of};
use dev::Reg;

/// Een ring-control-block in registers (`TG3_BDINFO_*`): waar de ring in
/// host-geheugen staat, maat en vlaggen, en waar de chip zijn eigen kopie in
/// NIC-SRAM houdt.
#[repr(C)]
pub(crate) struct BdInfo {
    /// TG3_BDINFO_HOST_ADDR, hoog.
    pub(crate) host_hi: Reg<u32>,
    /// TG3_BDINFO_HOST_ADDR, laag.
    pub(crate) host_lo: Reg<u32>,
    /// TG3_BDINFO_MAXLEN_FLAGS.
    pub(crate) maxlen_flags: Reg<u32>,
    /// TG3_BDINFO_NIC_ADDR.
    pub(crate) nic_addr: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(BdInfo, host_hi) == 0x0);
    assert!(offset_of!(BdInfo, host_lo) == 0x4);
    assert!(offset_of!(BdInfo, maxlen_flags) == 0x8);
    assert!(offset_of!(BdInfo, nic_addr) == 0xc);
    assert!(size_of::<BdInfo>() == 0x10);
};

/// De eerste 0x100 bytes van de functie: de echte config-space (ECAM) en, met dezelfde indeling, de spiegel aan het begin van BAR0.
#[repr(C)]
pub(crate) struct Config {
    /// Vendor (15:0) en device (31:16).
    pub(crate) id: Reg<u32>,
    /// PCI_COMMAND (15:0) en PCI_STATUS (31:16).
    pub(crate) command: Reg<u32>,
    _r0: [u8; 0x60],
    /// TG3PCI_MISC_HOST_CTRL; CHIPREV in 31:16.
    pub(crate) misc_host_ctrl: Reg<u32>,
    /// TG3PCI_DMA_RW_CTRL.
    pub(crate) dma_rw_ctrl: Reg<u32>,
    /// TG3PCI_PCISTATE.
    pub(crate) pci_state: Reg<u32>,
    _r1: [u8; 0x8],
    /// TG3PCI_MEM_WIN_BASE_ADDR: het GEHEUGEN-venster (0x78 is dat van de registers).
    pub(crate) mem_win_base: Reg<u32>,
    _r2: [u8; 0x4],
    /// TG3PCI_MEM_WIN_DATA (0x80 is dat van de registers).
    pub(crate) mem_win_data: Reg<u32>,
    _r3: [u8; 0x74],
    /// TG3PCI_GEN15_PRODID_ASICREV: het echte ASIC-nummer als CHIPREV 0xf zegt.
    pub(crate) prodid_asicrev: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Config, id) == 0x0000);
    assert!(offset_of!(Config, command) == 0x0004);
    assert!(offset_of!(Config, misc_host_ctrl) == 0x0068);
    assert!(offset_of!(Config, dma_rw_ctrl) == 0x006c);
    assert!(offset_of!(Config, pci_state) == 0x0070);
    assert!(offset_of!(Config, mem_win_base) == 0x007c);
    assert!(offset_of!(Config, mem_win_data) == 0x0084);
    assert!(offset_of!(Config, prodid_asicrev) == 0x00fc);
    assert!(size_of::<Config>() == 0x100);
};

/// Het registerblok op BAR0 (Linux `tg3.h`), met een assertie per offset.
#[repr(C)]
pub(crate) struct Regs {
    /// De spiegel van de PCI-config-space (de eerste 0x100 bytes van BAR0).
    pub(crate) cfg: Config,
    _r0: [u8; 0x104],
    /// MAILBOX_INTERRUPT_0, lage helft: 1 = gemaskeerd, 0 = open.
    pub(crate) mb_interrupt: Reg<u32>,
    _r1: [u8; 0x64],
    /// MAILBOX_RCV_STD_PROD_IDX, lage helft.
    pub(crate) mb_rx_std_prod: Reg<u32>,
    _r2: [u8; 0x14],
    /// MAILBOX_RCVRET_CON_IDX_0, lage helft.
    pub(crate) mb_rx_ret_cons: Reg<u32>,
    _r3: [u8; 0x7c],
    /// MAILBOX_SNDHOST_PROD_IDX_0, lage helft.
    pub(crate) mb_tx_prod: Reg<u32>,
    _r4: [u8; 0xf8],
    /// MAC_MODE.
    pub(crate) mac_mode: Reg<u32>,
    _r5: [u8; 0xc],
    /// MAC_ADDR_0..3, elk HIGH (de eerste twee bytes) en LOW (de laatste
    /// vier, big-endian): de vier exact-match-filters van de MAC. Linux
    /// schrijft ze alle vier (`__tg3_set_mac_addr`).
    pub(crate) mac_addr: [Reg<u32>; 8],
    _r6: [u8; 0xc],
    /// MAC_RX_MTU_SIZE.
    pub(crate) rx_mtu_size: Reg<u32>,
    _r7: [u8; 0xc],
    /// MAC_MI_COM: de MDIO-transactie.
    pub(crate) mi_com: Reg<u32>,
    /// MAC_MI_STAT.
    pub(crate) mi_stat: Reg<u32>,
    /// MAC_MI_MODE.
    pub(crate) mi_mode: Reg<u32>,
    _r8: [u8; 0x4],
    /// MAC_TX_MODE.
    pub(crate) tx_mode: Reg<u32>,
    _r9: [u8; 0x4],
    /// MAC_TX_LENGTHS.
    pub(crate) tx_lengths: Reg<u32>,
    /// MAC_RX_MODE.
    pub(crate) rx_mode: Reg<u32>,
    _r10: [u8; 0x4],
    /// MAC_HASH_REG_0..3: het multicast-filter.
    pub(crate) hash: [Reg<u32>; 4],
    _r11: [u8; 0x80],
    /// MAC_RCV_RULE_CFG.
    pub(crate) rcv_rule_cfg: Reg<u32>,
    /// MAC_LOW_WMARK_MAX_RX_FRAME.
    pub(crate) low_wmark: Reg<u32>,
    _r12: [u8; 0x2f8],
    /// MAC_TX_STATS_OCTETS.
    pub(crate) tx_octets: Reg<u32>,
    _r13: [u8; 0x68],
    /// MAC_TX_STATS_UCAST.
    pub(crate) tx_ucast: Reg<u32>,
    _r14: [u8; 0x4],
    /// MAC_TX_STATS_BCAST.
    pub(crate) tx_bcast: Reg<u32>,
    _r15: [u8; 0x8],
    /// MAC_RX_STATS_OCTETS.
    pub(crate) rx_octets: Reg<u32>,
    _r16: [u8; 0x8],
    /// MAC_RX_STATS_UCAST.
    pub(crate) rx_ucast: Reg<u32>,
    /// MAC_RX_STATS_MCAST.
    pub(crate) rx_mcast: Reg<u32>,
    /// MAC_RX_STATS_BCAST.
    pub(crate) rx_bcast: Reg<u32>,
    /// MAC_RX_STATS_FCS_ERRORS.
    pub(crate) rx_fcs_err: Reg<u32>,
    _r17: [u8; 0x364],
    /// SNDDATAI_MODE.
    pub(crate) snddatai_mode: Reg<u32>,
    _r18: [u8; 0x4],
    /// SNDDATAI_STATSCTRL.
    pub(crate) snddatai_stats_ctrl: Reg<u32>,
    /// SNDDATAI_STATSENAB.
    pub(crate) snddatai_stats_enab: Reg<u32>,
    _r19: [u8; 0x3f0],
    /// SNDDATAC_MODE.
    pub(crate) snddatac_mode: Reg<u32>,
    _r20: [u8; 0x3fc],
    /// SNDBDS_MODE.
    pub(crate) sndbds_mode: Reg<u32>,
    _r21: [u8; 0x3fc],
    /// SNDBDI_MODE.
    pub(crate) sndbdi_mode: Reg<u32>,
    _r22: [u8; 0x3fc],
    /// SNDBDC_MODE.
    pub(crate) sndbdc_mode: Reg<u32>,
    _r23: [u8; 0x3fc],
    /// RCVLPC_MODE.
    pub(crate) rcvlpc_mode: Reg<u32>,
    /// RCVLPC_STATUS.
    pub(crate) rcvlpc_status: Reg<u32>,
    _r24: [u8; 0x4],
    /// RCVLPC_NON_EMPTY_BITS.
    pub(crate) rcvlpc_nonempty: Reg<u32>,
    /// RCVLPC_CONFIG.
    pub(crate) rcvlpc_config: Reg<u32>,
    /// RCVLPC_STATSCTRL.
    pub(crate) rcvlpc_stats_ctrl: Reg<u32>,
    /// RCVLPC_STATS_ENABLE.
    pub(crate) rcvlpc_stats_enable: Reg<u32>,
    _r25: [u8; 0x224],
    /// RCVLPC_STATS: door een filter gevallen.
    pub(crate) lpc_drop_filter: Reg<u32>,
    /// RCVLPC_STATS: werkrij vol.
    pub(crate) lpc_wq_full: Reg<u32>,
    _r26: [u8; 0x4],
    /// RCVLPC_STATS: geen ontvangst-BD beschikbaar.
    pub(crate) lpc_no_rcv_bd: Reg<u32>,
    /// RCVLPC_STATS: weggegooid.
    pub(crate) lpc_in_discards: Reg<u32>,
    /// RCVLPC_STATS: fouten.
    pub(crate) lpc_in_errors: Reg<u32>,
    /// RCVLPC_STATS: drempel geraakt.
    pub(crate) lpc_thresh_hit: Reg<u32>,
    _r27: [u8; 0x1a4],
    /// RCVDBDI_MODE.
    pub(crate) rcvdbdi_mode: Reg<u32>,
    /// RCVDBDI_STATUS.
    pub(crate) rcvdbdi_status: Reg<u32>,
    _r28: [u8; 0x38],
    /// RCVDBDI_JUMBO_BD: de jumbo-ring (bij ons uit).
    pub(crate) rcvdbdi_jumbo_bd: BdInfo,
    /// RCVDBDI_STD_BD: de standaard-producer-ring.
    pub(crate) rcvdbdi_std_bd: BdInfo,
    _r29: [u8; 0x14],
    /// RCVDBDI_STD_CON_IDX.
    pub(crate) rcvdbdi_std_con: Reg<u32>,
    _r30: [u8; 0x388],
    /// RCVDCC_MODE.
    pub(crate) rcvdcc_mode: Reg<u32>,
    _r31: [u8; 0x3fc],
    /// RCVBDI_MODE.
    pub(crate) rcvbdi_mode: Reg<u32>,
    /// RCVBDI_STATUS.
    pub(crate) rcvbdi_status: Reg<u32>,
    _r32: [u8; 0x4],
    /// RCVBDI_STD_PROD_IDX.
    pub(crate) rcvbdi_std_prod: Reg<u32>,
    _r33: [u8; 0x8],
    /// RCVBDI_STD_THRESH.
    pub(crate) rcvbdi_std_thresh: Reg<u32>,
    _r34: [u8; 0xe4],
    /// STD_REPLENISH_LWM (57765-plus).
    pub(crate) std_replenish_lwm: Reg<u32>,
    _r35: [u8; 0x2fc],
    /// RCVCC_MODE.
    pub(crate) rcvcc_mode: Reg<u32>,
    _r36: [u8; 0x600],
    /// TG3_CPMU_LSPD_10MB_CLK.
    pub(crate) cpmu_lspd_10mb_clk: Reg<u32>,
    _r37: [u8; 0x60],
    /// TG3_CPMU_PADRNG_CTL.
    pub(crate) cpmu_padrng_ctl: Reg<u32>,
    _r38: [u8; 0x594],
    /// HOSTCC_MODE.
    pub(crate) hostcc_mode: Reg<u32>,
    _r39: [u8; 0x4],
    /// HOSTCC_RXCOL_TICKS.
    pub(crate) hostcc_rx_ticks: Reg<u32>,
    /// HOSTCC_TXCOL_TICKS.
    pub(crate) hostcc_tx_ticks: Reg<u32>,
    /// HOSTCC_RXMAX_FRAMES.
    pub(crate) hostcc_rx_frames: Reg<u32>,
    /// HOSTCC_TXMAX_FRAMES.
    pub(crate) hostcc_tx_frames: Reg<u32>,
    _r40: [u8; 0x20],
    /// HOSTCC_STATUS_BLK_HOST_ADDR, hoog.
    pub(crate) hostcc_status_hi: Reg<u32>,
    /// HOSTCC_STATUS_BLK_HOST_ADDR, laag.
    pub(crate) hostcc_status_lo: Reg<u32>,
    _r41: [u8; 0x3c0],
    /// MEMARB_MODE.
    pub(crate) memarb_mode: Reg<u32>,
    _r42: [u8; 0x3fc],
    /// BUFMGR_MODE.
    pub(crate) bufmgr_mode: Reg<u32>,
    _r43: [u8; 0xc],
    /// BUFMGR_MB_RDMA_LOW_WATER.
    pub(crate) bufmgr_mb_rdma_low: Reg<u32>,
    /// BUFMGR_MB_MACRX_LOW_WATER.
    pub(crate) bufmgr_mb_macrx_low: Reg<u32>,
    /// BUFMGR_MB_HIGH_WATER.
    pub(crate) bufmgr_mb_high: Reg<u32>,
    _r44: [u8; 0x18],
    /// BUFMGR_DMA_LOW_WATER.
    pub(crate) bufmgr_dma_low: Reg<u32>,
    /// BUFMGR_DMA_HIGH_WATER.
    pub(crate) bufmgr_dma_high: Reg<u32>,
    _r45: [u8; 0x3c4],
    /// RDMAC_MODE.
    pub(crate) rdmac_mode: Reg<u32>,
    _r46: [u8; 0xfc],
    /// TG3_RDMA_RSRVCTRL_REG.
    pub(crate) rdma_rsrvctrl: Reg<u32>,
    _r47: [u8; 0x2fc],
    /// WDMAC_MODE.
    pub(crate) wdmac_mode: Reg<u32>,
    _r48: [u8; 0x1bfc],
    /// GRC_MODE.
    pub(crate) grc_mode: Reg<u32>,
    /// GRC_MISC_CFG.
    pub(crate) grc_misc_cfg: Reg<u32>,
    _r49: [u8; 0x1404],
    /// TG3_PCIE_DL_LO_FTSMAX (TG3_PCIE_TLDLPL_PORT + 0xc), alleen zichtbaar met GRC_MODE_PCIE_DL_SEL.
    pub(crate) pcie_dl_lo_ftsmax: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Regs, cfg) == 0x0000);
    assert!(offset_of!(Regs, mb_interrupt) == 0x0204);
    assert!(offset_of!(Regs, mb_rx_std_prod) == 0x026c);
    assert!(offset_of!(Regs, mb_rx_ret_cons) == 0x0284);
    assert!(offset_of!(Regs, mb_tx_prod) == 0x0304);
    assert!(offset_of!(Regs, mac_mode) == 0x0400);
    assert!(offset_of!(Regs, mac_addr) == 0x0410);
    assert!(offset_of!(Regs, rx_mtu_size) == 0x043c);
    assert!(offset_of!(Regs, mi_com) == 0x044c);
    assert!(offset_of!(Regs, mi_stat) == 0x0450);
    assert!(offset_of!(Regs, mi_mode) == 0x0454);
    assert!(offset_of!(Regs, tx_mode) == 0x045c);
    assert!(offset_of!(Regs, tx_lengths) == 0x0464);
    assert!(offset_of!(Regs, rx_mode) == 0x0468);
    assert!(offset_of!(Regs, hash) == 0x0470);
    assert!(offset_of!(Regs, rcv_rule_cfg) == 0x0500);
    assert!(offset_of!(Regs, low_wmark) == 0x0504);
    assert!(offset_of!(Regs, tx_octets) == 0x0800);
    assert!(offset_of!(Regs, tx_ucast) == 0x086c);
    assert!(offset_of!(Regs, tx_bcast) == 0x0874);
    assert!(offset_of!(Regs, rx_octets) == 0x0880);
    assert!(offset_of!(Regs, rx_ucast) == 0x088c);
    assert!(offset_of!(Regs, rx_mcast) == 0x0890);
    assert!(offset_of!(Regs, rx_bcast) == 0x0894);
    assert!(offset_of!(Regs, rx_fcs_err) == 0x0898);
    assert!(offset_of!(Regs, snddatai_mode) == 0x0c00);
    assert!(offset_of!(Regs, snddatai_stats_ctrl) == 0x0c08);
    assert!(offset_of!(Regs, snddatai_stats_enab) == 0x0c0c);
    assert!(offset_of!(Regs, snddatac_mode) == 0x1000);
    assert!(offset_of!(Regs, sndbds_mode) == 0x1400);
    assert!(offset_of!(Regs, sndbdi_mode) == 0x1800);
    assert!(offset_of!(Regs, sndbdc_mode) == 0x1c00);
    assert!(offset_of!(Regs, rcvlpc_mode) == 0x2000);
    assert!(offset_of!(Regs, rcvlpc_status) == 0x2004);
    assert!(offset_of!(Regs, rcvlpc_nonempty) == 0x200c);
    assert!(offset_of!(Regs, rcvlpc_config) == 0x2010);
    assert!(offset_of!(Regs, rcvlpc_stats_ctrl) == 0x2014);
    assert!(offset_of!(Regs, rcvlpc_stats_enable) == 0x2018);
    assert!(offset_of!(Regs, lpc_drop_filter) == 0x2240);
    assert!(offset_of!(Regs, lpc_wq_full) == 0x2244);
    assert!(offset_of!(Regs, lpc_no_rcv_bd) == 0x224c);
    assert!(offset_of!(Regs, lpc_in_discards) == 0x2250);
    assert!(offset_of!(Regs, lpc_in_errors) == 0x2254);
    assert!(offset_of!(Regs, lpc_thresh_hit) == 0x2258);
    assert!(offset_of!(Regs, rcvdbdi_mode) == 0x2400);
    assert!(offset_of!(Regs, rcvdbdi_status) == 0x2404);
    assert!(offset_of!(Regs, rcvdbdi_jumbo_bd) == 0x2440);
    assert!(offset_of!(Regs, rcvdbdi_std_bd) == 0x2450);
    assert!(offset_of!(Regs, rcvdbdi_std_con) == 0x2474);
    assert!(offset_of!(Regs, rcvdcc_mode) == 0x2800);
    assert!(offset_of!(Regs, rcvbdi_mode) == 0x2c00);
    assert!(offset_of!(Regs, rcvbdi_status) == 0x2c04);
    assert!(offset_of!(Regs, rcvbdi_std_prod) == 0x2c0c);
    assert!(offset_of!(Regs, rcvbdi_std_thresh) == 0x2c18);
    assert!(offset_of!(Regs, std_replenish_lwm) == 0x2d00);
    assert!(offset_of!(Regs, rcvcc_mode) == 0x3000);
    assert!(offset_of!(Regs, cpmu_lspd_10mb_clk) == 0x3604);
    assert!(offset_of!(Regs, cpmu_padrng_ctl) == 0x3668);
    assert!(offset_of!(Regs, hostcc_mode) == 0x3c00);
    assert!(offset_of!(Regs, hostcc_rx_ticks) == 0x3c08);
    assert!(offset_of!(Regs, hostcc_tx_ticks) == 0x3c0c);
    assert!(offset_of!(Regs, hostcc_rx_frames) == 0x3c10);
    assert!(offset_of!(Regs, hostcc_tx_frames) == 0x3c14);
    assert!(offset_of!(Regs, hostcc_status_hi) == 0x3c38);
    assert!(offset_of!(Regs, hostcc_status_lo) == 0x3c3c);
    assert!(offset_of!(Regs, memarb_mode) == 0x4000);
    assert!(offset_of!(Regs, bufmgr_mode) == 0x4400);
    assert!(offset_of!(Regs, bufmgr_mb_rdma_low) == 0x4410);
    assert!(offset_of!(Regs, bufmgr_mb_macrx_low) == 0x4414);
    assert!(offset_of!(Regs, bufmgr_mb_high) == 0x4418);
    assert!(offset_of!(Regs, bufmgr_dma_low) == 0x4434);
    assert!(offset_of!(Regs, bufmgr_dma_high) == 0x4438);
    assert!(offset_of!(Regs, rdmac_mode) == 0x4800);
    assert!(offset_of!(Regs, rdma_rsrvctrl) == 0x4900);
    assert!(offset_of!(Regs, wdmac_mode) == 0x4c00);
    assert!(offset_of!(Regs, grc_mode) == 0x6800);
    assert!(offset_of!(Regs, grc_misc_cfg) == 0x6804);
    assert!(offset_of!(Regs, pcie_dl_lo_ftsmax) == 0x7c0c);
    assert!(size_of::<Regs>() == 0x7c10);
};

// MISC_HOST_CTRL. INDIR_ACCESS is de sleutel tot het hele indirecte venster:
// staat dat bit uit, dan verdwijnt élke schrijf naar MEM_WIN_BASE/DATA en
// leest élke lees nul. tg3 schrijft deze waarde als állereerste handeling,
// vóór welke andere toegang dan ook (`tg3_enable_register_access`), en
// opnieuw na elke chip-reset.
pub(crate) const MISC_MASK_PCI_INT: u32 = 1 << 1;
pub(crate) const MISC_WORD_SWAP: u32 = 1 << 3;
pub(crate) const MISC_PCISTATE_RW: u32 = 1 << 4;
pub(crate) const MISC_INDIR_ACCESS: u32 = 1 << 7;
pub(crate) const MISC_CHIPREV_MASK: u32 = 0xffff_0000;

// PCISTATE zoals `tg3_restore_pci_state` hem na een reset achterlaat.
pub(crate) const PCISTATE_ROM_ENABLE: u32 = 1 << 5;
pub(crate) const PCISTATE_ROM_RETRY: u32 = 1 << 6;

// MAC_MODE.
pub(crate) const MAC_MODE_HALF_DUPLEX: u32 = 1 << 1;
pub(crate) const MAC_MODE_PORT_MASK: u32 = 0x0c;
pub(crate) const MAC_MODE_PORT_GMII: u32 = 0x08;
pub(crate) const MAC_MODE_PORT_MII: u32 = 0x04;
pub(crate) const MAC_MODE_RXSTAT_ENABLE: u32 = 1 << 11;
pub(crate) const MAC_MODE_RXSTAT_CLEAR: u32 = 1 << 12;
pub(crate) const MAC_MODE_TXSTAT_ENABLE: u32 = 1 << 14;
pub(crate) const MAC_MODE_TXSTAT_CLEAR: u32 = 1 << 15;
// De data-engines. Zonder deze drie doet de MAC niets met de ringen: de link
// staat en de tellers lopen, maar er komt geen enkel frame binnen (gemeten
// 29-08: 0 frames in 5 s met alleen de STAT-bits).
pub(crate) const MAC_MODE_TDE_ENABLE: u32 = 1 << 21;
pub(crate) const MAC_MODE_RDE_ENABLE: u32 = 1 << 22;
pub(crate) const MAC_MODE_FHDE_ENABLE: u32 = 1 << 23;
/// Wat een lopende MAC altijd aan heeft staan.
pub(crate) const MAC_MODE_RUN: u32 = MAC_MODE_RXSTAT_ENABLE
    | MAC_MODE_TXSTAT_ENABLE
    | MAC_MODE_TDE_ENABLE
    | MAC_MODE_RDE_ENABLE
    | MAC_MODE_FHDE_ENABLE;

// MI_COM (MDIO).
pub(crate) const MI_COM_CMD_READ: u32 = 0x0800_0000;
pub(crate) const MI_COM_CMD_WRITE: u32 = 0x0400_0000;
/// START bij het schrijven, BUSY bij het lezen: hetzelfde bit.
pub(crate) const MI_COM_BUSY: u32 = 0x2000_0000;
pub(crate) const MI_COM_PHY_SHIFT: u32 = 21;
pub(crate) const MI_COM_REG_SHIFT: u32 = 16;
pub(crate) const MI_COM_DATA_MASK: u32 = 0xffff;
/// MI_MODE: de basiswaarde zonder AUTO_POLL (bit 4). Zolang de MAC zelf de
/// PHY pollt, botsen onze MI_COM-transacties met de zijne.
pub(crate) const MI_MODE_BASE: u32 = 0x000c_0000;
pub(crate) const MI_STAT_LNKSTAT_ATTN: u32 = 0x1;

// GRC_MISC_CFG.
pub(crate) const GRC_MISC_CFG_CORECLK_RESET: u32 = 1 << 0;
/// tg3 zet dit bit op PCIe-chips apart vóór de reset (`tg3_chip_reset`).
pub(crate) const GRC_MISC_CFG_PCIE: u32 = 1 << 29;

// De 57765/57766-familie vraagt om een handvol eigen instellingen; tg3 doet
// ze in `tg3_reset_hw` vóór de ringen.
pub(crate) const DMA_RWCTRL_DIS_CACHE_ALIGN: u32 = 0x0000_0001;
pub(crate) const DMA_RWCTRL_WRITE_CMD: u32 = 0x7 << 28;
pub(crate) const DMA_RWCTRL_READ_CMD: u32 = 0x6 << 24;
pub(crate) const CPMU_LSPD_10MB_MACCLK_MASK: u32 = 0x001f_0000;
pub(crate) const CPMU_LSPD_10MB_MACCLK_6_25: u32 = 0x0013_0000;
pub(crate) const CPMU_PADRNG_CTL_RDIV2: u32 = 0x0004_0000;
pub(crate) const GRC_MODE_PCIE_DL_SEL: u32 = 0x2000_0000;
pub(crate) const GRC_MODE_PCIE_PORT_MASK: u32 = 0x6000_0000;
pub(crate) const PCIE_DL_LO_FTSMAX_MASK: u32 = 0x0000_00ff;
pub(crate) const PCIE_DL_LO_FTSMAX_VAL: u32 = 0x0000_002c;

// GRC_MODE-swapbits. Niet optioneel op een little-endian host: de chip is
// intern big-endian, en deze bits zetten DMA-data (BSWAP/WSWAP_DATA) en
// descriptors (WSWAP_NONFRM_DATA) in host-volgorde. tg3 zet ze altijd; alleen
// BSWAP_NONFRM_DATA is big-endian-only (29-08: ze stonden uit, en dat was één
// van de dingen naast INDIR_ACCESS).
pub(crate) const GRC_MODE_SWAP_DATA: u32 = 0x10 | 0x20;
pub(crate) const GRC_MODE_WSWAP_NONFRM: u32 = 0x04;
pub(crate) const GRC_MODE_HOST_STACKUP: u32 = 0x0001_0000;
pub(crate) const GRC_MODE_HOST_SENDBDS: u32 = 0x0002_0000;
pub(crate) const GRC_MODE_NO_TX_PHDR_CSUM: u32 = 0x0010_0000;
pub(crate) const GRC_MODE_IRQ_ON_MAC_ATTN: u32 = 0x0400_0000;

/// ENABLE: hetzelfde bit in bijna elk MODE-register.
pub(crate) const MODE_ENABLE: u32 = 0x2;
/// ATTN_ENABLE: idem.
pub(crate) const MODE_ATTN: u32 = 0x4;

// De DMA-engines: alleen ENABLE is niet genoeg. tg3 zet altijd de hele rij
// foutmeldings-bits erbij (target/master abort, pariteit, adres-overflow,
// FIFO over/under-run, lange lees); die horen bij een werkende engine, niet
// bij diagnose.
pub(crate) const DMAC_ERR_ENAB: u32 = 0x4 | 0x8 | 0x10 | 0x20 | 0x40 | 0x80 | 0x100 | 0x200;
/// PCIe.
pub(crate) const RDMAC_FIFO_LONG_BURST: u32 = 0x0003_0000;
/// 57766, MTU tot 1500.
pub(crate) const RDMAC_JMB_2K_MMRR: u32 = 0x0080_0000;
/// 57765-plus.
pub(crate) const RDMAC_IPV6_LSO_EN: u32 = 0x1000_0000;
/// 5755-plus.
pub(crate) const WDMAC_STATUS_TAG_FIX: u32 = 0x2000_0000;
/// Op 57765-plus zet tg3 hier een FIFO-overflow-fix in
/// (`TG3_RDMA_RSRVCTRL_FIFO_OFLW_FIX`).
pub(crate) const RDMA_RSRVCTRL_FIFO_OFLW_FIX: u32 = 0x4;

/// 32-byte status-blok (`tp->coalesce_mode`).
pub(crate) const HOSTCC_MODE_32BYTE: u32 = 0x100;
/// HOSTCC_MODE_NOW: status-blok nú bijwerken (`tg3_int_reenable`).
pub(crate) const HOSTCC_MODE_NOW: u32 = 0x8;
/// De ringgrootte staat in de RCB.
pub(crate) const RCVDBDI_INV_RING_SZ: u32 = 0x10;
pub(crate) const RCVBDI_RCB_ATTN: u32 = 0x4;
pub(crate) const RCVLPC_CLASS0_ATTN: u32 = 0x4;
pub(crate) const RCVLPC_MAPOOR_ATTN: u32 = 0x8;
pub(crate) const RCVLPC_STATSOFLOW_ATTN: u32 = 0x10;
/// RCV_RULE_CFG_DEFAULT_CLASS.
pub(crate) const RCV_RULE_DEFAULT_CLASS: u32 = 0x8;
/// RCVLPC_STATSENAB_DACK_FIX.
pub(crate) const RCVLPC_STATSENAB_DACK_FIX: u32 = 0x0004_0000;
pub(crate) const TX_MODE_ENABLE: u32 = 0x2;
/// TX_MODE_MBUF_LOCKUP_FIX (5755-plus).
pub(crate) const TX_MODE_MBUF_LOCKUP_FIX: u32 = 0x100;
pub(crate) const RX_MODE_ENABLE: u32 = 0x2;
/// RX_MODE_PROMISC: elk unicast-frame, ook niet aan ons.
pub(crate) const RX_MODE_PROMISC: u32 = 0x100;
/// 5755-plus.
pub(crate) const RX_MODE_IPV6_CSUM: u32 = 0x0100_0000;

pub(crate) const BDINFO_FLAGS_DISABLED: u32 = 0x2;
pub(crate) const BDINFO_MAXLEN_SHIFT: u32 = 16;
/// Afstand tussen twee RCB's in NIC-SRAM.
pub(crate) const BDINFO_SIZE: u32 = 0x10;
/// TG3_SRAM_RX_STD_BDCACHE_SIZE_5700.
pub(crate) const BDCACHE_MAX: u32 = 128;

// NIC-SRAM (via het geheugenvenster, `tg3_write_mem`): de RCB's van de send-
// en return-ring, de spiegels van de ringen, en de firmware-mailbox.
pub(crate) const SRAM_SEND_RCB: u32 = 0x0100;
pub(crate) const SRAM_RCV_RET_RCB: u32 = 0x0200;
pub(crate) const SRAM_TX_BUFFER_DESC: u32 = 0x4000;
pub(crate) const SRAM_RX_BUFFER_DESC: u32 = 0x6000;
/// De bootcode van de chip meldt zich hier: eerst MAGIC1, dan het
/// complement.
pub(crate) const SRAM_FW_MBOX: u32 = 0x0b50;
pub(crate) const FW_MBOX_MAGIC1: u32 = 0x4b65_7654;

// Descriptor-vlaggen.
pub(crate) const TXD_FLAG_END: u32 = 0x0004;
/// RXD_FLAG_END in `type_flags` van élke producer-BD: zonder neemt de chip
/// hem niet (29-08).
pub(crate) const RXD_FLAG_END: u32 = 0x0004;
pub(crate) const RXD_FLAG_ERROR: u32 = 0x0400;
/// RXD_ERR_MASK uit tg3.h. Let op dat ODD_NIBBLE_RCVD_MII (0x100000) er NIET
/// in zit: dat bit zet de chip ook op frames die verder in orde zijn.
pub(crate) const RXD_ERR_MASK: u32 = 0x01ef_0000;
/// `opaque` van een producer-BD: de index plus RXD_OPAQUE_RING_STD.
pub(crate) const RXD_OPAQUE_RING_STD: u32 = 0x0001_0000;

// De PHY.
/// Het adres van de ingebouwde PHY (tg3: TG3_PHY_MII_ADDR).
pub(crate) const PHY_ADDR: u8 = 1;
/// Broadcom: auxiliary status (snelheid en duplex in 10:8).
pub(crate) const MII_AUX_STAT: u8 = 0x19;
