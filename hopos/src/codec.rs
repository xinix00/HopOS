//! Het media-vlak van de kern-binary: de codec-dienst van de system-API en
//! het meetinstrument `hopos.codecdemo` (Go: `OLD/metal/cmd/hopos/codec.go`,
//! `codec_off.go` en `codecdemo.go`).
//!
//! Dit is compute, geen beeld: de node decodeert een stream naar frames in
//! het geheugen van een app, en er komt geen scherm aan te pas. Alleen met
//! de feature `media`; zonder is [`up`] een stub en antwoorden de codec-ops
//! "onbekende op" (de kern kent ze dan niet).
//!
//! # Eigendom
//!
//! De dienst ([`kern::codecabi::CodecCell`]) staat in een `static` en bezit
//! de engine: de VPU-driver van `media-mve` met zijn arena, zijn sessies en
//! zijn firmware-cache. De system-API leent hem per call één synchrone
//! beurt; de opruimtaak hier leent hem eens per seconde om de sessies van
//! gestopte bewoners te sluiten.
//!
//! # Wat nog van andere sporen komt
//!
//! Twee haken zitten buiten dit bestand en staan in `docs/media.md`: een
//! arena buiten de partitie-pool (de pool-reservering van Go's
//! `ReserveDevice`), en een kern-lezing van hopfs voor de firmware-blobs
//! (`/firmware/<naam>.fwb`). Zolang ze er niet zijn, meldt deze module luid
//! welke ontbreekt en blijft de codec uit. De derde, de partitie van een
//! levende bewoner, is er: `Servicers::partition` (kern/src/slots.rs).

#[cfg(feature = "media")]
mod on {
    use crate::KernConsole;
    use alloc::vec::Vec;
    use bounded::BoundedVec;
    use cpu::println;
    use driver_codec::Firmware;
    use executor::Executor;
    use kern::codecabi::{CodecCell, Coherence, Lives};
    use kern::{Region, Slot};
    use media_mve::Device;

    /// De engine van deze binary: de Linlon V8 met de firmware-cache.
    pub(crate) type Vpu = Device<Blobs>;

    /// De codec-dienst van de node.
    pub(crate) static CODEC: CodecCell<Vpu, NodeLives, DevCache, KernConsole> =
        CodecCell::new(NodeLives, DevCache, KernConsole);

    /// Hoeveel firmware-blobs de cache houdt: de O6N levert er zestien (elf
    /// decoders, vijf encoders).
    pub(crate) const MAX_BLOBS: usize = 16;

    /// De grootste blob die we aannemen; de echte zijn zo'n 300 KB.
    pub(crate) const MAX_BLOB: usize = 4 << 20;

    /// De levende bewoners en hun partities, uit de servicer-tabel.
    pub(crate) struct NodeLives;

    impl Lives for NodeLives {
        fn current(&self, slot: Slot) -> Option<u32> {
            crate::SERVICERS.current(slot)
        }

        fn partition(&self, slot: Slot) -> Option<Region> {
            // De basis en maat die de lifecycle-actor bij de start in de
            // servicer-besturing zette; `None` zodra de servicer weg is, en
            // dan weigert de grant met STATUS_DENIED "slot is being
            // released": veilig dicht.
            crate::SERVICERS.partition(slot)
        }
    }

    /// Het cache-onderhoud via `dev`: `dc cvac` en `dc civac`.
    pub(crate) struct DevCache;

    impl Coherence for DevCache {
        fn clean(&mut self, pa: u64, len: u64) {
            dev::push(dev::Pa(pa), usize::try_from(len).unwrap_or(0));
        }
        fn clean_inv(&mut self, pa: u64, len: u64) {
            dev::pull(dev::Pa(pa), usize::try_from(len).unwrap_or(0));
        }
    }

    /// De firmware in RAM: één keer bij `codec_up` van het volume gelezen,
    /// daarna per sessie uit de cache (de driver kent geen bestandssysteem).
    /// Go las elke open opnieuw van de NVMe; hier kan dat niet, want een
    /// open is één synchrone beurt, en een kern-flip leest ze toch opnieuw.
    #[derive(Default)]
    pub(crate) struct Blobs {
        blobs: BoundedVec<(&'static str, Vec<u8>), MAX_BLOBS>,
    }

    impl Blobs {
        /// Legt een blob in de cache (de lezer van het volume); vol of te
        /// groot wordt geweigerd.
        #[expect(dead_code, reason = "de lezer van het volume komt met de hopfs-haak")]
        pub(crate) fn put(&mut self, name: &'static str, bin: Vec<u8>) -> bool {
            bin.len() <= MAX_BLOB && self.blobs.push((name, bin)).is_ok()
        }

        /// Hoeveel blobs de cache houdt.
        #[cfg_attr(
            not(feature = "board-o6n"),
            expect(dead_code, reason = "alleen de O6N laadt firmware")
        )]
        pub(crate) fn len(&self) -> usize {
            self.blobs.len()
        }
    }

    impl Firmware for Blobs {
        fn load(&mut self, name: &str) -> Option<&[u8]> {
            self.blobs
                .iter()
                .find(|(n, _)| *n == name)
                .map(|(_, b)| b.as_slice())
        }
    }

    /// Zet de dienst op: altijd aangemeld bij de system-API (zonder ijzer
    /// weigert hij luid), en op een board met een VPU het ijzer erin.
    pub(crate) fn up(exec: &'static Executor) {
        if !kern::codecabi::install(&CODEC) {
            println!("codec: service installed twice HOPOS_CODEC_FAIL");
            return;
        }
        board_up(exec);
    }

    #[cfg(not(feature = "board-o6n"))]
    fn board_up(_exec: &'static Executor) {
        println!("codec: no video codec on this board, codec calls refused HOPOS_CODEC_NONE");
    }

    #[cfg(feature = "board-o6n")]
    use vpu::board_up;

    /// De VPU van de O6N: arena, stroom, driver, opruimtaak.
    #[cfg(feature = "board-o6n")]
    mod vpu {
        use super::{Blobs, CODEC, Vpu};
        use core::time::Duration;
        use cpu::println;
        use driver_codec::{Engine, Graveyard, Show};
        use executor::Executor;
        use kern::Region;
        use media_mve::Device;

        /// De bak van de gevallen sessie-handvatten van de VPU.
        static GRAVES: Graveyard = Graveyard::new();

        /// De arena als niets anders gezegd wordt. Een 4K HEVC-decoder
        /// houdt zestien referentieframes vast; in 10 bit is dat 24 MB per
        /// frame. Met 256 MB liep de arena leeg en eindigde dat in een MMU
        /// ABORT (22-09); 768 MB draagt één 4K10-stream met werkruimte en is
        /// op 16 GB nog geen twintigste. Alleen 8 bit: `hopos.codec=256`.
        pub(crate) const DEFAULT_ARENA_MB: u64 = 768;

        /// Hoe vaak de opruimtaak de sessies van gestopte bewoners sluit.
        const REAP_EVERY: Duration = Duration::from_secs(1);

        /// Het meetinstrument en de opruimtaak, als er een engine is.
        fn spawn_tasks(exec: &'static Executor) {
            let reaper = async move {
                loop {
                    exec.after(REAP_EVERY).await;
                    let _ = CODEC.with(|s, _| s.reap());
                }
            };
            if exec.spawn(reaper).is_err() {
                println!(
                    "codec: reaper not spawned, sessions of stopped tasks stay open HOPOS_CODEC_FAIL"
                );
            }
        }

        /// De O6N: arena, stroom, driver.
        pub(super) fn board_up(exec: &'static Executor) {
            let board = &crate::BOARD;
            let param = |k| fw::bootcfg::first(fw::bootcfg::all(board.config(), k));
            let mb = match param("hopos.codec") {
                "" => DEFAULT_ARENA_MB,
                v => match v.parse::<u64>() {
                    Ok(n) => n,
                    Err(_) => {
                        println!("codec: ignoring hopos.codec={v:?}");
                        DEFAULT_ARENA_MB
                    }
                },
            };
            if mb == 0 {
                println!("codec: off (hopos.codec=0), codec calls refused HOPOS_CODEC_OFF");
                return;
            }
            let Some(arena) = arena(mb << 20) else {
                println!(
                    "codec: no {mb} MB arena outside the partition pool (the pool has no device reservation yet), video codec stays off HOPOS_CODEC_OFF"
                );
                return;
            };
            let blobs = firmware();
            if blobs.len() == 0 {
                println!(
                    "codec: no firmware blobs read from /firmware, every open will be refused HOPOS_CODEC_NOFW"
                );
            }
            let w = match board.power_vpu(arena.base, arena.size) {
                Ok(w) => w,
                Err(e) => {
                    println!("codec: {e}, video codec stays off HOPOS_CODEC_OFF");
                    return;
                }
            };
            let a = match media_mve::Arena::new(arena.base, arena.size) {
                Ok(a) => a,
                Err(e) => {
                    println!("codec: {e}, video codec stays off HOPOS_CODEC_OFF");
                    return;
                }
            };
            // SAFETY: `w.base` komt uit de DSDT van dit board (of de SoC-constante)
            // en is door `power_vpu` gemapt, van stroom, klok en reset voorzien;
            // alleen deze driver raakt het blok aan. De arena ligt buiten elke
            // partitie en is door `power_vpu` ongecached gemapt (`_CCA = 0`).
            let probed = unsafe { Device::probe(w.base, a, blobs, &GRAVES, cpu::idle::now) };
            let dev = match probed {
                Ok(d) => d,
                Err(e) => {
                    println!("codec: {e}, video codec stays off HOPOS_CODEC_OFF");
                    return;
                }
            };
            println!(
                "codec: {} HOPOS_CODEC_UP",
                Show(&dev, |d: &Vpu, f| d.describe(f))
            );
            if CODEC.with(|s, _| s.install(dev)).is_none() {
                println!("codec: service busy at boot HOPOS_CODEC_FAIL");
                return;
            }
            spawn_tasks(exec);
        }

        /// De arena van de VPU: fysiek geheugen buiten de partitie-pool.
        fn arena(_size: u64) -> Option<Region> {
            // TODO: de pool-reservering (Go: `slots.ReserveDevice`), het
            // slot-spoor: een blok van `size` bytes dat de lifecycle-actor uit
            // de pool haalt en nooit aan een partitie geeft.
            None
        }

        /// De firmware van het volume (`/firmware/<naam>.fwb`).
        fn firmware() -> Blobs {
            // TODO: een kern-lezing van hopfs (kern/src/rpc.rs): de actor leest
            // voor de kern zelf, zonder slot en generatie. Zolang die er niet
            // is, blijft de cache leeg en weigert elke open met NoFirmware.
            Blobs::default()
        }
    }
}

#[cfg(feature = "media")]
pub(crate) use on::up;

#[cfg(not(feature = "media"))]
mod off {
    use executor::Executor;

    /// Buiten de media-smaak: geen VPU-driver, geen dienst. De codec-ops
    /// antwoorden "onbekende op", zoals op een board zonder codec-ijzer.
    pub(crate) fn up(_exec: &'static Executor) {}
}

#[cfg(not(feature = "media"))]
pub(crate) use off::up;
