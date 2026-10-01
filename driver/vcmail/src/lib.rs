//! De VideoCore-firmware-mailbox (property-interface, kanaal 8): het
//! universele Pi-kanaal voor alles wat de firmware beheert: temperatuur,
//! klokken, de echte board-MAC en de framebuffer.
//!
//! Zelfde blok op de Pi 4 (0xFE00_B880) en de Pi 5 (0x10_7C01_3880, DT
//! mailbox@7c013880); alleen de basis verschilt, en die kent het board.
//!
//! Protocol (brcm,bcm2835-mbox plus de property-tags uit de firmware-wiki):
//! schrijf het fysieke adres van een 16-byte-gealigneerde tag-buffer | 8 in
//! MBOX1 (+0x20, status +0x38), poll MBOX0 (+0x00, status +0x18) tot het
//! antwoord op kanaal 8 terugkomt; de firmware schrijft de respons in
//! dezelfde buffer. Het adres gaat rauw fysiek de mailbox in: de oude
//! 0xC000_0000-bus-alias was een speculatieve terugval die op de Pi 4 én 5
//! nooit nodig bleek (gesloopt 04-08). De buffer ligt in laag DRAM (onder
//! de 1 GB die de VideoCore ziet) en ongecachet: het board geeft hem uit
//! zijn DMA-regio.
//!
//! Eigendom: één [`Mbox`] per board, met `&mut self` op elke transactie. De
//! Go-kern had er een mutex op (`mboxMu`), omdat de fb-discovery en dvfs
//! elk hun eigen goroutine hadden; hier is de mailbox van één eigenaar (het
//! board houdt hem in een `Local`), en dat maakt het geval van 19-07
//! onmogelijk waarin een framebuffer-grant "3x1500000000" teruglas: de
//! kloksnelheid van dvfs, in het antwoord van de fb.

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

use core::fmt;
use core::mem::offset_of;
use dev::{Pa, Reg};

/// De mailbox-registers (brcm,bcm2835-mbox): MBOX0 is VC naar ARM, MBOX1
/// ARM naar VC.
#[repr(C)]
struct Regs {
    /// MBOX0 READ: het antwoord (adres | kanaal).
    read: Reg<u32>,
    _r0: [u32; 5],
    /// MBOX0 STATUS: bit 30 = leeg.
    status0: Reg<u32>,
    _r1: u32,
    /// MBOX1 WRITE: het verzoek (adres | kanaal).
    write: Reg<u32>,
    _r2: [u32; 5],
    /// MBOX1 STATUS: bit 31 = vol.
    status1: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Regs, read) == 0x00);
    assert!(offset_of!(Regs, status0) == 0x18);
    assert!(offset_of!(Regs, write) == 0x20);
    assert!(offset_of!(Regs, status1) == 0x38);
    assert!(core::mem::size_of::<Regs>() == 0x3c);
};

/// Het property-kanaal.
const CH_PROPS: u32 = 8;
/// STATUS: de FIFO is vol.
const STATUS_FULL: u32 = 1 << 31;
/// STATUS: de FIFO is leeg.
const STATUS_EMPTY: u32 = 1 << 30;
/// De respons-code "gelukt", in de header en per tag.
const RESP_SUCCESS: u32 = 0x8000_0000;

/// De maat van de property-buffer die het board geeft (bytes).
pub const BUFFER_BYTES: usize = 4096;
/// Woorden in de buffer.
const BUFFER_WORDS: usize = BUFFER_BYTES / 4;

/// Het hoogste bufferadres dat de mailbox draagt: het register is 32 bits
/// breed.
const MAX_BUS: u64 = u32::MAX as u64;

/// Hoe lang een stap van de transactie mag duren.
const TIMEOUT_NS: u64 = 500_000_000;

/// Property-tags (firmware-wiki "Mailbox property interface").
pub mod tag {
    /// De MAC van het board (6 bytes in 2 woorden).
    pub const GET_BOARD_MAC: u32 = 0x0001_0003;
    /// De actuele kloksnelheid: id, Hz.
    pub const GET_CLOCK_RATE: u32 = 0x0003_0002;
    /// Het firmware-maximum van een klok.
    pub const GET_MAX_CLOCK: u32 = 0x0003_0004;
    /// De SoC-temperatuur in milligraden: id, mC.
    pub const GET_TEMP: u32 = 0x0003_0006;
    /// Het firmware-minimum van een klok.
    pub const GET_MIN_CLOCK: u32 = 0x0003_0007;
    /// Zet een klok: id, Hz, skip-turbo.
    pub const SET_CLOCK_RATE: u32 = 0x0003_8002;
    /// Framebuffer: alignment in, busadres en maat uit.
    pub const FB_ALLOC: u32 = 0x0004_0001;
    /// Framebuffer: de pitch.
    pub const FB_PITCH: u32 = 0x0004_0008;
    /// Framebuffer: fysieke maat.
    pub const FB_PHYS_SIZE: u32 = 0x0004_8003;
    /// Framebuffer: virtuele maat.
    pub const FB_VIRT_SIZE: u32 = 0x0004_8004;
    /// Framebuffer: bits per pixel.
    pub const FB_DEPTH: u32 = 0x0004_8005;
    /// Laat de VideoCore de firmware van de VL805 (de USB-controller van
    /// de Pi 4 op PCIe) laden: één woord, het apparaatadres
    /// (`RPI_FIRMWARE_NOTIFY_XHCI_RESET`,
    /// `include/soc/bcm2835/raspberrypi-firmware.h`).
    pub const NOTIFY_XHCI_RESET: u32 = 0x0003_0058;
}

/// Het klok-id van de ARM-cores (het enige dat de klokwachter aanraakt).
pub const CLOCK_ARM: u32 = 3;

/// Waarom een transactie mislukte. Meetdata, geen paniek.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// De buffer is niet bruikbaar: nul, niet 16-gealigneerd of boven de
    /// 32 bits die de mailbox draagt.
    Buffer {
        /// Het adres.
        pa: u64,
    },
    /// De tags passen niet in de buffer.
    TooLarge {
        /// Woorden die nodig waren.
        words: usize,
    },
    /// Een stap van de transactie bleef hangen.
    Timeout {
        /// Welke stap: "drain", "full", "reply".
        stage: &'static str,
    },
    /// De firmware weigerde het bericht (header-code).
    Refused {
        /// De code in de header.
        code: u32,
    },
    /// De firmware kende een tag niet of weigerde hem.
    Tag {
        /// De tag.
        id: u32,
    },
    /// Een vorig verzoek wacht nog op zijn antwoord: deze buffer is nog
    /// van de firmware, dus er gaat niets overheen.
    Busy,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Buffer { pa } => write!(f, "vcmail: unusable buffer {pa:#x}"),
            Self::TooLarge { words } => {
                write!(f, "vcmail: {words} words do not fit in {BUFFER_WORDS}")
            }
            Self::Timeout { stage } => write!(f, "vcmail: timeout at {stage}"),
            Self::Refused { code } => write!(f, "vcmail: firmware refused ({code:#x})"),
            Self::Tag { id } => write!(f, "vcmail: tag {id:#x} refused"),
            Self::Busy => f.write_str("vcmail: earlier request still unanswered"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén property-tag in een (mogelijk multi-tag) transactie: `words` is het
/// payload-venster: verzoek erin, respons eroverheen (in-place, zoals het
/// protocol werkt).
pub struct Tag<'a> {
    /// De tag.
    pub id: u32,
    /// Het payload-venster.
    pub words: &'a mut [u32],
}

/// De woorden die een transactie in de buffer inneemt: header (2), per tag
/// 3 plus zijn payload, en de eind-tag.
fn words_needed(tags: &[Tag<'_>]) -> usize {
    tags.iter().fold(3usize, |n, t| {
        n.saturating_add(3).saturating_add(t.words.len())
    })
}

/// De property-buffer in device-geheugen, woord voor woord.
struct DevBuf(Pa);

impl DevBuf {
    fn put(&mut self, i: usize, v: u32) {
        dev::write32(self.0.add(4 * i as u64), v);
    }
    fn get(&self, i: usize) -> u32 {
        dev::read32(self.0.add(4 * i as u64))
    }
}

/// Schrijft het bericht: maat, code 0, per tag {id, payload-bytes, 0,
/// payload}, eind-tag. Geeft het aantal woorden.
fn encode(buf: &mut DevBuf, tags: &[Tag<'_>]) -> Result<usize> {
    let n = words_needed(tags);
    if n > BUFFER_WORDS {
        return Err(Error::TooLarge { words: n });
    }
    let mut p = 2;
    for t in tags {
        buf.put(p, t.id);
        buf.put(p + 1, (t.words.len() * 4) as u32);
        buf.put(p + 2, 0);
        for (i, &w) in t.words.iter().enumerate() {
            buf.put(p + 3 + i, w);
        }
        p += 3 + t.words.len();
    }
    buf.put(p, 0);
    buf.put(0, (n * 4) as u32);
    buf.put(1, 0);
    Ok(n)
}

/// Leest de respons terug in de tags; elke tag moet zijn respons-bit hebben.
fn decode(buf: &DevBuf, tags: &mut [Tag<'_>]) -> Result {
    let code = buf.get(1);
    if code != RESP_SUCCESS {
        return Err(Error::Refused { code });
    }
    let mut p = 2;
    for t in tags.iter_mut() {
        if buf.get(p + 2) & RESP_SUCCESS == 0 {
            return Err(Error::Tag { id: t.id });
        }
        for (i, w) in t.words.iter_mut().enumerate() {
            *w = buf.get(p + 3 + i);
        }
        p += 3 + t.words.len();
    }
    Ok(())
}

/// De framebuffer zoals de firmware hem toekende: de RESPONS telt, niet ons
/// verzoek. GEMETEN 11-07 op de Pi 5: 32 bpp gevraagd, 16 bpp gekregen (de
/// streepjes-salade op Dereks scherm was onze 32-bit render op een 16-bit
/// scanout).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Fb {
    /// Het ARM-fysieke adres (de 0xC000_0000-alias eraf).
    pub base: u64,
    /// Bytes per regel.
    pub pitch: u32,
    /// Breedte in pixels.
    pub width: u32,
    /// Hoogte in pixels.
    pub height: u32,
    /// Bits per pixel.
    pub depth: u32,
}

/// Eén mailbox met zijn property-buffer.
pub struct Mbox {
    base: Pa,
    buf: Pa,
    clock: fn() -> u64,
    /// Het adres van een verzoek dat nog geen antwoord kreeg; 0 = geen.
    /// Zolang dit staat, is de buffer van de firmware.
    pending: u32,
}

impl Mbox {
    /// Een mailbox op `base` met de property-buffer op `buf`
    /// ([`BUFFER_BYTES`], 16-gealigneerd, onder 4 GB, ongecachet gemapt).
    /// `clock` geeft monotone nanoseconden, voor de grenzen.
    ///
    /// # Safety
    ///
    /// `base` is het gemapte mailbox-blok van dit board, `buf` een
    /// buffer die alleen deze mailbox gebruikt; beide blijven zolang het
    /// programma draait.
    #[must_use]
    pub const unsafe fn new(base: Pa, buf: Pa, clock: fn() -> u64) -> Self {
        Self {
            base,
            buf,
            clock,
            pending: 0,
        }
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`: `base` is het gemapte blok.
        unsafe { dev::regs(self.base) }
    }

    fn buffer_ok(&self) -> Result {
        let pa = self.buf.0;
        if pa == 0 || !pa.is_multiple_of(16) || pa.saturating_add(BUFFER_BYTES as u64) > MAX_BUS {
            return Err(Error::Buffer { pa });
        }
        Ok(())
    }

    /// Voert één transactie uit: alle tags samen (de framebuffer-setup eist
    /// dat: de firmware finaliseert per bericht).
    pub fn call(&mut self, tags: &mut [Tag<'_>]) -> Result {
        let n = words_needed(tags);
        if n > BUFFER_WORDS {
            return Err(Error::TooLarge { words: n });
        }
        self.buffer_ok()?;
        if self.pending != 0 {
            // Het vorige verzoek kreeg nog geen antwoord: eerst dat
            // ophalen, anders schrijven we door de buffer van de firmware.
            self.wait_reply(self.pending)?;
            self.pending = 0;
        }
        self.drain()?;
        self.submit(tags)?;
        self.wait_reply(self.pending)?;
        self.pending = 0;
        dev::mb();
        decode(&DevBuf(self.buf), tags)
    }

    /// Veegt de inbox leeg: een eerder getimeout antwoord dat blijft liggen,
    /// zet anders álle volgende calls één respons achter (GEMETEN 11-07: na
    /// een trage SetClockRate tijdens HDMI-werk las elke call het antwoord
    /// van zijn voorganger: 0.0 graden, ARM 0 MHz).
    fn drain(&self) -> Result {
        let r = self.regs();
        let deadline = (self.clock)().saturating_add(TIMEOUT_NS);
        while r.status0.read() & STATUS_EMPTY == 0 {
            if (self.clock)() > deadline {
                return Err(Error::Timeout { stage: "drain" });
            }
            let _ = r.read.read();
        }
        Ok(())
    }

    /// Schrijft het bericht en geeft het aan de firmware.
    fn submit(&mut self, tags: &[Tag<'_>]) -> Result {
        encode(&mut DevBuf(self.buf), tags)?;
        dev::mb();
        let r = self.regs();
        let deadline = (self.clock)().saturating_add(TIMEOUT_NS);
        while r.status1.read() & STATUS_FULL != 0 {
            if (self.clock)() > deadline {
                return Err(Error::Timeout { stage: "full" });
            }
        }
        // Past, want `buffer_ok` toetste de 32 bits.
        let addr = (self.buf.0 as u32) | CH_PROPS;
        self.pending = addr;
        r.write.write(addr);
        Ok(())
    }

    /// Wacht op het antwoord op précies dit adres. Een time-out laat
    /// `pending` staan, zodat de volgende call de buffer niet overschrijft.
    fn wait_reply(&self, addr: u32) -> Result {
        let r = self.regs();
        let deadline = (self.clock)().saturating_add(TIMEOUT_NS);
        loop {
            if r.status0.read() & STATUS_EMPTY == 0 && r.read.read() == addr {
                return Ok(());
            }
            if (self.clock)() > deadline {
                return Err(Error::Timeout { stage: "reply" });
            }
            core::hint::spin_loop();
        }
    }

    fn one(&mut self, id: u32, words: &mut [u32]) -> Result {
        self.call(&mut [Tag { id, words }])
    }

    /// De SoC-temperatuur in milligraden Celsius.
    pub fn temp(&mut self) -> Result<u32> {
        let mut w = [0, 0];
        self.one(tag::GET_TEMP, &mut w)?;
        Ok(w[1])
    }

    /// De actuele kloksnelheid van klok `id` (Hz).
    pub fn clock_rate(&mut self, id: u32) -> Result<u32> {
        let mut w = [id, 0];
        self.one(tag::GET_CLOCK_RATE, &mut w)?;
        Ok(w[1])
    }

    /// Het firmware-maximum van klok `id` (Hz): de "vol"-stand.
    pub fn max_clock_rate(&mut self, id: u32) -> Result<u32> {
        let mut w = [id, 0];
        self.one(tag::GET_MAX_CLOCK, &mut w)?;
        Ok(w[1])
    }

    /// Het firmware-minimum van klok `id` (Hz). Lager klemt de firmware
    /// toch (GEMETEN 11-07 op de Pi 5: SetClockRate(600M) werd stilzwijgend
    /// 1500M, de `arm_freq_min`-vloer).
    pub fn min_clock_rate(&mut self, id: u32) -> Result<u32> {
        let mut w = [id, 0];
        self.one(tag::GET_MIN_CLOCK, &mut w)?;
        Ok(w[1])
    }

    /// Zet klok `id` op `hz` (skip-turbo = 0) en geeft de waarde die de
    /// firmware werkelijk koos.
    pub fn set_clock_rate(&mut self, id: u32, hz: u32) -> Result<u32> {
        let mut w = [id, hz, 0];
        self.one(tag::SET_CLOCK_RATE, &mut w)?;
        Ok(w[1])
    }

    /// De MAC van het board zoals de firmware hem kent (OTP).
    pub fn board_mac(&mut self) -> Result<[u8; 6]> {
        let mut w = [0, 0];
        self.one(tag::GET_BOARD_MAC, &mut w)?;
        Ok(mac_from_words(w))
    }

    /// Meldt de VideoCore dat de VL805 op `dev_addr` (bus << 20 | dev <<
    /// 15 | fn << 12) net uit PCIe-reset kwam, zodat hij er de firmware
    /// in laadt (Linux `rpi_reset_reset`, `drivers/reset/reset-raspberrypi.c`).
    /// Eén tag van vier bytes, precies zoals Linux hem stuurt. Geeft het
    /// woord dat de firmware terugschreef; Linux kijkt er niet naar, wij
    /// loggen het. Een geslaagde call zegt alleen dat de VideoCore het
    /// bericht nam: of de VL805 draait, zegt pas zijn versieregister.
    pub fn notify_xhci_reset(&mut self, dev_addr: u32) -> Result<u32> {
        let mut w = [dev_addr];
        self.one(tag::NOTIFY_XHCI_RESET, &mut w)?;
        Ok(w[0])
    }

    /// Vraagt een `w` x `h` x 32 bpp framebuffer (één transactie: maten,
    /// diepte, allocatie, pitch) en geeft wat er werkelijk kwam.
    ///
    /// ÉÉN beeld, geen dubbele buffer: de Pi 5-firmware neemt een virtuele
    /// hoogte van 2x wel aan, maar weigert daarna elk pan-verzoek (gemeten
    /// 05-08).
    pub fn alloc_fb(&mut self, w: u32, h: u32) -> Result<Fb> {
        let mut phys = [w, h];
        let mut virt = [w, h];
        let mut depth = [32];
        let mut alloc = [4096, 0];
        let mut pitch = [0];
        self.call(&mut [
            Tag {
                id: tag::FB_PHYS_SIZE,
                words: &mut phys,
            },
            Tag {
                id: tag::FB_VIRT_SIZE,
                words: &mut virt,
            },
            Tag {
                id: tag::FB_DEPTH,
                words: &mut depth,
            },
            Tag {
                id: tag::FB_ALLOC,
                words: &mut alloc,
            },
            Tag {
                id: tag::FB_PITCH,
                words: &mut pitch,
            },
        ])?;
        fb_from(phys, depth[0], alloc, pitch[0]).ok_or(Error::Tag { id: tag::FB_ALLOC })
    }
}

/// De MAC uit de twee antwoordwoorden: de bytes staan in volgorde, little
/// endian over de woorden.
#[must_use]
pub fn mac_from_words(w: [u32; 2]) -> [u8; 6] {
    let a = w[0].to_le_bytes();
    let b = w[1].to_le_bytes();
    [a[0], a[1], a[2], a[3], b[0], b[1]]
}

/// De framebuffer uit de antwoorden; `None` als de firmware iets op nul
/// liet (geen scherm, of geweigerd). Het busadres kan 0xC000_0000-gealiast
/// terugkomen; dat masker eraf is het ARM-fysieke adres.
fn fb_from(phys: [u32; 2], depth: u32, alloc: [u32; 2], pitch: u32) -> Option<Fb> {
    if alloc[0] == 0 || pitch == 0 || depth == 0 {
        return None;
    }
    Some(Fb {
        base: u64::from(alloc[0] & !0xC000_0000),
        pitch,
        width: phys[0],
        height: phys[1],
        depth,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering::SeqCst};

    static NOW: AtomicU64 = AtomicU64::new(0);

    /// Een klok die bij elke lezing 1 ms verspringt: elke grens loopt af.
    fn ticking() -> u64 {
        NOW.fetch_add(1_000_000, SeqCst)
    }

    /// Nep-registers en een nep-buffer in RAM (u64 voor de uitlijning).
    fn fake() -> (Vec<u64>, Vec<u64>) {
        (vec![0; 0x40 / 8], vec![0; BUFFER_BYTES / 8])
    }

    fn pa(v: &mut [u64]) -> Pa {
        Pa(v.as_mut_ptr() as usize as u64)
    }

    /// Een [`DevBuf`] over een slice: de protocoltoetsen draaien op gewoon
    /// geheugen (boven 4 GB, maar `encode` en `decode` kijken niet naar het
    /// adres).
    fn devbuf(v: &mut [u64]) -> DevBuf {
        DevBuf(pa(v))
    }

    /// De eerste `n` woorden van de buffer.
    fn words(b: &DevBuf, n: usize) -> Vec<u32> {
        (0..n).map(|i| b.get(i)).collect()
    }

    #[test]
    fn encode_lays_out_header_tags_and_end() {
        let mut a = [7, 0];
        let mut b = [1, 2, 3];
        let tags = [
            Tag {
                id: tag::GET_TEMP,
                words: &mut a,
            },
            Tag {
                id: tag::SET_CLOCK_RATE,
                words: &mut b,
            },
        ];
        let mut mem = [u64::MAX; 8];
        let mut buf = devbuf(&mut mem);
        let n = encode(&mut buf, &tags).unwrap();
        assert_eq!(n, 2 + 3 + 2 + 3 + 3 + 1);
        assert_eq!(
            words(&buf, n),
            [
                56,
                0,
                tag::GET_TEMP,
                8,
                0,
                7,
                0,
                tag::SET_CLOCK_RATE,
                12,
                0,
                1,
                2,
                3,
                0
            ]
        );
    }

    #[test]
    fn decode_needs_the_success_codes() {
        let mut w = [0, 0];
        let mut mem = [0u64; 4];
        let mut buf = devbuf(&mut mem);
        {
            let tags = [Tag {
                id: tag::GET_TEMP,
                words: &mut w,
            }];
            encode(&mut buf, &tags).unwrap();
        }
        let mut tags = [Tag {
            id: tag::GET_TEMP,
            words: &mut w,
        }];
        assert_eq!(decode(&buf, &mut tags), Err(Error::Refused { code: 0 }));
        buf.put(1, RESP_SUCCESS);
        assert_eq!(
            decode(&buf, &mut tags),
            Err(Error::Tag { id: tag::GET_TEMP })
        );
        buf.put(4, RESP_SUCCESS | 8);
        buf.put(6, 51_234);
        decode(&buf, &mut tags).unwrap();
        assert_eq!(w, [0, 51_234]);
    }

    #[test]
    fn too_many_words_are_refused_before_any_mmio() {
        let (mut regs, mut buf) = fake();
        // SAFETY: de vectoren leven de hele test.
        let mut m = unsafe { Mbox::new(pa(&mut regs), pa(&mut buf), ticking) };
        let mut big = vec![0u32; BUFFER_WORDS];
        assert!(matches!(
            m.call(&mut [Tag {
                id: 1,
                words: &mut big
            }]),
            Err(Error::TooLarge { .. })
        ));
        assert!(regs.iter().all(|&w| w == 0));
    }

    #[test]
    fn unusable_buffers_are_refused() {
        let (mut regs, _) = fake();
        for b in [0, 1, 0x18] {
            // SAFETY: de registers leven de hele test; de buffer wordt
            // geweigerd vóór iemand hem aanraakt.
            let mut m = unsafe { Mbox::new(pa(&mut regs), Pa(b), ticking) };
            assert!(matches!(m.temp(), Err(Error::Buffer { .. })));
        }
    }

    #[test]
    fn the_buffer_must_fit_in_32_bits() {
        let at = |b: u64| {
            // SAFETY: `buffer_ok` raakt registers noch buffer aan.
            unsafe { Mbox::new(Pa(0), Pa(b), ticking) }.buffer_ok()
        };
        let top = (1u64 << 32) - BUFFER_BYTES as u64;
        assert_eq!(at(0x1000), Ok(()));
        assert_eq!(at(top - 16), Ok(()));
        assert_eq!(at(top + 16), Err(Error::Buffer { pa: top + 16 }));
        assert_eq!(at(1 << 32), Err(Error::Buffer { pa: 1 << 32 }));
    }

    #[test]
    fn a_pending_request_is_never_overwritten() {
        let (mut regs, _) = fake();
        let r = pa(&mut regs);
        dev::write32(r.add(0x18), STATUS_EMPTY);
        // De buffer moet onder 4 GB liggen, en daar heeft de host geen
        // geheugen: 0x1000 is ongemapt, dus elke aanraking zou de test
        // laten crashen. Dat is de toets "buffer untouched".
        // SAFETY: de registers leven de hele test; de buffer wordt niet
        // aangeraakt (zie boven).
        let mut m = unsafe { Mbox::new(r, Pa(0x1000), ticking) };
        m.pending = 0x1008;
        assert_eq!(m.temp(), Err(Error::Timeout { stage: "reply" }));
        assert_eq!(m.pending, 0x1008);
        assert_eq!(dev::read32(r.add(0x20)), 0, "nothing published");
    }

    #[test]
    fn a_full_round_trip_with_a_fake_firmware() {
        let (mut regs, mut buf) = fake();
        let r = pa(&mut regs);
        let b = pa(&mut buf);
        dev::write32(r.add(0x18), STATUS_EMPTY);
        // SAFETY: de vectoren leven de hele test.
        let mut m = unsafe { Mbox::new(r, b, ticking) };
        let mut w = [0u32, 0];
        m.submit(&[Tag {
            id: tag::GET_TEMP,
            words: &mut w,
        }])
        .unwrap();
        let addr = (b.0 as u32) | 8;
        assert_eq!(dev::read32(r.add(0x20)), addr);
        // De firmware: respons in de buffer, het adres terug in MBOX0.
        let mut db = DevBuf(b);
        db.put(1, RESP_SUCCESS);
        db.put(4, RESP_SUCCESS | 8);
        db.put(6, 48_000);
        dev::write32(r.add(0x00), addr);
        dev::write32(r.add(0x18), 0);
        m.wait_reply(m.pending).unwrap();
        let mut tags = [Tag {
            id: tag::GET_TEMP,
            words: &mut w,
        }];
        decode(&DevBuf(b), &mut tags).unwrap();
        assert_eq!(w[1], 48_000);
    }

    #[test]
    fn notify_xhci_reset_is_linux_word_for_word_and_reads_the_reply() {
        let (mut regs, mut buf) = fake();
        let r = pa(&mut regs);
        let b = pa(&mut buf);
        dev::write32(r.add(0x18), STATUS_EMPTY);
        // SAFETY: de vectoren leven de hele test.
        let mut m = unsafe { Mbox::new(r, b, ticking) };
        let mut w = [0x0010_0000u32];
        m.submit(&[Tag {
            id: tag::NOTIFY_XHCI_RESET,
            words: &mut w,
        }])
        .unwrap();
        // Wat de VideoCore leest: maat 28, verzoek, de tag met vier bytes en
        // code 0, het adres van bus 1, de eind-tag. Linux
        // (`rpi_firmware_property`) legt precies deze zeven woorden neer.
        assert_eq!(
            words(&DevBuf(b), 7),
            [28, 0, 0x0003_0058, 4, 0, 0x0010_0000, 0]
        );
        assert_eq!(dev::read32(r.add(0x20)), (b.0 as u32) | 8);
        // De nep-firmware: gelukt, de tag met zijn respons-bit, een woord
        // terug, en het adres in MBOX0.
        let mut db = DevBuf(b);
        db.put(1, RESP_SUCCESS);
        db.put(4, RESP_SUCCESS | 4);
        db.put(5, 0);
        dev::write32(r.add(0x00), (b.0 as u32) | 8);
        dev::write32(r.add(0x18), 0);
        m.wait_reply(m.pending).unwrap();
        let mut tags = [Tag {
            id: tag::NOTIFY_XHCI_RESET,
            words: &mut w,
        }];
        decode(&DevBuf(b), &mut tags).unwrap();
        assert_eq!(w, [0]);
    }

    #[test]
    fn notify_xhci_reset_without_the_tag_bit_is_an_error() {
        // Een firmware die de tag niet kent (een oude start4.elf) laat het
        // respons-bit leeg: dat is een fout, geen stille nul.
        let mut mem = [0u64; 4];
        let mut buf = devbuf(&mut mem);
        let mut w = [0x0010_0000u32];
        encode(
            &mut buf,
            &[Tag {
                id: tag::NOTIFY_XHCI_RESET,
                words: &mut w,
            }],
        )
        .unwrap();
        buf.put(1, RESP_SUCCESS);
        let mut tags = [Tag {
            id: tag::NOTIFY_XHCI_RESET,
            words: &mut w,
        }];
        assert_eq!(
            decode(&buf, &mut tags),
            Err(Error::Tag {
                id: tag::NOTIFY_XHCI_RESET
            })
        );
    }

    #[test]
    fn mac_and_framebuffer_decoding() {
        assert_eq!(
            mac_from_words([0x33_27_eb_dc, 0x00_00_7a_12]),
            [0xdc, 0xeb, 0x27, 0x33, 0x12, 0x7a]
        );
        let fb = fb_from(
            [1920, 1080],
            16,
            [0xfe00_0000 | 0x1e00_0000, 0x7e_9000],
            3840,
        );
        assert_eq!(
            fb,
            Some(Fb {
                base: 0x3e00_0000,
                pitch: 3840,
                width: 1920,
                height: 1080,
                depth: 16
            })
        );
        assert_eq!(fb_from([1, 1], 32, [0, 0], 4), None);
    }
}
