//! `bounds_test.go`, geport, plus de hele keten op nep-registerblokken in
//! RAM.
//!
//! De blokken zijn gewoon geheugen; het model hieronder (`Model::on_write`)
//! speelt het silicium: het ziet elke schrijf van de driver via het
//! journaal en zet wat de hardware zou zetten (PWR_ST, de PLL-lock, de
//! done-bits van de twee I2C-masters met hun write-1-to-clear, de
//! PHY-lock, de EDID-bytes, het latchen van REG_CFG_DONE). Zo bewijst de
//! host de waarden én de volgorde, en de klok is nep: een grens die
//! verstrijkt, kost geen echte tijd.

use super::*;
use crate::edid::{self, Mode};
use crate::hdmi::*;
use crate::pd::{PD_VO_PWR, PD_VO_REQ};
use crate::vop2::*;
use std::cell::{Cell, RefCell};
use std::format;
use std::vec::Vec;

/// Eén pagina nep-registergeheugen.
#[derive(Clone)]
#[repr(C, align(4096))]
struct Page([u8; 4096]);

const PAGE: u64 = 4096;

/// Het silicium, gespeeld.
struct Model {
    b: Blocks,
    /// Schakelt PD_VO als erom gevraagd wordt.
    pd_responds: bool,
    /// HPLL lockt.
    hpll_locks: bool,
    /// VP0 latcht REG_CFG_DONE.
    vop_latches: bool,
    /// De PHY-I2C-master meldt done.
    phy_i2c_answers: bool,
    /// De PHY lockt na power-on.
    phy_locks: bool,
    /// Er hangt een sink aan de kabel (HPD en DDC).
    sink: bool,
    /// De EDID van de sink.
    edid: [u8; edid::BLOCK],
    /// IH_I2CMPHY_STAT0 (write-1-to-clear).
    phy_stat: u8,
    /// IH_I2CM_STAT0 (write-1-to-clear).
    ddc_stat: u8,
    /// Wat de PHY via zijn I2C-master kreeg.
    phy_writes: Vec<(u8, u16)>,
}

std::thread_local! {
    static MODEL: RefCell<Option<Model>> = const { RefCell::new(None) };
    static NOW: Cell<u64> = const { Cell::new(0) };
}

/// Een klok die per lees een microseconde verspringt.
fn clock() -> u64 {
    NOW.with(|n| {
        n.set(n.get() + 1_000);
        n.get()
    })
}

fn now() -> u64 {
    NOW.with(Cell::get)
}

/// De haak van het journaal: elke schrijf van de driver komt hier langs.
pub(crate) fn on_write(pa: u64, v: u32) {
    MODEL.with(|m| {
        if let Some(m) = m.borrow_mut().as_mut() {
            m.on_write(pa, v);
        }
    });
}

fn rel(pa: u64, base: Pa, size: u64) -> Option<u64> {
    pa.checked_sub(base.0).filter(|&o| o < size)
}

fn clear(pa: Pa, mask: u32) {
    dev::write32(pa, dev::read32(pa) & !mask);
}

impl Model {
    fn hd(&self, r: u16) -> Pa {
        self.b.hdmi.add(at(r))
    }

    fn on_write(&mut self, pa: u64, v: u32) {
        let b = self.b;
        if let Some(o) = rel(pa, b.pmu, PAGE) {
            let asks_on = |bit: u32| v & (bit << 16) != 0 && v & bit == 0;
            if o == 0xA0 && self.pd_responds && asks_on(PD_VO_PWR) {
                clear(b.pmu.add(0x98), PD_VO_PWR);
            }
            if o == 0x50 && self.pd_responds && asks_on(PD_VO_REQ) {
                clear(b.pmu.add(0x60), PD_VO_REQ);
                clear(b.pmu.add(0x68), PD_VO_REQ);
            }
        }
        if rel(pa, b.pmucru, PAGE) == Some(0x44) && self.hpll_locks {
            dev::write32(b.pmucru.add(0x44), v | PLL_LOCK);
        }
        if rel(pa, b.vop, PAGE) == Some(0) && v & CFG_DONE_VP0 != 0 && self.vop_latches {
            dev::write32(b.vop, v & !CFG_DONE_VP0);
        }
        if let Some(o) = rel(pa, b.hdmi, 0x20000) {
            self.on_hdmi((o >> 2) as u16, v as u8);
        }
    }

    fn on_hdmi(&mut self, r: u16, v: u8) {
        let rd = |r: u16| dev::read32(self.hd(r)) as u8;
        match r {
            PHY_I2CM_OPERATION if v == I2C_OP_WRITE && self.phy_i2c_answers => {
                let data = u16::from_be_bytes([rd(PHY_I2CM_DATAO_1), rd(PHY_I2CM_DATAO_0)]);
                self.phy_writes.push((rd(PHY_I2CM_ADDRESS), data));
                self.phy_stat |= I2C_STAT_DONE;
            }
            IH_I2CMPHY_STAT0 => self.phy_stat &= !v,
            PHY_CONF0 => {
                let lock = self.phy_locks && v & CONF0_TXPWRON != 0 && v & CONF0_PDDQ == 0;
                let hpd = if self.sink { STAT0_HPD } else { 0 };
                dev::write32(self.hd(PHY_STAT0), u32::from(u8::from(lock) | hpd));
            }
            I2CM_OPERATION if v == I2C_OP_READ => {
                if self.sink && rd(I2CM_SLAVE) == DDC_EDID_ADDR {
                    let byte = self.edid[usize::from(rd(I2CM_ADDRESS))];
                    dev::write32(self.hd(I2CM_DATAI), u32::from(byte));
                    self.ddc_stat |= I2C_STAT_DONE;
                } else {
                    self.ddc_stat |= I2C_STAT_ERROR;
                }
            }
            IH_I2CM_STAT0 => self.ddc_stat &= !v,
            _ => {}
        }
        dev::write32(self.hd(IH_I2CMPHY_STAT0), u32::from(self.phy_stat));
        dev::write32(self.hd(IH_I2CM_STAT0), u32::from(self.ddc_stat));
    }
}

/// De nep-blokken, het model en een keten erop.
struct Fake {
    _mem: Vec<Page>,
    b: Blocks,
}

impl Fake {
    /// Een RK3566 zoals U-Boot hem achterlaat: PD_VO uit, de IOMMU van de
    /// eerste instantie aan, auto-gating aan, een sink met een
    /// 1080p60-EDID.
    fn new() -> Self {
        let mut mem = vec![Page([0; 4096]); 40];
        let base = mem.as_mut_ptr() as u64;
        let page = |n: u64| Pa(base + n * PAGE);
        let b = Blocks {
            cru: page(0),
            pmucru: page(1),
            pmu: page(2),
            grf: page(3),
            vop: page(4),
            hdmi: page(8),
        };
        dev::write32(b.pmu.add(0x98), PD_VO_PWR);
        dev::write32(b.pmu.add(0x60), PD_VO_REQ);
        dev::write32(b.pmu.add(0x68), PD_VO_REQ);
        dev::write32(b.pmucru.add(0x48), 0xABCD_1234);
        dev::write32(b.vop.add(0x004), 0x4015_8023); // GEMETEN 06-08
        dev::write32(b.vop.add(0x008), 1 << 31);
        dev::write32(b.vop.add(0x028), 0x1);
        dev::write32(b.vop.add(IOMMU[0] + 4), 1);
        let hd = |r: u16, v: u32| dev::write32(b.hdmi.add(at(r)), v);
        hd(DESIGN_ID, 0x20);
        hd(REVISION_ID, 0x2A);
        hd(PRODUCT_ID0, 0xA0);
        hd(PRODUCT_ID1, 0xC1); // met HDCP-bits: telt niet mee
        hd(CONFIG2_ID, 0xF3);
        hd(PHY_STAT0, u32::from(STAT0_HPD));
        hd(A_VIDPOLCFG, 0x0F);
        hd(A_HDCPCFG0, 0xFF);
        MODEL.with(|m| {
            *m.borrow_mut() = Some(Model {
                b,
                pd_responds: true,
                hpll_locks: true,
                vop_latches: true,
                phy_i2c_answers: true,
                phy_locks: true,
                sink: true,
                edid: edid::tests::monitor_1080p60(),
                phy_stat: 0,
                ddc_stat: 0,
                phy_writes: Vec::new(),
            });
        });
        let _ = journal::take();
        Self { _mem: mem, b }
    }

    fn model(&self, f: impl FnOnce(&mut Model)) {
        MODEL.with(|m| f(m.borrow_mut().as_mut().unwrap()));
    }

    fn chain(&self) -> Chain {
        // SAFETY: de blokken zijn RAM van de juiste maat dat leeft zolang
        // `self`, en de test gebruikt de keten niet langer.
        unsafe { Chain::new(self.b, clock) }
    }

    fn phy_writes(&self) -> Vec<(u8, u16)> {
        MODEL.with(|m| m.borrow().as_ref().unwrap().phy_writes.clone())
    }

    fn rd(&self, pa: Pa) -> u32 {
        dev::read32(pa)
    }

    fn hd(&self, r: u16) -> Pa {
        self.b.hdmi.add(at(r))
    }
}

/// Het journaal van de laatste run.
struct Log(Vec<(u64, u32)>);

impl Log {
    /// Alle waarden die naar `pa` gingen, in volgorde.
    fn to(&self, pa: Pa) -> Vec<u32> {
        self.0
            .iter()
            .filter(|(a, _)| *a == pa.0)
            .map(|&(_, v)| v)
            .collect()
    }

    /// De index van de eerste schrijf van `v` naar `pa`.
    fn at(&self, pa: Pa, v: u32) -> usize {
        self.0
            .iter()
            .position(|&w| w == (pa.0, v))
            .unwrap_or_else(|| panic!("no write of {v:#x} to {:#x}", pa.0))
    }

    /// De index van de laatste schrijf naar `pa`.
    fn last(&self, pa: Pa) -> usize {
        self.0
            .iter()
            .rposition(|&(a, _)| a == pa.0)
            .unwrap_or_else(|| panic!("no write to {:#x}", pa.0))
    }
}

fn fb() -> Desc {
    Desc {
        base: Pa(0x0700_0000),
        width: 1920,
        height: 1080,
        stride: 1920 * 4,
        bpp: 32,
        swap_rb: false,
    }
}

// ---------------------------------------------------------------------------
// bounds_test.go
// ---------------------------------------------------------------------------

#[test]
fn invalid_scanout_rejected_before_registers() {
    let f = Fake::new();
    let c = f.chain();
    let cases = [
        (1, 0, 1080, 7680),
        (1, 1920, 1080, 4),
        (1 << 32, 1920, 1080, 7680),
        (0xffff_0000, 1920, 1080, 7680),
        // Van ons: nul, een stride die geen veelvoud van vier is, en de
        // verkeerde diepte.
        (0, 1920, 1080, 7680),
        (0x0700_0000, 1920, 1080, 7682),
    ];
    for (base, width, height, stride) in cases {
        let d = Desc {
            base: Pa(base),
            width,
            height,
            stride,
            ..fb()
        };
        assert!(check_geometry(&d).is_err(), "accepted {d:?}");
        assert!(c.vop_scanout(&d).is_err(), "scanout accepted {d:?}");
        assert!(c.start(&d).is_err(), "start accepted {d:?}");
    }
    let d16 = Desc { bpp: 16, ..fb() };
    assert!(matches!(
        c.start(&d16),
        Err(Error::Geometry { bpp: 16, .. })
    ));
    assert!(journal::take().is_empty(), "a register was written");
    // De grens: een buffer die precies tot 4 GB loopt, past.
    let top = Desc {
        base: Pa((1 << 32) - 7680 * 1080),
        ..fb()
    };
    assert!(check_geometry(&top).is_ok());
    assert!(check_geometry(&fb()).is_ok());
}

// ---------------------------------------------------------------------------
// De keten.
// ---------------------------------------------------------------------------

#[test]
fn the_whole_chain_comes_up_in_order() {
    let f = Fake::new();
    let st = f.chain().start(&fb()).unwrap();
    let log = Log(journal::take());
    assert!(st.sink && st.latched);
    assert_eq!(st.ids.config2, 0xF3);
    let info = st.edid.unwrap();
    assert_eq!(info.preferred, Mode::CEA_1080P60);
    assert_eq!(info.vendor_str(), "DEL");

    let (b, cru, pmu, pc) = (f.b, f.b.cru, f.b.pmu, f.b.pmucru);
    // Het domein: eerst de klokken (alle zeven gates in één hiword-write),
    // dan aan, dan de idle-request los.
    assert_eq!(log.to(cru.add(0x350)), [0x0747_0000]);
    assert_eq!(log.to(pmu.add(0xA0)), [0x0080_0000]);
    assert_eq!(log.to(pmu.add(0x50)), [0x0010_0000]);
    assert!(log.at(cru.add(0x350), 0x0747_0000) < log.at(pmu.add(0xA0), 0x0080_0000));
    assert!(log.at(pmu.add(0xA0), 0x0080_0000) < log.at(pmu.add(0x50), 0x0010_0000));

    // HPLL: SLOW, coëfficiënten, NORM, dan de deler en de HDMI-referentie.
    let slow = log.at(pc.add(0x80), 0x000C_0000);
    let con0 = log.at(pc.add(0x40), 0x7FFF_4063);
    let con1 = log.at(pc.add(0x44), 0x11FF_1101);
    let norm = log.at(pc.add(0x80), 0x000C_0004);
    let sel39 = log.at(cru.add(0x19C), 0x0CFF_0000);
    assert!(slow < con0 && con0 < con1 && con1 < norm && norm < sel39);
    assert_eq!(
        log.to(pc.add(0x48)),
        [0xAB00_0000],
        "frac is RMW, not hiword"
    );
    assert_eq!(log.to(pc.add(0x120)), [0x0080_0000]);
    // De klokboom is niet van `start` (zoals in Go).
    assert!(log.to(cru.add(0x194)).is_empty());

    // De IOMMU: alleen de instantie die pagede, en stall, uit, los.
    assert_eq!(log.to(b.vop.add(IOMMU[0] + 8)), [2, 1, 3]);
    assert!(log.to(b.vop.add(IOMMU[1] + 8)).is_empty());

    // Het venster: base, stride in woorden, maat, enable als laatste.
    let w = b.vop.add(SMART0);
    assert_eq!(log.to(w.add(0x14)), [0x0700_0000]);
    assert_eq!(log.to(w.add(0x1C)), [1920]);
    assert_eq!(log.to(w.add(0x20)), [0x0437_077F]);
    assert_eq!(log.to(w.add(0x24)), [0x0437_077F]);
    let enable = log.at(w.add(0x10), WIN_EN);
    for off in [0x00, 0x04, 0x14, 0x1C, 0x20, 0x24, 0x28, 0x30, 0x34, 0xD0] {
        assert!(
            log.last(w.add(off)) < enable,
            "window reg {off:#x} after enable"
        );
    }
    // De VP0-timing en de post-config.
    let vp = b.vop.add(VP0);
    assert_eq!(log.to(vp.add(0x48)), [0x0898_002C]);
    assert_eq!(log.to(vp.add(0x4C)), [0x00C0_0840]);
    assert_eq!(log.to(vp.add(0x50)), [0x0465_0005]);
    assert_eq!(log.to(vp.add(0x54)), [0x0029_0461]);
    assert_eq!(log.to(vp.add(0x30)), [0x03E9_002C]);
    assert_eq!(log.to(vp.add(0x3C)), [0x1000_1000]);
    assert_eq!(log.to(b.vop.add(0x604)), [LAYER_SEL]);
    // Latchen ná alles, dan pas STANDBY los.
    let done = log.at(b.vop, CFG_DONE_GLB_EN | CFG_DONE_VP0);
    assert!(enable < done);
    for off in [0x2C, 0x30, 0x34, 0x38, 0x3C, 0x40, 0x48, 0x54] {
        assert!(
            log.last(vp.add(off)) < done,
            "vp reg {off:#x} after cfg_done"
        );
    }
    let run = log.at(vp, OUT_MODE_AAAA);
    assert!(done < run);
    assert_eq!(
        f.rd(b.vop.add(0x008)) & (1 << 31),
        0,
        "auto-gating still on"
    );
    assert_eq!(
        f.rd(b.vop.add(0x028)),
        0x3,
        "HDMI not routed, or DSP_IF_EN clobbered"
    );

    // HDMI pas ná de VOP2.
    let hdmi_clk = log.at(cru.add(0x354), 0x0018_0000);
    assert!(run < hdmi_clk);
    assert_eq!(log.to(b.grf.add(0x364)), [0xC000_C000]);
    // De frame composer, byte voor byte.
    for (r, v) in [
        (FC_INHACTV1, 0x07),
        (FC_INHACTV0, 0x80),
        (FC_INVACTV1, 0x04),
        (FC_INVACTV0, 0x38),
        (FC_INHBLANK1, 0x01),
        (FC_INHBLANK0, 0x18),
        (FC_INVBLANK, 45),
        (FC_HSYNCINDELAY1, 0),
        (FC_HSYNCINDELAY0, 88),
        (FC_VSYNCINDELAY, 4),
        (FC_HSYNCINWIDTH0, 44),
        (FC_VSYNCINWIDTH, 5),
    ] {
        assert_eq!(log.to(f.hd(r)), [v], "FC reg {r:#06x}");
    }
    // De PHY: de hele sequentie twee keer, in de volgorde van de driver.
    let twice: Vec<_> = PHY_148M5.iter().chain(PHY_148M5.iter()).copied().collect();
    assert_eq!(f.phy_writes(), twice);
    assert_eq!(f.rd(f.hd(PHY_CONF0)) as u8 & CONF0_SVSRET, CONF0_SVSRET);
    // Eerst alleen de pixelklok, dan TMDS erbij; FC_INVIDCONF opnieuw ná
    // de TMDS-softreset.
    assert_eq!(log.to(f.hd(MC_CLKDIS)), [0x7E, 0x7C]);
    assert_eq!(
        log.to(f.hd(FC_INVIDCONF)),
        [u32::from(INVIDCONF_DVI), u32::from(INVIDCONF_DVI)]
    );
    assert!(log.at(f.hd(MC_SWRSTZ), u32::from(MC_SWRSTZ_TMDS)) < log.last(f.hd(FC_INVIDCONF)));
    // De EDID kwam over 0x50, vóór de PHY aanging.
    assert_eq!(log.to(f.hd(I2CM_SLAVE)), [u32::from(DDC_EDID_ADDR)]);
    assert!(log.last(f.hd(I2CM_OPERATION)) < log.at(f.hd(PHY_I2CM_SLAVE), 0x69));
}

#[test]
fn a_power_domain_already_on_is_left_alone() {
    let f = Fake::new();
    dev::write32(f.b.pmu.add(0x98), 0);
    f.chain().power_on_vo().unwrap();
    let log = Log(journal::take());
    assert!(log.to(f.b.pmu.add(0xA0)).is_empty());
    assert_eq!(log.to(f.b.pmu.add(0x50)), [0x0010_0000]);
}

#[test]
fn no_sink_still_drives_1080p60() {
    let f = Fake::new();
    f.model(|m| m.sink = false);
    dev::write32(f.hd(PHY_STAT0), 0);
    let st = f.chain().start(&fb()).unwrap();
    assert!(!st.sink);
    assert_eq!(st.edid, Err(edid::Error::Nack { byte: 0 }));
    assert_eq!(f.phy_writes().len(), 2 * PHY_148M5.len());
}

#[test]
fn a_dead_power_domain_is_named_and_bounded() {
    let f = Fake::new();
    f.model(|m| m.pd_responds = false);
    let t0 = now();
    let e = f.chain().start(&fb()).unwrap_err();
    assert_eq!(
        e,
        Error::Settle {
            step: Step::PdPower,
            off: 0x98,
            got: 0x80,
            mask: 0x80,
            want: 0
        }
    );
    assert_eq!(e.layer(), Layer::Power);
    let s = format!("{e}");
    assert!(
        s.starts_with("power domain PD_VO: power") && s.contains("pmu+0x98"),
        "{s}"
    );
    // Begrensd: ~10 ms nep-tijd, en daarna niets meer aan de VOP2.
    assert!(now() - t0 < 20_000_000);
    let log = Log(journal::take());
    assert!(
        log.0
            .iter()
            .all(|&(a, _)| rel(a, f.b.vop, 0x4000).is_none())
    );
}

#[test]
fn an_hpll_that_never_locks_stops_before_norm() {
    let f = Fake::new();
    f.model(|m| m.hpll_locks = false);
    let e = f.chain().start(&fb()).unwrap_err();
    assert!(matches!(
        e,
        Error::Settle {
            step: Step::HpllLock,
            off: 0x44,
            ..
        }
    ));
    assert_eq!(e.layer(), Layer::Vop2);
    let log = Log(journal::take());
    assert_eq!(
        log.to(f.b.pmucru.add(0x80)),
        [0x000C_0000],
        "left SLOW, never NORM"
    );
    assert!(format!("{e}").starts_with("VOP2: HPLL lock"));
}

#[test]
fn a_silent_vop_is_refused() {
    let f = Fake::new();
    dev::write32(f.b.vop.add(0x004), 0xFFFF_FFFF);
    let e = f.chain().start(&fb()).unwrap_err();
    assert_eq!(e, Error::VopDead { version: u32::MAX });
    assert_eq!(e.layer(), Layer::Vop2);
}

#[test]
fn an_unlatched_vop_is_reported_not_fatal() {
    let f = Fake::new();
    f.model(|m| m.vop_latches = false);
    let st = f.chain().start(&fb()).unwrap();
    assert!(!st.latched);
}

#[test]
fn a_wrong_hdmi_id_stops_before_configuration() {
    let f = Fake::new();
    dev::write32(f.hd(PRODUCT_ID0), 0);
    let e = f.chain().start(&fb()).unwrap_err();
    assert_eq!(
        e,
        Error::HdmiId {
            prod0: 0,
            prod1: 0xC1
        }
    );
    assert_eq!(e.layer(), Layer::Hdmi);
    let log = Log(journal::take());
    assert!(log.to(f.hd(FC_INVIDCONF)).is_empty());
    assert!(log.to(f.hd(IH_MUTE)).is_empty());
}

#[test]
fn a_phy_i2c_without_done_names_the_register() {
    let f = Fake::new();
    f.model(|m| m.phy_i2c_answers = false);
    let e = f.chain().start(&fb()).unwrap_err();
    assert_eq!(
        e,
        Error::PhyI2c {
            reg: 0x06,
            round: 1
        }
    );
    assert!(format!("{e}").starts_with("HDMI-TX: PHY register 0x06"));
}

#[test]
fn a_phy_without_lock_is_a_settle_error() {
    let f = Fake::new();
    f.model(|m| m.phy_locks = false);
    let e = f.chain().start(&fb()).unwrap_err();
    assert!(matches!(
        e,
        Error::Settle {
            step: Step::PhyLock,
            off: 0xC010,
            ..
        }
    ));
    assert_eq!(e.layer(), Layer::Hdmi);
}

#[test]
fn svsret_only_on_three_phy_types() {
    let ids = |config2| HdmiIds {
        design: 0,
        rev: 0,
        prod0: 0xA0,
        prod1: 0x01,
        config2,
    };
    for (c, want) in [
        (0xB2, true),
        (0xC2, true),
        (0xF3, true),
        (0xE2, false),
        (0, false),
    ] {
        assert_eq!(ids(c).has_svsret(), want, "{c:#x}");
    }
    let f = Fake::new();
    dev::write32(f.hd(CONFIG2_ID), 0xE2);
    f.chain().start(&fb()).unwrap();
    assert_eq!(f.rd(f.hd(PHY_CONF0)) as u8 & CONF0_SVSRET, 0);
}

#[test]
fn hiword_puts_the_mask_sixteen_higher() {
    assert_eq!(hiword(0, 1, 7), 0x0080_0000);
    assert_eq!(hiword(1, 0x3, 2), 0x000C_0004);
    assert_eq!(hiword(99, 0xFFF, 0), 0x0FFF_0063);
    assert_eq!(VO_CON1_DDC_IN, 0xC000_C000);
}
