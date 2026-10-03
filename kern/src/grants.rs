//! Het `DeviceGrant`-primitief: een venster van een device in de kooi van
//! precies één app.
//!
//! Dit BEZIT de boekhouding van één grant: welk fysiek venster het board
//! aanbiedt en welk slot het vasthoudt. Het bezit GEEN beleid (wie mag het
//! vragen, welke env erbij hoort, wat de console doet als het glas weg is):
//! dat is van de aanbieder, vandaag `gui-fbgrant` voor de framebuffer. En
//! het bezit geen tabellen: het mappen gaat via [`WindowMap`], die op ijzer
//! `cpu::el2::stage2::grant_window` en `has_grant_window` is.
//!
//! Waarom een primitief in de kern en het beleid erbuiten (Go:
//! `kern/slots/grants.go`, 19-07): de kern draagt alleen de
//! lifecycle-haakjes ([`Grants`]), de gui-smaak linkt de aanbieder, en een
//! kale build geeft het glas nooit weg ([`NoGrants`]).
//!
//! Alleen voor apparaten ZONDER DMA (gui-ontwerp §7, categorie 1): een
//! lineaire framebuffer, later GPIO of I2C. Een DMA-master (xHCI, een NIC)
//! in een kooi is zonder IOMMU het hele geheugen; die blijft van HOP
//! (06-08, de USB-beslissing).
//!
//! # De haakjes in de lifecycle
//!
//! | Stap | Haak | Go |
//! | --- | --- | --- |
//! | Na de claim, vóór de env op de control-page gaat | [`Grants::env`] | `grantEnv` in `startGrant.prepare` |
//! | Na de kooibouw, vóór de dispatch | [`Grants::arm`] | `grantArm` in `armSlot` |
//! | Bij de adoptie na een kern-flip | [`Grants::adopt`] | `GrantHooks.Adopt` |
//! | Na een bevestigde stop, en bij een abort | [`Grants::release`] | `grantRelease` in `releaseSlot` |

use crate::{Error, Result, Slot};
use abi::layout::FB_IPA;
use alloc::vec::Vec;
use core::fmt::{self, Write as _};

/// De korrel van een stage-2-blok: het venster staat op [`FB_IPA`] plus de
/// offset van het device in zijn 2 MB-blok.
const BLOCK: u64 = 2 << 20;

/// Een fysiek venster van een device.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Window {
    /// Het fysieke beginadres.
    pub pa: u64,
    /// De maat in bytes.
    pub size: u64,
}

impl Window {
    /// Het IPA waarop de houder het venster ziet: [`FB_IPA`] plus de offset
    /// in het 2 MB-blok. De kooi-IPA-ruimte laat geen identiteitsmap toe:
    /// de `ramfb` van QEMU lag op 0x1_bc7a_0000, boven 4 GB, en de
    /// stage-2-fault (ESR 0x930c0044, FAR 0x1bc7a0000) was de vondst die het
    /// vaste IPA opleverde (19-07).
    #[must_use]
    pub fn ipa(&self) -> u64 {
        FB_IPA + (self.pa & (BLOCK - 1))
    }

    /// Een venster met een leeg of overlopend bereik is geen venster.
    #[must_use]
    pub fn is_valid(&self) -> bool {
        self.pa != 0 && self.size != 0 && self.pa.checked_add(self.size).is_some()
    }
}

/// Het mappen van een venster in een kooi: de tabellen zijn niet van dit
/// primitief.
pub trait WindowMap {
    /// Mapt `w` Normal-NC op het vaste venster-IPA in de kooi van `slot`.
    /// Aanroepen ná de kooibouw en vóór de dispatch.
    fn map(&mut self, slot: Slot, w: Window) -> Result;

    /// Toetst zonder te wijzigen of de kooi van `slot` precies `w` al mapt
    /// (de adoptie na een flip). Een afwijkende map is een fout, geen
    /// `false`: een bredere oude grant is geen bewijs van eigendom.
    fn is_mapped(&self, slot: Slot, w: Window) -> Result<bool>;
}

/// Eén grant: een venster en zijn houder. Eén houder tegelijk; de eerste
/// aanvrager wint (Go: `fbgrant.Env`), en hetzelfde slot opnieuw is geen
/// fout (een herstart van de houder).
///
/// De eigenaar is de taak die de lifecycle-haakjes draait: de
/// lifecycle-actor. Geen slot, geen atomics: `&mut self` op elke overgang.
#[derive(Debug)]
pub struct DeviceGrant {
    name: &'static str,
    window: Option<Window>,
    holder: Option<Slot>,
}

impl DeviceGrant {
    /// Een grant zonder venster (het board heeft het device niet).
    #[must_use]
    pub const fn new(name: &'static str) -> Self {
        DeviceGrant {
            name,
            window: None,
            holder: None,
        }
    }

    /// De naam, voor de logregels.
    #[must_use]
    pub fn name(&self) -> &'static str {
        self.name
    }

    /// Het board biedt een venster aan. Een ongeldig venster is geen
    /// venster; een nieuw venster onder een houder wordt geweigerd (het
    /// venster van een lopende app verschuift niet).
    pub fn offer(&mut self, w: Window) -> Result {
        if !w.is_valid() {
            return Err(Error::Range {
                base: w.pa,
                size: w.size,
            });
        }
        if let Some(h) = self.holder
            && self.window != Some(w)
        {
            return Err(Error::StillOwned { slot: h.get() });
        }
        self.window = Some(w);
        Ok(())
    }

    /// Het aangeboden venster.
    #[must_use]
    pub fn window(&self) -> Option<Window> {
        self.window
    }

    /// Het slot dat het venster vasthoudt.
    #[must_use]
    pub fn holder(&self) -> Option<Slot> {
        self.holder
    }

    /// Kent het venster exclusief aan `slot` toe.
    ///
    /// Geen venster is [`Error::NoEnt`] (de app draait dan headless, dat is
    /// geen fout van de app); een andere houder is [`Error::StillOwned`] met
    /// het slot van die houder.
    pub fn claim(&mut self, slot: Slot) -> Result<Window> {
        let w = self.window.ok_or(Error::NoEnt)?;
        match self.holder {
            Some(h) if h != slot => Err(Error::StillOwned { slot: h.get() }),
            _ => {
                self.holder = Some(slot);
                Ok(w)
            }
        }
    }

    /// Mapt het venster in de kooi van `slot` als dat de houder is; voor
    /// elk ander slot een no-op. Geeft of er gemapt is.
    pub fn arm(&self, slot: Slot, map: &mut impl WindowMap) -> Result<bool> {
        match (self.holder, self.window) {
            (Some(h), Some(w)) if h == slot => map.map(slot, w).map(|()| true),
            _ => Ok(false),
        }
    }

    /// Herstelt de houder uit een geërfde kooi (de kern-flip), nooit uit de
    /// env of uit wat een app beweert. Een tweede kooi die hetzelfde
    /// venster mapt, is een fout: dan hielden twee slots het glas vast.
    pub fn adopt(&mut self, slot: Slot, map: &impl WindowMap) -> Result<bool> {
        let Some(w) = self.window else {
            return Ok(false);
        };
        if !map.is_mapped(slot, w)? {
            return Ok(false);
        }
        if let Some(h) = self.holder
            && h != slot
        {
            return Err(Error::StillOwned { slot: h.get() });
        }
        self.holder = Some(slot);
        Ok(true)
    }

    /// Geeft het venster terug als `slot` de houder was. Aanroepen na een
    /// bevestigde stop (of een abort: er draaide nooit iets); de map zelf
    /// verdwijnt met de kooitabellen (`revoke_tables`). Geeft of er iets
    /// terugkwam.
    pub fn release(&mut self, slot: Slot) -> bool {
        if self.holder == Some(slot) {
            self.holder = None;
            return true;
        }
        false
    }
}

/// De lifecycle-haakjes van een grant-aanbieder. De binary implementeert
/// hem (de gui-smaak over `gui-fbgrant`, kaal [`NoGrants`]) en de
/// lifecycle roept hem op de vier plekken uit de moduledoc.
pub trait Grants {
    /// Na de claim: mag de env van `slot` aanvullen. `env` is de blob van de
    /// start (`key=val\n`); wat erbij moet, komt in `out` (leeg = niets
    /// erbij). Een weigering is geen fout van de start: de app draait dan
    /// headless, en de aanbieder logt waarom.
    fn env(&mut self, slot: Slot, env: &[u8], out: &mut Vec<u8>);

    /// Na de kooibouw, vóór de dispatch: mapt het venster bij de houder.
    fn arm(&mut self, slot: Slot) -> Result;

    /// Bij de adoptie: herstelt de houder uit de geërfde kooi.
    fn adopt(&mut self, slot: Slot) -> Result;

    /// Na een bevestigde stop of een abort.
    fn release(&mut self, slot: Slot);
}

/// De kale build: geen aanbieder, grants staan uit. Een job die toch om
/// het glas vraagt, krijgt een diagnose in plaats van stilte (Go:
/// `grantEnv` zonder provider).
#[derive(Debug, Default)]
pub struct NoGrants;

impl Grants for NoGrants {
    fn env(&mut self, _slot: Slot, _env: &[u8], _out: &mut Vec<u8>) {}
    fn arm(&mut self, _slot: Slot) -> Result {
        Ok(())
    }
    fn adopt(&mut self, _slot: Slot) -> Result {
        Ok(())
    }
    fn release(&mut self, _slot: Slot) {}
}

/// De waarde van `key` in een env-blob (`key=val\n`), of `None`.
#[must_use]
pub fn env_get<'a>(env: &'a [u8], key: &str) -> Option<&'a [u8]> {
    env.split(|&b| b == b'\n').find_map(|line| {
        let rest = line.strip_prefix(key.as_bytes())?;
        rest.strip_prefix(b"=")
    })
}

/// Zet `key=val\n` achter `out`, met een faalbare reservering (handboek §6)
/// en de plafondtoets van de control-page: samen met `base` mag de env niet
/// boven `max` bytes komen.
pub fn env_put(
    out: &mut Vec<u8>,
    base: usize,
    max: usize,
    key: &str,
    val: fmt::Arguments<'_>,
) -> Result {
    /// Een `fmt::Write` over een `Vec` die faalbaar groeit.
    struct Sink<'a>(&'a mut Vec<u8>);
    impl fmt::Write for Sink<'_> {
        fn write_str(&mut self, s: &str) -> fmt::Result {
            self.0.try_reserve(s.len()).map_err(|_| fmt::Error)?;
            self.0.extend_from_slice(s.as_bytes());
            Ok(())
        }
    }
    let start = out.len();
    let oom = Error::OutOfMemory { bytes: key.len() };
    let mut w = Sink(out);
    write!(w, "{key}=").map_err(|_| oom)?;
    w.write_fmt(val).map_err(|_| oom)?;
    w.write_str("\n").map_err(|_| oom)?;
    let len = base + out.len();
    if len > max {
        out.truncate(start);
        return Err(Error::TooLarge { len, max });
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    const FB_PA: u64 = 0x1_bc7a_0000; // de ramfb van 19-07, boven 4 GB

    fn s(i: usize) -> Slot {
        Slot::new(i).unwrap()
    }

    /// De kooien als een tabel van vensters; de tabellen zelf toetst
    /// `cpu::el2::stage2`.
    #[derive(Default)]
    struct Cages(HashMap<usize, Window>);

    impl WindowMap for Cages {
        fn map(&mut self, slot: Slot, w: Window) -> Result {
            self.0.insert(slot.get(), w);
            Ok(())
        }
        fn is_mapped(&self, slot: Slot, w: Window) -> Result<bool> {
            match self.0.get(&slot.get()) {
                None => Ok(false),
                Some(m) if *m == w => Ok(true),
                Some(_) => Err(Error::Range {
                    base: w.pa,
                    size: w.size,
                }),
            }
        }
    }

    fn map() -> Cages {
        Cages::default()
    }

    fn fb() -> Window {
        Window {
            pa: FB_PA,
            size: 1024 * 768 * 4,
        }
    }

    // Go: TestAdoptHolderIsExclusive (gui/fbgrant/adopt_test.go), het
    // primitief-deel: één houder, dezelfde mag opnieuw, een tweede niet.
    #[test]
    fn claim_is_exclusive() {
        let mut g = DeviceGrant::new("fb");
        assert_eq!(g.claim(s(1)), Err(Error::NoEnt), "no window, no grant");
        g.offer(fb()).unwrap();
        assert_eq!(g.claim(s(3)), Ok(fb()));
        assert_eq!(g.claim(s(3)), Ok(fb()), "same holder again");
        assert_eq!(g.claim(s(4)), Err(Error::StillOwned { slot: 3 }));
        assert_eq!(g.holder(), Some(s(3)));
        assert!(!g.release(s(4)), "not the holder");
        assert!(g.release(s(3)));
        assert_eq!(g.holder(), None);
        assert_eq!(g.claim(s(4)), Ok(fb()), "free again after release");
    }

    #[test]
    fn offer_refuses_bad_windows_and_a_moving_window() {
        let mut g = DeviceGrant::new("fb");
        assert!(g.offer(Window { pa: 0, size: 1 }).is_err());
        assert!(
            g.offer(Window {
                pa: u64::MAX,
                size: 2
            })
            .is_err()
        );
        g.offer(fb()).unwrap();
        g.claim(s(2)).unwrap();
        let moved = Window {
            pa: 0x3e00_0000,
            ..fb()
        };
        assert_eq!(g.offer(moved), Err(Error::StillOwned { slot: 2 }));
        g.offer(fb()).unwrap();
    }

    #[test]
    fn arm_maps_only_the_holder_and_adopt_reads_it_back() {
        let mut m = map();
        let mut g = DeviceGrant::new("fb");
        g.offer(fb()).unwrap();
        g.claim(s(2)).unwrap();
        assert_eq!(g.arm(s(1), &mut m), Ok(false), "not the holder");
        assert_eq!(g.arm(s(2), &mut m), Ok(true));
        assert_eq!(m.is_mapped(s(1), fb()), Ok(false));
        assert_eq!(m.is_mapped(s(2), fb()), Ok(true));
        // De flip: een verse grant leert de houder uit de kooi, niet uit de
        // env.
        let mut after = DeviceGrant::new("fb");
        after.offer(fb()).unwrap();
        assert_eq!(after.adopt(s(1), &m), Ok(false));
        assert_eq!(after.adopt(s(2), &m), Ok(true));
        assert_eq!(after.holder(), Some(s(2)));
        // Twee kooien met hetzelfde venster: fout, de eerste houder blijft.
        m.map(s(3), fb()).unwrap();
        assert_eq!(after.adopt(s(3), &m), Err(Error::StillOwned { slot: 2 }));
        assert_eq!(after.holder(), Some(s(2)));
    }

    #[test]
    fn the_window_ipa_keeps_the_offset_in_its_block() {
        assert_eq!(fb().ipa(), FB_IPA + (FB_PA & (BLOCK - 1)));
        let aligned = Window {
            pa: 0x3e00_0000,
            size: 8 << 20,
        };
        assert_eq!(aligned.ipa(), FB_IPA);
    }

    #[test]
    fn env_helpers() {
        let env = b"BUCKET=hop\nFB=1\nGUI=display\n";
        assert_eq!(env_get(env, "FB"), Some(&b"1"[..]));
        assert_eq!(env_get(env, "GUI"), Some(&b"display"[..]));
        assert_eq!(env_get(env, "FB_BASE"), None);
        assert_eq!(env_get(b"FBX=1\n", "FB"), None);
        let mut out = Vec::new();
        env_put(
            &mut out,
            env.len(),
            256,
            "FB_BASE",
            format_args!("{:#x}", 0x2000_0000),
        )
        .unwrap();
        env_put(
            &mut out,
            env.len(),
            256,
            "FB_WIDTH",
            format_args!("{}", 1024),
        )
        .unwrap();
        assert_eq!(out, b"FB_BASE=0x20000000\nFB_WIDTH=1024\n");
        let before = out.clone();
        assert_eq!(
            env_put(&mut out, 250, 256, "FB_HEIGHT", format_args!("{}", 768)),
            Err(Error::TooLarge {
                len: 250 + before.len() + 14,
                max: 256
            })
        );
        assert_eq!(out, before, "a refused key leaves nothing behind");
    }
}
