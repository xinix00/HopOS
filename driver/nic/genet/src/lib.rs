//! De Broadcom GENET v5 gigabit-MAC: de geïntegreerde NIC van de Raspberry
//! Pi 4 (BCM2711, ARM-zicht 0xFD58_0000).
//!
//! Géén PCIe zoals de Pi 5/RP1: direct memory-mapped, en de DMA-descriptors
//! leven in registerruimte (on-chip), niet in RAM; alleen de framebuffers
//! liggen in DRAM (busadres = fysiek adres, 1:1; geen dma-ranges op de
//! scb-bus).
//!
//! Recept uit Linux' `bcmgenet.c`/`bcmmii.c` (rpi-6.12.y) en vooral U-Boots
//! minimale gepolde driver: die bewijst op de Pi 4 precies onze vorm: ring
//! 16 (de hardware-default), register-mode (géén 64-byte status-blocks),
//! geen HFB. Boardvast bewezen in de Go-kern op 11-07, één boot: v5-rev,
//! MAC-reset, PHY op adres 1, autoneg, ring-16-DMA, DHCP-lease.
//!
//! De valkuilen zitten als commentaar bij de code; de twee grootste: elke
//! TX-descriptor MOET QTAG 0x3F<<7 dragen (anders eet de arbiter het
//! frame), en de prod/cons-indexen zijn vrijlopende 16-bit-tellers (mod
//! 0x10000) náást de descriptor-pointer (mod 256).
//!
//! Eigendom: de driver is van de RX-pomp (één eigenaar, `&mut self`); de
//! DMA-regio die het board geeft is van hem alleen. De regio is Normal
//! non-cacheable gemapt, dus er is geen cache-onderhoud; de barrières staan
//! waar het protocol ze eist.

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

use core::fmt;
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};
use driver_mdio::{Link, Mdio};
use netdev::{Mac, TxError};

#[cfg(test)]
mod tests;

/// SYS-blok (+0x000).
#[repr(C)]
struct Sys {
    /// Versie-nibble [27:24]: v5 meldt 6.
    rev_ctrl: Reg<u32>,
    /// 3 = PORT_MODE_EXT_GPHY.
    port_ctrl: Reg<u32>,
    /// Bit 0 = RX-flush, bit 1 = umac-sw-reset.
    rbuf_flush_ctrl: Reg<u32>,
}

/// EXT-blok (+0x080).
#[repr(C)]
struct Ext {
    _r0: [u32; 3],
    /// RGMII_LINK bit 4, OOB_DISABLE 5, RGMII_MODE_EN 6, ID_MODE_DIS 16.
    rgmii_oob_ctrl: Reg<u32>,
}

/// Een INTRL2-blok (+0x200 en +0x240).
#[repr(C)]
struct Intrl2 {
    stat: Reg<u32>,
    set: Reg<u32>,
    clear: Reg<u32>,
    mask_status: Reg<u32>,
    mask_set: Reg<u32>,
    mask_clear: Reg<u32>,
}

/// RBUF-blok (+0x300).
#[repr(C)]
struct Rbuf {
    /// Bit 1 = ALIGN_2B (2 pad-bytes vóór elk RX-frame).
    ctrl: Reg<u32>,
    _r0: [u32; 44],
    tbuf_size_ctrl: Reg<u32>,
}

/// UMAC-blok (+0x800).
#[repr(C)]
struct Umac {
    _r0: [u32; 2],
    /// TX_EN 0, RX_EN 1, speed [3:2], PROMISC 4, HD_EN 10, SW_RESET 13,
    /// LCL_LOOP_EN 15.
    cmd: Reg<u32>,
    mac0: Reg<u32>,
    mac1: Reg<u32>,
    max_frame: Reg<u32>,
    _r1: [u32; 199],
    tx_flush: Reg<u32>,
    _r2: [u32; 146],
    mib_ctrl: Reg<u32>,
    _r3: [u32; 36],
    /// START_BUSY 29, READ_FAIL 28, RD 2<<26, WR 1<<26, phy<<21, reg<<16.
    mdio_cmd: Reg<u32>,
    _r4: [u32; 14],
    /// Filter-enable: filter n = bit 16-n.
    mdf_ctrl: Reg<u32>,
    /// 17 filters x 2 woorden.
    mdf_addr: [Reg<u32>; 34],
}

/// Eén DMA-descriptor in registerruimte: 3 woorden.
#[repr(C)]
struct Desc {
    /// Lengte [27:16], status [15:0].
    len_status: Reg<u32>,
    addr_lo: Reg<u32>,
    addr_hi: Reg<u32>,
}

/// De RDMA-ring 16 plus de gedeelde RDMA-registers (+0x3000).
#[repr(C)]
struct RxDma {
    write_ptr: Reg<u32>,
    _wp_hi: u32,
    prod: Reg<u32>,
    cons: Reg<u32>,
    buf_size: Reg<u32>,
    start: Reg<u32>,
    _start_hi: u32,
    end: Reg<u32>,
    _end_hi: u32,
    _thresh: u32,
    xon_xoff: Reg<u32>,
    read_ptr: Reg<u32>,
    _rp_hi: u32,
    _r0: [u32; 3],
    ring_cfg: Reg<u32>,
    ctrl: Reg<u32>,
    status: Reg<u32>,
    burst: Reg<u32>,
}

/// De TDMA-ring 16 plus de gedeelde TDMA-registers (+0x5000).
#[repr(C)]
struct TxDma {
    read_ptr: Reg<u32>,
    _rp_hi: u32,
    cons: Reg<u32>,
    prod: Reg<u32>,
    buf_size: Reg<u32>,
    start: Reg<u32>,
    _start_hi: u32,
    end: Reg<u32>,
    _end_hi: u32,
    _r0: u32,
    flow: Reg<u32>,
    write_ptr: Reg<u32>,
    _wp_hi: u32,
    _r1: [u32; 3],
    ring_cfg: Reg<u32>,
    ctrl: Reg<u32>,
    status: Reg<u32>,
    burst: Reg<u32>,
}

/// Blokken vanaf de GENET-basis (`bcmgenet.h`, GENET_V5-hw_params).
const SYS: u64 = 0x000;
const EXT: u64 = 0x080;
const INTRL2_0: u64 = 0x200;
const INTRL2_1: u64 = 0x240;
const RBUF: u64 = 0x300;
const UMAC: u64 = 0x800;
const RX_BD: u64 = 0x2000;
const RX_DMA: u64 = 0x3000;
const TX_BD: u64 = 0x4000;
const TX_DMA: u64 = 0x5000;
/// De maat van het hele blok.
pub const MMIO_SIZE: u64 = 0x5050;

const _: () = {
    assert!(offset_of!(Sys, rbuf_flush_ctrl) == 0x08);
    assert!(EXT + offset_of!(Ext, rgmii_oob_ctrl) as u64 == 0x08c);
    assert!(INTRL2_0 + offset_of!(Intrl2, clear) as u64 == 0x208);
    assert!(INTRL2_0 + offset_of!(Intrl2, mask_set) as u64 == 0x210);
    assert!(INTRL2_1 + offset_of!(Intrl2, clear) as u64 == 0x248);
    assert!(RBUF + offset_of!(Rbuf, tbuf_size_ctrl) as u64 == 0x3b4);
    assert!(UMAC + offset_of!(Umac, cmd) as u64 == 0x808);
    assert!(UMAC + offset_of!(Umac, mac0) as u64 == 0x80c);
    assert!(UMAC + offset_of!(Umac, max_frame) as u64 == 0x814);
    assert!(UMAC + offset_of!(Umac, tx_flush) as u64 == 0xb34);
    assert!(UMAC + offset_of!(Umac, mib_ctrl) as u64 == 0xd80);
    assert!(UMAC + offset_of!(Umac, mdio_cmd) as u64 == 0xe14);
    assert!(UMAC + offset_of!(Umac, mdf_ctrl) as u64 == 0xe50);
    assert!(UMAC + offset_of!(Umac, mdf_addr) as u64 == 0xe54);
    assert!(size_of::<Desc>() == 12);
    assert!(RX_BD + (N_BD * 12) as u64 <= RX_DMA);
    assert!(offset_of!(RxDma, prod) == 0x08);
    assert!(offset_of!(RxDma, buf_size) == 0x10);
    assert!(offset_of!(RxDma, end) == 0x1c);
    assert!(offset_of!(RxDma, xon_xoff) == 0x28);
    assert!(offset_of!(RxDma, read_ptr) == 0x2c);
    assert!(offset_of!(RxDma, ring_cfg) == 0x40);
    assert!(offset_of!(RxDma, burst) == 0x4c);
    assert!(offset_of!(TxDma, cons) == 0x08);
    assert!(offset_of!(TxDma, prod) == 0x0c);
    assert!(offset_of!(TxDma, flow) == 0x28);
    assert!(offset_of!(TxDma, write_ptr) == 0x2c);
    assert!(offset_of!(TxDma, ring_cfg) == 0x40);
    assert!(offset_of!(TxDma, burst) == 0x4c);
    assert!(TX_DMA + size_of::<TxDma>() as u64 == MMIO_SIZE);
};

/// LENGTH_STATUS: einde van het pakket.
const DMA_EOP: u32 = 0x4000;
/// LENGTH_STATUS: begin van het pakket.
const DMA_SOP: u32 = 0x2000;
/// TX: QTAG [12:7] moet 0x3F zijn (valkuil 1).
const TX_QTAG: u32 = 0x3f << 7;
/// TX: laat de MAC de CRC aanhangen.
const TX_APPEND_CRC: u32 = 0x0040;
/// RX-foutbits: OV, CRC, RXER, NO, LG.
const RX_ERR_MASK: u32 = 0x001f;
/// Descriptors per richting (alle aan ring 16, als U-Boot).
pub const N_BD: usize = 256;
/// RX_BUF_LENGTH; ook de TX-korrel.
pub const BUF_SIZE: usize = 2048;
/// Wat deze driver aan DMA-geheugen vraagt: de RX- en TX-buffers.
pub const DMA_NEED: u64 = (2 * N_BD * BUF_SIZE) as u64;
/// DMA-enable plus alle ring-enables (globaal en per ring).
const DMA_ENABLE_MASK: u32 = 1 | (0xffff << 1) | (1 << 17);
/// DMA_STATUS: de motor staat stil.
const DMA_DISABLED: u32 = 1;
/// Ring 16 in de enable-bits (valkuil 10).
const RING16_EN: u32 = 1 << 17;
/// Hoe lang een DMA-stop mag duren.
const DMA_STOP_NS: u64 = 5_000_000;
/// De MDIO-pollgrens: ~25 µs typisch, ruim begrensd, nooit eeuwig.
const MDIO_POLLS: u32 = 100_000;

/// Waarom de GENET iets weigert.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// De versie-nibble is geen 6 (geen v5).
    Rev {
        /// Het rauwe SYS_REV_CTRL.
        raw: u32,
    },
    /// De DMA-regio is te klein.
    Dma {
        /// Wat er is.
        have: u64,
        /// Wat er nodig is.
        need: u64,
    },
    /// Een DMA-motor bevestigde zijn stop niet.
    Stop {
        /// "TX" of "RX".
        dir: &'static str,
        /// De laatste DMA_STATUS.
        status: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Rev { raw } => write!(f, "genet: rev {raw:#x}, expected v5 (nibble 6)"),
            Self::Dma { have, need } => write!(f, "genet: DMA region {have:#x} < {need:#x}"),
            Self::Stop { dir, status } => {
                write!(f, "genet: {dir} DMA stop timeout (status {status:#x})")
            }
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén GENET.
pub struct Genet {
    base: Pa,
    mac: [u8; 6],
    rx_bufs: Pa,
    tx_bufs: Pa,
    dma_size: u64,
    /// Vrijlopende indexen mod 0x10000 (valkuil 3); de descriptor is
    /// index % N_BD, en N_BD deelt 0x10000.
    rx_cons: u32,
    tx_prod: u32,
    clock: fn() -> u64,
}

impl Genet {
    /// Een GENET op `base` met zijn buffers in `[dma, dma + dma_size)`.
    /// Raakt nog niets aan.
    ///
    /// # Safety
    ///
    /// `base` is het gemapte GENET-blok ([`MMIO_SIZE`]); de DMA-regio is
    /// van deze driver alleen, busadres = fysiek adres, en coherent gemapt
    /// (Normal non-cacheable of Device). Beide blijven zolang de driver
    /// leeft.
    #[must_use]
    pub const unsafe fn new(
        base: Pa,
        dma: Pa,
        dma_size: u64,
        mac: [u8; 6],
        clock: fn() -> u64,
    ) -> Self {
        Self {
            base,
            mac,
            rx_bufs: dma,
            tx_bufs: dma.add((N_BD * BUF_SIZE) as u64),
            dma_size,
            rx_cons: 0,
            tx_prod: 0,
            clock,
        }
    }

    fn at<R>(&self, off: u64) -> &'static R {
        // SAFETY: de voorwaarde van `new`: het blok is gemapt; elke `R` is
        // een van de registerblokken hierboven op zijn eigen offset (de
        // const-asserties).
        unsafe { dev::regs(self.base.add(off)) }
    }
    fn sys(&self) -> &'static Sys {
        self.at(SYS)
    }
    fn umac(&self) -> &'static Umac {
        self.at(UMAC)
    }
    fn rx(&self) -> &'static RxDma {
        self.at(RX_DMA)
    }
    fn tx(&self) -> &'static TxDma {
        self.at(TX_DMA)
    }
    fn rx_bd(&self, i: usize) -> &'static Desc {
        self.at(RX_BD + (12 * (i % N_BD)) as u64)
    }
    fn tx_bd(&self, i: usize) -> &'static Desc {
        self.at(TX_BD + (12 * (i % N_BD)) as u64)
    }

    /// Het rauwe SYS_REV_CTRL; nibble [27:24] hoort 6 te zijn.
    #[must_use]
    pub fn rev(&self) -> u32 {
        self.sys().rev_ctrl.read()
    }

    /// Toetst de versie: v5 of een fout.
    pub fn check_rev(&self) -> Result {
        let raw = self.rev();
        if (raw >> 24) & 0xf != 6 {
            return Err(Error::Rev { raw });
        }
        Ok(())
    }

    /// Brengt de MAC in een bekende staat: eerst een actieve DMA stoppen
    /// (een flip erft een lopende NIC; `bcmgenet_dma_teardown`: TX eerst,
    /// laten leeglopen, dan RX, en een gewiste enable-bit alleen is geen
    /// bevestiging), dan de U-Boot-sequence: sw-reset mét tijdelijke
    /// local-loopback voor een stabiele rxclk, en SW_RESET daarna écht
    /// wissen (valkuil 2); MIB-reset, framelengte, RX-alignment, interrupts
    /// dicht, de poort-mux naar de externe GPHY. Hierna werkt MDIO.
    pub fn reset(&mut self) -> Result {
        let u = self.umac();
        u.cmd.update(|c| c & !(1 << 1));
        self.stop_dma(true)?;
        dev::delay(self.clock, 10_000_000);
        self.stop_dma(false)?;
        self.tx().ctrl.update(|c| c & !DMA_ENABLE_MASK);
        self.rx().ctrl.update(|c| c & !DMA_ENABLE_MASK);
        u.cmd.update(|c| c & !1);
        let s = self.sys();
        let r = s.rbuf_flush_ctrl.read();
        s.rbuf_flush_ctrl.write(r | 2);
        dev::delay(self.clock, 10_000);
        s.rbuf_flush_ctrl.write(r & !2);
        dev::delay(self.clock, 10_000);
        s.rbuf_flush_ctrl.write(0);
        dev::delay(self.clock, 10_000);

        u.cmd.write(0);
        u.cmd.write((1 << 13) | (1 << 15));
        dev::delay(self.clock, 2_000);
        u.cmd.write(0);

        u.mib_ctrl.write(7);
        u.mib_ctrl.write(0);
        u.max_frame.write(1536);
        // ALIGN_2B; géén 64-byte-statusblocks (valkuil 7).
        let rb: &Rbuf = self.at(RBUF);
        rb.ctrl.update(|c| c | (1 << 1));
        rb.tbuf_size_ctrl.write(1);

        for off in [INTRL2_0, INTRL2_1] {
            let i: &Intrl2 = self.at(off);
            i.mask_set.write(u32::MAX);
            i.clear.write(u32::MAX);
        }
        s.port_ctrl.write(3);
        Ok(())
    }

    /// Wacht op de bevestiging van de hardware: DMA_STATUS.DISABLED.
    fn stop_dma(&self, tx: bool) -> Result {
        let (ctrl, status, dir) = if tx {
            (&self.tx().ctrl, &self.tx().status, "TX")
        } else {
            (&self.rx().ctrl, &self.rx().status, "RX")
        };
        ctrl.update(|c| c & !1);
        let mut v = 0;
        if dev::poll_until(self.clock, DMA_STOP_NS, || {
            v = status.read();
            v & DMA_DISABLED != 0
        }) {
            return Ok(());
        }
        Err(Error::Stop { dir, status: v })
    }

    /// Zet de START_BUSY-bit en wacht begrensd tot de transactie klaar is.
    fn mdio_kick(&self) -> bool {
        let m = &self.umac().mdio_cmd;
        m.update(|v| v | (1 << 29));
        (0..MDIO_POLLS).any(|_| m.read() & (1 << 29) == 0)
    }

    /// Zet MAC-adres en filters, ringen en DMA klaar en schakelt zender en
    /// ontvanger in, op de snelheid die de autonegotiatie afsprak. Na
    /// [`reset`](Self::reset).
    pub fn init(&mut self, link: Link) -> Result {
        if self.dma_size < DMA_NEED {
            return Err(Error::Dma {
                have: self.dma_size,
                need: DMA_NEED,
            });
        }
        let u = self.umac();
        let m = self.mac;
        // MAC-adres + MDF-filter: broadcast (filter 0) en het eigen adres
        // (filter 1); bit 16-n schakelt filter n in; geen promiscuous.
        u.mac0.write(u32::from_be_bytes([m[0], m[1], m[2], m[3]]));
        u.mac1.write(u32::from_be_bytes([0, 0, m[4], m[5]]));
        let words = [
            0xffff,
            u32::MAX,
            u32::from_be_bytes([0, 0, m[0], m[1]]),
            u32::from_be_bytes([m[2], m[3], m[4], m[5]]),
        ];
        for (r, w) in u.mdf_addr.iter().zip(words) {
            r.write(w);
        }
        u.mdf_ctrl.write((1 << 16) | (1 << 15));

        // Reset bevestigde de DMA-stop; eerst flushen, dan de ringen.
        u.tx_flush.write(1);
        dev::delay(self.clock, 10_000);
        u.tx_flush.write(0);
        let s = self.sys();
        let r = s.rbuf_flush_ctrl.read();
        s.rbuf_flush_ctrl.write(r | 1);
        dev::delay(self.clock, 10_000);
        s.rbuf_flush_ctrl.write(r);
        dev::delay(self.clock, 10_000);

        self.init_rx();
        self.init_tx();

        // DMA aan: eerst RDMA, dan TDMA (ring 16 = enable-bit 17, valkuil
        // 10).
        self.rx().ctrl.update(|c| c | RING16_EN | 1);
        self.tx().ctrl.update(|c| c | RING16_EN | 1);

        // RGMII, U-Boot-variant: ID_MODE_DIS (de PHY-strap levert de
        // klokskew, wij programmeren geen skew-registers; valkuil 8) + LINK +
        // MODE_EN.
        let e: &Ext = self.at(EXT);
        e.rgmii_oob_ctrl.write((1 << 4) | (1 << 6) | (1 << 16));

        u.cmd.write(cmd_for(u.cmd.read(), link) | 1 | (1 << 1));
        dev::mb();
        Ok(())
    }

    /// RX-ring 16: elke descriptor wijst vast naar zijn eigen buffer; de
    /// hardware schrijft LENGTH_STATUS per pakket. PROD is van de hardware
    /// en houdt zijn telling; de pointers volgen die telling, anders kiezen
    /// DMA en software andere buffers. Pointers tellen woorden (3 per
    /// descriptor).
    fn init_rx(&mut self) {
        for i in 0..N_BD {
            let bus = self.rx_bufs.add((i * BUF_SIZE) as u64).0;
            let d = self.rx_bd(i);
            d.addr_lo.write(bus as u32);
            d.addr_hi.write((bus >> 32) as u32);
        }
        let rx = self.rx();
        rx.burst.write(8); // BCM2711: dma_max_burst_length = 8
        rx.start.write(0);
        rx.end.write((N_BD * 3 - 1) as u32);
        self.rx_cons = rx.prod.read() & 0xffff;
        let ptr = (self.rx_cons % N_BD as u32) * 3;
        rx.read_ptr.write(ptr);
        rx.write_ptr.write(ptr);
        rx.cons.write(self.rx_cons);
        rx.buf_size.write(((N_BD as u32) << 16) | BUF_SIZE as u32);
        rx.xon_xoff.write((5 << 16) | (N_BD as u32 >> 4));
        rx.ring_cfg.write(1 << 16);
    }

    /// TX spiegelt RX: CONS is van de hardware; PROD volgt hem vóór de DMA
    /// aangaat.
    fn init_tx(&mut self) {
        let tx = self.tx();
        tx.burst.write(8);
        tx.start.write(0);
        tx.end.write((N_BD * 3 - 1) as u32);
        self.tx_prod = tx.cons.read() & 0xffff;
        let ptr = (self.tx_prod % N_BD as u32) * 3;
        tx.read_ptr.write(ptr);
        tx.write_ptr.write(ptr);
        tx.prod.write(self.tx_prod);
        tx.flow.write(0);
        tx.buf_size.write(((N_BD as u32) << 16) | BUF_SIZE as u32);
        tx.ring_cfg.write(1 << 16);
    }
}

/// UMAC_CMD voor een link: snelheid [3:2] (2 = 1000, 1 = 100), HD_EN bij
/// half duplex; de rest van `cmd` blijft staan.
fn cmd_for(cmd: u32, link: Link) -> u32 {
    let mut c = cmd & !((3 << 2) | (1 << 10));
    match link.mbps {
        1000 => c |= 2 << 2,
        100 => c |= 1 << 2,
        _ => {}
    }
    if !link.full_duplex {
        c |= 1 << 10;
    }
    c
}

impl Mdio for Genet {
    /// Clause-22 via de interne unimac-MDIO.
    fn read(&mut self, phy: u8, reg: u8) -> driver_mdio::Result<u16> {
        let m = &self.umac().mdio_cmd;
        m.write((2 << 26) | (u32::from(phy & 0x1f) << 21) | (u32::from(reg & 0x1f) << 16));
        if !self.mdio_kick() {
            return Err(driver_mdio::Error::Bus { phy, reg });
        }
        let v = m.read();
        if v & (1 << 28) != 0 {
            // READ_FAIL: niemand op dit adres.
            return Err(driver_mdio::Error::Bus { phy, reg });
        }
        Ok(v as u16)
    }

    fn write(&mut self, phy: u8, reg: u8, val: u16) -> driver_mdio::Result {
        self.umac().mdio_cmd.write(
            (1 << 26)
                | (u32::from(phy & 0x1f) << 21)
                | (u32::from(reg & 0x1f) << 16)
                | u32::from(val),
        );
        if !self.mdio_kick() {
            return Err(driver_mdio::Error::Bus { phy, reg });
        }
        Ok(())
    }
}

impl netdev::Device for Genet {
    /// Eén frame op de ring; de kick is de schrijf van de opgehoogde PROD.
    /// Een volle ring is een antwoord, geen wachtlus (256 in de lucht).
    fn transmit(&mut self, frame: &[u8]) -> core::result::Result<(), TxError> {
        if frame.is_empty() || frame.len() > BUF_SIZE {
            return Err(TxError::Size(frame.len()));
        }
        let tx = self.tx();
        let in_flight = self.tx_prod.wrapping_sub(tx.cons.read()) & 0xffff;
        if in_flight >= N_BD as u32 {
            return Err(TxError::Full);
        }
        let i = (self.tx_prod as usize) % N_BD;
        let dst = self.tx_bufs.add((i * BUF_SIZE) as u64);
        dev::copy_in(dst, frame);
        let d = self.tx_bd(i);
        d.addr_lo.write(dst.0 as u32);
        d.addr_hi.write((dst.0 >> 32) as u32);
        dev::mb();
        d.len_status
            .write(((frame.len() as u32) << 16) | TX_QTAG | TX_APPEND_CRC | DMA_SOP | DMA_EOP);
        self.tx_prod = self.tx_prod.wrapping_add(1) & 0xffff;
        tx.prod.write(self.tx_prod);
        Ok(())
    }

    /// Eén frame, of `None`. Alleen complete, foutvrije frames; ALIGN_2B
    /// zet 2 pad-bytes vóór het frame, in de lengte meegeteld (valkuil 6).
    /// Een kapot frame gaat terug naar de DMA en de volgende ronde kijkt
    /// verder.
    fn receive(&mut self, buf: &mut [u8]) -> Option<usize> {
        let rx = self.rx();
        loop {
            if rx.prod.read() & 0xffff == self.rx_cons {
                return None;
            }
            // De PROD-toets vóór het lezen van LENGTH_STATUS en de buffer.
            dev::mb();
            let i = (self.rx_cons as usize) % N_BD;
            let ls = self.rx_bd(i).len_status.read();
            let got = rx_len(ls).map(|n| n.min(buf.len()));
            if let Some(n) = got
                && let Some(dst) = buf.get_mut(..n)
            {
                dev::copy_out(dst, self.rx_bufs.add((i * BUF_SIZE + 2) as u64));
            }
            // Klaar met lezen vóór de buffer terug naar de DMA gaat.
            dev::mb();
            self.rx_cons = self.rx_cons.wrapping_add(1) & 0xffff;
            rx.cons.write(self.rx_cons);
            if let Some(n) = got {
                return Some(n);
            }
        }
    }

    fn mac(&self) -> Mac {
        Mac(self.mac)
    }
}

/// De framelengte uit LENGTH_STATUS, zonder de 2 pad-bytes; `None` voor een
/// frame dat niet compleet, niet foutvrij of niet in één buffer is.
fn rx_len(ls: u32) -> Option<usize> {
    let len = ((ls >> 16) & 0xfff) as usize;
    let flags = ls & 0xffff;
    let whole = flags & (DMA_SOP | DMA_EOP) == DMA_SOP | DMA_EOP;
    (whole && flags & RX_ERR_MASK == 0 && len > 2 && len <= BUF_SIZE).then_some(len - 2)
}
