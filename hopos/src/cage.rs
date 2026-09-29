//! De kooi-lijm: de traits van `kern::cage` over `cpu::el2`, `cpu::psci`,
//! `dev` en de executor van core 0.
//!
//! Dit bezit de per-slot wetenschap die de kern niet heeft en de switcher
//! niet mag hebben: waar de control-page van een levende kooi staat en op
//! welke core hij draait. De boekhouding (wie welke partitie en core heeft)
//! is van de lifecycle-actor; die krijgt [`ArmCage`] en [`ArmCores`] als
//! waarde en is dus de enige aanroeper (`&mut self`, handboek §1).
//!
//! De stage-2-bouwer is die van `cpu::el2::stage2`, niet `kern::stage2`:
//! de intrekking (`cpu::el2::revoke`) en de switcher lezen dezelfde tabellen
//! via hetzelfde kooiblok, en één bouwer naast zijn eigen intrekker kan niet
//! uit elkaar groeien. Hij doet bovendien het cache-onderhoud (de walker van
//! de app-core leest cacheable) waar `kern::stage2` over een `PhysMem`
//! alleen rekent; die blijft de rekenkern voor de host-tests en de flip.
//!
//! Dit is de ARM-helft van `OLD/metal/kern/slots/cage_arm64.go` plus het
//! schrijfwerk van `armSlot` (control-page, ringen, ctx-woorden).

use abi::hopabi::{
    AppStatus, CTRL_CORES, CTRL_ENTRY, CTRL_EXIT_CODE, CTRL_FAULT_ESR, CTRL_FAULT_FAR,
    CTRL_FAULT_VEC, CTRL_HEARTBEAT, CTRL_KILL, CTRL_MBOX_PA, CTRL_RAM_SIZE, CTRL_S2_TABLE,
    CTRL_SLOT, CTRL_SMP_REQ, CTRL_STATUS, CTRL_VEC_PA, CTRL_WALL_OFF,
};
use abi::layout::{
    self, ABI_TAIL, CTRL_STRIDE, CtxState, LINK_BASE, NET_RING_DATA_CAP, Plan, RING_DATA_CAP, Tail,
};
use abi::ring;
use board_qemuvirt::slots::mpidr;
use core::future::Future;
use core::time::Duration;
use cpu::el2::{self, CoreState, Flavor, Installed, Start};
use cpu::println;
use dev::Pa;
use executor::Executor;
use kern::cage::{Cage, CageError, Console, CoreClass, Cores, PhysMem, Power, Status, Timer};
use kern::slots::Outbox;
use kern::{Core, Region, SLOT_CAP, Slot};
use net::ring::AbiTx;
use net::switch::{Ack, Command};

/// De bevestiging van de `Attach` van een verse kooi aan de switch. Niemand
/// wacht erop (de kooi-trait is synchroon); het resultaat wordt bij de
/// volgende attach opgehaald en gemeld als het een weigering was.
static ATTACH_ACK: Ack = Ack::new();
/// De bevestiging van de `Detach` bij een stop.
static DETACH_ACK: Ack = Ack::new();

/// De EL2-smaak van QEMU virt: E2H=0, de switcher slaapt in WFE. Het board
/// kiest, niet een bouwvlag (cpu::el2 `Flavor`).
pub(crate) const FLAVOR: Flavor = Flavor::Nvhe;

/// De foutcodes van [`CageError`] aan deze kant. De tekst met de getallen
/// staat op de console (één regel met marker); de code gaat de kern in.
mod code {
    /// Het plan weigerde een slot- of core-index.
    pub(super) const PLAN: u32 = 1;
    /// De partitie geeft geen geldige ABI-staart.
    pub(super) const TAIL: u32 = 2;
    /// De stage-2-bouw weigerde.
    pub(super) const STAGE2: u32 = 3;
    /// Een ring kon niet klaargezet worden.
    pub(super) const RING: u32 = 4;
    /// Dispatch zonder build.
    pub(super) const NOT_BUILT: u32 = 5;
    /// Het startschot via de mailbox weigerde.
    pub(super) const DISPATCH: u32 = 6;
    /// PSCI CPU_ON faalde; de code erbij is 0x100 plus de PSCI-fout.
    pub(super) const PSCI: u32 = 0x100;
    /// SMP-apps zijn op dit spoor nog niet gedragen.
    pub(super) const NO_SMP: u32 = 7;
}

const fn err(code: u32) -> CageError {
    CageError { code }
}

/// Het app-adresvenster van een partitie van `size` bytes: het canonieke
/// venster vanaf [`LINK_BASE`] tot de rand van het 39-bit-regime
/// (`cageLinkWindow`).
pub(crate) fn link_window(size: u64) -> u64 {
    size.min(el2::stage2::IPA_LIMIT - LINK_BASE)
}

/// De tabelopslag achter een partitie van `size` (`cageReserve`).
pub(crate) fn reserve(size: u64) -> u64 {
    el2::stage2::table_reserve(LINK_BASE, size)
}

/// Het app-RAM van een partitie: alles onder de ABI-staart.
fn app_ram(part: Region) -> Option<u64> {
    part.size.checked_sub(ABI_TAIL).filter(|n| *n > 0)
}

/// De staart van een partitie, in fysieke adressen.
pub(crate) fn tail_of(part: Region) -> Option<Tail> {
    Tail::new(part.base, app_ram(part)?)
}

/// Wat de lijm per gebouwde kooi onthoudt.
#[derive(Copy, Clone, Debug)]
struct Built {
    /// De control-page (fysiek), het x0 van de trampoline.
    ctrl: Pa,
    /// De primaire core.
    core: layout::Core,
    /// De entry uit de ELF en de partitie, voor de startregel.
    entry: u64,
    /// De partitie van deze levensduur.
    part: Region,
}

/// De kooi van QEMU virt: stage-2 onder EL2, de switcher in de plan-regio,
/// de park-mailboxen, PSCI voor de eerste opgang.
pub(crate) struct ArmCage {
    plan: Plan,
    installed: Installed,
    built: [Option<Built>; SLOT_CAP + 1],
}

impl ArmCage {
    /// Zet de switch-code in de plan-regio en de app-cores klaar (vectoren,
    /// parkeerlus, sched-blokken, lege ctx-staten). Eén keer bij boot, vóór
    /// de eerste dispatch; er leven nog geen bewoners.
    pub(crate) fn new(plan: Plan) -> Result<ArmCage, el2::Error> {
        let installed = el2::install_switch_code(&plan, FLAVOR)?;
        el2::init_app_cores(&plan, &installed, false)?;
        Ok(ArmCage {
            plan,
            installed,
            built: [None; SLOT_CAP + 1],
        })
    }

    /// De geïnstalleerde switch-code.
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

    /// Leest woord `off` van de control-page van `slot`, vers uit DRAM: de
    /// app schrijft hem met de MMU uit, wij lezen hem gecached.
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

    /// De ctx-staat van `slot`, voor de metingen van de boot-plaatsing.
    pub(crate) fn ctx_state(&self, slot: Slot) -> Option<CtxState> {
        el2::ctx_state(self.ctx(slot)?)
    }

    /// De control-page, de ringen en de ctx-woorden van een verse bewoner
    /// (het schrijfwerk van `armSlot`, zonder de switch en de mounts).
    fn arm_tail(
        &self,
        s: layout::Slot,
        tail: Tail,
        l1: Pa,
        entry: u64,
        core: layout::Core,
        cores: usize,
    ) -> Result<(), CageError> {
        let mbox = self.plan.park_mbox_pa(core).map_err(|_| err(code::PLAN))?;
        // Geen veeg van de page: de claim wiste de partitie al (E3), en een
        // stream via de system-API legde de env (CTRL_ENV_LEN, CTRL_ENV_DATA)
        // er vóór de Arm al op. Alleen de woorden van de kern.
        let ctrl = tail.ctrl_page();
        for (off, v) in [
            (CTRL_ENTRY, entry),
            (CTRL_S2_TABLE, l1.0),
            (CTRL_VEC_PA, self.plan.vec_base_pa().0),
            (CTRL_SLOT, s.get() as u64),
            (CTRL_MBOX_PA, mbox.0),
            (CTRL_CORES, cores as u64),
            (CTRL_STATUS, AppStatus::Booting as u64),
            // De wandklok vóór de start, zoals de Go-kern: Hop stempelt
            // zijn taken ermee vanaf zijn eerste regel (clock.rs).
            (CTRL_WALL_OFF, crate::clock::offset()),
        ] {
            dev::write64(ctrl.add(off), v);
        }
        // De hele verse page naar DRAM: de trampoline leest hem met de MMU
        // uit, langs elke cache heen.
        dev::push(ctrl, CTRL_STRIDE as usize);
        for (base, cap) in [
            (tail.outbox(), RING_DATA_CAP),
            (tail.net_tx(), NET_RING_DATA_CAP),
            (tail.net_rx(), NET_RING_DATA_CAP),
        ] {
            ring::init(base, cap).map_err(|e| {
                println!("cage: slot {s}: ring at {:#x}: {e} HOPOS_CAGE_RING", base.0);
                err(code::RING)
            })?;
        }
        let ctx = self.plan.ctx_pa(s).map_err(|_| err(code::PLAN))?;
        el2::arm_context(ctx, ctrl, s, tail.net_rx().0 + ring::HEAD_OFF);
        dev::mb();
        attach(s, tail);
        Ok(())
    }
}

/// Hangt de frame-ringen van een verse kooi aan de switch (`hopswitch.
/// Attach` in `armSlot`): ná de ring-init, vóór het startschot. De switch
/// wordt eigenaar van de handvatten; een oude poort op dit slot vervalt.
/// Een volle brievenbus of een switch die er niet is (geen NIC) laat de app
/// zonder slot-LAN draaien: één regel, geen weigering van de start.
fn attach(s: layout::Slot, tail: Tail) {
    if let Some(Err(e)) = ATTACH_ACK.try_take() {
        println!("cage: an earlier attach was refused: {e} HOPOS_CAGE_ATTACH");
    }
    let (Ok(tx), Ok(rx)) = (
        AbiTx::open(tail.net_tx(), NET_RING_DATA_CAP),
        ring::Writer::open(tail.net_rx(), NET_RING_DATA_CAP),
    ) else {
        println!("cage: slot {s}: frame rings do not open HOPOS_CAGE_ATTACH");
        return;
    };
    let cmd = Command::Attach {
        slot: s.get(),
        tx,
        rx,
        ack: &ATTACH_ACK,
    };
    if crate::net::COMMANDS.try_send(cmd).is_err() {
        println!("cage: slot {s}: switch mailbox full, no slot LAN HOPOS_CAGE_ATTACH");
    }
}

/// Haalt de ringen van `slot` weer van de switch, bij elke stop. FIXME: de
/// kooi-trait is synchroon, dus niemand wacht op de bevestiging; de
/// partitie komt pas vrij na de stil-toets van de actor en een nieuwe claim
/// is een later bericht, en in die tijd draait de switch zijn ronde. Een
/// asynchrone ontkoppel-haak in `kern::slots::stop` maakt dit hard.
fn detach(slot: Slot) {
    let _ = DETACH_ACK.try_take();
    let cmd = Command::Detach {
        slot: slot.get(),
        ack: &DETACH_ACK,
    };
    if crate::net::COMMANDS.try_send(cmd).is_err() {
        println!("cage: slot {slot}: switch mailbox full, detach not sent HOPOS_CAGE_DETACH");
    }
}

impl Cage for ArmCage {
    fn link_window(&self, size: u64) -> u64 {
        link_window(size)
    }

    fn reserve(&self, size: u64) -> u64 {
        reserve(size)
    }

    fn clear(&mut self, base: u64, len: u64) {
        let Ok(n) = usize::try_from(len) else { return };
        dev::clear(Pa(base), n);
        // Naar DRAM: de nieuwe eigenaar leest zijn partitie ongecached.
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
        let core = layout::Core::new(first.get()).ok_or(err(code::PLAN))?;
        let tail = tail_of(part).ok_or(err(code::TAIL))?;
        let block = self.plan.cage_table_pa(s).map_err(|_| err(code::PLAN))?;
        let l1 = el2::stage2::build(block, LINK_BASE, part.base, part.size).map_err(|e| {
            println!("cage: slot {slot}: stage-2 refused: {e} HOPOS_CAGE_STAGE2");
            err(code::STAGE2)
        })?;
        self.arm_tail(s, tail, l1, entry, core, cores)?;
        if let Some(b) = self.built.get_mut(slot.get()) {
            *b = Some(Built {
                ctrl: tail.ctrl_page(),
                core,
                entry,
                part,
            });
        }
        crate::clock::attach(slot, tail.ctrl_page());
        println!(
            "cage: slot {slot} built: part {:#x}+{:#x} -> ipa {LINK_BASE:#x}, l1 {:#x}, ctrl {:#x}, core {core}",
            part.base,
            part.size,
            l1.0,
            tail.ctrl_page().0
        );
        Ok(())
    }

    fn dispatch(&mut self, slot: Slot, core: Core) -> Result<(), CageError> {
        let b = self.built(slot).ok_or(err(code::NOT_BUILT))?;
        let s = self.slot(slot)?;
        let c = layout::Core::new(core.get()).ok_or(err(code::PLAN))?;
        let ctx = self.plan.ctx_pa(s).map_err(|_| err(code::PLAN))?;
        let tramp = self.installed.tramp;
        // De startregel van elke levensduur, welke weg hij ook kwam (de
        // boot-plaatsing of `STREAM_IMAGE` van Hop): hier, want dit is de
        // ene plek waar elke start langskomt.
        let started = || {
            println!(
                "HOPOS_SLOT_START slot={slot} core={core} entry={:#x} part={:#x}+{:#x}",
                b.entry, b.part.base, b.part.size
            );
        };
        match el2::dispatch(&self.plan, c, ctx, tramp, b.ctrl.0) {
            Ok(Start::Woken) => {
                println!("cage: slot {slot} dispatched to parked core {core} (mailbox + SEV)");
                started();
                Ok(())
            }
            Ok(Start::Cold) => {
                // De eerste opgang van deze core: PSCI CPU_ON rechtstreeks
                // de trampoline in, x0 = de control-page. Daarna leeft hij
                // in de parkeerlus van HopOS en gaat elke dispatch via de
                // mailbox.
                let target = mpidr(core.get());
                let r = cpu::psci::cpu_on(target, tramp.0, b.ctrl.0);
                println!(
                    "cage: slot {slot} core {core} cold: PSCI CPU_ON mpidr={target:#x} entry={:#x} x0={:#x} -> {r:?}",
                    tramp.0, b.ctrl.0
                );
                if r.is_ok() {
                    started();
                }
                r.map_err(|e| err(code::PSCI + e.code().unsigned_abs() as u32))
            }
            Err(e) => {
                println!("cage: slot {slot} core {core}: {e} HOPOS_CAGE_DISPATCH");
                Err(err(code::DISPATCH))
            }
        }
    }

    fn dispatch_secondary(&mut self, slot: Slot, core: Core) -> Result<(), CageError> {
        // SMP-apps (prepare_smp plus de SMP-trampoline) horen bij een later
        // spoor; weigeren is de veilige kant, de kern quarantaineert.
        println!(
            "cage: slot {slot}: SMP core {core} not carried on this board yet HOPOS_CAGE_NO_SMP"
        );
        Err(err(code::NO_SMP))
    }

    fn request_exit(&mut self, slot: Slot) {
        // Elke stop begint hier: eerst van de switch af, dan de kill-vlag.
        detach(slot);
        self.ctrl_write(slot, CTRL_KILL, 1);
    }

    fn quiet(&self, slot: Slot, core: Core) -> bool {
        let dead = matches!(self.ctx_state(slot), Some(CtxState::Empty | CtxState::Dead));
        let parked = layout::Core::new(core.get())
            .and_then(|c| el2::core_state(&self.plan, c).ok())
            .is_some_and(|st| matches!(st, CoreState::Cold | CoreState::Parked));
        dead || parked
    }

    fn live(&self, slot: Slot) -> bool {
        matches!(
            self.ctx_state(slot),
            Some(CtxState::Running | CtxState::Saved | CtxState::BootPending)
        )
    }

    fn revoke(&mut self, slot: Slot) {
        crate::clock::detach(slot);
        let Ok(s) = self.slot(slot) else { return };
        if let Err(e) = el2::revoke(&self.plan, s) {
            println!("cage: slot {slot}: revoke: {e} HOPOS_CAGE_REVOKE");
        }
    }

    fn smp_request(&self, slot: Slot) -> u64 {
        self.ctrl_read(slot, CTRL_SMP_REQ)
    }

    fn clear_smp_request(&mut self, slot: Slot) {
        self.ctrl_write(slot, CTRL_SMP_REQ, 0);
    }

    fn status(&self, slot: Slot) -> Status {
        let core_on = self
            .built(slot)
            .and_then(|b| el2::core_state(&self.plan, b.core).ok())
            .is_some_and(|st| matches!(st, CoreState::Running(_)));
        Status {
            core_on,
            app: self.ctrl_read(slot, CTRL_STATUS),
            exit_code: self.ctrl_read(slot, CTRL_EXIT_CODE),
            heartbeat: self.ctrl_read(slot, CTRL_HEARTBEAT),
            ram_size: self.ctrl_read(slot, CTRL_RAM_SIZE),
            fault_vec: self.ctrl_read(slot, CTRL_FAULT_VEC),
            fault_esr: self.ctrl_read(slot, CTRL_FAULT_ESR),
            fault_far: self.ctrl_read(slot, CTRL_FAULT_FAR),
        }
    }
}

/// De app-cores van QEMU virt: logische core i is MPIDR i (`mpidr`), de
/// toestand komt uit de park-mailbox.
pub(crate) struct ArmCores {
    plan: Plan,
}

impl ArmCores {
    /// De cores van dit plan.
    pub(crate) fn new(plan: Plan) -> ArmCores {
        ArmCores { plan }
    }
}

impl Cores for ArmCores {
    fn app_cores(&self) -> usize {
        self.plan.app_cores()
    }

    fn phys(&self, core: Core) -> Option<u32> {
        (core.get() <= self.plan.app_cores()).then(|| u32::try_from(core.get()).ok())?
    }

    fn class(&self, _core: Core) -> Option<CoreClass> {
        // Homogeen (allemaal cortex-a53): het board kent geen klassen.
        None
    }

    fn power(&self, core: Core) -> Power {
        match layout::Core::new(core.get()).and_then(|c| el2::core_state(&self.plan, c).ok()) {
            Some(CoreState::Running(_)) => Power::On,
            _ => Power::Off,
        }
    }

    fn kick(&mut self, core: Core) {
        el2::kick(FLAVOR, mpidr(core.get()));
    }
}

/// Fysiek geheugen over `dev`: de adressen komen uit het plan en uit de
/// partitie van een grant, nergens anders vandaan.
pub(crate) struct DevMem;

impl PhysMem for DevMem {
    fn read64(&self, pa: u64) -> u64 {
        dev::read64(Pa(pa))
    }

    fn write64(&mut self, pa: u64, v: u64) {
        dev::write64(Pa(pa), v);
    }

    fn clear(&mut self, pa: u64, len: u64) {
        let Ok(n) = usize::try_from(len) else { return };
        dev::clear(Pa(pa), n);
        dev::push(Pa(pa), n);
    }

    fn clean_inv(&mut self, pa: u64, len: u64) {
        if let Ok(n) = usize::try_from(len) {
            dev::pull(Pa(pa), n);
        }
    }

    fn copy_in(&mut self, pa: u64, src: &[u8]) {
        dev::copy_in(Pa(pa), src);
        // De app leest zijn image met de MMU uit: naar DRAM ermee.
        dev::push(Pa(pa), src.len());
    }

    fn copy_out(&self, dst: &mut [u8], pa: u64) {
        dev::pull(Pa(pa), dst.len());
        dev::copy_out(dst, Pa(pa));
    }
}

/// De tijd van de executor van core 0.
#[derive(Copy, Clone)]
pub(crate) struct ExecTimer(pub(crate) &'static Executor);

impl Timer for ExecTimer {
    fn now(&self) -> u64 {
        self.0.now()
    }

    fn sleep(&self, d: Duration) -> impl Future<Output = ()> {
        self.0.after(d)
    }
}

/// De console van de kern: `cpu::println!`, en een app-regel als
/// `slot N: <regel>`.
#[derive(Copy, Clone)]
pub(crate) struct KernConsole;

impl Console for KernConsole {
    fn log(&self, args: core::fmt::Arguments<'_>) {
        println!("{args}");
    }

    fn app_line(&self, slot: Slot, line: &[u8]) {
        match core::str::from_utf8(line) {
            Ok(s) => println!("slot {slot}: {s}"),
            Err(_) => println!("slot {slot}: {}", line.escape_ascii()),
        }
    }
}

/// De outbox van één levensduur: de lezer op de ring in de staart, en het
/// ctx-blok en de control-page voor de vragen van de servicer.
pub(crate) struct SlotOutbox {
    reader: Option<ring::Reader>,
    ctx: Pa,
    ctrl: Option<Pa>,
}

impl SlotOutbox {
    /// Opent de outbox van de partitie `part`; `ctx` is het ctx-blok van
    /// het slot. Een partitie zonder geldige staart geeft een outbox die
    /// meteen corrupt meldt, zodat de servicer luid stopt.
    pub(crate) fn open(part: Region, ctx: Pa) -> SlotOutbox {
        let tail = tail_of(part);
        SlotOutbox {
            reader: tail.and_then(|t| ring::Reader::open(t.outbox(), RING_DATA_CAP).ok()),
            ctx,
            ctrl: tail.map(|t| t.ctrl_page()),
        }
    }
}

impl Outbox for SlotOutbox {
    fn read_into(&mut self, buf: &mut [u8]) -> Option<(u8, usize)> {
        let rec = self.reader.as_mut()?.read_into(buf)?;
        Some((
            u8::try_from(rec.kind.raw()).unwrap_or(u8::MAX),
            rec.payload.len(),
        ))
    }

    fn corrupt(&self) -> bool {
        self.reader.as_ref().is_none_or(ring::Reader::is_corrupt)
    }

    fn live(&self) -> bool {
        matches!(
            el2::ctx_state(self.ctx),
            Some(CtxState::Running | CtxState::Saved | CtxState::BootPending)
        )
    }

    fn smp_pending(&self) -> bool {
        self.ctrl.is_some_and(|c| {
            dev::pull(c.add(CTRL_SMP_REQ), 8);
            dev::read64(c.add(CTRL_SMP_REQ)) != 0
        })
    }
}
