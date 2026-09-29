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
