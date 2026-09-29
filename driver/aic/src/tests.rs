//! De AIC tegen een nep-registerblok in RAM: de offsets uit de ADT, de
//! tabellen erachter, en een event-register dat de test vult.

use super::*;

const CAP0: u64 = 0x4;
const MAXN: u64 = 0xc;
const GLB: u64 = 0x14;
const EXTINT: u64 = 0x2000;
const IACK: u64 = 0x4000;

struct Fake {
    mem: Vec<u64>,
}

impl Fake {
    fn new(nr: u32, max: u32) -> Self {
        let mut f = Fake {
            mem: vec![0u64; 0x8000 / 8],
        };
        f.put(0, 0x3); // versie
        f.put(CAP0, nr);
        f.put(MAXN, max);
        f
    }
    fn base(&self) -> Pa {
        Pa(self.mem.as_ptr() as u64)
    }
    fn put(&mut self, off: u64, v: u32) {
        dev::write32(Pa(self.mem.as_ptr() as u64 + off), v);
    }
    fn get(&self, off: u64) -> u32 {
        dev::read32(self.base().add(off))
    }
    fn props() -> Props {
        Props {
            cap0: CAP0,
            max_num_irq: MAXN,
            extint_base: EXTINT,
            iack: IACK,
            glb_cfg: GLB,
        }
    }
}

/// `Aic::init` op het nep-blok.
fn init(aic: &Aic, f: &Fake, p: Props) -> Result {
    // SAFETY: het blok is een Vec van 32 KB die de test bezit en die langer
    // leeft dan elke toegang via `aic`.
    unsafe { aic.init(f.base(), p) }
}

#[test]
fn init_lays_out_the_tables_and_enables_the_controller() {
    let f = Fake::new(40, 64);
    let aic = Aic::empty();
    assert!(!aic.is_ready());
    init(&aic, &f, Fake::props()).unwrap();
    assert!(aic.is_ready());
    assert_eq!(aic.nr_irq(), 40);
    assert_ne!(f.get(GLB) & GLB_ENABLE, 0, "iBoot laat hem uit");
    // 64 lijnen: 256 bytes config, dan vijf tabellen van twee woorden.
    let sw_set = EXTINT + 4 * 64;
    aic.soft_raise(33);
    assert_eq!(f.get(sw_set + 4), 1 << 1);
    aic.soft_clear(33);
    assert_eq!(f.get(sw_set + 8 + 4), 1 << 1);
    // Buiten `nr_irq`: niets geschreven.
    aic.soft_raise(40);
    assert_eq!(f.get(sw_set + 4), 1 << 1);
    let _ = aic.describe().to_string();
}

#[test]
fn enable_writes_the_target_and_opens_the_mask() {
    let mut f = Fake::new(64, 64);
    f.put(EXTINT + 4 * 5, 0xabc0 | 0x7);
    let aic = Aic::empty();
    init(&aic, &f, Fake::props()).unwrap();
    aic.set_target(0x13); // alleen de onderste vier bits
    aic.enable(Line(5)).unwrap();
    assert_eq!(aic.cfg(5), 0xabc0 | 0x3, "de rest van het woord blijft");
    let mask_clr = EXTINT + 4 * 64 + 3 * 8;
    assert_eq!(f.get(mask_clr), 1 << 5);
    aic.disable(Line(5));
    assert_eq!(f.get(mask_clr - 8), 1 << 5);
    assert_eq!(aic.enable(Line(64)), Err(IrqError::Rejected { line: 64 }));
    assert_eq!(Aic::empty().enable(Line(1)), Err(IrqError::NoController));
}

#[test]
fn claim_decodes_hw_events_and_skips_the_rest() {
    let mut f = Fake::new(64, 64);
    let aic = Aic::empty();
    init(&aic, &f, Fake::props()).unwrap();
    f.put(IACK, (EVENT_TYPE_HW << 16) | 17);
    assert_eq!(aic.claim(), Some(Line(17)));
    // Die 1: het nummer telt `max_irq` op.
    f.put(IACK, (1 << 24) | (EVENT_TYPE_HW << 16) | 2);
    assert_eq!(aic.claim(), Some(Line(66)));
    f.put(IACK, 0);
    assert_eq!(aic.claim(), None);
    // Een vreemd type blijft staan (het nep-register wist niet): begrensd
    // overslaan, geen hang.
    f.put(IACK, 4 << 16);
    assert_eq!(aic.claim(), None);
    assert_eq!(aic.odd_events.load(Relaxed), 16);
}

#[test]
fn impossible_sizes_are_refused() {
    let f = Fake::new(65, 64);
    assert!(matches!(
        init(&Aic::empty(), &f, Fake::props()),
        Err(Error::Sizes { .. })
    ));
    let f = Fake::new(10, 48);
    assert!(init(&Aic::empty(), &f, Fake::props()).is_err());
    let p = Props {
        iack: 0,
        ..Fake::props()
    };
    assert!(matches!(
        init(&Aic::empty(), &f, p),
        Err(Error::Incomplete { .. })
    ));
}

#[test]
fn ipi_target_is_core_and_cluster() {
    // cpu6 van de M4: MPIDR 0x80010100 → core 0, cluster 1.
    assert_eq!(ipi::target(0x8001_0100), 1 << 16);
    assert_eq!(ipi::target(0x8001_0103), (1 << 16) | 3);
    assert_eq!(ipi::target(0x8000_0005), 5);
    assert_eq!(
        ipi::target(0x8001_0102),
        cpu::el2::apple_ipi_target(0x8001_0102)
    );
    assert!(!ipi::ack());
}

#[test]
fn decode_is_die_type_number() {
    assert_eq!(decode(0, 64), None);
    assert_eq!(decode((1 << 16) | 9, 64), Some((1, 9)));
    assert_eq!(decode((2 << 24) | (1 << 16) | 9, 64), Some((1, 137)));
}
