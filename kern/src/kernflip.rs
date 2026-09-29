//! De kern-flip: de overdracht (het handoff-blob), de adoptie door de nieuwe
//! kern ([`adopted`]), en de vluchtrecorder met zijn archief.
//!
//! Alles in het blob is BOEKHOUDING, geen inhoud: de app-werelden blijven
//! staan waar ze staan. Bij een onbruikbare overdracht stopt de nieuwe boot;
//! de allocator mag mogelijk levende eigenaren nooit als vrije ruimte
//! behandelen (E8). Het herstel van de claims zelf is
//! [`crate::slots::Lifecycle::adopt`].
//!
//! Daarnaast de bundel ([`Bundle`]: de kern-ELF plus zijn relocatietabel,
//! `image/flip-bundle.sh`), het platte neerleggen ervan ([`flatten`]) en de
//! som die hem vertrouwd maakt ([`sha256`]). Het relokeren en de sprong met
//! de MMU uit zijn van `cpu::el2::chain`; het ophalen doet Hop (`POST
//! /flip`), dat de bundel in een gereserveerd slot stroomt.

use crate::cage::PhysMem;
use crate::slots::{Mount, SlotState, try_push};
use crate::{Error, Result};
use alloc::vec::Vec;

/// "HOPHAND1" little-endian.
pub const HAND_MAGIC: u64 = 0x3144_4E41_4850_4F48;
/// De versie van het blob.
pub const HAND_VERSION: u64 = 6;
/// De ruimte boven de RAM-declaratie van de nieuwe kern voor het blob.
/// 256 KB: de volle conntrack (4096 flows van 24 bytes, ongeveer 96 KB) plus
/// de kop en de slot-records; 0,1% van een kernvenster, en daarvoor
/// overleeft elke verbinding door de switch een kernwissel.
pub const HANDOFF_TAIL: usize = 0x40000;
/// De grootste agent-state (JSON van Hop); groter is een teken dat er iets
/// anders mis is, en strandt vóór de sprong.
pub const MAX_AGENT_STATE: usize = 128 << 10;
/// De volle conntrack van de switch (`hopswitch.MaxFlows`).
pub const MAX_FLOWS: usize = 4096;
const HAND_HEAD: usize = 128;
const SLOT_HEAD: usize = 80;

/// Eén NAT-flow uit de conntrack van de switch.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub struct FlowState {
    /// IP-protocol (6 of 17).
    pub proto: u8,
    /// Het slot.
    pub slot: u8,
    /// Hoeveel FIN's er gezien zijn.
    pub fins: u8,
    /// De poort van de app.
    pub slot_port: u16,
    /// De bestemmingspoort.
    pub dst_port: u16,
    /// Het IP van de app.
    pub slot_ip: u32,
    /// Het bestemmings-IP.
    pub dst_ip: u32,
    /// De node-poort van de masquerade.
    pub node_port: u16,
}

/// De NAT-staat van de switch.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct NatState {
    /// De volgende masquerade-poort.
    pub masq_next: u16,
    /// Het MAC van de gateway.
    pub gw_mac: [u8; 6],
    /// Is dat MAC bekend?
    pub gw_known: bool,
    /// De flows.
    pub flows: Vec<FlowState>,
}

/// Wat de vertrekkende kern achterliet.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Handoff {
    /// Het venster van de vorige kern.
    pub old_base: u64,
    /// De maat daarvan.
    pub old_size: u64,
    /// Het eigen venster, inclusief de handoff-staart.
    pub window: u64,
    /// De totale maat daarvan.
    pub total: u64,
    /// De hoeveelste flip deze boot is (1 = de eerste).
    pub generation: u64,
    /// De som van de bundel: "flip naar deze URL" in een boot-config mag
    /// geen eeuwige lus worden.
    pub bundle_sum: u64,
    /// De bewoners.
    pub slots: Vec<SlotState>,
    /// De conntrack: zonder deze tabel breekt elke verbinding door de
    /// masquerade bij een kernwissel, terwijl de app doorleeft.
    pub nat: NatState,
    /// De state van Hop zelf (JSON), anders worden de apps wezen.
    pub agent: Vec<u8>,
}

fn put(b: &mut Vec<u8>, s: &[u8]) -> Result {
    b.try_reserve(s.len())
        .map_err(|_| Error::OutOfMemory { bytes: s.len() })?;
    b.extend_from_slice(s);
    Ok(())
}

fn put64(b: &mut Vec<u8>, v: u64) -> Result {
    put(b, &v.to_le_bytes())
}

fn pad8(b: &mut Vec<u8>) -> Result {
    while !b.len().is_multiple_of(8) {
        put(b, &[0])?;
    }
    Ok(())
}

/// Bouwt het blob. Past het niet in `max`, dan is dat een fout vóór de
/// sprong (en dus geen flip) in plaats van een half blob.
pub fn encode(h: &Handoff, max: usize) -> Result<Vec<u8>> {
    if h.agent.len() > MAX_AGENT_STATE {
        return Err(Error::TooLarge {
            len: h.agent.len(),
            max: MAX_AGENT_STATE,
        });
    }
    let mut b = Vec::new();
    for v in [
        HAND_MAGIC,
        HAND_VERSION,
        h.old_base,
        h.old_size,
        h.window,
        h.total,
        h.slots.len() as u64,
        h.generation,
        h.bundle_sum,
    ] {
        put64(&mut b, v)?;
    }
    put(&mut b, &[0; HAND_HEAD - 72])?;
    for s in &h.slots {
        for v in [
            s.slot as u64,
            s.part_base,
            s.part_size,
            s.core as u64,
            s.ports.len() as u64,
            s.job.len() as u64,
            s.cores as u64,
            s.mounts.len() as u64,
            s.share_group.len() as u64,
            s.group_cores.len() as u64,
        ] {
            put64(&mut b, v)?;
        }
        for c in &s.group_cores {
            put64(&mut b, *c as u64)?;
        }
        put(&mut b, &s.share_group)?;
        pad8(&mut b)?;
        for p in &s.ports {
            put64(&mut b, u64::from(*p))?;
        }
        put(&mut b, &s.job)?;
        pad8(&mut b)?;
        for m in &s.mounts {
            put64(&mut b, m.local.len() as u64)?;
            put64(&mut b, m.shared.len() as u64)?;
            put(&mut b, &m.local)?;
            put(&mut b, &m.shared)?;
            pad8(&mut b)?;
        }
    }
    let mut mac = 0u64;
    for (i, v) in h.nat.gw_mac.iter().enumerate() {
        mac |= u64::from(*v) << (8 * i);
    }
    if h.nat.gw_known {
        mac |= 1 << 56;
    }
    put64(&mut b, u64::from(h.nat.masq_next))?;
    put64(&mut b, mac)?;
    put64(&mut b, h.nat.flows.len() as u64)?;
    for f in &h.nat.flows {
        put64(
            &mut b,
            u64::from(f.proto)
                | u64::from(f.slot) << 8
                | u64::from(f.fins) << 16
                | u64::from(f.slot_port) << 32
                | u64::from(f.dst_port) << 48,
        )?;
        put64(&mut b, u64::from(f.slot_ip) | u64::from(f.dst_ip) << 32)?;
        put64(&mut b, u64::from(f.node_port))?;
    }
    put64(&mut b, h.agent.len() as u64)?;
    put(&mut b, &h.agent)?;
    pad8(&mut b)?;
    if b.len() > max {
        return Err(Error::TooLarge { len: b.len(), max });
    }
    Ok(b)
}

struct R<'a> {
    b: &'a [u8],
    pos: usize,
}

impl<'a> R<'a> {
    fn u64(&mut self) -> Result<u64> {
        let s = self
            .b
            .get(self.pos..self.pos + 8)
            .and_then(|s| <[u8; 8]>::try_from(s).ok())
            .ok_or(Error::Corrupt { at: self.pos })?;
        self.pos += 8;
        Ok(u64::from_le_bytes(s))
    }

    fn bytes(&mut self, n: u64) -> Result<&'a [u8]> {
        let n = usize::try_from(n).map_err(|_| Error::Corrupt { at: self.pos })?;
        let s = self
            .b
            .get(self.pos..self.pos.saturating_add(n))
            .ok_or(Error::Corrupt { at: self.pos })?;
        self.pos += n;
        Ok(s)
    }

    fn align(&mut self) {
        self.pos += (8 - self.pos % 8) % 8;
    }

    fn left(&self) -> u64 {
        self.b.len().saturating_sub(self.pos) as u64
    }
}

fn bounded(v: u64, max: u64, at: usize) -> Result<u64> {
    if v > max {
        return Err(Error::Corrupt { at });
    }
    Ok(v)
}

/// Leest het blob terug. Elke afwijking is een fout: de aanroeper stopt de
/// boot voordat een gedeeltelijke allocator bruikbaar wordt.
pub fn decode(b: &[u8]) -> Result<Handoff> {
    if b.len() < HAND_HEAD {
        return Err(Error::Corrupt { at: b.len() });
    }
    let mut r = R { b, pos: 0 };
    let magic = r.u64()?;
    if magic != HAND_MAGIC {
        return Err(Error::Version {
            have: magic,
            want: HAND_MAGIC,
        });
    }
    let v = r.u64()?;
    if v != HAND_VERSION {
        return Err(Error::Version {
            have: v,
            want: HAND_VERSION,
        });
    }
    let mut h = Handoff {
        old_base: r.u64()?,
        old_size: r.u64()?,
        window: r.u64()?,
        total: r.u64()?,
        ..Handoff::default()
    };
    let n = bounded(r.u64()?, 1024, 48)?;
    h.generation = r.u64()?;
    h.bundle_sum = r.u64()?;
    r.pos = HAND_HEAD;
    for _ in 0..n {
        if r.left() < SLOT_HEAD as u64 {
            return Err(Error::Corrupt { at: r.pos });
        }
        let mut s = SlotState {
            slot: r.u64()? as usize,
            part_base: r.u64()?,
            part_size: r.u64()?,
            core: r.u64()? as usize,
            ..SlotState::default()
        };
        let n_ports = r.u64()?;
        let job_len = r.u64()?;
        s.cores = r.u64()? as usize;
        let n_mounts = r.u64()?;
        let group_len = bounded(r.u64()?, 256, r.pos)?;
        let n_group = bounded(r.u64()?, 1024, r.pos)?;
        if n_group * 8 + group_len > r.left() {
            return Err(Error::Corrupt { at: r.pos });
        }
        for _ in 0..n_group {
            try_push(&mut s.group_cores, r.u64()? as usize)?;
        }
        s.share_group = crate::slots::try_vec(r.bytes(group_len)?)?;
        r.align();
        let n_ports = bounded(n_ports, 64, r.pos)?;
        let job_len = bounded(job_len, 256, r.pos)?;
        if n_ports * 8 + job_len > r.left() {
            return Err(Error::Corrupt { at: r.pos });
        }
        for _ in 0..n_ports {
            try_push(&mut s.ports, r.u64()? as u16)?;
        }
        s.job = crate::slots::try_vec(r.bytes(job_len)?)?;
        r.align();
        for _ in 0..bounded(n_mounts, 64, r.pos)? {
            let ll = bounded(r.u64()?, 4096, r.pos)?;
            let sl = bounded(r.u64()?, 4096, r.pos)?;
            let local = crate::slots::try_vec(r.bytes(ll)?)?;
            let shared = crate::slots::try_vec(r.bytes(sl)?)?;
            try_push(&mut s.mounts, Mount { local, shared })?;
            r.align();
        }
        try_push(&mut h.slots, s)?;
    }
    // Deze versie draagt altijd beide dienstblokken, ook leeg.
    h.nat.masq_next = r.u64()? as u16;
    let mac = r.u64()?;
    for (i, v) in h.nat.gw_mac.iter_mut().enumerate() {
        *v = (mac >> (8 * i)) as u8;
    }
    h.nat.gw_known = mac & (1 << 56) != 0;
    let nf = bounded(r.u64()?, MAX_FLOWS as u64, r.pos)?;
    if nf * 24 > r.left() {
        return Err(Error::Corrupt { at: r.pos });
    }
    for _ in 0..nf {
        let (w0, w1, w2) = (r.u64()?, r.u64()?, r.u64()?);
        try_push(
            &mut h.nat.flows,
            FlowState {
                proto: w0 as u8,
                slot: (w0 >> 8) as u8,
                fins: (w0 >> 16) as u8,
                slot_port: (w0 >> 32) as u16,
                dst_port: (w0 >> 48) as u16,
                slot_ip: w1 as u32,
                dst_ip: (w1 >> 32) as u32,
                node_port: w2 as u16,
            },
        )?;
    }
    let na = bounded(r.u64()?, MAX_AGENT_STATE as u64, r.pos)?;
    h.agent = crate::slots::try_vec(r.bytes(na)?)?;
    Ok(h)
}

/// Waar de flip zijn woorden houdt, uit `layout`.
#[derive(Copy, Clone, Debug)]
pub struct FlipPlan {
    /// Het pointer/magic-paar op de boot-scratch (`layout.HandoffPtrPA`).
    pub handoff_ptr_pa: u64,
    /// De vluchtrecorder (`layout.FlipStagePA`), 0 = geen.
    pub stage_pa: u64,
    /// Het einde van de eigen RAM-declaratie: daar MOET het blob liggen.
    pub own_ram_end: u64,
}

/// Wat een boot over de flip weet.
#[derive(Debug, PartialEq, Eq)]
pub enum Boot {
    /// Een gewone (koude) boot, of het paar was al geconsumeerd.
    Cold,
    /// Een flip-boot met deze overdracht.
    Adopted(Handoff),
}

/// Is er een overdracht KLAAR, zonder hem te consumeren? Het pointer/magic-
/// paar is het enige harde bewijs; de recorder niet (die houdt "geland" vast
/// tot de agent draait, en een koude boot daarna zou zich als adoptie zien).
pub fn flip_pending(mem: &impl PhysMem, plan: &FlipPlan) -> bool {
    mem.read64(plan.handoff_ptr_pa + 8) == HAND_MAGIC && mem.read64(plan.handoff_ptr_pa) != 0
}

/// Consumeert het pointer/magic-paar (vóór het vertrouwen, ook als het niet
/// klopt: half garbage mag geen tweede boot besmetten) en leest het blob.
///
/// Een kapot blob met een geldig paar is GEEN koude boot: er is echt geflipt
/// en er kunnen bewoners leven, en een koude boot veegt juist hun regio. De
/// fout gaat naar de aanroeper, die blijft staan tot de watchdog komt
/// (`HOPOS_FLIP_BLOB_BAD`).
pub fn adopted(mem: &mut impl PhysMem, plan: &FlipPlan) -> Result<Boot> {
    let ptr = mem.read64(plan.handoff_ptr_pa);
    let magic = mem.read64(plan.handoff_ptr_pa + 8);
    if ptr == 0 && magic == 0 {
        return Ok(Boot::Cold);
    }
    mem.write64(plan.handoff_ptr_pa, 0);
    mem.write64(plan.handoff_ptr_pa + 8, 0);
    if magic != HAND_MAGIC {
        return Ok(Boot::Cold); // Een verdwaalde pointer.
    }
    // De pointer wijst exact op het einde van de eigen RAM-declaratie: een
    // eigenschap van de constructie, en de hele klasse "lees op een adres
    // dat een ander daar neerlegde" is weg.
    if ptr == 0 || !ptr.is_multiple_of(8) || (plan.own_ram_end != 0 && ptr != plan.own_ram_end) {
        return Err(Error::Range {
            base: ptr,
            size: HANDOFF_TAIL as u64,
        });
    }
    let mut b = Vec::new();
    b.try_reserve_exact(HANDOFF_TAIL)
        .map_err(|_| Error::OutOfMemory {
            bytes: HANDOFF_TAIL,
        })?;
    b.resize(HANDOFF_TAIL, 0);
    mem.copy_out(&mut b, ptr);
    match decode(&b) {
        Ok(h) => {
            archive_stage(mem, plan, h.generation);
            stage(mem, plan, Stage::Landed, 0);
            Ok(Boot::Adopted(h))
        }
        Err(e) => {
            stage(mem, plan, Stage::AdoptBlobBad, 0);
            Err(e)
        }
    }
}

/// De stappen van de flip, in volgorde. Een nieuwe stap hoort ACHTERAAN:
/// ertussen schuiven hernummert de rest, en dan meldt een oudere kern zijn
/// eigen stand verkeerd (06-09, het kostte een ronde).
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
#[repr(u16)]
pub enum Stage {
    /// Bundel opgehaald en geverifieerd.
    Fetched = 1,
    /// Het lifecycle-venster vast.
    WindowHeld,
    /// Bundel gevalideerd.
    BundleOk,
    /// Venster geleend.
    Borrowed,
    /// Vectoren geïnstalleerd.
    Vectors,
    /// Venster gewist.
    Scrubbed,
    /// Segmenten geplaatst.
    Placed,
    /// Relocaties toegepast.
    Rebased,
    /// Bewoners, NAT en agent vastgelegd.
    Captured,
    /// Blob geschreven.
    Handoff,
    /// Op het punt van springen.
    Jumping,
    /// De nieuwe kern haalde main.
    EarlyMain,
    /// De overdracht is geconsumeerd.
    Landed,
    /// Het net van de nieuwe kern is op.
    NetUp,
    /// Het blob decodeerde niet; de kern wacht op de watchdog.
    AdoptBlobBad,
}

impl Stage {
    fn from(n: u64) -> Option<Stage> {
        use Stage::*;
        [
            Fetched,
            WindowHeld,
            BundleOk,
            Borrowed,
            Vectors,
            Scrubbed,
            Placed,
            Rebased,
            Captured,
            Handoff,
            Jumping,
            EarlyMain,
            Landed,
            NetUp,
            AdoptBlobBad,
        ]
        .into_iter()
        .find(|s| *s as u64 == n)
    }

    /// De consoletekst bij een stand.
    #[must_use]
    pub fn describe(self) -> &'static str {
        match self {
            Stage::Fetched => "bundle fetched and verified",
            Stage::WindowHeld => "slot lifecycle window held",
            Stage::BundleOk => "bundle validated",
            Stage::Borrowed => "window borrowed from the pool",
            Stage::Vectors => "vectors installed",
            Stage::Scrubbed => "window scrubbed",
            Stage::Placed => "segments placed",
            Stage::Rebased => "relocations applied",
            Stage::Captured => "residents, NAT and agent state captured",
            Stage::Handoff => "handoff blob written",
            Stage::Jumping => "about to jump into the new kernel",
            Stage::EarlyMain => "the new kernel reached main but died before the handover",
            Stage::Landed => {
                "jump landed and the handover was consumed — died later in the new kernel's boot"
            }
            Stage::NetUp => "the new kernel's network is up — died between net and agent",
            Stage::AdoptBlobBad => {
                "the handoff blob did not decode; the kernel refused to continue and waited for the watchdog"
            }
        }
    }
}

/// "FLIP" in de bovenste 32 bits: willekeurige DRAM-rommel na een koude
/// start wordt nooit als "vorige flip" gelezen.
const STAGE_TAG: u64 = 0x464C_4950 << 32;
const ARCHIVE_OFF: u64 = 8;

fn stage_of(v: u64) -> Option<(Stage, u64)> {
    if v & !0xFFFF_FFFF != STAGE_TAG {
        return None;
    }
    Some((Stage::from(v & 0xFFFF)?, (v >> 16) & 0xFFFF))
}

/// Legt de lopende stand vast, rechtstreeks naar DRAM geveegd: dit woord
/// moet een crash één instructie later nog overleven.
pub fn stage(mem: &mut impl PhysMem, plan: &FlipPlan, s: Stage, generation: u64) {
    if plan.stage_pa == 0 {
        return;
    }
    mem.write64(
        plan.stage_pa,
        STAGE_TAG | s as u64 | (generation & 0xFFFF) << 16,
    );
    mem.clean_inv(plan.stage_pa, 8);
}

/// Schuift een onafgemaakte stand naar het archief. Wat ONZE eigen
/// generatie schreef is eigen voortgang, geen mislukking.
///
/// Het archief bestaat omdat de flip die een gevallen node weer optilt zijn
/// eigen stappen over het spoor heen schrijft: de eerste twee gevallen flips
/// op de M4 (06-09) waren daardoor onverklaarbaar.
pub fn archive_stage(mem: &mut impl PhysMem, plan: &FlipPlan, mine: u64) {
    if plan.stage_pa == 0 {
        return;
    }
    let v = mem.read64(plan.stage_pa);
    match stage_of(v) {
        Some((_, generation)) if generation != mine => {
            mem.write64(plan.stage_pa + ARCHIVE_OFF, v);
            mem.clean_inv(plan.stage_pa + ARCHIVE_OFF, 8);
        }
        _ => {}
    }
}

/// Het archief (de laatste poging die niet landde), gewist na het lezen.
/// Elke boot vraagt het; `None` betekent dat elke eerdere flip netjes eindigde.
pub fn take_archived(mem: &mut impl PhysMem, plan: &FlipPlan) -> Option<(Stage, u64)> {
    if plan.stage_pa == 0 {
        return None;
    }
    let r = stage_of(mem.read64(plan.stage_pa + ARCHIVE_OFF))?;
    mem.write64(plan.stage_pa + ARCHIVE_OFF, 0);
    mem.clean_inv(plan.stage_pa + ARCHIVE_OFF, 8);
    Some(r)
}

/// De lopende stand, gewist na het lezen. Alleen op een KOUDE boot: op een
/// flip-boot staat daar de sprong die ons bracht.
pub fn take_last_flip(mem: &mut impl PhysMem, plan: &FlipPlan) -> Option<(Stage, u64)> {
    if plan.stage_pa == 0 {
        return None;
    }
    let r = stage_of(mem.read64(plan.stage_pa))?;
    mem.write64(plan.stage_pa, 0);
    mem.clean_inv(plan.stage_pa, 8);
    Some(r)
}

/// De eerste regel van main van een geflipte kern: stond er "springen",
/// dan zijn wij die sprong.
pub fn mark_early_boot(mem: &mut impl PhysMem, plan: &FlipPlan) {
    if plan.stage_pa != 0
        && let Some((Stage::Jumping, generation)) = stage_of(mem.read64(plan.stage_pa))
    {
        stage(mem, plan, Stage::EarlyMain, generation);
    }
}

// ---------------------------------------------------------------------------
// De bundel: de kern-ELF plus zijn relocatietabel (image/flip-bundle.sh).
// ---------------------------------------------------------------------------

/// De versie van het flip-contract van v3: de staart-vorm, het handoff-blob
/// en de ingangsconditie samen. De Go-generatie sprak ABI 2 (een geleend
/// venster, `RamStart`/`RamSize`-patches); v3 plaatst de nieuwe kern op het
/// koude adres van de oude, dus een Go-bundel wordt hier geweigerd.
pub const FLIP_ABI: u32 = 3;
/// "HOPRELO1" little-endian: hetzelfde woord als `mkkernel -elfreloc`.
pub const RELOC_MAGIC: u64 = 0x314F_4C45_5250_4F48;
/// De kop van de staart: magic, versie, flip-ABI, ELF-maat, linkbasis,
/// platte maat, entry, aantal relocaties.
pub const BUNDLE_HEAD: usize = 56;
/// De voet: de offset van de kop en nog een keer de magic.
pub const BUNDLE_FOOT: usize = 16;
/// De grootste platte kern (image plus BSS en stack): een kern van v3 is
/// ruim een MiB (1,1 MiB gemeten 29-09); 64 MiB is een typefout, geen kern.
pub const MAX_FLAT: u64 = 64 << 20;

/// Een gevalideerde flip-bundel over de bytes waarin Hop hem stroomde.
///
/// # Invariants
///
/// `elf` en `relocs` liggen binnen de bundel; elke relocatie-offset is
/// 8-uitgelijnd en valt met zijn woord binnen `flat_size`; `entry` ligt in
/// `[link_load, link_load + flat_size)` en is 4-uitgelijnd.
#[derive(Debug)]
pub struct Bundle<'a> {
    /// De onaangeroerde kern-ELF (met symbooltabel).
    pub elf: &'a [u8],
    /// De flip-ABI van de bundel.
    pub flip_abi: u32,
    /// De linkbasis: het laagste laadadres van de ELF.
    pub link_load: u64,
    /// Het platte beeld inclusief BSS en stack.
    pub flat_size: u64,
    /// Het entrypoint, absoluut op de linkbasis.
    pub entry: u64,
    relocs: &'a [u8],
}

fn le64(b: &[u8], at: usize) -> Result<u64> {
    b.get(at..at + 8)
        .and_then(|s| <[u8; 8]>::try_from(s).ok())
        .map(u64::from_le_bytes)
        .ok_or(Error::Corrupt { at })
}

fn le32(b: &[u8], at: usize) -> Result<u32> {
    b.get(at..at + 4)
        .and_then(|s| <[u8; 4]>::try_from(s).ok())
        .map(u32::from_le_bytes)
        .ok_or(Error::Corrupt { at })
}

impl<'a> Bundle<'a> {
    /// Valideert een bundel. Elke afwijking is een fout: een bundel is
    /// compleet geldig of bestaat niet, want de bytes komen van het net en
    /// dit pad springt er straks in (`bundle.go`, ParseBundleReader).
    pub fn parse(b: &'a [u8]) -> Result<Bundle<'a>> {
        if b.len() < BUNDLE_HEAD + BUNDLE_FOOT + 64 {
            return Err(Error::Corrupt { at: b.len() });
        }
        let foot = b.len() - BUNDLE_FOOT;
        let magic = le64(b, foot + 8)?;
        if magic != RELOC_MAGIC {
            // Een kale ELF in plaats van een bundel, of een afgekapte stroom.
            return Err(Error::Version {
                have: magic,
                want: RELOC_MAGIC,
            });
        }
        let head = usize::try_from(le64(b, foot)?).map_err(|_| Error::Corrupt { at: foot })?;
        if !head.is_multiple_of(8) || head > foot - BUNDLE_HEAD {
            return Err(Error::Corrupt { at: foot });
        }
        if le64(b, head)? != RELOC_MAGIC {
            return Err(Error::Corrupt { at: head });
        }
        let version = le32(b, head + 8)?;
        if version != 1 {
            return Err(Error::Version {
                have: u64::from(version),
                want: 1,
            });
        }
        let elf_size = le64(b, head + 16)?;
        let count = le64(b, head + 48)?;
        let bundle = Bundle {
            flip_abi: le32(b, head + 12)?,
            link_load: le64(b, head + 24)?,
            flat_size: le64(b, head + 32)?,
            entry: le64(b, head + 40)?,
            elf: b
                .get(..usize::try_from(elf_size).unwrap_or(usize::MAX))
                .filter(|_| elf_size != 0 && elf_size <= head as u64)
                .ok_or(Error::Corrupt { at: head + 16 })?,
            relocs: {
                let start = head + BUNDLE_HEAD;
                let room = (foot - start) as u64 / 4;
                if count > room {
                    return Err(Error::Corrupt { at: head + 48 });
                }
                b.get(start..start + count as usize * 4)
                    .ok_or(Error::Corrupt { at: start })?
            },
        };
        if bundle.flat_size == 0 || bundle.flat_size > MAX_FLAT {
            return Err(Error::TooLarge {
                len: usize::try_from(bundle.flat_size).unwrap_or(usize::MAX),
                max: MAX_FLAT as usize,
            });
        }
        let end = bundle.link_load.checked_add(bundle.flat_size);
        if !bundle.link_load.is_multiple_of(0x1000) || end.is_none() {
            return Err(Error::Range {
                base: bundle.link_load,
                size: bundle.flat_size,
            });
        }
        if bundle.entry < bundle.link_load
            || end.is_some_and(|e| bundle.entry >= e)
            || !bundle.entry.is_multiple_of(4)
        {
            return Err(Error::Range {
                base: bundle.entry,
                size: bundle.flat_size,
            });
        }
        for (i, off) in bundle.relocs().enumerate() {
            if !off.is_multiple_of(8) || u64::from(off) + 8 > bundle.flat_size {
                return Err(Error::Corrupt {
                    at: head + BUNDLE_HEAD + i * 4,
                });
            }
        }
        // INVARIANT: bereiken, uitlijning en elke relocatie zijn getoetst.
        Ok(bundle)
    }

    /// De relocatie-offsets: elk een 8-byte-woord in het platte beeld dat
    /// een absoluut adres op de linkbasis draagt.
    pub fn relocs(&self) -> impl Iterator<Item = u32> + Clone + '_ {
        self.relocs
            .chunks_exact(4)
            .filter_map(|c| <[u8; 4]>::try_from(c).ok())
            .map(u32::from_le_bytes)
    }

    /// Het aantal relocaties.
    #[must_use]
    pub fn reloc_count(&self) -> usize {
        self.relocs.len() / 4
    }
}

/// Legt de PT_LOAD-segmenten van de bundel plat neer op `dst` (het beeld
/// zoals het op de linkbasis zou staan, maar dan op `dst`): eerst het hele
/// beeld op nul (BSS, stack, gaten), dan de bytes. Geeft het aantal
/// segmenten. Relocaties zijn niet van hier (`cpu::el2::chain::relocate`).
///
/// De ELF-entry moet die van de staart zijn: twee bronnen die het oneens
/// zijn, is een bundel die niet van één build komt.
pub fn flatten(bundle: &Bundle<'_>, mem: &mut impl PhysMem, dst: u64) -> Result<usize> {
    let f = leanelf::File::parse(bundle.elf).map_err(|_| Error::Corrupt { at: 0 })?;
    if f.entry != bundle.entry {
        return Err(Error::Version {
            have: f.entry,
            want: bundle.entry,
        });
    }
    let end = bundle.link_load + bundle.flat_size;
    mem.clear(dst, bundle.flat_size.next_multiple_of(8));
    let mut n = 0;
    for s in f.segments().filter(|s| s.kind == leanelf::PT_LOAD) {
        let bad = Error::Range {
            base: s.paddr,
            size: s.memsz,
        };
        let s_end = s.paddr.checked_add(s.memsz).ok_or(bad)?;
        if s.paddr < bundle.link_load || s_end > end || s.filesz > s.memsz {
            return Err(bad);
        }
        let bytes = usize::try_from(s.off)
            .ok()
            .zip(usize::try_from(s.filesz).ok())
            .and_then(|(o, l)| bundle.elf.get(o..o.checked_add(l)?))
            .ok_or(Error::Corrupt { at: 0 })?;
        mem.copy_in(dst + (s.paddr - bundle.link_load), bytes);
        n += 1;
    }
    if n == 0 {
        return Err(Error::Corrupt { at: 0 });
    }
    Ok(n)
}

// ---------------------------------------------------------------------------
// SHA-256 (FIPS 180-4): de som van de bundel is het vertrouwensanker.
// ---------------------------------------------------------------------------

/// Een incrementele SHA-256. Hier en niet uit `leantls`: die houdt hem
/// crate-privé, en een kern die één hash nodig heeft linkt geen TLS-stapel
/// (dezelfde afweging als `leans3/src/sha256.rs`).
#[derive(Clone)]
pub struct Sha256 {
    h: [u32; 8],
    buf: [u8; 64],
    fill: usize,
    len: u64,
}

const SHA_K: [u32; 64] = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

impl Default for Sha256 {
    fn default() -> Self {
        Self::new()
    }
}

impl Sha256 {
    /// Een verse hash.
    #[must_use]
    pub const fn new() -> Sha256 {
        Sha256 {
            h: [
                0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
                0x5be0cd19,
            ],
            buf: [0; 64],
            fill: 0,
            len: 0,
        }
    }

    fn block(&mut self, b: &[u8; 64]) {
        let mut w = [0u32; 64];
        for (i, c) in b.chunks_exact(4).enumerate() {
            if let (Some(d), Ok(v)) = (w.get_mut(i), <[u8; 4]>::try_from(c)) {
                *d = u32::from_be_bytes(v);
            }
        }
        for i in 16..64 {
            let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
            let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }
        let [mut a, mut b2, mut c, mut d, mut e, mut f, mut g, mut h] = self.h;
        for (k, wi) in SHA_K.iter().zip(w.iter()) {
            let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let ch = (e & f) ^ (!e & g);
            let t1 = h
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(*k)
                .wrapping_add(*wi);
            let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let maj = (a & b2) ^ (a & c) ^ (b2 & c);
            let t2 = s0.wrapping_add(maj);
            h = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b2;
            b2 = a;
            a = t1.wrapping_add(t2);
        }
        for (s, v) in self.h.iter_mut().zip([a, b2, c, d, e, f, g, h]) {
            *s = s.wrapping_add(v);
        }
    }

    /// Voert bytes in.
    pub fn update(&mut self, mut data: &[u8]) {
        self.len = self.len.wrapping_add(data.len() as u64);
        while !data.is_empty() {
            let take = (64 - self.fill).min(data.len());
            let (now, rest) = data.split_at(take);
            if let Some(d) = self.buf.get_mut(self.fill..self.fill + take) {
                d.copy_from_slice(now);
            }
            self.fill += take;
            data = rest;
            if self.fill == 64 {
                let b = self.buf;
                self.block(&b);
                self.fill = 0;
            }
        }
    }

    /// De som.
    #[must_use]
    pub fn finish(mut self) -> [u8; 32] {
        let bits = self.len.wrapping_mul(8);
        self.update(&[0x80]);
        while self.fill != 56 {
            self.update(&[0]);
        }
        self.update(&bits.to_be_bytes());
        let mut out = [0u8; 32];
        for (o, v) in out.chunks_exact_mut(4).zip(self.h) {
            o.copy_from_slice(&v.to_be_bytes());
        }
        out
    }
}

/// De SHA-256 van `data` in één keer.
#[must_use]
pub fn sha256(data: &[u8]) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(data);
    h.finish()
}

/// De eerste acht bytes van een som als getal: de `bundle_sum` in het
/// handoff-blob ("flip naar deze bundel" mag geen eeuwige lus worden).
#[must_use]
pub fn sum64(sum: &[u8; 32]) -> u64 {
    let mut w = [0u8; 8];
    w.copy_from_slice(sum.get(..8).unwrap_or(&[0; 8]));
    u64::from_le_bytes(w)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::stage2::tests::SparseMem;
    use std::string::String;
    use std::vec;

    fn m(l: &str, s: &str) -> Mount {
        Mount {
            local: l.as_bytes().to_vec(),
            shared: s.as_bytes().to_vec(),
        }
    }

    fn st(slot: usize, base: u64, mib: u64, core: usize) -> SlotState {
        SlotState {
            slot,
            part_base: base,
            part_size: mib << 20,
            core,
            cores: 1,
            ..SlotState::default()
        }
    }

    #[test]
    fn handoff_round_trip() {
        let mut first = st(1, 0xBC00_0000, 64, 1);
        first.job = b"welcome".to_vec();
        first.ports = vec![8080, 443];
        first.mounts = vec![m("/data", "/data"), m("/shared/logs", "/logs")];
        let h = Handoff {
            old_base: 0x4000_0000,
            old_size: 0x0F00_0000,
            window: 0xA0E0_0000,
            total: 0x0F20_0000,
            generation: 2,
            slots: vec![first, st(7, 0x9000_0000, 48, 3)],
            ..Handoff::default()
        };
        let b = encode(&h, HANDOFF_TAIL).unwrap();
        assert_eq!(decode(&b).unwrap(), h);
    }

    #[test]
    fn handoff_weigert_onzin() {
        let h = Handoff {
            generation: 1,
            slots: vec![st(2, 0x9000_0000, 32, 2)],
            ..Handoff::default()
        };
        let good = encode(&h, HANDOFF_TAIL).unwrap();
        let breaks: [fn(&mut [u8]); 4] = [
            |b| b[0] ^= 0xFF,
            |b| b[8..16].copy_from_slice(&99u64.to_le_bytes()),
            |b| b[48..56].copy_from_slice(&(1u64 << 40).to_le_bytes()),
            |b| b[48..56].copy_from_slice(&4u64.to_le_bytes()),
        ];
        for brk in breaks {
            let mut b = good.clone();
            brk(&mut b);
            assert!(decode(&b).is_err());
        }
        assert!(decode(&good[..32]).is_err());
    }

    #[test]
    fn handoff_nat_round_trip() {
        let h = Handoff {
            generation: 1,
            nat: NatState {
                masq_next: 20345,
                gw_mac: [0x52, 0x54, 0x00, 0x12, 0x34, 0x56],
                gw_known: true,
                flows: vec![
                    FlowState {
                        proto: 6,
                        slot: 3,
                        fins: 2,
                        slot_port: 49152,
                        dst_port: 443,
                        node_port: 20001,
                        slot_ip: 0x0A64_0004,
                        dst_ip: 0x0808_0808,
                    },
                    FlowState {
                        proto: 17,
                        slot: 1,
                        fins: 0,
                        slot_port: 5353,
                        dst_port: 53,
                        node_port: 20002,
                        slot_ip: 0x0A64_0002,
                        dst_ip: 0x0A00_0203,
                    },
                ],
            },
            ..Handoff::default()
        };
        assert_eq!(decode(&encode(&h, HANDOFF_TAIL).unwrap()).unwrap(), h);
    }

    #[test]
    fn handoff_full_conntrack_fits() {
        let mut h = Handoff {
            generation: 1,
            ..Handoff::default()
        };
        for i in 0..MAX_FLOWS {
            h.nat.flows.push(FlowState {
                proto: 6,
                slot: (i % 64 + 1) as u8,
                slot_port: (30000 + i % 1000) as u16,
                dst_port: 443,
                node_port: (20000 + i % 9000) as u16,
                slot_ip: 0x0A64_0002,
                dst_ip: 0x0808_0808,
                fins: 0,
            });
        }
        for s in 1..=16 {
            let mut x = st(s, (s as u64) << 24, 64, s);
            x.job = b"some-job-name".to_vec();
            x.ports = vec![8080, 443];
            h.slots.push(x);
        }
        let b = encode(&h, HANDOFF_TAIL).unwrap();
        assert_eq!(decode(&b).unwrap().nat.flows.len(), MAX_FLOWS);
    }

    #[test]
    fn handoff_preserves_group_pool() {
        let mut s = st(4, 0, 0, 1);
        s.share_group = b"trusted".to_vec();
        s.group_cores = vec![1, 2, 3];
        let h = Handoff {
            slots: vec![s],
            ..Handoff::default()
        };
        let mut b = encode(&h, HANDOFF_TAIL).unwrap();
        let got = &decode(&b).unwrap().slots[0];
        assert_eq!(
            (got.share_group.as_slice(), got.group_cores.as_slice()),
            (&b"trusted"[..], &[1, 2, 3][..])
        );
        b[HAND_HEAD + 72..HAND_HEAD + 80].copy_from_slice(&u64::MAX.to_le_bytes());
        assert!(decode(&b).is_err(), "accepted impossible group length");
    }

    #[test]
    fn handoff_rejects_truncated_services() {
        let h = Handoff {
            agent: b"owners".to_vec(),
            ..Handoff::default()
        };
        let b = encode(&h, HANDOFF_TAIL).unwrap();
        for n in [HAND_HEAD, HAND_HEAD + 24, b.len() - 8] {
            assert!(decode(&b[..n]).is_err(), "accepted truncation at {n}");
        }
    }

    #[test]
    fn handoff_refuses_what_the_tail_cannot_hold() {
        let h = Handoff {
            agent: vec![0; MAX_AGENT_STATE + 1],
            ..Handoff::default()
        };
        assert!(encode(&h, HANDOFF_TAIL).is_err());
        assert!(encode(&Handoff::default(), 64).is_err());
    }

    const PLAN: FlipPlan = FlipPlan {
        handoff_ptr_pa: 0x1000,
        stage_pa: 0x2000,
        own_ram_end: 0x10_0000,
    };

    #[test]
    fn adopted_consumes_the_pair_before_trusting_it() {
        let mut mem = SparseMem::default();
        assert_eq!(adopted(&mut mem, &PLAN), Ok(Boot::Cold));
        // Een verdwaalde pointer: geconsumeerd, koude boot.
        mem.write64(PLAN.handoff_ptr_pa, 0x10_0000);
        mem.write64(PLAN.handoff_ptr_pa + 8, 0xdead);
        assert_eq!(adopted(&mut mem, &PLAN), Ok(Boot::Cold));
        assert!(!flip_pending(&mem, &PLAN));
        // Een geldige overdracht.
        let h = Handoff {
            generation: 3,
            slots: vec![st(2, 0x9000_0000, 32, 2)],
            ..Handoff::default()
        };
        let b = encode(&h, HANDOFF_TAIL).unwrap();
        mem.copy_in(PLAN.own_ram_end, &b);
        mem.write64(PLAN.handoff_ptr_pa, PLAN.own_ram_end);
        mem.write64(PLAN.handoff_ptr_pa + 8, HAND_MAGIC);
        assert!(flip_pending(&mem, &PLAN));
        assert_eq!(adopted(&mut mem, &PLAN), Ok(Boot::Adopted(h)));
        assert_eq!(mem.read64(PLAN.handoff_ptr_pa), 0, "pair not consumed");
        assert_eq!(
            adopted(&mut mem, &PLAN),
            Ok(Boot::Cold),
            "blob adopted twice"
        );
    }

    #[test]
    fn adopted_refuses_foreign_pointer_and_bad_blob() {
        let mut mem = SparseMem::default();
        mem.write64(PLAN.handoff_ptr_pa, 0x20_0000);
        mem.write64(PLAN.handoff_ptr_pa + 8, HAND_MAGIC);
        assert!(
            adopted(&mut mem, &PLAN).is_err(),
            "read a blob somebody else placed"
        );
        mem.write64(PLAN.handoff_ptr_pa, PLAN.own_ram_end);
        mem.write64(PLAN.handoff_ptr_pa + 8, HAND_MAGIC);
        assert!(
            adopted(&mut mem, &PLAN).is_err(),
            "garbage blob became a cold boot"
        );
        assert_eq!(
            stage_of(mem.read64(PLAN.stage_pa)).map(|s| s.0),
            Some(Stage::AdoptBlobBad)
        );
    }

    #[test]
    fn recorder_archives_foreign_generation_only() {
        let mut mem = SparseMem::default();
        // Een eerdere poging (generatie 4) sprong en landde nooit.
        stage(&mut mem, &PLAN, Stage::Jumping, 4);
        // Onze eigen sprong (generatie 5) is voortgang, geen mislukking.
        archive_stage(&mut mem, &PLAN, 4);
        assert_eq!(take_archived(&mut mem, &PLAN), None);
        archive_stage(&mut mem, &PLAN, 5);
        assert_eq!(take_archived(&mut mem, &PLAN), Some((Stage::Jumping, 4)));
        assert_eq!(take_archived(&mut mem, &PLAN), None, "archive not cleared");
        // Rommel is geen stand.
        mem.write64(PLAN.stage_pa, 0x1234_5678_0000_0003);
        assert_eq!(take_last_flip(&mut mem, &PLAN), None);
        stage(&mut mem, &PLAN, Stage::Jumping, 9);
        mark_early_boot(&mut mem, &PLAN);
        assert_eq!(take_last_flip(&mut mem, &PLAN), Some((Stage::EarlyMain, 9)));
        assert_eq!(mem.read64(PLAN.stage_pa), 0);
        assert!(Stage::NetUp.describe().contains("network"));
    }
    #[test]
    fn sha256_known_vectors() {
        let hex = |s: [u8; 32]| {
            s.iter()
                .map(|b| std::format!("{b:02x}"))
                .collect::<String>()
        };
        assert_eq!(
            hex(sha256(b"abc")),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(
            hex(sha256(b"")),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        // Over een blokgrens, in brokken: dezelfde som als in één keer.
        let long: Vec<u8> = (0..1000u32).map(|i| i as u8).collect();
        let mut h = Sha256::new();
        for c in long.chunks(37) {
            h.update(c);
        }
        assert_eq!(h.finish(), sha256(&long));
        assert_eq!(
            hex(sha256(
                b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"
            )),
            "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"
        );
    }

    /// Een minimale ELF64 met één PT_LOAD: `code` op `paddr`, `memsz` groot.
    fn mini_elf(entry: u64, paddr: u64, code: &[u8], memsz: u64) -> Vec<u8> {
        let mut e = vec![0u8; 128];
        e[..4].copy_from_slice(b"\x7fELF");
        e[4] = 2;
        e[5] = 1;
        e[6] = 1;
        e[16..18].copy_from_slice(&2u16.to_le_bytes());
        e[18..20].copy_from_slice(&183u16.to_le_bytes());
        e[20..24].copy_from_slice(&1u32.to_le_bytes());
        e[24..32].copy_from_slice(&entry.to_le_bytes());
        e[32..40].copy_from_slice(&64u64.to_le_bytes());
        e[52..54].copy_from_slice(&64u16.to_le_bytes());
        e[54..56].copy_from_slice(&56u16.to_le_bytes());
        e[56..58].copy_from_slice(&1u16.to_le_bytes());
        let ph = 64;
        e[ph..ph + 4].copy_from_slice(&1u32.to_le_bytes());
        e[ph + 4..ph + 8].copy_from_slice(&5u32.to_le_bytes());
        e[ph + 8..ph + 16].copy_from_slice(&128u64.to_le_bytes());
        e[ph + 16..ph + 24].copy_from_slice(&paddr.to_le_bytes());
        e[ph + 24..ph + 32].copy_from_slice(&paddr.to_le_bytes());
        e[ph + 32..ph + 40].copy_from_slice(&(code.len() as u64).to_le_bytes());
        e[ph + 40..ph + 48].copy_from_slice(&memsz.to_le_bytes());
        e[ph + 48..ph + 56].copy_from_slice(&0x1000u64.to_le_bytes());
        e.extend_from_slice(code);
        e
    }

    /// Een bundel zoals image/flip-bundle.sh hem schrijft.
    fn mini_bundle(
        elf: &[u8],
        abi: u32,
        link: u64,
        flat: u64,
        entry: u64,
        relocs: &[u32],
    ) -> Vec<u8> {
        let mut b = elf.to_vec();
        while !b.len().is_multiple_of(8) {
            b.push(0);
        }
        let head = b.len() as u64;
        b.extend_from_slice(&RELOC_MAGIC.to_le_bytes());
        b.extend_from_slice(&1u32.to_le_bytes());
        b.extend_from_slice(&abi.to_le_bytes());
        for v in [elf.len() as u64, link, flat, entry, relocs.len() as u64] {
            b.extend_from_slice(&v.to_le_bytes());
        }
        for r in relocs {
            b.extend_from_slice(&r.to_le_bytes());
        }
        while !b.len().is_multiple_of(8) {
            b.push(0);
        }
        b.extend_from_slice(&head.to_le_bytes());
        b.extend_from_slice(&RELOC_MAGIC.to_le_bytes());
        b
    }

    #[test]
    fn bundle_parses_flattens_and_lists_relocations() {
        let link = 0x6020_0000u64;
        let mut code = vec![0u8; 64];
        code[8..16].copy_from_slice(&(link + 0x20).to_le_bytes());
        let elf = mini_elf(link, link, &code, 0x2000);
        let b = mini_bundle(&elf, FLIP_ABI, link, 0x2000, link, &[8]);
        let bun = Bundle::parse(&b).unwrap();
        assert_eq!(
            (bun.flip_abi, bun.link_load, bun.flat_size, bun.entry),
            (FLIP_ABI, link, 0x2000, link)
        );
        assert_eq!(bun.relocs().collect::<Vec<_>>(), [8]);
        assert_eq!(bun.reloc_count(), 1);
        let mut mem = SparseMem::default();
        mem.write64(0x10_0000 + 0x1ff8, 0xdead); // rommel in de BSS
        assert_eq!(flatten(&bun, &mut mem, 0x10_0000).unwrap(), 1);
        assert_eq!(mem.read64(0x10_0008), link + 0x20);
        assert_eq!(mem.read64(0x10_1ff8), 0, "BSS not cleared");
    }

    #[test]
    fn bundle_refuses_what_it_cannot_jump_into() {
        let link = 0x4020_0000u64;
        let elf = mini_elf(link, link, &[0u8; 64], 0x1000);
        let good = mini_bundle(&elf, FLIP_ABI, link, 0x1000, link, &[0, 8]);
        assert!(Bundle::parse(&good).is_ok());
        // Een kale ELF, een afgekapte stroom, een omgevallen staart.
        assert!(Bundle::parse(&elf).is_err());
        assert!(Bundle::parse(&good[..good.len() - 1]).is_err());
        let bad: [(u64, u64, &[u32]); 5] = [
            (0x1000, link + 0x1000, &[]),   // entry buiten het beeld
            (0x1000, link + 2, &[]),        // entry niet uitgelijnd
            (0x1000, link, &[4]),           // relocatie niet uitgelijnd
            (0x1000, link, &[0x1000 - 4]),  // relocatie over de rand
            (MAX_FLAT + 0x1000, link, &[]), // een beeld dat geen kern is
        ];
        for (flat, entry, relocs) in bad {
            let b = mini_bundle(&elf, FLIP_ABI, link, flat, entry, relocs);
            assert!(
                Bundle::parse(&b).is_err(),
                "accepted {flat:#x} {entry:#x} {relocs:?}"
            );
        }
        // Een segment buiten het gedeclareerde beeld is een weigering bij het
        // neerleggen, niet een schrijf over de buren.
        let b = mini_bundle(&elf, FLIP_ABI, link, 0x800, link, &[]);
        let bun = Bundle::parse(&b).unwrap();
        assert!(flatten(&bun, &mut SparseMem::default(), 0x10_0000).is_err());
        // Twee entries die het oneens zijn: niet van één build.
        let b = mini_bundle(&elf, FLIP_ABI, link, 0x1000, link + 8, &[]);
        let bun = Bundle::parse(&b).unwrap();
        assert!(flatten(&bun, &mut SparseMem::default(), 0x10_0000).is_err());
    }
}
