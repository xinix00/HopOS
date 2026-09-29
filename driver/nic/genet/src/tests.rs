//! Host-tests over een nep-registerblok en een nep-DMA-regio in RAM.

use super::*;
use netdev::Device;
use std::sync::atomic::{AtomicU64, Ordering::SeqCst};

static NOW: AtomicU64 = AtomicU64::new(0);

/// Een klok die bij elke lezing 1 µs verspringt.
fn ticking() -> u64 {
    NOW.fetch_add(1_000, SeqCst)
}

struct Fake {
    regs: Vec<u64>,
    dma: Vec<u64>,
}

impl Fake {
    fn new() -> Self {
        Self {
            regs: vec![0; (MMIO_SIZE as usize).div_ceil(8)],
            dma: vec![0; DMA_NEED as usize / 8],
        }
    }
    fn base(&mut self) -> Pa {
        Pa(self.regs.as_mut_ptr() as usize as u64)
    }
    fn dma(&mut self) -> Pa {
        Pa(self.dma.as_mut_ptr() as usize as u64)
    }
    fn nic(&mut self) -> Genet {
        let (b, d) = (self.base(), self.dma());
        // SAFETY: de vectoren leven langer dan de driver in elke test.
        unsafe { Genet::new(b, d, DMA_NEED, [2, 0x48, 1, 2, 3, 4], ticking) }
    }
}

const LINK_1G: Link = Link {
    mbps: 1000,
    full_duplex: true,
};

#[test]
fn rev_must_be_v5() {
    let mut f = Fake::new();
    let b = f.base();
    let n = f.nic();
    dev::write32(b, 0x0500_0000);
    assert_eq!(n.check_rev(), Err(Error::Rev { raw: 0x0500_0000 }));
    dev::write32(b, 0x0600_0000);
    assert!(n.check_rev().is_ok());
}

#[test]
fn reset_waits_for_the_dma_stop_confirmation() {
    let mut f = Fake::new();
    let b = f.base();
    let mut n = f.nic();
    assert_eq!(
        n.reset(),
        Err(Error::Stop {
            dir: "TX",
            status: 0
        })
    );
    dev::write32(b.add(TX_DMA + 0x48), DMA_DISABLED);
    dev::write32(b.add(RX_DMA + 0x48), DMA_DISABLED);
    dev::write32(b.add(TX_DMA + 0x44), DMA_ENABLE_MASK);
    n.reset().unwrap();
    assert_eq!(dev::read32(b.add(TX_DMA + 0x44)) & DMA_ENABLE_MASK, 0);
    assert_eq!(dev::read32(b.add(0x004)), 3, "external GPHY");
    assert_eq!(dev::read32(b.add(0x814)), 1536);
    assert_eq!(dev::read32(b.add(0x300)) & 2, 2, "ALIGN_2B");
    assert_eq!(dev::read32(b.add(0x210)), u32::MAX, "interrupts masked");
    assert_eq!(dev::read32(b.add(0x808)), 0, "SW_RESET cleared again");
}

#[test]
fn init_follows_the_hardware_counters_and_programs_the_mac() {
    let mut f = Fake::new();
    let b = f.base();
    let d = f.dma();
    // De hardware houdt een telling vast uit een vorig leven.
    dev::write32(b.add(RX_DMA + 0x08), 0x1_0105);
    dev::write32(b.add(TX_DMA + 0x08), 0x0203);
    let mut n = f.nic();
    n.init(LINK_1G).unwrap();
    assert_eq!(n.rx_cons, 0x0105);
    assert_eq!(n.tx_prod, 0x0203);
    assert_eq!(
        dev::read32(b.add(RX_DMA + 0x2c)),
        5 * 3,
        "RX read ptr in words"
    );
    assert_eq!(
        dev::read32(b.add(TX_DMA + 0x0c)),
        0x0203,
        "TX PROD follows CONS"
    );
    assert_eq!(dev::read32(b.add(0x80c)), 0x0248_0102);
    assert_eq!(dev::read32(b.add(0x810)), 0x0304);
    assert_eq!(dev::read32(b.add(0xe50)), (1 << 16) | (1 << 15));
    // Descriptor 7 wijst naar buffer 7.
    let bus = d.0 + 7 * BUF_SIZE as u64;
    assert_eq!(dev::read32(b.add(RX_BD + 7 * 12 + 4)), bus as u32);
    assert_eq!(dev::read32(b.add(RX_BD + 7 * 12 + 8)), (bus >> 32) as u32);
    // Gigabit, full duplex, TX en RX aan; ring 16 in de DMA.
    assert_eq!(dev::read32(b.add(0x808)), (2 << 2) | 3);
    assert_eq!(dev::read32(b.add(RX_DMA + 0x44)), RING16_EN | 1);
    assert_eq!(dev::read32(b.add(0x08c)), (1 << 4) | (1 << 6) | (1 << 16));
}

#[test]
fn a_small_dma_region_is_refused() {
    let mut f = Fake::new();
    let (b, d) = (f.base(), f.dma());
    // SAFETY: de vectoren leven de hele test.
    let mut n = unsafe { Genet::new(b, d, DMA_NEED - 1, [0; 6], ticking) };
    assert!(matches!(n.init(LINK_1G), Err(Error::Dma { .. })));
}

#[test]
fn transmit_fills_the_descriptor_with_qtag_and_kicks_prod() {
    let mut f = Fake::new();
    let b = f.base();
    let d = f.dma();
    let mut n = f.nic();
    n.tx_prod = 0xffff;
    dev::write32(b.add(TX_DMA + 0x08), 0xffff);
    n.transmit(&[0xab; 60]).unwrap();
    let i = 0xffff % N_BD;
    let ls = dev::read32(b.add(TX_BD + (i * 12) as u64));
    assert_eq!(ls >> 16, 60);
    assert_eq!(ls & TX_QTAG, TX_QTAG, "valkuil 1");
    assert_eq!(
        ls & (DMA_SOP | DMA_EOP | TX_APPEND_CRC),
        DMA_SOP | DMA_EOP | TX_APPEND_CRC
    );
    assert_eq!(
        dev::read32(b.add(TX_DMA + 0x0c)),
        0,
        "PROD wraps at 0x10000"
    );
    let buf = d.add((DMA_NEED / 2) + (i * BUF_SIZE) as u64);
    assert_eq!(dev::read8(buf), 0xab);
    assert_eq!(n.transmit(&[]), Err(TxError::Size(0)));
    assert_eq!(
        n.transmit(&[0; BUF_SIZE + 1]),
        Err(TxError::Size(BUF_SIZE + 1))
    );
}

#[test]
fn a_full_ring_says_full() {
    let mut f = Fake::new();
    let b = f.base();
    let mut n = f.nic();
    n.tx_prod = 300;
    dev::write32(b.add(TX_DMA + 0x08), 300 - N_BD as u32);
    assert_eq!(n.transmit(&[1; 64]), Err(TxError::Full));
    dev::write32(b.add(TX_DMA + 0x08), 300 - N_BD as u32 + 1);
    assert!(n.transmit(&[1; 64]).is_ok());
}

#[test]
fn receive_strips_the_pad_skips_bad_frames_and_returns_buffers() {
    let mut f = Fake::new();
    let b = f.base();
    let d = f.dma();
    let mut n = f.nic();
    let mut buf = [0u8; 2048];
    assert_eq!(n.receive(&mut buf), None);
    // Twee frames: een met een CRC-fout, dan een goed frame van 64 bytes.
    dev::write32(b.add(RX_BD), (66 << 16) | DMA_SOP | DMA_EOP | 0x2);
    dev::write32(b.add(RX_BD + 12), (66 << 16) | DMA_SOP | DMA_EOP);
    let payload = d.add(BUF_SIZE as u64 + 2);
    dev::copy_in(payload, &[0x5a; 64]);
    dev::write32(b.add(RX_DMA + 0x08), 2);
    assert_eq!(n.receive(&mut buf), Some(64));
    assert_eq!(buf[..64], [0x5a; 64]);
    assert_eq!(dev::read32(b.add(RX_DMA + 0x0c)), 2, "both buffers back");
    assert_eq!(n.receive(&mut buf), None);
}

#[test]
fn mdio_reports_a_stuck_bus_and_read_fail() {
    let mut f = Fake::new();
    let b = f.base();
    let mut n = f.nic();
    // De nep-bus wist START_BUSY nooit: een stall is een busfout.
    assert_eq!(
        Mdio::read(&mut n, 1, 2),
        Err(driver_mdio::Error::Bus { phy: 1, reg: 2 })
    );
    assert_ne!(dev::read32(b.add(0xe14)) & (1 << 29), 0);
}

#[test]
fn link_decides_speed_and_duplex() {
    assert_eq!(cmd_for(0xffff_ffff, LINK_1G) & 0x40c, 2 << 2);
    let half100 = Link {
        mbps: 100,
        full_duplex: false,
    };
    assert_eq!(cmd_for(0, half100), (1 << 2) | (1 << 10));
    assert_eq!(rx_len((2 << 16) | DMA_SOP | DMA_EOP), None);
    assert_eq!(rx_len((64 << 16) | DMA_SOP), None);
    assert_eq!(rx_len((1514 << 16) | DMA_SOP | DMA_EOP), Some(1512));
}
