//! De SMP- en sharegroup-paden van de lifecycle-actor, over de nep-kooi van
//! [`super::tests`]: het startschot van elke secundaire, de stop over de
//! hele span (E9: pas vrij na de bevestiging van élke core), quarantaine
//! als één core niet bevestigt, en leden van een sharegroup die hun core
//! delen en alleen stoppen.

use super::tests::{Actor, FakeConsole, Obey, actor, s, start, stop};
use super::*;
use crate::pool::GroupName;
use crate::testutil::block_on;

fn group(name: &str) -> Placement {
    let mut g = GroupName::new();
    for b in name.bytes() {
        g.push(b).unwrap();
    }
    Placement {
        group: Some(g),
        pool_cores: 1,
        cores: 1,
        class: None,
    }
}

/// Een lid van sharegroup `name`, zoals Hop hem start.
fn start_member(a: &mut Actor<'_>, slot: usize, name: &str) -> Result {
    let g = block_on(a.claim(StartSpec::new(s(slot), 8 << 20, group(name))))?;
    block_on(a.arm(g, 0x4001_0000))?;
    a.svc.ctl(s(slot)).unwrap().gone.set();
    Ok(())
}

// De app vraagt zijn cores één voor één (CTRL_SMP_REQ = slot + k, zoals
// Go's `task`); de actor dispatcht elke secundaire op de fysieke core van
// zijn eigen span, en beantwoordt elk verzoek.
#[test]
fn every_secondary_of_the_span_is_dispatched_in_the_cage() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 6);
    start(&mut a, 2, 8, 1).unwrap(); // Core 1.
    start(&mut a, 4, 8, 3).unwrap(); // Cores 2..4.
    assert_eq!(a.status(s(4)).core.map(|(c, n)| (c.get(), n)), Some((2, 3)));
    for vcpu in [5, 6] {
        a.cage.smp_req[4] = vcpu;
        block_on(a.handle(Request::Smp(s(4))));
        assert_eq!(a.cage.smp_req[4], 0, "request {vcpu} left unanswered");
    }
    assert_eq!(a.cage.secondaries, [(4, 3), (4, 4)]);
    assert!(con.saw("slot 4: SMP core 3 dispatched HOPOS_SMP_DISPATCH_OK"));
    assert!(con.saw("slot 4: SMP core 4 dispatched HOPOS_SMP_DISPATCH_OK"));
    // De primaire zelf is geen secundaire, en de buurman nooit.
    a.cage.smp_req[4] = 4;
    a.smp(s(4));
    a.cage.smp_req[4] = 7;
    a.smp(s(4));
    assert_eq!(a.cage.secondaries.len(), 2);
    assert!(con.saw("HOPOS_SMP_REJECT"));
}

// E9: de stop vraagt élke core van de span, en geeft pas vrij als ze
// allemaal stil zijn.
#[test]
fn stop_confirms_every_core_of_the_span() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    start(&mut a, 1, 16, 3).unwrap();
    for vcpu in [2, 3] {
        a.cage.smp_req[1] = vcpu;
        a.smp(s(1));
    }
    stop(&mut a, 1).unwrap();
    let asked = a.cage.asked_quiet.borrow().clone();
    for c in 1..=3 {
        assert!(asked.contains(&(1, c)), "stop never asked core {c}");
    }
    assert!(
        !asked.iter().any(|(_, c)| *c == 4),
        "stop touched a foreign core"
    );
    assert_eq!(a.status(s(1)).occupancy, Occupancy::Empty);
    // De hele span is terug: een nieuwe app van drie cores past.
    start(&mut a, 2, 16, 3).unwrap();
}

// Eén secundaire bevestigt niet, ook niet na de intrekking: quarantaine.
// De partitie en ELKE core van de span blijven van de eigenaar, een nieuw
// SMP-verzoek wordt niet beantwoord, en pas een latere stop die alle cores
// stil ziet, geeft vrij.
#[test]
fn one_unconfirmed_core_quarantines_the_whole_owner() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    start(&mut a, 1, 16, 3).unwrap();
    a.cage.smp_req[1] = 3;
    a.smp(s(1));
    a.cage.stuck[3] = true;
    assert!(matches!(
        stop(&mut a, 1),
        Err(Error::NotStopped { slot: 1, .. })
    ));
    assert!(a.cage.revoked[1], "no hard kill before quarantine");
    assert_eq!(&a.cores.kicks[1..=4], &[1, 1, 1, 0]);
    assert_eq!(a.status(s(1)).occupancy, Occupancy::Quarantined);
    assert!(a.parts.is_quarantined(s(1)));
    assert!(con.saw("HOPOS_PART_QUARANTINE"));
    // Geen core van de span komt vrij, ook de stille niet.
    for c in 1..=3 {
        assert!(!a.places.core_free(c), "core {c} was released early");
    }
    assert!(start(&mut a, 2, 8, 1).is_ok(), "the fourth core is free");
    assert!(start(&mut a, 3, 8, 1).is_err(), "a span core was reused");
    // In quarantaine blijft een SMP-verzoek liggen: geen nieuwe core erbij.
    a.cage.smp_req[1] = 2;
    a.smp(s(1));
    assert_eq!(a.cage.secondaries, [(1, 3)]);
    assert_eq!(a.cage.smp_req[1], 2);
    // De core bevestigt alsnog: de tweede stop ruimt alles op.
    a.cage.stuck[3] = false;
    stop(&mut a, 1).unwrap();
    assert!(!a.parts.is_quarantined(s(1)));
    for c in 1..=3 {
        assert!(a.places.core_free(c), "core {c} stayed claimed");
    }
}

// Een startschot van een secundaire met een onbekende uitkomst: de core kan
// alsnog aangaan, dus de eigenaar gaat in quarantaine en houdt alles.
#[test]
fn a_failed_secondary_dispatch_quarantines_the_owner() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    start(&mut a, 1, 16, 2).unwrap();
    a.cage.fail_secondary = true;
    a.cage.smp_req[1] = 2;
    a.smp(s(1));
    assert!(con.saw("HOPOS_SMP_DISPATCH_FAIL slot 1 core 2"));
    assert_eq!(a.status(s(1)).occupancy, Occupancy::Quarantined);
    assert_eq!(a.cage.smp_req[1], 0, "the app kept spinning on its request");
}

// Sharegroups op app-cores: leden delen de core van de groep, houden hun
// eigen partitie, en de stop van één lid laat de rest en de
// groepsreservering staan.
#[test]
fn members_share_the_group_core_and_stop_alone() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 3);
    start_member(&mut a, 2, "demo").unwrap();
    start_member(&mut a, 3, "demo").unwrap();
    let (c2, c3) = (a.status(s(2)).core, a.status(s(3)).core);
    assert_eq!(c2, c3, "members did not share the group core");
    let core = c2.unwrap().0;
    // Elk lid zijn eigen kooi en partitie, op dezelfde core.
    assert_eq!(a.cage.dispatched, [(2, core.get()), (3, core.get())]);
    let (p2, p3) = (a.parts.partition_of(s(2)), a.parts.partition_of(s(3)));
    assert!(p2.is_some() && p3.is_some() && p2 != p3);
    // Een dedicated job komt nooit op de groepscore.
    start(&mut a, 4, 8, 1).unwrap();
    assert_ne!(a.status(s(4)).core.unwrap().0, core);

    stop(&mut a, 2).unwrap();
    assert_eq!(a.status(s(2)).occupancy, Occupancy::Empty);
    assert_eq!(a.status(s(3)).occupancy, Occupancy::Running);
    assert!(!a.cage.exit_asked[3], "the neighbour was asked to stop");
    assert!(!a.places.core_free(core.get()), "the group lost its core");
    // Het nieuwe lid komt weer bij de groep.
    start_member(&mut a, 5, "demo").unwrap();
    assert_eq!(a.status(s(5)).core, c3);
    stop(&mut a, 3).unwrap();
    stop(&mut a, 5).unwrap();
    assert!(
        a.places.core_free(core.get()),
        "the last member kept the pool"
    );
}

// share.go `bootPendingDispatch`: een buur die nooit yieldt, houdt de
// gedeelde core vast. Na RECLAIM_WAIT wordt hij geofferd (zijn kooi
// ingetrokken), en dan krijgt het nieuwe lid zijn beurt.
#[test]
fn a_neighbour_that_never_yields_is_sacrificed_for_the_new_member() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 2);
    start_member(&mut a, 2, "demo").unwrap();
    a.cage.hog = Some(2);
    a.cage.boot_pending[3] = true;
    start_member(&mut a, 3, "demo").unwrap();
    assert!(a.cage.revoked[2], "the hog kept the core");
    assert!(!a.cage.revoked[3]);
    assert!(con.saw(
        "slot 3: core 1 never yielded in 2 s, sacrificing resident slot 2 HOPOS_CORE_RECLAIM"
    ));
    assert_eq!(a.status(s(3)).occupancy, Occupancy::Running);
}

// Niets op te offeren (de core zegt niet wie hem houdt): de start faalt
// zoals een dispatchfout, met de partitie in quarantaine, in plaats van
// een start die gelukt heet en nooit draait.
#[test]
fn a_member_that_never_gets_a_turn_is_a_dispatch_failure() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 2);
    start_member(&mut a, 2, "demo").unwrap();
    a.cage.boot_pending[3] = true;
    let e = start_member(&mut a, 3, "demo").unwrap_err();
    assert_eq!(e, Error::Dispatch { slot: 3, core: 1 });
    assert!(con.saw("HOPOS_CORE_RECLAIM_FAILED"));
    assert!(!a.cage.revoked[2]);
    assert_eq!(a.status(s(3)).occupancy, Occupancy::Quarantined);
}

// Hop is nooit het slachtoffer: zonder Hop herstart niemand iets.
#[test]
fn hop_is_never_sacrificed() {
    let (svc, con) = (Servicers::new(), FakeConsole::default());
    let mut a = actor(&svc, &con, Obey::Exit, 64, 2);
    start_member(&mut a, 2, "hop").unwrap();
    a.cage.hog = Some(2);
    a.cage.boot_pending[3] = true;
    assert!(start_member(&mut a, 3, "hop").is_err());
    assert!(!a.cage.revoked[2], "Hop was sacrificed");
    assert!(con.saw("HOPOS_CORE_RECLAIM_FAILED"));
}
