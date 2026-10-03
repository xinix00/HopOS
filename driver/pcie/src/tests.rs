//! Host-tests over een nep-config-space: een paar functies achter een
//! root-poort, met BAR's die zich gedragen als echte (alleen de adresbits
//! boven de maat zijn beschrijfbaar) en een capability-lijst.

use super::*;
use std::cell::RefCell;
use std::collections::BTreeMap;

/// Eén functie: 4 KB config-space plus per BAR-dword het beschrijfbare
/// masker (0 = alleen-lezen).
struct Fake {
    space: [u32; 1024],
    wmask: [u32; 1024],
}

impl Fake {
    fn new(vendor: u16, device: u16, class: u32, header: u8) -> Self {
        let mut f = Self {
            space: [0; 1024],
            wmask: [0; 1024],
        };
        f.space[0] = u32::from(vendor) | (u32::from(device) << 16);
        f.space[2] = class << 8 | 1;
        f.space[3] = u32::from(header) << 16;
        // Command beschrijfbaar (laag half).
        f.wmask[1] = 0xffff;
        f
    }

    /// Een memory-BAR van `size` bytes, 64-bit of niet.
    fn bar(mut self, idx: usize, size: u64, is64: bool, addr: u64) -> Self {
        let mask = !(size - 1);
        let ty = if is64 { 0b100 } else { 0 };
        self.space[4 + idx] = (addr as u32 & !0xf) | ty;
        self.wmask[4 + idx] = mask as u32 & !0xf;
        if is64 {
            self.space[5 + idx] = (addr >> 32) as u32;
            self.wmask[5 + idx] = (mask >> 32) as u32;
        }
        self
    }

    /// Capabilities: `(id, offset, extra dwords)` in lijstvolgorde.
    fn caps(mut self, caps: &[(u8, u16, &[u32])]) -> Self {
        self.space[1] |= STATUS_CAP_LIST;
        self.space[0x34 / 4] = u32::from(caps[0].1);
        for (i, (id, off, rest)) in caps.iter().enumerate() {
            let next = caps.get(i + 1).map_or(0, |c| c.1);
            let at = usize::from(*off) / 4;
            self.space[at] = u32::from(*id) | (u32::from(next) << 8) | (rest[0] & 0xffff_0000);
            self.wmask[at] = 0xffff_0000;
            for (j, w) in rest.iter().enumerate().skip(1) {
                self.space[at + j] = *w;
                self.wmask[at + j] = u32::MAX;
            }
        }
        self
    }
}

#[derive(Default)]
struct FakeCfg {
    funcs: RefCell<BTreeMap<Bdf, Fake>>,
}

impl FakeCfg {
    fn add(&self, bdf: Bdf, f: Fake) {
        self.funcs.borrow_mut().insert(bdf, f);
    }
}

impl Config for FakeCfg {
    fn read32(&self, bdf: Bdf, off: u16) -> u32 {
        self.funcs
            .borrow()
            .get(&bdf)
            .map_or(u32::MAX, |f| f.space[usize::from(off / 4)])
    }
    fn write32(&self, bdf: Bdf, off: u16, v: u32) {
        if let Some(f) = self.funcs.borrow_mut().get_mut(&bdf) {
            let i = usize::from(off / 4);
            f.space[i] = (f.space[i] & !f.wmask[i]) | (v & f.wmask[i]);
        }
    }
    fn write16(&self, bdf: Bdf, off: u16, v: u16) {
        let shift = (off & 2) * 8;
        if let Some(f) = self.funcs.borrow_mut().get_mut(&bdf) {
            let i = usize::from(off / 4);
            let m = f.wmask[i] & (0xffff << shift);
            f.space[i] = (f.space[i] & !m) | ((u32::from(v) << shift) & m);
        }
    }
}

fn bdf(bus: u8, dev: u8, func: u8) -> Bdf {
    Bdf::new(bus, dev, func).unwrap()
}

/// Bus 0: een host-bridge (00:00.0), een root-poort (00:01.0) naar bus 1,
/// en een multifunctie-device (00:02.0 en .1). Bus 1: een NIC.
fn fabric() -> FakeCfg {
    let c = FakeCfg::default();
    c.add(bdf(0, 0, 0), Fake::new(0x1b36, 0x0008, 0x060000, 0));
    let mut rp = Fake::new(0x1b36, 0x000c, 0x060400, 1);
    rp.space[6] = 0x0001_0100; // primary 0, secondary 1, subordinate 1
    c.add(bdf(0, 1, 0), rp);
    c.add(bdf(0, 2, 0), Fake::new(0x8086, 0x1111, 0x020000, 0x80));
    c.add(bdf(0, 2, 1), Fake::new(0x8086, 0x1112, 0x020000, 0));
    // Een functie op .3 van een niet-multifunctie-device telt niet.
    c.add(bdf(0, 3, 0), Fake::new(0x1af4, 0x1041, 0x020000, 0));
    c.add(bdf(0, 3, 3), Fake::new(0xdead, 0xbeef, 0, 0));
    c.add(
        bdf(1, 0, 0),
        Fake::new(0x10ec, 0x8126, 0x020000, 0)
            .bar(0, 0x4000, true, 0x1000_0000)
            .bar(2, 0x100, false, 0x2000_0000),
    );
    c
}

#[test]
fn ecam_offsets_follow_the_spec() {
    assert_eq!(bdf(0, 0, 0).ecam_offset(), 0);
    assert_eq!(bdf(1, 0, 0).ecam_offset(), 1 << 20);
    assert_eq!(bdf(0, 1, 0).ecam_offset(), 1 << 15);
    assert_eq!(bdf(0, 0, 7).ecam_offset(), 7 << 12);
    assert_eq!(bdf(0xff, 31, 7).ecam_offset(), 0x0fff_f000);
    assert!(Bdf::new(0, 32, 0).is_none());
    assert!(Bdf::new(0, 0, 8).is_none());
    assert_eq!(bdf(1, 2, 3).to_string(), "01:02.3");
}

#[test]
fn walk_follows_bridges_and_multifunction() {
    let c = fabric();
    let mut found = Vec::new();
    walk(&c, 0, |f| {
        found.push(*f);
        true
    });
    let names: Vec<String> = found.iter().map(ToString::to_string).collect();
    assert_eq!(
        names,
        [
            "00:00.0 1b36:0008 class 060000",
            "00:01.0 1b36:000c class 060400",
            "00:02.0 8086:1111 class 020000",
            "00:02.1 8086:1112 class 020000",
            "00:03.0 1af4:1041 class 020000",
            "01:00.0 10ec:8126 class 020000",
        ]
    );
    assert!(found[1].is_bridge());
    assert!(found[2].multi);
    assert_eq!(found[0].revision, 1);
}

#[test]
fn a_bridge_loop_ends() {
    let c = fabric();
    // De NIC-bus krijgt een bridge terug naar bus 0: gezien, dus klaar.
    let mut back = Fake::new(0x1b36, 0x000c, 0x060400, 1);
    back.space[6] = 0x0000_0001;
    c.add(bdf(1, 1, 0), back);
    let mut n = 0;
    walk(&c, 0, |_| {
        n += 1;
        true
    });
    assert_eq!(n, 7);
}

#[test]
fn find_stops_at_the_first_hit() {
    let c = fabric();
    let nic = find(&c, 0, |f| f.vendor == 0x10ec).unwrap();
    assert_eq!(nic.bdf, bdf(1, 0, 0));
    assert!(find(&c, 0, |f| f.vendor == 0x1234).is_none());
}

#[test]
fn bars_are_read_and_sized_without_moving() {
    let c = fabric();
    let nic = probe(&c, bdf(1, 0, 0)).unwrap();
    nic.set_command(&c, CMD_MEM);
    assert_eq!(
        nic.bar(&c, 0).unwrap(),
        Bar::Mem {
            addr: 0x1000_0000,
            size: 0x4000,
            is64: true,
            prefetch: false
        }
    );
    // De hoge helft van de 64-bit BAR is geen eigen BAR.
    assert_eq!(nic.bar(&c, 1).unwrap(), Bar::Unused);
    assert_eq!(nic.bar(&c, 2).unwrap().mem(), Some((0x2000_0000, 0x100)));
    assert_eq!(nic.bar(&c, 3).unwrap(), Bar::Unused);
    assert!(nic.bar(&c, 6).is_err());
    // Het meten liet adres en decode staan.
    assert_eq!(nic.bar_addr(&c, 0), 0x1000_0000);
    assert_eq!(nic.command(&c), CMD_MEM);
}

#[test]
fn a_bare_fabric_gets_its_bars_from_the_window() {
    let c = FakeCfg::default();
    c.add(
        bdf(0, 0, 0),
        Fake::new(0x144d, 0xa808, 0x010802, 0)
            .bar(0, 0x4000, true, 0)
            .bar(2, 0x1000, false, 0)
            .bar(3, 0x10_0000, false, 0),
    );
    let f = probe(&c, bdf(0, 0, 0)).unwrap();
    let mut win = MmioWindow::new(0x6000_1000, 0x40_0000);
    let bars = f.assign_bars(&c, &mut win).unwrap();
    assert_eq!(bars[0].mem(), Some((0x6000_4000, 0x4000)));
    assert_eq!(bars[2].mem(), Some((0x6000_8000, 0x1000)));
    // Naturel gealigneerd: 1 MB op een MB-grens.
    assert_eq!(bars[3].mem(), Some((0x6010_0000, 0x10_0000)));
    assert_eq!(f.bar_addr(&c, 3), 0x6010_0000);
    assert_eq!(f.command(&c) & CMD_MEM, CMD_MEM);
    // Het venster is op: de volgende weigert.
    assert_eq!(
        win.alloc(0x40_0000),
        Err(Error::NoSpace { size: 0x40_0000 })
    );
}

#[test]
fn enable_sets_decode_and_master() {
    let c = fabric();
    let nic = probe(&c, bdf(1, 0, 0)).unwrap();
    nic.enable(&c);
    assert_eq!(nic.command(&c), CMD_MEM | CMD_MASTER);
}

#[test]
fn capabilities_and_msix() {
    let c = FakeCfg::default();
    c.add(
        bdf(0, 4, 0),
        Fake::new(0x1af4, 0x1041, 0x020000, 0).caps(&[
            (CAP_MSIX, 0x40, &[0x0002_0000, 0x0000_3001, 0x0000_3801]),
            (CAP_VENDOR, 0x50, &[0x0000_0000]),
            (CAP_PCIE, 0x70, &[0, 0, 0, 0, 0x0042_0000]),
        ]),
    );
    let f = probe(&c, bdf(0, 4, 0)).unwrap();
    let ids: Vec<u8> = f.caps(&c).map(|(id, _)| id).collect();
    assert_eq!(ids, [CAP_MSIX, CAP_VENDOR, CAP_PCIE]);
    let m = f.msix(&c).unwrap();
    assert_eq!(m.size, 3);
    assert_eq!(
        m.table,
        BarOffset {
            bar: 1,
            off: 0x3000
        }
    );
    f.msix_enable(&c, &m, true);
    assert_eq!(c.read32(f.bdf, 0x40) >> 16, 0x8002);
    assert_eq!(f.msix_control(&c, &m), 0x8002);
    assert_eq!(f.command(&c) & CMD_INTX_DISABLE, CMD_INTX_DISABLE);

    assert_eq!(f.link(&c), Some(Link { speed: 2, width: 4 }));
}

#[test]
fn a_capability_ring_ends() {
    let c = FakeCfg::default();
    let mut f = Fake::new(1, 2, 0, 0);
    f.space[1] |= STATUS_CAP_LIST;
    f.space[0x34 / 4] = 0x40;
    f.space[0x40 / 4] = 0x09 | (0x40 << 8); // wijst naar zichzelf
    c.add(bdf(0, 0, 0), f);
    let f = probe(&c, bdf(0, 0, 0)).unwrap();
    assert_eq!(f.caps(&c).count(), CAP_WALK_MAX);
    assert!(f.msix(&c).is_none());
}

#[test]
fn msix_table_entries() {
    let mut mem = vec![0u32; 4 * 4];
    let base = Pa(mem.as_mut_ptr() as usize as u64);
    // SAFETY: `mem` leeft de hele test en heeft plek voor vier vectoren.
    let t = unsafe { MsixTable::new(base, 4) };
    assert!(t.set(2, 0x0808_0040, 77));
    assert!(!t.set(4, 0, 0));
    assert_eq!(&mem[8..12], &[0x0808_0040, 0, 77, 0]);
    assert_eq!(t.get(2), Some([0x0808_0040, 0, 77, 0]));
    assert_eq!(t.get(4), None);
}

#[test]
fn ecam_refuses_buses_outside_its_window() {
    // SAFETY: er wordt niets gelezen: bus 5 valt buiten 0..=0.
    let e = unsafe { Ecam::new(Pa(0x1000), 0, 0) };
    assert_eq!(e.read32(bdf(5, 0, 0), 0), u32::MAX);
    assert!(probe(&e, bdf(5, 0, 0)).is_none());
    assert_eq!(e.buses(), (0, 0));
}

#[test]
fn intx_swizzles_up_to_the_root_bus() {
    let c = fabric();
    // De NIC op 01:00.0 met INTB (pin 2 in het register): achter de
    // root-poort op 00:01.0 wordt dat (1 + 0) % 4 = INTB van device 1.
    c.funcs.borrow_mut().get_mut(&bdf(1, 0, 0)).unwrap().space[0x3c / 4] = 2 << 8;
    let nic = probe(&c, bdf(1, 0, 0)).unwrap();
    assert_eq!(nic.intx_pin(&c), Some(1));
    assert_eq!(intx_at_root(&c, 0, &nic), Some((1, 1)));
    // Een functie op de root-bus zelf: geen swizzle.
    c.funcs.borrow_mut().get_mut(&bdf(0, 2, 0)).unwrap().space[0x3c / 4] = 1 << 8;
    let f = probe(&c, bdf(0, 2, 0)).unwrap();
    assert_eq!(intx_at_root(&c, 0, &f), Some((2, 0)));
    // Zonder pin: geen INTx.
    let none = probe(&c, bdf(0, 3, 0)).unwrap();
    assert_eq!(intx_at_root(&c, 0, &none), None);
    assert_eq!(swizzle(3, 2), 1);
}

#[test]
fn msix_table_sits_in_its_bar() {
    let c = fabric();
    let nic = probe(&c, bdf(1, 0, 0)).unwrap();
    let m = Msix {
        cap: 0x50,
        size: 4,
        table: BarOffset { bar: 2, off: 0x40 },
    };
    assert_eq!(nic.msix_table_addr(&c, &m), Some(0x2000_0040));
    let unassigned = Msix {
        table: BarOffset { bar: 4, off: 0 },
        ..m
    };
    assert_eq!(nic.msix_table_addr(&c, &unassigned), None);
}
