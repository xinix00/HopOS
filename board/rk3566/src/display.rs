//! Het beeld van de Radxa Zero 3E: de framebuffer uit het plan
//! ([`FB_RAM`]) en de scanout erop naar HDMI (`gui-rkscan`). Alleen met de
//! feature `gui`; zonder is het board headless (de stub in `lib.rs`).
//!
//! Eén buffer, twee lezers (Go, 05-08 en 06-08): de VOP2 scant hem uit naar
//! de connector, en de display-app serveert hem over het netwerk (`/kvm`)
//! via de fb-grant. Daarom staat hij in het PLAN en niet in een driver, en
//! daarom is een keten die faalt geen reden om de buffer in te houden: zo
//! deed Go het ook (`Framebuffer()` gaf de buffer terug na
//! `scanoutOnce.Do`, wat de keten ook zei). Zonder HDMI blijft de
//! logconsole erop tekenen en blijft `/kvm` hem tonen.
//!
//! De keten draait één keer: de eerste [`framebuffer`]-aanroep start hem.
//! Twee keer zou de PLL onder een lopende scanout verzetten, en
//! `framebuffer()` wordt vaker gevraagd (de logconsole, de fb-grant bij
//! elke grant en release). Het "één keer" is een atomic, geen slot: alles
//! draait op de executor van de OS-core, en de atomische swap maakt ook
//! een tweede vrager op een andere core onschuldig (die krijgt de buffer
//! en start niets).

use crate::{DMA, FB_RAM, KERN_RAM, POOL_BASE, STAGE_WINDOW};
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use driver_fb::Desc;
use gui_rkscan::{Chain, Error, Layer, Status};

/// De breedte van het beeld. Board-kennis en een vrije keuze in Go (de
/// buffer werd eerst alleen over HTTP bekeken); nu vast, want de keten
/// drijft precies deze modus.
const WIDTH: u32 = gui_rkscan::WIDTH;
/// De hoogte.
const HEIGHT: u32 = gui_rkscan::HEIGHT;
/// Bytes per rij: x8r8g8b8, geen opvulling.
const STRIDE: u32 = WIDTH * 4;

// Het vangnet uit Go's `rk3566.FB()`, nu compile-time: direct boven de
// buffer begint het staging-venster en daarna de pool. Wie de resolutie
// verhoogt zonder FB_RAM mee te nemen, laat de scanout stil in andermans
// geheugen lezen, een fout die zich als willekeurige corruptie voordoet en
// nooit naar het beeld wijst.
const _: () = {
    let size = STRIDE as u64 * HEIGHT as u64;
    assert!(size <= FB_RAM.size);
    // Binnen de ongecachete DMA-regio (Normal-NC in `mmu`: elke store staat
    // meteen in DRAM, waar de VOP2 leest), buiten de kern-RAM, vóór het
    // staging-venster en de pool.
    assert!(FB_RAM.base.0 >= DMA.base.0 && FB_RAM.base.0 + FB_RAM.size <= DMA.base.0 + DMA.size);
    assert!(FB_RAM.base.0 >= KERN_RAM.base.0 + KERN_RAM.size);
    assert!(FB_RAM.base.0 + FB_RAM.size <= STAGE_WINDOW.base.0);
    assert!(FB_RAM.base.0 + FB_RAM.size <= POOL_BASE);
    // MST is 32 bits: de hele buffer onder 4 GB.
    assert!(FB_RAM.base.0 + size <= 1 << 32);
};

/// Is de keten al gestart (door de eerste vrager)?
static STARTED: AtomicBool = AtomicBool::new(false);

/// De framebuffer uit het plan.
///
/// `swap_rb` blijft UIT: GEMETEN 06-08 met een testpatroon van vier balken,
/// rood, groen, blauw, wit stonden in díe volgorde op het scherm, dus de
/// VOP2 leest x8r8g8b8 zoals wij schrijven. Rechte balken bevestigden
/// meteen de stride, en de rand de actieve regio.
const fn desc() -> Desc {
    Desc {
        base: FB_RAM.base,
        width: WIDTH,
        height: HEIGHT,
        stride: STRIDE,
        bpp: 32,
        swap_rb: false,
    }
}

/// De framebuffer; de eerste aanroep brengt de keten op. Altijd `Some`,
/// ook als de keten faalt (zie de moduledoc).
pub(crate) fn framebuffer(now: fn() -> u64) -> Option<Desc> {
    let fb = desc();
    if !STARTED.swap(true, Relaxed) {
        match gui_rkscan::start(fb, now) {
            Ok(st) => report(&st),
            Err(e) => fail(&e, now),
        }
    }
    Some(fb)
}

/// De regels van een geslaagde keten: de identificatie en de marker als
/// laatste.
fn report(st: &Status) {
    cpu::println!(
        "display: HDMI-TX design {:#04x} rev {:#04x} phy {:#04x}, fb {:#x} {WIDTH}x{HEIGHT} stride {STRIDE}",
        st.ids.design,
        st.ids.rev,
        st.ids.config2,
        FB_RAM.base.0
    );
    if !st.latched {
        cpu::println!(
            "display: WARNING VP0 did not latch its configuration within 50 ms, the scan may not run HOPOS_DISPLAY_NOLATCH"
        );
    }
    cpu::println!(
        "display: {WIDTH}x{HEIGHT}p60 on HDMI (sink attached: {}) HOPOS_DISPLAY_UP",
        st.sink
    );
}

/// De regel van een mislukte keten, met de registers van de laag die
/// faalde. Alleen die laag: een blok in een dood domein lezen kan de bus
/// vasthouden, en de laag die faalde heeft net bewezen dat hij antwoordt.
fn fail(e: &Error, now: fn() -> u64) {
    let c = Chain::rk3566(now);
    match (e, e.layer()) {
        (Error::Geometry { .. }, _) => {
            cpu::println!("display: {e}, framebuffer stays network-only (/kvm) HOPOS_DISPLAY_FAIL")
        }
        (_, Layer::Power) => cpu::println!(
            "display: {e}, framebuffer stays network-only (/kvm) ({:?}) HOPOS_DISPLAY_FAIL",
            c.power_info()
        ),
        (_, Layer::Vop2) => cpu::println!(
            "display: {e}, framebuffer stays network-only (/kvm) ({:?}) HOPOS_DISPLAY_FAIL",
            c.vop_info()
        ),
        (_, Layer::Hdmi) => cpu::println!(
            "display: VOP2 scans but {e}, framebuffer stays network-only (/kvm) ({:?}) HOPOS_DISPLAY_FAIL",
            c.hdmi_info()
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_plan_buffer_fits_the_chain() {
        let fb = desc();
        assert_eq!(fb.check(), Ok(4));
        assert!(gui_rkscan::check_geometry(&fb).is_ok());
        assert_eq!(fb.base.0, 0x0700_0000);
        assert!(fb.size().unwrap() <= FB_RAM.size);
    }
}
