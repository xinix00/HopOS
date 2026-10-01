//! `dwmac_test.go` en `receive_bounds_test.go`, geport, plus de
//! descriptorbits, de ringlayout op cachelines en de registerprogrammering
//! op een nep-registerblok in RAM.
//!
//! De descriptorwoorden zijn bit-arithmetiek en horen op de host bewezen te
//! worden, niet op een bordje waar één ronde een kaartwissel kost. Deze
//! tests bestaan omdat precies dit fout ging op de eerste DMA-boot (30-07):
//! de buffergrootte werd met een 11-bits masker naar nul geveegd en de MAC
//! kreeg "elke buffer is 0 bytes" te horen; link stond, TX liep, RX gaf 128
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

/// Een op een cacheline gealigneerd stuk RAM dat de test lang genoeg leeft.
struct Mem {
    _v: Vec<u64>,
    base: Pa,
}

fn mem(bytes: usize) -> Mem {
    let mut v = vec![0u64; bytes / 8 + 16];
    let base = Pa((v.as_mut_ptr() as u64).next_multiple_of(LINE));
    Mem { _v: v, base }
}

/// Een driver op een nep-registerblok en een nep-DMA-regio, zonder reset
/// (in RAM klaart de reset-bit nooit).
struct Fake {
    _regs: Mem,
    _dma: Mem,
    n: Dwmac,
}

const TEST_MAC: Mac = Mac([0x02, 0x48, 0x4f, 0x50, 0x12, 0x34]);

fn fake() -> Fake {
    let regs = mem(regs::REGS_SIZE);
    let dma = mem(NEED_BYTES as usize);
    let n = Dwmac {
        base: regs.base,
        mac: TEST_MAC,
        ring: Rings::at(dma.base),
        rx_cur: 0,
        tx_cur: 0,
        rx_dirty: false,
        tx_dirty: false,
        stats: Stats::default(),
    };
    Fake {
        _regs: regs,
        _dma: dma,
        n,
    }
}

/// Een draaiende nep-driver op 100 Mbit full duplex.
fn running() -> Fake {
    let mut f = fake();
    f.n.program(mac_conf(100, true).unwrap());
    f
}

// --- dwmac_test.go --------------------------------------------------------

#[test]
fn rx_cntl_reports_a_real_buffer_size() {
    let c = rx_cntl(false);
    // Een maat die door het masker valt, betekent nul-byte buffers.
    assert_eq!((c & CNTL_SIZE1_MASK) as usize, MAX_FRAME);
    assert_eq!(c & RING_END, 0, "ring end on a descriptor that is not last");
}

#[test]
fn rx_cntl_sets_ring_end_on_the_last() {
    let c = rx_cntl(true);
    assert_ne!(c & RING_END, 0, "last descriptor without ring end");
    assert_eq!((c & CNTL_SIZE1_MASK) as usize, MAX_FRAME);
}

/// `MAX_FRAME` moet in het veld passen én boven een volledig ethernetframe
/// liggen (1518 = 1500 MTU + 14 kop + 4 FCS), anders splitst de MAC frames
/// over meerdere descriptors en keurt `receive` ze allemaal af. De crate
/// bewijst dit al bij het compileren; deze test houdt de getallen zichtbaar.
#[test]
fn max_frame_fits_the_field_and_exceeds_the_mtu() {
    const {
        assert!(MAX_FRAME as u32 & !CNTL_SIZE1_MASK == 0);
        assert!(MAX_FRAME >= 1518);
        assert!(MAX_FRAME <= BUF_SIZE);
    }
    // En 2048, de maat die de vendor als buffer heeft, zou er niet in passen.
    assert_eq!(2048 & CNTL_SIZE1_MASK, 0);
}

#[test]
fn tx_cntl_sets_first_last_and_length() {
    let c = tx_cntl(64, false).unwrap();
    // Zonder FS+LS wacht de MAC op de rest van een frame dat niet komt.
    assert_eq!(
        c & (TX_CNTL_FIRST | TX_CNTL_LAST),
        TX_CNTL_FIRST | TX_CNTL_LAST
    );
    assert_eq!(c & CNTL_SIZE1_MASK, 64);
    assert_eq!(c & RING_END, 0);
    let c = tx_cntl(1518, true).unwrap();
    assert_ne!(c & RING_END, 0, "{c:#010x}");
    assert_eq!(c & CNTL_SIZE1_MASK, 1518);
}

#[test]
fn tx_cntl_refuses_what_does_not_fit_the_field() {
    for n in [0, MAX_FRAME + 1, BUF_SIZE, 0x800] {
        assert_eq!(tx_cntl(n, false), None, "{n}");
    }
    assert!(tx_cntl(MAX_FRAME, false).is_some());
}

/// De ringen en buffers moeten binnen de gereserveerde DMA-regio blijven;
/// het board rekent `NEED_BYTES` na in zijn plan.
#[test]
fn need_bytes_covers_rings_and_buffers() {
    let n = u64::from(NUM_DESC);
    assert_eq!(NEED_BYTES, 2 * n * DESC_STRIDE + 2 * n * BUF_SIZE as u64);
    assert_eq!(NEED_BYTES, 432 * 1024);
    // DESC_STRIDE, niet DESC_SIZE: de descriptors liggen een cacheline uit
    // elkaar (DSL), dus de ringen zijn 4× zo groot als hun inhoud. De Go-test
    // keek eerst naar descSize en zag daardoor niet dat de TX-ring bij 64
    // descriptors bovenop de eerste RX-buffers landde (30-07).
    let r = Rings::at(Pa(0));
    assert!(r.tx(NUM_DESC - 1).0 + DESC_STRIDE <= r.rx_buf.0);
}

// --- receive_bounds_test.go -----------------------------------------------

/// Het echte receive-pad met een grote bestemming: een lengte uit het device
/// mag nooit bytes uit de naburige DMA-buffer blootgeven.
#[test]
fn receive_bounds_and_recycle() {
    for size in [64, MAX_FRAME - FCS_LEN, MAX_FRAME - FCS_LEN + 1] {
        let mut f = running();
        let n = &mut f.n;
        let d = n.ring.rx(0);
        dev::write32(
            d.add(DES_STATUS),
            ((size + FCS_LEN) as u32) << RX_LEN_SHIFT | RX_STS_FIRST | RX_STS_LAST,
        );
        // De hele buffer én de volgende gevuld: wie te ver leest, ziet 0x5a.
        dev::copy_in(n.ring.rx_buf(0), &vec![0x5a; 2 * BUF_SIZE]);
        let mut out = vec![0xccu8; 8192];
        let got = n.receive(&mut out);
        let want = (size <= MAX_FRAME - FCS_LEN).then_some(size);
        assert_eq!(got, want, "{size}");
        let k = got.unwrap_or(0);
        assert!(
            out[..k].iter().all(|&b| b == 0x5a),
            "{size}: packet differs"
        );
        assert!(
            out[k..].iter().all(|&b| b == 0xcc),
            "{size}: destination modified beyond returned packet"
        );
        // Teruggegeven, met de controle en het adres nog op hun plek.
        assert_eq!(n.rx_cur, 1, "{size}: not recycled");
        assert_ne!(dev::read32(d.add(DES_STATUS)) & DESC_OWN, 0);
        assert_eq!(dev::read32(d.add(DES_CNTL)), rx_cntl(false));
        assert_eq!(dev::read32(d.add(DES_BUF)), lo(n.ring.rx_buf(0)));
        if want.is_none() {
            assert_eq!(n.stats.rx_bad_len, 1);
        }
    }
}

// --- de descriptorbits ----------------------------------------------------

#[test]
fn rx_len_strips_the_fcs_and_refuses_the_impossible() {
    let sts = |raw: u32| (raw << RX_LEN_SHIFT) | RX_STS_FIRST | RX_STS_LAST;
    assert_eq!(rx_len(sts(1518)), Some(1514));
    assert_eq!(rx_len(sts(5)), Some(1));
    assert_eq!(rx_len(sts(MAX_FRAME as u32)), Some(MAX_FRAME - FCS_LEN));
    for raw in [0, 1, FCS_LEN as u32, MAX_FRAME as u32 + 1, RX_LEN_MASK] {
        assert_eq!(rx_len(sts(raw)), None, "{raw}");
    }
    // OWN en de statusbits lekken niet in de lengte.
    assert_eq!(rx_len(DESC_OWN | RX_STS_ERROR | sts(100)), Some(96));
}

#[test]
fn rx_whole_wants_first_and_last_and_no_error() {
    assert!(rx_whole(RX_STS_FIRST | RX_STS_LAST));
    assert!(!rx_whole(RX_STS_FIRST));
    assert!(!rx_whole(RX_STS_LAST));
    assert!(!rx_whole(0));
    assert!(!rx_whole(RX_STS_ERROR | RX_STS_FIRST | RX_STS_LAST));
}

#[test]
fn descriptor_fields_sit_at_the_databook_positions() {
    // Normal format: OWN bovenin RDES0/TDES0, RER/TER bit 25, FS/LS in TDES1
    // op 29/30 en in RDES0 op 9/8, de lengte in RDES0 [29:16].
    assert_eq!(DESC_OWN, 0x8000_0000);
    assert_eq!(RING_END, 0x0200_0000);
    assert_eq!(TX_CNTL_FIRST | TX_CNTL_LAST, 0x6000_0000);
    assert_eq!(RX_STS_FIRST | RX_STS_LAST, 0x300);
    assert_eq!(RX_LEN_MASK << RX_LEN_SHIFT, 0x3FFF_0000);
    assert_eq!((DES_STATUS, DES_CNTL, DES_BUF, DES_NEXT), (0, 4, 8, 12));
    assert_eq!(DES_NEXT + 4, DESC_SIZE);
}

#[test]
fn bus_mode_carries_the_skip_length_for_one_descriptor_per_line() {
    // DSL [6:2] = 12 woorden = 48 bytes skip, plus 16 bytes descriptor = 64.
    assert_eq!((BUS_MODE >> 2) & 0x1F, 12);
    assert_eq!(u64::from((BUS_MODE >> 2) & 0x1F) * 4 + DESC_SIZE, LINE);
    assert_eq!(BUS_MODE & BUS_SW_RESET, 0);
    // ALTDESCRIPTOR (bit 7) blijft uit: het normal format.
    assert_eq!(BUS_MODE & (1 << 7), 0);
    assert_eq!(BUS_MODE, (1 << 16) | (8 << 8) | (12 << 2));
}

#[test]
fn every_descriptor_and_buffer_owns_its_cachelines() {
    // De les van 30-07: een clean van de één mag nooit de DMA-schrijf in de
    // ander overschrijven, en een invalidate van de één nooit een
    // CPU-schrijf naar de ander weggooien. Dus: elk begin op een line, en
    // geen line gedeeld.
    let base = Pa(0x8000_0000);
    let r = Rings::at(base);
    let mut spans = Vec::new();
    for i in 0..NUM_DESC {
        spans.push((r.rx(i).0, DESC_SIZE));
        spans.push((r.tx(i).0, DESC_SIZE));
        spans.push((r.rx_buf(i).0, BUF_SIZE as u64));
        spans.push((r.tx_buf(i).0, BUF_SIZE as u64));
    }
    for &(a, len) in &spans {
        assert!(Pa(a).is_aligned(LINE), "{a:#x} not on a line");
        assert!(a + len <= base.0 + NEED_BYTES, "{a:#x} beyond NEED_BYTES");
    }
    // Lines per stuk: disjunct over alle 512.
    spans.sort_unstable();
    for w in spans.windows(2) {
        let (a, alen) = w[0];
        let (b, _) = w[1];
        let a_last_line = (a + alen - 1) / LINE;
        assert!(a_last_line < b / LINE, "{a:#x} and {b:#x} share a line");
    }
    // En de regio eindigt precies op de laatste TX-buffer.
    assert_eq!(
        r.tx_buf(NUM_DESC - 1).0 + BUF_SIZE as u64,
        base.0 + NEED_BYTES
    );
}

#[test]
fn start_refuses_a_region_off_a_cacheline_or_above_4g() {
    for dma in [Pa(0x8000_0010), Pa(0x8000_0020), Pa(0xFFFF_0000)] {
        let regs = mem(regs::REGS_SIZE);
        // SAFETY: het nep-blok leeft de hele test; `start` weigert vóór hij
        // iets aanraakt.
        let p = unsafe { Probe::new(regs.base, CSR_250_300M, clock) };
        // SAFETY: zie hierboven.
        let e = unsafe { p.start(dma, NEED_BYTES, TEST_MAC, 100, true) };
        assert_eq!(e.err(), Some(Error::DmaPlace { base: dma.0 }), "{dma:?}");
    }
}

#[test]
fn start_refuses_a_speed_rmii_does_not_have() {
    let regs = mem(regs::REGS_SIZE);
    // SAFETY: het nep-blok leeft de hele test.
    let p = unsafe { Probe::new(regs.base, CSR_250_300M, clock) };
    // SAFETY: zie hierboven; `start` weigert vóór hij iets aanraakt.
    let e = unsafe { p.start(Pa(0x8000_0000), NEED_BYTES, TEST_MAC, 1000, true) };
    assert_eq!(e.err(), Some(Error::Speed { mbps: 1000 }));
    assert_eq!(regs::REGS_SIZE, 0x1050);
}

#[test]
fn a_stuck_reset_and_a_small_region_are_refused() {
    let regs = mem(regs::REGS_SIZE);
    // SAFETY: het nep-blok leeft de hele test.
    let mut p = unsafe { Probe::new(regs.base, CSR_250_300M, clock) };
    assert_eq!(p.check(), Err(Error::NoMac { version: 0 }));
    // In RAM blijft de reset-bit staan.
    assert_eq!(p.reset(), Err(Error::ResetStuck { bus_mode: 1 }));
    // SAFETY: zie hierboven; `start` weigert vóór hij iets aanraakt.
    let e = unsafe { p.start(Pa(0x8000_0000), 64, TEST_MAC, 100, true) };
    assert_eq!(
        e.err(),
        Some(Error::DmaTooSmall {
            need: NEED_BYTES,
            have: 64
        })
    );
}

#[test]
fn mac_conf_sets_the_rmii_bits() {
    let base = CONF_PORT_MII | CONF_DIS_RX_OWN | CONF_TX_EN | CONF_RX_EN;
    assert_eq!(mac_conf(10, false), Some(base));
    assert_eq!(mac_conf(10, true), Some(base | CONF_DUPLEX));
    assert_eq!(mac_conf(100, true), Some(base | CONF_FES100 | CONF_DUPLEX));
    assert_eq!(mac_conf(100, false), Some(base | CONF_FES100));
    assert_eq!(mac_conf(1000, true), None);
}

#[test]
fn mdio_fields_sit_at_the_dwmac1000_positions() {
    // Go: phy << 11 | reg << 6 | 0x14 | busy (| write).
    assert_eq!(mdio_cmd(0, 2, CSR_250_300M, false), (2 << 6) | 0x14 | 1);
    assert_eq!(
        mdio_cmd(3, 4, CSR_250_300M, true),
        (3 << 11) | (4 << 6) | 0x14 | 2 | 1
    );
    // Te grote adressen worden gemaskeerd, niet doorgeschoven.
    assert_eq!(mdio_cmd(0xFF, 0, 0, false) >> 11, 0x1F);
    assert_eq!(mdio_cmd(0, 0xFF, 0, false), (0x1F << 6) | 1);
}

#[test]
fn a_busy_mdio_is_a_bus_error_and_the_command_is_right() {
    let regs = mem(regs::REGS_SIZE);
    // SAFETY: het nep-blok leeft de hele test.
    let mut p = unsafe { Probe::new(regs.base, CSR_250_300M, clock) };
    // Vrij: het commando gaat erin, maar in RAM klaart BUSY nooit.
    assert_eq!(
        p.read(0, 2),
        Err(driver_mdio::Error::Bus { phy: 0, reg: 2 })
    );
    assert_eq!(
        p.regs().gmii_addr.read(),
        mdio_cmd(0, 2, CSR_250_300M, false)
    );
    // Bezet: er wordt niets geschreven.
    assert_eq!(
        p.write(0, 0, 0x1200),
        Err(driver_mdio::Error::Bus { phy: 0, reg: 0 })
    );
    assert_eq!(p.regs().gmii_data.read(), 0);
    // De scan ziet een hangende bus als lege bus, niet als PHY.
    assert_eq!(driver_mdio::scan(&mut p), None);
}

#[test]
fn missed_counter_splits_ring_fifo_and_overflow() {
    assert_eq!(missed(0), (0, 0, 0));
    assert_eq!(missed(0x1234), (0x1234, 0, 0));
    assert_eq!(missed(1 << 16), (0, 0, 1));
    assert_eq!(missed(0x7FF << 17), (0, 0x7FF, 0));
    assert_eq!(missed((1 << 28) | (1 << 16) | (3 << 17) | 7), (7, 3, 2));
}

// --- de ringen in RAM -----------------------------------------------------

#[test]
fn program_lays_out_the_rings_and_the_registers() {
    let f = running();
    let n = &f.n;
    let r = n.regs();
    assert_eq!(r.rx_list.read(), lo(n.ring.rx_desc));
    assert_eq!(r.tx_list.read(), lo(n.ring.tx_desc));
    assert_eq!(r.bus_mode.read(), BUS_MODE);
    // RX: van de DMA, met de gemelde maat, ring-end alleen op de laatste.
    for i in 0..NUM_DESC {
        let d = n.ring.rx(i);
        assert_eq!(dev::read32(d.add(DES_STATUS)), DESC_OWN, "rx {i}");
        assert_eq!(dev::read32(d.add(DES_CNTL)), rx_cntl(i == NUM_DESC - 1));
        assert_eq!(dev::read32(d.add(DES_BUF)), lo(n.ring.rx_buf(i)));
        let t = n.ring.tx(i);
        assert_eq!(dev::read32(t.add(DES_STATUS)), 0, "tx {i}");
        assert_eq!(dev::read32(t.add(DES_BUF)), lo(n.ring.tx_buf(i)));
    }
    // De ruimte tussen twee descriptors is leeg: de DMA slaat hem over.
    assert_eq!(dev::read32(n.ring.rx(0).add(DESC_SIZE)), 0);
    let m = TEST_MAC.0;
    assert_eq!(
        r.addr0_lo.read(),
        u32::from_le_bytes([m[0], m[1], m[2], m[3]])
    );
    assert_eq!(r.addr0_hi.read(), (u32::from(m[5]) << 8) | u32::from(m[4]));
    assert_eq!(r.filter.read(), FILTER_PM | FILTER_PR);
    assert_eq!(r.conf.read(), mac_conf(100, true).unwrap());
    let op = r.op_mode.read();
    let want = OP_STORE_FORWARD | OP_FLUSH_TX_FIFO | OP_TX_START | OP_RX_START;
    assert_eq!(op, want, "{op:#x}");
}

#[test]
fn a_bad_frame_is_counted_and_skipped() {
    let mut f = running();
    let n = &mut f.n;
    let sts = |raw: u32| (raw << RX_LEN_SHIFT) | RX_STS_FIRST | RX_STS_LAST;
    // 0: foutframe; 1: gesplitst (FS zonder LS); 2: runt; 3: goed.
    dev::write32(n.ring.rx(0), RX_STS_ERROR | sts(68));
    dev::write32(n.ring.rx(1), (68 << RX_LEN_SHIFT) | RX_STS_FIRST);
    dev::write32(n.ring.rx(2), sts(3));
    dev::write32(n.ring.rx(3), sts(68));
    let mut out = [0u8; 2048];
    assert_eq!(n.receive(&mut out), Some(64));
    let s = n.stats;
    assert_eq!((s.rx_errors, s.rx_bad_len, s.rx_frames), (2, 1, 1));
    assert_eq!(s.rx_last_err, sts(3));
    assert_eq!(n.rx_cur, 4);
    // Leeg: de RX-poll-demand valt meteen, want er ging werk terug.
    assert_eq!(n.receive(&mut out), None);
    assert_eq!(n.regs().rx_poll.read(), 1);
    assert_eq!(n.stats.doorbells, 1);
}

#[test]
fn a_frame_larger_than_the_caller_buffer_is_cut() {
    let mut f = running();
    let n = &mut f.n;
    dev::write32(
        n.ring.rx(0),
        (1518 << RX_LEN_SHIFT) | RX_STS_FIRST | RX_STS_LAST,
    );
    dev::copy_in(n.ring.rx_buf(0), &[7; 1514]);
    let mut out = [0u8; 100];
    assert_eq!(n.receive(&mut out), Some(100));
    assert!(out.iter().all(|&b| b == 7));
}

#[test]
fn receive_wraps_the_ring() {
    let mut f = running();
    let n = &mut f.n;
    let mut out = [0u8; 2048];
    for round in 0..2 {
        for i in 0..NUM_DESC {
            let v = (60 + u32::from(i) % 64 + FCS_LEN as u32) << RX_LEN_SHIFT;
            dev::write32(n.ring.rx(i), v | RX_STS_FIRST | RX_STS_LAST);
        }
        for i in 0..NUM_DESC {
            let want = 60 + usize::from(i) % 64;
            assert_eq!(n.receive(&mut out), Some(want), "round {round} desc {i}");
        }
        // Rond: terug bij 0, en alles weer van de DMA.
        assert_eq!(n.rx_cur, 0);
        assert_eq!(n.receive(&mut out), None);
    }
    assert_eq!(n.stats.rx_frames, 2 * u64::from(NUM_DESC));
}

#[test]
fn transmit_fills_the_descriptor_and_batches_until_flush() {
    let mut f = running();
    let n = &mut f.n;
    assert_eq!(n.transmit(&[]), Err(TxError::Size(0)));
    assert_eq!(
        n.transmit(&[0; MAX_FRAME + 1]),
        Err(TxError::Size(MAX_FRAME + 1))
    );
    n.transmit(&[1; 60]).unwrap();
    let d = n.ring.tx(0);
    assert_eq!(dev::read32(d.add(DES_STATUS)), DESC_OWN);
    assert_eq!(dev::read32(d.add(DES_CNTL)), tx_cntl(60, false).unwrap());
    assert_eq!(dev::read32(d.add(DES_BUF)), lo(n.ring.tx_buf(0)));
    let mut back = [0u8; 60];
    dev::copy_out(&mut back, n.ring.tx_buf(0));
    assert_eq!(back, [1; 60]);
    // Geen doorbell vóór de flush, één erna, en geen tweede zonder werk.
    assert_eq!(n.regs().tx_poll.read(), 0);
    n.flush();
    assert_eq!(n.regs().tx_poll.read(), 1);
    n.flush();
    assert_eq!(n.stats.doorbells, 1);
}

#[test]
fn transmit_wraps_with_ring_end_and_stops_when_full() {
    let mut f = running();
    let n = &mut f.n;
    for _ in 0..NUM_DESC {
        n.transmit(&[2; 60]).unwrap();
    }
    // De laatste descriptor draagt de ring-end-bit, de rest niet.
    let last = n.ring.tx(NUM_DESC - 1);
    assert_eq!(dev::read32(last.add(DES_CNTL)), tx_cntl(60, true).unwrap());
    assert_eq!(dev::read32(n.ring.tx(1).add(DES_CNTL)) & RING_END, 0);
    assert_eq!(n.tx_cur, 0);
    // De DMA gaf niets terug, dus plek 0 is nog van hem.
    assert_eq!(n.transmit(&[3; 60]), Err(TxError::Full));
    assert_eq!(n.stats.tx_full, 1);
    // De DMA verzond descriptor 0 (OWN weg, status erin): er is weer plaats.
    dev::write32(n.ring.tx(0), 0);
    n.transmit(&[4; 60]).unwrap();
    assert_eq!(n.tx_cur, 1);
    assert_eq!(dev::read32(n.ring.tx(0).add(DES_BUF)), lo(n.ring.tx_buf(0)));
}

#[test]
fn diag_is_one_line_with_the_numbers_and_samples_missed() {
    let mut f = running();
    f.n.regs().missed.write(5 | (2 << 17));
    let s = f.n.diag().to_string();
    assert!(!s.contains('\n'));
    assert!(s.contains("rxdesc[0] 0x80000000"), "{s}");
    assert!(s.contains("missed-ring=5 missed-fifo=2"), "{s}");
    assert_eq!(f.n.stats.rx_missed_ring, 5);
}
