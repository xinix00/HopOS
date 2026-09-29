//! De USB-invoer van de Pi's: wat de Pi 4 en de Pi 5 delen. Het stuk
//! DMA-geheugen ([`USB_DMA`]), de firmware-handshake die de VL805 van de
//! Pi 4 zijn firmware laat laden ([`notify_xhci_reset`]), en een
//! Device-gigabyte voor een PCIe-venster buiten de vaste tabel
//! ([`map_device_gb`]). Welke controllers er zijn, weet de SoC
//! (`Soc::usb_hosts`: de RP1 op de Pi 5, de VL805 op de Pi 4).
//!
//! Kaal is er niets: zonder de feature `gui` geeft het board een lege
//! lijst en wordt `Soc::usb_hosts` nooit gevraagd (handboek §7,
//! docs/gui.md).

use crate::Soc;
use board::{Region, UsbHosts};
use dev::Pa;

/// Het USB-DMA-stuk: 2 MB in de DMA-regio (Normal-NC in de vaste tabel,
/// onder 1 GB), boven de NIC-helft en de mailbox-buffer. De 2 MB-korrel is
/// die van `abi::layout::USB_DMA_SIZE`; de VL805 van de Pi 4 is 32-bit
/// DMA, dus laag is een eis (Go legde hem op 0x1480_0000, waar in v3 de
/// mailbox-buffer staat).
pub const USB_DMA: Region = Region {
    base: Pa(0x14A0_0000),
    size: abi::layout::USB_DMA_SIZE,
};

const _: () = {
    use crate::map::{DMA, VCMAIL_BUF};
    assert!(USB_DMA.base.0 >= VCMAIL_BUF + driver_vcmail::BUFFER_BYTES as u64);
    assert!(USB_DMA.base.0 >= DMA.base + abi::layout::NET_DMA_SIZE);
    assert!(USB_DMA.base.0 + USB_DMA.size <= DMA.base + DMA.size);
    assert!(USB_DMA.base.0.is_multiple_of(0x20_0000));
};

/// Wat een SoC van het board krijgt om zijn controllers te noemen.
#[derive(Copy, Clone, Debug)]
pub struct UsbCtx {
    /// Het DMA-geheugen voor alle controllers samen ([`USB_DMA`]).
    pub dma: Region,
    /// De klok, voor de PCIe-bring-up.
    pub clock: fn() -> u64,
}

/// De controllers van dit board (`Board::usb_hosts`).
pub(crate) fn hosts<S: Soc>() -> UsbHosts {
    imp::hosts::<S>()
}

#[cfg(not(feature = "gui"))]
mod imp {
    //! Kaal gebouwd: geen USB.

    use crate::Soc;
    use board::UsbHosts;

    // Dezelfde signatuur als de gui-smaak, waar `S` de controllers noemt;
    // de cfg bepaalt of hij gebruikt wordt (handboek §10).
    #[allow(
        clippy::extra_unused_type_parameters,
        reason = "kaal vraagt niemand de SoC; de gui-smaak wel"
    )]
    pub(super) fn hosts<S: Soc>() -> UsbHosts {
        UsbHosts::new()
    }
}

#[cfg(feature = "gui")]
mod imp {
    use super::{USB_DMA, UsbCtx};
    use crate::Soc;
    use board::UsbHosts;

    pub(super) fn hosts<S: Soc>() -> UsbHosts {
        S::usb_hosts(&UsbCtx {
            dma: USB_DMA,
            clock: cpu::idle::now,
        })
    }
}

/// De property-tag die de VideoCore de firmware van de VL805 laat laden
/// (`RPI_FIRMWARE_NOTIFY_XHCI_RESET`, `include/soc/bcm2835/raspberrypi-firmware.h`).
pub const TAG_NOTIFY_XHCI_RESET: u32 = 0x0003_0058;

/// Het apparaatadres voor [`TAG_NOTIFY_XHCI_RESET`]: bus, device en
/// functie zoals Linux ze samenstelt (`rpi_firmware_init_vl805`).
#[must_use]
pub const fn vl805_dev_addr(bus: u8, dev: u8, func: u8) -> u32 {
    (bus as u32) << 20 | (dev as u32 & 0x1f) << 15 | (func as u32 & 0x7) << 12
}

/// Vraagt de VideoCore de firmware van de VL805 te laden (Linux
/// `rpi_firmware_init_vl805`): op een Pi 4 zonder SPI-EEPROM voor de VL805
/// (de latere revisies en de CM4) heeft de controller na de PCIe-reset
/// geen firmware en antwoordt hij niet. De mailbox is die van het board
/// (één eigenaar, de executor van core 0). Een fout is de reden, in het
/// Engels.
pub fn notify_xhci_reset(dev_addr: u32) -> Result<u32, &'static str> {
    let Ok(mut cell) = crate::MBOX.try_borrow_mut() else {
        return Err("mailbox busy");
    };
    let Some(m) = cell.as_mut() else {
        return Err("no mailbox (discover did not run)");
    };
    let mut words = [dev_addr];
    let mut tags = [driver_vcmail::Tag {
        id: TAG_NOTIFY_XHCI_RESET,
        words: &mut words,
    }];
    m.call(&mut tags)
        .map_err(|_| "NOTIFY_XHCI_RESET refused by the firmware")?;
    Ok(words[0])
}

/// Mapt gigabyte `gb` als Device in de niveau-1-tabel van dit board, als
/// die regel nog leeg is (een PCIe-venster buiten de vaste tabel: het
/// outbound-venster van de Pi 4 op 0x6_0000_0000). Geeft of de gigabyte
/// nu Device gemapt is.
pub fn map_device_gb<S: Soc>(gb: u64) -> bool {
    let Some(t) = S::tables() else {
        return false;
    };
    if gb >= 512 || t.l2.iter().flatten().any(|l| l.gb == gb) {
        return false;
    }
    let at = t.l1.add(8 * gb);
    let want = cpu::boot::block(gb << 30, cpu::boot::ATTR_DEVICE);
    match dev::read64(at) {
        0 => {
            dev::write64(at, want);
            crate::arch::tables_changed();
            true
        }
        cur => cur == want,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_vl805_behind_the_bridge_is_bus_1() {
        assert_eq!(vl805_dev_addr(1, 0, 0), 0x0010_0000);
        assert_eq!(vl805_dev_addr(0, 31, 7), 31 << 15 | 7 << 12);
    }
}
