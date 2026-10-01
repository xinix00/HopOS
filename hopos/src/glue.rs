//! De arch-vrije helft van de kooi-lijm: wat `cage.rs` (arm64) en
//! `cage_riscv.rs` (riscv64) letter voor letter deelden, plus de handvatten
//! van de kern op `dev` en de console die elke laag erboven krijgt.
//!
//! Dit bezit de vier bevestigingen van de switch voor de kooi (attach,
//! detach, publish, unpublish) en niets anders: de staat van een kooi is
//! van de lijm per architectuur, de boekhouding van de lifecycle-actor.

use abi::layout::{self, ABI_TAIL, CtxState, NET_RING_DATA_CAP, RING_DATA_CAP, Tail};
use abi::ring;
use cpu::el2;
use cpu::println;
use dev::Pa;
use kern::cage::{CageError, Console, PhysMem, PortError};
use kern::slots::Outbox;
use kern::{Region, Slot};
use net::ring::AbiTx;
use net::switch::{Ack, Command};

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

/// De foutcodes van [`CageError`] van de lijm. De tekst met de getallen
/// staat op de console (één regel met marker); de code gaat de kern in.
pub(crate) mod code {
    /// Het plan weigerde een slot- of core-index.
    pub(crate) const PLAN: u32 = 1;
    /// De partitie geeft geen geldige ABI-staart.
    pub(crate) const TAIL: u32 = 2;
    /// De kooi weigerde: de stage-2-bouw (arm64), of de Sv39-vertaling of
    /// de PMP-whitelist (riscv64).
    pub(crate) const CAGE: u32 = 3;
    /// Een ring kon niet klaargezet worden.
    pub(crate) const RING: u32 = 4;
    /// Dispatch zonder build.
    pub(crate) const NOT_BUILT: u32 = 5;
    /// Het startschot weigerde (de mailbox, de OS-core, een vol rooster).
    pub(crate) const DISPATCH: u32 = 6;
    /// Een secundaire buiten de span van de kooi, of op de OS-core; op
    /// riscv64 één core per slot.
    pub(crate) const SPAN: u32 = 7;
    /// De bewonerslijst van een core weigerde de kooi.
    pub(crate) const ROSTER: u32 = 8;
}

/// Een [`CageError`] met `code`.
pub(crate) const fn err(code: u32) -> CageError {
    CageError { code }
}

/// Het app-RAM van een partitie: alles onder de ABI-staart.
pub(crate) fn app_ram(part: Region) -> Option<u64> {
    part.size.checked_sub(ABI_TAIL).filter(|n| *n > 0)
}

/// De staart van een partitie, in fysieke adressen.
pub(crate) fn tail_of(part: Region) -> Option<Tail> {
    Tail::new(part.base, app_ram(part)?)
}

/// Hangt de frame-ringen van een verse kooi aan de switch (`hopswitch.
/// Attach` in `armSlot`): ná de ring-init, vóór het startschot. De switch
/// wordt eigenaar van de handvatten; een oude poort op dit slot vervalt.
/// Een volle brievenbus of een switch die er niet is (geen NIC) laat de app
/// zonder slot-LAN draaien: één regel, geen weigering van de start.
pub(crate) fn attach(s: layout::Slot, tail: Tail) {
    if let Some(Err(e)) = ATTACH_ACK.try_take() {
        println!("cage: an earlier attach was refused: {e} HOPOS_CAGE_ATTACH");
    }
    let rings = tail_rings::promise(s, tail.base());
    let (Ok(tx), Ok(rx)) = (
        AbiTx::open(tail.net_tx(), NET_RING_DATA_CAP, rings),
        ring::Writer::open_with(tail.net_rx(), NET_RING_DATA_CAP, rings),
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

/// De belofte van de kern voor de frame-ringen in de staart van een slot,
/// op een board dat de pool Device mapt (Apple, de Radxa): de staart eerst
/// Normal write-back in de kernmap (Go: `mapTailNormal`, slot-ABI 7), en
/// alleen dan belooft de kern zijn kant zonder onderhoud. Weigert de remap,
/// dan blijft het onderhoud: traag maar correct. GEMETEN 01-10: app naar
/// app op de M4 van 52 naar duizenden MB/s (M8), op de Radxa van 29,83 met
/// een corrupte RX-ring (Device tegen de cache van de app) naar 257 tot 262
/// (RX1, ook 40 GiB foutloos).
#[cfg(any(feature = "board-apple", feature = "board-rk3566"))]
mod tail_rings {
    use super::{ABI_TAIL, layout, ring};
    use cpu::println;
    use dev::Pa;

    pub(super) fn promise(s: layout::Slot, base: Pa) -> ring::Coherence {
        match vboard::map_tail_normal(base.0, ABI_TAIL) {
            Ok(()) => ring::Coherence::Hardware,
            Err(why) => {
                println!(
                    "cage: slot {s}: tail {:#x} stays device-mapped ({why}), rings with maintenance HOPOS_CAGE_TAIL",
                    base.0
                );
                ring::Coherence::Maintained
            }
        }
    }
}

/// De belofte van de kern voor de ringen van een slot waar de pool al zo
/// gemapt is als [`crate::net::RINGS`] zegt (op riscv64 `Maintained`: de
/// harts van de C906 zijn niet coherent).
#[cfg(not(any(feature = "board-apple", feature = "board-rk3566")))]
mod tail_rings {
    use super::{layout, ring};
    use dev::Pa;

    pub(super) fn promise(_: layout::Slot, _: Pa) -> ring::Coherence {
        crate::net::RINGS
    }
}

/// Haalt de ringen van `slot` weer van de switch, bij elke stop. FIXME: de
/// kooi-trait is synchroon, dus niemand wacht op de bevestiging; de
/// partitie komt pas vrij na de stil-toets van de actor en een nieuwe claim
/// is een later bericht, en in die tijd draait de switch zijn ronde. Een
/// asynchrone ontkoppel-haak in `kern::slots::stop` maakt dit hard.
pub(crate) fn detach(slot: Slot) {
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
/// open stond, trekt de lifecycle in (`Cage::unpublish`).
pub(crate) async fn publish_ports(slot: Slot, ports: &[u16]) -> Result<(), PortError> {
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
pub(crate) fn unpublish_ports(slot: Slot) {
    let _ = UNPUBLISH_ACK.try_take();
    let cmd = Command::UnpublishSlot {
        slot: slot.get(),
        ack: &UNPUBLISH_ACK,
    };
    if crate::net::COMMANDS.try_send(cmd).is_err() {
        println!("cage: slot {slot}: switch mailbox full, unpublish not sent HOPOS_CAGE_PUBLISH");
    }
}

/// Fysiek geheugen over `dev`, voor de kooi, de image-stream van de
/// system-API en de flip. De adressen komen uit het plan, uit de partitie
/// van een grant (die bewaakt de lifecycle) en uit `layout`, nergens anders
/// vandaan. Wat de kern schrijft, gaat naar DRAM: een app leest zijn
/// partitie met de MMU uit, een nieuwe kern zijn overdracht ook.
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
        dev::push(Pa(pa), src.len());
    }

    fn copy_out(&self, dst: &mut [u8], pa: u64) {
        dev::pull(Pa(pa), dst.len());
        dev::copy_out(dst, Pa(pa));
    }
}

/// De console van de kern: `cpu::println!`, en een app-regel als
/// `slot N: <regel>` zonder zijn regeleinde; een regel die geen UTF-8 is,
/// ge-escaped.
#[derive(Copy, Clone)]
pub(crate) struct KernConsole;

impl Console for KernConsole {
    fn log(&self, args: core::fmt::Arguments<'_>) {
        println!("{args}");
    }

    fn app_line(&self, slot: Slot, line: &[u8]) {
        let line = line.trim_ascii_end();
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
            dev::pull(c.add(abi::hopabi::CTRL_SMP_REQ), 8);
            dev::read64(c.add(abi::hopabi::CTRL_SMP_REQ)) != 0
        })
    }
}
