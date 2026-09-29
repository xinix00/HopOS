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
    let p = slots::plan(4).unwrap();
    assert_eq!((p.app_cores(), p.max_slots()), (3, 3));
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
    assert_eq!(slots::plan(1).unwrap().app_cores(), 1);
}

#[test]
fn app_core_affinity_follows_virt() {
    use crate::slots::mpidr;
    assert_eq!(mpidr(1), 1);
    assert_eq!(mpidr(15), 15);
    assert_eq!(mpidr(17), 0x101);
}
