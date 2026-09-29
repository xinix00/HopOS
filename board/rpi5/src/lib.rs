//! Raspberry Pi 5 (BCM2712, 4x Cortex-A76): het BCM2712-eigene. De rest
//! (boot, kaart, plan, mailbox, GIC-bedrading) is `board_raspi`.
//!
//! Boot zonder UEFI: de EEPROM-bootloader laadt `hop-agent5.img` rauw op
//! 0x80000 (hij negeert `kernel_address`, en eist `os_check=0`) en levert
//! ons op EL2 af; PSCI komt van de armstub (TF-A BL31) op EL3 via SMC.
//!
//! Netwerk: de hele keten is van ons, de firmware doet er niets van
//! (gemeten 10-07): RESCAL, de pcie2-RC met de 54 MHz-PLL, link-training
//! (gen 2), de RP1 (1de4:0001), zijn BAR's (BAR1 op PCIe 0: de
//! DMA-loopback-eis), de PHY-reset via RP1-GPIO32, autonegotiatie, de GEM,
//! en de interrupt: RP1-MSI-X naar de MIP naar GIC-SPI 128+n.
//!
//! LET OP de C1-stepping: de BCM2712 C1 heeft een interconnect-deadlock
//! onder PCIe-inbound-DMA (OLD/docs/v1/archief/bcm2712-c1-erratum.md): een
//! stille totale freeze bij aanhoudend RX-verkeer plus fabric-breed werk
//! (image-kopie, cache-onderhoud, TLBI, core-start). D0 heeft de fix. De
//! registerkeuzes die de kans verkleinen staan in `driver-brcmpcie` en
//! `driver-gem` (MAX_BURST 128 B, VDM-QoS, AMP 8/8, PAE).

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

use board::Error;
use board_raspi::driver_gicv2::Gic;
use board_raspi::map::{self, L2, Tables};
use board_raspi::{NicCtx, Raspi, Soc};
use cpu::irq::Line;
use dev::Pa;
use driver_brcmpcie::{EpBar, InWin, OutWin, Rc};
use driver_gem::Gem;
use driver_pl011::Pl011;

// De twee xHCI's in de RP1 (usb.rs).
mod usb;

pub use board_raspi::{DMA, Disk, KERN_RAM};

/// De debug-UART (PL011, de 3-pins JST-SH-connector; Linux ttyAMA10). De
/// firmware zet hem op 115200 zodra hij zelf logt (`uart_2ndstage=1`).
pub const UART10: Pa = Pa(0x10_7D00_1000);
/// De GIC-400: distributor op +0x1000, CPU-interface op +0x2000.
pub const GIC: Pa = Pa(0x10_7FFF_8000);
/// De VideoCore-mailbox (DT mailbox@7c013880).
pub const VCMAIL: Pa = Pa(0x10_7C01_3880);
/// De PCIe-root-complex van de RP1-link (pcie2).
pub const PCIE2: Pa = Pa(0x10_0012_0000);
/// Het gedeelde RESCAL-blok van de PCIe-controllers.
pub const PCIE_RESCAL: u64 = 0x10_0011_9500;
/// De brcm,brcmstb-reset SW_INIT-bank.
pub const PCIE_SW_INIT: Pa = Pa(0x10_0150_4318);
/// Het bridge-reset-id van pcie2 (42/43/44 = pcie0/1/2).
pub const PCIE2_SW_INIT_ID: u32 = 44;
/// De MIP: een MSI-write naar [`MIP_MSI_ADDR`] wordt GIC-SPI 128 + data.
pub const MIP: Pa = Pa(0x10_0013_0000);
/// Het PCIe-adres dat een endpoint in zijn MSI-X-entry zet.
pub const MIP_MSI_ADDR: u64 = 0xff_ffff_f000;
/// De eerste SPI van de MIP.
pub const MIP_FIRST_SPI: u32 = 128;
/// Het ARM-zicht op de RP1: het peripheral-venster (RP1-adres - 0x4000_0000).
pub const RP1: u64 = 0x1f_0000_0000;
/// De GEM in de RP1.
pub const RP1_ETH: Pa = Pa(RP1 + 0x10_0000);
/// RP1-IRQ van de ethernet (dt-bindings/mfd/rp1.h).
pub const RP1_INT_ETH: u32 = 6;
/// De DMA-vertaling van de RP1: busadres = fysiek + dit (het
/// inbound-window van de RC legt PCIe 0x10_0000_0000 op DRAM 0).
pub const RP1_BUS_OFF: u64 = 0x10_0000_0000;
/// De compatible van de RP1-ethernet in de DTB.
pub const GEM_COMPAT: &str = "cdns,macb";

// SAFETY: de debug-PL011 van de BCM2712, in de Device-gigabyte 65 van de
// vaste tabel.
static UART: Pl011 = unsafe { Pl011::new(UART10) };

// SAFETY: de GIC-400 van de BCM2712, in dezelfde Device-gigabyte.
static GIC400: Gic = unsafe { Gic::new(GIC.add(0x1000), GIC.add(0x2000)) };

/// De BCM2712 als SoC.
pub struct Bcm2712;

/// De Pi 5 als board.
pub type Rpi5 = Raspi<Bcm2712>;

impl Soc for Bcm2712 {
    type Nic = Gem;
    const NAME: &'static str = "rpi5";
    const SOC: &'static str = "BCM2712";
    const MAC_FALLBACK: u8 = 0x05;
    const VCMAIL: Pa = VCMAIL;

    fn uart() -> &'static Pl011 {
        &UART
    }

    fn gic() -> &'static Gic {
        &GIC400
    }

    fn mpidr(core: usize) -> u64 {
        slots::mpidr(core)
    }

    fn core_of(mpidr: u64) -> usize {
        slots::core_of(mpidr)
    }

    fn usb_hosts(ctx: &board_raspi::usb::UsbCtx) -> board::UsbHosts {
        usb::hosts(ctx)
    }

    fn tables() -> Option<Tables> {
        let (l1, gb0) = arch::tables()?;
        Some(Tables {
            l1,
            l2: [
                Some(L2 {
                    gb: 0,
                    table: gb0,
                    fixed: abi::Region::new(0, map::FIXED_END),
                }),
                None,
            ],
        })
    }

    fn probe_nic(ctx: &NicCtx) -> Result<Option<Gem>, Error> {
        if board_raspi::device_enabled(GEM_COMPAT) == Some(false) {
            cpu::println!("net: {GEM_COMPAT} disabled in the DTB, no NIC");
            return Ok(None);
        }
        let rc = rp1_link(ctx.clock)?;
        // De ethernet-PHY (BCM54213PE) hangt in reset aan RP1-GPIO32
        // (actief laag, DT phy-reset-gpios; gemeten: zonder dit géén PHY op
        // MDIO).
        rp1_gpio_out(32, false);
        delay(ctx.clock, 10_000_000);
        rp1_gpio_out(32, true);
        delay(ctx.clock, 50_000_000);

        // SAFETY: RP1_ETH ligt achter de net getrainde link in het
        // outbound-window (Device-gigabyte 124); de NIC-regio is van deze
        // driver alleen, Normal non-cacheable, en de RP1 bereikt hem via het
        // inbound-window op fysiek + RP1_BUS_OFF.
        let mut nic = unsafe {
            Gem::new(
                RP1_ETH,
                RP1_BUS_OFF,
                Pa(ctx.dma.base),
                ctx.dma.size,
                ctx.mac,
                ctx.clock,
            )
        };
        nic.mdio_enable();
        let phy = driver_mdio::scan(&mut nic).ok_or(Error::Nic("rp1: no PHY on MDIO"))?;
        cpu::println!(
            "net: rp1 up, PHY at {} ({:#06x}:{:#06x}), autonegotiating",
            phy.addr,
            phy.id1,
            phy.id2
        );
        let link = driver_mdio::autoneg(&mut nic, phy.addr, true, ctx.clock, 8_000_000_000)
            .map_err(|_| Error::Nic("rp1: no link within 8 s"))?;
        nic.init(link)
            .map_err(|_| Error::Nic("gem: DMA region too small"))?;
        match wire_irq(&rc, &mut nic) {
            Ok(id) => {
                cpu::println!("net: gem up, {link}, RX on INTID {id} (RP1 MSI-X via the MIP)")
            }
            Err(why) => cpu::println!("net: gem up, {link}, RX polled: {why} HOPOS_NIC_POLLED"),
        }
        Ok(Some(nic))
    }
}

fn delay(clock: fn() -> u64, ns: u64) {
    let end = clock().saturating_add(ns);
    while clock() < end {
        core::hint::spin_loop();
    }
}

/// De link naar de RP1, boardvast bewezen met probe6 (10-07, runs 2/4/5):
/// RESCAL, de pcie2-RC (54 MHz-PLL!), link-training (gen 2), de RP1
/// (1de4:0001) en zijn BAR's. BAR1 MOET op PCIe 0: RP1's eigen DMA bereikt
/// zijn peripherals via de loopback door het eerste inbound-window.
fn rp1_link(clock: fn() -> u64) -> Result<Rc, Error> {
    let inb = [
        // RP1-loopback (BAR1).
        Some(InWin {
            pcie: 0,
            cpu: RP1,
            size: 0x40_0000,
        }),
        // Al het DRAM.
        Some(InWin {
            pcie: RP1_BUS_OFF,
            cpu: 0,
            size: 0x10_0000_0000,
        }),
        // De MIP: het MSI-X-doel van de RP1 (bcm2712.dtsi dma-ranges). Zonder
        // dit window slaat een MSI-write op een ongemapt PCIe-adres,
        // kansgedreven fabric-gif midden in de RX-stroom (13-07).
        Some(InWin {
            pcie: MIP_MSI_ADDR,
            cpu: MIP.0,
            size: 0x1000,
        }),
        None,
    ];
    let out = OutWin {
        cpu: RP1,
        pcie: 0,
        size: 0x1000_0000,
    };
    // SAFETY: PCIE2 en de SW_INIT-bank zijn BCM2712-blokken in de
    // Device-gigabyte 64; de windows zijn het adresplan hierboven.
    let rc = unsafe {
        Rc::new(
            driver_brcmpcie::Soc::Bcm2712,
            PCIE2,
            PCIE_SW_INIT,
            PCIE2_SW_INIT_ID,
            2,
            out,
            inb,
            clock,
        )
    };
    // BAR-groottes gemeten met probe6: 16 KB / 4 MB / 64 KB.
    let bars = [
        EpBar {
            off: 0x10,
            val: 0x100_0000,
        },
        EpBar { off: 0x14, val: 0 },
        EpBar {
            off: 0x18,
            val: 0x101_0000,
        },
    ];
    // SAFETY: PCIE_RESCAL is het gedeelde RESCAL-blok (Device-gigabyte 64).
    unsafe { rc.bring_up(PCIE_RESCAL, 0x0001_1de4, &bars) }.map_err(|e| {
        cpu::println!("net: {e}");
        Error::Nic("rp1: PCIe bring-up failed")
    })?;
    cpu::println!(
        "net: pcie2 link up (status {:#x}), RP1 1de4:0001",
        rc.status()
    );
    Ok(rc)
}

/// Zet RP1-GPIO `pin` als software-output (funcsel 5 = sys_rio) op het
/// gegeven niveau: eerst het niveau, dán output-enable, geen glitch. Alleen
/// bij een getrainde link met BAR1 op PCIe 0.
fn rp1_gpio_out(pin: u32, high: bool) {
    let (bank, off) = match pin {
        34.. => (2, pin - 34),
        28.. => (1, pin - 28),
        _ => (0, pin),
    };
    let io = Pa(RP1 + 0xd_0000 + bank * 0x4000);
    let rio = Pa(RP1 + 0xe_0000 + bank * 0x4000);
    let pads = Pa(RP1 + 0xf_0000 + bank * 0x4000);
    let o = u64::from(off);
    // Pad: output-disable (bit 7) eraf, de rest laten staan.
    let pad = pads.add(4 + o * 4);
    dev::write32(pad, dev::read32(pad) & !(1 << 7));
    dev::write32(io.add(o * 8 + 4), 5);
    let out = if high {
        rio.add(0x2000)
    } else {
        rio.add(0x3000)
    };
    dev::write32(out, 1 << off);
    dev::write32(rio.add(0x2000 + 4), 1 << off);
    dev::mb();
}

/// De RP1-kant van de MSI (drivers/mfd/rp1.c): per peripheral-IRQ een
/// MSIX_CFG-woord op 0x10_8000 + 8 + 4n, met de SET-alias op +0x800.
const RP1_MSIX_CFG: u64 = RP1 + 0x10_8000 + 0x8 + 0x800;
const RP1_MSIX_ENABLE: u32 = 1 << 0;
const RP1_MSIX_IACK: u32 = 1 << 2;
const RP1_MSIX_IACK_EN: u32 = 1 << 3;

/// De rearm van de board-kant: de RP1 bekijkt de lijn opnieuw (IACK-modus:
/// na één MSI wacht hij hierop voor hij een nog staande lijn opnieuw
/// afvuurt).
fn rp1_iack() {
    dev::write32(Pa(RP1_MSIX_CFG + 4 * u64::from(RP1_INT_ETH)), RP1_MSIX_IACK);
    dev::mb();
}

/// De device-ack van de dispatcher.
fn gem_ack() {
    // SAFETY: RP1_ETH is het GEM-blok; `wire_irq` zet de lijn pas aan
    // nadat de GEM leeft, en die leeft zolang de node draait.
    unsafe { driver_gem::ack_irq(RP1_ETH) };
}

static GEM_ACK: fn() = gem_ack;

/// Bedraadt de GEM-interrupt: RP1-MSI-X-entry naar de MIP naar de GIC.
/// Elke stap die faalt laat de NIC pollen, met de reden.
fn wire_irq(rc: &Rc, nic: &mut Gem) -> Result<u32, &'static str> {
    // 1. De MSI-X-capability van de RP1 (bus 1, dev 0): tabel-BAR en offset.
    let mut ptr = u64::from(rc.cfg_read32(1, 0, 0, 0x34) & 0xff);
    let mut cap = 0;
    for _ in 0..48 {
        if ptr < 0x40 {
            break;
        }
        let hdr = rc.cfg_read32(1, 0, 0, ptr);
        if hdr & 0xff == 0x11 {
            cap = ptr;
            break;
        }
        ptr = u64::from((hdr >> 8) & 0xff);
    }
    if cap == 0 {
        return Err("rp1: no MSI-X capability");
    }
    let hdr = rc.cfg_read32(1, 0, 0, cap);
    let entries = ((hdr >> 16) & 0x7ff) + 1;
    if entries <= RP1_INT_ETH {
        return Err("rp1: MSI-X table too small");
    }
    let tab = rc.cfg_read32(1, 0, 0, cap + 4);
    let (bir, off) = (u64::from(tab & 7), u64::from(tab & !7));
    let bar = u64::from(rc.cfg_read32(1, 0, 0, 0x10 + 4 * bir) & !0xf);
    let entry = Pa(RP1 + bar + off + 16 * u64::from(RP1_INT_ETH));
    dev::write32(entry, MIP_MSI_ADDR as u32);
    dev::write32(entry.add(4), (MIP_MSI_ADDR >> 32) as u32);
    dev::write32(entry.add(8), RP1_INT_ETH);
    dev::write32(entry.add(12), 0);
    dev::mb();
    // Function mask eraf, MSI-X aan.
    rc.cfg_write32(1, 0, 0, cap, (hdr & !(1 << 30)) | (1 << 31));

    // 2. De MIP open naar de host (mip_probe: MASK_HOST 0, MASK_VPU ~0,
    //    CFG_HOST ~0), de lijn als flank (de MIP levert een MSI als edge).
    for (o, v) in [
        (0x40, 0),
        (0x50, 0),
        (0x60, u32::MAX),
        (0x70, u32::MAX),
        (0x20, u32::MAX),
        (0x30, u32::MAX),
    ] {
        dev::write32(MIP.add(o), v);
    }
    dev::mb();
    let id = 32 + MIP_FIRST_SPI + RP1_INT_ETH;
    GIC400.set_edge(id);
    let bell = cpu::irq::enable(Line(id), Some(&GEM_ACK)).map_err(|_| "gic: line refused")?;

    // 3. De RP1: MSI aan in IACK-modus, en de GEM zelf open.
    dev::write32(
        Pa(RP1_MSIX_CFG + 4 * u64::from(RP1_INT_ETH)),
        RP1_MSIX_ENABLE | RP1_MSIX_IACK_EN,
    );
    dev::mb();
    nic.set_irq(bell, rp1_iack);
    Ok(id)
}

/// Het slot-plan: dat van `board_raspi`, met de A76-nummering.
pub mod slots {
    pub use board_raspi::slots::*;

    /// Het MPIDR-target van logische core `core`. LET OP: de Cortex-A76
    /// nummert in affiniteit 1 (MT-formaat: aff0 = thread, altijd 0),
    /// anders dan de A72 van de Pi 4 en QEMU's A53.
    #[must_use]
    pub const fn mpidr(core: usize) -> u64 {
        (core as u64) << 8
    }

    /// De logische core bij een MPIDR: aff1.
    #[must_use]
    pub const fn core_of(mpidr: u64) -> usize {
        ((mpidr >> 8) & 0xff) as usize
    }
}

board_raspi::pi_entry!(0x10_7D00_1000u64, board_raspi::map::FIXED_END);

mod arch {
    //! De vaste tabellen van de Pi 5, als data in het image.
    //!
    //! - niveau 1: gigabyte 0 via een eigen niveau-2-tabel; 64 en 65 (de
    //!   SoC-peripherals op 0x10_0000_0000: PCIe, MIP, GIC, UART, mailbox)
    //!   en 124 (het RP1-venster op 0x1f_0000_0000) als Device-blokken; de
    //!   rest leeg tot `discover` er DRAM bij zet;
    //! - gigabyte 0: als de Pi 4 (`board_raspi::map`).
    use dev::Pa;

    #[cfg(all(target_arch = "aarch64", target_os = "none"))]
    core::arch::global_asm!(
        r#"
    .section .data.pagetables, "aw"
    .balign 4096
    .global __boot_ttbr0
__boot_ttbr0:
    .quad __pi_l2_gb0 + 3
    .fill 63, 8, 0
    .quad {d64}
    .quad {d65}
    .fill 58, 8, 0
    .quad {d124}
    .fill 387, 8, 0

    .balign 4096
    .global __pi_l2_gb0
__pi_l2_gb0:
    .set blk, 0
    .rept 128
    .quad {nrm} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept 32
    .quad {dev} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept 8
    .quad {nc} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .fill 344, 8, 0
"#,
        nrm = const cpu::boot::block(0, cpu::boot::ATTR_NORMAL),
        dev = const cpu::boot::block(0, cpu::boot::ATTR_DEVICE),
        nc = const cpu::boot::block(0, cpu::boot::ATTR_NORMAL_NC),
        d64 = const cpu::boot::block(64 << 30, cpu::boot::ATTR_DEVICE),
        d65 = const cpu::boot::block(65 << 30, cpu::boot::ATTR_DEVICE),
        d124 = const cpu::boot::block(124 << 30, cpu::boot::ATTR_DEVICE),
    );

    // De tellingen hierboven tegen het plan en de adressen.
    const _: () = {
        use board_raspi::map::{DEVICE_WINDOW, DMA, FIXED_END, LOADER, MB2};
        assert!(LOADER.base + LOADER.size == 128 * MB2);
        assert!(DEVICE_WINDOW.size == 32 * MB2);
        assert!(DMA.size == 8 * MB2);
        assert!(FIXED_END == 168 * MB2);
        assert!(super::PCIE2.0 >> 30 == 64 && super::MIP.0 >> 30 == 64);
        assert!(super::UART10.0 >> 30 == 65 && super::GIC.0 >> 30 == 65);
        assert!(super::VCMAIL.0 >> 30 == 65);
        assert!(super::RP1 >> 30 == 124);
    };

    #[cfg(all(target_arch = "aarch64", target_os = "none"))]
    pub(super) fn tables() -> Option<(Pa, Pa)> {
        unsafe extern "C" {
            static __boot_ttbr0: u8;
            static __pi_l2_gb0: u8;
        }
        let a = |p: *const u8| Pa(p as usize as u64);
        Some((a(&raw const __boot_ttbr0), a(&raw const __pi_l2_gb0)))
    }

    #[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
    pub(super) fn tables() -> Option<(Pa, Pa)> {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use board::Board;

    #[test]
    fn a76_numbers_cores_in_aff1() {
        assert_eq!(slots::mpidr(2), 0x200);
        assert_eq!(slots::core_of(0x8100_0300), 3);
        assert_eq!(<Bcm2712 as Soc>::core_of(<Bcm2712 as Soc>::mpidr(1)), 1);
    }

    #[test]
    fn the_board_has_a_name_and_the_nic_line_is_mip_vector_6() {
        assert_eq!(<Rpi5 as Board>::NAME, "rpi5");
        assert!(Bcm2712::tables().is_none());
        assert_eq!(32 + MIP_FIRST_SPI + RP1_INT_ETH, 166);
        assert_eq!(RP1_MSIX_CFG, 0x1f_0010_8808);
    }
}
