//! De control-page van een slot en de payload van een system-call.
//!
//! **De control-page** ([`CtrlPage`]) is de eerste pagina van de staart
//! ([`crate::layout::Tail::ctrl_page`]): 64-bit woorden in de kop, een
//! env-blob in het midden, en woorden die later kwamen bovenaan de page,
//! naar beneden groeiend. Elk veld heeft één schrijver; die staat bij het
//! veld. De offsets zijn byte voor byte die van Go-ABI 10 (Go telde door tot
//! 10; deze crate begint opnieuw bij 1, zie [`crate::ABI_VERSION`]).
//!
//! **De call-payload** ([`Req`], [`Resp`]) is wat een `Call`- of
//! `Result`-frame van [`crate::systemapi`] draagt: een kop van 24 bytes
//! (little-endian) plus pad en data.
//!
//! ```text
//! req:  ver u8 | op u8 | path_len u16 | seq u32 | off u64 | n u64 | path | data
//! resp: ver u8 | op u8 | status u16   | seq u32 | size u64 | _ u64 | data
//! ```
//!
//! Stateless (paden, geen fd's): een app-crash laat bij de kern niets
//! achter.
//!
//! Wat hier NIET staat: de codec- en device-payloads van Go (`codec.go`,
//! `device.go`); die horen bij de features `media` en `gui` en komen met
//! hun drivers. De opcodes staan er wel, zodat hun nummers bezet blijven.

use crate::{Error, Result};
use core::mem::{offset_of, size_of};

// ---------------------------------------------------------------------------
// De control-page.
// ---------------------------------------------------------------------------

/// App: de status ([`AppStatus`]).
pub const CTRL_STATUS: u64 = 0x00;
/// App: de exitcode, gezet bij exit.
pub const CTRL_EXIT_CODE: u64 = 0x08;
/// Kern naar app: 1 is "stop jezelf" (coöperatief).
pub const CTRL_KILL: u64 = 0x10;
/// App: een oplopende teller, voor hang-detectie.
pub const CTRL_HEARTBEAT: u64 = 0x18;
/// App: de eigen RAM-maat, als bewijs dat de patch aankwam.
pub const CTRL_RAM_SIZE: u64 = 0x20;
/// Kern naar app: de lengte van de env-blob in bytes.
pub const CTRL_ENV_LEN: u64 = 0x28;
/// Kern naar trampoline: de app-entry (EL1) voor de ERET.
pub const CTRL_ENTRY: u64 = 0x30;
/// Kern naar trampoline: het fysieke adres van de stage-2-L1-tabel.
pub const CTRL_S2_TABLE: u64 = 0x38;
/// Kern naar app: de klok-offset (wall-ns bij tellerstand 0, `i64` als
/// bits; 0 = geen klok). De teller is gedeeld over alle cores, dus de
/// offset van de kern geldt exact voor elke app.
pub const CTRL_WALL_OFF: u64 = 0x40;
/// App naar kern: geaccumuleerde idle-TIJD in timer-tikken. Sinds 18-07
/// tijd in plaats van rondes: rondes bleken op ijzer door SEV-ruis
/// opgeblazen. Stond op 0xD8 en botste daar met [`CTRL_SMP_TCR`]; de
/// uniekheidstoets bewaakt dat voortaan.
pub const CTRL_IDLE: u64 = 0x48;
/// Kern naar trampoline: de fysieke basis van de EL2-vectoren.
pub const CTRL_VEC_PA: u64 = 0x50;
/// Vector: ESR_EL2 van de fault die het slot deed vallen.
pub const CTRL_FAULT_ESR: u64 = 0x58;
/// Vector: FAR_EL2, het faultadres.
pub const CTRL_FAULT_FAR: u64 = 0x60;
/// Vector: vectorindex plus 1 (0 = geen fault gezien); zie [`FAULT_SYNC`].
pub const CTRL_FAULT_VEC: u64 = 0x68;
/// Kern naar app: het aantal cores (1 = geen SMP).
pub const CTRL_CORES: u64 = 0x70;
/// De actieve VBAR_EL1 van de dispatchende primaire, voor een secundaire.
pub const CTRL_SMP_VBAR: u64 = 0x78;
/// Kern naar app: het fysieke adres van de EL2-SMP-trampoline.
pub const CTRL_SMP_TRAMP: u64 = 0x80;
/// App naar secundaire: de stacktop (IPA).
pub const CTRL_SMP_SP: u64 = 0x88;
/// App naar secundaire: eerste runtime-argument (Go: `*m`).
pub const CTRL_SMP_MP: u64 = 0x90;
/// App naar secundaire: tweede runtime-argument (Go: `g0`).
pub const CTRL_SMP_G0: u64 = 0x98;
/// App naar secundaire: de entry (IPA).
pub const CTRL_SMP_FN: u64 = 0xA0;
/// App naar trampoline: de EL1-stub waar de secundaire heen ERET't.
pub const CTRL_SMP_STUB: u64 = 0xA8;
/// App naar secundaire: de stage-1-L1-tabel, zodat de stub geen geheugen
/// leest vóór zijn MMU aan staat (een pre-MMU-lees kan stale zijn).
pub const CTRL_SMP_TTBR0: u64 = 0xB0;
/// Kern naar trampoline: het slotnummer (= VMID).
pub const CTRL_SLOT: u64 = 0xB8;
/// App naar kern: de core-index die de runtime als extra SMP-core wil
/// (0 = geen verzoek). De kern valideert tegen [`CTRL_CORES`].
pub const CTRL_SMP_REQ: u64 = 0xC0;
/// Kern naar trampoline: de park-mailbox van déze core.
pub const CTRL_MBOX_PA: u64 = 0xC8;
/// Kern naar trampoline: de park-mailbox van de secundaire.
pub const CTRL_SMP_MBOX: u64 = 0xD0;
/// De actieve TCR_EL1 van de primaire, voor een secundaire (gelezen van de
/// levende registers, geen afgeleide kopie: de 39-bit-standaard kon de
/// Altra-UART op 16 TB niet vertalen, gemeten 17-07).
pub const CTRL_SMP_TCR: u64 = 0xD8;
/// Apploader naar kern: de maat van het gestagede image.
pub const CTRL_STAGED_SIZE: u64 = 0xE0;
/// App naar kern: de werkelijke geheugen-draw van de runtime (0 = nog niet
/// gerapporteerd).
pub const CTRL_MEM_SYS: u64 = 0xE8;
/// Apploader naar kern: de IPA van het zelfplaatsings-stubje (0 = geen).
/// Niet vertrouwd voor isolatie: het draait ín de kooi.
pub const CTRL_PLACE_ENTRY: u64 = 0xF0;
/// De actieve MAIR_EL1 van de primaire, voor een secundaire.
pub const CTRL_SMP_MAIR: u64 = 0xF8;
/// Kern naar app: 1 als dit slot zijn core deelt; de idle-governor yieldt
/// dan naar de switcher in plaats van WFE te slapen.
pub const CTRL_SHARED: u64 = 0x100;
/// App naar kern: het aantal idle-rondes. Bewust ongelezen (besluit Derek
/// 06-08) tot een onverklaarbaar hoog cpu-percentage erom vraagt.
pub const CTRL_WAKES: u64 = 0x108;
/// App naar switcher: de wek-drempel van de doorbell (head | bit 63). Alleen
/// wie de ring draint mag hem wapenen: anders maakt elke ARP-flood een app
/// zonder netstack permanent "due".
pub const CTRL_RX_DOOR: u64 = 0x110;
/// App naar kern: 1 is "maak van mijn doorbell een vFIQ". Alleen voor een
/// app met één core: HCR_EL2 is per core.
pub const CTRL_DOOR_IRQ: u64 = 0x118;
/// De env-blob: `key=val\n`-bytes die de kern schrijft en de app bij start
/// inleest.
pub const CTRL_ENV_DATA: u64 = 0x120;
/// Kern naar app: hoe de cores van dit slot idlen ([`IDLE_YIELD`]; 0 = wat
/// de architectuur zelf doet). Bovenaan de page: woorden die ná de env
/// kwamen, groeien naar beneden, zodat de env niet meer verschuift.
pub const CTRL_IDLE_MODE: u64 = 0xFF8;
/// De ruimte voor de env-blob.
pub const CTRL_ENV_MAX: u64 = CTRL_IDLE_MODE - CTRL_ENV_DATA;

/// De bit in [`CTRL_RX_DOOR`] die de drempel wapent; een byte-index haalt
/// dat bit nooit.
pub const RX_DOOR_ARMED: u64 = 1 << 63;

/// [`CTRL_IDLE_MODE`]: idle is een yield naar EL2 (HVC #1), ook met een
/// eigen core. Zo idlet Apple silicon: op de M4 slaapt een app-core op EL1
/// niet (gemeten 02-09). De kern zet dit niet op een SMP-slot.
pub const IDLE_YIELD: u64 = 1;

/// [`CTRL_FAULT_VEC`]: geen fault gezien sinds de laatste start.
pub const FAULT_NONE: u64 = 0;
/// [`CTRL_FAULT_VEC`]: synchroon vanuit EL1 (index 8), een stage-2-fault;
/// ESR en FAR zijn geldig. Een kooi-overtreding en een hard-kill landen
/// allebei hier.
pub const FAULT_SYNC: u64 = 9;

/// [`CTRL_KILL`]: stop jezelf.
pub const KILL_STOP: u64 = 1;

/// De status van een app op de control-page ([`CTRL_STATUS`]).
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
#[repr(u64)]
pub enum AppStatus {
    /// De kern heeft de page geveegd.
    Empty = 0,
    /// De kern heeft de core gestart; de runtime is nog niet klaar.
    Booting = 1,
    /// De runtime draait (gezet door de app).
    Ready = 2,
    /// De app is gestopt; de exitcode staat in [`CTRL_EXIT_CODE`].
    Exited = 3,
    /// De apploader heeft het echte image gestaged en geparkeerd.
    Staged = 4,
}

impl AppStatus {
    /// De status van een rauw woord, of `None` voor een onbekende waarde.
    #[must_use]
    pub const fn from_raw(v: u64) -> Option<AppStatus> {
        Some(match v {
            0 => Self::Empty,
            1 => Self::Booting,
            2 => Self::Ready,
            3 => Self::Exited,
            4 => Self::Staged,
            _ => return None,
        })
    }

    /// Het rauwe woord.
    #[must_use]
    pub const fn raw(self) -> u64 {
        self as u64
    }
}

/// De indeling van de control-page, als type: de offsets hierboven zijn de
/// velden van deze struct, en de asserties eronder houden ze byte voor
/// byte gelijk. Een botsing zoals `CtrlSMPTcr` en `CtrlIdle` op 0xD8
/// (18-07) is zo een compilefout.
#[repr(C)]
#[derive(Debug)]
pub struct CtrlPage {
    /// [`CTRL_STATUS`].
    pub status: u64,
    /// [`CTRL_EXIT_CODE`].
    pub exit_code: u64,
    /// [`CTRL_KILL`].
    pub kill: u64,
    /// [`CTRL_HEARTBEAT`].
    pub heartbeat: u64,
    /// [`CTRL_RAM_SIZE`].
    pub ram_size: u64,
    /// [`CTRL_ENV_LEN`].
    pub env_len: u64,
    /// [`CTRL_ENTRY`].
    pub entry: u64,
    /// [`CTRL_S2_TABLE`].
    pub s2_table: u64,
    /// [`CTRL_WALL_OFF`].
    pub wall_off: u64,
    /// [`CTRL_IDLE`].
    pub idle: u64,
    /// [`CTRL_VEC_PA`].
    pub vec_pa: u64,
    /// [`CTRL_FAULT_ESR`].
    pub fault_esr: u64,
    /// [`CTRL_FAULT_FAR`].
    pub fault_far: u64,
    /// [`CTRL_FAULT_VEC`].
    pub fault_vec: u64,
    /// [`CTRL_CORES`].
    pub cores: u64,
    /// [`CTRL_SMP_VBAR`].
    pub smp_vbar: u64,
    /// [`CTRL_SMP_TRAMP`].
    pub smp_tramp: u64,
    /// [`CTRL_SMP_SP`].
    pub smp_sp: u64,
    /// [`CTRL_SMP_MP`].
    pub smp_mp: u64,
    /// [`CTRL_SMP_G0`].
    pub smp_g0: u64,
    /// [`CTRL_SMP_FN`].
    pub smp_fn: u64,
    /// [`CTRL_SMP_STUB`].
    pub smp_stub: u64,
    /// [`CTRL_SMP_TTBR0`].
    pub smp_ttbr0: u64,
    /// [`CTRL_SLOT`].
    pub slot: u64,
    /// [`CTRL_SMP_REQ`].
    pub smp_req: u64,
    /// [`CTRL_MBOX_PA`].
    pub mbox_pa: u64,
    /// [`CTRL_SMP_MBOX`].
    pub smp_mbox: u64,
    /// [`CTRL_SMP_TCR`].
    pub smp_tcr: u64,
    /// [`CTRL_STAGED_SIZE`].
    pub staged_size: u64,
    /// [`CTRL_MEM_SYS`].
    pub mem_sys: u64,
    /// [`CTRL_PLACE_ENTRY`].
    pub place_entry: u64,
    /// [`CTRL_SMP_MAIR`].
    pub smp_mair: u64,
    /// [`CTRL_SHARED`].
    pub shared: u64,
    /// [`CTRL_WAKES`].
    pub wakes: u64,
    /// [`CTRL_RX_DOOR`].
    pub rx_door: u64,
    /// [`CTRL_DOOR_IRQ`].
    pub door_irq: u64,
    /// [`CTRL_ENV_DATA`].
    pub env: [u8; CTRL_ENV_MAX as usize],
    /// [`CTRL_IDLE_MODE`].
    pub idle_mode: u64,
}

/// Alle woord-offsets van de page, voor de uniekheidstoets.
pub const CTRL_WORDS: [u64; 37] = [
    CTRL_STATUS,
    CTRL_EXIT_CODE,
    CTRL_KILL,
    CTRL_HEARTBEAT,
    CTRL_RAM_SIZE,
    CTRL_ENV_LEN,
    CTRL_ENTRY,
    CTRL_S2_TABLE,
    CTRL_WALL_OFF,
    CTRL_IDLE,
    CTRL_VEC_PA,
    CTRL_FAULT_ESR,
    CTRL_FAULT_FAR,
    CTRL_FAULT_VEC,
    CTRL_CORES,
    CTRL_SMP_VBAR,
    CTRL_SMP_TRAMP,
    CTRL_SMP_SP,
    CTRL_SMP_MP,
    CTRL_SMP_G0,
    CTRL_SMP_FN,
    CTRL_SMP_STUB,
    CTRL_SMP_TTBR0,
    CTRL_SLOT,
    CTRL_SMP_REQ,
    CTRL_MBOX_PA,
    CTRL_SMP_MBOX,
    CTRL_SMP_TCR,
    CTRL_STAGED_SIZE,
    CTRL_MEM_SYS,
    CTRL_PLACE_ENTRY,
    CTRL_SMP_MAIR,
    CTRL_SHARED,
    CTRL_WAKES,
    CTRL_RX_DOOR,
    CTRL_DOOR_IRQ,
    CTRL_IDLE_MODE,
];

macro_rules! at {
    ($field:ident, $off:expr) => {
        const _: () = assert!(offset_of!(CtrlPage, $field) as u64 == $off);
    };
}

const _: () = assert!(size_of::<CtrlPage>() as u64 == crate::layout::CTRL_STRIDE);
at!(status, CTRL_STATUS);
at!(exit_code, CTRL_EXIT_CODE);
at!(kill, CTRL_KILL);
at!(heartbeat, CTRL_HEARTBEAT);
at!(ram_size, CTRL_RAM_SIZE);
at!(env_len, CTRL_ENV_LEN);
at!(entry, CTRL_ENTRY);
at!(s2_table, CTRL_S2_TABLE);
at!(wall_off, CTRL_WALL_OFF);
at!(idle, CTRL_IDLE);
at!(vec_pa, CTRL_VEC_PA);
at!(fault_esr, CTRL_FAULT_ESR);
at!(fault_far, CTRL_FAULT_FAR);
at!(fault_vec, CTRL_FAULT_VEC);
at!(cores, CTRL_CORES);
at!(smp_vbar, CTRL_SMP_VBAR);
at!(smp_tramp, CTRL_SMP_TRAMP);
at!(smp_sp, CTRL_SMP_SP);
at!(smp_mp, CTRL_SMP_MP);
at!(smp_g0, CTRL_SMP_G0);
at!(smp_fn, CTRL_SMP_FN);
at!(smp_stub, CTRL_SMP_STUB);
at!(smp_ttbr0, CTRL_SMP_TTBR0);
at!(slot, CTRL_SLOT);
at!(smp_req, CTRL_SMP_REQ);
at!(mbox_pa, CTRL_MBOX_PA);
at!(smp_mbox, CTRL_SMP_MBOX);
at!(smp_tcr, CTRL_SMP_TCR);
at!(staged_size, CTRL_STAGED_SIZE);
at!(mem_sys, CTRL_MEM_SYS);
at!(place_entry, CTRL_PLACE_ENTRY);
at!(smp_mair, CTRL_SMP_MAIR);
at!(shared, CTRL_SHARED);
at!(wakes, CTRL_WAKES);
at!(rx_door, CTRL_RX_DOOR);
at!(door_irq, CTRL_DOOR_IRQ);
at!(env, CTRL_ENV_DATA);
at!(idle_mode, CTRL_IDLE_MODE);
// De SMP-handoff in het ctx-blok draagt de control-velden onder 256 bytes.
const _: () = assert!(CTRL_SMP_MAIR + 8 <= crate::layout::CTX_LEN - crate::layout::CTX_SMP);

// ---------------------------------------------------------------------------
// De call-payload.
// ---------------------------------------------------------------------------

/// De versie van het payload-formaat.
pub const VERSION: u8 = 1;
/// De lengte van de kop van een request of response.
pub const HDR_LEN: usize = 24;
/// De historische mailboxgrens; nieuwe verbindingen begrenzen met
/// [`crate::systemapi::MAX_IO_CHUNK`].
pub const MAX_CHUNK: usize = 8 << 10;

/// `stat(path)`: de maat (een map: 0, status OK).
pub const OP_STAT: u8 = 1;
/// `read(path, off, n)`: de data.
pub const OP_READ: u8 = 2;
/// `write(path, off, data)`: maakt bestand en ouder-mappen.
pub const OP_WRITE: u8 = 3;
/// `list(path)`: namen, `\n`-gescheiden (`naam/` is een map).
pub const OP_LIST: u8 = 4;
/// `remove(path)`: een bestand of lege map.
pub const OP_REMOVE: u8 = 5;
// 6 was OpFetch: de kern downloadde een app-opgegeven URL met zijn volle
// rechten, een SSRF-pad naar alles wat de node bereikt. Gesloopt; het nummer
// blijft leeg, zodat een oud image een nette "onbekende op" krijgt.
/// `truncate(path, n)`: maakt bestand en ouder-mappen.
pub const OP_TRUNCATE: u8 = 7;
/// Object naar eigen pad (vervangend); de maat.
pub const OP_STORE_PULL: u8 = 8;
/// Eigen pad naar object (vervangend); de maat.
pub const OP_STORE_PUSH: u8 = 9;
/// Keys onder de eigen map plus pad-prefix, `\n`-gescheiden.
pub const OP_STORE_LIST: u8 = 10;
/// Object weg (idempotent).
pub const OP_STORE_DROP: u8 = 11;
// 12 en 13 waren OpSurfGrant/OpSurfRevoke (gesloopt 06-08, dezelfde dag als
// gebouwd); de nummers blijven leeg.
/// Codec openen; het handvat in `size`. Niet idempotent, zoals alle
/// codec-ops: twee keer dezelfde feed is twee happen bitstream.
pub const OP_CODEC_OPEN: u8 = 14;
/// Bitstream erin (een buffer in de eigen partitie: `off`, `n`).
pub const OP_CODEC_FEED: u8 = 15;
/// Een lege beeldbuffer erin.
pub const OP_CODEC_OFFER: u8 = 16;
/// Nul of meer events.
pub const OP_CODEC_POLL: u8 = 17;
/// Codec sluiten.
pub const OP_CODEC_CLOSE: u8 = 18;
/// Eén SCSI-uitwisseling; niet herhaalbaar.
pub const OP_DEVICE_COMMAND: u8 = 19;
/// Het hoogste opnummer van deze module; de bevoegde operaties van
/// [`crate::systemapi`] liggen erboven.
pub const OP_MAX: u8 = OP_DEVICE_COMMAND;

/// Een call-status: gelukt.
pub const STATUS_OK: u16 = 0;
/// Een call-status: algemene fout (tekst in de data).
pub const STATUS_ERROR: u16 = 1;
/// Een call-status: het pad bestaat niet.
pub const STATUS_NO_ENT: u16 = 2;
/// Een call-status: buiten de mounts of de eigen root.
pub const STATUS_DENIED: u16 = 3;

/// Een request.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Req<'a> {
    /// De operatie.
    pub op: u8,
    /// Het volgnummer, terug in de response.
    pub seq: u32,
    /// De offset.
    pub off: u64,
    /// De lengte of het getal-argument.
    pub n: u64,
    /// Het pad.
    pub path: &'a [u8],
    /// De data.
    pub data: &'a [u8],
}

/// Een response.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Resp<'a> {
    /// De operatie.
    pub op: u8,
    /// De status (`STATUS_*`); bij een fout draagt `data` de tekst.
    pub status: u16,
    /// Het volgnummer van de request.
    pub seq: u32,
    /// De maat of het handvat.
    pub size: u64,
    /// De data.
    pub data: &'a [u8],
}

/// Een little-endian `u16` op `b[i..]`; de aanroeper toetste de lengte.
fn le16(b: &[u8], i: usize) -> u16 {
    let mut w = [0u8; 2];
    w.copy_from_slice(&b[i..i + 2]);
    u16::from_le_bytes(w)
}

/// Een little-endian `u32` op `b[i..]`.
fn le32(b: &[u8], i: usize) -> u32 {
    let mut w = [0u8; 4];
    w.copy_from_slice(&b[i..i + 4]);
    u32::from_le_bytes(w)
}

/// Een little-endian `u64` op `b[i..]`.
fn le64(b: &[u8], i: usize) -> u64 {
    let mut w = [0u8; 8];
    w.copy_from_slice(&b[i..i + 8]);
    u64::from_le_bytes(w)
}

/// Schrijft de kop van 24 bytes; `dst` is minstens [`HDR_LEN`] lang.
fn put_head(dst: &mut [u8], op: u8, w16: u16, seq: u32, a: u64, b: u64) {
    dst[0] = VERSION;
    dst[1] = op;
    dst[2..4].copy_from_slice(&w16.to_le_bytes());
    dst[4..8].copy_from_slice(&seq.to_le_bytes());
    dst[8..16].copy_from_slice(&a.to_le_bytes());
    dst[16..24].copy_from_slice(&b.to_le_bytes());
}

/// Toetst lengte en versie van een binnengekomen payload.
fn check_head(b: &[u8]) -> Result {
    if b.len() < HDR_LEN {
        return Err(Error::Short {
            len: b.len(),
            need: HDR_LEN,
        });
    }
    if b[0] != VERSION {
        return Err(Error::BadVersion {
            got: b[0],
            want: VERSION,
        });
    }
    Ok(())
}

/// Serialiseert een request in `dst`; geeft de lengte.
pub fn encode_req(dst: &mut [u8], r: &Req<'_>) -> Result<usize> {
    let plen = u16::try_from(r.path.len()).map_err(|_| Error::PayloadTooLarge {
        len: r.path.len(),
        max: u16::MAX as usize,
    })?;
    let total = HDR_LEN + r.path.len() + r.data.len();
    if dst.len() < total {
        return Err(Error::Short {
            len: dst.len(),
            need: total,
        });
    }
    put_head(dst, r.op, plen, r.seq, r.off, r.n);
    let (path, data) = dst[HDR_LEN..total].split_at_mut(r.path.len());
    path.copy_from_slice(r.path);
    data.copy_from_slice(r.data);
    Ok(total)
}

/// Parseert een request; pad en data lenen uit `b`.
pub fn decode_req(b: &[u8]) -> Result<Req<'_>> {
    check_head(b)?;
    let plen = usize::from(le16(b, 2));
    let (path, data) = b[HDR_LEN..].split_at_checked(plen).ok_or(Error::Short {
        len: b.len(),
        need: HDR_LEN + plen,
    })?;
    Ok(Req {
        op: b[1],
        seq: le32(b, 4),
        off: le64(b, 8),
        n: le64(b, 16),
        path,
        data,
    })
}

/// Serialiseert een response in `dst`; geeft de lengte.
pub fn encode_resp(dst: &mut [u8], r: &Resp<'_>) -> Result<usize> {
    let total = HDR_LEN + r.data.len();
    if dst.len() < total {
        return Err(Error::Short {
            len: dst.len(),
            need: total,
        });
    }
    dst[HDR_LEN..total].copy_from_slice(r.data);
    encode_resp_head(dst, r, r.data.len())
}

/// Schrijft alleen de kop van `r` in `dst[..HDR_LEN]`; de `n` databytes
/// staan er al op `dst[HDR_LEN..]` (de aanroeper liet de opslag er direct in
/// lezen). `r.data` wordt genegeerd. Geeft de lengte: zelfde draadvorm als
/// [`encode_resp`], zonder de kopie.
pub fn encode_resp_head(dst: &mut [u8], r: &Resp<'_>, n: usize) -> Result<usize> {
    let total = HDR_LEN.saturating_add(n);
    if dst.len() < total {
        return Err(Error::Short {
            len: dst.len(),
            need: total,
        });
    }
    put_head(dst, r.op, r.status, r.seq, r.size, 0);
    Ok(total)
}

/// Parseert een response; de data leent uit `b`.
pub fn decode_resp(b: &[u8]) -> Result<Resp<'_>> {
    check_head(b)?;
    Ok(Resp {
        op: b[1],
        status: le16(b, 2),
        seq: le32(b, 4),
        size: le64(b, 8),
        data: &b[HDR_LEN..],
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Geport uit `TestCtrlOffsetsUniek`: élke offset is uniek,
    /// 8-gealigneerd, binnen de page en buiten de env-regio. Aanleiding
    /// (18-07): `CtrlSMPTcr` en `CtrlIdle` stonden allebei op 0xD8.
    #[test]
    fn ctrl_offsets_uniek() {
        let page = crate::layout::CTRL_STRIDE;
        for (i, &v) in CTRL_WORDS.iter().enumerate() {
            assert!(v.is_multiple_of(8), "{v:#x} niet gealigneerd");
            assert!(v + 8 <= page, "{v:#x} buiten de page");
            assert!(
                v + 8 <= CTRL_ENV_DATA || v >= CTRL_ENV_DATA + CTRL_ENV_MAX,
                "{v:#x} overlapt de env-regio"
            );
            assert!(
                !CTRL_WORDS[i + 1..].contains(&v),
                "OFFSET-COLLISIE op {v:#x}"
            );
        }
        const { assert!(CTRL_WORDS.len() >= 20) };
    }

    #[test]
    fn req_roundtrip() {
        let r = Req {
            op: OP_WRITE,
            seq: 7,
            off: 1 << 40,
            n: 3,
            path: b"/data/x",
            data: b"abc",
        };
        let mut buf = [0u8; 64];
        let n = encode_req(&mut buf, &r).unwrap();
        assert_eq!(n, HDR_LEN + 7 + 3);
        assert_eq!(decode_req(&buf[..n]).unwrap(), r);
        assert!(matches!(
            decode_req(&buf[..HDR_LEN + 3]),
            Err(Error::Short { .. })
        ));
        buf[0] = 2;
        assert!(matches!(
            decode_req(&buf[..n]),
            Err(Error::BadVersion { got: 2, want: 1 })
        ));
    }

    #[test]
    fn resp_roundtrip_en_kop_alleen() {
        let r = Resp {
            op: OP_READ,
            status: STATUS_NO_ENT,
            seq: 9,
            size: 42,
            data: b"weg",
        };
        let mut buf = [0u8; 64];
        let n = encode_resp(&mut buf, &r).unwrap();
        assert_eq!(decode_resp(&buf[..n]).unwrap(), r);
        let mut other = [0u8; 64];
        other[HDR_LEN..HDR_LEN + 3].copy_from_slice(b"weg");
        let m = encode_resp_head(&mut other, &Resp { data: &[], ..r }, 3).unwrap();
        assert_eq!(&other[..m], &buf[..n]);
        assert!(decode_resp(&buf[..HDR_LEN - 1]).is_err());
    }

    #[test]
    fn app_status_roundtrip() {
        for s in [
            AppStatus::Empty,
            AppStatus::Booting,
            AppStatus::Ready,
            AppStatus::Exited,
            AppStatus::Staged,
        ] {
            assert_eq!(AppStatus::from_raw(s.raw()), Some(s));
        }
        assert_eq!(AppStatus::from_raw(5), None);
    }
}
