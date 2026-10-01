//! `dwmac4_test.go` en `receive_bounds_test.go`, geport, plus de
//! registerprogrammering op een nep-registerblok in RAM.
//!
//! De bit-arithmetiek hoort op de host bewezen te worden en niet op een
//! bordje waar één ronde een SD-kaartwissel kost: precies dit ging mis bij
//! de vorige generatie (dwmac, 30-07), waar de buffergrootte door een te
//! smal veld naar nul werd gemaskeerd: link stond, TX liep, RX gaf
//! descriptors terug zonder één frame.

use super::*;
use netdev::Device as _;
use std::cell::Cell;
use std::vec::Vec;

thread_local! {
    static NOW: Cell<u64> = const { Cell::new(0) };
}

/// Een klok die per lees een milliseconde verspringt.
fn clock() -> u64 {
    NOW.with(|n| {
        n.set(n.get() + 1_000_000);
        n.get()
    })
}

/// Een 16-gealigneerd stuk RAM dat de test lang genoeg leeft.
struct Mem {
    _v: Vec<u64>,
    base: Pa,
}

fn mem(bytes: usize) -> Mem {
    let mut v = vec![0u64; bytes / 8 + 4];
    let base = Pa((v.as_mut_ptr() as u64).next_multiple_of(16));
    Mem { _v: v, base }
}

/// Een driver op een nep-registerblok en een nep-DMA-regio, zonder reset
/// (in RAM klaart de reset-bit nooit).
struct Fake {
    _regs: Mem,
    _dma: Mem,
    n: Dwmac4,
}

const TEST_MAC: Mac = Mac([0x02, 0x48, 0x4f, 0x50, 0x12, 0x34]);

fn fake() -> Fake {
    let regs = mem(regs::REGS_SIZE);
    let dma = mem(NEED_BYTES as usize);
    let n = Dwmac4 {
        base: regs.base,
        mac: TEST_MAC,
        ring: Rings::at(dma.base),
        rx_cur: 0,
        tx_cur: 0,
        rx_dirty: false,
        tx_dirty: false,
        irq: None,
        stats: Stats::default(),
    };
    Fake {
        _regs: regs,
        _dma: dma,
        n,
    }
}

/// Een draaiende nep-driver: FIFO's van 4 KB, dan `program`.
fn running() -> Fake {
    let mut f = fake();
    f.n.regs().hw_feature1.write((5 << 6) | 5);
    f.n.program(1000, true).unwrap();
    f
}

#[test]
fn mac_config_sets_the_right_speed_bits() {
    // PS = MII (10/100), FES = 100 in plaats van 10, geen van beide = GMII.
    for (mbps, fd, ps, fes, dm) in [
        (10, false, true, false, false),
        (10, true, true, false, true),
        (100, true, true, true, true),
        (1000, true, false, false, true),
        (1000, false, false, false, false),
    ] {
        let got = mac_config(0, mbps, fd);
        assert_eq!(got & CFG_PS != 0, ps, "{mbps}: PS in {got:#x}");
        assert_eq!(got & CFG_FES != 0, fes, "{mbps}: FES in {got:#x}");
        assert_eq!(got & CFG_DM != 0, dm, "{mbps} fd={fd}: DM");
    }
}

#[test]
fn mac_config_clears_the_old_speed_and_keeps_jumbo_off() {
    // Van 100 terug naar 1000: blijft FES staan, dan klokt een node na een
    // link-flap op de verkeerde snelheid. En jumbo moet uit blijven.
    let prev = mac_config(0, 100, true);
    assert_ne!(prev & CFG_FES, 0);
    let got = mac_config(prev | CFG_JE, 1000, true);
    assert_eq!(got & (CFG_FES | CFG_PS), 0, "{got:#x}");
    assert_eq!(got & CFG_JE, 0, "{got:#x}");
    assert_eq!(mac_config(got, 1000, true), got, "not idempotent");
}

#[test]
fn mac_config_keeps_core_init_and_leaves_acs_off() {
    let got = mac_config(0, 1000, true);
    assert_eq!(got & CORE_INIT, CORE_INIT);
    // Met ACS aan zou rx_len vier bytes te veel aftrekken.
    assert_eq!(got & CFG_ACS, 0);
}

#[test]
fn rx_control_carries_a_real_buffer_size() {
    let got = rx_control(0, BUF_SIZE).unwrap();
    assert_eq!(
        ((got & RX_RBSZ_MASK) >> RX_RBSZ_SHIFT) as usize,
        BUF_SIZE,
        "{got:#x}"
    );
    assert_ne!(got & (PBL << RX_PBL_SHIFT), 0);
    // Een tweede keer over de eerste heen verdubbelt of vervuilt niets.
    assert_eq!(rx_control(got, BUF_SIZE), Some(got));
}

#[test]
fn rx_control_refuses_a_size_that_does_not_fit() {
    for size in [0, 1537, 0x8000, usize::MAX] {
        assert_eq!(rx_control(0, size), None, "{size}");
    }
}

#[test]
fn tx_descriptor_words_carry_the_length_twice() {
    let (d2, d3) = tx_desc23(1000).unwrap();
    assert_eq!(d2, 1000);
    assert_eq!(d3 & TX_PKT_LEN_MASK, 1000);
    for bit in [TX_OWN, TX_FIRST, TX_LAST] {
        assert_ne!(d3 & bit, 0, "{d3:#x} misses {bit:#x}");
    }
    for n in [0, MAX_FRAME + 1, 0x10000] {
        assert_eq!(tx_desc23(n), None, "{n}");
    }
}

#[test]
fn rx_len_strips_the_fcs_and_refuses_the_impossible() {
    assert_eq!(rx_len(RX_OWN | RX_FIRST | RX_LAST | 1518), Some(1514));
    assert_eq!(rx_len(5), Some(1));
    assert_eq!(rx_len(BUF_SIZE as u32), Some(BUF_SIZE - FCS_LEN));
    for raw in [0, 1, FCS_LEN as u32, BUF_SIZE as u32 + 1, RX_PKT_LEN_MASK] {
        assert_eq!(rx_len(raw), None, "{raw}");
    }
    // De statusbits boven [14:0] lekken niet in de lengte.
    assert_eq!(rx_len(0xFFFF_8000 | 100), Some(96));
}

#[test]
fn the_rings_fit_need_bytes_without_overlap() {
    let r = Rings::at(Pa(0));
    let spans = [
        ("rx-desc", r.rx_desc.0, u64::from(NUM_RX) * DESC_SIZE),
        ("tx-desc", r.tx_desc.0, u64::from(NUM_TX) * DESC_SIZE),
        ("rx-buf", r.rx_buf.0, u64::from(NUM_RX) * BUF_SIZE as u64),
        ("tx-buf", r.tx_buf.0, u64::from(NUM_TX) * BUF_SIZE as u64),
    ];
    for (i, (an, ab, asz)) in spans.iter().enumerate() {
        assert!(ab + asz <= NEED_BYTES, "{an} beyond NEED_BYTES");
        for (bn, bb, bsz) in &spans[i + 1..] {
            assert!(ab + asz <= *bb || bb + bsz <= *ab, "{an} overlaps {bn}");
        }
    }
    assert_eq!(r.tx_buf(NUM_TX - 1).0 + BUF_SIZE as u64, NEED_BYTES);
}

#[test]
fn mdio_fields_sit_at_the_dwmac4_positions() {
    // Een verwisselde shift geeft een bus die stil naar het verkeerde
    // register schrijft; dit is de reden voor een eigen crate per generatie.
    let want = (3 << 21) | (5 << 16) | (CSR_100_150M << 8) | (3 << 2) | 1;
    assert_eq!(mdio_cmd(3, 5, CSR_100_150M, false), want);
    assert_eq!(
        mdio_cmd(3, 5, CSR_100_150M, true),
        (want & !(3 << 2)) | (1 << 2)
    );
    // Te grote adressen worden gemaskeerd, niet doorgeschoven.
    assert_eq!(mdio_cmd(0xFF, 0, 0, false) >> 21, 0x1F);
}

#[test]
fn fifo_sizes_come_from_hw_feature1() {
    assert_eq!(fifo_sizes((5 << 6) | 4), (4096, 2048));
    assert_eq!(fifo_sizes(0), (128, 128));
    assert_eq!(fifo_sizes(0x1F), (128, 0));
}

#[test]
fn program_lays_out_the_rings_and_the_registers() {
    let f = running();
    let n = &f.n;
    let r = n.regs();
    let c = &r.chan;
    // Ringen: basis, lengte (aantal min één), tails.
    assert_eq!(c.rx_base.read(), lo(n.ring.rx_desc));
    assert_eq!(c.tx_base.read(), lo(n.ring.tx_desc));
    assert_eq!(c.rx_ring_len.read(), u32::from(NUM_RX) - 1);
    assert_eq!(c.tx_ring_len.read(), u32::from(NUM_TX) - 1);
    assert_eq!(c.rx_end.read(), lo(n.ring.tx_desc)); // één voorbij RX
    assert_eq!(c.tx_end.read(), lo(n.ring.tx_desc));
    // RX-descriptors: van de DMA, met hun eigen buffer; TX leeg.
    for i in [0, 1, NUM_RX - 1] {
        let d = n.ring.rx(i);
        assert_eq!(dev::read32(d), lo(n.ring.rx_buf(i)), "rx {i}");
        assert_eq!(dev::read32(d.add(12)), RX_OWN | RX_BUF1_VALID | RX_IOC);
    }
    assert_eq!(dev::read32(n.ring.tx(NUM_TX - 1).add(12)), 0);
    // De AXI-kant zoals de DTS hem voorschrijft.
    assert_eq!(r.dma_sys_bus_mode.read(), SYS_BUS_MODE);
    // MTL: 4 KB-FIFO's, dus TQS = RQS = 15.
    assert_eq!(
        r.mtl_tx_op_mode.read(),
        MTL_TSF | MTL_TXQ_EN | (15 << MTL_TQS_SHIFT)
    );
    assert_eq!(r.mtl_rx_op_mode.read(), MTL_RSF | (15 << MTL_RQS_SHIFT));
    // RBSZ en start.
    let rxc = c.rx_control.read();
    assert_eq!(((rxc & RX_RBSZ_MASK) >> RX_RBSZ_SHIFT) as usize, BUF_SIZE);
    assert_ne!(rxc & CHAN_START, 0);
    assert_ne!(c.tx_control.read() & CHAN_START, 0);
    // Het MAC-adres in de filter, en de MAC aan op gigabit full duplex.
    let m = TEST_MAC.0;
    assert_eq!(
        r.addr0_lo.read(),
        u32::from_le_bytes([m[0], m[1], m[2], m[3]])
    );
    assert_eq!(r.addr0_hi.read(), (u32::from(m[5]) << 8) | u32::from(m[4]));
    assert_eq!(r.rxq_ctrl0.read(), RXQ0_DCB_ENABLE);
    let cfg = r.config.read();
    assert_eq!(cfg & (CFG_TE | CFG_RE | CFG_DM), CFG_TE | CFG_RE | CFG_DM);
    assert_eq!(cfg & SPEED_MASK, 0);
    // Interrupts dicht tot het board ze bedraadt.
    assert_eq!(c.intr_ena.read(), 0);
}

#[test]
fn an_unclocked_block_is_refused_on_its_fifo_sizes() {
    let mut f = fake();
    // HW_FEATURE1 leest nul: 128 B-FIFO's, en fifo/256 - 1 zou omlopen.
    assert_eq!(
        f.n.program(1000, true),
        Err(Error::Fifo {
            tx: 128,
            rx: 128,
            hw_feature1: 0
        })
    );
}

#[test]
fn a_stuck_reset_and_a_small_region_are_refused() {
    let regs = mem(regs::REGS_SIZE);
    let dma = mem(64);
    // SAFETY: het nep-blok is groot genoeg en leeft de hele test.
    let mut p = unsafe { Probe::new(regs.base, CSR_100_150M, clock) };
    assert_eq!(p.check(), Err(Error::NoMac { version: 0 }));
    // In RAM blijft de reset-bit staan: precies het beeld van 05-08.
    assert_eq!(p.reset(), Err(Error::ResetStuck { bus_mode: 1 }));
    // SAFETY: zie hierboven; `start` weigert vóór hij iets aanraakt.
    let e = unsafe { p.start(dma.base, 64, TEST_MAC, 1000, true) };
    assert_eq!(
        e.err(),
        Some(Error::DmaTooSmall {
            need: NEED_BYTES,
            have: 64
        })
    );
}

#[test]
fn a_busy_mdio_is_a_bus_error_and_the_command_is_right() {
    let regs = mem(regs::REGS_SIZE);
    // SAFETY: het nep-blok leeft de hele test.
    let mut p = unsafe { Probe::new(regs.base, CSR_100_150M, clock) };
    // Vrij: het commando gaat erin, maar in RAM klaart BUSY nooit.
    assert_eq!(
        p.read(1, 2),
        Err(driver_mdio::Error::Bus { phy: 1, reg: 2 })
    );
    assert_eq!(p.mdio_state(), mdio_cmd(1, 2, CSR_100_150M, false));
    // Bezet: er wordt niets geschreven.
    assert_eq!(
        p.write(1, 0, 0x1200),
        Err(driver_mdio::Error::Bus { phy: 1, reg: 0 })
    );
    assert_eq!(p.regs().mdio_data.read(), 0);
    // De scan ziet een hangende bus als lege bus, niet als PHY.
    assert_eq!(driver_mdio::scan(&mut p), None);
}

#[test]
fn receive_bounds_and_recycle() {
    for size in [64, BUF_SIZE - FCS_LEN, BUF_SIZE - FCS_LEN + 1] {
        let mut f = running();
        let n = &mut f.n;
        let d = n.ring.rx(0);
        dev::write32(d.add(12), (size + FCS_LEN) as u32 | RX_FIRST | RX_LAST);
        dev::copy_in(n.ring.rx_buf(0), &vec![0x5a; BUF_SIZE]);
        let mut out = vec![0xccu8; 8192];
        let got = n.receive_one(&mut out);
        let want = (size <= BUF_SIZE - FCS_LEN).then_some(size);
        assert_eq!(got, want, "{size}");
        let k = got.unwrap_or(0);
        assert!(out[..k].iter().all(|&b| b == 0x5a), "{size}: packet");
        assert!(out[k..].iter().all(|&b| b == 0xcc), "{size}: beyond");
        // Teruggegeven, met het bufferadres er opnieuw in.
        assert_eq!(n.rx_cur, 1, "{size}");
        assert_eq!(dev::read32(d), lo(n.ring.rx_buf(0)));
        assert_ne!(dev::read32(d.add(12)) & RX_OWN, 0);
        // De tail pas in de flush: batching.
        assert_eq!(n.regs().chan.rx_end.read(), lo(n.ring.tx_desc));
        n.flush();
        assert_eq!(n.regs().chan.rx_end.read(), lo(n.ring.rx(1)));
    }
}

#[test]
fn a_bad_frame_is_counted_and_skipped() {
    let mut f = running();
    let n = &mut f.n;
    // Descriptor 0: foutframe; descriptor 1: een gesplitst frame (FD
    // zonder LD); descriptor 2: goed.
    dev::write32(
        n.ring.rx(0).add(12),
        RX_ERR_SUMMARY | RX_FIRST | RX_LAST | 68,
    );
    dev::write32(n.ring.rx(1).add(12), RX_FIRST | 68);
    dev::write32(n.ring.rx(2).add(12), RX_FIRST | RX_LAST | 68);
    let mut out = [0u8; 2048];
    assert_eq!(n.receive(&mut out), Some(64));
    assert_eq!((n.stats.rx_errors, n.stats.rx_frames), (2, 1));
    assert_eq!(n.stats.rx_last_err, RX_FIRST | 68);
    assert_eq!(n.rx_cur, 3);
    assert_eq!(n.receive(&mut out), None);
}

#[test]
fn a_frame_of_only_fcs_is_no_frame() {
    let mut f = running();
    let n = &mut f.n;
    // Descriptor 0 meldt vier bytes: alleen een FCS. Descriptor 1 is nog
    // van de DMA.
    dev::write32(n.ring.rx(0).add(12), RX_FIRST | RX_LAST | FCS_LEN as u32);
    let mut out = [0u8; 2048];
    assert_eq!(n.receive(&mut out), None);
    assert_eq!((n.stats.rx_bad_len, n.stats.rx_frames), (1, 0));
    assert_eq!(n.stats.rx_last_err, RX_FIRST | RX_LAST | FCS_LEN as u32);
    assert_eq!(n.rx_cur, 1);
}

#[test]
fn transmit_batches_until_flush_and_stops_when_full() {
    let mut f = running();
    let n = &mut f.n;
    assert_eq!(n.transmit(&[]), Err(TxError::Size(0)));
    assert_eq!(
        n.transmit(&[0; MAX_FRAME + 1]),
        Err(TxError::Size(MAX_FRAME + 1))
    );
    n.transmit(&[1; 60]).unwrap();
    let d = n.ring.tx(0);
    assert_eq!(dev::read32(d), lo(n.ring.tx_buf(0)));
    assert_eq!(dev::read32(d.add(8)), 60);
    assert_eq!(dev::read32(d.add(12)), TX_OWN | TX_FIRST | TX_LAST | 60);
    // Geen doorbell vóór de flush, één erna, en geen tweede zonder werk.
    assert_eq!(n.regs().chan.tx_end.read(), lo(n.ring.tx(0)));
    n.flush();
    assert_eq!(n.regs().chan.tx_end.read(), lo(n.ring.tx(1)));
    n.flush();
    assert_eq!(n.stats.doorbells, 1);
    // De ring rond: de DMA gaf niets terug, dus plek 0 is nog van hem.
    for _ in 1..NUM_TX {
        n.transmit(&[2; 60]).unwrap();
    }
    assert_eq!(n.transmit(&[3; 60]), Err(TxError::Full));
    // De DMA verzond descriptor 0 (schrijfvorm: OWN weg, status erin): er
    // is weer plaats, en het bufferadres komt er opnieuw in.
    dev::write32(d, 0xdead_beef);
    dev::write32(d.add(12), TX_FIRST | TX_LAST);
    n.transmit(&[4; 60]).unwrap();
    assert_eq!(dev::read32(d), lo(n.ring.tx_buf(0)));
}

#[test]
fn the_irq_masks_on_ack_and_rearms_when_the_ring_is_empty() {
    static BELL: Signal = Signal::new();
    let mut f = running();
    let n = &mut f.n;
    n.set_irq(&BELL);
    let c = &n.regs().chan;
    assert_eq!(c.intr_ena.read(), INTR_NIE | INTR_RIE);
    c.status.write(STAT_RI | STAT_NIS);
    let ack = n.irq_ack();
    assert_eq!(ack.ack(), STAT_RI | STAT_NIS);
    assert_eq!(c.intr_ena.read(), 0, "ack leaves the line masked");
    // De pomp leest de ring leeg: het masker gaat weer open, één keer.
    let mut out = [0u8; 64];
    let rearms = n.stats.rearms;
    assert_eq!(n.receive(&mut out), None);
    assert_eq!(c.intr_ena.read(), INTR_NIE | INTR_RIE);
    assert_eq!(n.receive(&mut out), None);
    assert_eq!(n.stats.rearms, rearms + 1);
}

#[test]
fn diag_is_one_line_with_the_numbers() {
    let f = running();
    let s = f.n.diag().to_string();
    assert!(!s.contains('\n'));
    assert!(s.contains("rxdesc[0] 0xc1000000"), "{s}");
}
