//! De USB-invoer van de Pi 5: de twee xHCI's in de RP1 (Go:
//! `cmd/hopos/usb_rpi5.go`). De goedkoopste van de Pi's, en dat is geen
//! toeval: de PCIe-link naar de RP1 staat al voor de GEM (`probe_nic`,
//! `rp1_link`), dus hier blijft alleen het venster binnen de RP1 over.
//!
//! De RP1 heeft zijn cores al in hostmodus staan voor wij er zijn
//! (`generic-xhci` in de Linux-DT), dus hier geen DWC3-init zoals op de
//! Radxa. Is de link er niet (de NIC-probe faalde of draaide niet), dan
//! noemen we geen controller: een toegang op een dood PCIe-venster is op
//! de BCM2712 geen nette 0xffffffff maar een gok.

use crate::{PCIE_SW_INIT, PCIE2, PCIE2_SW_INIT_ID, RP1, RP1_BUS_OFF};
use board::{Region, UsbHost, UsbHosts, UsbKind, usb_dma_slice};
use board_raspi::usb::UsbCtx;
use dev::Pa;
use driver_brcmpcie::{OutWin, Rc};

/// De eerste USB3-hostcontroller van de RP1 (datasheet §5; Linux-DT
/// `xhci@200000`).
pub(crate) const RP1_USB0: Pa = Pa(RP1 + 0x20_0000);
/// De tweede (`xhci@300000`).
pub(crate) const RP1_USB1: Pa = Pa(RP1 + 0x30_0000);
/// Het venster van elk: 1 MB.
const RP1_USB_SIZE: u64 = 0x10_0000;

/// De twee controllers achter de RP1, met elk de helft van het DMA-stuk.
///
/// `bus_off`: de RP1 is een PCIe-master; wat hij een adres noemt, gaat door
/// het inbound-venster van de BCM2712-RC, en dat legt PCIe 0x10_0000_0000
/// op DRAM 0 (dezelfde term als de GEM). Zonder die term zoekt de
/// controller zijn ringen 64 GB naast het DRAM, en dat geeft geen
/// foutmelding, alleen stilte (Go, 06-08).
pub(crate) fn hosts(ctx: &UsbCtx) -> UsbHosts {
    let mut out = UsbHosts::new();
    let (phy, dl) = link_status(ctx.clock);
    if !(phy && dl) {
        cpu::println!(
            "usb: the RP1 link is down (phy {phy}, dl {dl}): the NIC probe trains it, no USB HOPOS_USB_NONE"
        );
        return out;
    }
    for (i, (name, base)) in [("rp1-usb0", RP1_USB0), ("rp1-usb1", RP1_USB1)]
        .into_iter()
        .enumerate()
    {
        let _ = out.push(UsbHost {
            name,
            kind: UsbKind::Xhci,
            regs: Region {
                base,
                size: RP1_USB_SIZE,
            },
            // De lijnen lopen via de RP1-MSI-X en de MIP; de driver pollt,
            // dus die bedrading komt pas als hij dat niet meer doet.
            irq: None,
            bus_off: RP1_BUS_OFF,
            dma: usb_dma_slice(ctx.dma, i, 2),
        });
    }
    out
}

/// De stand van de RP1-link, alleen gelezen: een RC-handvat zonder
/// windows, want er wordt niets opgezet.
fn link_status(clock: fn() -> u64) -> (bool, bool) {
    let none = OutWin {
        cpu: 0,
        pcie: 0,
        size: 0,
    };
    // SAFETY: PCIE2 en de SW_INIT-bank zijn BCM2712-blokken in de
    // Device-gigabyte 64 (de vaste tabel); `link_status` leest alleen
    // PCIE_STATUS.
    let rc = unsafe {
        Rc::new(
            driver_brcmpcie::Soc::Bcm2712,
            PCIE2,
            PCIE_SW_INIT,
            PCIE2_SW_INIT_ID,
            0,
            none,
            [None; driver_brcmpcie::MAX_IN],
            clock,
        )
    };
    rc.link_status()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_rp1_windows_are_in_gigabyte_124() {
        assert_eq!(RP1_USB0.0, 0x1f_0020_0000);
        assert_eq!(RP1_USB1.0 >> 30, 124);
        let d = board_raspi::usb::USB_DMA;
        assert_eq!(usb_dma_slice(d, 1, 2).base.0, d.base.0 + d.size / 2);
    }
}
