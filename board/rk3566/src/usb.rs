//! De USB-invoer van de Radxa Zero 3E: twee DWC3-cores, elk eerst in
//! hostmodus en dan als xHCI (Go: `cmd/hopos/usb_rk3566.go`). De hostmodus
//! zelf is van `driver-dwc3`, die de binary vóór de xHCI draait
//! ([`board::UsbKind::Dwc3`]).
//!
//! De cores hangen aan hun eigen klokken en PHY's, en die zetten wij NIET
//! aan. Dat is een keuze en geen gat: de CRU-gates en de
//! USB2-PHY-GRF-bits van dit silicium zijn niet geverifieerd, en de TSADC
//! van juli is precies wat er gebeurt als je Rockchip-klokregisters gokt.
//! Wat U-Boot achterlaat, is het startpunt; leest GSNPSID nul, dan zegt de
//! bring-up "niet geklokt", en dat is dan de volgende meting.
//!
//! Kaal is er niets: zonder de feature `gui` geeft [`hosts`] een lege lijst
//! (handboek §7).

use dev::Pa;

/// usbdrd30: de OTG-core (de USB-C van de Radxa).
pub const USBDRD30: Pa = Pa(0xFCC0_0000);
/// usbhost30: de hostcore (de USB-A).
pub const USBHOST30: Pa = Pa(0xFD00_0000);
/// Het venster van een DWC3-core: 1 MB (de xHCI onderin, de globale
/// registers op +0xC100).
pub const DWC3_SIZE: u64 = 0x10_0000;

const _: () = {
    // Beide in de Device-MMIO van de SoC (vanaf 0xF000_0000, `mmu`).
    assert!(USBDRD30.0 >= 0xF000_0000 && USBHOST30.0 >= 0xF000_0000);
};

pub(crate) use imp::hosts;

#[cfg(not(feature = "gui"))]
mod imp {
    //! Kaal gebouwd: geen USB.

    /// Geen controllers.
    pub(crate) fn hosts() -> board::UsbHosts {
        board::UsbHosts::new()
    }
}

#[cfg(feature = "gui")]
mod imp {
    use super::{DWC3_SIZE, USBDRD30, USBHOST30};
    use crate::USB_DMA;
    use board::{Region, UsbHost, UsbHosts, UsbKind, usb_dma_slice};

    /// De twee cores met elk de helft van het DMA-stuk. `bus_off` nul: de
    /// cores hangen rechtstreeks op de geheugenbus, dus wat de CPU een adres
    /// noemt, noemt de controller ook zo.
    pub(crate) fn hosts() -> UsbHosts {
        let mut out = UsbHosts::new();
        for (i, (name, base)) in [("usbdrd30", USBDRD30), ("usbhost30", USBHOST30)]
            .into_iter()
            .enumerate()
        {
            let _ = out.push(UsbHost {
                name,
                kind: UsbKind::Dwc3,
                regs: Region {
                    base,
                    size: DWC3_SIZE,
                },
                // De GIC-lijnen staan in de DT; de driver pollt, dus die
                // bedrading komt pas als hij dat niet meer doet.
                irq: None,
                bus_off: 0,
                dma: usb_dma_slice(USB_DMA, i, 2),
            });
        }
        out
    }
}

#[cfg(all(test, feature = "gui"))]
mod tests {
    use super::*;
    use crate::USB_DMA;
    use board::UsbKind;

    #[test]
    fn two_cores_split_the_usb_region() {
        let h = hosts();
        assert_eq!(h.len(), 2);
        assert_eq!(h[0].dma.base, USB_DMA.base);
        assert_eq!(h[1].dma.base.0, USB_DMA.base.0 + USB_DMA.size / 2);
        assert!(h.iter().all(|u| u.kind == UsbKind::Dwc3));
    }
}
