//! Het beleid van de framebuffer-grant: de kleinste `DeviceGrant`
//! (gui-ontwerp §7 en §8). HOP mapt de framebuffer in de kooi van één app,
//! de display-app, zodat de compositie die headless via `/screen.png` loopt
//! ook op echt glas landt. Een lineaire pixelbuffer: geen registers, geen
//! DMA, geen interrupts, en daarom mag hij in een kooi.
//!
//! Dit BEZIT het beleid: wie het glas mag vragen, welke env erbij hoort, en
//! dat HOP's eigen console van het glas gaat zolang de grant leeft en
//! terugkomt als het slot vrijkomt (een gecrashte display-app geeft je zo
//! vanzelf de bootconsole terug). Het primitief eronder (één houder, het
//! venster in de kooi, de adoptie na een flip) is `kern::grants`; de console
//! zelf is van de binary en komt binnen als [`Glass`].
//!
//! De aanvraag: de jobspec zegt `gui: display`, en Hop zet dat als
//! `GUI=display` in de env van de job. `FB=1` (de Go-vorm, 19-07) werkt
//! ook. De jobspec is van Hop; zolang er geen publisher-signing is, is Hop
//! de enige bron van jobs, dus dit is geen escalatiepad (herzien bij
//! signing). Eén houder tegelijk; de eerste aanvrager wint.
//!
//! Het glas, het toetsenbord en de muis zijn één zitplaats (Derek, 06-08):
//! wie het scherm krijgt, krijgt `INPUT_ADDR` erbij, het adres waar HOP de
//! USB-invoer als JSON-regels uitserveert (`gui-usbin`). Geen werkende USB
//! op dit board: dan staat er ook geen adres in de env.

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
#![forbid(unsafe_code)]

extern crate alloc;

use alloc::vec::Vec;
use core::net::Ipv4Addr;
use driver_fb::Desc;
use kern::cage::Console;
use kern::grants::{DeviceGrant, Window, WindowMap, env_get, env_put};
use kern::{Error, Result, Slot};

/// De console van HOP op het glas, zoals de grant hem ziet.
pub trait Glass {
    /// Het glas is van de app: de console eraf (hij tekent niets meer).
    fn hand_over(&mut self);
    /// Het glas is terug: de console erop, met een schone lei.
    fn take_back(&mut self, d: Desc);
}

/// Het plafond van de env-blob op de control-page.
const ENV_MAX: usize = abi::hopabi::CTRL_ENV_MAX as usize;

/// Vraagt deze env om het glas? `GUI=display` (de jobspec) of `FB=1` (de
/// Go-vorm).
#[must_use]
pub fn wants_glass(env: &[u8]) -> bool {
    env_get(env, "GUI") == Some(b"display") || env_get(env, "FB") == Some(b"1")
}

/// De framebuffer-grant van één node.
///
/// De eigenaar is de lifecycle-actor (via de `kern::grants::Grants`-impl
/// van de binary): elke overgang is `&mut self`, er is geen slot.
#[derive(Debug)]
pub struct FbGrant {
    grant: DeviceGrant,
    desc: Option<Desc>,
    input: Option<(Ipv4Addr, u16)>,
}

impl Default for FbGrant {
    fn default() -> Self {
        Self::new()
    }
}

impl FbGrant {
    /// Een grant zonder framebuffer: elke aanvraag draait headless.
    #[must_use]
    pub const fn new() -> Self {
        FbGrant {
            grant: DeviceGrant::new("fb"),
            desc: None,
            input: None,
        }
    }

    /// Het board heeft een framebuffer. Een descriptor die de console niet
    /// zou nemen, neemt de grant ook niet.
    pub fn offer(&mut self, d: Desc) -> Result {
        let size = d.size().filter(|_| d.check().is_ok());
        let size = size.ok_or(Error::Range {
            base: d.base.0,
            size: d.size().unwrap_or(0),
        })?;
        self.grant.offer(Window { pa: d.base.0, size })?;
        self.desc = Some(d);
        Ok(())
    }

    /// Vertelt de grant waar HOP de invoerstroom serveert (`gui-usbin`).
    /// Nooit gezet = dit board heeft geen werkende USB, en dan komt er ook
    /// geen `INPUT_ADDR` in de env.
    pub fn use_input(&mut self, ip: Ipv4Addr, port: u16) {
        self.input = Some((ip, port));
    }

    /// Het slot dat het glas vasthoudt. Dat is per definitie de display-app,
    /// en dus ook de enige die de invoer mag lezen.
    #[must_use]
    pub fn holder(&self) -> Option<Slot> {
        self.grant.holder()
    }

    /// De framebuffer die de grant aanbiedt.
    #[must_use]
    pub fn desc(&self) -> Option<Desc> {
        self.desc
    }

    /// De env-haak na de claim: kent bij een aanvraag het glas exclusief
    /// aan `slot` toe en zet de `FB_*`-beschrijving in `out` (het contract
    /// van de display-app, `cmd/display/fbblit.go` in hop-os-surf). Zonder
    /// framebuffer of bij een andere houder blijft `out` leeg en draait de
    /// app headless; dat is geen fout, wel een regel.
    pub fn env(
        &mut self,
        slot: Slot,
        env: &[u8],
        out: &mut Vec<u8>,
        glass: &mut impl Glass,
        log: &impl Console,
    ) {
        if !wants_glass(env) {
            return;
        }
        let Some(d) = self.desc else {
            log.log(format_args!(
                "slot {slot}: fb grant requested, board has no framebuffer HOPOS_FB_NONE"
            ));
            return;
        };
        let w = match self.grant.claim(slot) {
            Ok(w) => w,
            Err(Error::StillOwned { slot: h }) => {
                log.log(format_args!(
                    "slot {slot}: fb grant requested, already held by slot {h} HOPOS_FB_BUSY"
                ));
                return;
            }
            Err(e) => {
                log.log(format_args!(
                    "slot {slot}: fb grant refused: {e} HOPOS_FB_NONE"
                ));
                return;
            }
        };
        if let Err(e) = put_env(out, env.len(), &d, w, self.input) {
            // Geen halve beschrijving: de app krijgt alles of niets, en het
            // glas blijft bij HOP.
            out.clear();
            self.grant.release(slot);
            log.log(format_args!(
                "slot {slot}: fb grant refused: env {e} HOPOS_FB_ENV"
            ));
            return;
        }
        glass.hand_over();
        log.log(format_args!(
            "slot {slot}: fb granted {}x{} stride {} @ {:#x} ipa {:#x} HOPOS_FB_GRANT",
            d.width,
            d.height,
            d.stride,
            d.base.0,
            w.ipa()
        ));
    }

    /// De arm-haak: mapt het venster (ná de kooibouw) in de kooi van de
    /// houder. Voor elk ander slot een no-op.
    pub fn arm(&self, slot: Slot, map: &mut impl WindowMap) -> Result<bool> {
        self.grant.arm(slot, map)
    }

    /// De adoptie na een kern-flip: de houder komt alleen uit de geërfde
    /// kooi, nooit uit de env of uit wat een herverbindende app beweert.
    pub fn adopt(&mut self, slot: Slot, map: &impl WindowMap, glass: &mut impl Glass) -> Result {
        if self.grant.adopt(slot, map)? {
            glass.hand_over();
        }
        Ok(())
    }

    /// De release-haak: geeft het glas terug bij het vrijkomen van het slot
    /// en zet HOP's console terug (verse init: schone lei, de log loopt
    /// weer). Geeft of er iets terugkwam.
    pub fn release(&mut self, slot: Slot, glass: &mut impl Glass, log: &impl Console) -> bool {
        if !self.grant.release(slot) {
            return false;
        }
        if let Some(d) = self.desc {
            glass.take_back(d);
        }
        log.log(format_args!(
            "slot {slot}: fb grant released, console back on glass HOPOS_FB_RELEASE"
        ));
        true
    }
}

/// Zet de `FB_*`-sleutels (en `INPUT_ADDR`) achter `out`.
fn put_env(
    out: &mut Vec<u8>,
    base: usize,
    d: &Desc,
    w: Window,
    input: Option<(Ipv4Addr, u16)>,
) -> Result {
    // De app ziet het venster op het vaste IPA plus de offset in zijn
    // 2 MB-blok: de fysieke buffer mag boven de 4 GB liggen.
    env_put(
        out,
        base,
        ENV_MAX,
        "FB_BASE",
        format_args!("{:#x}", w.ipa()),
    )?;
    env_put(out, base, ENV_MAX, "FB_WIDTH", format_args!("{}", d.width))?;
    env_put(
        out,
        base,
        ENV_MAX,
        "FB_HEIGHT",
        format_args!("{}", d.height),
    )?;
    env_put(
        out,
        base,
        ENV_MAX,
        "FB_STRIDE",
        format_args!("{}", d.stride),
    )?;
    env_put(out, base, ENV_MAX, "FB_BPP", format_args!("{}", d.bpp))?;
    if d.swap_rb {
        env_put(out, base, ENV_MAX, "FB_SWAP", format_args!("1"))?;
    }
    if let Some((ip, port)) = input {
        env_put(
            out,
            base,
            ENV_MAX,
            "INPUT_ADDR",
            format_args!("{ip}:{port}"),
        )?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use core::cell::RefCell;
    use dev::Pa;
    use kern::stage2::Stage2;
    use std::collections::HashMap;
    use std::string::String;

    /// Een console die zijn staat onthoudt.
    #[derive(Default)]
    struct TestGlass {
        on: bool,
        back: u32,
    }

    impl Glass for TestGlass {
        fn hand_over(&mut self) {
            self.on = false;
        }
        fn take_back(&mut self, _d: Desc) {
            self.on = true;
            self.back += 1;
        }
    }

    #[derive(Default)]
    struct Log(RefCell<String>);

    impl Console for Log {
        fn log(&self, args: core::fmt::Arguments<'_>) {
            use core::fmt::Write as _;
            let _ = writeln!(self.0.borrow_mut(), "{args}");
        }
    }

    #[derive(Default)]
    struct Mem(HashMap<u64, u64>);

    impl kern::cage::PhysMem for Mem {
        fn read64(&self, pa: u64) -> u64 {
            self.0.get(&pa).copied().unwrap_or(0)
        }
        fn write64(&mut self, pa: u64, v: u64) {
            self.0.insert(pa, v);
        }
        fn clear(&mut self, pa: u64, len: u64) {
            self.0.retain(|a, _| *a < pa || *a >= pa + len);
        }
        fn clean_inv(&mut self, _: u64, _: u64) {}
    }

    fn desc() -> Desc {
        Desc {
            base: Pa(0x1_bc7a_0000),
            width: 1024,
            height: 768,
            stride: 4096,
            bpp: 32,
            swap_rb: false,
        }
    }

    fn s(i: usize) -> Slot {
        Slot::new(i).unwrap()
    }

    #[test]
    fn a_display_job_gets_the_glass_and_the_env() {
        let mut g = FbGrant::new();
        g.offer(desc()).unwrap();
        g.use_input(Ipv4Addr::new(10, 100, 0, 1), 7879);
        let (mut glass, log) = (TestGlass { on: true, back: 0 }, Log::default());
        let mut out = Vec::new();
        g.env(s(3), b"GUI=display\n", &mut out, &mut glass, &log);
        let env = String::from_utf8(out).unwrap();
        assert_eq!(
            env,
            "FB_BASE=0x201a0000\nFB_WIDTH=1024\nFB_HEIGHT=768\nFB_STRIDE=4096\nFB_BPP=32\nINPUT_ADDR=10.100.0.1:7879\n"
        );
        assert!(!glass.on, "the console left the glass");
        assert_eq!(g.holder(), Some(s(3)));
        assert!(log.0.borrow().contains("HOPOS_FB_GRANT"));
    }

    #[test]
    fn no_request_no_framebuffer_or_a_second_display_stays_headless() {
        let (mut glass, log) = (TestGlass { on: true, back: 0 }, Log::default());
        let mut out = Vec::new();
        // Geen framebuffer.
        let mut g = FbGrant::new();
        g.env(s(1), b"FB=1\n", &mut out, &mut glass, &log);
        assert!(out.is_empty() && glass.on);
        assert!(log.0.borrow().contains("HOPOS_FB_NONE"));
        // Geen aanvraag.
        g.offer(desc()).unwrap();
        g.env(s(1), b"BUCKET=x\nFB=0\n", &mut out, &mut glass, &log);
        assert!(out.is_empty() && glass.on && g.holder().is_none());
        // De eerste wint; de tweede draait headless.
        g.env(s(1), b"FB=1\n", &mut out, &mut glass, &log);
        assert!(!out.is_empty());
        out.clear();
        g.env(s(2), b"GUI=display\n", &mut out, &mut glass, &log);
        assert!(out.is_empty());
        assert!(
            log.0
                .borrow()
                .contains("already held by slot 1 HOPOS_FB_BUSY")
        );
        assert_eq!(g.holder(), Some(s(1)));
    }

    #[test]
    fn release_gives_the_console_back_only_for_the_holder() {
        let mut g = FbGrant::new();
        g.offer(Desc {
            swap_rb: true,
            ..desc()
        })
        .unwrap();
        let (mut glass, log) = (TestGlass { on: true, back: 0 }, Log::default());
        let mut out = Vec::new();
        g.env(s(2), b"FB=1\n", &mut out, &mut glass, &log);
        assert!(String::from_utf8_lossy(&out).contains("FB_SWAP=1\n"));
        assert!(!g.release(s(3), &mut glass, &log));
        assert!(!glass.on);
        assert!(g.release(s(2), &mut glass, &log));
        assert!(glass.on && glass.back == 1);
        assert!(g.holder().is_none());
        assert!(log.0.borrow().contains("HOPOS_FB_RELEASE"));
    }

    // Go: TestAdoptHolderIsExclusive (adopt_test.go): de houder komt uit de
    // geërfde kooi, de console gaat van het glas, en een tweede kooi met
    // hetzelfde venster vervangt de houder niet.
    #[test]
    fn adopt_holder_is_exclusive() {
        let mut map = (
            Stage2 {
                cage_pa: 0x4000_0000,
                max_slots: 8,
            },
            Mem::default(),
        );
        let mut before = FbGrant::new();
        before.offer(desc()).unwrap();
        let (mut glass, log) = (TestGlass { on: true, back: 0 }, Log::default());
        let mut out = Vec::new();
        before.env(s(3), b"FB=1\n", &mut out, &mut glass, &log);
        assert_eq!(before.arm(s(3), &mut map), Ok(true));
        assert_eq!(before.arm(s(4), &mut map), Ok(false));

        let mut after = FbGrant::new();
        after.offer(desc()).unwrap();
        let mut glass = TestGlass { on: true, back: 0 };
        after.adopt(s(4), &map, &mut glass).unwrap();
        assert!(after.holder().is_none() && glass.on, "slot 4 maps nothing");
        after.adopt(s(3), &map, &mut glass).unwrap();
        assert_eq!(after.holder(), Some(s(3)));
        assert!(!glass.on, "ownership restored, console off the glass");
        after.adopt(s(3), &map, &mut glass).unwrap();
        // Een tweede kooi die hetzelfde venster mapt.
        map.map(s(4), before.grant.window().unwrap()).unwrap();
        assert_eq!(
            after.adopt(s(4), &map, &mut glass),
            Err(Error::StillOwned { slot: 3 })
        );
        assert_eq!(after.holder(), Some(s(3)), "duplicate replaced owner");
    }

    #[test]
    fn an_undrawable_framebuffer_is_not_offered() {
        let mut g = FbGrant::new();
        assert!(g.offer(Desc { bpp: 24, ..desc() }).is_err());
        assert!(g.offer(Desc::default()).is_err());
        assert!(g.desc().is_none());
    }
}
