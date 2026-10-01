//! Het virtio-transport (virtio 1.2 §4): de trait [`Transport`] met twee
//! implementaties, [`mmio::Mmio`] en [`pci::Pci`].
//!
//! Een virtio-driver (`driver-virtionet`, `driver-virtioblk`) praat alleen
//! het protocol: status-handdruk, features, split-virtqueues in een
//! DMA-regio, de device-config. Hoe die registers bereikt worden, verschilt
//! per bus en is het enige dat deze crate bezit:
//!
//! - **virtio-mmio** (QEMU virt, geen firmware): één registerblok van 0x200
//!   bytes per slot, de config op 0x100.
//! - **virtio-pci** (EDK2 op QEMU, straks ijzer met een PCIe-fabric): vijf
//!   structuren achter vendor-capabilities in de config-space, elk in een
//!   BAR; een notify-adres per queue.
//!
//! Wat deze crate niet bezit: de ringen en de DMA-regio (dat is de driver),
//! het vinden van de functie en het toewijzen van BAR's (`driver-pcie` en
//! het board), en een interruptlijn (de drivers pollen over PCI; INTx via
//! ACPI `_PRT` of MSI via de ITS komt later).
//!
//! Alleen het moderne transport: VERSION_1, little-endian. Legacy virtio
//! (mmio versie 1, de I/O-BAR van een transitional PCI-device) bestaat hier
//! niet.

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

use core::fmt;
use dev::Pa;

pub mod mmio;
pub mod pci;

pub use mmio::Mmio;
pub use pci::Pci;

#[cfg(test)]
mod tests;

/// De bits van het device-statusregister (virtio 1.2 §2.1).
pub mod status {
    /// De driver zag het device.
    pub const ACKNOWLEDGE: u8 = 1 << 0;
    /// De driver weet hoe hij het device moet drijven.
    pub const DRIVER: u8 = 1 << 1;
    /// De driver is klaar; vanaf nu mag het device de queues gebruiken.
    pub const DRIVER_OK: u8 = 1 << 2;
    /// De feature-onderhandeling is rond (het device kan het weigeren).
    pub const FEATURES_OK: u8 = 1 << 3;
}

/// VIRTIO_F_VERSION_1 (bit 32): bit 0 van feature-venster 1.
pub const FEAT_VERSION_1_HI: u32 = 1 << 0;

/// Hoe vaak [`Transport::reset`] de status leest voordat hij het opgeeft.
/// QEMU reset synchroon (de eerste lees is al 0); de grens is er voor een
/// device dat nooit terugkomt, zodat de boot niet eeuwig hangt.
pub const RESET_POLLS: u32 = 100_000;

/// Hoe vaak [`Transport::config_read64`] opnieuw leest als de
/// config-generatie tussen de twee helften wisselt. Een device dat blijft
/// wisselen, is stuk en geeft geen waarde.
pub const CONFIG_RETRIES: u32 = 16;

/// Waarom een transport weigert.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// Geen virtio: een mmio-slot zonder magie, of een PCI-functie die geen
    /// virtio-device is.
    NotVirtio,
    /// Een legacy virtio-mmio (versie 1; QEMU zonder `force-legacy=false`).
    Legacy {
        /// De versie die het slot gaf.
        version: u32,
    },
    /// Een virtio-pci-structuur ontbreekt (of is te klein of scheef).
    Missing {
        /// Het `cfg_type`: 1 common, 2 notify, 3 ISR.
        cfg_type: u8,
    },
    /// De BAR van een virtio-pci-structuur is niet toegewezen; op een kale
    /// fabric wijst het board de BAR's eerst toe.
    BarUnassigned {
        /// De BAR-index.
        bar: u8,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::NotVirtio => f.write_str("virtio: not a virtio device"),
            Self::Legacy { version } => {
                write!(
                    f,
                    "virtio: legacy mmio transport version {version} (need 2)"
                )
            }
            Self::Missing { cfg_type } => {
                write!(f, "virtio: pci capability cfg_type {cfg_type} missing")
            }
            Self::BarUnassigned { bar } => write!(f, "virtio: pci BAR {bar} not assigned"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Het moderne virtio-transport: alles wat een driver van zijn device ziet
/// behalve de ringen zelf.
///
/// De volgorde van de aanroepen is die van de spec (§3.1): [`reset`],
/// status ACKNOWLEDGE en DRIVER, features per venster van 32 bits,
/// FEATURES_OK, per queue [`select_queue`], [`queue_num_max`],
/// [`set_queue_num`], [`set_queue_addrs`], [`enable_queue`], en dan
/// DRIVER_OK.
///
/// [`reset`]: Transport::reset
/// [`select_queue`]: Transport::select_queue
/// [`queue_num_max`]: Transport::queue_num_max
/// [`set_queue_num`]: Transport::set_queue_num
/// [`set_queue_addrs`]: Transport::set_queue_addrs
/// [`enable_queue`]: Transport::enable_queue
pub trait Transport {
    /// Het virtio-devicetype (1 = net, 2 = blk; virtio 1.2 §5).
    fn device_id(&self) -> u32;

    /// Feature-venster `window` van het device (0 = bits 0..32, 1 = 32..64).
    fn device_features(&self, window: u32) -> u32;

    /// Zet feature-venster `window` van de driver.
    fn set_driver_features(&self, window: u32, bits: u32);

    /// Het statusregister.
    fn status(&self) -> u8;

    /// Schrijft het statusregister (0 = reset).
    fn set_status(&self, status: u8);

    /// Kiest de queue waar de volgende queue-aanroepen over gaan.
    fn select_queue(&mut self, queue: u16);

    /// De grootste queue die het device voor de gekozen queue biedt; 0 = de
    /// queue is er niet (of dit transport kan hem niet bedienen).
    fn queue_num_max(&self) -> u16;

    /// Zet de grootte van de gekozen queue.
    fn set_queue_num(&self, num: u16);

    /// Zet de drie adressen van de gekozen queue: de descriptortabel, de
    /// avail-ring (driver area) en de used-ring (device area).
    fn set_queue_addrs(&self, desc: Pa, driver: Pa, device: Pa);

    /// Zet de gekozen queue aan; vanaf nu werkt [`notify`](Self::notify)
    /// ervoor.
    fn enable_queue(&mut self);

    /// De doorbell: er staat nieuw werk in `queue`.
    fn notify(&self, queue: u16);

    /// Leest de interruptstatus en bevestigt hem, zodat het device zijn
    /// (level-)lijn loslaat. Bit 0 = used-ring bijgewerkt, bit 1 = config
    /// veranderd. Geeft de bits die stonden.
    fn ack_interrupt(&self) -> u32;

    /// De config-generatie: verandert als het device zijn config wijzigt.
    fn config_generation(&self) -> u32;

    /// Leest één byte van de device-config op offset `off`. Buiten de config
    /// leest hij als alle enen, zoals een bus die niets vindt.
    fn config_read8(&self, off: u32) -> u8;

    /// Leest een gealigneerd woord van de device-config op offset `off`.
    /// Buiten de config of scheef: alle enen.
    fn config_read32(&self, off: u32) -> u32;

    /// Leest een 64-bit veld van de device-config als twee woorden,
    /// consistent: de spec (§2.5.1) laat de generatie ervoor en erna lezen
    /// en opnieuw beginnen als hij wisselde. `None` als hij na
    /// [`CONFIG_RETRIES`] pogingen nog wisselt.
    fn config_read64(&self, off: u32) -> Option<u64> {
        for _ in 0..CONFIG_RETRIES {
            let g = self.config_generation();
            let lo = u64::from(self.config_read32(off));
            let hi = u64::from(self.config_read32(off.saturating_add(4)));
            if self.config_generation() == g {
                return Some((hi << 32) | lo);
            }
        }
        None
    }

    /// Reset het device: status 0 schrijven en wachten tot hij 0 leest
    /// (§4.1.4.3.2: een PCI-device mag de reset asynchroon doen). `false`
    /// als het device na [`RESET_POLLS`] lezen niet terug is.
    ///
    /// Nodig ook omdat de firmware het device al gebruikte: EDK2 reset zijn
    /// virtio-devices bij ExitBootServices, maar de driver rekent daar niet
    /// op.
    fn reset(&self) -> bool {
        self.set_status(0);
        (0..RESET_POLLS).any(|_| self.status() == 0)
    }
}

/// Splitst een adres in de lage en de hoge 32 bits: beide transports zetten
/// de queue-adressen als twee woorden.
fn split(pa: Pa) -> (u32, u32) {
    ((pa.0 & 0xffff_ffff) as u32, (pa.0 >> 32) as u32)
}
