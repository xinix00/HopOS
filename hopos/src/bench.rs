//! De meetbanken van de kern, aan met een bootparameter: `hopos.nvmebench=1`
//! (de schijf rauw en door hopfs, Go: `nvmeBench` en `hopfsBench` in
//! `OLD/metal/cmd/hopos/nvmebench.go`) en `hopos.idlestat=1` (de
//! idle-meetlat van de OS-core, Go: `idleStat` in `node.go`).
//!
//! Dit module bezit niets dat na zijn eigen aanroep blijft, behalve de
//! idlestat-taak; die leest alleen atomics (handboek §1.3).
//!
//! # De schijf in een bench-boot
//!
//! `probe_disk` mag één keer, dus de opslag probet (`storage::probe`), de
//! bench leent de schijf vóór de opslag hem mount en geeft hem terug
//! ([`start`]), en `storage::start` neemt hem aan. Zo mount een bench-boot
//! daarna gewoon, zoals in Go (`HOPOS_FS_UP` na `HOPOS_NVMEBENCH_DONE`).
//!
//! De bench schrijft alleen in de staart van de schijf: de helft, hoogstens
//! 1 GiB. hopfs legt zijn boom aan het begin en alloceert van voren, dus de
//! staart is op een schijf die niet vol is van niemand; maar een meetbank
//! die ongevraagd op de schijf schrijft is geen bench maar een bug, dus
//! alleen met de sleutel.

use alloc::string::String;
use alloc::vec::Vec;
use blkdev::{Blocking, block_on};
use core::sync::atomic::Ordering::Relaxed;
use cpu::println;
use driver_virtioblk::{MAX_TRANSFER, SECTOR};
use executor::Executor;
use kern::hopfs::{BlockDevice, Fs};

/// De staart die de bench hoogstens beschrijft.
const SPAN_MAX: u64 = 1 << 30;

/// Per commandomaat van de Go-tabel: 16 MiB (Go: `16<<20`).
const PER_SIZE: u64 = 16 << 20;

/// De commandomaten van de Go-tabel.
const SIZES: [u64; 4] = [4 << 10, 64 << 10, 256 << 10, 1 << 20];

/// Hoogstens zoveel willekeurige 4 KiB-opdrachten (64 MiB).
const RANDOM_OPS: u64 = 16384;

/// Eén bootparameter: de eerste waarde van `key`, of "" als hij niet gezet
/// is. De tekst komt van het board (`src::text`): de FDT-bootargs op virt en
/// de Pi's, `hopos.cfg` op de UEFI-boards, en beide op de Radxa.
pub(crate) fn bootparam(dtb: u64, key: &'static str) -> String {
    src::param(dtb, key)
}

/// Start wat de bootparameters vragen: de schijf-bench (synchroon, nu, op
/// de geprobede schijf `disk`) en de idlestat-taak. Geeft de schijf terug
/// voor `storage::start`: de bench leent hem alleen.
pub(crate) fn start(
    exec: &'static Executor,
    dtb: u64,
    mut disk: Option<vboard::Disk>,
) -> Option<vboard::Disk> {
    let idle = bootparam(dtb, "hopos.idlestat");
    if !idle.is_empty() && idle != "0" {
        // `1` is elke seconde; een ander getal is de periode in seconden.
        let every = idle.parse::<u64>().unwrap_or(1).clamp(1, 3600);
        match exec.spawn(idlestat(exec, every)) {
            Ok(()) => println!("bench: idlestat every {every} s HOPOS_IDLESTAT_ON"),
            Err(_) => println!("bench: idlestat not spawned HOPOS_IDLESTAT_FAIL"),
        }
    }
    if bootparam(dtb, "hopos.nvmebench") == "1" {
        match disk.as_mut() {
            Some(d) => bench_disk(exec, d),
            None => println!("nvme bench: no disk on this board, skipped HOPOS_NVMEBENCH_NONE"),
        }
    }
    disk
}

/// De idle-meetlat van de OS-core, per `every` seconden: wekken en
/// polls van de executor, de interrupts, de lege RX-rondes, het werk van
/// de switch, en de rotatie van de OS-core (in, terug op irq/ipi/timer/
/// yield, de tijd van de bewoners, de kicks). Tempo's per seconde, plus één
/// regel met de cumulatieve drops. Dit is het getal waarop de NIC-interrupt
/// en de slaap afgerekend worden (Go: gepold ~3.300 wekken/s bij stilte,
/// op de interrupt ~100).
async fn idlestat(exec: &'static Executor, every: u64) {
    let hz = cpu::idle::freq().max(1);
    let mut prev = Snap::take(exec);
    let mut n: u64 = 0;
    let start = exec.now();
    loop {
        n += 1;
        exec.until(start.saturating_add(n.saturating_mul(every).saturating_mul(1_000_000_000)))
            .await;
        let now = Snap::take(exec);
        let dt = now.at.saturating_sub(prev.at).max(1);
        let r = |a: u64, b: u64| a.saturating_sub(b).saturating_mul(1_000_000_000) / dt;
        let res_ns = now.os[7]
            .saturating_sub(prev.os[7])
            .saturating_mul(1_000_000_000)
            / hz;
        let res_pm = res_ns.saturating_mul(1000) / dt;
        println!(
            "idle: {} wakes/s, {} polls/s, {} irq/s (timer {}, nic {}, other {}), {} empty rx rounds/s; switch by door {}/s, by failsafe {}/s; os core {} in/s, back on irq {}/s ipi {}/s timer {}/s yield {}/s, residents {}.{}%, idle rounds {}/s, kicks {}/s HOPOS_IDLESTAT",
            r(now.exec[0], prev.exec[0]),
            r(now.exec[1], prev.exec[1]),
            r(now.irq.iter().sum(), prev.irq.iter().sum()),
            r(now.irq[0], prev.irq[0]),
            r(now.irq[1], prev.irq[1]),
            r(now.irq[2], prev.irq[2]),
            r(now.net[0], prev.net[0]),
            r(now.net[1], prev.net[1]),
            r(now.net[2], prev.net[2]),
            r(now.os[0], prev.os[0]),
            r(now.os[1], prev.os[1]),
            r(now.os[2], prev.os[2]),
            r(now.os[3], prev.os[3]),
            r(now.os[4], prev.os[4]),
            res_pm / 10,
            res_pm % 10,
            r(now.os[8], prev.os[8]),
            r(now.os[9], prev.os[9]),
        );
        let s = &crate::net::STATS;
        let h = crate::HEAP.stats();
        println!(
            "idle: totals rx full {}, rx drops {}, uplink drops rx {} tx {}, nat oversize {} no-route {} flow-full {}, nic tx errors {}, host drops rx {} tx {}; os exits {} faults {}; kern heap {} KB used, {} leaked, {} refused",
            s.rx_full.load(Relaxed),
            s.rx_drops.load(Relaxed),
            s.uplink_rx_drops.load(Relaxed),
            s.uplink_tx_drops.load(Relaxed),
            s.nat_oversize.load(Relaxed),
            s.nat_no_route.load(Relaxed),
            s.nat_flow_full.load(Relaxed),
            s.nic_tx_errors.load(Relaxed),
            s.host_rx_drops.load(Relaxed),
            s.host_tx_drops.load(Relaxed),
            now.os[5],
            now.os[6],
            h.used >> 10,
            h.leaked,
            h.refused,
        );
        prev = now;
    }
}

/// Eén foto van alle tellers die idlestat leest.
struct Snap {
    /// Nu, in ns op de klok van de executor.
    at: u64,
    /// Executor: sleeps (de wekken), polls.
    exec: [u64; 2],
    /// De IRQ-dispatch: timer, nic, other.
    irq: [u64; 3],
    /// Het netwerkvlak: lege RX-rondes, werk na de bel, werk na de failsafe.
    net: [u64; 3],
    /// De OS-core: in, irq, ipi, timer, yield, exit, fault, ticks, idle, kicks.
    os: [u64; 10],
}

impl Snap {
    fn take(exec: &'static Executor) -> Self {
        let s = &exec.stats;
        let o = &cpu::el2::OS_STATS;
        let n = &crate::net::STATS;
        Self {
            at: exec.now(),
            exec: [s.sleeps.load(Relaxed), s.polls.load(Relaxed)],
            irq: [
                crate::IRQS[0].load(Relaxed),
                crate::IRQS[1].load(Relaxed),
                crate::IRQS[2].load(Relaxed),
            ],
            net: [
                n.rx_idle.load(Relaxed),
                n.work_by_door.load(Relaxed),
                n.work_by_timer.load(Relaxed),
            ],
            os: [
                o.entries.load(Relaxed),
                o.irq.load(Relaxed),
                o.ipi.load(Relaxed),
                o.timer.load(Relaxed),
                o.yields.load(Relaxed),
                o.exits.load(Relaxed),
                o.faults.load(Relaxed),
                o.ticks.load(Relaxed),
                o.idle.load(Relaxed),
                o.kicks.load(Relaxed),
            ],
        }
    }
}

/// De schijf-bench: rauw (de Go-tabel per commandomaat, dan sequentieel
/// en willekeurig over de staart) en door hopfs. Synchroon: de executor
/// draait nog niet, en een bench-boot wacht erop, zoals in Go.
fn bench_disk(exec: &'static Executor, disk: &mut vboard::Disk) {
    let sectors = disk.sectors();
    let bytes = sectors.saturating_mul(SECTOR);
    // De staart: de helft van de schijf, hoogstens 1 GiB, op hele MiB.
    let span = (bytes / 2).min(SPAN_MAX) & !((1 << 20) - 1);
    if span < 4 << 20 {
        println!("nvme bench: disk of {bytes} bytes is too small, skipped HOPOS_NVMEBENCH_FAIL");
        return;
    }
    let base = sectors - span / SECTOR;
    let mut buf: Vec<u8> = Vec::new();
    if buf.try_reserve_exact(MAX_TRANSFER).is_err() {
        println!("nvme bench: no heap for a {MAX_TRANSFER}-byte buffer HOPOS_NVMEBENCH_FAIL");
        return;
    }
    buf.resize(MAX_TRANSFER, 0);
    for (i, b) in buf.iter_mut().enumerate() {
        *b = (i as u8).wrapping_mul(7);
    }
    println!(
        "nvme bench: {} ({} MB), writing the tail LBA {base}..{} ({} MB); the disk goes to the storage after HOPOS_NVMEBENCH_START",
        disk.model(),
        bytes / 1_000_000,
        sectors - 1,
        span >> 20
    );
    let t = Bench { exec, base };
    let ok = t.sizes(disk, &mut buf, span)
        && t.sequential(disk, &mut buf, span)
        && t.random(disk, &mut buf, span);
    if ok {
        t.hopfs(disk, &mut buf, span);
    }
    println!("nvme bench: done HOPOS_NVMEBENCH_DONE");
}

/// De staart en de klok van één bench-run.
struct Bench {
    exec: &'static Executor,
    base: u64,
}

impl Bench {
    /// Nu, in ns.
    fn now(&self) -> u64 {
        self.exec.now()
    }

    /// `n` opdrachten van `sz` bytes schrijven, dan lezen, opdracht `k` op
    /// LBA `lba(k)`. Geeft (schrijf-ns, lees-ns), of `None` na een luide
    /// fout.
    fn pass<D: BlockDevice>(
        &self,
        disk: &mut D,
        buf: &mut [u8],
        sz: usize,
        n: u64,
        lba: impl Fn(u64) -> u64,
    ) -> Option<(u64, u64)> {
        let chunk = buf.get_mut(..sz)?;
        let t0 = self.now();
        for k in 0..n {
            if let Err(e) = disk.write(lba(k), chunk) {
                println!(
                    "nvme bench: write {sz} at {}: {e} HOPOS_NVMEBENCH_FAIL",
                    lba(k)
                );
                return None;
            }
        }
        let t1 = self.now();
        for k in 0..n {
            if let Err(e) = disk.read(lba(k), chunk) {
                println!(
                    "nvme bench: read {sz} at {}: {e} HOPOS_NVMEBENCH_FAIL",
                    lba(k)
                );
                return None;
            }
        }
        Some((t1.saturating_sub(t0), self.now().saturating_sub(t1)))
    }

    /// De Go-tabel: per commandomaat 16 MiB schrijven en lezen (dezelfde
    /// regelvorm als `nvmeBench`, zodat de getallen naast elkaar passen).
    fn sizes<D: BlockDevice>(&self, disk: &mut D, buf: &mut [u8], span: u64) -> bool {
        for sz in SIZES {
            if sz > MAX_TRANSFER as u64 {
                continue;
            }
            let n = PER_SIZE.min(span) / sz;
            let step = sz / SECTOR;
            let base = self.base;
            let Some((w, r)) = self.pass(disk, buf, sz as usize, n, |k| base + k * step) else {
                return false;
            };
            line("nvme bench:", sz, n, w, r, "cmd");
        }
        true
    }

    /// Sequentieel over de hele staart in opdrachten van 1 MiB.
    fn sequential<D: BlockDevice>(&self, disk: &mut D, buf: &mut [u8], span: u64) -> bool {
        let sz = MAX_TRANSFER as u64;
        let n = span / sz;
        let step = sz / SECTOR;
        let base = self.base;
        let Some((w, r)) = self.pass(disk, buf, MAX_TRANSFER, n, |k| base + k * step) else {
            return false;
        };
        println!(
            "nvme bench: sequential {} MiB in {} KiB commands: write {} MB/s, read {} MB/s HOPOS_NVMEBENCH_SEQ",
            span >> 20,
            sz >> 10,
            rate(span, w),
            rate(span, r)
        );
        true
    }

    /// Willekeurig: 4 KiB-opdrachten op pseudo-willekeurige 4 KiB-plekken
    /// in de staart (xorshift, vast zaad: elke run dezelfde plekken).
    fn random<D: BlockDevice>(&self, disk: &mut D, buf: &mut [u8], span: u64) -> bool {
        let sz: u64 = 4 << 10;
        let slots = span / sz;
        let n = slots.min(RANDOM_OPS);
        let (base, step) = (self.base, sz / SECTOR);
        let at = |k: u64| {
            let mut x = k.wrapping_add(0x9e37_79b9_7f4a_7c15);
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            base + (x % slots) * step
        };
        let Some((w, r)) = self.pass(disk, buf, sz as usize, n, at) else {
            return false;
        };
        let iops = |ns: u64| n.saturating_mul(1_000_000_000) / ns.max(1);
        println!(
            "nvme bench: random 4 KiB x{n}: write {} IOPS ({} MB/s), read {} IOPS ({} MB/s) HOPOS_NVMEBENCH_RAND",
            iops(w),
            rate(n * sz, w),
            iops(r),
            rate(n * sz, r)
        );
        true
    }

    /// Door hopfs: een vluchtige bestandslaag op dezelfde staart, 16 MiB
    /// in calls van 1 MiB en 64 KiB (Go: `hopfsBench`). Het verschil met
    /// de rauwe regels is wat hopfs zelf kost, zonder servicer en
    /// transport. Synchroon, vóór de executor draait: de schijf achter
    /// `Blocking`, elke call met `block_on` afgedraaid.
    fn hopfs<D: BlockDevice>(&self, disk: &mut D, buf: &mut [u8], span: u64) {
        let disk = Blocking(disk);
        let mut fs = match Fs::new(disk, self.base, span / SECTOR, SECTOR, MAX_TRANSFER as u64) {
            Ok(f) => f,
            Err(e) => {
                println!("hopfs bench: {e} HOPOS_NVMEBENCH_FAIL");
                return;
            }
        };
        let path: &[u8] = b"/.bench/hopfs.bin";
        if let Err(e) = fs.mkdir_all(b"/.bench") {
            println!("hopfs bench: mkdir: {e} HOPOS_NVMEBENCH_FAIL");
            return;
        }
        for sz in [1u64 << 20, 64 << 10] {
            let n = PER_SIZE.min(span / 2) / sz;
            let Some(chunk) = buf.get_mut(..sz as usize) else {
                return;
            };
            let t0 = self.now();
            for k in 0..n {
                if let Err(e) = block_on(fs.write_at(path, k * sz, chunk)) {
                    println!("hopfs bench: write: {e} HOPOS_NVMEBENCH_FAIL");
                    return;
                }
            }
            let t1 = self.now();
            for k in 0..n {
                if let Err(e) = block_on(fs.read_at(path, k * sz, chunk)) {
                    println!("hopfs bench: read: {e} HOPOS_NVMEBENCH_FAIL");
                    return;
                }
            }
            let (w, r) = (t1.saturating_sub(t0), self.now().saturating_sub(t1));
            line("hopfs bench:", sz, n, w, r, "call");
        }
    }
}

/// Eén regel van de Go-tabel: maat, aantal, en per richting MB/s en µs
/// per opdracht.
fn line(what: &str, sz: u64, n: u64, w: u64, r: u64, per: &str) {
    let us = |ns: u64| ns / 1000 / n.max(1);
    println!(
        "{what} {:4} KiB x{n:<4} write {:>6} MB/s ({:5} us/{per}), read {:>6} MB/s ({:5} us/{per}) HOPOS_NVMEBENCH",
        sz >> 10,
        rate(n * sz, w),
        us(w),
        rate(n * sz, r),
        us(r)
    );
}

/// Decimale MB/s met één decimaal, als tekst (geen float in de kern).
fn rate(bytes: u64, ns: u64) -> String {
    let tenths = u128::from(bytes) * 10_000 / u128::from(ns.max(1));
    alloc::format!("{}.{}", tenths / 10, tenths % 10)
}

/// De bron van de bootparameters op virt en de Pi's: de bootargs in de
/// FDT (QEMU `-append`, de `cmdline.txt` van de Pi).
#[cfg(any(
    feature = "board-qemuvirt",
    feature = "board-rpi4",
    feature = "board-rpi5"
))]
mod src {
    use alloc::string::String;
    use alloc::vec::Vec;

    /// De grootste DTB die gelezen wordt (die van de Pi 5 is ~80 KB).
    const DTB_MAX: usize = 1 << 20;

    pub(super) fn param(dtb: u64, key: &'static str) -> String {
        let Some(blob) = copy(dtb).or_else(|| copy(fallback())) else {
            return String::new();
        };
        let Ok(f) = fw::fdt::Fdt::new(&blob) else {
            return String::new();
        };
        let args = f.bootargs().unwrap_or("");
        String::from(fw::bootcfg::first(fw::bootcfg::cmdline(args, key)))
    }

    /// Waar QEMU de DTB legt als x0 leeg is (een ELF-kern).
    #[cfg(feature = "board-qemuvirt")]
    fn fallback() -> u64 {
        board_qemuvirt::DTB_FALLBACK.0
    }

    /// De Pi's krijgen hem altijd in x0.
    #[cfg(not(feature = "board-qemuvirt"))]
    fn fallback() -> u64 {
        0
    }

    /// Een kopie van de DTB op `pa`, als daar een geldige kop staat: eerst
    /// de acht kopbytes per woord via `dev`, dan de gedeclareerde maat. Het
    /// board las hem bij boot op dezelfde plek (`discover`).
    fn copy(pa: u64) -> Option<Vec<u8>> {
        if pa == 0 || !pa.is_multiple_of(8) {
            return None;
        }
        let mut head = [0u8; 8];
        dev::copy_out(&mut head, dev::Pa(pa));
        let total = fw::fdt::total_size(&head)?;
        if total > DTB_MAX {
            return None;
        }
        let mut blob = Vec::new();
        blob.try_reserve_exact(total).ok()?;
        blob.resize(total, 0);
        dev::copy_out(&mut blob, dev::Pa(pa));
        Some(blob)
    }
}

/// De bron op de UEFI-boards: `hopos.cfg` van de ESP.
#[cfg(any(feature = "board-uefi", feature = "board-o6n", feature = "board-altra"))]
mod src {
    use alloc::string::String;

    pub(super) fn param(_dtb: u64, key: &'static str) -> String {
        String::from(fw::bootcfg::first(fw::bootcfg::all(
            crate::BOARD.config(),
            key,
        )))
    }
}

/// De bron op de Radxa: het board leest `hopos.cfg` (de initrd) en de
/// bootargs zelf. De riscv64-boards leveren dezelfde `boot_param`.
#[cfg(any(
    feature = "board-rk3566",
    feature = "board-qemuvirt-riscv",
    feature = "board-licheerv",
    feature = "board-apple"
))]
mod src {
    use alloc::string::String;

    pub(super) fn param(_dtb: u64, key: &'static str) -> String {
        String::from(vboard::boot_param(key))
    }
}
