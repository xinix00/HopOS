//! Host-tests over nep-registerblokken in RAM.

use super::*;
use std::sync::atomic::{AtomicU64, Ordering::SeqCst};

static NOW: AtomicU64 = AtomicU64::new(0);

/// 100 µs per lezing: de milliseconden van de link-training lopen vlot af.
fn ticking() -> u64 {
    NOW.fetch_add(100_000, SeqCst)
}

fn block(len: u64) -> Vec<u64> {
    vec![0; (len as usize).div_ceil(8)]
}

fn pa(v: &mut [u64]) -> Pa {
    Pa(v.as_mut_ptr() as usize as u64)
}

const RP1_BASE: u64 = 0x1f_0000_0000;

fn rc(soc: Soc, base: Pa, sw: Pa) -> Rc {
    let inb = [
        Some(InWin {
            pcie: 0,
            cpu: RP1_BASE,
            size: 0x40_0000,
        }),
        Some(InWin {
            pcie: 0x10_0000_0000,
            cpu: 0,
            size: 0x10_0000_0000,
        }),
        Some(InWin {
            pcie: 0xff_ffff_f000,
            cpu: 0x10_0013_0000,
            size: 0x1000,
        }),
        None,
    ];
    let out = OutWin {
        cpu: RP1_BASE,
        pcie: 0,
        size: 0x1000_0000,
    };
    // SAFETY: de blokken leven de hele test.
    unsafe { Rc::new(soc, base, sw, 44, 2, out, inb, ticking) }
}

#[test]
fn window_encodings() {
    assert_eq!(in_size_enc(0x1000), 0x1c);
    assert_eq!(in_size_enc(0x8000), 0x1f);
    assert_eq!(in_size_enc(0x40_0000), 22 - 15);
    assert_eq!(in_size_enc(0x10_0000_0000), 36 - 15);
    assert_eq!(in_size_enc(0x800), 0);
    assert_eq!(in_size_enc(0), 0);
    let (bl, bh, lh) = out_encoding(OutWin {
        cpu: RP1_BASE,
        pcie: 0,
        size: 0x1000_0000,
    });
    // 0x1f_0000_0000 >> 20 = 0x1f000: laag 0x000, hoog 0x1f.
    assert_eq!(bl, (0x0ff << 20));
    assert_eq!((bh, lh), (0x1f, 0x1f));
    assert_eq!(ecam_index(1, 0, 0), 1 << 20);
    assert_eq!(ecam_index(2, 3, 1), (2 << 20) | (3 << 15) | (1 << 12));
}

#[test]
fn setup_programs_windows_ubus_and_the_pll() {
    let mut b = block(MMIO_SIZE);
    let mut sw = block(0x100);
    let (bp, sp) = (pa(&mut b), pa(&mut sw));
    dev::write32(bp.add(off::MISC_PCIE_STATUS), STATUS_RC_MODE);
    dev::write32(bp.add(off::HARD_DEBUG), HD_SERDES_IDDQ | 1);
    let r = rc(Soc::Bcm2712, bp, sp);
    let (rc_mode, pll) = r.setup();
    assert!(rc_mode);
    // De nep-MDIO wist bit 31 nooit: de PLL is niet bevestigd.
    assert!(!pll);
    assert_eq!(
        dev::read32(bp.add(off::HARD_DEBUG)),
        1,
        "SerDes out of IDDQ"
    );
    // Bridge-reset van id 44: bank 1 (+0x18), SET dan CLEAR (+4), bit 12.
    assert_eq!(dev::read32(sp.add(0x18)), 1 << 12);
    assert_eq!(dev::read32(sp.add(0x1c)), 1 << 12);
    // RC-BAR 2: DRAM op PCIe 0x10_0000_0000, 64 GB.
    assert_eq!(dev::read32(bp.add(0x402c + 8)), 21);
    assert_eq!(dev::read32(bp.add(0x402c + 12)), 0x10);
    // Zijn UBUS-remap: CPU 0 met ACCESS_EN.
    assert_eq!(dev::read32(bp.add(0x40ac + 8)), 1);
    // RC-BAR 3: het MIP-venster, remap naar 0x10_0013_0000.
    assert_eq!(dev::read32(bp.add(0x402c + 16)), 0xffff_f000 | 0x1c);
    assert_eq!(dev::read32(bp.add(0x40ac + 16)), 0x0013_0001);
    assert_eq!(dev::read32(bp.add(0x40ac + 20)), 0x10);
    // MAX_BURST 128 B op de BCM2712.
    assert_eq!(dev::read32(bp.add(off::MISC_CTRL)) & 0x30_0000, 0x10_0000);
    assert_eq!(dev::read32(bp.add(off::MISC_VDM_QOS_HI)), 0xbbaa_9888);
    assert_eq!(dev::read32(bp.add(off::CFG_PHY_CTL15)) & 0xff, 0x12);
    assert_eq!(dev::read32(bp.add(off::MISC_WIN0_BASE_HI)), 0x1f);
}

#[test]
fn bcm2711_has_no_ubus_and_resets_through_rgr1() {
    let mut b = block(MMIO_SIZE);
    let mut sw = block(0x100);
    let (bp, sp) = (pa(&mut b), pa(&mut sw));
    dev::write32(bp.add(off::MISC_PCIE_STATUS), STATUS_RC_MODE);
    let r = rc(Soc::Bcm2711, bp, sp);
    assert_eq!(r.setup(), (true, true));
    assert!(sw.iter().all(|&w| w == 0), "no external reset controller");
    assert_eq!(dev::read32(bp.add(0x40ac)), 0, "no UBUS remap");
    // PERST# blijft vast, de bridge is weer uit reset.
    assert_eq!(dev::read32(bp.add(off::RGR1_SW_INIT)), RGR1_PERST);
    assert_eq!(dev::read32(bp.add(off::MISC_CTRL)) & 0x30_0000, 0);
}

#[test]
fn not_a_root_complex_and_no_link_are_errors() {
    let mut b = block(MMIO_SIZE);
    let mut sw = block(0x100);
    let (bp, sp) = (pa(&mut b), pa(&mut sw));
    let r = rc(Soc::Bcm2712, bp, sp);
    // SAFETY: geen RESCAL-blok.
    let first = unsafe { r.bring_up(0, 0, &[]) };
    assert_eq!(first, Err(Error::NotRc { status: 0 }));
    dev::write32(bp.add(off::MISC_PCIE_STATUS), STATUS_RC_MODE);
    assert_eq!(
        // SAFETY: geen RESCAL-blok.
        unsafe { r.bring_up(0, 0, &[]) },
        Err(Error::NoLink {
            status: STATUS_RC_MODE
        })
    );
    // PERSTB is gelost, en de refclk-overrides staan uit.
    assert_ne!(dev::read32(bp.add(off::MISC_PCIE_CTRL)) & (1 << 2), 0);
    assert_eq!(dev::read32(bp.add(off::CFG_LNKCTL2)) & 0xf, 2);
}

#[test]
fn a_trained_link_checks_the_endpoint_and_assigns_bars() {
    let mut b = block(MMIO_SIZE);
    let mut sw = block(0x100);
    let (bp, sp) = (pa(&mut b), pa(&mut sw));
    dev::write32(
        bp.add(off::MISC_PCIE_STATUS),
        STATUS_RC_MODE | STATUS_PHY_LINK_UP | STATUS_DL_ACTIVE,
    );
    // De endpoint (RP1: vendor 0x1de4, device 0x0001) in het venster.
    dev::write32(bp.add(off::EXT_CFG_DATA), 0x0001_1de4);
    let r = rc(Soc::Bcm2712, bp, sp);
    let bars = [
        EpBar { off: 0x14, val: 0 },
        EpBar {
            off: 0x10,
            val: 0x100_0000,
        },
    ];
    // SAFETY: geen RESCAL-blok.
    unsafe { r.bring_up(0, 0x0001_1de4, &bars) }.unwrap();
    assert_eq!(dev::read32(bp.add(off::EXT_CFG_INDEX)), 1 << 20);
    assert_eq!(dev::read32(bp.add(off::EXT_CFG_DATA + 0x10)), 0x100_0000);
    assert_eq!(dev::read32(bp.add(off::EXT_CFG_DATA + 4)) & 6, 6);
    assert_eq!(dev::read32(bp.add(off::CFG_PRIMARY_BUS)), 0x01_0100);
    assert_eq!(dev::read32(bp.add(off::CFG_MEM_BASE)), 0x0ff0_0000);
    // Een andere endpoint is een fout.
    dev::write32(bp.add(off::EXT_CFG_DATA), 0x1234_5678);
    assert!(matches!(
        // SAFETY: geen RESCAL-blok.
        unsafe { r.bring_up(0, 0x0001_1de4, &[]) },
        Err(Error::Endpoint { .. })
    ));
}

#[test]
fn rescal_needs_start_to_stick_and_status_to_rise() {
    let mut blk = block(12);
    let p = pa(&mut blk);
    // Een nep-blok houdt START vast, maar STATUS komt nooit.
    // SAFETY: het blok leeft de hele test.
    assert!(!unsafe { rescal(p, ticking) });
    assert_eq!(dev::read32(p) & 1, 0, "START cleared again");
    dev::write32(p.add(8), 1);
    // SAFETY: zie hierboven.
    assert!(unsafe { rescal(p, ticking) });
}
