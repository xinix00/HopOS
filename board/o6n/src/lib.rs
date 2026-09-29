//! De Radxa Orion O6N (Cix P1 / CD8180: twaalf Armv9-cores in drie
//! clusters, 2x RTL8125/8126, M.2 NVMe): het primaire productiedoel van
//! HopOS, en in v3 het UEFI-board (`board-uefi`) plus wat de O6N eigen
//! heeft (`OLD/metal/board/o6n/hop`).
//!
//! Alles wat de firmware vertelt, komt langs de universele weg van
//! `board-uefi`: cores uit de MADT, RAM uit de memory-map, PCIe uit de MCFG,
//! de console uit de SPCR, CPU_ON via PSCI. Wat dit crate toevoegt, is de
//! kennis die geen ACPI-tabel draagt:
//!
//! - de NIC ([`probe`]): de Realtek achter de eerste root-poort die er een
//!   heeft, en zijn INTx-lijn per root-poort;
//! - de schijf: de NVMe via dezelfde zoektocht;
//! - de klok ([`clock`], [`cpc`]): de `_CPC`-fastchannels en de knop voor
//!   `driver-dvfs`;
//! - de core-klassen ([`class`]): small/mid/big, want de Cix-firmware vult de
//!   MADT-efficiëntieklasse niet;
//! - de thermometer ([`thermal`]): de SCP via SCMI.
//!
//! - de codec ([`codec`], feature `media`): de VPU uit de DSDT, stroom en
//!   klokken via SCMI, en de stroomcyclus als hij vastzit. De driver zelf
//!   (`media-mve`) importeert board niet en omgekeerd.
//!
//! USB hoort bij gui en staat hier niet.

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

pub mod class;
pub mod clock;
#[cfg(feature = "media")]
pub mod codec;
pub mod cpc;
mod machine;
pub mod probe;
pub mod thermal;
// De tien native xHCI's uit de DSDT (usb.rs), alleen in de gui-smaak.
mod usb;

pub use machine::{LINK_TIMEOUT_NS, O6n};

// De slot- en flip-lijm van de binary leest het plan onder deze namen
// (`slots::plan`, `mpidr`, de staging, `KERN_RAM`, `DMA`), zoals bij de
// Pi's; op de O6N zijn het die van het UEFI-board.
pub use board_uefi::{DMA, KERN_RAM, KERN_VHE, irq, slots, watchdog};
// De rekenkern en de taak van het klokbeleid, voor de telemetrie van de
// binary (die dit crate alleen als `vboard` kent).
pub use driver_dvfs as dvfs;

/// De schijf die `probe_disk` geeft: de NVMe. De binary noemt hem
/// `vboard::Disk`, zodat de geprobede schijf van de bench naar de opslag gaat
/// zonder dat de binary het type per board kent.
pub type Disk = driver_nvme::Nvme;

/// De XSDT-OEM-ID van de Cix-firmware: de runtime-toets dat dit werkelijk
/// een Cix P1 is vóór er board-kennis (mailbox-adressen) in MMIO gaat. Een
/// generieke UEFI-doos met dit image krijgt dan gewoon het UEFI-gedrag (de
/// Ampere draaide dit image op 19-09).
pub const OEM_ID: [u8; 6] = *b"CIXTEK";

/// UART2, de 3-pins debug-header van de Orion O6/O6N (PL011, 115200 8n1,
/// door de firmware al opgezet: haar eigen console). De SPCR wijst naar
/// UART0 of UART3 (in dmesg: ttyAMA0 op 0x040b0000), die de SCP dicht houdt;
/// zonder deze spiegel ziet de kabel niets.
pub const HEADER_UART: u64 = 0x040d_0000;

/// De naam, voor de bootlog.
pub const NAME: &str = "o6n";
