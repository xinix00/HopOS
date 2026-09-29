//! Het videocodec-contract van HopOS: het ijzer dat een bitstream in pixels
//! omzet (of terug), losgekoppeld van welk blok dat doet (Go:
//! `OLD/metal/driver/codec`).
//!
//! De O6N heeft een Arm China Linlon V8 (`media-mve`), de Radxa een Hantro,
//! Apple een AVD: drie registerbeesten met hetzelfde gedrag. Je voert er
//! buffers in, je krijgt er buffers uit, en het ijzer vertelt onderweg wat
//! het van de stream begrepen heeft. Dit crate staat onder `driver` én
//! `kern`, om dezelfde reden als `netdev` en `blkdev`: de codec-dienst van de
//! kern hangt aan geen enkele driver.
//!
//! Het model is de "stateful" vorm: de firmware parseert de bitstream zelf.
//! Wij dragen bytes, geen NAL-units en geen referentielijsten.
//!
//! # Eigendom
//!
//! De [`Engine`] is de enige eigenaar van het ijzer en van alle
//! sessiestaat: één taak, `&mut self`, geen slot (handboek §1). Een sessie
//! is een handvat ([`Session`]), geen object met een eigen verwijzing naar
//! het device. Wie het handvat laat vallen, sluit de sessie: de `Drop` legt
//! het nummer in het [`Graveyard`] van de engine, en de engine ruimt het bij
//! zijn volgende beurt op ([`Engine::reap`]). Zo geldt "Drop is de
//! vrijgave" (handboek §1.2) ook voor een handvat dat het device zelf niet
//! kan bereiken: een app die omvalt met een open decoder houdt geen
//! hardware-sessie vast, op welk pad de kern hem ook opruimt.
//!
//! Een [`Buffer`] is fysiek geheugen dat het ijzer mag lezen of vullen. Hij
//! is van de engine vanaf `feed`/`offer` tot hij als event terugkomt
//! (handboek §1.2, DMA-eigendom); het cache-onderhoud eromheen is van wie hem
//! aanbiedt, want alleen die weet of hij gecached gemapt is (op de O6N is de
//! VPU niet coherent: `_CCA = 0` in de DSDT).

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

use core::fmt;
use core::sync::atomic::{AtomicU32, Ordering::AcqRel};

/// Het bitstream-formaat. De nummering is die van HopOS zelf en staat op de
/// draad (`abi::hopabi::codec`); elke driver vertaalt naar wat zijn ijzer
/// spreekt.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
#[repr(u8)]
pub enum Codec {
    /// Onbekend: een weigering bij open, geen paniek.
    #[default]
    Unknown = 0,
    /// H.264 / AVC.
    H264 = 1,
    /// H.265 / HEVC.
    Hevc = 2,
    /// AV1.
    Av1 = 3,
    /// VP8.
    Vp8 = 4,
    /// VP9.
    Vp9 = 5,
    /// MPEG-2.
    Mpeg2 = 6,
    /// MPEG-4 part 2.
    Mpeg4 = 7,
    /// VC-1.
    Vc1 = 8,
    /// JPEG.
    Jpeg = 9,
    /// AVS.
    Avs = 10,
    /// AVS2.
    Avs2 = 11,
    /// H.263.
    H263 = 12,
    /// RealVideo.
    Rv = 13,
}

impl Codec {
    /// Alle bekende codecs, in draadvolgorde.
    pub const ALL: [Codec; 13] = [
        Codec::H264,
        Codec::Hevc,
        Codec::Av1,
        Codec::Vp8,
        Codec::Vp9,
        Codec::Mpeg2,
        Codec::Mpeg4,
        Codec::Vc1,
        Codec::Jpeg,
        Codec::Avs,
        Codec::Avs2,
        Codec::H263,
        Codec::Rv,
    ];

    /// De codec bij een draadnummer; onbekend wordt [`Codec::Unknown`].
    #[must_use]
    pub fn from_raw(v: u8) -> Codec {
        Codec::ALL
            .iter()
            .copied()
            .find(|c| *c as u8 == v)
            .unwrap_or(Codec::Unknown)
    }

    /// De korte naam zoals hij in logs, config en firmwarenamen staat.
    #[must_use]
    pub const fn name(self) -> &'static str {
        match self {
            Codec::Unknown => "unknown",
            Codec::H264 => "h264",
            Codec::Hevc => "hevc",
            Codec::Av1 => "av1",
            Codec::Vp8 => "vp8",
            Codec::Vp9 => "vp9",
            Codec::Mpeg2 => "mpeg2",
            Codec::Mpeg4 => "mpeg4",
            Codec::Vc1 => "vc1",
            Codec::Jpeg => "jpeg",
            Codec::Avs => "avs",
            Codec::Avs2 => "avs2",
            Codec::H263 => "h263",
            Codec::Rv => "rv",
        }
    }

    /// De codec bij een naam (jobspec, bootparameter); onbekend wordt
    /// [`Codec::Unknown`].
    #[must_use]
    pub fn parse(name: &str) -> Codec {
        Codec::ALL
            .iter()
            .copied()
            .find(|c| c.name() == name)
            .unwrap_or(Codec::Unknown)
    }

    /// De codec bij de extensie van een bestandsnaam (Go: `codecFromName`
    /// in `codecdemo.go`): `.hevc`/`.265`, `.h264`/`.264`, `.av1`, ... Een
    /// kale elementaire stream draagt geen kop die zegt wat hij is; de naam
    /// is de enige aanwijzing. Onbekend wordt [`Codec::Unknown`].
    #[must_use]
    pub fn from_file_name(path: &str) -> Codec {
        let Some((_, ext)) = path.rsplit_once('.') else {
            return Codec::Unknown;
        };
        let is = |names: &[&str]| names.iter().any(|n| ext.eq_ignore_ascii_case(n));
        if is(&["h264", "264", "avc"]) {
            Codec::H264
        } else if is(&["hevc", "265", "h265"]) {
            Codec::Hevc
        } else if is(&["av1", "obu"]) {
            Codec::Av1
        } else if is(&["vp9"]) {
            Codec::Vp9
        } else if is(&["vp8"]) {
            Codec::Vp8
        } else if is(&["mpeg2", "m2v"]) {
            Codec::Mpeg2
        } else if is(&["vc1"]) {
            Codec::Vc1
        } else if is(&["jpg", "jpeg", "mjpeg"]) {
            Codec::Jpeg
        } else {
            Codec::Unknown
        }
    }
}

impl fmt::Display for Codec {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.name())
    }
}

/// Een ongecomprimeerd frame-formaat: alleen wat een transcode-keten
/// werkelijk nodig heeft. Blu-ray is NV12 (AVC, VC-1, MPEG-2) of P010 (UHD,
/// 10-bit HDR).
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
#[repr(u8)]
pub enum Pixel {
    /// Geen formaat.
    #[default]
    None = 0,
    /// Y plus verweven CbCr, 8 bit.
    Nv12 = 1,
    /// Y plus verweven CrCb, 8 bit.
    Nv21 = 2,
    /// Y, Cb, Cr, 8 bit.
    I420 = 3,
    /// Y plus verweven CbCr, 10 bit in 16-bit woorden.
    P010 = 4,
    /// Alleen luma.
    Y8 = 5,
}

impl Pixel {
    /// Het formaat bij een draadnummer; onbekend wordt [`Pixel::None`].
    #[must_use]
    pub const fn from_raw(v: u8) -> Pixel {
        match v {
            1 => Pixel::Nv12,
            2 => Pixel::Nv21,
            3 => Pixel::I420,
            4 => Pixel::P010,
            5 => Pixel::Y8,
            _ => Pixel::None,
        }
    }

    /// De korte naam.
    #[must_use]
    pub const fn name(self) -> &'static str {
        match self {
            Pixel::None => "none",
            Pixel::Nv12 => "nv12",
            Pixel::Nv21 => "nv21",
            Pixel::I420 => "i420",
            Pixel::P010 => "p010",
            Pixel::Y8 => "y8",
        }
    }

    /// Het formaat bij een naam (bootparameter, env van een app); onbekend
    /// wordt [`Pixel::None`].
    #[must_use]
    pub fn parse(name: &str) -> Pixel {
        [
            Pixel::Nv12,
            Pixel::Nv21,
            Pixel::I420,
            Pixel::P010,
            Pixel::Y8,
        ]
        .into_iter()
        .find(|p| p.name().eq_ignore_ascii_case(name))
        .unwrap_or(Pixel::None)
    }
}

impl fmt::Display for Pixel {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.name())
    }
}

/// Wat een sessie doet.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
#[repr(u8)]
pub enum Direction {
    /// Bitstream in, frames uit.
    #[default]
    Decode = 0,
    /// Frames in, bitstream uit.
    Encode = 1,
}

impl Direction {
    /// De richting bij een draadnummer: alles behalve 1 is decode, zoals in
    /// Go.
    #[must_use]
    pub const fn from_raw(v: u8) -> Direction {
        if v == 1 {
            Direction::Encode
        } else {
            Direction::Decode
        }
    }
}

impl fmt::Display for Direction {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Direction::Decode => "decode",
            Direction::Encode => "encode",
        })
    }
}

/// Opent een sessie. Bij decode is `pixel` het gewenste uitvoerformaat en
/// zijn `width`/`height` hooguit een hint: de stream bepaalt de maat, en die
/// komt terug als [`Kind::Format`]. Bij encode zijn ze verplicht.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Config {
    /// Het bitstream-formaat.
    pub codec: Codec,
    /// De richting.
    pub dir: Direction,
    /// Het pixelformaat.
    pub pixel: Pixel,
    /// De breedte (hint bij decode).
    pub width: u32,
    /// De hoogte (hint bij decode).
    pub height: u32,
}

/// Een aaneengesloten stuk fysiek geheugen dat het codec-ijzer mag lezen of
/// vullen. Fysiek, want het ijzer DMA't erin: wie hem aanbiedt staat ervoor
/// in dat hij niet verhuist en dat de cache-staat klopt.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Buffer {
    /// Het fysieke basisadres.
    pub pa: u64,
    /// De maat in bytes.
    pub size: u64,
}

/// De vlaggen bij een ingevoerde buffer.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Flags(pub u32);

impl Flags {
    /// Het einde van de stream: het ijzer loopt zijn interne vertraging leeg
    /// en sluit af met [`Kind::Done`]. Een decoder houdt frames vast
    /// (herordening); zonder dit blijven de laatste binnen.
    pub const EOS: Flags = Flags(1);
    /// Alleen codec-configuratie (VPS/SPS/PPS van de container); geen frame.
    pub const HEADERS: Flags = Flags(1 << 1);
    /// Bij encode: een IDR op dit frame.
    pub const KEY_FRAME: Flags = Flags(1 << 2);

    /// Staat `f` aan?
    #[must_use]
    pub const fn has(self, f: Flags) -> bool {
        self.0 & f.0 != 0
    }
}

/// Eén vlak van een frame binnen een [`Buffer`].
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Plane {
    /// De afstand vanaf `Buffer::pa`.
    pub off: u64,
    /// Bytes per regel; 0 = dit vlak bestaat niet.
    pub stride: u32,
}

/// Wat het ijzer van de stream begrepen heeft: hoe groot een uitvoerbuffer
/// moet zijn, hoeveel het er tegelijk nodig heeft en waar de vlakken liggen.
/// Komt als [`Kind::Format`] zodra de headers gelezen zijn, en opnieuw bij
/// een resolutiewissel.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Layout {
    /// Zichtbaar beeld.
    pub width: u32,
    /// Zichtbaar beeld.
    pub height: u32,
    /// Wat een buffer moet kunnen dragen (macroblok-afronding).
    pub alloc_width: u32,
    /// Idem.
    pub alloc_height: u32,
    /// Het pixelformaat.
    pub pixel: Pixel,
    /// De vlakken.
    pub planes: [Plane; 3],
    /// De minimale buffermaat voor één frame.
    pub frame_size: u64,
    /// Hoeveel buffers het ijzer tegelijk vasthoudt.
    pub min_buffers: u32,
}

/// Het soort event.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum Kind {
    /// Een ingevoerde buffer is verwerkt en weer van de aanroeper.
    Consumed = 1,
    /// Een aangeboden buffer bevat nu een frame (decode) of bitstream.
    Produced = 2,
    /// [`Layout`] is bekend of veranderd.
    Format = 3,
    /// De stream is afgelopen (na een EOS-invoer).
    Done = 4,
    /// Het ijzer meldt een fout; de sessie is verloren.
    Fault = 5,
}

/// Eén ding dat er in een sessie gebeurde.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Event {
    /// Het soort.
    pub kind: Kind,
    /// De buffer waar het over gaat (consumed, produced, en een lege laatste
    /// buffer die met Done meereist).
    pub buf: Option<Buffer>,
    /// Wat de aanroeper bij `feed` meegaf: zo vindt hij zijn eigen
    /// tijdstempel terug.
    pub tag: u64,
    /// Produced: gevulde bytes (encode) of framegrootte (decode); 0 = niets
    /// bruikbaars (een decode-only beeld).
    pub bytes: u64,
    /// Produced bij encode: een keyframe.
    pub key: bool,
    /// Format, en meegegeven bij produced na decode.
    pub layout: Layout,
    /// Fault: waarom.
    pub fault: Option<Error>,
}

impl Event {
    /// Een kaal event van soort `kind`.
    #[must_use]
    pub const fn of(kind: Kind) -> Event {
        Event {
            kind,
            buf: None,
            tag: 0,
            bytes: 0,
            key: false,
            layout: Layout {
                width: 0,
                height: 0,
                alloc_width: 0,
                alloc_height: 0,
                pixel: Pixel::None,
                planes: [Plane { off: 0, stride: 0 }; 3],
                frame_size: 0,
                min_buffers: 0,
            },
            fault: None,
        }
    }
}

/// Waarom een codec-verzoek niet lukte. Elke variant draagt de getallen
/// (handboek §6): "firmware error" zonder code is op een headless node
/// niets waard.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Error {
    /// Alle hardware-sessies zijn bezet: straks nog eens.
    Busy,
    /// Deze codec en richting kan dit ijzer niet.
    Unsupported,
    /// De sessie is dicht (of het handvat is van een andere engine).
    Closed,
    /// Het pixelformaat kan dit ijzer niet leveren.
    Pixel(Pixel),
    /// Geen firmware voor deze codec op het volume.
    NoFirmware,
    /// De firmware-blob klopt niet; `at` is het kopveld dat faalde.
    BadFirmware {
        /// De offset van het veld in de kop.
        at: u32,
        /// De waarde die er stond.
        got: u32,
    },
    /// Het ijzer is niet wat de driver kent.
    Hardware {
        /// Het HARDWARE_ID-register.
        id: u32,
    },
    /// Een onmogelijke geometrie (cores, sessies).
    Geometry {
        /// Cores.
        cores: u32,
        /// Sessies.
        sessions: u32,
    },
    /// Een buffer die geen hele pagina's is of niet in de adresruimte past.
    Buffer {
        /// Het fysieke adres.
        pa: u64,
        /// De maat.
        size: u64,
    },
    /// De adresruimte van de firmware is vol.
    AddressSpace {
        /// Gevraagde pagina's.
        pages: u64,
    },
    /// De arena van de driver is vol.
    ArenaFull {
        /// Gevraagde pagina's.
        pages: u64,
    },
    /// Een adres buiten de arena of een onzinnige uitlijning.
    ArenaRange {
        /// Het adres of de uitlijning.
        at: u64,
    },
    /// De ring naar de firmware is vol.
    QueueFull,
    /// Een ringpositie buiten de ring (de firmware schreef onzin).
    QueuePos {
        /// De positie.
        pos: u32,
    },
    /// Een bericht groter dan de ring of de ontvangstbuffer.
    MsgTooBig {
        /// De lengte in bytes.
        len: u32,
    },
    /// Een berichtcode buiten het protocol.
    MsgUnknown {
        /// De code.
        code: u16,
    },
    /// De firmware meldde een fout.
    Firmware {
        /// De foutcode van de firmware.
        code: u32,
    },
    /// De firmware kan deze stream niet decoderen.
    StreamUnsupported,
    /// De decoder gaf een corrupt beeld terug.
    CorruptFrame,
    /// De firmware weigerde een optie.
    OptionRejected,
    /// Een geheugenverzoek van de firmware liep vast.
    RpcOom {
        /// Megabytes gevraagd.
        mb: u32,
        /// Wat er op was: 0 adresruimte, 1 arena, 2 page tables, 3 de
        /// gereserveerde span.
        what: u8,
    },
    /// De firmware vroeg een uitlijning van 2^n boven 2^31.
    Alignment {
        /// De exponent.
        log2: u8,
    },
    /// Een hardware-sessie wil niet afbreken.
    Terminate {
        /// Het hardware-slot.
        lsid: u8,
    },
    /// Een vaste tabel is vol.
    Full {
        /// De capaciteit.
        cap: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Error::Busy => f.write_str("codec: all hardware sessions in use"),
            Error::Unsupported => {
                f.write_str("codec: codec/direction not supported by this hardware")
            }
            Error::Closed => f.write_str("codec: session closed"),
            Error::Pixel(p) => write!(f, "codec: pixel format {p} not supported"),
            Error::NoFirmware => f.write_str("codec: no firmware for this codec"),
            Error::BadFirmware { at, got } => {
                write!(f, "codec: bad firmware header field at {at}: {got:#x}")
            }
            Error::Hardware { id } => {
                write!(f, "codec: hardware id {id:#x} (model {:#x})", id >> 16)
            }
            Error::Geometry { cores, sessions } => write!(
                f,
                "codec: implausible geometry: {cores} cores, {sessions} sessions"
            ),
            Error::Buffer { pa, size } => {
                write!(
                    f,
                    "codec: buffer {pa:#x}+{size} is not usable as whole pages"
                )
            }
            Error::AddressSpace { pages } => write!(
                f,
                "codec: {pages} pages do not fit in the firmware address space"
            ),
            Error::ArenaFull { pages } => {
                write!(f, "codec: no {pages} contiguous pages left in the arena")
            }
            Error::ArenaRange { at } => write!(f, "codec: {at:#x} outside the arena"),
            Error::QueueFull => f.write_str("codec: firmware queue full"),
            Error::QueuePos { pos } => write!(f, "codec: queue position {pos} out of range"),
            Error::MsgTooBig { len } => write!(f, "codec: message of {len} bytes too large"),
            Error::MsgUnknown { code } => write!(f, "codec: message code {code} out of range"),
            Error::Firmware { code } => write!(f, "codec: firmware error {code}"),
            Error::StreamUnsupported => f.write_str("codec: firmware cannot decode this stream"),
            Error::CorruptFrame => f.write_str("codec: decoder returned a corrupt frame"),
            Error::OptionRejected => f.write_str("codec: firmware rejected an option"),
            Error::RpcOom { mb, what } => {
                let what = match what {
                    0 => "virtual address space",
                    1 => "arena",
                    2 => "page tables",
                    _ => "the reserved span",
                };
                write!(f, "codec: firmware asked for {mb} MB and {what} ran out")
            }
            Error::Alignment { log2 } => {
                write!(f, "codec: firmware asked for 2^{log2} alignment")
            }
            Error::Terminate { lsid } => {
                write!(f, "codec: session slot {lsid} will not terminate")
            }
            Error::Full { cap } => write!(f, "codec: table of {cap} is full"),
        }
    }
}

/// Het resultaat van een codec-verzoek.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// De bak waarin een gevallen [`Session`] zijn nummer legt. Eén bit per
/// hardware-sessie; de engine haalt ze leeg bij zijn volgende beurt.
///
/// Een atomic en geen tabel: het handvat kan vallen waar de engine niet bij
/// is (een verbindingstaak, de `Drop` van een levensduur), en twee bits die
/// los iets betekenen zijn geen protocol (handboek §1.3).
#[derive(Debug, Default)]
pub struct Graveyard(AtomicU32);

impl Graveyard {
    /// Een lege bak, voor in een `static`.
    #[must_use]
    pub const fn new() -> Graveyard {
        Graveyard(AtomicU32::new(0))
    }

    fn bury(&self, id: u8) {
        if let Some(bit) = 1u32.checked_shl(u32::from(id)) {
            self.0.fetch_or(bit, AcqRel);
        }
    }

    /// Haalt alle begraven nummers op (bit `i` = sessie `i`).
    pub fn take(&self) -> u32 {
        self.0.swap(0, AcqRel)
    }
}

/// Een handvat op één lopende sessie. Geen `Clone`: er is precies één
/// eigenaar, en wie hem laat vallen sluit de sessie (via het
/// [`Graveyard`]).
#[derive(Debug)]
pub struct Session {
    id: u8,
    graves: &'static Graveyard,
}

impl Session {
    /// Een handvat op hardware-sessie `id` (hoogstens 31) van de engine met
    /// deze bak. Alleen een engine maakt er een.
    #[must_use]
    pub fn new(id: u8, graves: &'static Graveyard) -> Session {
        Session { id, graves }
    }

    /// Het nummer van de hardware-sessie.
    #[must_use]
    pub fn id(&self) -> u8 {
        self.id
    }

    /// Is dit handvat van de engine met deze bak? Een handvat van een
    /// ander blok is voor deze engine dicht.
    #[must_use]
    pub fn is_of(&self, graves: &Graveyard) -> bool {
        core::ptr::eq(self.graves, graves)
    }

    /// Ontmantelt het handvat zonder te begraven: voor een engine die de
    /// sessie nu meteen zelf sluit ([`Engine::close`]).
    #[must_use]
    pub fn defuse(self) -> u8 {
        let id = self.id;
        core::mem::forget(self);
        id
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        self.graves.bury(self.id);
    }
}

/// Levert de firmware-blob die sommige codec-blokken nodig hebben (de Linlon
/// V8 laadt een eigen binary per codec, zo'n 300 KB). De drivers kennen geen
/// bestandssysteem; de kern hangt hier zijn eigen bron onder (de blobs van
/// het volume, in RAM gelezen bij `codec_up`).
pub trait Firmware {
    /// De binary voor een naam als `hevcdec` of `h264enc`.
    fn load(&mut self, name: &str) -> Option<&[u8]>;
}

/// Het codec-ijzer van een node: de enige eigenaar van zijn sessies.
///
/// Alle methodes zijn non-blocking: het ijzer werkt asynchroon en
/// [`Engine::next_event`] is de enige plek waar voortgang vandaan komt. Elke
/// methode ruimt eerst de begraven sessies op.
pub trait Engine {
    /// Eén regel voor de bootlog (wat, welke revisie, hoeveel sessies).
    fn describe(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result;

    /// Kan deze combinatie open?
    fn supports(&self, codec: Codec, dir: Direction) -> bool;

    /// Start een sessie. [`Error::Busy`] is geen fout maar een "straks".
    fn open(&mut self, cfg: &Config) -> Result<Session>;

    /// Voert `filled` bytes uit `buf` in. De buffer is van het ijzer tot hij
    /// als [`Kind::Consumed`] terugkomt.
    fn feed(&mut self, s: &Session, buf: Buffer, filled: u64, flags: Flags, tag: u64) -> Result;

    /// Biedt een lege buffer aan voor het resultaat. Een decoder heeft er
    /// [`Layout::min_buffers`] tegelijk nodig; minder is stilstand, geen
    /// verlies.
    fn offer(&mut self, s: &Session, buf: Buffer) -> Result;

    /// Het volgende event, of `None`: niets te melden.
    fn next_event(&mut self, s: &Session) -> Option<Event>;

    /// Sluit de sessie nu. Aangeboden buffers zijn daarna weer van de
    /// aanroeper en komen nooit meer als event terug.
    fn close(&mut self, s: Session);

    /// Sluit alle sessies waarvan het handvat viel.
    fn reap(&mut self);

    /// De hardwarestaat, voor diagnose: als er niets uit een sessie komt,
    /// is de eerste vraag of het ijzer het werk aannam.
    fn state(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("no state")
    }
}

/// Maakt van een `fn(&self, &mut Formatter)` een `Display`, voor
/// [`Engine::describe`] en [`Engine::state`] in een logregel.
pub struct Show<'a, E: ?Sized>(
    pub &'a E,
    pub fn(&E, &mut fmt::Formatter<'_>) -> fmt::Result,
);

impl<E: ?Sized> fmt::Display for Show<'_, E> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        (self.1)(self.0, f)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_roundtrip_and_wire_numbers_are_those_of_go() {
        for c in Codec::ALL {
            assert_eq!(Codec::parse(c.name()), c);
            assert_eq!(Codec::from_raw(c as u8), c);
        }
        assert_eq!(Codec::parse("mkv"), Codec::Unknown);
        assert_eq!(Codec::from_raw(99), Codec::Unknown);
        // Go: `Unknown Codec = iota; H264; HEVC; ...; RV` en `NoPixel; NV12;
        // NV21; I420; P010; Y8`. De draad draagt deze getallen.
        assert_eq!(Codec::Hevc as u8, 2);
        assert_eq!(Codec::Rv as u8, 13);
        assert_eq!(Pixel::P010 as u8, 4);
        assert_eq!(Pixel::from_raw(5), Pixel::Y8);
        assert_eq!(Pixel::from_raw(6), Pixel::None);
        assert_eq!(Direction::from_raw(1), Direction::Encode);
        assert_eq!(Direction::from_raw(7), Direction::Decode);
        assert_eq!(Flags::EOS.0 | Flags::HEADERS.0 | Flags::KEY_FRAME.0, 7);
    }

    /// De extensies van Go's `codecFromName`, plus de pixelnamen van de
    /// bootparameter en de env.
    #[test]
    fn the_codec_comes_from_the_file_name() {
        assert_eq!(Codec::from_file_name("/data/clip.hevc"), Codec::Hevc);
        assert_eq!(Codec::from_file_name("x.265"), Codec::Hevc);
        assert_eq!(Codec::from_file_name("x.H264"), Codec::H264);
        assert_eq!(Codec::from_file_name("a.b/film.avc"), Codec::H264);
        assert_eq!(Codec::from_file_name("x.obu"), Codec::Av1);
        assert_eq!(Codec::from_file_name("x.m2v"), Codec::Mpeg2);
        assert_eq!(Codec::from_file_name("x.jpeg"), Codec::Jpeg);
        assert_eq!(Codec::from_file_name("x.mkv"), Codec::Unknown);
        assert_eq!(Codec::from_file_name("clip"), Codec::Unknown);
        assert_eq!(Pixel::parse("p010"), Pixel::P010);
        assert_eq!(Pixel::parse("NV12"), Pixel::Nv12);
        assert_eq!(Pixel::parse("rgb"), Pixel::None);
    }

    #[test]
    fn a_dropped_session_lands_in_the_graveyard_a_defused_one_not() {
        let g: &'static Graveyard = Box::leak(Box::new(Graveyard::new()));
        let other: &'static Graveyard = Box::leak(Box::new(Graveyard::new()));
        let a = Session::new(3, g);
        let b = Session::new(5, g);
        assert!(a.is_of(g) && !a.is_of(other));
        drop(a);
        assert_eq!(b.defuse(), 5);
        assert_eq!(g.take(), 1 << 3);
        assert_eq!(g.take(), 0);
        // Een onmogelijk nummer begraaft niets in plaats van te panikeren.
        drop(Session::new(40, g));
        assert_eq!(g.take(), 0);
    }

    #[test]
    fn errors_carry_their_numbers() {
        let s = format!("{}", Error::RpcOom { mb: 24, what: 1 });
        assert!(s.contains("24 MB") && s.contains("arena"), "{s}");
        let s = format!("{}", Error::Hardware { id: 0x5650_0000 });
        assert!(s.contains("0x5650"), "{s}");
    }
}
