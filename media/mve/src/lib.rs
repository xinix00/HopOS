//! De videocodec van de Orion O6N: een Arm China Linlon V8, de
//! doorontwikkelde Mali Video Engine (Go: `OLD/metal/media/driver/vpu/mve`).
//! Acht codecs voor decode (waaronder alle vier van een Blu-ray: MPEG-2,
//! VC-1, H.264 en HEVC) plus AV1 en VP9, tot 8K60.
//!
//! Het blok is firmware-gedreven, en dat bepaalt de vorm van deze driver:
//! per sessie laden we een codec-firmware in een eigen adresruimte, en
//! daarna is het verkeer berichten en bufferbeschrijvingen in gedeeld
//! geheugen. Wij dragen bytes en pixels, geen bitstream-kennis.
//!
//! Twee dingen die je moet weten voor je hieraan sleutelt:
//!
//! - De VPU heeft een EIGEN MMU ([`mmu`]). Die is onze isolatie: een sessie
//!   ziet alleen wat de kern in haar tabel hangt. Daarom woont deze driver
//!   in de kern en krijgt een app nooit de registers.
//! - De firmware van de O6N draagt `-sum` in zijn versie en eist een lopende
//!   checksum achter elke berichtkop ([`queue`]). Zonder dat woord komt er
//!   geen sessie van de grond.
//!
//! # Eigendom
//!
//! Eén [`Device`] per VPU, van één taak (de codec-dienst van de kern). De
//! Go-sloten (`device.mu`, `arena.mu`, `session.mu`) bestaan niet: de arena
//! en elke sessiestaat zijn velden van het device en worden alleen via
//! `&mut self` aangeraakt. Een sessie is een [`driver_codec::Session`]-
//! handvat; valt het, dan sluit de volgende beurt van het device de sessie.

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

extern crate alloc;

mod arena;
mod fwbin;
mod hwreg;
pub mod mmu;
mod proto;
mod pump;
pub mod queue;
mod session;

pub use arena::Arena;
pub use hwreg::BLOCK_LEN;

use alloc::vec::Vec;
use core::fmt;
use dev::Pa;
use driver_codec::{
    Buffer, Codec, Config, Direction, Engine, Error, Event, Firmware, Flags, Graveyard, Pixel,
    Result, Session,
};
use hwreg::{Block, EMPTY_JOB_QUEUE, FUSE_NO_HEVC, FUSE_NO_VPX, MAX_LSID};
use proto::{FMT_I420, FMT_NV12, FMT_NV21, FMT_P010, FMT_Y8};
use session::{Hw, Ses};

/// Het model in de bovenste helft van HARDWARE_ID op de O6N. De
/// 0x5664-familie deelt registerlayout en protocol met v52/v76; oudere
/// blokken (v500/v550/v61) hebben een andere formaattabel en weigeren we:
/// ongetest ijzer liever niet dan half bediend.
pub const HW_ID_LINLON_V8: u32 = 0x5664;

/// Eén VPU.
pub struct Device<F: Firmware> {
    hw: Hw,
    fw: F,
    graves: &'static Graveyard,
    ses: Vec<Ses>,
    hw_id: u32,
    rev: u32,
    fuse: u32,
    ncores: u32,
}

impl<F: Firmware> Device<F> {
    /// Neemt een VPU in gebruik: registers controleren, blok resetten,
    /// scheduler aanzetten, en per hardware-sessie de tabellen reserveren
    /// (de enige heap die dit device ooit vraagt).
    ///
    /// Op ijzer moet hiervóór het power-domein aan staan: op de O6N via SCMI
    /// naar de TF-A, die ook de interconnect-permissies zet. Zonder geeft de
    /// eerste registerlees een SError die geen handler opvangt (board-o6n).
    ///
    /// # Safety
    ///
    /// `vpu` is het gemapte registerblok van een Linlon-VPU (minstens
    /// [`BLOCK_LEN`] bytes) dat blijft bestaan, en niemand anders raakt het
    /// aan. De arena is fysiek geheugen dat alleen van deze VPU is en voor
    /// de CPU ongecached gemapt (`_CCA = 0`).
    pub unsafe fn probe(
        vpu: u64,
        arena: Arena,
        fw: F,
        graves: &'static Graveyard,
        now: fn() -> u64,
    ) -> Result<Self> {
        // SAFETY: de voorwaarde van deze functie: `vpu` is het gemapte blok
        // en blijft bestaan.
        let regs: &'static Block = unsafe { dev::regs(Pa(vpu)) };
        // HARDWARE_ID draagt het model in de bovenste helft en een variant
        // in de onderste: de O6N leest 0x56648002. Op de hele waarde
        // vergelijken werkt dus niet.
        let hw_id = regs.hardware_id.read();
        if hw_id >> 16 != HW_ID_LINLON_V8 {
            return Err(Error::Hardware { id: hw_id });
        }
        let ncores = regs.ncores.read();
        let nlsid = regs.nlsid.read();
        if ncores < 1 || nlsid < 1 || nlsid as usize > MAX_LSID {
            return Err(Error::Geometry {
                cores: ncores,
                sessions: nlsid,
            });
        }
        let mut ses = Vec::new();
        ses.try_reserve_exact(nlsid as usize)
            .map_err(|_| Error::Full { cap: nlsid })?;
        for id in 0..nlsid as u8 {
            ses.push(Ses::reserve(id)?);
        }
        // Software-reset, dan de scheduler aan met een lege job-queue. Een
        // VPU die nog een job van vóór een kern-flip vasthoudt, zou anders
        // meteen in het geheugen van de vorige wereld graven.
        regs.reset.write(1);
        regs.clk_force.write(0);
        regs.job_queue.write(EMPTY_JOB_QUEUE);
        dev::mb();
        regs.enable.write(1);
        Ok(Device {
            hw: Hw {
                regs,
                arena,
                now,
                trace: None,
            },
            fw,
            graves,
            ses,
            hw_id,
            rev: regs.svn_rev.read(),
            fuse: regs.fuse.read(),
            ncores,
        })
    }

    /// Het aantal videocores.
    #[must_use]
    pub fn cores(&self) -> u32 {
        self.ncores
    }

    /// Het aantal hardware-sessies.
    #[must_use]
    pub fn sessions(&self) -> usize {
        self.ses.len()
    }

    /// De arena: capaciteit en wat vrij is.
    #[must_use]
    pub fn arena(&self) -> &Arena {
        &self.hw.arena
    }

    /// Zet de trace: elk bericht van de firmware (bring-up). Zonder te zien
    /// wát de firmware zegt, is een stille sessie niet van een verkeerd
    /// begrepen sessie te onderscheiden.
    pub fn set_trace(&mut self, t: Option<fn(u16, &[u8])>) {
        self.hw.trace = t;
    }
}

/// De firmwarenaam bij codec en richting (`hevcdec`, `h264enc`); `None`:
/// dit ijzer doet het niet.
pub fn fw_name(c: Codec, dir: Direction) -> Option<&'static str> {
    match (dir, c) {
        (Direction::Decode, Codec::H264) => Some("h264dec"),
        (Direction::Decode, Codec::Hevc) => Some("hevcdec"),
        (Direction::Decode, Codec::Av1) => Some("av1dec"),
        (Direction::Decode, Codec::Vp8) => Some("vp8dec"),
        (Direction::Decode, Codec::Vp9) => Some("vp9dec"),
        (Direction::Decode, Codec::Mpeg2) => Some("mpeg2dec"),
        (Direction::Decode, Codec::Mpeg4) => Some("mpeg4dec"),
        (Direction::Decode, Codec::Vc1) => Some("vc1dec"),
        (Direction::Decode, Codec::Jpeg) => Some("jpegdec"),
        (Direction::Decode, Codec::Avs) => Some("avsdec"),
        (Direction::Decode, Codec::Avs2) => Some("avs2dec"),
        (Direction::Encode, Codec::H264) => Some("h264enc"),
        (Direction::Encode, Codec::Hevc) => Some("hevcenc"),
        (Direction::Encode, Codec::Vp8) => Some("vp8enc"),
        (Direction::Encode, Codec::Vp9) => Some("vp9enc"),
        (Direction::Encode, Codec::Jpeg) => Some("jpegenc"),
        _ => None,
    }
}

/// Het pixelformaat als bitveldcode van de firmware.
fn fw_pixel(p: Pixel) -> Option<u16> {
    match p {
        Pixel::Nv12 => Some(FMT_NV12),
        Pixel::Nv21 => Some(FMT_NV21),
        Pixel::I420 => Some(FMT_I420),
        Pixel::P010 => Some(FMT_P010),
        Pixel::Y8 => Some(FMT_Y8),
        Pixel::None => None,
    }
}

impl<F: Firmware> Engine for Device<F> {
    fn describe(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let (total, free) = self.hw.arena.pages();
        write!(
            f,
            "Linlon V8 (id {:#x} rev {:#x}): {} cores, {} sessions, fuse {:#x}, arena {} MB ({} MB free)",
            self.hw_id,
            self.rev,
            self.ncores,
            self.ses.len(),
            self.fuse,
            (u64::from(total) * mmu::PAGE) >> 20,
            (u64::from(free) * mmu::PAGE) >> 20
        )
    }

    fn supports(&self, c: Codec, dir: Direction) -> bool {
        if fw_name(c, dir).is_none() {
            return false;
        }
        match c {
            Codec::Hevc => self.fuse & FUSE_NO_HEVC == 0,
            Codec::Vp8 | Codec::Vp9 => self.fuse & FUSE_NO_VPX == 0,
            _ => true,
        }
    }

    fn open(&mut self, cfg: &Config) -> Result<Session> {
        self.reap();
        let name = fw_name(cfg.codec, cfg.dir).ok_or(Error::Unsupported)?;
        if !self.supports(cfg.codec, cfg.dir) {
            return Err(Error::Unsupported);
        }
        let px = fw_pixel(cfg.pixel).ok_or(Error::Pixel(cfg.pixel))?;
        let Some(i) = self.ses.iter().position(|s| !s.open) else {
            return Err(Error::Busy);
        };
        let bin = self.fw.load(name).ok_or(Error::NoFirmware)?;
        let h = fwbin::parse(bin)?;
        let (hw, ses) = (&mut self.hw, &mut self.ses);
        let s = ses.get_mut(i).ok_or(Error::Busy)?;
        if let Err(e) = s.start(hw, *cfg, px, bin, &h) {
            s.release(hw);
            return Err(e);
        }
        Ok(Session::new(i as u8, self.graves))
    }

    fn feed(&mut self, s: &Session, buf: Buffer, filled: u64, flags: Flags, tag: u64) -> Result {
        self.reap();
        let (hw, ses) = (&mut self.hw, &mut self.ses);
        let x = live(ses, s, self.graves)?;
        x.feed(hw, buf, filled, flags, tag)
    }

    fn offer(&mut self, s: &Session, buf: Buffer) -> Result {
        self.reap();
        let (hw, ses) = (&mut self.hw, &mut self.ses);
        live(ses, s, self.graves)?.offer(hw, buf)
    }

    fn next_event(&mut self, s: &Session) -> Option<Event> {
        self.reap();
        let (hw, ses) = (&mut self.hw, &mut self.ses);
        live(ses, s, self.graves).ok()?.next(hw)
    }

    fn close(&mut self, s: Session) {
        let mine = s.is_of(self.graves);
        let id = s.defuse();
        if mine && let Some(x) = self.ses.get_mut(usize::from(id)) {
            x.close(&mut self.hw);
        }
        self.reap();
    }

    fn reap(&mut self) {
        let dead = self.graves.take();
        if dead == 0 {
            return;
        }
        for (i, x) in self.ses.iter_mut().enumerate() {
            if dead & (1 << i) != 0 {
                x.close(&mut self.hw);
            }
        }
    }

    fn state(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let r = self.hw.regs;
        write!(
            f,
            "enable={} jobqueue={:#x} corelsid={:#x} irqve={:#x}",
            r.enable.read(),
            r.job_queue.read(),
            r.core_lsid.read(),
            r.irq_ve.read()
        )?;
        for (i, s) in self.ses.iter().enumerate().filter(|(_, s)| s.open) {
            let Some(l) = r.lsid.get(i) else { continue };
            let st = &s.stat;
            write!(
                f,
                " | lsid{i} alloc={} sched={} irqhost={} lirqve={} mmu={:#x} frames[decodeonly={} corrupt={} rejected={} ref={} unknown={} streamcorrupt={} flushes={} flushback={} eos={}] rpc[allocs={} {}MB] bufs[offered={} back={} held={}] va[frame={:#x} prot={:#x}]",
                l.alloc.read(),
                l.sched.read(),
                l.irq_host.read(),
                l.lirq_ve.read(),
                l.mmu_ctrl.read(),
                st.decode_only,
                st.corrupt,
                st.rejected,
                st.ref_frame,
                st.unknown,
                st.stream_corrupt,
                st.flushes,
                st.flush_back,
                st.eos,
                st.rpc_allocs,
                (u64::from(st.rpc_pages) * mmu::PAGE) >> 20,
                st.offers,
                st.returns,
                s.bufs.len(),
                s.va_frame.next,
                s.va_prot.next
            )?;
        }
        Ok(())
    }
}

/// De open sessie achter een handvat van déze engine.
fn live<'a>(ses: &'a mut [Ses], s: &Session, graves: &Graveyard) -> Result<&'a mut Ses> {
    if !s.is_of(graves) {
        return Err(Error::Closed);
    }
    ses.get_mut(usize::from(s.id()))
        .filter(|x| x.open)
        .ok_or(Error::Closed)
}

#[cfg(test)]
mod tests;
