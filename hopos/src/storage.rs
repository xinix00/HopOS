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
//! Stateful: een koude boot laadt de laatst vastgelegde boom (Go:
//! `hopos.storage=stateful`). Deze kern kent nog geen bootparameters, en
//! Hop's staat op `/hop/` moet een herstart overleven; wie leeg wil
//! beginnen, geeft QEMU een verse schijf (image/qemu-run.sh).

use board::Board;
use core::future::Future;
use core::time::Duration;
use cpu::println;
use driver_virtioblk::{MAX_TRANSFER, SECTOR};
use executor::Executor;
use kern::cage::Timer;
use kern::hopfs::{Fs, Mounted};
use kern::rpc::{FsActor, FsInbox, committer};
use sync::mpsc::Mailbox;

/// De brievenbus van de hopfs-actor: de verbindingstaken van de
/// system-API sturen er hun bestandscalls heen, de committer zijn commits.
pub(crate) static FS_INBOX: FsInbox<'static> = Mailbox::new();

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

/// Zet de opslag op: schijf zoeken, hopfs mounten, actor en committer
/// spawnen. Geeft `true` als de bestandscalls bediend worden; zonder schijf
/// draait de node door en weigert elke bestandscall luid.
pub(crate) fn start(exec: &'static Executor) -> bool {
    let board = &crate::BOARD;
    let disk = match board.probe_disk() {
        Ok(Some(d)) => d,
        Ok(None) => {
            println!("disk: no virtio-blk on this board, file calls refused HOPOS_DISK_NONE");
            return false;
        }
        Err(e) => {
            println!("disk: {e}, file calls refused HOPOS_DISK_FAIL");
            return false;
        }
    };
    let sectors = disk.sectors();
    println!(
        "disk: up HOPOS_DISK_UP model={} blocks={sectors} block_size={SECTOR} max_transfer={MAX_TRANSFER}",
        disk.model()
    );
    let (fs, found) = match Fs::mount(disk, 0, sectors, SECTOR, MAX_TRANSFER as u64, false) {
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
    let commit = async move {
        committer(&crate::SERVICERS, &FS_INBOX, &ExecTimer(exec), max_slots).await;
    };
    if exec.spawn(commit).is_err() {
        // De calls werken; alleen het vastleggen niet. Luid, niet fataal.
        println!("hopfs: committer not spawned, the tree is not kept HOPOS_FS_COMMIT_FAIL");
    }
    true
}
