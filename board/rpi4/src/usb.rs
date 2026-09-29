//! De USB-invoer van de Pi 4: de VL805 achter de BCM2711-root-complex (Go:
//! `cmd/hopos/usb_rpi4.go`).
//!
//! Dit is de duurste van de Pi's, want op de Pi 4 IS alle USB die
//! PCIe-kaart: hier moet een root-complex op die voor niets anders bestaat
//! (op de Pi 5 staat de link al voor de GEM). Waarom dan toch: het bord
//! wordt nog verkocht en ligt bij iedere tinkerer in de la (Derek, 06-08).
//!
//! De stappen, elk met zijn regel:
//!
//! 1. het outbound-venster (CPU 0x6_0000_0000, 64 MB naar PCIe
//!    0xF800_0000, uit de Pi 4-DT) als Device in de niveau-1-tabel: de
//!    vaste tabel mapt alleen gigabyte 0 en 3;
//! 2. de RC op (`driver-brcmpcie`, BCM2711: geen RESCAL, gen 2), de link, de
//!    VL805 (1106:3483) op bus 1 en zijn BAR 0 op 0xF800_0000;
//! 3. de firmware-handshake: een VL805 zonder eigen SPI-EEPROM (de latere
//!    Pi 4-revisies, de CM4) heeft na de PCIe-reset geen firmware tot de
//!    VideoCore hem laadt (`NOTIFY_XHCI_RESET`, Linux
//!    `rpi_firmware_init_vl805`). Staat er al een versie in config 0x50,
//!    dan is hij geladen en vragen we niets (zoals Linux).
//!
//! Kaal is er niets: zonder de feature `gui` is dit een lege stub en linkt
//! de binary geen PCIe (handboek §7).

pub(crate) use imp::hosts;

#[cfg(not(feature = "gui"))]
mod imp {
    //! Kaal gebouwd: geen USB.

    use board::UsbHosts;
    use board_raspi::usb::UsbCtx;

    /// Geen controllers (en het board vraagt het kaal ook nooit).
    pub(crate) fn hosts(_ctx: &UsbCtx) -> UsbHosts {
        UsbHosts::new()
    }
}

#[cfg(feature = "gui")]
mod imp {
    use crate::Bcm2711;
    use board::{Region, UsbHost, UsbHosts, UsbKind};
    use board_raspi::usb::{self, UsbCtx};
    use cpu::println;
    use dev::Pa;
    use driver_brcmpcie::{EpBar, InWin, OutWin, Rc};

    /// De PCIe-root-complex (`brcm,bcm2711-pcie`, DT `pcie@7d500000` in de
    /// 0x7e000000-view, in low-peripheral-mode op 0xFD50_0000).
    const PCIE: Pa = Pa(0xFD50_0000);
    /// Het outbound-venster: CPU 0x6_0000_0000 ziet PCIe 0xF800_0000.
    const OUT_CPU: u64 = 0x6_0000_0000;
    const OUT_PCIE: u64 = 0xF800_0000;
    const OUT_SIZE: u64 = 0x0400_0000;
    /// De VL805 zoals de config-read hem geeft: device << 16 | vendor.
    const VL805_ID: u32 = 0x3483_1106;
    /// Waar de VL805 zijn firmwareversie laat zien (Linux
    /// `VL805_PCI_CONFIG_VERSION_OFFSET`): nul = niet geladen.
    const VL805_VERSION: u64 = 0x50;
    /// Het xHCI-venster van de VL805 (BAR 0): 4 KB.
    const VL805_SIZE: u64 = 0x1000;
    /// Hoe lang de VL805 na de handshake nodig heeft (Linux: 200 tot
    /// 1000 µs).
    const VL805_SETTLE_NS: u64 = 1_000_000;

    /// De VL805 op, of één regel waarom niet.
    pub(crate) fn hosts(ctx: &UsbCtx) -> UsbHosts {
        let mut out = UsbHosts::new();
        if !usb::map_device_gb::<Bcm2711>(OUT_CPU >> 30) {
            println!(
                "usb: vl805: gigabyte {} for the PCIe window cannot be mapped HOPOS_USB_NONE",
                OUT_CPU >> 30
            );
            return out;
        }
        let rc = rc(ctx.clock);
        let bars = [
            // BAR 0 laag: de xHCI-registers; hoog: 64-bit BAR, bovenhelft nul.
            EpBar {
                off: 0x10,
                val: OUT_PCIE as u32,
            },
            EpBar { off: 0x14, val: 0 },
        ];
        // SAFETY: geen RESCAL op de BCM2711 (0), de RC staat op PCIE.
        if let Err(e) = unsafe { rc.bring_up(0, VL805_ID, &bars) } {
            println!("usb: vl805: bcm2711 pcie: {e} HOPOS_USB_NONE");
            return out;
        }
        firmware(&rc, ctx.clock);
        println!(
            "usb: vl805 on PCIe (status {:#x}), xHCI window at {OUT_CPU:#x} (bus {OUT_PCIE:#x})",
            rc.status()
        );
        let _ = out.push(UsbHost {
            name: "vl805",
            kind: UsbKind::Xhci,
            regs: Region {
                base: Pa(OUT_CPU),
                size: VL805_SIZE,
            },
            // De INTx van de RC is nog niet bedraad (zoals de GENET); de
            // driver pollt.
            irq: None,
            // De dma-ranges van dit bord leggen PCIe 0 op DRAM 0: wat de
            // VL805 een adres noemt, is het fysieke adres (het tweede
            // inbound-venster hieronder).
            bus_off: 0,
            dma: ctx.dma,
        });
        out
    }

    /// De RC met het adresplan van de Pi 4. Inbound: op de BCM2711 is RC_BAR2
    /// hét DRAM-venster en hoort BAR1 uit te staan (`pcie-brcmstb.c`); de
    /// lege eerste regel schrijft size-encoding 0 in BAR1. 4 GB en niet het
    /// hele DRAM: de enige DMA hier is die van de xHCI naar
    /// `board_raspi::usb::USB_DMA`, en die ligt laag.
    fn rc(clock: fn() -> u64) -> Rc {
        let inb = [
            Some(InWin {
                pcie: 0,
                cpu: 0,
                size: 0,
            }),
            Some(InWin {
                pcie: 0,
                cpu: 0,
                size: 0x1_0000_0000,
            }),
            None,
            None,
        ];
        let out = OutWin {
            cpu: OUT_CPU,
            pcie: OUT_PCIE,
            size: OUT_SIZE,
        };
        // SAFETY: PCIE is het RC-blok van de BCM2711 in het Device-venster
        // van gigabyte 3 (de vaste tabel); de SW_INIT-bank bestaat op deze
        // SoC niet (de reset zit in RGR1) en wordt niet aangeraakt.
        unsafe {
            Rc::new(
                driver_brcmpcie::Soc::Bcm2711,
                PCIE,
                Pa(0),
                0,
                2, // de VL805 is een gen2 x1-endpoint
                out,
                inb,
                clock,
            )
        }
    }

    /// De firmware van de VL805: al geladen (een versie in config 0x50), of
    /// de VideoCore vragen. Elke uitkomst is één regel.
    fn firmware(rc: &Rc, clock: fn() -> u64) {
        let version = rc.cfg_read32(1, 0, 0, VL805_VERSION);
        if version != 0 {
            println!("usb: vl805 firmware {version:#x} already loaded");
            return;
        }
        match usb::notify_xhci_reset(usb::vl805_dev_addr(1, 0, 0)) {
            Ok(_) => {
                let until = clock().saturating_add(VL805_SETTLE_NS);
                while clock() < until {
                    core::hint::spin_loop();
                }
                println!(
                    "usb: vl805 firmware loaded by the VideoCore, version now {:#x}",
                    rc.cfg_read32(1, 0, 0, VL805_VERSION)
                );
            }
            Err(e) => println!("usb: vl805 firmware handshake: {e} HOPOS_USB_VL805"),
        }
    }
}
