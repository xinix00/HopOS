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
//! gestopte bewoners te sluiten, en het meetinstrument per stap één beurt.
//!
//! # De bring-up
//!
//! [`up`] meldt de dienst aan en spawnt één taak die de drie haken van
//! `docs/media.md` na elkaar loopt, elk als bericht aan zijn eigenaar:
//!
//! 1. De arena: een blok buiten de partitie-pool (`hopos.codec=<MB>`, 0 is
//!    uit) van de lifecycle-actor (`Request::ReserveDevice`, de regel
//!    `HOPOS_POOL_DEVICE`). Op een board zonder VPU gaat hij na
//!    `HOPOS_CODEC_NONE` meteen terug; zo bewijst QEMU de weg.
//! 2. De firmware: de zestien blobs van het volume, door de hopfs-actor voor
//!    de kern gelezen (`kern::rpc::read_file`), ná de mount en vóór de VPU.
//!    Wat mist, staat bij naam in `HOPOS_CODEC_NOFW`.
//! 3. Het ijzer: stroom, klokken, reset, de driver, `HOPOS_CODEC_UP`; faalt
//!    dat, dan gaat de arena terug (Go: `ReleaseDevice`).
//!
//! De taak draait op de executor, dus pas als de actoren draaien: de
//! lifecycle-actor handelt de reservering af ná een eventuele adoptie
//! (kern-flip), en vóór de eerste plaatsing die daarna in de rij staat.

#[cfg(feature = "media")]
mod on {
    use crate::KernConsole;
    use alloc::string::String;
    use alloc::vec::Vec;
    use bounded::BoundedVec;
    use core::time::Duration;
    use cpu::println;
    use driver_codec::Firmware;
    use executor::Executor;
    use kern::codecabi::{CodecCell, Coherence, Lives};
    use kern::slots::{Reply, Request, Response};
    use kern::{Region, Slot};
    use media_mve::Device;
    use sync::{Either, select};

    /// De engine van deze binary: de Linlon V8 met de firmware-cache.
    pub(crate) type Vpu = Device<Blobs>;

    /// De codec-dienst van de node.
    pub(crate) static CODEC: CodecCell<Vpu, NodeLives, DevCache, KernConsole> =
        CodecCell::new(NodeLives, DevCache, KernConsole);

    /// Hoeveel firmware-blobs de cache houdt: de O6N levert er zestien (elf
    /// decoders, vijf encoders).
    pub(crate) const MAX_BLOBS: usize = media_mve::FIRMWARE.len();

    /// De grootste blob die we aannemen; de echte zijn zo'n 300 KB (Go: de
    /// grens van `codecFirmware.Load`).
    pub(crate) const MAX_BLOB: usize = 4 << 20;

    /// De antwoordplek van de bring-up en het meetinstrument, bij de
    /// lifecycle-actor en bij de hopfs-actor. Eén aanroeper tegelijk: de
    /// bring-up spawnt het meetinstrument pas als hij zelf klaar is.
    static REPLY: Reply = Reply::new();

    /// Hoe lang de bring-up op de eigenaar van de pool wacht. Op een board
    /// waar de slots niet opkomen, leest niemand de brievenbus; dan liever
    /// een luide regel dan een taak die voor altijd hangt.
    const POOL_WAIT: Duration = Duration::from_secs(10);

    /// Hoe lang één lezing van de hopfs-actor mag duren (een blob van 300
    /// KB is een paar milliseconden van de NVMe).
    #[cfg(feature = "board-o6n")]
    pub(crate) const FS_WAIT: Duration = Duration::from_secs(5);

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

    /// De firmware in RAM: één keer bij de bring-up van het volume gelezen,
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
        #[cfg_attr(
            not(feature = "board-o6n"),
            expect(dead_code, reason = "alleen de O6N laadt firmware")
        )]
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
    /// weigert hij luid), en de bring-up als taak (arena, firmware, ijzer).
    pub(crate) fn up(exec: &'static Executor) {
        if !kern::codecabi::install(&CODEC) {
            println!("codec: service installed twice HOPOS_CODEC_FAIL");
            return;
        }
        if exec.spawn(bring_up(exec)).is_err() {
            println!("codec: bring-up not spawned, video codec stays off HOPOS_CODEC_FAIL");
        }
    }

    /// Eén bootparameter, of "" (de bron van het board: FDT-bootargs op
    /// virt, `hopos.cfg` op de UEFI-boards).
    pub(crate) fn param(key: &'static str) -> String {
        crate::bench::bootparam(0, key)
    }

    /// De maat van de arena: `hopos.codec=<MB>`, of die van het board.
    fn arena_mb() -> u64 {
        match param("hopos.codec").as_str() {
            "" => hw::DEFAULT_ARENA_MB,
            v => match v.parse::<u64>() {
                Ok(n) => n,
                Err(_) => {
                    println!(
                        "codec: ignoring hopos.codec={v:?}, using {} MB",
                        hw::DEFAULT_ARENA_MB
                    );
                    hw::DEFAULT_ARENA_MB
                }
            },
        }
    }

    /// Het pad van het meetinstrument (`hopos.codecdemo=1` is de standaard
    /// clip, een pad is dat bestand op het volume), of `None`.
    fn demo_path() -> Option<String> {
        match param("hopos.codecdemo").as_str() {
            "" | "0" => None,
            "1" => Some(String::from(DEMO_CLIP)),
            p => Some(String::from(p)),
        }
    }

    /// Waar een meetbundel zijn teststream neerzet (Go: `codecClipPath`).
    const DEMO_CLIP: &str = "/data/clip.hevc";

    /// De buffers van het meetinstrument: invoer en beelden samen (4K P010
    /// is 25 MB per beeld; Go: `codecDemoArenaMB`). Van de kern zolang de
    /// node draait: een tweede meting in dezelfde boot wil hetzelfde blok.
    const DEMO_ARENA_MB: u64 = 320;

    /// Wat de bring-up straks uit de partitie-pool haalt (de arena plus de
    /// demo-buffers), in bytes: de plaatsing van Hop trekt dat af van de
    /// poolgrootte die hij aan Hop meldt, anders overschat Hop de node met
    /// de arena (768 MB op de O6N) en plaatst hij een job die niet past.
    pub(crate) fn planned_bytes() -> u64 {
        let mb = arena_mb();
        if mb == 0 {
            return 0;
        }
        let demo_mb = if demo_path().is_some() {
            DEMO_ARENA_MB
        } else {
            0
        };
        (mb + demo_mb) << 20
    }

    /// De bring-up: arena, dan het ijzer van het board; wat niet opkomt,
    /// geeft zijn blok terug.
    async fn bring_up(exec: &'static Executor) {
        let mb = arena_mb();
        if mb == 0 {
            println!("codec: off (hopos.codec=0), codec calls refused HOPOS_CODEC_OFF");
            return;
        }
        let demo = demo_path();
        let demo_mb = if demo.is_some() { DEMO_ARENA_MB } else { 0 };
        let what = if demo.is_some() {
            "the codec arena and the codecdemo buffers"
        } else {
            "the codec arena"
        };
        // Een onzinnig getal in de config is een weigering, geen omloop.
        let size = mb.checked_add(demo_mb).and_then(|m| m.checked_mul(1 << 20));
        let block = match size {
            Some(size) => reserve(exec, size, what).await,
            None => None,
        };
        let Some(block) = block else {
            println!(
                "codec: no {mb} MB arena outside the partition pool, video codec stays off HOPOS_CODEC_OFF"
            );
            return;
        };
        // Eén reservering, één keer bij boot; de arena vooraan, de buffers
        // van het meetinstrument erachter.
        let arena = Region::new(block.base, mb.saturating_mul(1 << 20).min(block.size));
        let rest = Region::new(arena.base + arena.size, block.size - arena.size);
        let demo = demo.filter(|_| rest.size != 0).map(|p| (p, rest));
        if !hw::up(exec, arena, demo).await {
            release(exec, block, what).await;
        }
    }

    /// Vraagt de eigenaar van de pool om een blok buiten de partities. De
    /// actor zegt het zelf luid (`HOPOS_POOL_DEVICE` of `_FAIL`); hier
    /// alleen wat hij niet kan zeggen: dat hij nooit antwoordde.
    async fn reserve(exec: &'static Executor, size: u64, what: &'static str) -> Option<Region> {
        let call = kern::slots::call(
            &crate::LIFECYCLE,
            &REPLY,
            Request::ReserveDevice { size, what },
        );
        match select(call, exec.after(POOL_WAIT)).await {
            Either::Left(Ok(Response::Device(r))) => Some(r),
            Either::Left(Ok(Response::Failed(_))) => None,
            Either::Left(Ok(r)) => {
                println!("codec: the pool owner answered {r:?} to a device reservation");
                None
            }
            Either::Left(Err(e)) => {
                println!("codec: device reservation not sent: {e}");
                None
            }
            Either::Right(()) => {
                // Staat het verzoek al in de rij, dan geeft de actor het blok
                // straks toch: dat lekt (veilig dicht), en dit zegt waarom.
                println!(
                    "codec: the pool owner did not answer within {} s (no slots on this board?)",
                    POOL_WAIT.as_secs()
                );
                None
            }
        }
    }

    /// Geeft het blok terug: het telt af van wat apps kunnen krijgen, en
    /// honderden MB kwijt zijn omdat het ijzer niet opkwam is zonde (Go).
    async fn release(exec: &'static Executor, region: Region, what: &'static str) {
        let call = kern::slots::call(
            &crate::LIFECYCLE,
            &REPLY,
            Request::ReleaseDevice { region, what },
        );
        match select(call, exec.after(POOL_WAIT)).await {
            Either::Left(Ok(Response::Done)) => {}
            Either::Left(Ok(r)) => println!(
                "codec: {} MB at {:#x} not back in the pool: {r:?}",
                region.size >> 20,
                region.base
            ),
            Either::Left(Err(e)) => println!(
                "codec: {} MB at {:#x} not back in the pool: {e}",
                region.size >> 20,
                region.base
            ),
            Either::Right(()) => println!(
                "codec: the pool owner did not take {} MB at {:#x} back within {} s",
                region.size >> 20,
                region.base,
                POOL_WAIT.as_secs()
            ),
        }
    }

    /// Een board zonder videocodec: de arena gaat terug, de dienst weigert.
    #[cfg(not(feature = "board-o6n"))]
    mod hw {
        use alloc::string::String;
        use cpu::println;
        use executor::Executor;
        use kern::Region;

        /// De arena als niets anders gezegd wordt: klein, want hij gaat meteen
        /// terug. Hij bestaat om de weg van de reservering op QEMU te bewijzen
        /// (`HOPOS_POOL_DEVICE` en `_RELEASE` rond `HOPOS_CODEC_NONE`).
        pub(super) const DEFAULT_ARENA_MB: u64 = 64;

        /// Geen ijzer: `false`, en de aanroeper geeft het blok terug.
        pub(super) async fn up(
            _exec: &'static Executor,
            _arena: Region,
            demo: Option<(String, Region)>,
        ) -> bool {
            println!("codec: no video codec on this board, codec calls refused HOPOS_CODEC_NONE");
            if demo.is_some() {
                println!(
                    "codecdemo: no codec hardware on this board, nothing to measure HOPOS_CODECDEMO_SKIP"
                );
            }
            false
        }
    }

    /// De VPU van de O6N: firmware, stroom, driver, opruimtaak, meetinstrument.
    #[cfg(feature = "board-o6n")]
    mod hw {
        use super::{Blobs, CODEC, FS_WAIT, MAX_BLOB, REPLY, Vpu};
        use alloc::string::String;
        use alloc::vec::Vec;
        use bounded::BoundedVec;
        use core::fmt;
        use core::time::Duration;
        use cpu::println;
        use driver_codec::{Engine, Graveyard, Show};
        use executor::Executor;
        use kern::Region;
        use media_mve::{Device, FIRMWARE};
        use sync::{Either, select};

        /// De bak van de gevallen sessie-handvatten van de VPU.
        static GRAVES: Graveyard = Graveyard::new();

        /// De arena als niets anders gezegd wordt. Een 4K HEVC-decoder
        /// houdt zestien referentieframes vast; in 10 bit is dat 24 MB per
        /// frame. Met 256 MB liep de arena leeg en eindigde dat in een MMU
        /// ABORT (22-09); 768 MB draagt één 4K10-stream met werkruimte en is
        /// op 16 GB nog geen twintigste. Alleen 8 bit: `hopos.codec=256`.
        pub(super) const DEFAULT_ARENA_MB: u64 = 768;

        /// Waar de blobs op het volume staan, in deze volgorde. `/firmware`
        /// is die van de Go-kern (`codecFirmwareDir`); `/codec-firmware` is
        /// waar Lumen ze neerzet: zijn jobspec mount `/firmware` op dat
        /// volume (OLD/image/hopos-media-o6n.cfg), en een volume is een
        /// gewoon hopfs-pad.
        const FW_DIRS: [&str; 2] = ["/firmware", "/codec-firmware"];

        /// Hoe vaak de opruimtaak de sessies van gestopte bewoners sluit.
        const REAP_EVERY: Duration = Duration::from_secs(1);

        /// De opruimtaak, als er een engine is.
        fn spawn_reaper(exec: &'static Executor) {
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

        /// De O6N: firmware, stroom, driver. `false` als het ijzer niet
        /// opkwam; de aanroeper geeft de arena dan terug.
        pub(super) async fn up(
            exec: &'static Executor,
            arena: Region,
            demo: Option<(String, Region)>,
        ) -> bool {
            let board = &crate::BOARD;
            let blobs = firmware(exec).await;
            let w = match board.power_vpu(arena.base, arena.size) {
                Ok(w) => w,
                Err(e) => {
                    println!("codec: {e}, video codec stays off HOPOS_CODEC_OFF");
                    return false;
                }
            };
            let a = match media_mve::Arena::new(arena.base, arena.size) {
                Ok(a) => a,
                Err(e) => {
                    println!("codec: {e}, video codec stays off HOPOS_CODEC_OFF");
                    return false;
                }
            };
            // SAFETY: `w.base` komt uit de DSDT van dit board (of de SoC-constante)
            // en is door `power_vpu` gemapt, van stroom, klok en reset voorzien;
            // alleen deze driver raakt het blok aan. De arena komt van de
            // eigenaar van de pool (`Request::ReserveDevice`), ligt dus buiten
            // elke partitie, en is door `power_vpu` ongecached gemapt (`_CCA = 0`).
            let probed = unsafe { Device::probe(w.base, a, blobs, &GRAVES, cpu::idle::now) };
            let dev = match probed {
                Ok(d) => d,
                Err(e) => {
                    println!("codec: {e}, video codec stays off HOPOS_CODEC_OFF");
                    return false;
                }
            };
            println!(
                "codec: {} HOPOS_CODEC_UP",
                Show(&dev, |d: &Vpu, f| d.describe(f))
            );
            if CODEC.with(|s, _| s.install(dev)).is_none() {
                // De engine viel hier: zijn arena is van niemand meer, maar
                // het blok blijft gemapt voor het ijzer. Dicht houden.
                println!("codec: service busy at boot HOPOS_CODEC_FAIL");
                return true;
            }
            spawn_reaper(exec);
            if let Some((path, buffers)) = demo
                && exec.spawn(super::demo::run(exec, path, buffers)).is_err()
            {
                println!("codecdemo: not spawned HOPOS_CODECDEMO_FAIL");
            }
            true
        }

        /// Namen met een komma ertussen, voor één regel.
        struct Names<'a>(&'a [&'static str]);

        impl fmt::Display for Names<'_> {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                for (i, n) in self.0.iter().enumerate() {
                    if i > 0 {
                        f.write_str(",")?;
                    }
                    f.write_str(n)?;
                }
                Ok(())
            }
        }

        /// De zestien blobs van het volume, door de hopfs-actor voor de kern
        /// gelezen. Wat mist, staat bij naam in één regel: een open van die
        /// codec weigert straks met "no firmware for this codec".
        async fn firmware(exec: &'static Executor) -> Blobs {
            let mut blobs = Blobs::default();
            if !crate::storage::is_up() {
                println!(
                    "codec: no volume to read /firmware from, every open will be refused HOPOS_CODEC_NOFW missing=all"
                );
                return blobs;
            }
            let mut missing: BoundedVec<&'static str, { FIRMWARE.len() }> = BoundedVec::new();
            let mut bytes = 0usize;
            for name in FIRMWARE {
                match read_blob(exec, name).await {
                    Ok(bin) => {
                        let n = bin.len();
                        if blobs.put(name, bin) {
                            bytes += n;
                        } else {
                            let _ = missing.push(name);
                        }
                    }
                    Err(kern::Error::Busy) => {
                        // Geen antwoord binnen de termijn: het verzoek staat
                        // nog in de rij en zijn antwoord komt op dezelfde
                        // plek. Een volgende lezing zou dat antwoord voor het
                        // hare houden; dus stoppen, en de rest mist.
                        println!(
                            "codec: the hopfs actor did not answer (within {} s), firmware reading stopped at {name}",
                            FS_WAIT.as_secs()
                        );
                        for rest in FIRMWARE.iter().skip_while(|n| **n != name) {
                            let _ = missing.push(rest);
                        }
                        break;
                    }
                    Err(e) => {
                        if e != kern::Error::NoEnt {
                            println!("codec: firmware {name}: {e}");
                        }
                        let _ = missing.push(name);
                    }
                }
            }
            println!(
                "codec: {} of {} firmware blobs read from the volume ({} KB)",
                blobs.len(),
                FIRMWARE.len(),
                bytes >> 10
            );
            if !missing.is_empty() {
                println!(
                    "codec: no firmware for {} in {} or {}, those codecs refuse to open HOPOS_CODEC_NOFW missing={}",
                    Names(missing.as_slice()),
                    FW_DIRS[0],
                    FW_DIRS[1],
                    Names(missing.as_slice())
                );
            }
            blobs
        }

        /// Eén blob: `<dir>/<naam>.fwb` in elk van [`FW_DIRS`], de eerste die
        /// er is. Een lege is geen firmware.
        async fn read_blob(exec: &'static Executor, name: &str) -> kern::Result<Vec<u8>> {
            let mut last = kern::Error::NoEnt;
            for dir in FW_DIRS {
                let mut path = [0u8; 64];
                let n = join(&mut path, &[dir.as_bytes(), b"/", name.as_bytes(), b".fwb"]);
                let p = path.get(..n).unwrap_or(&[]);
                let read = kern::rpc::read_file(&crate::storage::FS_INBOX, &REPLY, p, MAX_BLOB);
                match select(read, exec.after(FS_WAIT)).await {
                    Either::Left(Ok(b)) if b.is_empty() => last = kern::Error::Corrupt { at: 0 },
                    Either::Left(Ok(b)) => return Ok(b),
                    // Een volle brievenbus (of een bevroren actor) is ook
                    // `Busy`: dan staat er niets van ons in de rij, maar een
                    // actor die nu niet leest, leest de volgende ook niet.
                    Either::Left(Err(kern::Error::Busy)) | Either::Right(()) => {
                        return Err(kern::Error::Busy);
                    }
                    Either::Left(Err(e)) => last = e,
                }
            }
            Err(last)
        }

        /// Plakt `parts` in `out`; geeft de lengte (afgekapt op de buffer).
        fn join(out: &mut [u8], parts: &[&[u8]]) -> usize {
            let mut n = 0;
            for p in parts {
                for b in *p {
                    if let Some(d) = out.get_mut(n) {
                        *d = *b;
                        n += 1;
                    }
                }
            }
            n
        }
    }

    /// Het meetinstrument (Go: `codecDemo`): één bestand van het volume door
    /// de decoder, met de tijd erbij, zonder app. Het bewijst op ijzer wat
    /// de host-tests alleen tegen een model bewijzen: dat de firmware start,
    /// dat de page tables kloppen, dat de frames in ons geheugen landen, en
    /// hoe snel. Geen ABI, geen slot, geen kooi: als dit werkt, ligt de fout
    /// daarna nooit meer hier.
    ///
    /// ```text
    /// hopos.codecdemo=1                  /data/clip.hevc van het volume
    /// hopos.codecdemo=/pad/film.h264     een ander bestand; de codec uit de extensie
    /// hopos.codecdemo.pixel=p010|nv12    het uitvoerformaat (standaard p010)
    /// hopos.codecdemo.chunk=<KB>         de hap bitstream (standaard 256)
    /// hopos.codecdemo.bufs=<n>           de beeldbuffers (standaard 12)
    /// ```
    ///
    /// De meting is die van `docs/media.md`: beelden per seconde, en de
    /// bytes die de decoder per seconde in de buffers schreef, "door de
    /// grant" (24 fps 4K P010 = 597 MB/s; de Go-kern haalde 27,25 fps met
    /// Lumen, 27-09). Eén regel: `HOPOS_CODECDEMO fps=… MBps=…`.
    #[cfg(feature = "board-o6n")]
    mod demo {
        use super::{CODEC, FS_WAIT, REPLY, Vpu, param};
        use alloc::string::String;
        use alloc::vec::Vec;
        use bounded::BoundedVec;
        use core::fmt;
        use core::time::Duration;
        use cpu::println;
        use driver_codec::{
            Buffer, Codec, Config, Direction, Engine, Event, Flags, Kind, Layout, Pixel, Session,
            Show,
        };
        use executor::Executor;
        use kern::Region;
        use kern::rpc::{KernRead, kern_read};
        use sync::{Either, select};

        /// De hap bitstream per keer (Go: `codecDemoChunk`).
        const CHUNK_KB: usize = 256;
        /// Zoveel invoerbuffers tegelijk bij de decoder: een decoder geeft
        /// zijn eerste hap pas terug als hij beeldbuffers heeft, dus wie op
        /// de teruggave wacht voor hij verder voert, wacht voor altijd.
        const IN_BUFS: usize = 4;
        /// De beeldbuffers die we de decoder lenen (Go: `codecDemoBuffers`).
        const FRAMES: usize = 12;
        /// Meer dan dit houdt de boekhouding niet bij.
        const MAX_FRAMES: usize = 32;
        /// Zo lang wacht het meetinstrument op een teken van leven. Elk event
        /// zet de klok terug; een stilgevallen firmware hield de node op
        /// 22-09 tegen tot de watchdog kwam.
        const DEADLINE: Duration = Duration::from_secs(20);
        /// Niets te doen: even wachten in plaats van de core opstoken. De
        /// dienst hangt later aan INTID 358; dit pad moet ook zonder werken.
        const IDLE: Duration = Duration::from_micros(200);
        /// De paginamaat van de codec-MMU.
        const PAGE: u64 = 4096;

        /// Eén beurt op de engine; `None` zonder engine of met een lopende
        /// beurt (een bug op één core, en dan liever stoppen dan hangen).
        fn eng<R>(f: impl FnOnce(&mut Vpu) -> R) -> Option<R> {
            CODEC.with(|s, _| s.engine().map(f)).flatten()
        }

        /// Wat de hardware zegt, voor de regels bij een stilte of een fout.
        fn state() {
            let _ = eng(|e| {
                println!(
                    "codecdemo: hardware says {}",
                    Show(&*e, |d: &Vpu, f| d.state(f))
                )
            });
        }

        /// Een getal in tienden, als `12.3`.
        struct Tenths(u128);

        impl fmt::Display for Tenths {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                write!(f, "{}.{}", self.0 / 10, self.0 % 10)
            }
        }

        /// Zestien bytes als hex.
        struct Hex([u8; 16]);

        impl fmt::Display for Hex {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                self.0.iter().try_for_each(|b| write!(f, "{b:02x}"))
            }
        }

        /// Een getal uit een bootparameter, of `default`.
        fn knob(key: &'static str, default: usize) -> usize {
            param(key)
                .parse::<usize>()
                .ok()
                .filter(|n| *n > 0)
                .unwrap_or(default)
        }

        /// De meting: `path` door de decoder, de buffers in `buffers`.
        pub(super) async fn run(exec: &'static Executor, path: String, buffers: Region) {
            let codec = Codec::from_file_name(&path);
            if codec == Codec::Unknown {
                println!("codecdemo: cannot tell the codec from {path:?} HOPOS_CODECDEMO_FAIL");
                return;
            }
            let pixel = match Pixel::parse(&param("hopos.codecdemo.pixel")) {
                Pixel::None => Pixel::P010,
                p => p,
            };
            // Een hap die middenin een beeld eindigt, geeft een kapot beeld:
            // één hap voor het hele bestand gaf nul corrupte, 26 happen gaven
            // er 25 (22-09). Meer dan de helft van het blok voor invoer laat
            // geen 4K-beelden over.
            let cap_kb = (buffers.size >> 10) as usize / 2 / IN_BUFS;
            let chunk = knob("hopos.codecdemo.chunk", CHUNK_KB).min(cap_kb) << 10;
            let bufs = knob("hopos.codecdemo.bufs", FRAMES).min(MAX_FRAMES);
            // De codec is niet coherent: bitstream en beelden direct in DRAM,
            // net als de arena (Go: `memattr.NormalNC`).
            if cpu::memattr::normal_nc(buffers.base, buffers.size).is_err() {
                println!("codecdemo: cannot make the buffers uncached HOPOS_CODECDEMO_FAIL");
                return;
            }
            let mut rd = KernRead {
                path: Vec::new(),
                off: 0,
                out: Vec::new(),
            };
            if rd.path.try_reserve_exact(path.len()).is_err()
                || rd.out.try_reserve_exact(chunk).is_err()
            {
                println!(
                    "codecdemo: out of memory for a {chunk}-byte read buffer HOPOS_CODECDEMO_FAIL"
                );
                return;
            }
            rd.path.extend_from_slice(path.as_bytes());
            let (size, rd) = match read(exec, rd).await {
                (Ok((size, _)), rd) if size > 0 => (size, rd),
                (r, _) => {
                    println!(
                        "codecdemo: {path}: {r:?} (put a stream there first) HOPOS_CODECDEMO_FAIL"
                    );
                    return;
                }
            };
            let cfg = Config {
                codec,
                dir: Direction::Decode,
                pixel,
                width: 0,
                height: 0,
            };
            let ses = match eng(|e| e.open(&cfg)) {
                Some(Ok(s)) => s,
                Some(Err(e)) => {
                    println!("codecdemo: open {codec}: {e} HOPOS_CODECDEMO_FAIL");
                    return;
                }
                None => {
                    println!("codecdemo: no codec engine HOPOS_CODECDEMO_FAIL");
                    return;
                }
            };
            println!(
                "codecdemo: {path} ({} KB, {codec} to {pixel}) in {chunk}-byte bites, {bufs} frame buffers",
                size >> 10
            );
            let mut m = Meter::new(exec, buffers, chunk, bufs, size, rd);
            let end = m.pump(&ses).await;
            let ns = exec.now().saturating_sub(m.start).max(1);
            let _ = eng(move |e| e.close(ses));
            match end {
                End::Done => m.report(ns),
                End::Quiet => {
                    println!(
                        "codecdemo: gave up after {} s without an event: {} frames, {} input buffer(s) outstanding, layout {}x{} HOPOS_CODECDEMO_FAIL",
                        DEADLINE.as_secs(),
                        m.frames,
                        m.busy,
                        m.layout.width,
                        m.layout.height
                    );
                    state();
                }
                End::Failed => state(),
            }
        }

        /// Eén lezing van de teststream voor de kern, met een termijn.
        async fn read(
            exec: &'static Executor,
            rd: KernRead,
        ) -> (kern::Result<(u64, usize)>, KernRead) {
            let r = kern_read(&crate::storage::FS_INBOX, &REPLY, rd);
            match select(r, exec.after(FS_WAIT)).await {
                Either::Left(d) => (
                    d.result,
                    KernRead {
                        path: d.buf,
                        off: 0,
                        out: d.out,
                    },
                ),
                // De buffers zitten in de rij van de actor en komen niet
                // meer terug; de meting stopt hier toch.
                Either::Right(()) => (
                    Err(kern::Error::Busy),
                    KernRead {
                        path: Vec::new(),
                        off: 0,
                        out: Vec::new(),
                    },
                ),
            }
        }

        /// Hoe de pomp eindigde.
        enum End {
            /// De stream is door (Done en een lege rij).
            Done,
            /// [`DEADLINE`] zonder event.
            Quiet,
            /// Een fout; de regel staat er al.
            Failed,
        }

        /// De staat van één meting: de buffers, de stream en de tellers.
        struct Meter {
            exec: &'static Executor,
            chunk: u64,
            bufs: usize,
            size: u64,
            rd: KernRead,
            pool: u64,
            pool_end: u64,
            in_free: BoundedVec<Buffer, IN_BUFS>,
            free: BoundedVec<Buffer, MAX_FRAMES>,
            layout: Layout,
            offset: u64,
            busy: usize,
            frames: u64,
            skipped: u64,
            out_bytes: u64,
            eos: bool,
            done: bool,
            start: u64,
        }

        impl Meter {
            fn new(
                exec: &'static Executor,
                buffers: Region,
                chunk: usize,
                bufs: usize,
                size: u64,
                rd: KernRead,
            ) -> Meter {
                let chunk = chunk as u64;
                let mut in_free = BoundedVec::new();
                for i in 0..IN_BUFS as u64 {
                    let _ = in_free.push(Buffer {
                        pa: buffers.base + i * chunk,
                        size: chunk,
                    });
                }
                Meter {
                    exec,
                    chunk,
                    bufs,
                    size,
                    rd,
                    pool: buffers.base + IN_BUFS as u64 * chunk,
                    pool_end: buffers.base + buffers.size,
                    in_free,
                    free: BoundedVec::new(),
                    layout: Layout::default(),
                    offset: 0,
                    busy: 0,
                    frames: 0,
                    skipped: 0,
                    out_bytes: 0,
                    eos: false,
                    done: false,
                    start: exec.now(),
                }
            }

            /// Voeren, aanbieden, events halen, tot de stream door is.
            async fn pump(&mut self, ses: &Session) -> End {
                let mut deadline = self.exec.now().saturating_add(DEADLINE.as_nanos() as u64);
                loop {
                    if self.exec.now() > deadline {
                        return End::Quiet;
                    }
                    if !self.feed(ses).await || !self.offer(ses) {
                        return End::Failed;
                    }
                    let Some(ev) = eng(|e| e.next_event(ses)).flatten() else {
                        // Na Done pas stoppen als de rij écht leeg is: de
                        // laatste beelden staan er dan vaak nog in.
                        if self.done {
                            return End::Done;
                        }
                        self.exec.after(IDLE).await;
                        continue;
                    };
                    deadline = self.exec.now().saturating_add(DEADLINE.as_nanos() as u64);
                    if !self.event(&ev) {
                        return End::Failed;
                    }
                }
            }

            /// Bitstream bijvoeren zolang er een vrije invoerbuffer is.
            async fn feed(&mut self, ses: &Session) -> bool {
                while !self.eos && !self.done {
                    let Some(inb) = self.in_free.pop() else {
                        return true;
                    };
                    let n = self.chunk.min(self.size - self.offset);
                    if n > 0 {
                        let mut rd = core::mem::replace(
                            &mut self.rd,
                            KernRead {
                                path: Vec::new(),
                                off: 0,
                                out: Vec::new(),
                            },
                        );
                        rd.off = self.offset;
                        rd.out.resize(n as usize, 0);
                        let (r, back) = read(self.exec, rd).await;
                        self.rd = back;
                        match r {
                            Ok((_, got)) if got as u64 == n => {}
                            other => {
                                println!(
                                    "codecdemo: read at {}: {other:?} HOPOS_CODECDEMO_FAIL",
                                    self.offset
                                );
                                return false;
                            }
                        }
                        dev::copy_in(
                            dev::Pa(inb.pa),
                            self.rd.out.get(..n as usize).unwrap_or(&[]),
                        );
                        self.offset += n;
                    }
                    let flags = if self.offset >= self.size {
                        self.eos = true;
                        Flags::EOS
                    } else {
                        Flags(0)
                    };
                    let tag = self.offset;
                    match eng(|e| e.feed(ses, inb, n, flags, tag)) {
                        Some(Ok(())) => self.busy += 1,
                        other => {
                            println!("codecdemo: feed: {other:?} HOPOS_CODECDEMO_FAIL");
                            return false;
                        }
                    }
                }
                true
            }

            /// Lege beeldbuffers aanbieden zodra de maat bekend is.
            fn offer(&mut self, ses: &Session) -> bool {
                if self.layout.frame_size == 0 || self.done {
                    return true;
                }
                while let Some(b) = self.free.pop() {
                    match eng(|e| e.offer(ses, b)) {
                        Some(Ok(())) => {}
                        other => {
                            println!("codecdemo: offer: {other:?} HOPOS_CODECDEMO_FAIL");
                            return false;
                        }
                    }
                }
                true
            }

            /// Eén event; `false` is het einde met een regel.
            fn event(&mut self, ev: &Event) -> bool {
                match ev.kind {
                    Kind::Format => return self.format(&ev.layout),
                    Kind::Consumed => {
                        self.busy = self.busy.saturating_sub(1);
                        if let Some(b) = ev.buf {
                            let _ = self.in_free.push(b);
                        }
                    }
                    Kind::Produced => {
                        let Some(b) = ev.buf else { return true };
                        if ev.bytes == 0 {
                            // Gedecodeerd maar niet om te tonen: geen beeld.
                            self.skipped += 1;
                        } else {
                            self.frames += 1;
                            self.out_bytes = self.out_bytes.saturating_add(ev.bytes);
                            if self.frames == 1 {
                                // De bit-indeling van een 10-bit beeld zonder
                                // een heel frame te vergelijken: staat de
                                // waarde links in het woord (P010) of rechts?
                                let mut head = [0u8; 16];
                                dev::copy_out(&mut head, dev::Pa(b.pa));
                                println!(
                                    "codecdemo: first luma bytes {} ({}, stride {})",
                                    Hex(head),
                                    ev.layout.pixel,
                                    ev.layout.planes[0].stride
                                );
                            }
                        }
                        let _ = self.free.push(b);
                    }
                    Kind::Done => self.done = true,
                    Kind::Fault => {
                        match ev.fault {
                            Some(e) => println!(
                                "codecdemo: {e} after {} frame(s) HOPOS_CODECDEMO_FAIL",
                                self.frames
                            ),
                            None => println!(
                                "codecdemo: fault after {} frame(s) HOPOS_CODECDEMO_FAIL",
                                self.frames
                            ),
                        }
                        return false;
                    }
                }
                true
            }

            /// De maat is bekend: beeldbuffers uit het blok, elk op een eigen
            /// pagina (de codec-MMU kent niets fijners; 1920x1080 NV12 is
            /// 759,375 pagina's, dus de stap rondt omhoog).
            fn format(&mut self, l: &Layout) -> bool {
                self.layout = *l;
                println!(
                    "codecdemo: {}x{} {}, {} bytes per frame, {} buffers wanted",
                    l.width, l.height, l.pixel, l.frame_size, l.min_buffers
                );
                // De firmware noemt een MINIMUM; een herordenende stream houdt
                // er meer vast, dus zoveel als er passen.
                let n = self.bufs.max(l.min_buffers as usize + 1).min(MAX_FRAMES);
                // De maat komt van de firmware: een onzinnig getal is geen
                // buffer, en geen omloop.
                let step = l.frame_size.checked_next_multiple_of(PAGE).unwrap_or(0);
                self.free.clear();
                for i in 0..n as u64 {
                    let pa = i.checked_mul(step).and_then(|o| self.pool.checked_add(o));
                    let end = pa.and_then(|pa| pa.checked_add(step));
                    let (Some(pa), Some(end)) = (pa, end) else {
                        break;
                    };
                    if step == 0 || end > self.pool_end {
                        break;
                    }
                    let _ = self.free.push(Buffer { pa, size: step });
                }
                if self.free.is_empty() {
                    println!("codecdemo: frames do not fit the buffer block HOPOS_CODECDEMO_FAIL");
                    return false;
                }
                true
            }

            /// De ene regel met de meting.
            fn report(&self, ns: u64) {
                let ns = u128::from(ns);
                let fps = u128::from(self.frames) * 10_000_000_000 / ns;
                // Bytes per nanoseconde maal duizend is MB/s (10^6).
                let mbps = u128::from(self.out_bytes) * 1_000 / ns;
                println!(
                    "codecdemo: {} frames ({}x{} {}) from {} MB in {} ms: {} fps, {mbps} MB/s through the grant HOPOS_CODECDEMO fps={} MBps={mbps}",
                    self.frames,
                    self.layout.width,
                    self.layout.height,
                    self.layout.pixel,
                    self.size >> 20,
                    ns / 1_000_000,
                    Tenths(fps),
                    Tenths(fps)
                );
                if self.skipped > 0 {
                    println!(
                        "codecdemo: {} frame(s) came back empty (decode-only or rejected)",
                        self.skipped
                    );
                }
                state();
            }
        }
    }
}

#[cfg(feature = "media")]
pub(crate) use on::{planned_bytes, up};

#[cfg(not(feature = "media"))]
mod off {
    use executor::Executor;

    /// Buiten de media-smaak: geen VPU-driver, geen dienst. De codec-ops
    /// antwoorden "onbekende op", zoals op een board zonder codec-ijzer.
    pub(crate) fn up(_exec: &'static Executor) {}

    /// Kaal reserveert de codec niets uit de pool.
    pub(crate) fn planned_bytes() -> u64 {
        0
    }
}

#[cfg(not(feature = "media"))]
pub(crate) use off::{planned_bytes, up};
