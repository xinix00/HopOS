//! De bootregel van de driver: getallen die bij het lezen uit de chip komen
//! en pas bij het formatteren tekst worden. Geen heap; een handvol `u32`'s
//! met een `Display`.

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
