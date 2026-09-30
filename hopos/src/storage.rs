//! De opslag van de kern-binary: de schijf van het board, hopfs erop, de
//! hopfs-actor die de bestandscalls van de system-API bedient, en de
//! committer die de boom vastlegt (Go: `useFS` en `fsCommitEvery` in
//! `OLD/metal/cmd/hopos/fspersist.go`).
//!
//! Eigendom: de actor bezit de [`Fs`] en daarmee de driver (de schijf gaat
//! als waarde de actor in); wie iets wil, stuurt een bericht naar
//! [`FS_INBOX`]. De committer bezit niets: hij kijkt naar de
//! servicer-tabel en stuurt een `Commit`.
//!
//! Wachten op de schijf: de driver zit in een [`Paced`] met de klok en de
//! timers van de executor ([`ExecPace`]). Elk blok-verzoek is een submit plus
//! een await op de bel van de lijn (vangrail 10 ms) of, zonder lijn, pollend
//! op een timer, en intussen draait de executor door. Les van 30-09 (de
//! soak): de synchrone commit hield op een trage schijf de OS-core tot 7 s
//! stil (`late_ms=6611` op de tik, Hop en de switch zonder beurt). De mount
//! bij de boot is de uitzondering: die draait vóór `exec.run`, met
//! `blkdev::block_on`.
//!
//! De kern-flip: vóór de sprong legt de actor de boom vast en neemt hij
//! niets meer aan ([`freeze_for_flip`]); de nieuwe kern mount dezelfde
//! schijf en vindt precies die generatie.
//!
//! Stateful: een koude boot laadt de laatst vastgelegde boom (Go:
//! `hopos.storage=stateful`). Deze kern kent nog geen bootparameters, en
//! Hop's staat op `/hop/` moet een herstart overleven; wie leeg wil
//! beginnen, geeft QEMU een verse schijf (image/qemu-run.sh).

use blkdev::{Pace, Paced, block_on};
use board::Board;
use core::future::Future;
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use core::time::Duration;
use cpu::println;
use driver_virtioblk::{MAX_TRANSFER, SECTOR};
use executor::Executor;
use kern::cage::Timer;
use kern::hopfs::{Fs, Mounted};
use kern::rpc::{self, FsActor, FsInbox, committer};
use kern::slots::Reply;
use sync::mpsc::Mailbox;
use sync::{Either, select};

/// De brievenbus van de hopfs-actor: de verbindingstaken van de
/// system-API sturen er hun bestandscalls heen, de committer zijn commits.
pub(crate) static FS_INBOX: FsInbox<'static> = Mailbox::new();

/// Draait de hopfs-actor? Zonder actor valt er vóór een flip niets te
/// bevriezen.
static UP: AtomicBool = AtomicBool::new(false);

/// De antwoordplek van de flip-taak bij de actor (één aanroeper).
static FREEZE_REPLY: Reply = Reply::new();

/// Draait de hopfs-actor?
pub(crate) fn is_up() -> bool {
    UP.load(Relaxed)
}

/// De kern-flip vóór de sprong: de boom vastleggen en de actor dicht (elke
/// call daarna is `Busy`, luid; `HOPOS_FS_FROZEN`). Geeft de vastgelegde
/// generatie: de nieuwe kern mount precies die (`HOPOS_FS_UP fresh=0
/// generation=N`). `Ok(None)` zonder schijf. Een actor die niet binnen
/// `wait` antwoordt, of een commit die faalt, is een fout: dan geen flip,
/// want wat de apps schreven zou de nieuwe kern niet vinden.
///
/// Dat de actor tussen deze bevriezing en de sprong niets meer schrijft,
/// is de vorm: hij is de enige die de schijf aanraakt (PORT.md §3), en hij
/// weigert alles.
pub(crate) async fn freeze_for_flip(
    exec: &'static Executor,
    wait: Duration,
) -> kern::Result<Option<u64>> {
    if !is_up() {
        return Ok(None);
    }
    match select(rpc::freeze(&FS_INBOX, &FREEZE_REPLY), exec.after(wait)).await {
        Either::Left(Ok(g)) => Ok(Some(g)),
        Either::Left(Err(e)) => {
            println!("hopfs: not frozen for the flip: {e} HOPOS_FS_FREEZE_FAIL");
            Err(e)
        }
        Either::Right(()) => {
            println!(
                "hopfs: the actor did not freeze within {} ms HOPOS_FS_FREEZE_FAIL",
                wait.as_millis()
            );
            Err(kern::Error::Busy)
        }
    }
}

/// De flip ging niet door: de actor weer open. `false` als het bericht de
/// actor niet bereikte (volle brievenbus).
#[must_use]
pub(crate) fn thaw_after_flip() -> bool {
    !is_up() || rpc::thaw(&FS_INBOX)
}

/// De klok van de executor als `kern::cage::Timer`, voor de committer.
struct ExecTimer(&'static Executor);

impl Timer for ExecTimer {
    fn now(&self) -> u64 {
        self.0.now()
    }
    fn sleep(&self, d: Duration) -> impl Future<Output = ()> {
        self.0.after(d)
    }
}

/// De klok en de timers van de executor als `blkdev::Pace`: waarop de
/// hopfs-actor tijdens een blok-verzoek wacht.
pub(crate) struct ExecPace(pub(crate) &'static Executor);

impl Pace for ExecPace {
    type Sleep = executor::After<512, 256>;
    fn now(&self) -> u64 {
        self.0.now()
    }
    fn sleep(&self, d: Duration) -> Self::Sleep {
        self.0.after(d)
    }
}

/// Zoekt de schijf van het board, één keer voor de hele boot: `probe_disk`
/// mag maar één keer, en de schijf gaat eerst langs de bench (bench.rs)
/// en dan naar [`start`]. Geen schijf of een fout is één regel en `None`:
/// de node draait door en weigert elke bestandscall luid.
pub(crate) fn probe() -> Option<vboard::Disk> {
    match crate::BOARD.probe_disk() {
        Ok(Some(d)) => Some(d),
        Ok(None) => {
            println!("disk: no virtio-blk on this board, file calls refused HOPOS_DISK_NONE");
            None
        }
        Err(e) => {
            println!("disk: {e}, file calls refused HOPOS_DISK_FAIL");
            None
        }
    }
}

/// Zet de opslag op de geprobede schijf ([`probe`]): hopfs mounten, actor
/// en committer spawnen. Geeft `true` als de bestandscalls bediend worden;
/// zonder schijf draait de node door en weigert elke bestandscall luid (de
/// regel gaf [`probe`] al).
pub(crate) fn start(exec: &'static Executor, disk: Option<vboard::Disk>) -> bool {
    let board = &crate::BOARD;
    let Some(disk) = disk else {
        return false;
    };
    let sectors = disk.sectors();
    println!(
        "disk: up HOPOS_DISK_UP model={} blocks={sectors} block_size={SECTOR} max_transfer={MAX_TRANSFER}",
        disk.model()
    );
    // Vóór `exec.run`: de mount wacht zelf (block_on pollt het device), en
    // er is nog niemand die stil zou staan.
    let disk = Paced::new(disk, ExecPace(exec));
    let mounted = block_on(Fs::mount(
        disk,
        0,
        sectors,
        SECTOR,
        MAX_TRANSFER as u64,
        false,
    ));
    let (fs, found) = match mounted {
        Ok(m) => m,
        Err(e) => {
            println!("hopfs: mount failed: {e}, file calls refused HOPOS_FS_FAIL");
            return false;
        }
    };
    let generation = fs.generation();
    let (fresh, what) = match &found {
        Mounted::Restored { generation, slot } => {
            println!(
                "hopfs: tree restored, generation {generation} from place {slot} (stateful: a reboot keeps the volumes)"
            );
            (0, "restored")
        }
        Mounted::Empty => (1, "empty disk"),
        Mounted::Fresh => (1, "wiped"),
        Mounted::Volatile { blocks } => {
            println!(
                "hopfs: {blocks} blocks is too small to keep the tree, volatile HOPOS_FS_VOLATILE"
            );
            (1, "volatile")
        }
        Mounted::Inconsistent { generation, err } => {
            println!(
                "hopfs: tree of generation {generation} inconsistent: {err}, starting empty HOPOS_FS_INCONSISTENT"
            );
            (1, "inconsistent")
        }
    };
    println!(
        "hopfs: mounted {} MiB ({what}) HOPOS_FS_UP fresh={fresh} generation={generation}",
        (sectors * SECTOR) >> 20
    );
    let max_slots = board.cores().saturating_sub(1).max(1);
    let actor = async move {
        let mut a = FsActor::new(fs, &crate::SERVICERS, crate::KernConsole);
        a.run(&FS_INBOX).await;
    };
    if exec.spawn(actor).is_err() {
        println!("hopfs: actor not spawned, file calls refused HOPOS_FS_FAIL");
        return false;
    }
    UP.store(true, Relaxed);
    let commit = async move {
        committer(&crate::SERVICERS, &FS_INBOX, &ExecTimer(exec), max_slots).await;
    };
    if exec.spawn(commit).is_err() {
        // De calls werken; alleen het vastleggen niet. Luid, niet fataal.
        println!("hopfs: committer not spawned, the tree is not kept HOPOS_FS_COMMIT_FAIL");
    }
    true
}
