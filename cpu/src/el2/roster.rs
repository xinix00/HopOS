//! De bewonerslijst van een core: één implementatie voor elke schrijver
//! (de kooi op arm64 en riscv64, de OS-core en de app-cores) en elke lezer
//! in Rust ([`super::next`]). De switchers lezen hem in hun assembly met
//! dezelfde regels.
//!
//! De lijst staat in het sched-blok van de core: de lengte op `SCHED_COUNT`
//! en `SLOT_CAP` bytes op `SCHED_LIST`, één kooi-context-id per byte, 0 is
//! een gat. Een gat houdt zijn plek en de lengte groeit alleen; een nieuwe
//! bewoner neemt het eerste gat (share.go `residentAdd`).
//!
//! De kern is de enige schrijver (regel 1..3 van het blok, de regel van de
//! cachelines van de C906: `abi::layout::SchedBlock`), dus hij leest zijn
//! eigen schrijfwerk zonder veeg. Eén schrijfvolgorde, die van een ring:
//! groeien is eerst de ingang, een barrière, dan de lengte; krimpen eerst
//! de lengte, dan de ingangen. Een lezer die de nieuwe lengte ziet, ziet de
//! ingang die erbij hoort. Na elke wijziging gaan de regels naar DRAM
//! (`dev::push`), voor een switcher op een hart dat niet coherent is.
//!
//! Woordgewijs (handboek §5, device-geheugen is woordgewijs): een byte
//! schrijven is lees-wijzig-schrijf van zijn woord. Dat mag omdat de kern
//! de enige schrijver is; een lezer ziet per byte de oude of de nieuwe.
//!
//! Gekozen uit de drie die er tot 03-10 waren (`el2::dispatch` voor de
//! app-cores op arm64, `el2::oscore` voor de OS-core, `cage_riscv.rs` voor
//! riscv64): de vorm van `dispatch` (eerst kijken of hij er al staat, dan
//! pas vol; de ingang vóór de lengte met een barrière; de push), met de
//! woordtoegang van riscv. De OS-core schreef de lengte vóór de ingang, en
//! riscv duwde de lengte vóór de lijst naar DRAM.

use super::Error;
use super::layout::{Core, CtxState, PARK_MBOX_LEN, Plan, SCHED_COUNT, SCHED_LIST, SLOT_CAP, Slot};
use dev::Pa;

/// Het sched-blok (en dus de bewonerslijst) van logische core `core`; 0 is
/// de OS-core.
pub fn sched(plan: &Plan, core: Core) -> Result<Pa, Error> {
    plan.park_mbox_pa(core).map_err(Error::Plan)
}

/// De lengte van de lijst van `sched`, geklemd op [`SLOT_CAP`]. Die klem is
/// een isolatiegrens: één plek voorbij de lijst is `SCHED_S2_PA`, waarmee
/// de switcher elk ctx-blok op de core vindt.
#[must_use]
pub fn len(sched: Pa) -> usize {
    usize::try_from(dev::read64(sched.add(SCHED_COUNT)))
        .unwrap_or(SLOT_CAP)
        .min(SLOT_CAP)
}

/// Ingang `i` van de lijst van `sched` (0 = een gat).
#[must_use]
pub fn get(sched: Pa, i: usize) -> u8 {
    let w = dev::read64(sched.add(SCHED_LIST + (i as u64 & !7)));
    w.to_le_bytes().get(i & 7).copied().unwrap_or(0)
}

/// Zet ingang `i` op `id`.
fn set(sched: Pa, i: usize, id: u8) {
    let at = sched.add(SCHED_LIST + (i as u64 & !7));
    let mut b = dev::read64(at).to_le_bytes();
    if let Some(x) = b.get_mut(i & 7) {
        *x = id;
    }
    dev::write64(at, u64::from_le_bytes(b));
}

/// De lengte en de lijst naar DRAM, na de schrijfwerken ervoor.
fn publish(sched: Pa) {
    dev::mb();
    dev::push(
        sched.add(SCHED_COUNT),
        (SCHED_LIST + SLOT_CAP as u64 - SCHED_COUNT) as usize,
    );
}

/// De plek van `id` in de lijst van `sched`.
#[must_use]
pub fn find(sched: Pa, id: u8) -> Option<usize> {
    (0..len(sched)).find(|&i| get(sched, i) == id)
}

/// Zet `id` in de lijst van `sched`: staat hij er al, dan niets
/// (`Ok(false)`); anders het eerste gat, anders achteraan. Eerst kijken of
/// hij er al staat, dán pas "vol": andersom weigerde Go een geldige
/// herstart zodra de lijst ooit tot `SLOT_CAP` gegroeid was.
pub fn add(sched: Pa, id: u8) -> Result<bool, Error> {
    let n = len(sched);
    let mut gap = None;
    for i in 0..n {
        match get(sched, i) {
            x if x == id => return Ok(false),
            0 if gap.is_none() => gap = Some(i),
            _ => {}
        }
    }
    match gap {
        Some(i) => set(sched, i, id),
        // `>=` en niet `>`: bij n == SLOT_CAP schreef de append op
        // `SCHED_S2_PA`.
        None if n >= SLOT_CAP => return Err(Error::RosterFull { count: n }),
        None => {
            set(sched, n, id);
            dev::mb();
            dev::write64(sched.add(SCHED_COUNT), n as u64 + 1);
        }
    }
    publish(sched);
    Ok(true)
}

/// Haalt `id` uit de lijst van `sched` (een gat, dat [`add`] hergebruikt).
/// Geeft of hij erin stond.
pub fn remove(sched: Pa, id: u8) -> bool {
    let mut was = false;
    for i in 0..len(sched) {
        if get(sched, i) == id {
            set(sched, i, 0);
            was = true;
        }
    }
    if was {
        publish(sched);
    }
    was
}

/// Maakt de lijst van `sched` leeg, of `[id]` met `Some(id)`: alleen op
/// een core waar geen rotatie meeleest (de mailbox-start, een hart dat uit
/// reset komt).
pub fn reset(sched: Pa, id: Option<u8>) {
    dev::write64(sched.add(SCHED_COUNT), 0);
    dev::mb();
    dev::clear(sched.add(SCHED_LIST), SLOT_CAP);
    if let Some(id) = id {
        set(sched, 0, id);
        dev::mb();
        dev::write64(sched.add(SCHED_COUNT), 1);
    }
    publish(sched);
}

/// De ids in de lijst van `sched`, gaten overgeslagen, in lijstvolgorde.
/// Geeft het aantal.
pub fn each(sched: Pa, mut f: impl FnMut(u8)) -> usize {
    let mut count = 0;
    for i in 0..len(sched) {
        let id = get(sched, i);
        if id != 0 {
            f(id);
            count += 1;
        }
    }
    count
}

/// Een bewoner erbij op de rotatie van `sched`: eerst de lijst, dan de
/// staat boot-pending. De staat is de publicatie: wie hem ziet, ziet de
/// lijst die erbij hoort, en een volle lijst laat niets half achter. Het
/// ctx-blok (`ctx`, id `id`) staat dan al klaar.
pub fn enlist(sched: Pa, ctx: Pa, id: u8) -> Result<(), Error> {
    add(sched, id)?;
    super::ctx_write(ctx, super::layout::CTX_STATE, CtxState::BootPending.raw());
    Ok(())
}

/// Haalt kooi `slot` uit de lijst van ELKE core, de OS-core inbegrepen,
/// vóór zijn nieuwe levensduur. Een gestopt lid blijft als dode byte in de
/// lijst van zijn oude core staan; komt het slot daarna op een andere core,
/// dan zou die oude rotatie zijn verse boot-pending staat zien en hem daar
/// óók starten. Vóór elke staatswissel van het slot, zodat de hercontrole
/// van de rotatie de verwijdering ziet.
pub fn forget(plan: &Plan, slot: Slot) -> Result<(), Error> {
    let id = u8::try_from(slot.get()).map_err(|_| Error::BadContextId { id: 0 })?;
    for c in 0..=plan.app_cores() {
        let core = Core::new(c).ok_or(Error::Plan(abi::Error::OutOfPlan {
            index: c,
            max: SLOT_CAP,
        }))?;
        remove(sched(plan, core)?, id);
    }
    Ok(())
}

/// De context-id's in de lijst van `core` (zie [`each`]).
pub fn residents(plan: &Plan, core: Core, f: impl FnMut(u8)) -> Result<usize, Error> {
    Ok(each(sched(plan, core)?, f))
}

const _: () = assert!(SCHED_LIST.is_multiple_of(8) && SCHED_COUNT < SCHED_LIST);
const _: () = assert!(SCHED_LIST + SLOT_CAP as u64 <= PARK_MBOX_LEN);

#[cfg(test)]
mod tests {
    use super::*;
    use crate::el2::harness::plan;

    fn ids(sched: Pa) -> Vec<u8> {
        (0..len(sched)).map(|i| get(sched, i)).collect()
    }

    #[test]
    fn a_resident_takes_the_first_gap_and_never_twice() {
        let (_b, p) = plan(2);
        let s = sched(&p, Core::new(1).unwrap()).unwrap();
        assert_eq!(add(s, 1), Ok(true));
        assert_eq!(add(s, 2), Ok(true));
        assert_eq!(add(s, 2), Ok(false));
        assert_eq!(ids(s), [1, 2]);
        assert!(remove(s, 1));
        assert!(!remove(s, 1));
        assert_eq!(find(s, 1), None);
        assert_eq!(ids(s), [0, 2]);
        assert_eq!(add(s, 3), Ok(true));
        assert_eq!(ids(s), [3, 2]);
        let mut seen = Vec::new();
        assert_eq!(each(s, |id| seen.push(id)), 2);
        assert_eq!(seen, [3, 2]);
        // Woordgewijs: de buren in hetzelfde woord blijven staan.
        for id in 4..=12 {
            add(s, id).unwrap();
        }
        remove(s, 7);
        assert_eq!(ids(s), [3, 2, 4, 5, 6, 0, 8, 9, 10, 11, 12]);
        reset(s, Some(9));
        assert_eq!(ids(s), [9]);
        assert_eq!(dev::read64(s.add(SCHED_LIST + 8)), 0);
        reset(s, None);
        assert_eq!(ids(s), []);
    }

    #[test]
    fn a_full_list_says_so_but_takes_who_is_already_there() {
        let (_b, p) = plan(2);
        let s = sched(&p, Core::new(0).unwrap()).unwrap();
        dev::write64(s.add(SCHED_COUNT), SLOT_CAP as u64);
        for i in 0..SLOT_CAP {
            set(s, i, 0xEE);
        }
        assert_eq!(add(s, 1), Err(Error::RosterFull { count: SLOT_CAP }));
        assert_eq!(add(s, 0xEE), Ok(false));
        // Een lengte voorbij de lijst wordt geklemd: de append schrijft
        // nooit op `SCHED_S2_PA`.
        dev::write64(s.add(SCHED_COUNT), u64::MAX);
        assert_eq!(len(s), SLOT_CAP);
    }

    #[test]
    fn forget_clears_every_core_the_os_core_too() {
        let (_b, p) = plan(2);
        for c in 0..=2 {
            add(sched(&p, Core::new(c).unwrap()).unwrap(), 2).unwrap();
        }
        forget(&p, Slot::new(2).unwrap()).unwrap();
        for c in 0..=2 {
            assert_eq!(
                residents(&p, Core::new(c).unwrap(), |_| {}),
                Ok(0),
                "core {c}"
            );
        }
    }

    #[test]
    fn enlist_publishes_the_state_after_the_list() {
        let (_b, p) = plan(2);
        let s = sched(&p, Core::new(1).unwrap()).unwrap();
        let ctx = p.ctx_pa(Slot::new(1).unwrap()).unwrap();
        enlist(s, ctx, 1).unwrap();
        assert_eq!(super::super::ctx_state(ctx), Some(CtxState::BootPending));
        assert_eq!(find(s, 1), Some(0));
        // Vol: geen staat zonder lijst.
        let ctx2 = p.ctx_pa(Slot::new(2).unwrap()).unwrap();
        dev::write64(s.add(SCHED_COUNT), SLOT_CAP as u64);
        for i in 0..SLOT_CAP {
            set(s, i, 0xEE);
        }
        assert!(enlist(s, ctx2, 2).is_err());
        assert_eq!(super::super::ctx_state(ctx2), Some(CtxState::Empty));
    }
}
