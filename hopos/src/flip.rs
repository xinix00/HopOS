//! De kern-flip in de kern-binary: de haak van `PrivOp::FLIP`, de sprong,
//! de landing en de flip-boot-guard (`OLD/metal/kern/kernflip/flip.go`,
//! `adopted.go`, en de gratie van `cmd/hopos/watchdog.go`). De procedure per
//! board, de markers en de faalmodi staan in `docs/flip.md`.
//!
//! De weg, in volgorde:
//!
//! 1. Hop haalt de bundel (`POST /flip` met URL en sha256) en stroomt hem
//!    rauw in een slot dat hij met `kern::system::FLIP_BUNDLE_JOB`
//!    reserveerde; dan `PrivOp::FLIP` met het slot en de som.
//! 2. [`prepare`] (de haak, synchroon): de som toetsen, de bundel lezen
//!    (`kern::kernflip::Bundle`), de som van zijn switch-code tegen de
//!    geïnstalleerde houden, de kern plat neerleggen in de staging en
//!    relokeren naar het koude adres (`cpu::el2::chain::relocate`). Elke
//!    weigering valt hier, vóór de sprong: Hop krijgt haar als antwoord op
//!    zijn FLIP, en deze kern draait gewoon door.
//! 3. De flip-taak ([`start`]) wacht even, zodat het antwoord op de FLIP
//!    Hop nog bereikt, en legt dan vast, in deze volgorde: hopfs bevriezen
//!    en committen (`storage::freeze_for_flip`), de conntrack van de
//!    switch-actor (`Command::SnapshotNat`, die ook de masquerade
//!    dichtzet), en de bewoners van de lifecycle-actor. Dan het
//!    handoff-blob, het pointer/magic-paar, de recorder op "springen", en
//!    de sprong (`cpu::el2::chain::chain`). Na de bewoners staat er geen
//!    `.await` meer: niemand kan er nog iets aan veranderen. Gaat er
//!    onderweg iets mis, dan ontdooien hopfs en de NAT weer.
//! 4. De nieuwe kern: [`land`] als eerste na de heap. Een overdracht is
//!    `HOPOS_FLIP_BOOT gen=N`; de NAT houdt de node-poorten van de oude
//!    flows vast vóór de switch zijn eerste ronde draait, de slots
//!    adopteren hun bewoners (`slots::start`, `HOPOS_FLIP_ADOPT`), daarna
//!    komt de conntrack terug (`HOPOS_FLIP_NAT`), en de guard ([`guard`])
//!    eist binnen [`GRACE`] adoptie en net, anders een koude herstart.
//!
//! De adressen zijn die van het board (`vboard::slots::FLIP_*`): de
//! staging, de recorder, de trampoline en het blob liggen buiten het
//! kern-RAM en buiten de pool. Het koude adres is het linkadres van het
//! board, behalve op UEFI: daar is de kern een PIE die de firmware ergens
//! neerlegde, en gaat de nieuwe kern op precies die basis (`__efi_head`),
//! binnen de maat die de firmware toen gaf.
//!
//! De flip heeft ÉÉN trigger: `PrivOp::FLIP` van Hop (`POST /flip` achter
//! dezelfde HMAC als een jobspec). De Go-kern had er ook een
//! console-commando en een `hopos.flip=<url>` bij boot voor, en die zijn
//! gesloopt: één weg, geen opties (Derek, 01-09). Beleid (wanneer, en
//! waarvandaan) is van Hop; de kern is mechanisme. Daarom bestaat er ook
//! geen `hopos.flip.cold`-sleutel in `hopos.cfg`.
//!
//! Wat hier bewust NIET gebeurt: een hardware-watchdog op QEMU (die is er
//! niet), en een "koude flip" die de bewoners stopt en koud herstart als
//! de switch-code niet past. Die weg hoort als vlag op de ene trigger
//! (`docs/flip.md`), en is hier niet gebouwd: na zo'n sprong moet de nieuwe
//! kern Hop koud plaatsen, en het image van Hop ligt in de staging, precies
//! waar de bundel nu ligt. Tot dan is een switch-code-verschil een koude
//! installatie (het image op het bootmedium en een herstart; hopfs houdt
//! de staat van Hop vast).

use crate::DevMem;
use abi::layout::{HANDOFF_MAGIC_OFF, HANDOFF_PTR_OFF};
use alloc::boxed::Box;
use alloc::vec::Vec;
use core::cell::Cell;
use core::sync::atomic::{AtomicBool, AtomicU64, Ordering::Relaxed};
use core::time::Duration;
use cpu::el2::chain::{self, Jump};
use cpu::println;
use dev::Pa;
use executor::Executor;
use kern::cage::PhysMem;
use kern::kernflip::{
    self, Boot, Bundle, FLIP_ABI, FlipPlan, HAND_MAGIC, HANDOFF_TAIL, Handoff, Stage,
};
use kern::slots::{Reply, Request, Response, SlotState};
use kern::system::FlipBundle;
use net::nat::{self as nat, MAX_ADOPT};
use net::switch::{Ack, Command, NatReply, NatSnapshot};
use sync::{Either, Local, Signal, select};
use vboard::slots::{
    BOOT_SCRATCH_PA, FLIP_HANDOFF_LEN, FLIP_HANDOFF_PA, FLIP_RECORDER_PA, FLIP_STAGE_MAX,
    FLIP_STAGE_PA, FLIP_TRAMP_PA,
};

// Het blob van de kern en de plek van het board zijn even groot: een
// nieuwe kern leest precies [`HANDOFF_TAIL`] bytes op [`FLIP_HANDOFF_PA`].
const _: () = assert!(FLIP_HANDOFF_LEN == HANDOFF_TAIL as u64);

/// Hoe lang de flip-taak na een geaccepteerde FLIP wacht: het antwoord moet
/// door de switch naar Hop, en Hop zijn 202 naar buiten. Op QEMU ruim
/// binnen 100 ms; de sprong is geen race die we willen winnen.
const JUMP_DELAY: Duration = Duration::from_millis(500);

/// Hoe lang de flip-taak op een actor (hopfs, de switch) wacht voordat hij
/// de flip opgeeft. Een actor die in twee seconden niet antwoordt, is een
/// kern die niet overgedragen hoort te worden.
const ACTOR_WAIT: Duration = Duration::from_secs(2);

/// De gratie van een geflipte kern: binnen deze tijd moeten de bewoners
/// geadopteerd en het net op zijn, anders komt de node koud terug. Twee
/// minuten, de les van de Go-kern ("een geflipte kern verliest na twee
/// minuten zijn gratie", watchdog.go): langer blind petten verbergt een
/// kern die hangt, korter verliest een trage DHCP.
pub(crate) const GRACE: Duration = Duration::from_secs(120);

/// PSCI SYSTEM_RESET (SMC32): de koude weg terug.
const PSCI_SYSTEM_RESET: u32 = 0x8400_0009;

/// Wat de firmware deze kern in x0 gaf: de nieuwe krijgt hetzelfde.
static FIRMWARE_X0: AtomicU64 = AtomicU64::new(0);
/// De generatie van deze kern: 1 na een koude boot, N+1 na een flip vanaf
/// generatie N (het blob draagt de generatie van de kern die landt).
static GENERATION: AtomicU64 = AtomicU64::new(1);
/// De som van de bundel waaruit deze kern kwam (0 = koud).
static SUM: AtomicU64 = AtomicU64::new(0);
/// Gezet door de slots na een geslaagde adoptie (de guard leest).
static ADOPTED: AtomicBool = AtomicBool::new(false);

/// Een klaargelegde flip: de nieuwe kern ligt gerelokeerd in de staging.
#[derive(Copy, Clone, Debug)]
struct Prepared {
    /// De maat van het platte beeld (8-uitgelijnd).
    len: u64,
    /// Het fysieke entrypoint op het koude adres.
    entry: u64,
    /// De som van de bundel ([`kernflip::sum64`]).
    sum: u64,
}

/// De klaargelegde flip, van de haak naar de flip-taak. Beide draaien op
/// de executor van de OS-core.
static PENDING: Local<Cell<Option<Prepared>>> = Local::new(Cell::new(None));
/// De conntrack uit het blob, van de landing naar de herstel-taak.
static LANDED_NAT: Local<Cell<Option<kernflip::NatState>>> = Local::new(Cell::new(None));
/// De bel van de flip-taak.
static BELL: Signal = Signal::new();
/// De antwoordplek van de flip-taak bij de lifecycle-actor (één aanroeper).
static REPLY: Reply = Reply::new();
/// De antwoordplek van de flip-taak bij de switch-actor (de snapshot).
static NAT_REPLY: NatReply = NatReply::new();
/// De bevestigingen van de NAT-commando's rond de flip (één aanroeper per
/// stuk: de flip-taak vóór de sprong, de herstel-taak erna).
static NAT_ACK: Ack = Ack::new();
static HOLD_ACK: Ack = Ack::new();

/// Waar de flip zijn woorden houdt.
fn plan() -> FlipPlan {
    FlipPlan {
        handoff_ptr_pa: BOOT_SCRATCH_PA + HANDOFF_PTR_OFF,
        stage_pa: FLIP_RECORDER_PA,
        own_ram_end: FLIP_HANDOFF_PA,
    }
}

/// De generatie van deze kern: 1 na een koude boot.
pub(crate) fn generation() -> u64 {
    GENERATION.load(Relaxed)
}

/// Het beeld van deze kern: waar het begint (het koude adres, en dus waar
/// de nieuwe heen gaat) en tot waar een nieuw beeld mag reiken.
///
/// Per board een eigen module (handboek §7: `cfg` op module-niveau): een
/// vast linkadres, of op UEFI de basis die de firmware koos.
#[cfg(not(any(feature = "board-uefi", feature = "board-o6n", feature = "board-altra")))]
mod image {
    use vboard::slots::{FLIP_IMAGE_END, FLIP_LINK_BASE, FLIP_PIE};

    const _: () = assert!(!FLIP_PIE && FLIP_LINK_BASE < FLIP_IMAGE_END);

    /// Het koude adres: het linkadres van het board.
    pub(super) fn base() -> u64 {
        FLIP_LINK_BASE
    }

    /// Tot waar een nieuw beeld mag reiken.
    pub(super) fn limit() -> u64 {
        FLIP_IMAGE_END
    }

    /// Het tweede veegvenster: leeg, het beeld ligt in het kern-RAM.
    pub(super) fn sweep() -> (u64, u64) {
        (0, 0)
    }
}

#[cfg(any(feature = "board-uefi", feature = "board-o6n", feature = "board-altra"))]
mod image {
    use vboard::slots::FLIP_PIE;

    const _: () = assert!(FLIP_PIE);

    unsafe extern "C" {
        /// Het begin van het PE-image (`hopos/efi.ld`, de header): de basis
        /// die de firmware koos.
        safe static __efi_head: u8;
        /// Het einde van het image met BSS en stack: `SizeOfImage`, dus
        /// precies wat de firmware toen alloceerde.
        safe static __image_end: u8;
    }

    /// Het koude adres: de basis van deze kern.
    pub(super) fn base() -> u64 {
        &raw const __efi_head as usize as u64
    }

    /// Een nieuw beeld moet in het oude passen: erachter ligt geheugen dat
    /// de firmware niet aan ons gaf, en dat nu misschien van de pool is.
    pub(super) fn limit() -> u64 {
        &raw const __image_end as usize as u64
    }

    /// Het tweede veegvenster: het beeld zelf, buiten het kernvenster.
    pub(super) fn sweep() -> (u64, u64) {
        (base(), limit())
    }
}

/// De landing, als eerste na de heap: een overdracht is er, of niet.
///
/// Een onbruikbaar blob met een geldig paar is GEEN koude boot: er leven
/// misschien bewoners, en een koude boot zou hun regio's vrij noemen. Op
/// ijzer wacht de kern dan op de watchdog; QEMU heeft er geen, dus een
/// PSCI-reset: de machine komt koud terug, zoals de watchdog dat ook deed.
pub(crate) fn land(x0: u64) -> Option<Handoff> {
    FIRMWARE_X0.store(x0, Relaxed);
    let (mut mem, p) = (DevMem, plan());
    kernflip::mark_early_boot(&mut mem, &p);
    match kernflip::adopted(&mut mem, &p) {
        Ok(Boot::Adopted(mut h)) => {
            GENERATION.store(h.generation, Relaxed);
            SUM.store(h.bundle_sum, Relaxed);
            println!(
                "flip: landed, generation {} from a {} MB kernel at {:#x}, {} resident(s), {} NAT flow(s), {} B agent state HOPOS_FLIP_BOOT gen={}",
                h.generation,
                h.old_size >> 20,
                h.old_base,
                h.slots.len(),
                h.nat.flows.len(),
                h.agent.len(),
                h.generation
            );
            // De conntrack is van de herstel-taak ([`start`]); de slots
            // krijgen de rest.
            LANDED_NAT.set(Some(core::mem::take(&mut h.nat)));
            Some(h)
        }
        Ok(Boot::Cold) => {
            report_cold(&mut mem, &p);
            None
        }
        Err(e) => {
            println!(
                "flip: the handoff blob does not decode: {e}; resetting cold HOPOS_FLIP_BLOB_BAD"
            );
            reset()
        }
    }
}

/// Een koude boot vertelt wat een eerdere flip achterliet: de laatste
/// poging die niet landde, en de lopende stand als de machine midden in
/// een flip omviel.
fn report_cold(mem: &mut DevMem, p: &FlipPlan) {
    if let Some((s, g)) = kernflip::take_archived(mem, p) {
        println!(
            "flip: an earlier flip (generation {g}) stopped at: {} HOPOS_FLIP_ARCHIVED",
            s.describe()
        );
    }
    if let Some((s, g)) = kernflip::take_last_flip(mem, p) {
        println!(
            "flip: the last flip (generation {g}) ended cold at: {} HOPOS_FLIP_LAST",
            s.describe()
        );
    }
}

/// De koude weg terug: PSCI SYSTEM_RESET. Keert niet terug; lukt de reset
/// niet, dan parkeert de core (de watchdog is de tweede lijn).
fn reset() -> ! {
    let _ = cpu::psci::smc(PSCI_SYSTEM_RESET, 0, 0, 0);
    cpu::boot::park()
}

/// De slots meldden een geslaagde adoptie.
pub(crate) fn adopted_ok() {
    ADOPTED.store(true, Relaxed);
}

/// De flip-boot-guard: een geflipte kern moet binnen [`GRACE`] zijn
/// bewoners geadopteerd en zijn net op hebben. Zo niet, dan PSCI-reset: een
/// halve kern met levende bewoners is erger dan een koude boot. Lukt het,
/// dan gaat de recorder leeg (een latere koude boot is geen mislukte flip).
///
/// De les van 06-09 (de M4): een guard die alleen "de kern draait" toetst,
/// liet een kern zonder net twee minuten petten en dan toch vallen; de
/// adoptie én het net zijn de voorwaarde, niet de executor.
///
/// Wat hij NIET dekt: een kern die hangt vóór de executor draait. Dat is
/// de hardware-watchdog, en QEMU virt heeft er geen.
pub(crate) async fn guard(exec: &'static Executor) {
    let deadline = exec.now().saturating_add(GRACE.as_nanos() as u64);
    let generation = generation();
    loop {
        if ADOPTED.load(Relaxed) && crate::net::uplink_ip().is_some() {
            let (mut mem, p) = (DevMem, plan());
            kernflip::stage(&mut mem, &p, Stage::NetUp, generation);
            let _ = kernflip::take_last_flip(&mut mem, &p);
            println!(
                "flip: generation {generation} settled, residents adopted and net up HOPOS_FLIP_SETTLED"
            );
            return;
        }
        if exec.now() >= deadline {
            println!(
                "flip: generation {generation} not settled within {} s (adopted={}), resetting cold HOPOS_FLIP_GUARD",
                GRACE.as_secs(),
                ADOPTED.load(Relaxed)
            );
            reset();
        }
        exec.after(Duration::from_millis(250)).await;
    }
}

/// Waarom een bundel vóór de sprong geweigerd werd: een korte reden voor
/// achter de marker (`HOPOS_FLIP_REFUSED <reden>`) en de fout voor Hop.
struct Refused {
    why: &'static str,
    err: kern::Error,
}

impl From<kern::Error> for Refused {
    fn from(err: kern::Error) -> Self {
        Refused {
            why: "bundle invalid",
            err,
        }
    }
}

fn refuse(why: &'static str, err: kern::Error) -> Refused {
    Refused { why, err }
}

/// De haak van `PrivOp::FLIP`: toetst de bundel en legt de nieuwe kern
/// klaar. Synchroon: na `Ok` geeft de system-API het slot terug.
pub(crate) fn prepare(b: &FlipBundle, sha256: &[u8; 32]) -> kern::Result {
    prepare_inner(b, sha256).map_err(|r| {
        println!(
            "flip: bundle in slot {} ({} bytes): {}; this kernel keeps running HOPOS_FLIP_REFUSED {}",
            b.slot, b.len, r.err, r.why
        );
        r.err
    })
}

fn prepare_inner(b: &FlipBundle, sha256: &[u8; 32]) -> Result<(), Refused> {
    let len = usize::try_from(b.len).map_err(|_| kern::Error::TooLarge {
        len: usize::MAX,
        max: 0,
    })?;
    // SAFETY: `[base, base+len)` is de partitie van een stroom die de
    // system-API tijdens deze aanroep in handen houdt (de grant is buiten
    // de actor: er draait niets in en niemand anders schrijft erin), en de
    // lengte is wat er gestroomd is, binnen de partitie (`Placer::raw`).
    // Het geheugen is pool-RAM, Normal gemapt in de identity map. Alleen
    // lezen, en de slice leeft niet langer dan deze functie.
    let bytes = unsafe { core::slice::from_raw_parts(b.base as usize as *const u8, len) };
    let sum = kernflip::sha256(bytes);
    if &sum != sha256 {
        return Err(refuse(
            "sha256 mismatch",
            kern::Error::Version {
                have: kernflip::sum64(&sum),
                want: kernflip::sum64(sha256),
            },
        ));
    }
    let sum64 = kernflip::sum64(&sum);
    if SUM.load(Relaxed) == sum64 {
        return Err(refuse("same bundle", kern::Error::Busy));
    }
    let bundle = Bundle::parse(bytes)?;
    if bundle.flip_abi != FLIP_ABI {
        return Err(refuse(
            "flip ABI mismatch",
            kern::Error::Version {
                have: u64::from(bundle.flip_abi),
                want: u64::from(FLIP_ABI),
            },
        ));
    }
    check_switch_code(&bundle)?;
    let base = image::base();
    let flat = bundle.flat_size.next_multiple_of(8);
    // Het beeld moet in de staging passen, en op het koude adres onder de
    // grens van het board blijven (virt: de DMA-regio, waar de NIC schrijft
    // tot de nieuwe kern hem reset; UEFI: het einde van het oude beeld).
    let room = image::limit().saturating_sub(base);
    if flat > FLIP_STAGE_MAX || flat > room {
        return Err(refuse(
            "image too large",
            kern::Error::TooLarge {
                len: usize::try_from(flat).unwrap_or(usize::MAX),
                max: usize::try_from(FLIP_STAGE_MAX.min(room)).unwrap_or(0),
            },
        ));
    }
    let generation = generation() + 1;
    let (mut mem, p) = (DevMem, plan());
    kernflip::archive_stage(&mut mem, &p, generation);
    kernflip::stage(&mut mem, &p, Stage::Fetched, generation);
    kernflip::stage(&mut mem, &p, Stage::BundleOk, generation);
    let segs = kernflip::flatten(&bundle, &mut mem, FLIP_STAGE_PA)?;
    kernflip::stage(&mut mem, &p, Stage::Placed, generation);
    let delta = base.wrapping_sub(bundle.link_load);
    let relocs = chain::relocate(Pa(FLIP_STAGE_PA), flat, bundle.relocs(), delta).map_err(|e| {
        println!("flip: {e} HOPOS_FLIP_RELOC");
        refuse("relocation", kern::Error::Corrupt { at: 0 })
    })?;
    kernflip::stage(&mut mem, &p, Stage::Rebased, generation);
    let entry = bundle.entry.wrapping_add(delta);
    println!(
        "flip: bundle ok, sha256 verified: {} KiB image in {segs} segment(s) linked at {:#x}, {relocs} relocation(s) to {base:#x}, entry {entry:#x} HOPOS_FLIP_STAGED",
        flat >> 10,
        bundle.link_load
    );
    PENDING.set(Some(Prepared {
        len: flat,
        entry,
        sum: sum64,
    }));
    BELL.set();
    Ok(())
}

/// De som van de switch-code van de bundel tegen die van de geïnstalleerde
/// kopie. De bewoners draaien IN die kopie; de nieuwe kern adopteert haar
/// alleen bij een gelijke som (`cpu::el2::adopt`). Een verschil hier is dus
/// een weigering vóór de sprong, niet twee minuten later een koude herstart
/// door de guard. Zonder geïnstalleerde kopie (geen slots op dit board) is
/// er niets te adopteren en geldt elke som.
fn check_switch_code(bundle: &Bundle<'_>) -> Result<(), Refused> {
    let Some(theirs) = bundle.switch_sum else {
        return Err(refuse(
            "bundle carries no switch code sum",
            kern::Error::Version { have: 0, want: 2 },
        ));
    };
    let Some(ours) = crate::slots::os_plan()
        .ok()
        .and_then(|p| cpu::el2::installed_hash(&p))
    else {
        return Ok(());
    };
    if theirs != ours {
        return Err(refuse(
            "switch code mismatch",
            kern::Error::Version {
                have: theirs,
                want: ours,
            },
        ));
    }
    println!("flip: switch code {ours:#018x} matches the residents HOPOS_FLIP_SWITCHCODE_OK");
    Ok(())
}

/// Spawnt de flip-taak en, na een landing, de guard en het herstel van de
/// conntrack. Aangeroepen vóór de executor draait: de claim op de
/// node-poorten ligt dan in de brievenbus van de switch vóór zijn eerste
/// ronde, dus geen antwoord van een oude peer valt bij de node-stack (die
/// zou een levende verbinding met een RST doden).
pub(crate) fn start(exec: &'static Executor, landed: bool) {
    if let Err(e) = exec.spawn(run(exec)) {
        println!("flip: task not spawned: {e:?} HOPOS_FLIP_SPAWN");
    }
    if !landed {
        return;
    }
    if let Err(e) = exec.spawn(guard(exec)) {
        println!("flip: guard not spawned: {e:?}, no grace check HOPOS_FLIP_SPAWN");
    }
    let Some(state) = LANDED_NAT.take() else {
        return;
    };
    hold_ports(&state);
    if let Err(e) = exec.spawn(restore_nat(exec, state)) {
        println!("flip: NAT restore not spawned: {e:?}, the conntrack is lost HOPOS_FLIP_SPAWN");
    }
}

/// Houdt de node-poorten van de overgedragen flows vast tot het herstel.
/// Meer dan [`MAX_ADOPT`] poorten: de eerste gaan vast, de rest niet, luid
/// (die flows komen wel terug, maar een antwoord vóór het herstel kan bij
/// de node-stack vallen).
fn hold_ports(state: &kernflip::NatState) {
    let mut ports: Vec<u16> = Vec::new();
    let n = state.flows.len().min(MAX_ADOPT);
    if ports.try_reserve_exact(n).is_err() {
        println!("flip: out of memory for the held NAT ports HOPOS_FLIP_NAT_HOLD");
        return;
    }
    ports.extend(state.flows.iter().take(n).map(|f| f.node_port));
    if state.flows.len() > n {
        println!(
            "flip: {} NAT flow(s), only the first {n} node ports held until the restore HOPOS_FLIP_NAT_HOLD",
            state.flows.len()
        );
    }
    // 'static voor de brievenbus: de heap is een bump-allocator, en dit is
    // één keer per boot.
    let ports: &'static [u16] = Box::leak(ports.into_boxed_slice());
    let cmd = Command::HoldAdoption {
        ports,
        ack: &HOLD_ACK,
    };
    if crate::net::COMMANDS.try_send(cmd).is_err() {
        println!("flip: switch mailbox full, NAT ports not held HOPOS_FLIP_NAT_HOLD");
    }
}

/// Het herstel van de conntrack na de landing: wachten tot de slots hun
/// bewoners adopteerden (hun `Attach` staat dan vóór ons in de brievenbus,
/// en `restore` houdt alleen flows van aangesloten slots), dan
/// `RestoreNat`, dan de claim op de poorten los.
async fn restore_nat(exec: &'static Executor, state: kernflip::NatState) {
    let total = state.flows.len();
    let deadline = exec.now().saturating_add(GRACE.as_nanos() as u64);
    while !ADOPTED.load(Relaxed) && exec.now() < deadline {
        exec.after(Duration::from_millis(20)).await;
    }
    let restored = if ADOPTED.load(Relaxed) {
        let mut flows: Vec<nat::FlowState> = Vec::new();
        if flows.try_reserve_exact(total).is_ok() {
            flows.extend(state.flows.iter().map(to_net));
        }
        // 'static voor de brievenbus, zoals de poorten.
        let flows: &'static [nat::FlowState] = Box::leak(flows.into_boxed_slice());
        let st = nat::NatState {
            flows,
            masq_next: state.masq_next,
            gw_mac: state.gw_known.then_some(state.gw_mac),
        };
        let send = || {
            let cmd = Command::RestoreNat {
                state: st,
                ack: &NAT_ACK,
            };
            crate::net::COMMANDS.try_send(cmd).is_ok()
        };
        match send_and_wait(exec, send, &NAT_ACK).await {
            Some(Ok(n)) => Some(n),
            Some(Err(e)) => {
                println!("flip: NAT restore refused: {e} HOPOS_FLIP_NAT_FAIL");
                None
            }
            None => None,
        }
    } else {
        println!("flip: residents not adopted, the conntrack is dropped HOPOS_FLIP_NAT_FAIL");
        None
    };
    // De claim los, ook na een mislukt herstel: anders krijgt geen enkele
    // bewoner ooit nog een nieuwe uitgaande verbinding.
    let finish = || {
        let cmd = Command::FinishAdoption { ack: &NAT_ACK };
        crate::net::COMMANDS.try_send(cmd).is_ok()
    };
    if send_and_wait(exec, finish, &NAT_ACK).await.is_none() {
        println!(
            "flip: NAT adoption not finished, outbound mapping stays closed HOPOS_FLIP_NAT_FAIL"
        );
    }
    if let Some(n) = restored {
        println!(
            "flip: conntrack restored, {n} of {total} NAT flow(s) carried over HOPOS_FLIP_NAT restored={n} of={total}"
        );
    }
}

/// Een commando naar de switch en wachten op zijn bevestiging, hooguit
/// [`ACTOR_WAIT`] (een switch die er niet is, antwoordt nooit). `send`
/// bouwt het commando en probeert het af te leveren (de ring-typen van de
/// brievenbus zijn van `net.rs`); een volle brievenbus probeert het even
/// opnieuw.
async fn send_and_wait(
    exec: &'static Executor,
    send: impl Fn() -> bool,
    ack: &'static Ack,
) -> Option<net::Result<u32>> {
    let _ = ack.try_take();
    let deadline = exec.now().saturating_add(ACTOR_WAIT.as_nanos() as u64);
    while !send() {
        if exec.now() >= deadline {
            println!("flip: switch mailbox full HOPOS_FLIP_NAT_FAIL");
            return None;
        }
        exec.after(Duration::from_millis(5)).await;
    }
    match select(ack.wait(), exec.after(ACTOR_WAIT)).await {
        Either::Left(r) => Some(r),
        Either::Right(()) => {
            println!(
                "flip: the switch did not answer within {} s HOPOS_FLIP_NAT_FAIL",
                ACTOR_WAIT.as_secs()
            );
            None
        }
    }
}

fn to_net(f: &kernflip::FlowState) -> nat::FlowState {
    nat::FlowState {
        proto: f.proto,
        slot: f.slot,
        fins: f.fins,
        slot_port: f.slot_port,
        dst_port: f.dst_port,
        node_port: f.node_port,
        slot_ip: f.slot_ip,
        dst_ip: f.dst_ip,
    }
}

fn to_kern(f: &nat::FlowState) -> kernflip::FlowState {
    kernflip::FlowState {
        proto: f.proto,
        slot: f.slot,
        fins: f.fins,
        slot_port: f.slot_port,
        dst_port: f.dst_port,
        slot_ip: f.slot_ip,
        dst_ip: f.dst_ip,
        node_port: f.node_port,
    }
}

/// De flip-taak: wacht op een klaargelegde flip en springt.
async fn run(exec: &'static Executor) {
    loop {
        BELL.wait().await;
        let Some(p) = PENDING.take() else { continue };
        exec.after(JUMP_DELAY).await;
        let e = jump(exec, p).await;
        println!("flip: not jumped: {e}; this kernel keeps running HOPOS_FLIP_FAIL");
        // Niet gesprongen is geen mislukte flip voor een latere boot: de
        // recorder gaat leeg (Go: `stageClear`).
        let _ = kernflip::take_last_flip(&mut DevMem, &plan());
    }
}

/// Waarom de sprong niet doorging.
enum JumpError {
    Kern(kern::Error),
    Nat(&'static str),
    Chain(chain::ChainError),
}

impl core::fmt::Display for JumpError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::Kern(e) => write!(f, "{e}"),
            Self::Nat(why) => write!(f, "conntrack: {why}"),
            Self::Chain(e) => write!(f, "{e}"),
        }
    }
}

/// Wat vóór de sprong bevroren is, en dus ontdooid moet worden als hij
/// niet doorgaat.
struct Frozen {
    fs: bool,
    nat: bool,
}

impl Frozen {
    /// De flip gaat niet door: hopfs en de NAT weer open, luid als het
    /// bericht niet aankomt.
    fn thaw(&self) {
        if self.fs && !crate::storage::thaw_after_flip() {
            println!("flip: hopfs mailbox full, the tree stays frozen HOPOS_FLIP_FAIL");
        }
        if self.nat {
            let _ = NAT_ACK.try_take();
            let cmd = Command::FinishAdoption { ack: &NAT_ACK };
            if crate::net::COMMANDS.try_send(cmd).is_err() {
                println!(
                    "flip: switch mailbox full, outbound mapping stays closed HOPOS_FLIP_FAIL"
                );
            }
        }
    }
}

/// Vastleggen, overdragen en springen. Keert alleen terug met een fout; het
/// pointer/magic-paar is dan weer weg, en hopfs en de NAT zijn ontdooid.
async fn jump(exec: &'static Executor, p: Prepared) -> JumpError {
    let mut frozen = Frozen {
        fs: false,
        nat: false,
    };
    let e = capture_and_jump(exec, p, &mut frozen).await;
    frozen.thaw();
    e
}

async fn capture_and_jump(exec: &'static Executor, p: Prepared, frozen: &mut Frozen) -> JumpError {
    // 1. De opslag: vastleggen en dicht. Wat na deze commit geschreven zou
    //    worden, ziet de nieuwe kern niet, dus er wordt niets meer
    //    geschreven.
    match crate::storage::freeze_for_flip(exec, ACTOR_WAIT).await {
        Ok(_) => frozen.fs = crate::storage::is_up(),
        Err(e) => return JumpError::Kern(e),
    }
    // 2. De conntrack, als waarde terug van de switch-actor.
    let snap = match snapshot_nat(exec).await {
        Ok(s) => {
            frozen.nat = s.is_some();
            s.unwrap_or_default()
        }
        Err(why) => return JumpError::Nat(why),
    };
    // 3. Het laatste `.await` van deze kern: de bewoners.
    let slots: Vec<SlotState> =
        match kern::slots::call(&crate::LIFECYCLE, &REPLY, Request::Snapshot).await {
            Ok(Response::Snapshot(s)) => s,
            Ok(Response::Failed(e)) | Err(e) => return JumpError::Kern(e),
            Ok(_) => return JumpError::Kern(kern::Error::Busy),
        };
    let mut flows: Vec<kernflip::FlowState> = Vec::new();
    if flows.try_reserve_exact(snap.flows.len()).is_err() {
        return JumpError::Kern(kern::Error::OutOfMemory {
            bytes: snap.flows.len() * 24,
        });
    }
    flows.extend(snap.flows.iter().map(to_kern));
    let nat = kernflip::NatState {
        masq_next: snap.masq_next,
        gw_mac: snap.gw_mac.unwrap_or_default(),
        gw_known: snap.gw_mac.is_some(),
        flows,
    };
    handoff_and_jump(p, slots, nat)
}

/// Vraagt de switch-actor om de conntrack (en zet daarmee de masquerade
/// dicht). `Ok(None)`: er is geen switch op deze node, dus ook niets om
/// mee te nemen.
async fn snapshot_nat(exec: &'static Executor) -> Result<Option<NatSnapshot>, &'static str> {
    if !crate::net::switch_up() {
        return Ok(None);
    }
    let mut buf: Vec<nat::FlowState> = Vec::new();
    if buf.try_reserve_exact(net::MAX_FLOWS).is_err() {
        return Err("out of memory for the snapshot buffer");
    }
    buf.resize(net::MAX_FLOWS, nat::FlowState::default());
    let cmd = Command::SnapshotNat {
        buf,
        reply: &NAT_REPLY,
    };
    if crate::net::COMMANDS.try_send(cmd).is_err() {
        return Err("switch mailbox full");
    }
    match select(NAT_REPLY.wait(), exec.after(ACTOR_WAIT)).await {
        Either::Left(s) => {
            println!(
                "flip: conntrack captured, {} NAT flow(s), outbound mapping closed until the jump HOPOS_FLIP_NAT_CAPTURED flows={}",
                s.flows.len(),
                s.flows.len()
            );
            Ok(Some(s))
        }
        Either::Right(()) => Err("the switch did not answer"),
    }
}

/// Het blob, het paar en de sprong. Synchroon: vanaf hier verandert er
/// niets meer aan de staat die overgaat.
fn handoff_and_jump(p: Prepared, slots: Vec<SlotState>, nat: kernflip::NatState) -> JumpError {
    let generation = generation() + 1;
    let (mut mem, fp) = (DevMem, plan());
    kernflip::stage(&mut mem, &fp, Stage::Captured, generation);
    let ram = vboard::KERN_RAM;
    let base = image::base();
    let h = Handoff {
        old_base: ram.base.0,
        old_size: ram.size,
        window: base,
        total: p.len,
        generation,
        bundle_sum: p.sum,
        slots,
        nat,
        agent: Vec::new(),
    };
    let blob = match kernflip::encode(&h, HANDOFF_TAIL) {
        Ok(b) => b,
        Err(e) => return JumpError::Kern(e),
    };
    mem.clear(FLIP_HANDOFF_PA, FLIP_HANDOFF_LEN);
    mem.copy_in(FLIP_HANDOFF_PA, &blob);
    dev::push(Pa(FLIP_HANDOFF_PA), HANDOFF_TAIL);
    // Het paar als laatste, en de regel zelf naar DRAM: hij deelt de
    // boot-scratch met woorden die straks met de MMU uit geschreven worden.
    let pair = BOOT_SCRATCH_PA + HANDOFF_PTR_OFF;
    mem.write64(pair, FLIP_HANDOFF_PA);
    mem.write64(BOOT_SCRATCH_PA + HANDOFF_MAGIC_OFF, HAND_MAGIC);
    dev::push(Pa(pair), 16);
    kernflip::stage(&mut mem, &fp, Stage::Handoff, generation);
    println!(
        "flip: {} KiB to {base:#x}, {} resident(s), {} NAT flow(s) and a {} B handoff, jumping to {:#x} HOPOS_FLIP_JUMP gen={generation}",
        p.len >> 10,
        h.slots.len(),
        h.nat.flows.len(),
        blob.len(),
        p.entry
    );
    kernflip::stage(&mut mem, &fp, Stage::Jumping, generation);
    let (a, b) = image::sweep();
    let j = Jump {
        dst: Pa(base),
        src: Pa(FLIP_STAGE_PA),
        len: p.len,
        entry: p.entry,
        x0: FIRMWARE_X0.load(Relaxed),
        sweep: [(ram.base, ram.end()), (Pa(a), Pa(b))],
        tramp: Pa(FLIP_TRAMP_PA),
    };
    // SAFETY: dit is de flip-taak op de OS-core, en na de sprong hoeft hier
    // niets meer te gebeuren. De staging draagt het complete beeld van een
    // bundel waarvan de som getoetst is (`prepare`), gerelokeerd naar het
    // koude adres; niemand schrijft er nog in (de firmware of QEMU legde er
    // alleen bij de boot iets neer). Het blob, het paar en de recorder zijn
    // net naar DRAM geveegd; hopfs is vastgelegd en dicht, de masquerade
    // dicht. De app-cores draaien in hun partities en in de switch-code in
    // de plan-regio, nooit in het kern-RAM of het kern-beeld dat de
    // trampoline veegt en overschrijft.
    let e = unsafe { chain::chain(&j) };
    mem.write64(pair, 0);
    mem.write64(BOOT_SCRATCH_PA + HANDOFF_MAGIC_OFF, 0);
    dev::push(Pa(pair), 16);
    JumpError::Chain(e)
}
