//! Host-tests van de board-logica die zonder ijzer te toetsen is.

use super::*;

#[test]
fn the_plan_is_consistent() {
    let p = QemuVirt::new().plan();
    assert_eq!(p.kern_ram.end(), p.dma.base);
    assert!(p.dma.contains(p.net_dma.base));
    assert!(p.net_dma.end().0 <= p.dma.end().0);
    assert!(p.kern_ram.contains(DTB_FALLBACK));
}

#[test]
fn a_dtb_outside_the_kern_ram_is_refused() {
    assert!(dtb_at(0).is_none());
    assert!(dtb_at(0x0900_0000).is_none());
    assert!(dtb_at(DMA.base.0).is_none());
}

#[test]
fn homogeneous_cores_are_all_big() {
    let b = QemuVirt::new();
    assert_eq!(b.cores(), CORES_DEFAULT);
    assert!((0..b.cores()).all(|c| b.core_class(c) == CoreClass::Big));
}

#[test]
fn the_slot_plan_validates_and_keeps_clear_of_the_kern() {
    use crate::slots::{self, STAGE_HDR_PA, STAGE_MAX, STAGE_PA};
    let p = slots::plan(4, 0).unwrap();
    assert_eq!((p.app_cores(), p.max_slots()), (3, 4));
    assert_eq!(p.vec_base_pa().0, slots::CAGE_PA);
    let stage = abi::Region::new(STAGE_HDR_PA, STAGE_PA + STAGE_MAX - STAGE_HDR_PA);
    for r in p.pool() {
        assert!(r.base >= DMA.end().0, "pool {r:?} in the kern RAM or DMA");
        assert!(!r.overlaps(stage), "pool {r:?} over the staging");
        assert!(
            !r.overlaps(slots::DEVICE_WINDOW),
            "pool {r:?} over the cage"
        );
    }
    // Eén core: toch één app-core in het plan, zodat het plan valideert;
    // plaatsen faalt dan pas bij de core-toets.
    assert_eq!(slots::plan(1, 0).unwrap().app_cores(), 1);
    // Twee cores met de kern op core 1: app-core 1 is fysiek core 0.
    let p = slots::plan(2, 1).unwrap();
    assert_eq!((p.app_cores(), p.max_slots(), p.os_core()), (1, 2, 1));
    assert_eq!(p.phys_core(abi::layout::Core::new(1).unwrap()), 0);
    assert!(slots::plan(2, 2).is_err());
}

#[test]
fn the_os_core_comes_from_the_bootargs() {
    let big = |_: usize| CoreClass::Big;
    assert_eq!(os_core_of("", 4, big), (0, None));
    assert_eq!(os_core_of("console=x hopos.oscore=2", 4, big), (2, None));
    assert_eq!(os_core_of("hopos.oscore=big", 4, big), (0, None));
    // Een klasse die er niet is, of een core die er niet is: de boot-core,
    // luid.
    assert!(matches!(
        os_core_of("hopos.oscore=small", 4, big),
        (0, Some(_))
    ));
    assert!(matches!(os_core_of("hopos.oscore=4", 4, big), (0, Some(_))));
    assert!(matches!(os_core_of("hopos.oscore=x", 4, big), (0, Some(_))));
    let mixed = |c: usize| {
        if c >= 2 {
            CoreClass::Small
        } else {
            CoreClass::Big
        }
    };
    assert_eq!(os_core_of("hopos.oscore=small", 4, mixed), (2, None));
}

#[test]
fn the_kick_aims_at_the_os_core() {
    assert_eq!(
        driver_gicv3::sgi1r(slots::mpidr(1), KICK_SGI),
        (8 << 24) | 2
    );
}

#[test]
fn app_core_affinity_follows_virt() {
    use crate::slots::mpidr;
    assert_eq!(mpidr(1), 1);
    assert_eq!(mpidr(15), 15);
    assert_eq!(mpidr(17), 0x101);
    for c in [0, 1, 15, 16, 17, 31] {
        assert_eq!(crate::slots::core_of(mpidr(c)), c);
    }
}
