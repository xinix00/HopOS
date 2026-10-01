//! De kooi-lijm: de traits van `kern::cage` over `cpu::el2`, `cpu::smp` (CPU_ON),
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
    AppStatus, CTRL_APP_FAULT_ELR, CTRL_APP_FAULT_ESR, CTRL_APP_FAULT_FAR, CTRL_APP_FAULT_VEC,
    CTRL_CORES, CTRL_ENTRY, CTRL_EXIT_CODE, CTRL_FAULT_ESR, CTRL_FAULT_FAR, CTRL_FAULT_VEC,
    CTRL_HEARTBEAT, CTRL_IDLE_MODE, CTRL_KILL, CTRL_MBOX_PA, CTRL_RAM_SIZE, CTRL_S2_TABLE,
    CTRL_SHARED, CTRL_SLOT, CTRL_SMP_REQ, CTRL_STATUS, CTRL_VEC_PA, CTRL_WALL_OFF, IDLE_YIELD,
};
use abi::layout::{
    self, ABI_TAIL, CTRL_STRIDE, CTX_KICK_TARGET, CTX_SMP, CtxState, LINK_BASE, NET_RING_DATA_CAP,
    Plan, RING_DATA_CAP, Tail,
};
use abi::ring;
use core::future::Future;
use core::time::Duration;
use cpu::el2::{self, CoreState, Flavor, Installed, Join, Start};
use cpu::println;
use dev::Pa;
use executor::Executor;
use kern::cage::{
    Cage, CageError, Console, CoreClass, Cores, PhysMem, PortError, Power, Status, Timer,
};
use kern::slots::Outbox;
use kern::{Core, Region, SLOT_CAP, Slot};
use net::ring::AbiTx;
use net::switch::{Ack, Command};
use vboard::slots::mpidr;

/// De bevestiging van de `Attach` van een verse kooi aan de switch. Niemand
/// wacht erop (de kooi-trait is synchroon); het resultaat wordt bij de
/// volgende attach opgehaald en gemeld als het een weigering was.
static ATTACH_ACK: Ack = Ack::new();
/// De bevestiging van de `Detach` bij een stop.
static DETACH_ACK: Ack = Ack::new();
/// De bevestiging van elke `Publish` van de poorten van een jobspec. De
/// lifecycle-actor is de enige zender en wacht elke bevestiging af.
static PUBLISH_ACK: Ack = Ack::new();
/// De bevestiging van de `UnpublishSlot` bij een stop. Niemand wacht erop:
/// de brievenbus is een rij, dus een publicatie van een volgende start komt
/// altijd ná deze intrekking aan de beurt.
static UNPUBLISH_ACK: Ack = Ack::new();

/// De EL2-smaak van de switcher (cpu::el2 `Flavor`), gekozen door het board
/// en niet door een losse bouwvlag:
/// - `Nvhe` (E2H=0, slapen in WFE) op QEMU virt, de Pi's, de Radxa en de
///   Altra;
/// - `Vhe` (E2H=1, de EL1-registers als `_EL12`) op de O6N: op de A720
///   stierf een EL1 onder nVHE binnen een halve seconde (Go, 17-09), dus
///   daar is VHE geen keuze maar een eis;
/// - `AppleVhe` op Apple silicium: E2H is er RES1 en de kick is de fast IPI.
///
/// De feature `vhe` van hopos dwingt `Vhe` af op het UEFI-board, samen met
/// de kern onder E2H = 1 (board-uefi `vhe`): zo worden de VHE-kern en de
/// VHE-switcher op QEMU (`CPU=neoverse-n1`, EDK2) bewezen vóór ze op de
/// O6N draaien. De keuze hangt aan de board-features omdat de andere boards
/// geen `FLAVOR`-constante dragen; Apple heeft er wel een
/// (`board_apple::FLAVOR`) en die hoort hiermee overeen te komen.
pub(crate) const FLAVOR: Flavor = if cfg!(feature = "board-apple") {
    Flavor::AppleVhe
} else if cfg!(any(feature = "board-o6n", feature = "vhe")) {
    Flavor::Vhe
} else {
    Flavor::Nvhe
};

// De smaak `Vhe` eist een kern onder E2H = 1: de OS-core-rotatie gebruikt
// de `_EL12`-encoderingen op de core van de kern zelf, en die zijn onder
// E2H = 0 UNDEFINED (29-09, QEMU neoverse-n1: EC 0x0 in
// `hopos_os_vhe_enter` bij de zelftest). Op het UEFI-board kiest de feature
// `vhe` van board-uefi die vorm (`KERN_VHE`); hier toetst de build dat
// switcher en kern dezelfde vorm hebben.
#[cfg(any(feature = "board-uefi", feature = "board-o6n", feature = "board-altra"))]
const _: () = assert!(
    vboard::KERN_VHE == matches!(FLAVOR, Flavor::Vhe),
    "the EL2 flavor of the switcher and the E2H form of the kern differ"
);
// Apple: het board draagt zijn eigen smaak (`board_apple::FLAVOR`, E2H is
// RES1 en de kick is de fast IPI); de lijm moet precies die installeren, en
// de OS-core-rotatie en de koude start (`cpu::smp::cpu_on`, de haak van het
// board in plaats van PSCI) lopen dan over hetzelfde silicium.
#[cfg(feature = "board-apple")]
const _: () = assert!(
    matches!(FLAVOR, Flavor::AppleVhe) && matches!(vboard::FLAVOR, Flavor::AppleVhe),
    "the cage glue must install the Apple flavor of the board"
);
// Alleen het UEFI-board kent een kern onder E2H = 1 (Apple is er VHE-only
// van zichzelf); `vhe` op een ander board gaf een nVHE-kern met een
// VHE-rotatie, en die valt bij de eerste zelftest.
#[cfg(all(
    feature = "vhe",
    not(any(feature = "board-uefi", feature = "board-o6n", feature = "board-apple"))
))]
compile_error!(
    "the feature `vhe` needs a board whose kern runs under E2H = 1 (board-uefi, board-o6n)"
);

/// Hoe een app-core idlet (`CTRL_IDLE_MODE`). Op QEMU virt een yield naar
/// de switcher (HVC #1): de kern slaapt daar in WFI, die geen SEV hoort,
/// dus een app die publiceert en dan op zijn antwoord wacht, bereikte de
/// kern pas op diens volgende deadline (de failsafe van de switch, 1 ms).
/// De switcher belt de OS-core bij een yield (`cpu::el2::OsCore::listen`).
/// GEMETEN 30-09, `tools/qemu-test-hop.sh` (4 cores): de `dial_us` van
/// appspike 3840 en 3543 met WFE-idle, 2852 en 2852 met yield-idle, bij 38
/// kicks per appspike-run. Een board met WFE in de kern hoort de SEV wel en
/// laat het veld leeg (de app kiest dan zelf).
#[cfg(feature = "board-qemuvirt")]
const APP_IDLE_MODE: u64 = IDLE_YIELD;
#[cfg(not(feature = "board-qemuvirt"))]
const APP_IDLE_MODE: u64 = 0;

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
    /// CPU_ON faalde (PSCI, of de haak van het board: `cpu::smp::cpu_on`);
    /// de code erbij is 0x100 plus de fout in PSCI-vorm.
    pub(super) const PSCI: u32 = 0x100;
    /// Een secundaire buiten de span van de kooi, of op de OS-core.
    pub(super) const SPAN: u32 = 7;
    /// De bewonerslijst van een gedeelde core weigerde de kooi.
    pub(super) const ROSTER: u32 = 8;
}

/// Hoe lang [`ArmCage::dispatch`] op een gedeelde app-core wacht tot de
/// rotatie een nieuwe bewoner oppikt. Een buur die idle is, geeft de core
/// binnen een event-stream-periode (~1,5 ms) of meteen na de kick; langer
/// is een buur die rekent, en dan start de nieuwe bewoner bij diens
/// volgende yield (een regel, geen fout: compute hoort op een eigen core).
/// Het wachten is een spin op de kern-core: de kooi-trait is synchroon, en
/// de park-race hieronder moet in dezelfde stap gesloten worden.
const JOIN_WAIT_NS: u64 = 5_000_000;

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
    /// De stage-2-L1 van de kooi: het gezag van elke secundaire, uit de
    /// bouw en nooit van de app-schrijfbare page (`CTRL_S2_TABLE`).
    l1: Pa,
    /// De vertrouwde SMP-breedte (1 = geen SMP).
    cores: usize,
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
        // De boot-core, als de kern bij boot naar de OS-core verhuisde
        // (main, `hopos.oscore`): nu is er een parkeerlus, en wordt hij een
        // geparkeerde app-core, vóór de eerste dispatch.
        if let Some(m) = el2::held() {
            let phys = vboard::slots::core_of(m);
            let core = plan.logical_core(phys).ok_or(el2::Error::UnparkedCore {
                core: phys,
                mbox: 0,
            })?;
            el2::release_held(&plan, core)?;
            println!("oscore: boot core {phys} parked as app core {core} HOPOS_OSCORE_PARKED");
        }
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
            (CTRL_IDLE_MODE, APP_IDLE_MODE),
        ] {
            dev::write64(ctrl.add(off), v);
        }
        // Het zaad vóór de start: de app mengt het bij zijn eerste
        // willekeur (applib::rand, seed.rs).
        crate::seed::plant(ctrl);
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

// FLIP: de adoptie na een kern-flip (hopos/src/flip.rs).
impl ArmCage {
    /// Neemt de zittende kooi-regio over: de switch-code moet dezelfde som
    /// hebben als de onze (`cpu::el2::adopt`), en er wordt niets
    /// geschreven, want er draaien cores in.
    pub(crate) fn adopt(plan: Plan) -> Result<ArmCage, el2::Error> {
        let installed = el2::adopt(&plan, FLAVOR)?;
        Ok(ArmCage {
            plan,
            installed,
            built: [None; SLOT_CAP + 1],
        })
    }

    /// Geeft een levende bewoner zijn plek in de lijm terug: de
    /// control-page, de core en de SMP-breedte `cores` (voor status, stop,
    /// klok en de secundaires bij een revoke), en zijn frame-ringen aan de
    /// switch van deze kern. Zonder ring-init: de indexen staan in de ring
    /// zelf, en de app schrijft er nog in. De breedte komt uit het
    /// handoff-blob (de kern schreef het, niet de app), dus hij is even
    /// vertrouwd als die van de bouw.
    pub(crate) fn adopt_slot(
        &mut self,
        slot: Slot,
        part: Region,
        core: usize,
        cores: usize,
    ) -> Result<(), CageError> {
        let s = self.slot(slot)?;
        let c = layout::Core::new(core).ok_or(err(code::PLAN))?;
        let tail = tail_of(part).ok_or(err(code::TAIL))?;
        let ctrl = tail.ctrl_page();
        dev::pull(ctrl.add(CTRL_ENTRY), 8);
        let entry = dev::read64(ctrl.add(CTRL_ENTRY));
        let l1 = self.plan.cage_table_pa(s).map_err(|_| err(code::PLAN))?;
        if let Some(b) = self.built.get_mut(slot.get()) {
            *b = Some(Built {
                ctrl,
                core: c,
                entry,
                part,
                l1,
                cores: cores.max(1),
            });
        }
        // De OS-core: de nieuwe kern geeft Hop de core weer in zijn idle.
        // Het plan (en dus de bewonerslijst van sched-blok 0) overleefde de
        // flip ongeschreven; staat hij er toch niet meer in, dan terug, met
        // zijn bewaarde staat.
        if core == 0 {
            let ctx = self.plan.ctx_pa(s).map_err(|_| err(code::PLAN))?;
            match el2::rehost(&self.plan, ctx) {
                Ok(again) => println!(
                    "cage: slot {slot} back in the OS core rotation (re-added: {again}) HOPOS_OS_HOST"
                ),
                Err(e) => {
                    println!(
                        "cage: slot {slot} not back on the OS core: {e} HOPOS_FLIP_ADOPT_FAIL"
                    );
                    return Err(err(code::DISPATCH));
                }
            }
        }
        attach(s, tail);
        crate::clock::attach(slot, ctrl);
        // Vers zaad van déze kern; de generatie telt door vanaf die van de
        // vorige, dus de bewoner ziet hem als nieuw (seed.rs).
        crate::seed::plant(ctrl);
        println!(
            "cage: slot {slot} adopted: part {:#x}+{:#x}, ctrl {:#x}, core {core}, entry {entry:#x}",
            part.base, part.size, ctrl.0
        );
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
        AbiTx::open(tail.net_tx(), NET_RING_DATA_CAP, crate::net::RINGS),
        ring::Writer::open_with(tail.net_rx(), NET_RING_DATA_CAP, crate::net::RINGS),
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

/// Zet de poorten van een jobspec door, elk voor tcp en udp (Go's
/// `armSlot`: de jobspec kent geen protocol, en een app die er één bedient
/// laat de ander onbeantwoord). Stopt bij de eerste weigering; wat er al
/// open stond, trekt de lifecycle in ([`Cage::unpublish`]).
async fn publish_ports(slot: Slot, ports: &[u16]) -> Result<(), PortError> {
    use net::nat::Proto;
    if !crate::net::switch_up() {
        let port = ports.first().copied().unwrap_or(0);
        println!(
            "cage: slot {slot}: no switch on this node, port {port} not published HOPOS_CAGE_PUBLISH"
        );
        return Err(PortError::Refused { port });
    }
    for &port in ports {
        for proto in [Proto::Tcp, Proto::Udp] {
            match crate::net::publish_via(&PUBLISH_ACK, proto, slot.get(), port).await {
                Ok(()) => {}
                Err(net::Error::AlreadyPublished { port, slot: owner }) => {
                    return Err(PortError::Taken { port, owner });
                }
                Err(e) => {
                    println!("cage: slot {slot}: port {port}: {e} HOPOS_CAGE_PUBLISH");
                    return Err(PortError::Refused { port });
                }
            }
        }
    }
    Ok(())
}

/// Trekt de publicaties (en flows) van `slot` in, zonder te wachten.
fn unpublish_ports(slot: Slot) {
    let _ = UNPUBLISH_ACK.try_take();
    let cmd = Command::UnpublishSlot {
        slot: slot.get(),
        ack: &UNPUBLISH_ACK,
    };
    if crate::net::COMMANDS.try_send(cmd).is_err() {
        println!("cage: slot {slot}: switch mailbox full, unpublish not sent HOPOS_CAGE_PUBLISH");
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
        // Eerst uit elke oude bewonerslijst (een vorige levensduur op een
        // gedeelde core laat een dode byte achter), vóór enige staatswissel
        // van deze levensduur: zie `el2::forget` en de hercontrole van de
        // rotatie.
        el2::forget(&self.plan, s).map_err(|e| {
            println!("cage: slot {slot}: resident lists: {e} HOPOS_CAGE_ROSTER");
            err(code::ROSTER)
        })?;
        let l1 = el2::stage2::build(block, LINK_BASE, part.base, part.size).map_err(|e| {
            println!("cage: slot {slot}: stage-2 refused: {e} HOPOS_CAGE_STAGE2");
            err(code::STAGE2)
        })?;
        self.arm_tail(s, tail, l1, entry, core, cores)?;
        self.arm_smp(s, tail.ctrl_page(), core, cores)?;
        if let Some(b) = self.built.get_mut(slot.get()) {
            *b = Some(Built {
                ctrl: tail.ctrl_page(),
                core,
                entry,
                part,
                l1,
                cores,
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
        let phys = self.plan.phys_core(c);
        // De startregel van elke levensduur, welke weg hij ook kwam (de
        // boot-plaatsing of `STREAM_IMAGE` van Hop): hier, want dit is de
        // ene plek waar elke start langskomt. `core` is logisch (0 = de
        // OS-core), `cpu` fysiek.
        let started = || {
            println!(
                "HOPOS_SLOT_START slot={slot} core={core} cpu={phys} entry={:#x} part={:#x}+{:#x}",
                b.entry, b.part.base, b.part.size
            );
        };
        if core == Core::OS {
            return self.host(slot, ctx, b, phys, started);
        }
        // Een app-core: stond dit slot ooit op de OS-core, dan niet meer.
        let _ = el2::unhost(&self.plan, ctx);
        // Draait de core al (een buur uit dezelfde sharegroup), dan komt de
        // kooi erbij in de rotatie; anders is het een gewoon startschot.
        if matches!(el2::core_state(&self.plan, c), Ok(CoreState::Running(_))) {
            match self.join(slot, c, ctx, tramp, b.ctrl) {
                Ok(true) => {
                    started();
                    return Ok(());
                }
                Ok(false) => {} // De core parkeerde: het startschot hieronder.
                Err(e) => return Err(e),
            }
        }
        match el2::dispatch(&self.plan, c, ctx, tramp, b.ctrl.0) {
            Ok(Start::Woken) => {
                println!("cage: slot {slot} dispatched to parked core {core} (mailbox + SEV)");
                started();
                Ok(())
            }
            Ok(Start::Cold) => {
                // De eerste opgang van deze core: CPU_ON rechtstreeks
                // de trampoline in, x0 = de control-page. Daarna leeft hij
                // in de parkeerlus van HopOS en gaat elke dispatch via de
                // mailbox.
                let target = mpidr(phys);
                let r = cpu::smp::cpu_on(target, tramp.0, b.ctrl.0);
                println!(
                    "cage: slot {slot} core {core} cold: CPU_ON mpidr={target:#x} entry={:#x} x0={:#x} -> {r:?}",
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
        let b = self.built(slot).ok_or(err(code::NOT_BUILT))?;
        let s = self.slot(slot)?;
        let c = layout::Core::new(core.get()).ok_or(err(code::PLAN))?;
        // De kern gaf de breedte al tegen zijn eigen boekhouding (nooit de
        // page); hier alleen nog de vorm: een secundaire is een andere core
        // van de span, nooit de OS-core.
        let span = b.core.get()..b.core.get() + b.cores;
        if core == Core::OS || c == b.core || !span.contains(&c.get()) {
            println!(
                "cage: slot {slot}: SMP core {core} outside span {}..{} HOPOS_SMP_DISPATCH_FAIL",
                span.start, span.end
            );
            return Err(err(code::SPAN));
        }
        let ctx = self.plan.smp_ctx_pa(c).map_err(|_| err(code::PLAN))?;
        let mbox = self.plan.park_mbox_pa(c).map_err(|_| err(code::PLAN))?;
        // De handoff is node-owned: de EL1-staat komt van de page van de app,
        // al het EL2-gezag (tabel, VMID, mailbox, vectoren) van hier. Na de
        // kopie kan de app hem niet meer veranderen.
        let handoff = ctx.add(CTX_SMP);
        el2::prepare_smp(
            handoff,
            b.ctrl,
            b.l1.0,
            s.get() as u64,
            mbox,
            self.plan.vec_base_pa(),
        );
        let tramp = self.installed.smp_tramp;
        let phys = self.plan.phys_core(c);
        match el2::dispatch(&self.plan, c, ctx, tramp, handoff.0) {
            Ok(Start::Woken) => {
                println!(
                    "cage: slot {slot} SMP core {core} (cpu {phys}) dispatched to parked core HOPOS_SMP_CORE"
                );
                Ok(())
            }
            Ok(Start::Cold) => {
                let target = mpidr(phys);
                let r = cpu::smp::cpu_on(target, tramp.0, handoff.0);
                println!(
                    "cage: slot {slot} SMP core {core} (cpu {phys}) cold: CPU_ON mpidr={target:#x} -> {r:?} HOPOS_SMP_CORE"
                );
                r.map_err(|e| err(code::PSCI + e.code().unsigned_abs() as u32))
            }
            Err(e) => {
                println!("cage: slot {slot} SMP core {core}: {e} HOPOS_SMP_DISPATCH_FAIL");
                Err(err(code::DISPATCH))
            }
        }
    }

    fn request_exit(&mut self, slot: Slot) {
        // Elke stop begint hier: eerst van de switch af, dan de kill-vlag.
        detach(slot);
        self.ctrl_write(slot, CTRL_KILL, 1);
        // Een buur die alleen overblijft, hoeft niet meer te yielden.
        if let Some(b) = self.built(slot) {
            self.refresh_shared(b.core, Some(slot));
        }
    }

    fn quiet(&self, slot: Slot, core: Core) -> bool {
        let dead = matches!(self.ctx_state(slot), Some(CtxState::Empty | CtxState::Dead));
        if core == Core::OS {
            // De OS-core parkeert nooit (de kern draait er); stil is dood,
            // of niet meer in zijn rotatie.
            return dead || !self.ctx(slot).is_some_and(|c| el2::hosts(&self.plan, c));
        }
        let Some(c) = layout::Core::new(core.get()) else {
            return false;
        };
        let parked = el2::core_state(&self.plan, c)
            .is_ok_and(|st| matches!(st, CoreState::Cold | CoreState::Parked));
        // De primaire is de context van de kooi; elke andere core van de
        // span is een secundaire met een eigen ctx-blok. Tot 30-09 las dit
        // voor elke core de staat van de primaire: een SMP-app waarvan de
        // primaire al dood was, telde zo een secundaire die nog draaide als
        // stil (E9).
        if self.built(slot).is_none_or(|b| b.core == c) {
            return dead || parked;
        }
        let sec_dead = self
            .plan
            .smp_ctx_pa(c)
            .is_ok_and(|x| matches!(el2::ctx_state(x), Some(CtxState::Empty | CtxState::Dead)));
        sec_dead || parked
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
        self.evict_all(slot);
        // Op de OS-core is de kern de enige die een bewoner de core geeft:
        // uit de rotatie is een bevestigd einde, ook voor een bewoner die
        // met een verre wektijd lag te slapen en de intrekking nooit zou
        // voelen.
        if let Some(ctx) = self.ctx(slot)
            && el2::unhost(&self.plan, ctx).unwrap_or(false)
        {
            el2::ctx_write(ctx, layout::CTX_STATE, CtxState::Dead.raw());
            println!("cage: slot {slot}: out of the OS core rotation HOPOS_OS_UNHOST");
        }
    }

    fn smp_request(&self, slot: Slot) -> u64 {
        self.ctrl_read(slot, CTRL_SMP_REQ)
    }

    fn clear_smp_request(&mut self, slot: Slot) {
        self.ctrl_write(slot, CTRL_SMP_REQ, 0);
    }

    fn status(&self, slot: Slot) -> Status {
        // Op de OS-core staat de core altijd aan; de vraag is of de bewoner
        // er nog in de rotatie staat.
        let core_on = self.built(slot).is_some_and(|b| {
            if b.core.get() == 0 {
                self.ctx(slot).is_some_and(|c| el2::hosts(&self.plan, c))
            } else {
                // Op een app-core de context van DEZE kooi, niet alleen de
                // core: een gedeelde core draait door zolang er een buur
                // leeft, en Hop las een lid dat al exit deed dan als
                // draaiend (30-09, de SHARE-toets: nooit herstart).
                el2::core_state(&self.plan, b.core)
                    .is_ok_and(|st| matches!(st, CoreState::Running(_)))
                    && self.live(slot)
            }
        });
        Status {
            core_on,
            app: self.ctrl_read(slot, CTRL_STATUS),
            exit_code: self.ctrl_read(slot, CTRL_EXIT_CODE),
            heartbeat: self.ctrl_read(slot, CTRL_HEARTBEAT),
            ram_size: self.ctrl_read(slot, CTRL_RAM_SIZE),
            fault_vec: self.ctrl_read(slot, CTRL_FAULT_VEC),
            fault_esr: self.ctrl_read(slot, CTRL_FAULT_ESR),
            fault_far: self.ctrl_read(slot, CTRL_FAULT_FAR),
            app_fault_vec: self.ctrl_read(slot, CTRL_APP_FAULT_VEC),
            app_fault_esr: self.ctrl_read(slot, CTRL_APP_FAULT_ESR),
            app_fault_elr: self.ctrl_read(slot, CTRL_APP_FAULT_ELR),
            app_fault_far: self.ctrl_read(slot, CTRL_APP_FAULT_FAR),
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

// SMP-apps en sharegroups op de app-cores (30-09).
impl ArmCage {
    /// De MPIDR-affiniteit van logische `core`, zoals de switcher hem bij
    /// een yield in `CTX_KICK_TARGET` zet (aff0..aff2).
    fn affinity(&self, core: layout::Core) -> u64 {
        mpidr(self.plan.phys_core(core)) & 0xFF_FFFF
    }

    /// De contexten van een SMP-eenheid vóór de eerste dispatch
    /// (`prepareSMPContexts`): per secundaire een gewist ctx-blok op zijn
    /// eigen core, en de vertrouwde wek-keten rond. Eén core: de keten van
    /// een vorige SMP-levensduur van dit slot gaat eraf.
    ///
    /// Het wekdoel van ook de primaire staat er meteen: een secundaire die
    /// de primaire wekt (HVC #4) vóór diens eerste yield, vond hem anders
    /// niet in de keten, en die wek was dan verloren tot de wektijd.
    fn arm_smp(
        &self,
        s: layout::Slot,
        ctrl: Pa,
        core: layout::Core,
        cores: usize,
    ) -> Result<(), CageError> {
        let prim = self.plan.ctx_pa(s).map_err(|_| err(code::PLAN))?;
        if cores <= 1 {
            el2::chain(&[prim]);
            return Ok(());
        }
        if cores > SMP_MAX {
            println!("cage: slot {s}: {cores} cores, the chain carries {SMP_MAX} HOPOS_CAGE_SMP");
            return Err(err(code::SPAN));
        }
        let mut ring = [prim; SMP_MAX];
        let n = cores;
        for (k, slot) in ring.iter_mut().enumerate().take(n).skip(1) {
            let c = layout::Core::new(core.get() + k).ok_or(err(code::SPAN))?;
            *slot =
                el2::prepare_secondary(&self.plan, c, s, ctrl, self.affinity(c)).map_err(|e| {
                    println!("cage: slot {s}: SMP context on core {c}: {e} HOPOS_CAGE_SMP");
                    err(code::SPAN)
                })?;
        }
        el2::ctx_write(prim, CTX_KICK_TARGET, self.affinity(core));
        el2::chain(ring.get(..n).unwrap_or(&[]));
        println!("cage: slot {s}: {n} cores from core {core}, contexts chained HOPOS_CAGE_SMP");
        Ok(())
    }

    /// Zet `slot` erbij op de draaiende app-core `c` (een sharegroup) en
    /// wacht kort tot de rotatie hem oppikt. `Ok(false)`: de core staat
    /// stil, en het is een gewoon startschot.
    ///
    /// De park-race uit share.go: de rotatie las de lijst nét vóór onze
    /// append, zag niemand meer, en parkeert. Dan pikt niemand de
    /// boot-pending bewoner op, en ziet deze wacht de core geparkeerd: dan
    /// alsnog het mailbox-startschot, en dat is dan het enige (de
    /// parkeerlus leest geen lijst). Ziet de wacht niets binnen
    /// [`JOIN_WAIT_NS`], dan rekent de buur, en start de bewoner bij diens
    /// volgende yield.
    fn join(
        &self,
        slot: Slot,
        c: layout::Core,
        ctx: Pa,
        tramp: Pa,
        ctrl: Pa,
    ) -> Result<bool, CageError> {
        let joined = el2::join(&self.plan, c, ctx, tramp, ctrl.0).map_err(|e| {
            println!("cage: slot {slot} on shared core {c}: {e} HOPOS_CAGE_DISPATCH");
            err(code::ROSTER)
        })?;
        if joined == Join::Idle {
            return Ok(false);
        }
        el2::kick(FLAVOR, mpidr(self.plan.phys_core(c)));
        let t0 = cpu::idle::now();
        let picked = loop {
            if el2::ctx_state(ctx) != Some(CtxState::BootPending) {
                break Some(true);
            }
            if matches!(el2::core_state(&self.plan, c), Ok(CoreState::Parked)) {
                break Some(false);
            }
            if cpu::idle::now().saturating_sub(t0) > JOIN_WAIT_NS {
                break None;
            }
            core::hint::spin_loop();
        };
        let mut others = 0usize;
        let _ = el2::residents(&self.plan, c, |id| {
            if usize::from(id) != slot.get() {
                others += 1;
            }
        });
        match picked {
            Some(false) => {
                println!(
                    "cage: slot {slot}: shared core {c} parked while it joined, dispatching HOPOS_SHARE_JOIN"
                );
                return Ok(false);
            }
            Some(true) => println!(
                "cage: slot {slot} joined shared core {c} next to {others} resident(s), up in {} us HOPOS_SHARE_JOIN",
                cpu::idle::now().saturating_sub(t0) / 1000
            ),
            None => println!(
                "cage: slot {slot} waits on shared core {c}: a neighbour has not yielded in {} ms HOPOS_SHARE_PENDING",
                JOIN_WAIT_NS / 1_000_000
            ),
        }
        self.refresh_shared(c, None);
        Ok(true)
    }

    /// Zet `CTRL_SHARED` van elke levende bewoner van app-core `c`: 1 als er
    /// twee of meer zijn (hun idle yieldt dan, zodat de buren draaien),
    /// anders 0 (share.go `refreshShared`). `leaving` telt niet meer mee:
    /// die is gevraagd te stoppen. De kern is de enige schrijver van dit
    /// woord; de app leest het alleen.
    fn refresh_shared(&self, c: layout::Core, leaving: Option<Slot>) {
        if c.get() == 0 {
            return;
        }
        let mut live = [0u8; SHARE_SCAN];
        let mut n = 0;
        let _ = el2::residents(&self.plan, c, |id| {
            let cage = usize::from(id);
            let Some(slot) = Slot::new(cage).filter(|s| Some(*s) != leaving) else {
                return;
            };
            if self.live(slot)
                && let Some(x) = live.get_mut(n)
            {
                *x = id;
                n += 1;
            }
        });
        let shared = u64::from(n >= 2);
        for id in live.iter().take(n) {
            if let Some(slot) = Slot::new(usize::from(*id)) {
                self.ctrl_write(slot, CTRL_SHARED, shared);
            }
        }
    }

    /// Haalt elke context van `slot` uit de rotatie van zijn core(s), na de
    /// intrekking: de primaire van zijn core, elke secundaire van de zijne.
    /// Een geyielde context met een verre wektijd hervatte anders pas op
    /// die wektijd en voelde de intrekking zo lang niet; dan liep de stop
    /// in quarantaine terwijl er niets meer draaide (`el2::evict`).
    fn evict_all(&self, slot: Slot) {
        let Some(b) = self.built(slot) else { return };
        if b.core.get() == 0 {
            return; // De OS-core: `unhost`, hieronder in `revoke`.
        }
        let Some(prim) = self.ctx(slot) else { return };
        if let Err(e) = el2::evict(&self.plan, b.core, prim) {
            println!(
                "cage: slot {slot}: evict from core {}: {e} HOPOS_CAGE_REVOKE",
                b.core
            );
        }
        for k in 1..b.cores {
            let Some(c) = layout::Core::new(b.core.get() + k) else {
                break;
            };
            if let Ok(x) = self.plan.smp_ctx_pa(c)
                && let Err(e) = el2::evict(&self.plan, c, x)
            {
                println!("cage: slot {slot}: evict SMP core {c}: {e} HOPOS_CAGE_REVOKE");
            }
        }
        self.refresh_shared(b.core, Some(slot));
    }
}

/// De grootste SMP-eenheid die de lijm aan elkaar ketent: meer dan de
/// app-cores van elk board dat we hebben (de O6N: 12).
const SMP_MAX: usize = 16;

/// Hoeveel bewoners van één gedeelde core [`ArmCage::refresh_shared`]
/// hoogstens bijwerkt.
const SHARE_SCAN: usize = SLOT_CAP;

// De OS-core (PORT.md beslissing 2): de bewoners van de kern-core.
impl ArmCage {
    /// Maakt `slot` bewoner van de OS-core (fysiek `cpu`): de rotatie van
    /// de kern (`cpu::el2::OsCore`) geeft hem de core in de idle van de
    /// executor. Zijn idle wordt een yield (`CTRL_IDLE_MODE`): WFE op EL1
    /// zou de hele core laten slapen, en op QEMU spinnen tot de volgende
    /// interrupt.
    fn host(
        &self,
        slot: Slot,
        ctx: Pa,
        b: Built,
        cpu: usize,
        started: impl Fn(),
    ) -> Result<(), CageError> {
        dev::write64(b.ctrl.add(CTRL_IDLE_MODE), IDLE_YIELD);
        dev::push(b.ctrl.add(CTRL_IDLE_MODE), 8);
        match el2::host(&self.plan, ctx, b.entry, b.ctrl.0) {
            Ok(()) => {
                println!(
                    "cage: slot {slot} hosted on the OS core (cpu {cpu}), next to the kern HOPOS_OS_HOST"
                );
                started();
                Ok(())
            }
            Err(e) => {
                println!("cage: slot {slot} on the OS core: {e} HOPOS_CAGE_DISPATCH");
                Err(err(code::DISPATCH))
            }
        }
    }
}

/// De cores van QEMU virt: logische core 0 is de OS-core, app-core i de
/// i-de andere fysieke core (`Plan::phys_core`), MPIDR via `mpidr`; de
/// toestand van een app-core komt uit de park-mailbox.
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
        let c = layout::Core::new(core.get()).filter(|_| core.get() <= self.plan.app_cores())?;
        u32::try_from(self.plan.phys_core(c)).ok()
    }

    fn class(&self, _core: Core) -> Option<CoreClass> {
        // Homogeen (allemaal cortex-a53): het board kent geen klassen.
        None
    }

    fn power(&self, core: Core) -> Power {
        if core == Core::OS {
            return Power::On; // de kern zelf
        }
        match layout::Core::new(core.get()).and_then(|c| el2::core_state(&self.plan, c).ok()) {
            Some(CoreState::Running(_)) => Power::On,
            _ => Power::Off,
        }
    }

    fn kick(&mut self, core: Core) {
        // De OS-core hoeft geen kick: de kern geeft zijn bewoners zelf de
        // core, in elke idle-ronde.
        if let Some(c) = layout::Core::new(core.get()).filter(|_| core != Core::OS) {
            el2::kick(FLAVOR, mpidr(self.plan.phys_core(c)));
        }
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

    fn sleep_deferrable(&self, d: Duration) -> impl Future<Output = ()> {
        self.0.after_deferrable(d)
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
