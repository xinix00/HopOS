//! De PCIe-host van QEMU virt (`hw/arm/virt.c`, `gpex`), gedeeld door wie
//! er een functie op zoekt: de qemu-xhci (usb.rs, met `gui`) en de i6300esb
//! (wdt.rs). Hier boot geen firmware die BAR's toewijst; dat doen we zelf
//! uit het 32-bit MMIO-venster, elk uit een eigen stuk, zodat twee
//! functies nooit hetzelfde adres krijgen.

use board::Region;
use dev::Pa;

/// Het ECAM-venster van virt met `highmem-ecam=off` (`VIRT_PCIE_ECAM`):
/// 16 MB, bus 0 tot 15, in de Device-gigabyte van de identity map (`mmu`).
pub(crate) const ECAM: Pa = Pa(0x3f00_0000);
/// De laatste bus in dat venster.
pub(crate) const ECAM_LAST_BUS: u8 = 15;

/// Het 32-bit MMIO-venster van de PCIe-host (`VIRT_PCIE_MMIO`), ook in de
/// Device-gigabyte.
const MMIO_ALL: Region = Region {
    base: Pa(0x1000_0000),
    size: 0x2eff_0000,
};

/// Het stuk voor de watchdog: de bovenste 64 KiB (zijn BAR 0 is 16 bytes).
pub(crate) const WDT_MMIO: Region = Region {
    base: Pa(MMIO_ALL.base.0 + MMIO_ALL.size - 0x1_0000),
    size: 0x1_0000,
};

/// De rest, voor de xHCI.
pub(crate) const MMIO: Region = Region {
    base: MMIO_ALL.base,
    size: MMIO_ALL.size - WDT_MMIO.size,
};

const _: () = assert!(MMIO.base.0 + MMIO.size == WDT_MMIO.base.0);
