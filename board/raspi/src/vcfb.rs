//! De framebuffer van de Pi's: eerst de simple-framebuffer uit de DTB (wat
//! Linux' vroege console ook leest), anders het beeld opeisen via de
//! VideoCore-mailbox (`FB_ALLOC`, het officiële pad; nog altijd
//! "firmware-buffer, geen driver"). GEMETEN 11-07 op beide Pi's: de
//! firmware laat aan een raw kernel geen simplefb-node na, ook niet met
//! HDMI erin, dus de mailbox is op ijzer het gewone pad.
//!
//! Dit bezit de ene ontdekking per boot en haar uitkomst. Het renderen is
//! van `driver-fb`, het weggeven van `gui-fbgrant`.
//!
//! EEN KEER, daarna vast (19-07): elke `FB_ALLOC` is een verse
//! firmware-allocatie, en de scanout blijft aan de BOOT-buffer hangen. Een
//! tweede allocatie kan een ander, niet-gescand adres geven. Ook een
//! mislukking of een onzinnig antwoord is definitief: de allocate-tag kan
//! geslaagd zijn terwijl de pitch-tag rommel bevat, en opnieuw proberen
//! stapelt dan bij elke vraag een verse, niet meer vrij te geven buffer.
//! Een Pi waarop deze ene probe faalt, draait veilig headless tot de
//! volgende boot.
//!
//! De respons telt, niet het verzoek, en binnen de respons telt de PITCH:
//! GEMETEN 11-07 (Pi 5) meldt de depth-tag 32 terwijl de scanout op de
//! 16-bpp-config van de bootsplash blijft (stride 3840 = 1920 x 2). De
//! pitch beschrijft wat de scanout echt leest, dus daaruit volgt de diepte.
//!
//! De map: de buffer ligt in het geheugen van de VideoCore, buiten elke
//! `/memory`-bank van de DTB, en staat dus niet in de tabellen van de kern.
//! [`plan_map`] zet de 2 MB-blokken eromheen erbij (alleen lege regels,
//! niets wat vast staat), Normal-NC. De freeze-jacht van 04-08 leert waarom
//! [`sane`] streng is: een verkeerd gelezen adres (de /chosen-cellen,
//! base=0x3f800000003f4800) werd drie weken als "32-bpp-freeze van het
//! silicium" gezien, terwijl de eerste pixel-veeg een bus-fault-reset was.

use crate::map::{self, MB2, Tables};
use abi::Region;
use core::cell::RefCell;
use dev::Pa;
use driver_fb::Desc;
use driver_vcmail::Mbox;
use sync::Local;

/// De maat die we de firmware vragen: 1080p, zoals Go.
const WANT: (u32, u32) = (1920, 1080);

/// Boven dit adres ligt geen Pi-geheugen (de BCM2712 heeft 36 bits, de
/// BCM2711 35): een buffer daarboven is een verkeerd gelezen getal.
const PA_LIMIT: u64 = 1 << 36;

/// De uitkomst van de ene ontdekking.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) enum State {
    /// Nog niet geprobeerd.
    Untried,
    /// Geprobeerd; `None` = headless tot de volgende boot.
    Done(Option<Desc>),
}

/// De staat van deze boot: één eigenaar, de executor van de OS-core.
static STATE: Local<RefCell<State>> = Local::new(RefCell::new(State::Untried));

/// Voert `find` hoogstens één keer uit en onthoudt ook een mislukking of
/// een onzinnig antwoord (Go: `discoveryState.get`).
pub(crate) fn once(state: &mut State, find: impl FnOnce() -> Option<Desc>) -> Option<Desc> {
    if let State::Done(d) = *state {
        return d;
    }
    let d = find().filter(sane);
    *state = State::Done(d);
    d
}

/// Weert onzin (mailboxruis, verkeerd gelezen cellen) uit de cache en uit
/// de grant.
pub(crate) fn sane(d: &Desc) -> bool {
    d.base.0 != 0
        && d.size()
            .and_then(|n| d.base.0.checked_add(n))
            .is_some_and(|end| end <= PA_LIMIT)
        && (64..=8192).contains(&d.width)
        && (64..=8192).contains(&d.height)
        && d.check().is_ok()
}

/// De descriptor uit een DTB-simplefb.
pub(crate) fn from_fdt(f: fw::fdt::Fb) -> Desc {
    Desc {
        base: Pa(f.base),
        width: f.width,
        height: f.height,
        stride: f.stride,
        bpp: u8::try_from(f.bpp).unwrap_or(0),
        swap_rb: false,
    }
}

/// De descriptor uit het mailbox-antwoord: de diepte volgt uit de pitch.
pub(crate) fn from_mbox(f: driver_vcmail::Fb) -> Desc {
    let bpp = f
        .pitch
        .checked_div(f.width)
        .map_or(0, |b| b.saturating_mul(8));
    Desc {
        base: Pa(f.base),
        width: f.width,
        height: f.height,
        stride: f.pitch,
        bpp: u8::try_from(bpp).unwrap_or(0),
        swap_rb: false,
    }
}

/// De framebuffer van deze boot: de eerste vraag ontdekt (DTB, dan de
/// mailbox) en mapt, elke volgende krijgt dezelfde uitkomst.
pub(crate) fn framebuffer(mbox: &RefCell<Option<Mbox>>, tables: Option<Tables>) -> Option<Desc> {
    let Ok(mut state) = STATE.try_borrow_mut() else {
        return None;
    };
    once(&mut state, || {
        let d = discover(mbox)?;
        let Some(t) = tables else {
            cpu::println!("fb: no page tables to map the VideoCore buffer HOPOS_FB_MAP_FAIL");
            return None;
        };
        let size = d.size()?;
        if !plan_map(Region::new(d.base.0, size), &t, dev::write64) {
            cpu::println!(
                "fb: VideoCore buffer {:#x}+{size:#x} is outside the first GB tables HOPOS_FB_MAP_FAIL",
                d.base.0
            );
            return None;
        }
        crate::arch::tables_changed();
        cpu::println!(
            "fb: VideoCore framebuffer {}x{} stride {} {} bpp @ {:#x} HOPOS_FB_UP",
            d.width,
            d.height,
            d.stride,
            d.bpp,
            d.base.0
        );
        Some(d)
    })
}

/// Hoe lang `discover` op de firmware wacht als die de allocatie weigert
/// (0x80000001: een tag faalde, "partial response"). GEMETEN 30-09 op de
/// Pi 5: op een koude boot weigerde de firmware de FB-vraag die elke
/// geflipte kern minuten later wél kreeg (dezelfde vraag, dezelfde
/// buffer); het beeld staat dan nog niet. Een weigering is geen
/// allocatie, dus opnieuw vragen stapelt niets (de regel hierboven gaat
/// over een geslaagde maar onzinnige respons). Vijf seconden in stappen van
/// een kwart; een Pi zonder scherm betaalt ze één keer per boot, alleen in
/// de gui-smaak.
const FB_WAIT_NS: u64 = 5_000_000_000;
/// De stap tussen twee vragen.
const FB_STEP_NS: u64 = 250_000_000;

/// DTB eerst, dan de mailbox, met geduld voor een firmware die het beeld
/// nog opzet.
fn discover(mbox: &RefCell<Option<Mbox>>) -> Option<Desc> {
    if let Some(f) = crate::fdt().and_then(|f| f.framebuffer()) {
        return Some(from_fdt(f));
    }
    let mut m = mbox.try_borrow_mut().ok()?;
    let m = m.as_mut()?;
    let start = cpu::idle::now();
    let mut refusals = 0u32;
    loop {
        match m.alloc_fb(WANT.0, WANT.1) {
            Ok(f) => {
                if refusals > 0 {
                    cpu::println!(
                        "fb: firmware ready after {} ms and {refusals} refusal(s)",
                        cpu::idle::now().saturating_sub(start) / 1_000_000
                    );
                }
                return Some(from_mbox(f));
            }
            Err(driver_vcmail::Error::Refused { code })
                if cpu::idle::now().saturating_sub(start) < FB_WAIT_NS =>
            {
                if refusals == 0 {
                    cpu::println!(
                        "fb: firmware refused the framebuffer ({code:#x}), waiting up to {} s for the display",
                        FB_WAIT_NS / 1_000_000_000
                    );
                }
                refusals += 1;
                // De watchdog van de vorige kern loopt door dit geduld heen
                // (een flip-landing, 30-09): elke stap een herlaad.
                crate::watchdog::reload_if_armed(crate::watchdog::MAX_MS);
                dev::delay(cpu::idle::now, FB_STEP_NS);
            }
            Err(e) => {
                cpu::println!("fb: mailbox framebuffer: {e} HOPOS_FB_NONE");
                return None;
            }
        }
    }
}

/// Rekent de tabelregels voor de buffer `fb`: elk 2 MB-blok dat hem raakt,
/// Normal-NC, via `put`. Alleen in een gigabyte met een eigen
/// niveau-2-tabel (de eerste, waar de VideoCore zijn geheugen heeft), nooit
/// over iets wat vast staat, en alleen regels die nu leeg zijn: een blok
/// dat al RAM is, houdt zijn attribuut. `false` = de buffer valt buiten
/// wat we kunnen mappen.
pub(crate) fn plan_map(fb: Region, t: &Tables, mut put: impl FnMut(Pa, u64)) -> bool {
    let Some(end) = fb.base.checked_add(fb.size) else {
        return false;
    };
    let lo = fb.base & !(MB2 - 1);
    let hi = end.next_multiple_of(MB2);
    // Eerst toetsen, dan schrijven: geen half gemapte buffer.
    let mut a = lo;
    while a < hi {
        let Some(l2) = t.l2.iter().flatten().find(|l| l.gb == a / map::GB) else {
            return false;
        };
        if l2.fixed.overlaps(Region::new(a, MB2)) {
            return false;
        }
        a += MB2;
    }
    let mut a = lo;
    while a < hi {
        if let Some(l2) = t.l2.iter().flatten().find(|l| l.gb == a / map::GB) {
            let entry = l2.table.add(8 * ((a % map::GB) / MB2));
            if dev::read64(entry) == 0 {
                put(entry, cpu::boot::block(a, cpu::boot::ATTR_NORMAL_NC));
            }
        }
        a += MB2;
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::map::L2;

    fn valid() -> Desc {
        Desc {
            base: Pa(0x10_0000),
            width: 1920,
            height: 1080,
            stride: 1920 * 4,
            bpp: 32,
            swap_rb: false,
        }
    }

    // Go: TestDiscoveryFailureIsCached.
    #[test]
    fn discovery_failure_is_cached() {
        let mut s = State::Untried;
        let mut calls = 0;
        assert!(
            once(&mut s, || {
                calls += 1;
                None
            })
            .is_none()
        );
        assert!(
            once(&mut s, || {
                calls += 1;
                Some(valid())
            })
            .is_none(),
            "a second discovery ran after the fail-once"
        );
        assert_eq!(calls, 1);
    }

    // Go: TestDiscoveryMalformedAllocationIsCachedAsFailure.
    #[test]
    fn discovery_malformed_allocation_is_cached_as_failure() {
        let mut s = State::Untried;
        let mut calls = 0;
        let bad = Desc {
            stride: 0,
            ..valid()
        };
        for _ in 0..2 {
            assert!(
                once(&mut s, || {
                    calls += 1;
                    Some(bad)
                })
                .is_none()
            );
        }
        assert_eq!(calls, 1);
    }

    // Go: TestDiscoverySuccessIsCached.
    #[test]
    fn discovery_success_is_cached() {
        let mut s = State::Untried;
        let mut calls = 0;
        for _ in 0..2 {
            let got = once(&mut s, || {
                calls += 1;
                Some(valid())
            });
            assert_eq!(got, Some(valid()));
        }
        assert_eq!(calls, 1);
    }

    #[test]
    fn the_pitch_decides_the_depth() {
        // Pi 5, 11-07: 32 bpp gevraagd, de scanout bleef op 16.
        let d = from_mbox(driver_vcmail::Fb {
            base: 0x3e40_0000,
            pitch: 3840,
            width: 1920,
            height: 1080,
            depth: 32,
        });
        assert_eq!(d.bpp, 16);
        assert!(sane(&d));
        let zero = from_mbox(driver_vcmail::Fb {
            base: 0x3e40_0000,
            pitch: 7680,
            width: 0,
            height: 1080,
            depth: 32,
        });
        assert!(!sane(&zero));
        // Het adres van de freeze-jacht (04-08): nooit een framebuffer.
        let wild = Desc {
            base: Pa(0x3f80_0000_003f_4800),
            ..valid()
        };
        assert!(!sane(&wild));
    }

    #[test]
    fn plan_map_fills_only_empty_rows_in_gb0() {
        // Een nep-L2 van GB0 op de host; blok 1 is al RAM.
        let table = vec![0u64; 512];
        let tp = Pa(table.as_ptr() as u64);
        dev::write64(tp.add(8 * 0x1f3), 0xdead_0001);
        let t = Tables {
            l1: Pa(0),
            l2: [
                Some(L2 {
                    gb: 0,
                    table: tp,
                    fixed: Region::new(0, map::FIXED_END),
                }),
                None,
            ],
        };
        let fb = Region::new(0x3e6f_a000, 1920 * 1080 * 4);
        let mut puts = Vec::new();
        assert!(plan_map(fb, &t, |pa, v| puts.push((pa, v))));
        let lo = 0x3e6f_a000u64 & !(MB2 - 1);
        let hi = (0x3e6f_a000u64 + 1920 * 1080 * 4).next_multiple_of(MB2);
        // Het eerste blok (0x1f3) was al gemapt en houdt zijn regel.
        assert_eq!(puts.len() as u64, (hi - lo) / MB2 - 1);
        for (i, (pa, v)) in puts.iter().enumerate() {
            let a = lo + (i as u64 + 1) * MB2;
            assert_eq!(pa.0, tp.0 + 8 * (a / MB2));
            assert_eq!(*v, cpu::boot::block(a, cpu::boot::ATTR_NORMAL_NC));
        }
        // Over de vaste regio's of buiten GB0: weigeren, niets schrijven.
        let mut n = 0;
        assert!(!plan_map(Region::new(0x1000_0000, 4096), &t, |_, _| n += 1));
        assert!(!plan_map(Region::new(0x4000_0000, 4096), &t, |_, _| n += 1));
        assert_eq!(n, 0);
        // Een blok dat al gemapt is, houdt zijn regel.
        let mut puts = Vec::new();
        assert!(plan_map(Region::new(0x3e60_0000, 4096), &t, |pa, v| puts
            .push((pa, v))));
        assert!(puts.is_empty());
    }
}
