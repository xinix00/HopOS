//! virtio-net over MMIO (QEMU virt): de virtqueues, DMA-buffers, batching.
//!
//! HopOS' eigen virtio-net-driver (virtio-mmio, modern / VERSION_1), in de
//! vorm die elke NIC-driver krijgt: getypeerde MMIO-registers, split
//! virtqueues (descriptortabel plus avail- en used-ring) en DMA-buffers in
//! een regio die het board uitdeelt, buiten de kern-RAM en niet gecached
//! (`layout.NetDMABase`). Op QEMU is die regio volledig coherent.
//!
//! De driver is een actor-onderdeel: wie hem heeft (`&mut self`) is de
//! enige die de ringen aanraakt. Het interrupt-pad raakt alleen
//! InterruptStatus/InterruptACK, via [`IrqAck`], en die registers delen
//! niets met de ringen.
//!
//! Batching: [`transmit`](netdev::Device::transmit) en de RX-recycle zetten
//! descriptors klaar; de doorbell (avail.idx publiceren plus QueueNotify)
//! valt pas in [`flush`](netdev::Device::flush), één keer per burst. Dat is
//! de meting uit de Go-kern: één doorbell per burst, niet per frame.

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
use core::mem::offset_of;
use dev::{Pa, Reg};
use netdev::{Mac, TxError};
use sync::Signal;

/// De virtio-mmio-registers, versie 2 (virtio 1.2, §4.2.2).
#[repr(C)]
struct Regs {
    magic: Reg<u32>,
    version: Reg<u32>,
    device_id: Reg<u32>,
    vendor_id: Reg<u32>,
    // Device-features (0x010/0x014) leest deze driver bewust niet: QEMU
    // virt levert een vaste, bekende set en wij onderhandelen alleen
    // VERSION_1.
    device_features: Reg<u32>,
    device_features_sel: Reg<u32>,
    _r0: [u32; 2],
    driver_features: Reg<u32>,
    driver_features_sel: Reg<u32>,
    _r1: [u32; 2],
    queue_sel: Reg<u32>,
    queue_num_max: Reg<u32>,
    queue_num: Reg<u32>,
    _r2: [u32; 2],
    queue_ready: Reg<u32>,
    _r3: [u32; 2],
    queue_notify: Reg<u32>,
    _r4: [u32; 3],
    /// Bit 0 = used-ring bijgewerkt, bit 1 = config veranderd.
    interrupt_status: Reg<u32>,
    /// Dezelfde bits terugschrijven laat de (level-)lijn los.
    interrupt_ack: Reg<u32>,
    _r5: [u32; 2],
    status: Reg<u32>,
    _r6: [u32; 3],
    queue_desc_lo: Reg<u32>,
    queue_desc_hi: Reg<u32>,
    _r7: [u32; 2],
    queue_driver_lo: Reg<u32>,
    queue_driver_hi: Reg<u32>,
    _r8: [u32; 2],
    queue_device_lo: Reg<u32>,
    queue_device_hi: Reg<u32>,
    _r9: [u32; 21],
    config_generation: Reg<u32>,
    /// De config van virtio-net; de eerste zes bytes zijn de MAC. Per byte:
    /// een 32-bit lees op een oneven offset is ongealigneerd en abort op
    /// device-geheugen.
    mac: [Reg<u8>; 6],
}

const _: () = {
    assert!(offset_of!(Regs, magic) == 0x000);
    assert!(offset_of!(Regs, version) == 0x004);
    assert!(offset_of!(Regs, device_id) == 0x008);
    assert!(offset_of!(Regs, vendor_id) == 0x00c);
    assert!(offset_of!(Regs, device_features) == 0x010);
    assert!(offset_of!(Regs, device_features_sel) == 0x014);
    assert!(offset_of!(Regs, driver_features) == 0x020);
    assert!(offset_of!(Regs, driver_features_sel) == 0x024);
    assert!(offset_of!(Regs, queue_sel) == 0x030);
    assert!(offset_of!(Regs, queue_num_max) == 0x034);
    assert!(offset_of!(Regs, queue_num) == 0x038);
    assert!(offset_of!(Regs, queue_ready) == 0x044);
    assert!(offset_of!(Regs, queue_notify) == 0x050);
    assert!(offset_of!(Regs, interrupt_status) == 0x060);
    assert!(offset_of!(Regs, interrupt_ack) == 0x064);
    assert!(offset_of!(Regs, status) == 0x070);
    assert!(offset_of!(Regs, queue_desc_lo) == 0x080);
    assert!(offset_of!(Regs, queue_desc_hi) == 0x084);
    assert!(offset_of!(Regs, queue_driver_lo) == 0x090);
    assert!(offset_of!(Regs, queue_driver_hi) == 0x094);
    assert!(offset_of!(Regs, queue_device_lo) == 0x0a0);
    assert!(offset_of!(Regs, queue_device_hi) == 0x0a4);
    assert!(offset_of!(Regs, config_generation) == 0x0fc);
    assert!(offset_of!(Regs, mac) == 0x100);
};

/// "virt", little-endian.
const MAGIC: u32 = 0x7472_6976;
/// Het moderne transport.
const VERSION_2: u32 = 2;
/// DeviceID van een netwerkkaart.
const DEVICE_NET: u32 = 1;

const STATUS_ACK: u32 = 1 << 0;
const STATUS_DRIVER: u32 = 1 << 1;
const STATUS_DRIVER_OK: u32 = 1 << 2;
const STATUS_FEATURES_OK: u32 = 1 << 3;

/// VIRTIO_F_VERSION_1 (bit 32): bit 0 van het hoge feature-venster.
const FEAT_VERSION_1_HI: u32 = 1 << 0;

/// Descriptor-flag: het device schrijft in deze buffer.
const DESC_WRITE: u16 = 2;

const RX_QUEUE: u32 = 0;
const TX_QUEUE: u32 = 1;

/// `virtio_net_hdr_mrg_rxbuf` onder VERSION_1: 12 bytes vóór elk frame.
pub const HDR_LEN: usize = 12;
/// Eén RX- of TX-buffer.
pub const BUF_SIZE: usize = 2048;
/// De grootste queue die we gebruiken (QEMU biedt 256).
pub const MAX_QUEUE: u16 = 256;
const DESC_BYTES: u64 = 16;

/// Waarom de driver weigert.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// Geen virtio-mmio op dit adres.
    NotVirtio,
    /// Een legacy transport (QEMU zonder `force-legacy=false`).
    Legacy,
    /// Wel virtio, maar geen netwerkkaart.
    NotNet(u32),
    /// Het device weigerde VERSION_1.
    FeaturesRefused,
    /// Een queue is er niet (QueueNumMax = 0).
    NoQueue(u32),
    /// De DMA-regio is te klein.
    DmaTooSmall {
        /// Wat nodig was.
        need: u64,
        /// Wat er was.
        have: u64,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NotVirtio => f.write_str("virtionet: no virtio-mmio"),
            Self::Legacy => f.write_str("virtionet: legacy transport (need version 2)"),
            Self::NotNet(id) => write!(f, "virtionet: device id {id} is not a network card"),
            Self::FeaturesRefused => f.write_str("virtionet: device refused VERSION_1"),
            Self::NoQueue(q) => write!(f, "virtionet: queue {q} not available"),
            Self::DmaTooSmall { need, have } => {
                write!(f, "virtionet: DMA region too small ({need} > {have} bytes)")
            }
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Is er op `base` een moderne virtio-netwerkkaart?
///
/// # Safety
///
/// `base` is een gemapt virtio-mmio-slot (minstens 0x200 bytes).
#[must_use]
pub unsafe fn is_modern_net(base: Pa) -> bool {
    // SAFETY: de voorwaarde van deze functie.
    let r: &Regs = unsafe { dev::regs(base) };
    r.magic.read() == MAGIC && r.version.read() == VERSION_2 && r.device_id.read() == DEVICE_NET
}

/// Een bump-allocator over de DMA-regio van de NIC.
struct Dma {
    next: u64,
    end: u64,
}

impl Dma {
    fn alloc(&mut self, size: u64, align: u64) -> Result<Pa> {
        let p = self.next.next_multiple_of(align);
        let end = p.saturating_add(size);
        if end > self.end {
            return Err(Error::DmaTooSmall {
                need: end,
                have: self.end,
            });
        }
        self.next = end;
        dev::clear(Pa(p), size as usize);
        Ok(Pa(p))
    }
}

/// Eén split-virtqueue.
#[derive(Default)]
struct Vq {
    desc: Pa,
    avail: Pa,
    used: Pa,
    bufs: Pa,
    /// De laatst geconsumeerde used.idx.
    last_used: u16,
    /// Onze avail.idx, lokaal; gepubliceerd in `flush`.
    avail_idx: u16,
    /// De laatst gepubliceerde avail.idx.
    published: u16,
}

impl Vq {
    fn buf(&self, i: u16) -> Pa {
        self.bufs.add(u64::from(i) * BUF_SIZE as u64)
    }

    fn set_desc(&self, i: u16, addr: Pa, len: u32, flags: u16) {
        let d = self.desc.add(u64::from(i) * DESC_BYTES);
        dev::write64(d, addr.0);
        dev::write32(d.add(8), len);
        dev::write16(d.add(12), flags);
        dev::write16(d.add(14), 0);
    }

    fn set_avail(&self, qsize: u16, slot: u16, desc: u16) {
        dev::write16(self.avail.add(4 + u64::from(slot % qsize) * 2), desc);
    }

    fn used_idx(&self) -> u16 {
        dev::read16(self.used.add(2))
    }

    /// Publiceert avail.idx als er iets nieuws is; `true` = de doorbell moet.
    fn publish(&mut self) -> bool {
        if self.avail_idx == self.published {
            return false;
        }
        dev::mb();
        dev::write16(self.avail.add(2), self.avail_idx);
        dev::mb();
        self.published = self.avail_idx;
        true
    }
}

/// Het interrupt-pad van de NIC: alleen InterruptStatus en InterruptACK.
/// `Copy`, zodat het board hem naast de driver kan houden.
#[derive(Clone, Copy)]
pub struct IrqAck {
    base: Pa,
}

impl IrqAck {
    /// Bevestigt de interrupt: wat in InterruptStatus staat gaat terug naar
    /// InterruptACK, waarop het device zijn level-lijn loslaat. Zonder deze
    /// schrijf vuurt de lijn na de EOI meteen weer, hoe leeg de ring ook is.
    /// Geeft de bits die stonden.
    pub fn ack(&self) -> u32 {
        // SAFETY: `base` kwam uit `VirtioNet::new`, dat een gemapt blok
        // eiste; InterruptStatus en InterruptACK delen niets met de ringen.
        let r: &Regs = unsafe { dev::regs(self.base) };
        let st = r.interrupt_status.read();
        if st != 0 {
            r.interrupt_ack.write(st);
        }
        st
    }
}

/// Eén virtio-net.
pub struct VirtioNet {
    base: Pa,
    mac: Mac,
    qsize: u16,
    rx: Vq,
    tx: Vq,
    irq: Option<&'static Signal>,
    /// Meetlat: doorbells.
    pub doorbells: u64,
    /// Meetlat: RX-entries met een id of lengte die niet klopt.
    pub rx_bad: u64,
}

impl VirtioNet {
    /// Zet het device op: reset, VERSION_1 onderhandelen, RX- en TX-queue
    /// in `dma`, de RX-buffers publiceren, DRIVER_OK.
    ///
    /// # Safety
    ///
    /// `base` is een gemapt virtio-mmio-blok dat voor altijd blijft, en
    /// `[dma, dma+dma_size)` is gemapt geheugen dat alleen deze driver en
    /// het device gebruiken, nu en zolang het programma draait.
    pub unsafe fn new(base: Pa, dma: Pa, dma_size: u64) -> Result<Self> {
        let mut n = Self {
            base,
            mac: Mac::default(),
            qsize: 0,
            rx: Vq::default(),
            tx: Vq::default(),
            irq: None,
            doorbells: 0,
            rx_bad: 0,
        };
        let r = n.regs();
        if r.magic.read() != MAGIC {
            return Err(Error::NotVirtio);
        }
        if r.version.read() != VERSION_2 {
            return Err(Error::Legacy);
        }
        let id = r.device_id.read();
        if id != DEVICE_NET {
            return Err(Error::NotNet(id));
        }
        let mut dma = Dma {
            next: dma.0,
            end: dma.0.saturating_add(dma_size),
        };

        // De status-handdruk: reset, ACK, DRIVER.
        r.status.write(0);
        r.status.write(STATUS_ACK);
        r.status.write(STATUS_ACK | STATUS_DRIVER);

        // Alleen VERSION_1.
        r.driver_features_sel.write(0);
        r.driver_features.write(0);
        r.driver_features_sel.write(1);
        r.driver_features.write(FEAT_VERSION_1_HI);
        r.status
            .write(STATUS_ACK | STATUS_DRIVER | STATUS_FEATURES_OK);
        if r.status.read() & STATUS_FEATURES_OK == 0 {
            return Err(Error::FeaturesRefused);
        }

        let mut mac = [0u8; 6];
        for (b, reg) in mac.iter_mut().zip(&r.mac) {
            *b = reg.read();
        }
        n.mac = Mac(mac);

        n.rx = n.setup_queue(RX_QUEUE, &mut dma)?;
        n.tx = n.setup_queue(TX_QUEUE, &mut dma)?;

        // Alle RX-buffers aan het device geven.
        for i in 0..n.qsize {
            n.rx.set_desc(i, n.rx.buf(i), BUF_SIZE as u32, DESC_WRITE);
            n.rx.set_avail(n.qsize, i, i);
        }
        n.rx.avail_idx = n.qsize;
        n.rx.publish();

        r.status
            .write(STATUS_ACK | STATUS_DRIVER | STATUS_FEATURES_OK | STATUS_DRIVER_OK);
        n.notify(RX_QUEUE);
        Ok(n)
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    fn setup_queue(&mut self, idx: u32, dma: &mut Dma) -> Result<Vq> {
        let r = self.regs();
        r.queue_sel.write(idx);
        let max = r.queue_num_max.read();
        if max == 0 {
            return Err(Error::NoQueue(idx));
        }
        if self.qsize == 0 {
            self.qsize = u16::try_from(max).unwrap_or(MAX_QUEUE).min(MAX_QUEUE);
        }
        let q = u64::from(self.qsize);
        r.queue_num.write(u32::from(self.qsize));
        let vq = Vq {
            desc: dma.alloc(q * DESC_BYTES, 16)?,
            avail: dma.alloc(6 + 2 * q, 16)?,
            used: dma.alloc(6 + 8 * q, 16)?,
            bufs: dma.alloc(q * BUF_SIZE as u64, 16)?,
            ..Vq::default()
        };
        let split = |pa: Pa| ((pa.0 & 0xffff_ffff) as u32, (pa.0 >> 32) as u32);
        let (lo, hi) = split(vq.desc);
        r.queue_desc_lo.write(lo);
        r.queue_desc_hi.write(hi);
        let (lo, hi) = split(vq.avail);
        r.queue_driver_lo.write(lo);
        r.queue_driver_hi.write(hi);
        let (lo, hi) = split(vq.used);
        r.queue_device_lo.write(lo);
        r.queue_device_hi.write(hi);
        r.queue_ready.write(1);
        Ok(vq)
    }

    fn notify(&mut self, queue: u32) {
        self.regs().queue_notify.write(queue);
        self.doorbells += 1;
    }

    /// Het interrupt-pad, voor het board.
    #[must_use]
    pub fn irq_ack(&self) -> IrqAck {
        IrqAck { base: self.base }
    }

    /// Hangt de bel van de NIC-interrupt aan de driver: de RX-pomp wacht
    /// erop in plaats van te pollen.
    pub fn set_irq(&mut self, bell: &'static Signal) {
        self.irq = Some(bell);
    }

    /// De queuegrootte die het device gaf.
    #[must_use]
    pub fn queue_size(&self) -> u16 {
        self.qsize
    }

    /// Geeft een RX-buffer terug aan het device (gepubliceerd in `flush`).
    fn recycle_rx(&mut self, desc: u16) {
        self.rx
            .set_desc(desc, self.rx.buf(desc), BUF_SIZE as u32, DESC_WRITE);
        self.rx.set_avail(self.qsize, self.rx.avail_idx, desc);
        self.rx.avail_idx = self.rx.avail_idx.wrapping_add(1);
    }
}

impl netdev::Device for VirtioNet {
    /// Zet één frame op de TX-ring (gepipelined: posten en teruggeven, niet
    /// per frame op voltooiing wachten). Slot = avail_idx % qsize, dus de
    /// buffer van dit slot werd het laatst gebruikt op avail_idx - qsize; vol
    /// is de ring pas als het device die oudste nog niet verzond.
    fn transmit(&mut self, frame: &[u8]) -> core::result::Result<(), TxError> {
        if frame.is_empty() || frame.len() > BUF_SIZE - HDR_LEN {
            return Err(TxError::Size(frame.len()));
        }
        self.tx.last_used = self.tx.used_idx();
        if self.tx.avail_idx.wrapping_sub(self.tx.last_used) >= self.qsize {
            return Err(TxError::Full);
        }
        let slot = self.tx.avail_idx % self.qsize;
        let buf = self.tx.buf(slot);
        dev::clear(buf, HDR_LEN);
        dev::copy_in(buf.add(HDR_LEN as u64), frame);
        self.tx
            .set_desc(slot, buf, (HDR_LEN + frame.len()) as u32, 0);
        self.tx.set_avail(self.qsize, self.tx.avail_idx, slot);
        self.tx.avail_idx = self.tx.avail_idx.wrapping_add(1);
        Ok(())
    }

    /// Haalt één frame op, of `None` als de RX-ring leeg is.
    ///
    /// De used-ring is device-eigen: een kromme of kwaadaardige id buiten
    /// `[0, qsize)` zou anders een index worden in de descriptortabel en de
    /// buffers. Zo'n entry wordt veilig gedropt en niet gerecycled (we weten
    /// niet welke buffer erbij hoort; we verliezen hooguit één descriptor).
    /// Een lengte buiten `(HDR_LEN, BUF_SIZE]` wordt gerecycled zonder kopie.
    fn receive(&mut self, buf: &mut [u8]) -> Option<usize> {
        loop {
            let used = self.rx.used_idx();
            if used == self.rx.last_used {
                return None;
            }
            dev::mb();
            let slot = u64::from(self.rx.last_used % self.qsize);
            let elem = self.rx.used.add(4 + slot * 8);
            let id = dev::read32(elem);
            let len = dev::read32(elem.add(4)) as usize;
            self.rx.last_used = self.rx.last_used.wrapping_add(1);
            let Ok(desc) = u16::try_from(id) else {
                self.rx_bad += 1;
                continue;
            };
            if desc >= self.qsize {
                self.rx_bad += 1;
                continue;
            }
            if len <= HDR_LEN || len > BUF_SIZE {
                self.rx_bad += 1;
                self.recycle_rx(desc);
                continue;
            }
            let n = (len - HDR_LEN).min(buf.len());
            if let Some(dst) = buf.get_mut(..n) {
                dev::copy_out(dst, self.rx.buf(desc).add(HDR_LEN as u64));
            }
            self.recycle_rx(desc);
            return Some(n);
        }
    }

    /// Eén doorbell per queue per burst.
    fn flush(&mut self) {
        if self.tx.publish() {
            self.notify(TX_QUEUE);
        }
        if self.rx.publish() {
            self.notify(RX_QUEUE);
        }
    }

    fn mac(&self) -> Mac {
        self.mac
    }

    fn irq(&self) -> Option<&'static Signal> {
        self.irq
    }
}

#[cfg(test)]
mod tests {
    //! `receive_bounds_test.go`, geport: de driver op nep-geheugen.
    use super::*;
    use netdev::Device as _;

    struct Fake {
        _regs: Vec<u64>,
        _mem: Vec<u64>,
        net: VirtioNet,
    }

    fn pa(v: &mut [u64]) -> Pa {
        Pa(v.as_mut_ptr() as usize as u64)
    }

    /// Een driver met qsize 2 zonder `new`: descriptors, ringen en buffers
    /// in één nep-regio, zoals de Go-test.
    fn fake(rx: bool) -> Fake {
        let mut regs = vec![0u64; 64];
        let mut mem = vec![0u64; (256 + 2 * BUF_SIZE) / 8 + 64];
        let base = pa(&mut mem);
        let q = Vq {
            desc: base,
            avail: base.add(64),
            used: base.add(128),
            bufs: base.add(256),
            ..Vq::default()
        };
        let mut net = VirtioNet {
            base: pa(&mut regs),
            mac: Mac::default(),
            qsize: 2,
            rx: Vq::default(),
            tx: Vq::default(),
            irq: None,
            doorbells: 0,
            rx_bad: 0,
        };
        if rx {
            net.rx = q;
        } else {
            net.tx = q;
        }
        Fake {
            _regs: regs,
            _mem: mem,
            net,
        }
    }

    #[test]
    fn receive_validates_the_device_record_before_copying() {
        for (name, id, len, want, recycled) in [
            ("valid", 0u32, 76u32, Some(64usize), true),
            (
                "largest",
                0,
                BUF_SIZE as u32,
                Some(BUF_SIZE - HDR_LEN),
                true,
            ),
            ("oversize", 0, BUF_SIZE as u32 + 1, None, true),
            ("short", 0, HDR_LEN as u32 - 1, None, true),
            ("id-outside-ring", 2, 76, None, false),
            ("id-truncated-to-valid", 1 << 16, 76, None, false),
        ] {
            let mut f = fake(true);
            let n = &mut f.net;
            dev::write16(n.rx.used.add(2), 1);
            dev::write32(n.rx.used.add(4), id);
            dev::write32(n.rx.used.add(8), len);
            dev::copy_in(n.rx.bufs.add(HDR_LEN as u64), &[0x5a; BUF_SIZE - HDR_LEN]);
            let mut out = vec![0xccu8; 8192];
            let got = n.receive(&mut out);
            assert_eq!(got, want, "{name}");
            let k = got.unwrap_or(0);
            assert!(
                out[..k].iter().all(|&b| b == 0x5a),
                "{name}: packet differs"
            );
            assert!(out[k..].iter().all(|&b| b == 0xcc), "{name}: beyond packet");
            assert_eq!(n.rx.last_used, 1, "{name}");
            assert_eq!(n.rx.avail_idx == 1, recycled, "{name}: recycle");
            // Niets gepubliceerd vóór de flush: batching.
            assert_eq!(dev::read16(n.rx.avail.add(2)), 0, "{name}");
            n.flush();
            assert_eq!(
                dev::read16(n.rx.avail.add(2)),
                u16::from(recycled),
                "{name}"
            );
        }
    }

    #[test]
    fn transmit_rejects_bad_sizes_before_touching_dma() {
        for size in [0, BUF_SIZE - HDR_LEN + 1] {
            let mut f = fake(false);
            let n = &mut f.net;
            assert_eq!(n.transmit(&vec![0; size]), Err(TxError::Size(size)));
            assert_eq!(n.tx.avail_idx, 0);
        }
    }

    #[test]
    fn transmit_batches_until_flush_and_stops_when_full() {
        let mut f = fake(false);
        let n = &mut f.net;
        n.transmit(&[1; 60]).unwrap();
        n.transmit(&[2; 60]).unwrap();
        assert_eq!(n.transmit(&[3; 60]), Err(TxError::Full));
        assert_eq!(dev::read16(n.tx.avail.add(2)), 0);
        assert_eq!(n.doorbells, 0);
        n.flush();
        assert_eq!(dev::read16(n.tx.avail.add(2)), 2);
        assert_eq!(n.doorbells, 1);
        n.flush(); // niets nieuws: geen doorbell
        assert_eq!(n.doorbells, 1);
        // Het device verzond er één: er is weer plaats.
        dev::write16(n.tx.used.add(2), 1);
        n.transmit(&[4; 60]).unwrap();
        // Descriptor 0 wijst naar zijn buffer met header plus frame.
        assert_eq!(dev::read32(n.tx.desc.add(8)), (HDR_LEN + 60) as u32);
    }
}
