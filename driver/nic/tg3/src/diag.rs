//! De diagnoseregels van de driver: getallen die bij het lezen uit de chip
//! komen en pas bij het formatteren tekst worden. Geen heap; elk type is een
//! handvol `u32`'s met een `Display`.
//!
//! Zonder deze regels was de bring-up van 29-08 gokwerk geweest, en elke gok
//! kost een boot van drie minuten: de MAC-tellers scheidden een MAC-probleem
//! van een transport-probleem, de LPC-tellers wezen de schuldige in de
//! ontvangstketen aan, en de zelftest zei of het SRAM-venster leefde.

use core::fmt;
use netdev::Mac;

/// Eén regel over de chip ([`Tg3::describe`](crate::Tg3::describe)).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Describe {
    /// De ASIC-revisie (0x57766 op de mini).
    pub asic: u32,
    /// Het MAC-adres.
    pub mac: Mac,
    /// Het PHY-id (reg 2 en 3).
    pub phy_id: u32,
    /// De firmware-mailbox na de reset (`0xb49a89ab` = de bootcode is klaar
    /// en het SRAM-venster leeft).
    pub fw_mbox: u32,
    /// MISC_HOST_CTRL.
    pub misc_host_ctrl: u32,
    /// PCI_COMMAND.
    pub pci_cmd: u32,
}

impl fmt::Display for Describe {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "tg3: ASIC {:#x} mac {} phy {:#010x} fw_mbox {:#x} MISC_HOST_CTRL {:#x} PCI_CMD {:#x}",
            self.asic, self.mac, self.phy_id, self.fw_mbox, self.misc_host_ctrl, self.pci_cmd
        )
    }
}

/// Het SRAM-venster, gemeten ([`Tg3::self_test`](crate::Tg3::self_test)).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SelfTest {
    /// Het proefadres in NIC-SRAM.
    pub off: u32,
    /// Wat het venster via BAR0 teruggaf.
    pub bar: u32,
    /// Wat het venster via de config-space teruggaf (`None` zonder ECAM).
    pub cfg: Option<u32>,
    /// Kwam het proefwoord via BAR0 terug?
    pub ok: bool,
    /// MISC_HOST_CTRL.
    pub misc_host_ctrl: u32,
    /// PCI_COMMAND.
    pub pci_cmd: u32,
    /// De firmware-mailbox na de reset.
    pub fw_mbox: u32,
}

impl fmt::Display for SelfTest {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "tg3: sram[{:#x}] bar={:#x} cfg=", self.off, self.bar)?;
        match self.cfg {
            Some(v) => write!(f, "{v:#x}")?,
            None => f.write_str("n/a")?,
        }
        write!(
            f,
            " ({}) MISC_HOST_CTRL={:#x} PCI_CMD={:#x} fw_mbox={:#x}",
            if self.ok { "OK" } else { "dead" },
            self.misc_host_ctrl,
            self.pci_cmd,
            self.fw_mbox
        )
    }
}

/// De MAC-tellers ([`Tg3::stats`](crate::Tg3::stats)).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Stats {
    /// MAC_RX_STATS_OCTETS.
    pub rx_octets: u32,
    /// Unicast ontvangen.
    pub rx_ucast: u32,
    /// Multicast ontvangen.
    pub rx_mcast: u32,
    /// Broadcast ontvangen.
    pub rx_bcast: u32,
    /// FCS-fouten.
    pub rx_fcs_err: u32,
    /// MAC_TX_STATS_OCTETS.
    pub tx_octets: u32,
    /// Unicast verzonden.
    pub tx_ucast: u32,
    /// Broadcast verzonden.
    pub tx_bcast: u32,
}

impl fmt::Display for Stats {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "tg3: rx oct={} ucast={} mcast={} bcast={} fcs_err={} | tx oct={} ucast={} bcast={}",
            self.rx_octets,
            self.rx_ucast,
            self.rx_mcast,
            self.rx_bcast,
            self.rx_fcs_err,
            self.tx_octets,
            self.tx_ucast,
            self.tx_bcast
        )
    }
}

/// De ontvangstketen ([`Tg3::counters`](crate::Tg3::counters)).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Counters {
    /// RCVLPC_STATUS.
    pub lpc_status: u32,
    /// RCVLPC_NON_EMPTY_BITS.
    pub lpc_nonempty: u32,
    /// Door een filter gevallen.
    pub drop_filter: u32,
    /// Werkrij vol.
    pub wq_full: u32,
    /// Geen ontvangst-BD beschikbaar.
    pub no_rcv_bd: u32,
    /// Weggegooid.
    pub in_discards: u32,
    /// Fouten.
    pub in_errors: u32,
    /// Drempel geraakt.
    pub thresh_hit: u32,
    /// RCVDBDI_STATUS (FRM_TOO_BIG was de derde les van 29-08).
    pub dbdi_status: u32,
    /// RCVDBDI_STD_CON_IDX.
    pub dbdi_std_con: u32,
    /// RCVBDI_STATUS.
    pub bdi_status: u32,
    /// RCVBDI_STD_PROD_IDX.
    pub bdi_std_prod: u32,
}

impl fmt::Display for Counters {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "tg3: lpc status={:#x} nonempty={:#x} drop_filter={} wq_full={} no_rcv_bd={} \
             in_discards={} in_errors={} thresh_hit={} | dbdi status={:#x} std_con={} | \
             bdi status={:#x} std_prod={}",
            self.lpc_status,
            self.lpc_nonempty,
            self.drop_filter,
            self.wq_full,
            self.no_rcv_bd,
            self.in_discards,
            self.in_errors,
            self.thresh_hit,
            self.dbdi_status,
            self.dbdi_std_con,
            self.bdi_status,
            self.bdi_std_prod
        )
    }
}

/// De twee ring-control-blocks in NIC-SRAM
/// ([`Tg3::rcb_dump`](crate::Tg3::rcb_dump)): adres hoog, laag, maxlen,
/// NIC-adres.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RcbDump {
    /// De send-ring.
    pub send: [u32; 4],
    /// De return-ring.
    pub ret: [u32; 4],
}

impl fmt::Display for RcbDump {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let [a, b, c, d] = self.send;
        let [e, g, h, i] = self.ret;
        write!(
            f,
            "tg3: send[addr={a:08x}{b:08x} maxlen={c:#x} nic={d:#x}] \
             ret[addr={e:08x}{g:08x} maxlen={h:#x} nic={i:#x}]"
        )
    }
}

/// Trekt de chip zijn lijn? ([`Tg3::irq_diag`](crate::Tg3::irq_diag)).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct IrqDiag {
    /// Het status-woord van het status-blok (bit 0 = UPDATED).
    pub status: u32,
    /// HOSTCC_MODE.
    pub hostcc: u32,
    /// PCI_STATUS (bit 3 = INTx# asserted).
    pub pci_status: u32,
}

impl fmt::Display for IrqDiag {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "tg3: status {:#x} hostcc {:#x} pci status {:#x}",
            self.status, self.hostcc, self.pci_status
        )
    }
}
