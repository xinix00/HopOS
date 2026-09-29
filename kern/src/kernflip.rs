//! De kern-flip: de overdracht (het handoff-blob), de adoptie door de nieuwe
//! kern ([`adopted`]), en de vluchtrecorder met zijn archief.
//!
//! Alles in het blob is BOEKHOUDING, geen inhoud: de app-werelden blijven
//! staan waar ze staan. Bij een onbruikbare overdracht stopt de nieuwe boot;
//! de allocator mag mogelijk levende eigenaren nooit als vrije ruimte
//! behandelen (E8). Het herstel van de claims zelf is
//! [`crate::slots::Lifecycle::adopt`].
//!
//! Het downloaden, plaatsen en relokeren van de bundel en de sprong zijn
//! van `cpu` en het board (zie het rapport, "niet geport").

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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::stage2::tests::SparseMem;
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
}
