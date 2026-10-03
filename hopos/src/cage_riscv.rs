//! De kooi-lijm op riscv64: de traits van `kern::cage` over
//! `cpu::riscv::{pmp, sv39, switch, boot}`, `dev` en de executor van hart 0.
//!
//! De riscv-helft van `hopos/src/cage.rs` (`ArmCage`), met wat ze delen in
//! `glue.rs`, en bedoeld om ernaast gelezen te worden: dezelfde plichten,
//! andere letters (Go, `OLD/metal/kern/slots/cage_riscv64.go`):
//!
//! ```text
//! ARM (cage.rs)                     RISC-V (dit bestand)
//! stage-2-tabel + VMID              PMP-whitelist (TOR) + Sv39 in de staart
//! trampoline, x0 = control-page     koude boot door de switcher, a0 = page
//! mailbox + SEV, koud PSCI CPU_ON   BootPending in de lijst + msip
//! revoke: tabel nul + TLBI          CTX_REVOKE: de switcher (yield, tick,
//!                                   rotatie) zet hem dood; of het resetblok
//! OS-core (Hop naast de kern)       dezelfde vorm: `cpu::riscv::oscore`
//! SMP-eenheden, sharegroepen        één bewoner per app-hart
//! ```
//!
//! Dit bezit de per-slot wetenschap die de kern niet heeft en de switcher
//! niet mag hebben: waar de control-page van een levende kooi staat en op
//! welk hart hij draait. De boekhouding (wie welke partitie en core heeft)
//! is van de lifecycle-actor; die krijgt [`RvCage`] en [`RvCores`] als
//! waarde en is dus de enige aanroeper (`&mut self`, handboek §1).
//!
//! Wie wat schrijft (de regel van de cachelines, `abi::layout::SchedBlock`):
//! de kern schrijft van het sched-blok alleen regel 1..3 (lijst, lengte, de
//! wekker, de bel, de slaap- en tickperiode) en van het ctx-blok alles vóór
//! BootPending, daarna alleen `CTX_REVOKE` (eigen regel); de switcher de
//! rest. Op de C906 zijn de harts niet coherent (30-07), dus elke schrijf
//! van hier gaat met `dev::push` naar DRAM en elke lees van een
//! switcher-woord met `dev::pull`.

use crate::glue::{app_ram, attach, code, detach, err, publish_ports, tail_of, unpublish_ports};
use abi::hopabi::{
    AppStatus, CTRL_CORES, CTRL_ENTRY, CTRL_EXIT_CODE, CTRL_FAULT_ESR, CTRL_FAULT_FAR,
    CTRL_FAULT_VEC, CTRL_HEARTBEAT, CTRL_IDLE, CTRL_IDLE_MODE, CTRL_KILL, CTRL_MEM_SYS,
    CTRL_RAM_SIZE, CTRL_SLOT, CTRL_SMP_REQ, CTRL_STATUS, CTRL_TIMEBASE_HZ, CTRL_WAKES,
    CTRL_WALL_OFF, IDLE_KICK, IDLE_YIELD,
};
use abi::layout::{
    self, ABI_CTRL_OFF, ABI_MAP_PAGES, ABI_TAIL, CTRL_STRIDE, CTX_BOOT_ARG, CTX_BOOT_PC,
    CTX_CTRL_PA, CTX_LEN, CTX_REGIME, CTX_REVOKE, CTX_RING_HEAD_PA, CTX_STATE, CTX_UNIT_SLOT,
    CtxState, LINK_BASE, NET_RING_DATA_CAP, PARK_MBOX_LEN, Plan, RING_DATA_CAP, SCHED_CLINT_PA,
    SCHED_COUNT, SCHED_CURRENT, SCHED_LIST, SCHED_MBOX_CTX, SCHED_MSIP_PA, SCHED_OFF_PC,
    SCHED_OS_BELL, SCHED_S2_PA, SCHED_SLEEP_CAP, SCHED_TICK_TICKS, Tail,
};
use abi::ring;
use core::future::Future;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use cpu::el2::{self, CoreState};
use cpu::println;
use cpu::riscv::switch::{self, AppHart, REGIME_PMPADDR0, REGIME_PMPCFG0, REGIME_SATP};
use cpu::riscv::{boot, pmp, sv39};
use dev::Pa;
use kern::cage::{Cage, CageError, CoreClass, Cores, PortError, Power, Status};
use kern::{Core, Region, SLOT_CAP, Slot};

/// De bel (`msip`-PA) van elk app-hart, per logische core; 0 = geen bel.
/// Eén schrijver ([`RvCage::new`], bij de boot), daarna alleen lezers: de
/// kick na elke RX-schrijf van de switch ([`wake_all`]) hoeft zo geen plan
/// te bouwen.
static BELLS: [AtomicU64; BELL_CAP] = [const { AtomicU64::new(0) }; BELL_CAP];

/// Hoeveel app-harts [`BELLS`] draagt: de parkeerlus kent er niet meer.
const BELL_CAP: usize = boot::MAX_HARTS;

/// Het hoogste linkadres dat één Sv39-niveau-1-tabel dekt: de gigabyte van
/// [`LINK_BASE`] (0x4000_0000..0x8000_0000). De staart heeft plek voor de
/// wortel en één tabel (`ABI_MAP_PAGES`), dus het venster eindigt hier.
const LINK_GIGA_END: u64 = 0x8000_0000;

const _: () = assert!(LINK_BASE >= 0x4000_0000 && LINK_BASE < LINK_GIGA_END);
const _: () = assert!(ABI_MAP_PAGES == 2);

/// Het app-adresvenster van een partitie van `size` bytes: vanaf
/// [`LINK_BASE`] tot het einde van zijn gigabyte (768 MB), want de
/// Sv39-tabel in de staart is een wortel plus één tabel.
pub(crate) fn link_window(size: u64) -> u64 {
    size.min(LINK_GIGA_END - LINK_BASE)
}

/// Extra vertaalopslag achter de partitie: geen. De Sv39-tabel woont in de
/// ABI-staart (`ABI_MAP_OFF`), binnen de kooi, want de walker is zelf aan
/// de whitelist onderworpen (Go, `kern/cage/relocate.go`).
pub(crate) fn reserve(_size: u64) -> u64 {
    0
}

/// Waarom de kooi niet op kon.
#[derive(Copy, Clone, Debug)]
pub(crate) enum Error {
    /// Het plan weigerde een index.
    Plan(abi::Error),
    /// Een app-hart kwam niet in de parkeerlus.
    Start(boot::StartError),
    /// Een warme flip bestaat op riscv64 niet: er is niets te adopteren.
    NoFlip,
}

impl core::fmt::Display for Error {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::Plan(e) => write!(f, "plan: {e}"),
            Self::Start(e) => write!(f, "app hart: {e}"),
            Self::NoFlip => f.write_str("riscv64 flips cold only, nothing to adopt"),
        }
    }
}

/// De switch-code zoals `slots::start` hem meldt: waar hij staat en zijn
/// som. Op riscv64 draait een app-hart hem uit het kern-image (hij is niet
/// naar de plan-regio gekopieerd zoals op arm64: dat is alleen nodig voor
/// de flip), dus `entry` is `mentry` in het image en `tramp` de
/// parkeer-ingang.
#[derive(Copy, Clone, Debug)]
pub(crate) struct Installed {
    /// De trap-ingang van de switcher (`mentry`).
    pub(crate) entry: Pa,
    /// De maat van de switch-code.
    pub(crate) len: u64,
    /// De parkeer-ingang (`parkenter`), waar een app-hart binnenkomt.
    pub(crate) tramp: Pa,
    /// FNV-1a over de code: dezelfde som op elke boot van hetzelfde image.
    pub(crate) hash: u64,
}

/// Wat de lijm per gebouwde kooi onthoudt.
#[derive(Copy, Clone, Debug)]
struct Built {
    /// De control-page (fysiek).
    ctrl: Pa,
    /// De logische core.
    core: layout::Core,
    /// Het hart.
    hart: usize,
    /// De entry uit de ELF, voor de startregel.
    entry: u64,
    /// De partitie van deze levensduur.
    part: Region,
}

/// De kooi van riscv64: PMP plus Sv39 per slot, de M-mode-switcher op elk
/// app-hart, de bewonerslijsten in de sched-blokken.
pub(crate) struct RvCage {
    plan: Plan,
    installed: Installed,
    built: [Option<Built>; SLOT_CAP + 1],
}

impl RvCage {
    /// Zet elk app-hart klaar: de kern-regels van zijn sched-blok (de
    /// kooi-regio, de wekker, de bel, de slaap en de kill-tick), elk
    /// ctx-blok leeg, en het hart de switcher in (`parkenter`). Eén keer bij
    /// boot, vóór de eerste dispatch.
    ///
    /// De ctx-blokken vers nullen is de les van 17-08 (Go, boot 3): een
    /// reboot liet `CtxRunning` van een vorige boot staan, en elke plaatsing
    /// weigerde met "still live".
    pub(crate) fn new(plan: Plan) -> Result<RvCage, Error> {
        let base = plan.vec_base_pa();
        for i in 1..=plan.max_slots() {
            let s = layout::Slot::new(i).ok_or(Error::Plan(abi::Error::TooMany {
                what: "slots",
                cap: SLOT_CAP,
            }))?;
            let ctx = plan.ctx_pa(s).map_err(Error::Plan)?;
            dev::clear(ctx, CTX_LEN as usize);
            dev::push(ctx, CTX_LEN as usize);
        }
        for c in 1..=plan.app_cores() {
            let core = layout::Core::new(c).ok_or(Error::Plan(abi::Error::TooMany {
                what: "app cores",
                cap: SLOT_CAP,
            }))?;
            let hart = plan.phys_core(core);
            let sched = plan.park_mbox_pa(core).map_err(Error::Plan)?;
            let t = crate::BOARD.app_hart(hart);
            if let Some(b) = BELLS.get(c) {
                b.store(t.msip.0, Relaxed);
            }
            let fresh = !boot::is_started(hart);
            arm_sched(sched, base.0, &t, fresh);
            if fresh {
                boot::start_hart(hart, switch::park_pc(), sched.0, t.msip).map_err(Error::Start)?;
                crate::BOARD.start_app_hart(hart);
            } else {
                // Al in de switcher (de zelftest van het board): de bel,
                // zodat hij de verse regels leest.
                ring_bell(&t);
            }
            println!(
                "cage: hart {hart} (core {core}) in the switcher: wake {:#x}, bell {:#x}, kick {:#x}, sleep cap {} ticks, kill tick {} ticks, reset {} HOPOS_RV_HART_UP",
                t.mtimecmp.0, t.msip.0, t.kick.0, t.sleep_cap, t.tick, t.resettable
            );
            if !t.resettable && (t.mtimecmp.0 == 0 || t.tick == 0) {
                // Go, `HOPOS_CORE_NO_KILL`: geen stille stand.
                println!(
                    "cage: hart {hart} has no reset and no kill tick: a resident that stops cooperating is only cleared by rebooting the node HOPOS_CORE_NO_KILL"
                );
            }
        }
        let (begin, end) = switch::code_range();
        Ok(RvCage {
            plan,
            installed: Installed {
                entry: Pa(switch::entry_pc()),
                len: end.saturating_sub(begin),
                tramp: Pa(switch::park_pc()),
                hash: code_hash(begin, end),
            },
            built: [None; SLOT_CAP + 1],
        })
    }

    /// Een warme flip bestaat op riscv64 niet (de switch-code draait uit het
    /// kern-image, en de koude flip draagt niemand over): er valt niets over
    /// te nemen. De flip weigert warm al vóór de sprong (`flip::WARM`).
    pub(crate) fn adopt(_plan: Plan) -> Result<RvCage, Error> {
        Err(Error::NoFlip)
    }

    /// Zie [`RvCage::adopt`]: elke bewoner weigert.
    pub(crate) fn adopt_slot(
        &mut self,
        _slot: Slot,
        _part: Region,
        _core: usize,
        _cores: usize,
    ) -> Result<(), CageError> {
        Err(err(code::DISPATCH))
    }

    /// De switch-code.
    pub(crate) fn installed(&self) -> &Installed {
        &self.installed
    }

    fn slot(&self, slot: Slot) -> Result<layout::Slot, CageError> {
        layout::Slot::new(slot.get()).ok_or(err(code::PLAN))
    }

    fn ctx(&self, slot: Slot) -> Option<Pa> {
        self.plan.ctx_pa(layout::Slot::new(slot.get())?).ok()
    }

    fn built(&self, slot: Slot) -> Option<Built> {
        self.built.get(slot.get()).copied().flatten()
    }

    /// De ctx-staat van `slot`, vers uit DRAM.
    fn ctx_state(&self, slot: Slot) -> Option<CtxState> {
        el2::ctx_state(self.ctx(slot)?)
    }

    /// Leest woord `off` van de control-page van `slot`, vers uit DRAM.
    fn ctrl_read(&self, slot: Slot, off: u64) -> u64 {
        self.built(slot).map_or(0, |b| {
            dev::pull(b.ctrl.add(off), 8);
            dev::read64(b.ctrl.add(off))
        })
    }

    fn ctrl_write(&self, slot: Slot, off: u64, v: u64) {
        if let Some(b) = self.built(slot) {
            dev::write64(b.ctrl.add(off), v);
            dev::push(b.ctrl.add(off), 8);
        }
    }

    /// Luidt de bel van het hart van `slot` (een no-op zonder bel).
    fn kick_slot(&self, slot: Slot) {
        // Op de OS-core geeft de kern zelf de beurten: geen bel.
        if let Some(b) = self.built(slot).filter(|b| b.core.get() != 0) {
            ring_bell(&crate::BOARD.app_hart(b.hart));
        }
    }

    /// Maakt `slot` bewoner van de OS-core: de rotatie van de kern
    /// (`cpu::riscv::oscore`) geeft hem het hart in de idle van de
    /// executor. Zijn idle is al een yield (`CTRL_IDLE_MODE`, `arm_tail`).
    fn host(
        &self,
        slot: Slot,
        s: layout::Slot,
        b: Built,
        ctx: Pa,
        sched: Pa,
    ) -> Result<(), CageError> {
        if let Err(e) = list_add(sched, s.get() as u8) {
            println!("cage: slot {slot}: the OS core's resident list is full HOPOS_CAGE_DISPATCH");
            return Err(e);
        }
        el2::ctx_write(ctx, CTX_STATE, CtxState::BootPending.raw());
        println!(
            "cage: slot {slot} hosted on the OS core (hart {}), next to the kern HOPOS_OS_HOST",
            b.hart
        );
        println!(
            "HOPOS_SLOT_START slot={slot} core=0 cpu={} entry={:#x} part={:#x}+{:#x}",
            b.hart, b.entry, b.part.base, b.part.size
        );
        Ok(())
    }

    /// Haalt `s` uit de bewonerslijst van elk app-hart (de kern is de enige
    /// schrijver van die regels). Vóór de nieuwe staat van een levensduur:
    /// de switcher leest de lijst bij een koude boot NÁ de staat nog eens,
    /// dus een hart dat hier weggehaald is, start het slot nooit meer.
    fn forget(&self, s: layout::Slot) {
        for c in 0..=self.plan.app_cores() {
            let Some(sched) = layout::Core::new(c).and_then(|c| self.plan.park_mbox_pa(c).ok())
            else {
                continue;
            };
            let n = list_len(sched);
            for i in 0..n {
                if list_get(sched, i) == s.get() as u8 {
                    list_set(sched, i, 0);
                }
            }
            dev::push(sched.add(SCHED_LIST), SLOT_CAP);
        }
        dev::mb();
    }

    /// De control-page, de ringen, de Sv39-tabel, de whitelist en de
    /// ctx-woorden van een verse bewoner (het schrijfwerk van `armSlot` en
    /// `cageBuild` in Go). Alles vóór de staat BootPending; die zet
    /// [`Cage::dispatch`].
    fn arm_tail(
        &self,
        slot: Slot,
        s: layout::Slot,
        part: Region,
        entry: u64,
        t: &AppHart,
        kick: bool,
    ) -> Result<(Tail, u64, pmp::Encoded), CageError> {
        let ram = app_ram(part).ok_or(err(code::TAIL))?;
        let tail = tail_of(part).ok_or(err(code::TAIL))?;
        let ctrl = tail.ctrl_page();
        // Geen veeg van de page: de claim wiste de partitie al (E3), en de
        // env kwam er vóór de Arm al op. Alleen de woorden van de kern.
        for (off, v) in [
            (CTRL_ENTRY, entry),
            (CTRL_SLOT, s.get() as u64),
            (CTRL_CORES, 1),
            (CTRL_STATUS, AppStatus::Booting as u64),
            (CTRL_WALL_OFF, crate::clock::offset()),
            // Een app-hart idlet met de yield (a0 = wektijd): een `wfi` van
            // een bewoner wekt nooit (hij draait met mie = 0), en de switcher
            // slaapt dan voor hem op de CLINT (Go, idle_riscv64.go: "de
            // ecall is zijn enige route naar een wfi"). Met IDLE_KICK belt
            // hij de kern na een publicatie (ecall, a7 = 2).
            (
                CTRL_IDLE_MODE,
                IDLE_YIELD | if kick { IDLE_KICK } else { 0 },
            ),
            // De timebase van de TIME-CSR: RISC-V heeft geen CNTFRQ.
            (CTRL_TIMEBASE_HZ, cpu::riscv::idle::hz()),
        ] {
            dev::write64(ctrl.add(off), v);
        }
        // Het zaad vóór de start (seed.rs, applib::rand).
        crate::seed::plant(ctrl);
        dev::push(ctrl, CTRL_STRIDE as usize);
        for (base, cap) in [
            (tail.outbox(), RING_DATA_CAP),
            (tail.net_tx(), NET_RING_DATA_CAP),
            (tail.net_rx(), NET_RING_DATA_CAP),
        ] {
            ring::init(base, cap).map_err(|e| {
                println!(
                    "cage: slot {slot}: ring at {:#x}: {e} HOPOS_CAGE_RING",
                    base.0
                );
                err(code::RING)
            })?;
        }
        let satp = relocate(slot, part, ram, tail, t)?;
        // De whitelist: één TOR-venster over de hele claim (app-RAM plus
        // staart), dan de deny-all. De switcher schrijft hem en leest
        // pmpcfg0 terug vóór hij de bewoner binnenlaat.
        let window = pmp::Window {
            base: part.base,
            size: part.size,
            r: true,
            w: true,
            x: true,
        };
        let enc = pmp::encode(&[window], t.pmp).map_err(|e| {
            println!("cage: slot {slot}: {e} HOPOS_CAGE_PMP");
            err(code::CAGE)
        })?;
        Ok((tail, satp, enc))
    }
}

/// Legt het linkvenster op de partitie: het app-RAM als normaal geheugen
/// (rwx), de staart erboven als device (rw, zonder de cachebits van de
/// C906: de kern leest en schrijft hem vanaf een ander hart, en de harts
/// zijn niet coherent). De tabel gaat naar `ABI_MAP_OFF` van de staart.
fn relocate(slot: Slot, part: Region, ram: u64, tail: Tail, t: &AppHart) -> Result<u64, CageError> {
    let map = [
        sv39::MapWindow {
            link: LINK_BASE,
            phys: part.base,
            size: ram,
            r: true,
            w: true,
            x: true,
            device: false,
        },
        sv39::MapWindow {
            link: LINK_BASE + ram,
            phys: tail.base().0,
            size: ABI_TAIL,
            r: true,
            w: true,
            x: false,
            device: true,
        },
    ];
    let mut tables = sv39::Tables::new();
    let satp = sv39::relocate(tail.map().0, &map, t.attrs, &mut tables).map_err(|e| {
        println!("cage: slot {slot}: {e} HOPOS_CAGE_SV39");
        err(code::CAGE)
    })?;
    if tables.used as u64 > ABI_MAP_PAGES {
        println!(
            "cage: slot {slot}: the map needs {} table pages, the tail holds {ABI_MAP_PAGES} HOPOS_CAGE_SV39",
            tables.used
        );
        return Err(err(code::CAGE));
    }
    for (i, page) in tables.pages.iter().take(tables.used).enumerate() {
        for (j, e) in page.iter().enumerate() {
            dev::write64(tail.map().add((i * 4096 + j * 8) as u64), *e);
        }
    }
    // De walker van het app-hart leest de tabel uit DRAM.
    dev::push(tail.map(), tables.len() as usize);
    Ok(satp)
}

/// Schrijft de kern-regels van sched-blok `sched`; `fresh` = het hart staat
/// nog niet in de switcher, dus ook regel 0 (lopend slot, rotor) mag nul.
fn arm_sched(sched: Pa, cage: u64, t: &AppHart, fresh: bool) {
    if fresh {
        dev::clear(sched, PARK_MBOX_LEN as usize);
    }
    dev::write64(sched.add(SCHED_COUNT), 0);
    dev::clear(sched.add(SCHED_LIST), SLOT_CAP);
    for (off, v) in [
        (SCHED_S2_PA, cage),
        (SCHED_CLINT_PA, t.mtimecmp.0),
        (SCHED_MSIP_PA, t.msip.0),
        (SCHED_OS_BELL, t.kick.0),
        (SCHED_SLEEP_CAP, t.sleep_cap),
        (SCHED_TICK_TICKS, t.tick),
    ] {
        dev::write64(sched.add(off), v);
    }
    dev::push(sched, PARK_MBOX_LEN as usize);
}

/// De bel van een app-hart (`msip`), als hij er een heeft.
fn ring_bell(t: &AppHart) {
    if t.msip.0 != 0 {
        dev::mb();
        dev::write32(t.msip, 1);
    }
}

/// De lengte van de bewonerslijst van `sched`.
fn list_len(sched: Pa) -> usize {
    dev::pull(sched.add(SCHED_COUNT), 8);
    usize::try_from(dev::read64(sched.add(SCHED_COUNT)))
        .unwrap_or(0)
        .min(SLOT_CAP)
}

/// Byte `i` van de bewonerslijst (woordgewijs gelezen: `dev` kent geen
/// byte-toegang op een adres dat twee harts delen).
fn list_get(sched: Pa, i: usize) -> u8 {
    let w = sched.add(SCHED_LIST + (i as u64 & !7));
    let b = dev::read64(w).to_le_bytes();
    b.get(i & 7).copied().unwrap_or(0)
}

/// Zet byte `i` van de bewonerslijst (lees-wijzig-schrijf van het woord;
/// de kern is de enige schrijver van deze regels).
fn list_set(sched: Pa, i: usize, v: u8) {
    let w = sched.add(SCHED_LIST + (i as u64 & !7));
    let mut b = dev::read64(w).to_le_bytes();
    if let Some(x) = b.get_mut(i & 7) {
        *x = v;
    }
    dev::write64(w, u64::from_le_bytes(b));
}

/// Zet `slot` in de bewonerslijst van `sched`: zijn oude plek, een gat, of
/// achteraan. De lengte is monotoon (0-bytes zijn gaten), zoals de switcher
/// hem leest.
fn list_add(sched: Pa, slot: u8) -> Result<(), CageError> {
    let n = list_len(sched);
    let at = (0..n)
        .find(|&i| list_get(sched, i) == slot)
        .or_else(|| (0..n).find(|&i| list_get(sched, i) == 0));
    match at {
        Some(i) => list_set(sched, i, slot),
        None if n < SLOT_CAP => {
            list_set(sched, n, slot);
            dev::write64(sched.add(SCHED_COUNT), n as u64 + 1);
        }
        None => return Err(err(code::ROSTER)),
    }
    dev::push(sched.add(SCHED_COUNT), 8);
    dev::push(sched.add(SCHED_LIST), SLOT_CAP);
    Ok(())
}

/// FNV-1a over `[begin, end)` van het image (woordgewijs).
fn code_hash(begin: u64, end: u64) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    let mut a = begin & !7;
    while a < end {
        for b in dev::read64(Pa(a)).to_le_bytes() {
            h ^= u64::from(b);
            h = h.wrapping_mul(0x0100_0000_01b3);
        }
        a += 8;
    }
    h
}

impl Cage for RvCage {
    fn clear(&mut self, base: u64, len: u64) {
        let Ok(n) = usize::try_from(len) else { return };
        // Geen `dev::pull` vooraf zoals op ARM: `th.dcache.cipa` raakt alleen
        // de cache van dit hart, en wat de vorige huurder vuil achterliet,
        // veegde zijn eigen switcher al vóór hij Dead meldde
        // (`HOPOS_RV_CIALL` in de teardown). Een regel in de cache van de
        // kern zelf overschrijft `dev::clear` hieronder.
        dev::clear(Pa(base), n);
        // Naar DRAM: de nieuwe eigenaar draait op een ander hart.
        dev::push(Pa(base), n);
    }

    fn build(
        &mut self,
        slot: Slot,
        part: Region,
        entry: u64,
        first: Core,
        cores: usize,
    ) -> Result<(), CageError> {
        let s = self.slot(slot)?;
        if cores != 1 {
            println!(
                "cage: slot {slot}: {cores} cores asked, riscv64 runs one core per slot HOPOS_CAGE_SMP"
            );
            return Err(err(code::SPAN));
        }
        let core = layout::Core::new(first.get()).ok_or(err(code::PLAN))?;
        // De OS-core (logisch 0) is het hart van de kern zelf: daar draait de
        // bewoner in de idle van de kern (`cpu::riscv::oscore`), niet onder
        // de switcher.
        let hart = if first == Core::OS {
            crate::BOARD.this_core()
        } else {
            self.plan.phys_core(core)
        };
        let t = crate::BOARD.app_hart(hart);
        let ctx = self.plan.ctx_pa(s).map_err(|_| err(code::PLAN))?;
        // Eerst uit elke oude bewonerslijst, dan het ctx-blok leeg (staat
        // Empty: een rotatie die hem nog ziet, slaat hem over), dan pas de
        // nieuwe levensduur.
        self.forget(s);
        dev::clear(ctx, CTX_LEN as usize);
        dev::push(ctx, CTX_LEN as usize);
        // De kick: op de OS-core een yield naar nu (de rotatie van de kern),
        // op een app-hart alleen met een bel naar de kern.
        let kick = first == Core::OS || t.kick.0 != 0;
        let (tail, satp, enc) = self.arm_tail(slot, s, part, entry, &t, kick)?;
        let ram = app_ram(part).ok_or(err(code::TAIL))?;
        let regime = ctx.add(CTX_REGIME);
        dev::write64(regime.add(REGIME_SATP), satp);
        dev::write64(regime.add(REGIME_PMPCFG0), enc.cfg);
        for (k, a) in enc.addr.iter().enumerate() {
            dev::write64(regime.add(REGIME_PMPADDR0 + 8 * k as u64), *a);
        }
        for (off, v) in [
            (CTX_BOOT_PC, entry),
            // a0 van de koude boot: de control-page zoals de app hem ziet.
            (CTX_BOOT_ARG, LINK_BASE + ram + ABI_CTRL_OFF),
            (CTX_CTRL_PA, tail.ctrl_page().0),
            (CTX_UNIT_SLOT, s.get() as u64),
            (CTX_RING_HEAD_PA, tail.net_rx().0 + ring::HEAD_OFF),
            (CTX_REVOKE, 0),
        ] {
            dev::write64(ctx.add(off), v);
        }
        dev::push(ctx, CTX_LEN as usize);
        dev::mb();
        attach(s, tail);
        if let Some(b) = self.built.get_mut(slot.get()) {
            *b = Some(Built {
                ctrl: tail.ctrl_page(),
                core,
                hart,
                entry,
                part,
            });
        }
        crate::clock::attach(slot, tail.ctrl_page());
        println!(
            "cage: slot {slot} built: part {:#x}+{:#x} -> va {LINK_BASE:#x}, satp {satp:#x}, pmp {} entries cfg {:#x}, ctrl {:#x}, core {core} (hart {hart})",
            part.base,
            part.size,
            enc.used,
            enc.cfg,
            tail.ctrl_page().0
        );
        Ok(())
    }

    fn dispatch(&mut self, slot: Slot, core: Core) -> Result<(), CageError> {
        let b = self.built(slot).ok_or(err(code::NOT_BUILT))?;
        let s = self.slot(slot)?;
        let c = layout::Core::new(core.get())
            .filter(|c| *c == b.core)
            .ok_or(err(code::DISPATCH))?;
        let ctx = self.plan.ctx_pa(s).map_err(|_| err(code::PLAN))?;
        let sched = self.plan.park_mbox_pa(c).map_err(|_| err(code::PLAN))?;
        if core == Core::OS {
            return self.host(slot, s, b, ctx, sched);
        }
        // De lijst eerst, dan de staat, dan de bel: de switcher leest de
        // lijst, dan de staat, en bij een koude boot de lijst nog eens.
        if let Err(e) = list_add(sched, s.get() as u8) {
            println!(
                "cage: slot {slot}: resident list of hart {} is full HOPOS_CAGE_DISPATCH",
                b.hart
            );
            return Err(e);
        }
        dev::mb();
        el2::ctx_write(ctx, CTX_STATE, CtxState::BootPending.raw());
        let t = crate::BOARD.app_hart(b.hart);
        ring_bell(&t);
        println!(
            "cage: slot {slot} dispatched to hart {} ({}) HOPOS_RV_DISPATCH",
            b.hart,
            if t.msip.0 != 0 {
                "BootPending + msip"
            } else {
                "BootPending, picked up by the spinning switcher"
            }
        );
        println!(
            "HOPOS_SLOT_START slot={slot} core={core} cpu={} entry={:#x} part={:#x}+{:#x}",
            b.hart, b.entry, b.part.base, b.part.size
        );
        Ok(())
    }

    fn dispatch_secondary(&mut self, slot: Slot, core: Core) -> Result<(), CageError> {
        println!(
            "cage: slot {slot}: SMP core {core} asked, riscv64 runs one core per slot HOPOS_SMP_DISPATCH_FAIL"
        );
        Err(err(code::SPAN))
    }

    fn request_exit(&mut self, slot: Slot) {
        // Elke stop begint hier: eerst van de switch af, dan de kill-vlag,
        // dan de bel, zodat een slaper hem nu ziet en niet op zijn wektijd.
        detach(slot);
        self.ctrl_write(slot, CTRL_KILL, 1);
        self.kick_slot(slot);
    }

    fn quiet(&self, slot: Slot, _core: Core) -> bool {
        // Eén core per slot: stil is de staat van zijn ctx-blok. Dead
        // schrijft de switcher pas ná de volledige cache-veeg van het hart
        // (de teardown), dus dan is de partitie echt terug.
        matches!(self.ctx_state(slot), Some(CtxState::Empty | CtxState::Dead))
    }

    fn live(&self, slot: Slot) -> bool {
        matches!(
            self.ctx_state(slot),
            Some(CtxState::Running | CtxState::Saved | CtxState::BootPending)
        )
    }

    fn revoke(&mut self, slot: Slot) {
        crate::clock::detach(slot);
        let Some(ctx) = self.ctx(slot) else { return };
        // Het woord dat de switcher leest bij elke yield, elke kill-tick en
        // elke ronde over een slaper.
        el2::ctx_write(ctx, CTX_REVOKE, 1);
        let Some(b) = self.built(slot) else { return };
        if b.core.get() == 0 {
            // Op de OS-core is de kern de enige die een bewoner het hart
            // geeft, en hij draait nu zelf (deze code): uit de lijst en dood
            // is een bevestigd einde, ook voor een slaper met een verre
            // wektijd (arm64: `el2::unhost`).
            if let Ok(s) = self.slot(slot) {
                self.forget(s);
            }
            el2::ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
            println!("cage: slot {slot}: out of the OS core rotation HOPOS_OS_UNHOST");
            return;
        }
        let t = crate::BOARD.app_hart(b.hart);
        ring_bell(&t);
        if t.resettable && t.tick == 0 && !self.quiet(slot, Core::OS) {
            self.reset_hart(slot, b, ctx);
        }
    }

    fn smp_request(&self, slot: Slot) -> u64 {
        self.ctrl_read(slot, CTRL_SMP_REQ)
    }

    fn clear_smp_request(&mut self, slot: Slot) {
        self.ctrl_write(slot, CTRL_SMP_REQ, 0);
    }

    fn status(&self, slot: Slot) -> Status {
        Status {
            core_on: self.built(slot).is_some() && self.live(slot),
            app: self.ctrl_read(slot, CTRL_STATUS),
            exit_code: self.ctrl_read(slot, CTRL_EXIT_CODE),
            heartbeat: self.ctrl_read(slot, CTRL_HEARTBEAT),
            ram_size: self.ctrl_read(slot, CTRL_RAM_SIZE),
            mem_sys: self.ctrl_read(slot, CTRL_MEM_SYS),
            idle_ns: idle_ns(self.ctrl_read(slot, CTRL_IDLE)),
            wakes: self.ctrl_read(slot, CTRL_WAKES),
            cores: self.ctrl_read(slot, CTRL_CORES),
            at_ns: cpu::idle::now(),
            fault_vec: self.ctrl_read(slot, CTRL_FAULT_VEC),
            fault_esr: self.ctrl_read(slot, CTRL_FAULT_ESR),
            fault_far: self.ctrl_read(slot, CTRL_FAULT_FAR),
            // De vectortabel van EL1 is ARM (applib::mmu); op RISC-V is een
            // trap van de app altijd een fault van de switcher.
            ..Status::default()
        }
    }

    fn publish(
        &mut self,
        slot: Slot,
        ports: &[u16],
    ) -> impl Future<Output = Result<(), PortError>> {
        publish_ports(slot, ports)
    }

    fn unpublish(&mut self, slot: Slot) {
        unpublish_ports(slot);
    }
}

impl RvCage {
    /// De harde intrekking op een hart met een resetblok en zonder
    /// kill-tick (de C906L van de LicheeRV): reset vast (dat wist ook zijn
    /// PMP en zijn caches, gemeten 30-07), de bewoner dood en uit de lijst,
    /// en het hart opnieuw de switcher in. Er draait daarna niets meer van
    /// deze bewoner: het hart kwam uit reset.
    fn reset_hart(&self, slot: Slot, b: Built, ctx: Pa) {
        if !crate::BOARD.hold_app_hart(b.hart) {
            return;
        }
        let Ok(sched) = self.plan.park_mbox_pa(b.core) else {
            return;
        };
        let t = crate::BOARD.app_hart(b.hart);
        el2::ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
        // Alle andere bewoners van dit hart gingen met de reset mee; op
        // riscv64 is er één per hart, dus de lijst mag leeg.
        arm_sched(sched, self.plan.vec_base_pa().0, &t, true);
        match boot::start_hart(b.hart, switch::park_pc(), sched.0, t.msip) {
            Ok(()) => {
                crate::BOARD.start_app_hart(b.hart);
                println!(
                    "cage: slot {slot}: hart {} reset and back in the switcher HOPOS_CAGE_REVOKE",
                    b.hart
                );
            }
            Err(e) => println!(
                "cage: slot {slot}: hart {} held in reset, restart refused: {e} HOPOS_CAGE_REVOKE",
                b.hart
            ),
        }
    }
}

/// De cores van riscv64: logische core 0 is het hart van de kern, app-core
/// i het i-de andere hart (`Plan::phys_core`). "Aan" is: de switcher van
/// dat hart draait een bewoner (`SCHED_CURRENT`).
pub(crate) struct RvCores {
    plan: Plan,
}

impl RvCores {
    /// De cores van dit plan.
    pub(crate) fn new(plan: Plan) -> RvCores {
        RvCores { plan }
    }
}

impl Cores for RvCores {
    fn app_cores(&self) -> usize {
        self.plan.app_cores()
    }

    fn class(&self, _core: Core) -> Option<CoreClass> {
        // De C906B en C906L zijn hetzelfde ontwerp; QEMU virt homogeen.
        None
    }

    fn power(&self, core: Core) -> Power {
        if core == Core::OS {
            return Power::On;
        }
        match layout::Core::new(core.get()).and_then(|c| core_state(&self.plan, c).ok()) {
            Some(CoreState::Running(_)) => Power::On,
            _ => Power::Off,
        }
    }

    fn kick(&mut self, core: Core) {
        if let Some(c) = layout::Core::new(core.get()).filter(|_| core != Core::OS) {
            ring_bell(&crate::BOARD.app_hart(self.plan.phys_core(c)));
        }
    }
}

// De architectuur-naad van `slots.rs`: wat daar op arm64 `cpu::el2` is.

/// De toestand van app-core `core` in de vorm van `cpu::el2::core_state`:
/// koud (nooit gestart), geparkeerd (de switcher draait niemand) of het
/// slot dat hij draait (`SCHED_CURRENT`, vers uit DRAM).
pub(crate) fn core_state(plan: &Plan, core: layout::Core) -> Result<CoreState, el2::Error> {
    let sched = plan.park_mbox_pa(core).map_err(el2::Error::Plan)?;
    if !boot::is_started(plan.phys_core(core)) {
        return Ok(CoreState::Cold);
    }
    dev::pull(sched.add(SCHED_CURRENT), 8);
    Ok(match dev::read64(sched.add(SCHED_CURRENT)) {
        0 => CoreState::Parked,
        s => CoreState::Running(s),
    })
}

/// Waar een app-hart staat na [`park_for_flip`].
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) enum Off {
    /// Nooit gestart: er draait niets van het image.
    Cold,
    /// In reset (de C906L): de nieuwe kern haalt hem eruit zoals bij elke
    /// boot (`start_app_hart`).
    Reset,
    /// Op weg naar de uit-stub; [`is_off`] zegt wanneer hij er is.
    Sent,
}

/// De app-harts die [`park_for_flip`] uit het image haalde (bit per core),
/// voor [`unpark_after_flip`]. Eén schrijver: de flip-taak.
static FLIP_PARKED: AtomicU64 = AtomicU64::new(0);

/// Haalt app-core `core` uit het kern-image voor de koude flip: de nieuwe
/// kern gaat over de switch-code heen, en die draait op riscv64 uit het
/// image. Een hart met een resetblok (de C906L) gaat in reset; een hart
/// zonder (QEMU) krijgt `SCHED_OFF_PC` = de uit-stub op `stub`
/// (`cpu::riscv::switch::off_stub`, buiten het image) en de bel, en springt
/// er aan het begin van zijn volgende ronde heen. Alleen een hart waarop
/// niemand draait: de bewoners zijn dan al gestopt.
///
/// De bevestiging is het adres van de stub in `SCHED_MBOX_CTX` (regel 0).
/// Elke start van een hart in deze boot wiste zijn sched-blok
/// ([`arm_sched`], de zelftest van het board), en alleen de stub schrijft
/// dat woord: staat er al iets, dan is het hart niet waar de kern denkt.
pub(crate) fn park_for_flip(
    plan: &Plan,
    core: layout::Core,
    stub: Pa,
) -> Result<Off, &'static str> {
    match core_state(plan, core) {
        Ok(CoreState::Cold) => return Ok(Off::Cold),
        Ok(CoreState::Parked) => {}
        Ok(CoreState::Running(_)) | Err(_) => return Err("app core still runs a resident"),
    }
    let hart = plan.phys_core(core);
    let t = crate::BOARD.app_hart(hart);
    let bit = 1u64 << core.get().min(63);
    if t.resettable && crate::BOARD.hold_app_hart(hart) {
        FLIP_PARKED.fetch_or(bit, Relaxed);
        return Ok(Off::Reset);
    }
    let sched = plan.park_mbox_pa(core).map_err(|_| "no sched block")?;
    dev::pull(sched.add(SCHED_MBOX_CTX), 8);
    if dev::read64(sched.add(SCHED_MBOX_CTX)) != 0 {
        return Err("sched block carries an off mark already");
    }
    if !switch::place_off_stub(stub) {
        return Err("no off stub in this build");
    }
    dev::write64(sched.add(SCHED_OFF_PC), stub.0);
    dev::push(sched.add(SCHED_OFF_PC), 8);
    FLIP_PARKED.fetch_or(bit, Relaxed);
    ring_bell(&t);
    Ok(Off::Sent)
}

/// Staat app-core `core` in de uit-stub op `stub` (zijn bevestiging)?
pub(crate) fn is_off(plan: &Plan, core: layout::Core, stub: Pa) -> bool {
    plan.park_mbox_pa(core).is_ok_and(|sched| {
        dev::pull(sched.add(SCHED_MBOX_CTX), 8);
        dev::read64(sched.add(SCHED_MBOX_CTX)) == stub.0
    })
}

/// De koude flip ging niet door: elk hart dat [`park_for_flip`] uit het
/// image haalde, gaat terug de switcher in, zoals na een harde intrekking
/// ([`RvCage::reset_hart`]): een vers sched-blok, het postvak en de bel
/// (de uit-stub springt op de bel naar `_start`, de parkeerlus), of het
/// resetblok los. De lijst is leeg: de bewoners waren al gestopt.
pub(crate) fn unpark_after_flip(plan: &Plan) {
    let parked = FLIP_PARKED.swap(0, Relaxed);
    for c in 1..=plan.app_cores() {
        let Some(core) = layout::Core::new(c).filter(|c| parked & 1u64 << c.get().min(63) != 0)
        else {
            continue;
        };
        let (hart, Ok(sched)) = (plan.phys_core(core), plan.park_mbox_pa(core)) else {
            continue;
        };
        let t = crate::BOARD.app_hart(hart);
        arm_sched(sched, plan.vec_base_pa().0, &t, true);
        match boot::start_hart(hart, switch::park_pc(), sched.0, t.msip) {
            Ok(()) => {
                crate::BOARD.start_app_hart(hart);
                println!(
                    "cage: hart {hart} back in the switcher after a cold flip that did not jump HOPOS_FLIP_CORE_BACK"
                );
            }
            Err(e) => {
                println!("cage: hart {hart} stays out of the switcher: {e} HOPOS_FLIP_CORE_BACK")
            }
        }
    }
}

/// De kick na een schrijf in een RX-ring van een slot (`slot_wake` van de
/// switch): de bel van elk app-hart. Het slot kiest geen doel, want de
/// lijm van de switch weet niet op welk hart het woont; een bel te veel is
/// een lege ronde van de switcher.
pub(crate) fn wake_all() {
    let mut rung = false;
    for b in BELLS.iter() {
        let pa = b.load(Relaxed);
        if pa != 0 {
            if !rung {
                dev::mb();
                rung = true;
            }
            dev::write32(Pa(pa), 1);
        }
    }
}

/// Kan de kern zijn core delen (de groep `system`, en Hop als het board
/// dat wil: `hopos.hop.sharegroup=system`)? Ja: de kern geeft zijn bewoners het
/// hart in zijn idle (`cpu::riscv::oscore`, PORT.md beslissing 2).
pub(crate) const SHARES_OS_CORE: bool = true;

/// De rotatie van de OS-core, zoals `slots.rs` hem aan de slaap van de
/// executor geeft.
pub(crate) use cpu::riscv::oscore::OsCore;

/// De rotatie van de OS-core voor de slaap van de executor, na de zelftest
/// van de overgang: de wekker (een spinner komt terug op de deadline), de
/// yield en de kick (een spinner komt terug op de `msip` van het eigen
/// hart), en de exit. Aanroepen op het hart van de kern, ná de interrupts.
pub(crate) fn os_core(plan: &Plan) -> Result<OsCore, el2::Error> {
    use cpu::riscv::oscore::Probe;
    let board = &crate::BOARD;
    let hart = board.this_core();
    // De CLINT-index van dit hart: op de LicheeRV altijd 0 (één CLINT per
    // core), op virt het hart-id.
    let mut os = OsCore::new(
        plan,
        board.clint(),
        board.clint_hart(),
        board.app_hart(hart).pmp,
    )?;
    let ms = cpu::riscv::idle::hz() / 1000;
    let t = os.selftest(Probe::Spin, ms, &|| {});
    let y = os.selftest(Probe::Yield, 100 * ms, &|| {});
    let k = os.selftest(Probe::Spin, 100 * ms, &|| board.kick_self());
    let x = os.selftest(Probe::Exit, 100 * ms, &|| {});
    let us = |r: Option<(el2::Back, u64)>| r.map(|(b, dt)| (b, dt * 1000 / ms.max(1)));
    let (t, y, k, x) = (us(t), us(y), us(k), us(x));
    let ok = matches!(t, Some((el2::Back::Timer, _)))
        && matches!(y, Some((el2::Back::Yield, _)))
        && matches!(k, Some((el2::Back::Ipi, _)))
        && matches!(x, Some((el2::Back::Exit, _)));
    println!(
        "oscore: hart {hart} self-test timer={t:?} yield={y:?} kick={k:?} exit={x:?} (back, us) {}",
        if ok {
            "HOPOS_OS_SELFTEST ok"
        } else {
            "HOPOS_OS_SELFTEST_FAIL"
        }
    );
    Ok(os)
}

/// De idle-ticks van de architectuurteller in nanoseconden.
fn idle_ns(ticks: u64) -> u64 {
    let f = cpu::idle::freq().max(1);
    u64::try_from(u128::from(ticks) * 1_000_000_000 / u128::from(f)).unwrap_or(u64::MAX)
}
