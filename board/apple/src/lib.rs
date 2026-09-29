//! De Mac mini M4 (Apple t8132, target J773g, Mac16,10): 6 E-cores
//! "sawtooth" in cluster 0 en 4 P-cores "everest" in cluster 1, 24 GB
//! LPDDR5 vanaf 1 TiB, één Broadcom 57762 achter een Apple-PCIe-rootpoort,
//! de SSD achter de ANS-coprocessor. Geen PSCI, geen GIC, geen device tree
//! van de fabrikant: iBoot levert boot_args in x0 en een Apple Device Tree
//! (ADT), en m1n1 (Asahi) is tot de installatie de laag die ons afzet.
//!
//! Wat dit crate bezit, en waar:
//!
//! - de voorkant van het image ([`head`]): de relocatie-stub op offset 0x800
//!   (`kmutil --entry-point 2048`), de brievenbus op offset 0 (RVBAR), de
//!   ingang `_start_apple` met het VHE-regime en de 48-bit-map ([`mmu`]);
//! - de console ([`console`]): de dockchannel en uart0;
//! - de feiten van de firmware ([`fwinfo`]): boot_args, de ADT, de cores met
//!   hun soort, het MAC, het serienummer, en wat alleen de m1n1-loader weet;
//! - de interrupts ([`irq`]): de AIC als controller van `cpu::irq`, de
//!   timer-FIQ, de fast IPI;
//! - de cores ([`cores`]): de CPU_ON van dit board (m1n1's spin-table, of
//!   PMGR plus de brievenbus als wij het bootobject zijn);
//! - de PCIe ([`pcie`], [`pmgr`]): de controller zelf opbrengen, de link, de
//!   DART, en dan de tg3;
//! - de opslag en de thermometer ([`storage`]): de ANS-NVMe achter RTKit met
//!   het schrijfvenster uit de GPT, en de SMC;
//! - de watchdogs en de klok van de clusters ([`wdt`]);
//! - het slot-plan ([`slots`]).
//!
//! De Go-voorganger is `OLD/metal/board/apple` (plus `hop/`), en
//! `OLD/docs/v1/archief/apple-m4.md` is het dossier met elke meting.
//!
//! Wat de kern voor dit board draagt (29-09): de EL2-smaak `AppleVhe` in de
//! kooi-lijm en de OS-core-rotatie met de fast IPI als kick
//! (`cpu::el2::Bell::apple`), en de CPU_ON-haak van `cpu::smp`
//! ([`cores::cpu_on_mpidr`], er is geen PSCI). Wat er op ijzer nog bewezen
//! moet worden, staat als checklist in `docs/boards-apple.md`.

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

pub mod console;
pub mod cores;
pub mod fwinfo;
mod head;
mod irq;
mod mmu;
pub mod pcie;
pub mod pmgr;
pub mod slots;
pub mod storage;
pub mod wdt;

use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, Plan, Region};
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use cpu::el2::Flavor;
use cpu::println;
use dev::Pa;
use driver_tg3::Tg3;
use netdev::Mac;
use sync::Signal;

/// De schijf die `probe_disk` geeft: de ANS-NVMe. De binary noemt hem
/// `vboard::Disk`, zodat de geprobede schijf van de bench naar de opslag gaat
/// zonder dat de binary het type per board kent.
pub type Disk = storage::Disk;

/// De DRAM-basis van élk Apple-silicon-systeem sinds de M1 (GEMETEN 28-08:
/// 0x100_0000_0000).
pub const DRAM_BASE: u64 = 0x100_0000_0000;
/// Het venster van de kern: 4 GB boven de DRAM-basis, want daaronder woont
/// de boot-keten (boot_args en de ADT, m1n1 met zijn heap op ~DRAM + 56 MB)
/// en de bovenkant is van iBoot (carve-outs, de framebuffer). Het image is
/// hierop gelinkt (`hopos/link-apple.ld`).
pub const RAM_BASE: u64 = DRAM_BASE + 0x1_0000_0000;
/// De scratch van de stub (waar hij vandaan kwam, x0, de brievenbus).
pub const SCRATCH: u64 = RAM_BASE + 0xE000;
/// Het param-blok van de m1n1-loader.
pub const PARAMS: u64 = RAM_BASE + 0xE100;

/// De kern-RAM: image, BSS (met de tabellen), stack en heap. Normal WB.
pub const KERN_RAM: Region = Region {
    base: Pa(RAM_BASE),
    size: 0x1000_0000,
};
/// De DMA-regio: 16 MB, Normal-NC (GEMETEN 03-09: Device kost ~290 ns per
/// 8-byte-load; NC is coherent met de tg3 en de ANS zonder onderhoud).
pub const DMA: Region = Region {
    base: Pa(RAM_BASE + 0x1000_0000),
    size: 0x0100_0000,
};
/// De NIC-helft van de DMA-regio (de tg3 vraagt 4 MB).
pub const NET_DMA: Region = Region {
    base: Pa(RAM_BASE + 0x1000_0000),
    size: 0x0080_0000,
};
/// De opslag-helft: de queues en TCB's van de ANS, de databuffer, en de
/// buffers die de coprocessor bij zijn opstart vraagt.
pub const BLK_DMA: Region = Region {
    base: Pa(RAM_BASE + 0x1080_0000),
    size: 0x0080_0000,
};
/// Het kooi-venster: 16 MB Device (control-pages, kooien, de flip-recorder
/// en de zwarte doos).
pub const ADMIN: Region = Region {
    base: Pa(RAM_BASE + 0x1100_0000),
    size: 0x0100_0000,
};
/// De loader-regio: boot-scratch en staging, 64 MB Normal.
pub const LOADER: Region = Region {
    base: Pa(RAM_BASE + 0x1200_0000),
    size: 0x0400_0000,
};
/// Het einde van het venster; daarboven begint de bovenste pool-regio.
pub const WINDOW_END: u64 = RAM_BASE + 0x1600_0000;

const _: () = {
    assert!(KERN_RAM.end().0 == DMA.base.0 && DMA.end().0 == ADMIN.base.0);
    assert!(NET_DMA.end().0 == BLK_DMA.base.0 && BLK_DMA.end().0 == DMA.end().0);
    assert!(ADMIN.end().0 == LOADER.base.0 && LOADER.end().0 == WINDOW_END);
    assert!(RAM_BASE.is_multiple_of(2 << 20) && WINDOW_END.is_multiple_of(2 << 20));
    assert!(driver_tg3::DMA_NEED <= NET_DMA.size);
};

/// De EL2-smaak van dit silicium: E2H is RES1 (VHE-only, GEMETEN 28-08) en
/// WFE slaapt er op een core die wij opbrachten niet (CYC_OVRD vergrendeld),
/// dus de switcher slaapt in WFI en wekt met de fast IPI (GEMETEN 02-09).
/// De kooi-lijm van de binary moet déze smaak installeren, niet Nvhe.
pub const FLAVOR: Flavor = Flavor::AppleVhe;

static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);

/// De OS-core uit de config-waarde `v` op een board met `cores` cores en
/// klassen `class` (dezelfde regel als op virt en UEFI).
fn os_core_of(
    v: &str,
    cores: usize,
    class: impl Fn(usize) -> CoreClass,
) -> (usize, Option<&'static str>) {
    let want = match v {
        "" => return (usize::MAX, None),
        "small" => Some(CoreClass::Small),
        "mid" => Some(CoreClass::Mid),
        "big" => Some(CoreClass::Big),
        _ => None,
    };
    if let Some(k) = want {
        return match (0..cores).find(|c| class(*c) == k) {
            Some(c) => (c, None),
            None => (usize::MAX, Some("no core of that class")),
        };
    }
    match v.parse::<usize>() {
        Ok(n) if n < cores => (n, None),
        Ok(_) => (usize::MAX, Some("no such core")),
        Err(_) => (usize::MAX, Some("not small, mid, big or a core number")),
    }
}

/// Eén bootparameter uit `hopos.cfg` van de loader ("" als hij er niet
/// is): de bron van de bench en de knoppen van de kern.
#[must_use]
pub fn boot_param(key: &'static str) -> &'static str {
    fw::bootcfg::first(fw::bootcfg::all(fwinfo::config_text(), key))
}

/// De Mac mini M4.
pub struct Apple;

impl Apple {
    /// Het board. Alle staat staat in statics; dit is het handvat.
    #[must_use]
    pub const fn new() -> Self {
        Self
    }

    /// De fysieke index (ADT-volgorde) van de core waar dit draait.
    #[must_use]
    pub fn this_core(&self) -> usize {
        slots::core_of(arch::mpidr())
    }

    /// De OS-core die `hopos.cfg` vraagt (`hopos.oscore=<small|mid|big|N>`),
    /// met een reden als de kern er niet heen kan. De firmware levert ons af
    /// op een P-core (cpu 6); `small` is de zuinige core die HopOS hoort te
    /// bewonen (Go: "de hop", 29-08). De verhuizing start die core via
    /// `cpu::smp::start_one`, en dat is sinds 29-09 de CPU_ON van dit board
    /// ([`cores::cpu_on_mpidr`], gezet in `discover`).
    ///
    /// Let op (29-08): een core die de kern zelf opbracht, krijgt op t8132
    /// geen timer-FIQ. De kern draait daar dan in WFE (`sleeper` meet het
    /// opnieuw) en de voorproef van de kooien zakt op de timer-beurt
    /// (`HOPOS_APPLE_PREFLIGHT_FAIL`): de verhuizing kost de kooien, luid.
    #[must_use]
    pub fn os_core(&self) -> (usize, Option<&'static str>) {
        let here = self.this_core();
        let v = fw::bootcfg::first(fw::bootcfg::all(self.config(), "hopos.oscore"));
        match os_core_of(v, self.cores(), |c| self.core_class(c)) {
            (_, Some(why)) => (here, Some(why)),
            (usize::MAX, None) => (here, None),
            (w, None) => (w, None),
        }
    }

    /// De kick van de OS-core voor de rotatie van `cpu::el2`: de fast IPI
    /// naar de core waar de kern draait (IPI_RR_GLOBAL, geackt via IPI_SR op
    /// EL2), want Apple heeft geen GIC-SGI.
    #[must_use]
    pub fn os_bell(&self) -> cpu::el2::Bell {
        cpu::el2::Bell::apple(arch::mpidr())
    }

    /// De fast IPI naar deze core zelf: de zelftest van het IPI-pad.
    pub fn kick_self(&self) {
        cores::kick(arch::mpidr());
    }

    /// De CPU_ON van dit board (zie [`cores`]): core `core` op `entry` met
    /// `ctx` in x0.
    pub fn cpu_on(&self, core: usize, entry: u64, ctx: u64) -> Result<(), cores::Error> {
        cores::cpu_on(core, self.this_core(), entry, ctx)
    }

    /// `hopos.cfg`: het venster dat `image/apple-m4.sh` in het image bakte,
    /// of de tekst van de m1n1-loader ("" als er geen van beide is).
    #[must_use]
    pub fn config(&self) -> &'static str {
        fwinfo::config_text()
    }

    /// De schijf: de ANS-NVMe, met het schrijfvenster uit de GPT. Geen
    /// methode van [`Board`], net als op de andere boards.
    pub fn probe_disk(&self) -> Result<Option<storage::Disk>, Error> {
        storage::probe_disk(self.config())
    }

    /// De temperatuur van de die in milli-°C via de SMC; `None` = onbekend
    /// (alleen met `hopos.smc=1`). Elke aanroep praat de SMC wakker en weer
    /// in slaap (tot seconden, RTKit): niet voor een lus.
    #[must_use]
    pub fn temp_milli_c(&self) -> Option<i32> {
        storage::temp_milli_c(self.config())
    }

    /// De temperatuur in de bootlog: één meting met `hopos.smc=1`, anders
    /// een regel waarom niet. Niet standaard: onder de Go-kern kwam het
    /// INITIALIZE-antwoord van de SMC nooit (31-08), en een half opgestarte
    /// RTKit-coprocessor die niemand meer pollt loopt vol (driver_smc
    /// `open`). Dat is op ijzer niet uitgesloten, dus blijft het één
    /// bewuste knop per installatie.
    fn report_temp(&self, cfg: &str) {
        if fw::bootcfg::first(fw::bootcfg::all(cfg, "hopos.smc")) != "1" {
            println!(
                "smc: no die temperature (hopos.smc=1 measures once at boot; the SMC answer is unproven on metal, 31-08) HOPOS_APPLE_SMC_OFF"
            );
            return;
        }
        match self.temp_milli_c() {
            Some(t) => println!(
                "smc: die {}.{} C at boot HOPOS_APPLE_TEMP",
                t / 1000,
                (t % 1000).abs() / 100
            ),
            None => println!("smc: no die temperature from the SMC HOPOS_APPLE_SMC_FAIL"),
        }
    }

    /// De firmware-bootregels na `discover`.
    fn report(&self, x0: u64, args: Option<fw::xnuboot::Args>) {
        match args {
            Some(a) => println!(
                "xnuboot: rev {}.{} RAM {:#x}+{} MB (physical {} MB), firmware up to {:#x}, ADT {:#x}+{:#x}, framebuffer {:#x} {}x{} HOPOS_APPLE_BOOTARGS",
                a.revision,
                a.version,
                a.phys_base,
                a.mem_size >> 20,
                a.mem_size_actual >> 20,
                a.top_of_kernel_data,
                a.adt,
                a.adt_size,
                a.fb.base,
                a.fb.width,
                a.fb.height
            ),
            None => println!(
                "xnuboot: no boot_args at x0 {x0:#x}: no ADT, no cores, no devices HOPOS_APPLE_NO_BOOTARGS"
            ),
        }
        let Some(t) = fwinfo::adt() else { return };
        let p = (0..fwinfo::cpus())
            .filter(|&i| fwinfo::is_p_core(i))
            .count();
        println!(
            "adt: {} bytes, {} ({}), {} cores ({} E + {p} P), this is core {}, serial {}",
            t.size(),
            t.str(fw::adt::Node::ROOT, "model").unwrap_or("?"),
            t.str(fw::adt::Node::ROOT, "target-type").unwrap_or("?"),
            fwinfo::cpus(),
            fwinfo::cpus() - p,
            self.this_core(),
            fwinfo::serial().unwrap_or("?")
        );
        if let Some((d, _)) = fwinfo::reg("/arm-io/dockchannel-uart", 0)
            && d != console::DOCKCHANNEL
        {
            println!(
                "console: the ADT puts the dockchannel at {d:#x}, not {:#x} HOPOS_APPLE_CONSOLE_MISMATCH",
                console::DOCKCHANNEL
            );
        }
        match cores::own_cores() {
            Ok(()) => println!("cores: ours, RVBAR points at this image (PMGR + mailbox)"),
            Err(why) if fwinfo::has_params() => {
                println!("cores: via m1n1's spin-table ({why})");
            }
            Err(why) => println!("cores: NOT ours: {why}"),
        }
    }
}

impl Default for Apple {
    fn default() -> Self {
        Self::new()
    }
}

impl Board for Apple {
    type Nic = Tg3;
    type Sleeper = cpu::idle::ArmSleeper;

    const NAME: &'static str = "apple-m4";

    fn console(&self) -> fn(&[u8]) {
        console::write
    }

    fn firmware(&self) -> &'static str {
        "boot: iBoot or m1n1 (boot_args in x0, ADT), no PSCI, VHE-only EL2, 48-bit identity map"
    }

    fn init_heap(&self, heap: &Heap) {
        let (start, end) = arch::heap_bounds();
        // SAFETY: `hopos/link-apple.ld` legt `__heap_start` achter image,
        // BSS en stack en `__heap_end` op het einde van de kern-RAM; dat
        // bereik is Normal gemapt (`mmu::build`) en niemand anders gebruikt het.
        unsafe { heap.init(start, end) };
    }

    /// Leest boot_args en de ADT (x0), zet de watchdogs van de firmware
    /// stil (ALLEREERST: natief reset de node anders op 1:43, 31-08), zet
    /// de CPU_ON van dit board in `cpu::smp` (vóór de verhuizing naar de
    /// OS-core), en zet de clusters op hun klok.
    fn discover(&self, dtb: u64) {
        let x0 = if dtb != 0 {
            dtb
        } else {
            head::FIRMWARE_X0.load(Relaxed)
        };
        let args = fwinfo::load(x0);
        println!("watchdog: {}", wdt::quiet().as_str());
        self.report(x0, args);
        cpu::smp::set_cpu_on(cores::cpu_on_mpidr);
        let cfg = self.config();
        match fwinfo::config_source() {
            fwinfo::CfgSource::Image => println!(
                "cfg: hopos.cfg baked into the image, {} bytes HOPOS_CFG",
                cfg.len()
            ),
            fwinfo::CfgSource::Loader => println!(
                "cfg: hopos.cfg from the loader, {} bytes HOPOS_CFG",
                cfg.len()
            ),
            fwinfo::CfgSource::None => println!(
                "cfg: no hopos.cfg (none baked in by image/apple-m4.sh CFG=, no loader) HOPOS_CFG_NONE"
            ),
        }
        let ps = fw::bootcfg::first(fw::bootcfg::all(cfg, "hopos.pstate"));
        match wdt::pstate_targets(ps) {
            Some(t) => wdt::pstate_tune(t, false),
            None => wdt::pstate_tune(wdt::PS_DEFAULT, true),
        }
        self.report_temp(cfg);
    }

    fn clock(&self) -> executor::Clock {
        cpu::idle::now
    }

    /// WFI op de timer als de timer-FIQ deze core bereikt (gemeten, niet
    /// aangenomen: `irq::timer_wakes`), anders WFE op de event-stream
    /// (CNTFRQ 1 GHz: met FEAT_ECV kiest `cpu::idle` EVNTI 11 met EVNTIS,
    /// 1,048 ms, 954 wekken/s, GEMETEN 29-08).
    fn sleeper(&self) -> Self::Sleeper {
        let (fired, wakes) = irq::timer_wakes();
        let mode = if wakes {
            cpu::idle::Mode::Wfi
        } else {
            cpu::idle::Mode::Wfe
        };
        println!(
            "idle: timer fired={fired} fiq-at-core={wakes}, {} Hz, sleeping in {mode:?} HOPOS_APPLE_IDLE",
            cpu::idle::freq()
        );
        // Onder E2H = 1 schrijft `cntkctl_el1` vanaf EL2 CNTHCTL_EL2; sinds
        // 29-09 vervangt `cpu::idle` daar alleen de stream-bits, dus de
        // timertoegang van de bewoners (EL1PCTEN/EL1PTEN, de ingang in
        // head.rs) blijft staan en hoeft het board niets terug te zetten.
        cpu::idle::ArmSleeper::new(mode)
    }

    fn mem_total(&self) -> u64 {
        fwinfo::ram().3
    }

    fn cores(&self) -> usize {
        fwinfo::cpus().max(1)
    }

    /// E-cores ("sawtooth", `cluster-type` E) zijn small, P-cores
    /// ("everest") big.
    fn core_class(&self, core: usize) -> CoreClass {
        if fwinfo::is_p_core(core) || fwinfo::cpus() == 0 {
            CoreClass::Big
        } else {
            CoreClass::Small
        }
    }

    fn plan(&self) -> Plan {
        Plan {
            kern_ram: KERN_RAM,
            dma: DMA,
            net_dma: NET_DMA,
        }
    }

    fn start_interrupts(&self) -> Result<&'static Signal, Error> {
        let target = irq::start().map_err(Error::Irq)?;
        println!(
            "irq: {} (target {target} reaches this core), fast IPI and timer FIQ",
            irq::AIC.describe()
        );
        arch::unmask();
        Ok(&cpu::irq::IRQ_PENDING)
    }

    fn dispatch_interrupts(&self) -> Dispatched {
        let r = irq::dispatch();
        Dispatched {
            timer: r.timer,
            nic: r.lines,
            other: r.unknown.saturating_add(r.ipi),
        }
    }

    /// De hele keten: de PCIe-controller, de link, de DART, de brug en het
    /// endpoint, en dan de tg3 met het MAC uit de ADT (na een PERST draagt
    /// de chip alleen nog Broadcom's default). Gepold: de INTx-bedrading
    /// van Go (19-09) is nog niet geport.
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error> {
        if NIC_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_nic"));
        }
        let how = pcie::init().map_err(Error::Nic)?;
        println!("{how}");
        let ep = match pcie::enumerate_nic() {
            Ok(ep) => ep,
            Err(why) => {
                println!("net: {why}");
                return Ok(None);
            }
        };
        println!("apcie: link up, endpoint {} bar0 {:#x}", ep.f, ep.bar0);
        if !driver_tg3::drives(ep.f.vendor, ep.f.device) {
            println!("net: {} is not the tg3 we know", ep.f);
            return Ok(None);
        }
        let mac = fwinfo::nic_mac().ok_or(Error::Nic("no local-mac-address in the ADT"))?;
        // SAFETY: BAR0 is net toegewezen uit het 64-bit venster (onder 512 GB,
        // Device-gemapt) en `cfg` is de ECAM-config-space van deze functie;
        // NET_DMA is van deze driver alleen, Normal-NC, en de DART staat in
        // bypass, dus een DMA-adres is een fysiek adres.
        let mut nic = unsafe {
            Tg3::new(
                Pa(ep.bar0),
                Pa(ep.cfg),
                Mac(mac),
                NET_DMA.base,
                NET_DMA.size,
                cpu::idle::now,
            )
        }
        .map_err(|e| {
            println!("tg3: {e} HOPOS_TG3_FAIL");
            Error::Nic("tg3 init failed")
        })?;
        println!("tg3: {}", nic.describe());
        match nic.link_up(8_000_000_000) {
            Ok(l) => println!(
                "tg3: LINK UP, {} Mb/s {} duplex",
                l.mbps,
                if l.full_duplex { "full" } else { "half" }
            ),
            Err(e) => {
                println!("tg3: {e}, cable plugged in? HOPOS_TG3_NOLINK");
                return Err(Error::Nic("no link"));
            }
        }
        Ok(Some(nic))
    }
}

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
mod arch {
    use core::arch::asm;

    unsafe extern "C" {
        /// Het begin van de heap (`hopos/link-apple.ld`).
        static __heap_start: u8;
        /// Het einde van de heap: het einde van de kern-RAM.
        static __heap_end: u8;
    }

    pub(super) fn heap_bounds() -> (usize, usize) {
        (
            (&raw const __heap_start) as usize,
            (&raw const __heap_end) as usize,
        )
    }

    pub(super) fn mpidr() -> u64 {
        let v: u64;
        // SAFETY: MPIDR_EL1 lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, mpidr_el1", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn unmask() {
        // SAFETY: opent I en F op deze core; de vectoren staan (boot).
        unsafe { asm!("msr daifclr, #3", options(nomem, nostack)) };
    }
}

#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
mod arch {
    //! Host-stubs.
    pub(super) fn heap_bounds() -> (usize, usize) {
        (0, 0)
    }
    pub(super) fn mpidr() -> u64 {
        0x8001_0100
    }
    pub(super) fn unmask() {}
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_window_layout_is_consistent() {
        assert_eq!(KERN_RAM.end(), DMA.base);
        assert_eq!(slots::STAGE_PA + slots::STAGE_MAX, LOADER.end().0);
        assert_eq!(slots::DEVICE_WINDOW.base, ADMIN.base.0);
        assert_eq!(PARAMS, SCRATCH + 0x100);
    }

    #[test]
    fn the_os_core_follows_the_config() {
        // De M4: cpu0..5 E (small), cpu6..9 P (big).
        let class = |c: usize| {
            if c < 6 {
                CoreClass::Small
            } else {
                CoreClass::Big
            }
        };
        assert_eq!(os_core_of("", 10, class), (usize::MAX, None));
        assert_eq!(os_core_of("small", 10, class), (0, None));
        assert_eq!(os_core_of("big", 10, class), (6, None));
        assert_eq!(
            os_core_of("mid", 10, class).1,
            Some("no core of that class")
        );
        assert_eq!(os_core_of("12", 10, class).1, Some("no such core"));
        // Zonder boom: één core, big. (`os_core` leest het param-blok op
        // zijn vaste adres: dat is ijzer, geen host.)
        let a = Apple::new();
        assert_eq!(a.cores(), 1);
        assert_eq!(a.core_class(0), CoreClass::Big);
    }
}
