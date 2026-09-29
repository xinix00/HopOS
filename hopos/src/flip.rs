//! De kern-flip in de kern-binary: de haak van `PrivOp::FLIP`, de sprong,
//! de landing en de flip-boot-guard (`OLD/metal/kern/kernflip/flip.go`,
//! `adopted.go`, en de gratie van `cmd/hopos/watchdog.go`).
//!
//! De weg, in volgorde:
//!
//! 1. Hop haalt de bundel (`POST /flip` met URL en sha256) en stroomt hem
//!    rauw in een slot dat hij met `kern::system::FLIP_BUNDLE_JOB`
//!    reserveerde; dan `PrivOp::FLIP` met het slot en de som.
//! 2. [`prepare`] (de haak, synchroon): de som toetsen, de bundel lezen
//!    (`kern::kernflip::Bundle`), de kern plat neerleggen in de staging en
//!    relokeren naar het koude adres (`cpu::el2::chain::relocate`). Daarna
//!    geeft de system-API het slot terug; de bundel zelf is niet meer nodig.
//! 3. De flip-taak ([`start`]) wacht even, zodat het antwoord op de FLIP
//!    Hop nog bereikt, vraagt de lifecycle-actor om zijn bewoners, schrijft
//!    het handoff-blob en het pointer/magic-paar, zet de recorder op
//!    "springen" en springt (`cpu::el2::chain::chain`). Tussen de snapshot
//!    en de sprong staat geen `.await`: niemand kan er nog iets aan
//!    veranderen.
//! 4. De nieuwe kern: [`land`] als eerste na de heap. Een overdracht is
//!    `HOPOS_FLIP_BOOT gen=N`; de slots adopteren hun bewoners
//!    (`slots::start`, `HOPOS_FLIP_ADOPT`), en de guard ([`guard`]) eist
//!    binnen [`GRACE`] adoptie en net, anders een koude herstart.
//!
//! Wat hier bewust NIET gebeurt (zie het rapport): de conntrack gaat leeg
//! over (de switch heeft nog geen snapshot-bericht), de opslag wordt niet
//! bevroren (wat hopfs na de laatste commit schreef, ziet de nieuwe kern
//! niet), en er is geen hardware-watchdog op QEMU.
//!
//! De adressen zijn die van QEMU virt: de staging is de plek waar QEMU het
//! image van Hop legde (na de plaatsing van Hop dood geheugen), de
//! trampoline, de recorder en het blob liggen op de boot-scratch-pagina's
//! ertussen. Allemaal buiten het kern-RAM en buiten de pool.

use crate::DevMem;
use abi::layout::{HANDOFF_MAGIC_OFF, HANDOFF_PTR_OFF};
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
use sync::{Local, Signal};
use vboard::slots::{BOOT_SCRATCH_PA, STAGE_HDR_PA, STAGE_MAX, STAGE_PA};

/// De vluchtrecorder (twee woorden: de lopende stand en het archief), op
/// de boot-scratch-pagina maar buiten het deel dat `cpuinit` beschrijft.
const RECORDER_PA: u64 = BOOT_SCRATCH_PA + 0x1000;
/// De trampoline van de sprong: een pagina, uitvoerbaar (Normal) gemapt.
const TRAMP_PA: u64 = BOOT_SCRATCH_PA + 0x2000;
/// Het handoff-blob: de 256 KiB direct onder het staging-maatwoord. De
/// nieuwe kern leest de pointer alleen als hij PRECIES hier wijst.
const HANDOFF_PA: u64 = STAGE_HDR_PA - HANDOFF_TAIL as u64;

const _: () = assert!(RECORDER_PA >= BOOT_SCRATCH_PA + abi::layout::BOOT_SCRATCH_LEN);
const _: () = assert!(TRAMP_PA >= RECORDER_PA + 16 && TRAMP_PA + 0x1000 <= HANDOFF_PA);

/// Hoe lang de flip-taak na een geaccepteerde FLIP wacht: het antwoord moet
/// door de switch naar Hop, en Hop zijn 202 naar buiten. Op QEMU ruim
/// binnen 100 ms; de sprong is geen race die we willen winnen.
const JUMP_DELAY: Duration = Duration::from_millis(500);

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
/// de executor van core 0.
static PENDING: Local<Cell<Option<Prepared>>> = Local::new(Cell::new(None));
/// De bel van de flip-taak.
static BELL: Signal = Signal::new();
/// De antwoordplek van de flip-taak bij de lifecycle-actor (één aanroeper).
static REPLY: Reply = Reply::new();

/// Waar de flip zijn woorden houdt.
fn plan() -> FlipPlan {
    FlipPlan {
        handoff_ptr_pa: BOOT_SCRATCH_PA + HANDOFF_PTR_OFF,
        stage_pa: RECORDER_PA,
        own_ram_end: HANDOFF_PA,
    }
}

/// De generatie van deze kern: 1 na een koude boot.
pub(crate) fn generation() -> u64 {
    GENERATION.load(Relaxed)
}

/// Het koude adres van deze kern: waar zijn eigen beeld begint, en dus
/// waar de nieuwe heen gaat. `_start` staat vooraan (`.text.boot`).
fn own_base() -> u64 {
    unsafe extern "C" {
        fn _start();
    }
    _start as *const () as usize as u64
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
        Ok(Boot::Adopted(h)) => {
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

/// De haak van `PrivOp::FLIP`: toetst de bundel en legt de nieuwe kern
/// klaar. Synchroon: na `Ok` geeft de system-API het slot terug.
pub(crate) fn prepare(b: &FlipBundle, sha256: &[u8; 32]) -> kern::Result {
    let r = prepare_inner(b, sha256);
    if let Err(e) = &r {
        println!(
            "flip: bundle in slot {} ({} bytes) refused: {e} HOPOS_FLIP_REFUSED",
            b.slot, b.len
        );
    }
    r
}

fn prepare_inner(b: &FlipBundle, sha256: &[u8; 32]) -> kern::Result {
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
        return Err(kern::Error::Version {
            have: kernflip::sum64(&sum),
            want: kernflip::sum64(sha256),
        });
    }
    let sum64 = kernflip::sum64(&sum);
    if SUM.load(Relaxed) == sum64 {
        println!("flip: this kernel already came from that bundle HOPOS_FLIP_SAME");
        return Err(kern::Error::Busy);
    }
    let bundle = Bundle::parse(bytes)?;
    if bundle.flip_abi != FLIP_ABI {
        return Err(kern::Error::Version {
            have: u64::from(bundle.flip_abi),
            want: u64::from(FLIP_ABI),
        });
    }
    let base = own_base();
    let flat = bundle.flat_size.next_multiple_of(8);
    // Het beeld moet in de staging passen, en op het koude adres onder de
    // DMA-regio blijven (daar schrijft de NIC tot de nieuwe kern hem reset).
    let dma = vboard::DMA.base.0;
    if flat > STAGE_MAX || base.saturating_add(flat) > dma {
        return Err(kern::Error::TooLarge {
            len: usize::try_from(flat).unwrap_or(usize::MAX),
            max: usize::try_from(STAGE_MAX.min(dma - base)).unwrap_or(0),
        });
    }
    let generation = generation() + 1;
    let (mut mem, p) = (DevMem, plan());
    kernflip::archive_stage(&mut mem, &p, generation);
    kernflip::stage(&mut mem, &p, Stage::Fetched, generation);
    kernflip::stage(&mut mem, &p, Stage::BundleOk, generation);
    let segs = kernflip::flatten(&bundle, &mut mem, STAGE_PA)?;
    kernflip::stage(&mut mem, &p, Stage::Placed, generation);
    let delta = base.wrapping_sub(bundle.link_load);
    let relocs = chain::relocate(Pa(STAGE_PA), flat, bundle.relocs(), delta).map_err(|e| {
        println!("flip: {e} HOPOS_FLIP_RELOC");
        kern::Error::Corrupt { at: 0 }
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

/// Spawnt de flip-taak en, na een landing, de guard.
pub(crate) fn start(exec: &'static Executor, landed: bool) {
    if let Err(e) = exec.spawn(run(exec)) {
        println!("flip: task not spawned: {e:?} HOPOS_FLIP_SPAWN");
    }
    if landed && let Err(e) = exec.spawn(guard(exec)) {
        println!("flip: guard not spawned: {e:?}, no grace check HOPOS_FLIP_SPAWN");
    }
}

/// De flip-taak: wacht op een klaargelegde flip en springt.
async fn run(exec: &'static Executor) {
    loop {
        BELL.wait().await;
        let Some(p) = PENDING.take() else { continue };
        exec.after(JUMP_DELAY).await;
        let e = jump(p).await;
        println!("flip: not jumped: {e}; this kernel keeps running HOPOS_FLIP_FAIL");
        // Niet gesprongen is geen mislukte flip voor een latere boot: de
        // recorder gaat leeg (Go: `stageClear`).
        let _ = kernflip::take_last_flip(&mut DevMem, &plan());
    }
}

/// Waarom de sprong niet doorging.
enum JumpError {
    Kern(kern::Error),
    Chain(chain::ChainError),
}

impl core::fmt::Display for JumpError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::Kern(e) => write!(f, "{e}"),
            Self::Chain(e) => write!(f, "{e}"),
        }
    }
}

/// Vastleggen, overdragen en springen. Keert alleen terug met een fout; het
/// pointer/magic-paar is dan weer weg.
async fn jump(p: Prepared) -> JumpError {
    // Het laatste `.await` van deze kern: de bewoners.
    let slots: Vec<SlotState> =
        match kern::slots::call(&crate::LIFECYCLE, &REPLY, Request::Snapshot).await {
            Ok(Response::Snapshot(s)) => s,
            Ok(Response::Failed(e)) | Err(e) => return JumpError::Kern(e),
            Ok(_) => return JumpError::Kern(kern::Error::Busy),
        };
    let generation = generation() + 1;
    let (mut mem, fp) = (DevMem, plan());
    kernflip::stage(&mut mem, &fp, Stage::Captured, generation);
    let ram = vboard::KERN_RAM;
    let h = Handoff {
        old_base: ram.base.0,
        old_size: ram.size,
        window: own_base(),
        total: p.len,
        generation,
        bundle_sum: p.sum,
        slots,
        // TODO: de conntrack; de switch heeft nog geen snapshot-bericht.
        nat: kernflip::NatState::default(),
        agent: Vec::new(),
    };
    let blob = match kernflip::encode(&h, HANDOFF_TAIL) {
        Ok(b) => b,
        Err(e) => return JumpError::Kern(e),
    };
    mem.clear(HANDOFF_PA, HANDOFF_TAIL as u64);
    mem.copy_in(HANDOFF_PA, &blob);
    dev::push(Pa(HANDOFF_PA), HANDOFF_TAIL);
    // Het paar als laatste, en de regel zelf naar DRAM: hij deelt de
    // boot-scratch met woorden die straks met de MMU uit geschreven worden.
    let pair = BOOT_SCRATCH_PA + HANDOFF_PTR_OFF;
    mem.write64(pair, HANDOFF_PA);
    mem.write64(BOOT_SCRATCH_PA + HANDOFF_MAGIC_OFF, HAND_MAGIC);
    dev::push(Pa(pair), 16);
    kernflip::stage(&mut mem, &fp, Stage::Handoff, generation);
    let base = own_base();
    println!(
        "flip: {} KiB to {base:#x}, {} resident(s) and a {} B handoff, jumping to {:#x} HOPOS_FLIP_JUMP gen={generation}",
        p.len >> 10,
        h.slots.len(),
        blob.len(),
        p.entry
    );
    kernflip::stage(&mut mem, &fp, Stage::Jumping, generation);
    let j = Jump {
        dst: Pa(base),
        src: Pa(STAGE_PA),
        len: p.len,
        entry: p.entry,
        x0: FIRMWARE_X0.load(Relaxed),
        sweep: (ram.base, ram.end()),
        tramp: Pa(TRAMP_PA),
    };
    // SAFETY: dit is de flip-taak op core 0, en na de sprong hoeft hier
    // niets meer te gebeuren. De staging draagt het complete beeld van een
    // bundel waarvan de som getoetst is (`prepare`), gerelokeerd naar het
    // koude adres; niemand schrijft er nog in (QEMU legde er alleen bij de
    // boot iets neer). Het blob, het paar en de recorder zijn net naar DRAM
    // geveegd. De app-cores draaien in hun partities en in de switch-code
    // in de plan-regio (0xC200_0000), nooit in het kern-RAM dat de
    // trampoline veegt en overschrijft.
    let e = unsafe { chain::chain(&j) };
    mem.write64(pair, 0);
    mem.write64(BOOT_SCRATCH_PA + HANDOFF_MAGIC_OFF, 0);
    dev::push(Pa(pair), 16);
    JumpError::Chain(e)
}
