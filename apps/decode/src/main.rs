//! decode: een stream door de hardwaredecoder van de node, met de fps erbij.
//!
//! De kleinste app die de codec-dienst gebruikt zoals Lumen dat doet
//! (`OLD/metal/app/lumen`): een sessie openen met `applib::codec`,
//! bitstream in de eigen partitie schrijven en voeren, beeldbuffers
//! aanbieden, events ophalen, en tellen. Er gaat geen beeld over de
//! verbinding: een buffer is een stuk van de eigen partitie, de kern hangt
//! het in de page tables van de codec (docs/media.md). Aan het eind één
//! regel: `HOPOS_DECODE fps=… MBps=…`, de meting door de hele ABI heen.
//!
//! De stream komt van het eigen volume (`DECODE_FILE`, standaard
//! `/data/clip.hevc`) of van een URL over `appnet` (`DECODE_URL`); de codec
//! uit de extensie of `DECODE_CODEC`, het uitvoerformaat uit `DECODE_PIXEL`
//! (standaard p010), het aantal beeldbuffers uit `DECODE_BUFS` (standaard
//! 12). De jobspec staat in de README.
//!
//! Eerst de codec, dan de stream: op een node zonder VPU (QEMU) weigert de
//! kern de open luid ("this node has no codec hardware"), de app zegt dat
//! in één regel (`HOPOS_DECODE_NOCODEC`) en blijft leven zonder iets te
//! doen, zodat Hop hem niet in een herstartlus neemt. Na de meting blijft hij
//! om dezelfde reden staan.

#![cfg_attr(target_os = "none", no_std, no_main)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

extern crate alloc;

mod mem;
mod source;

use applib::appnet::{self, SystemClient};
use applib::codec::{Codec, Config, Direction, Event, Flags, Kind, MAX_EVENTS, Pixel, Session};
use applib::rt::Exec;
use applib::{App, EXEC, clock, log, park};
use bounded::BoundedVec;
use core::fmt;
use core::time::Duration;
use mem::PageBuf;
use source::{Source, SourceError, file_part};

applib::main!(decode);

/// Op de host bestaat dit image niet: daar is dit een lege binary, zodat de
/// host-poort de logica kan toetsen en clippy de rest kan lezen.
#[cfg(not(target_os = "none"))]
fn main() {}

/// De system-client van de app.
pub(crate) type Sys = SystemClient;

/// De stream zonder `DECODE_FILE` en `DECODE_URL`: dezelfde plek als waar
/// het meetinstrument van de kern hem zoekt (Go: `codecClipPath`), zodat
/// één bestand op het volume beide meet.
const DEFAULT_FILE: &str = "/data/clip.hevc";

/// Invoerbuffers tegelijk bij de decoder: een decoder geeft zijn eerste hap
/// pas terug als hij beeldbuffers heeft, dus wie op de teruggave wacht voor
/// hij verder voert, wacht voor altijd (Go: `codecDemoInBufs`).
const IN_BUFS: usize = 4;

/// Eén hap bitstream. Een hap die middenin een beeld eindigt geeft geen
/// fout, alleen een kapot beeld (22-09); een grote hap maakt dat zeldzaam.
const IN_SIZE: usize = 1 << 20;

/// Waar een hap knipt: het begin van de laatste Annex-B-startcode in `b`
/// (`00 00 01`, met de `00` ervoor als die er is), zodat elke NAL-eenheid
/// heel bij de decoder komt en de rest naar de volgende hap gaat. `None`
/// als er na de eerste byte geen startcode staat (één NAL groter dan de
/// hap: dan gaat alles, gesplitst).
fn nal_cut(b: &[u8]) -> Option<usize> {
    let mut i = b.len().checked_sub(3)?;
    while i > 0 {
        if b.get(i..i + 3) == Some(&[0, 0, 1]) {
            let cut = if b.get(i - 1) == Some(&0) { i - 1 } else { i };
            return (cut > 0).then_some(cut);
        }
        i -= 1;
    }
    None
}

/// De beeldbuffers zonder `DECODE_BUFS` (Go: `codecDemoBuffers`).
const FRAMES: usize = 12;

/// Meer beeldbuffers houdt de boekhouding niet bij.
const MAX_FRAMES: usize = 32;

/// Hoe lang de kern een poll laat wachten op een event: de app slaapt op
/// het antwoord en ziet een stilgevallen decoder na [`QUIET`].
const WAIT: Duration = Duration::from_secs(1);

/// Zo lang zonder event is een stilgevallen decoder.
const QUIET: Duration = Duration::from_secs(20);

async fn decode(app: &'static App) {
    let exec: &'static Exec = EXEC.get();
    let net = match appnet::up(app) {
        Ok(n) => n,
        Err(e) => {
            log!("decode: no network stack: {e} HOPOS_DECODE_FAIL");
            park().await;
        }
    };
    let mut sys = net.system_client();
    let name = app
        .env("DECODE_URL")
        .or_else(|| app.env("DECODE_FILE"))
        .unwrap_or(DEFAULT_FILE);
    let codec = match app.env("DECODE_CODEC") {
        Some(c) => Codec::parse(c),
        None => Codec::from_file_name(file_part(name)),
    };
    if codec == Codec::Unknown {
        log!(
            "decode: cannot tell the codec of {name}; set DECODE_CODEC (hevc, h264, av1, ...) HOPOS_DECODE_FAIL"
        );
        park().await;
    }
    let pixel = match app.env("DECODE_PIXEL").map(Pixel::parse) {
        None | Some(Pixel::None) => Pixel::P010,
        Some(p) => p,
    };
    let cfg = Config {
        codec,
        dir: Direction::Decode,
        pixel,
        width: 0,
        height: 0,
    };
    // Eerst de codec: zonder VPU hoeft er geen byte stream te komen.
    let ses = match Session::open(&mut sys, &cfg).await {
        Ok(s) => s,
        Err(e) => {
            log!(
                "decode: the node has no {codec} decoder for this app: {e}; staying up without one HOPOS_DECODE_NOCODEC"
            );
            park().await;
        }
    };
    log!(
        "decode: {codec} session {} open, {name} to {pixel} HOPOS_DECODE_OPEN",
        ses.handle()
    );
    let bufs = app
        .env("DECODE_BUFS")
        .and_then(|b| b.parse::<usize>().ok())
        .filter(|n| *n > 0)
        .unwrap_or(FRAMES)
        .min(MAX_FRAMES);
    match run(app, exec, &mut sys, &ses, name, bufs).await {
        Ok(m) => m.report(name),
        Err(e @ Stop::Stopped { .. }) => log!("decode: {name}: {e}"),
        Err(e) => log!("decode: {name}: {e} HOPOS_DECODE_FAIL"),
    }
    // De sessie dicht vóór de exit: een sessie die met de app sterft, laat
    // de codec met een slot zitten ("session slot 0 will not terminate" op
    // de O6N, 04-10). Dan wachten tot de kern vraagt te stoppen.
    if let Err(e) = ses.close(&mut sys).await {
        log!("decode: close: {e}");
    }
    app.stopped().await;
}

/// Waarom een meting stopte.
#[derive(Debug)]
enum Stop {
    /// De bron.
    Source(SourceError),
    /// Een codec-call.
    Codec(applib::sys::Error),
    /// De heap had de buffers niet.
    Memory {
        /// Hoeveel buffers het ijzer minstens wil.
        want: usize,
        /// Van welke maat.
        size: u64,
    },
    /// Een grotere resolutie midden in de stream.
    Grew {
        /// De nieuwe maat per beeld.
        size: u64,
    },
    /// De decoder meldde een fout; de sessie is verloren.
    Fault {
        /// Beelden tot dan.
        frames: u64,
    },
    /// [`QUIET`] zonder event.
    Quiet {
        /// Beelden tot dan.
        frames: u64,
    },
    /// De kern vroeg de app te stoppen.
    Stopped {
        /// Beelden tot dan.
        frames: u64,
    },
}

impl fmt::Display for Stop {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Stop::Source(e) => write!(f, "stream: {e}"),
            Stop::Codec(e) => write!(f, "codec: {e}"),
            Stop::Memory { want, size } => {
                write!(f, "no heap for {want} frame buffers of {size} bytes")
            }
            Stop::Grew { size } => write!(f, "the stream grew to {size} bytes per frame"),
            Stop::Fault { frames } => write!(f, "the decoder faulted after {frames} frame(s)"),
            Stop::Stopped { frames } => write!(f, "stopped after {frames} frame(s)"),
            Stop::Quiet { frames } => write!(
                f,
                "no event for {} s after {frames} frame(s)",
                QUIET.as_secs()
            ),
        }
    }
}

/// Wat een meting telde.
#[derive(Default)]
struct Meter {
    frames: u64,
    skipped: u64,
    bytes: u64,
    fed: u64,
    ns: u64,
    width: u16,
    height: u16,
    pixel: Pixel,
}

/// Een getal in tienden, als `24.3`.
struct Tenths(u128);

impl fmt::Display for Tenths {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}", self.0 / 10, self.0 % 10)
    }
}

/// Beelden per seconde in tienden, en MB/s (10^6) door de grant.
fn rate(frames: u64, bytes: u64, ns: u64) -> (Tenths, u128) {
    let ns = u128::from(ns.max(1));
    (
        Tenths(u128::from(frames) * 10_000_000_000 / ns),
        u128::from(bytes) * 1_000 / ns,
    )
}

impl Meter {
    /// De ene regel met de meting.
    fn report(&self, name: &str) {
        let (fps, mbps) = rate(self.frames, self.bytes, self.ns);
        log!(
            "decode: {} frames {}x{} {} from {} ({} MB) in {} ms: {fps} fps, {mbps} MB/s through the grant HOPOS_DECODE fps={fps} MBps={mbps}",
            self.frames,
            self.width,
            self.height,
            self.pixel,
            name,
            self.fed >> 20,
            self.ns / 1_000_000
        );
        if self.skipped > 0 {
            log!(
                "decode: {} frame(s) came back empty (decode-only or rejected)",
                self.skipped
            );
        }
    }
}

/// De buffers van één meting: invoer en beelden, en welke vrij zijn.
struct Bufs {
    ram: u64,
    ins: BoundedVec<PageBuf, IN_BUFS>,
    in_free: BoundedVec<usize, IN_BUFS>,
    outs: BoundedVec<PageBuf, MAX_FRAMES>,
    out_free: BoundedVec<usize, MAX_FRAMES>,
}

impl Bufs {
    /// De invoerbuffers; `None` zonder heap.
    fn new(ram: u64) -> Option<Bufs> {
        let mut b = Bufs {
            ram,
            ins: BoundedVec::new(),
            in_free: BoundedVec::new(),
            outs: BoundedVec::new(),
            out_free: BoundedVec::new(),
        };
        for i in 0..IN_BUFS {
            b.ins.push(PageBuf::new(IN_SIZE)?).ok()?;
            b.in_free.push(i).ok()?;
        }
        Some(b)
    }

    /// De beeldbuffers na Format: `want` van `size`, minstens `min`.
    fn frames(&mut self, want: usize, min: usize, size: u64) -> Result<(), Stop> {
        let len = usize::try_from(size).unwrap_or(usize::MAX);
        while self.outs.len() < want.min(MAX_FRAMES) {
            let Some(b) = PageBuf::new(len) else { break };
            let i = self.outs.len();
            if self.outs.push(b).is_err() || self.out_free.push(i).is_err() {
                break;
            }
        }
        if self.outs.len() < min {
            return Err(Stop::Memory { want: min, size });
        }
        Ok(())
    }

    /// Welke buffer ligt op afstand `off`?
    fn find(list: &[PageBuf], ram: u64, off: u64) -> Option<usize> {
        list.iter().position(|b| b.off(ram) == off)
    }
}

/// De meting: voeren, aanbieden, ophalen, tot Done en een lege rij.
async fn run(
    app: &'static App,
    exec: &'static Exec,
    sys: &mut Sys,
    ses: &Session,
    name: &'static str,
    want: usize,
) -> Result<Meter, Stop> {
    let mut src = Source::open(exec, name).await.map_err(Stop::Source)?;
    let mut b = Bufs::new(app.ram_start()).ok_or(Stop::Memory {
        want: IN_BUFS,
        size: IN_SIZE as u64,
    })?;
    let mut m = Meter::default();
    // De rest van een hap: de NAL-eenheid die over de grens van een
    // invoerbuffer liep, voor de volgende buffer (hoogstens één hap).
    let mut carry: alloc::vec::Vec<u8> = alloc::vec::Vec::new();
    carry.try_reserve_exact(IN_SIZE).map_err(|_| Stop::Memory {
        want: 1,
        size: IN_SIZE as u64,
    })?;
    let (mut eos, mut done) = (false, false);
    let mut evs = [Event::default(); MAX_EVENTS];
    let start = clock::now_ns();
    let quiet = u64::try_from(QUIET.as_nanos()).unwrap_or(u64::MAX);
    let mut last_event = start;
    loop {
        // De stopbel: de meting staakt, de sessie gaat dan netjes dicht.
        if app.stop().is_set() {
            return Err(Stop::Stopped { frames: m.frames });
        }
        // Bitstream bijvoeren zolang er een vrije invoerbuffer is.
        while !eos {
            let Some(i) = b.in_free.pop() else { break };
            let Some(buf) = b.ins.as_mut_slice().get_mut(i) else {
                break;
            };
            // Eerst de rest van de vorige hap, dan de bron erachter; dan
            // alleen hele NAL-eenheden voeren. GEMETEN 30-09 op de O6N: een
            // NAL die over twee happen liep liet de decoder faulten (een
            // clip van 15 MB na 14 beelden, precies bij 1 MB; docs/media.md
            // zag op 22-09 al kapotte beelden bij happen), een clip in één
            // hap gaf 43 fps.
            let n = {
                let bytes = buf.bytes_mut();
                let kept = carry.len();
                if let Some(head) = bytes.get_mut(..kept) {
                    head.copy_from_slice(&carry);
                }
                carry.clear();
                let room = bytes.get_mut(kept..).unwrap_or_default();
                let got = src.fill(sys, room).await.map_err(Stop::Source)?;
                eos = got < room.len();
                let mut n = kept + got;
                if !eos && let Some(cut) = nal_cut(bytes.get(..n).unwrap_or_default()) {
                    carry.extend_from_slice(bytes.get(cut..n).unwrap_or_default());
                    n = cut;
                }
                n
            };
            let flags = if eos { Flags::EOS } else { Flags(0) };
            m.fed += n as u64;
            ses.feed(sys, buf.off(b.ram), buf.len(), n as u64, flags, i as u64)
                .await
                .map_err(Stop::Codec)?;
        }
        // Lege beeldbuffers aanbieden (na Format, tot Done).
        if !done {
            while let Some(j) = b.out_free.pop() {
                let Some(o) = b.outs.as_slice().get(j) else {
                    break;
                };
                ses.offer(sys, o.off(b.ram), o.len())
                    .await
                    .map_err(Stop::Codec)?;
            }
        }
        // Na Done is er niets meer te verwachten: dan de laatste blik meteen,
        // anders telt de wacht mee in de meting.
        let wait = if done { Duration::ZERO } else { WAIT };
        let k = ses.poll(sys, &mut evs, wait).await.map_err(Stop::Codec)?;
        if k == 0 {
            if done {
                break;
            }
            if clock::now_ns().saturating_sub(last_event) > quiet {
                return Err(Stop::Quiet { frames: m.frames });
            }
            continue;
        }
        last_event = clock::now_ns();
        for e in evs.iter().take(k) {
            match e.kind {
                Kind::Format => {
                    let min = usize::try_from(e.bytes)
                        .unwrap_or(usize::MAX)
                        .saturating_add(1);
                    if b.outs.is_empty() {
                        log!(
                            "decode: {}x{} {}, {} bytes per frame, {} buffers wanted",
                            e.width,
                            e.height,
                            Pixel::from_raw(e.pixel),
                            e.size,
                            e.bytes
                        );
                        b.frames(want.max(min), min, e.size)?;
                    } else if b.outs.as_slice().first().is_some_and(|o| o.len() < e.size) {
                        return Err(Stop::Grew { size: e.size });
                    }
                    (m.width, m.height, m.pixel) = (e.width, e.height, Pixel::from_raw(e.pixel));
                }
                Kind::Consumed => {
                    if let Some(i) = Bufs::find(b.ins.as_slice(), b.ram, e.off) {
                        let _ = b.in_free.push(i);
                    }
                }
                Kind::Produced => {
                    if e.bytes == 0 {
                        m.skipped += 1;
                    } else {
                        m.frames += 1;
                        m.bytes = m.bytes.saturating_add(e.bytes);
                    }
                    if let Some(j) = Bufs::find(b.outs.as_slice(), b.ram, e.off) {
                        let _ = b.out_free.push(j);
                    }
                }
                Kind::Done => done = true,
                Kind::Fault => return Err(Stop::Fault { frames: m.frames }),
                Kind::None => {}
            }
        }
    }
    m.ns = clock::now_ns().saturating_sub(start);
    Ok(m)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_rate_is_frames_and_bytes_per_second() {
        // De meting van docs/media.md: 24 beelden 4K P010 in een seconde.
        let (fps, mbps) = rate(24, 24 * 24_883_200, 1_000_000_000);
        assert_eq!(fps.to_string(), "24.0");
        assert_eq!(mbps, 597);
        let (fps, _) = rate(109, 0, 4_000_000_000);
        assert_eq!(fps.to_string(), "27.2");
        assert_eq!(rate(1, 1, 0).0.to_string(), "1000000000.0");
    }

    #[test]
    fn the_codec_comes_from_the_stream_name() {
        assert_eq!(Codec::from_file_name(file_part(DEFAULT_FILE)), Codec::Hevc);
        assert_eq!(
            Codec::from_file_name(file_part("http://10.0.2.2:8000/x.h264?v=2")),
            Codec::H264
        );
    }

    #[test]
    fn a_buffer_is_found_back_by_its_offset() {
        let ram = 0;
        let mut b = Bufs::new(ram).unwrap();
        b.frames(3, 2, 8192 + 1).unwrap();
        assert_eq!(b.outs.len(), 3);
        assert!(b.outs.as_slice().iter().all(|o| o.len() == 3 * 4096));
        let off = b.outs.as_slice()[2].off(ram);
        assert_eq!(Bufs::find(b.outs.as_slice(), ram, off), Some(2));
        assert_eq!(Bufs::find(b.ins.as_slice(), ram, off), None);
    }
}

#[cfg(test)]
mod nal_tests {
    use super::nal_cut;

    #[test]
    fn a_chunk_is_cut_at_the_last_start_code() {
        // Een 3-byte startcode op 6.
        assert_eq!(nal_cut(&[0, 0, 0, 1, 9, 9, 0, 0, 1, 7, 7]), Some(6));
        // Een 4-byte startcode op 6: de 00 ervoor hoort erbij.
        assert_eq!(nal_cut(&[0, 0, 0, 1, 9, 9, 0, 0, 0, 1, 7]), Some(6));
        // Alleen de startcode aan het begin: niets te knippen.
        assert_eq!(nal_cut(&[0, 0, 0, 1, 9, 9, 9]), None);
        assert_eq!(nal_cut(&[1, 2, 3]), None);
        assert_eq!(nal_cut(&[]), None);
    }
}
