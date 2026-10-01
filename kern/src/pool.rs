//! De core-plaatsing: welke fysieke core(s) een kooi krijgt.
//!
//! Zonder sharegroup krijgt een kooi een eigen, dedicated core-run (één core,
//! of `cores` aaneengesloten voor een SMP-app). Met een sharegroup krijgt de
//! groep een vaste set hele cores en balanceert de plaatsing de leden
//! daarover. Kooi is geen core: er passen meer kooien dan cores, tot de
//! partitie-pool of [`crate::SLOT_CAP`] op is.
//!
//! Een jobspec vraagt een klasse ([`CoreClass`]); de plaatsing kiest alleen
//! cores met die klasse ([`Cores::class`]). Hop-de-bewoner vraagt
//! [`Placement::hop`]: sharegroup `hop`, één core, en die core deelt hij met
//! wie zich in dezelfde groep meldt (PORT.md beslissing 1).
//!
//! De OS-core ([`Core::OS`], PORT.md beslissing 2, 30-09): de kern deelt
//! zijn eigen core met de groepen die hij daarvoor aanwijst
//! ([`CorePool::share_os_core`]): Hop, en vertrouwde apps. Zo past het hele
//! OS inclusief Hop op één core en houdt een board met twee cores een volle
//! app-core over. Een groep die de OS-core niet mag delen, komt er nooit;
//! een dedicated plaatsing ook niet.
//!
//! Puur boekhouding, geen MMIO. Eigendom van de lifecycle-actor.

use crate::cage::{CoreClass, Cores};
use crate::{CORE_CAP, Core, Error, Result, SLOT_CAP, Slot};
use bounded::BoundedVec;

/// De langste sharegroup-naam (dezelfde grens als het handoff-blob draagt).
pub const MAX_GROUP_NAME: usize = 256;
/// Het maximale aantal sharegroups tegelijk.
pub const MAX_GROUPS: usize = 32;
/// Het maximale aantal cores in één sharegroup.
pub const MAX_GROUP_CORES: usize = 64;
/// Hoeveel sharegroups tegelijk de OS-core mogen delen: Hop en een handvol
/// vertrouwde groepen.
pub const MAX_OS_GROUPS: usize = 4;

/// De naam van een sharegroup.
pub type GroupName = BoundedVec<u8, MAX_GROUP_NAME>;

/// De vraag van een jobspec aan de plaatsing.
#[derive(Clone, Debug, Default)]
pub struct Placement {
    /// De sharegroup, of `None` voor een dedicated run.
    pub group: Option<GroupName>,
    /// De poolgrootte van de groep in hele cores (0 telt als 1).
    pub pool_cores: usize,
    /// Het eigen core-aantal van de app (1 = gewoon, meer = SMP).
    pub cores: usize,
    /// De gevraagde klasse, of `None` voor elke klasse.
    pub class: Option<CoreClass>,
}

/// De naam van Hops eigen sharegroup.
pub const HOP_GROUP: &[u8] = b"hop";

impl Placement {
    /// De plaatsing van Hop-de-bewoner: één core, gedeeld in de groep `hop`.
    /// Mag de groep de OS-core delen ([`CorePool::share_os_core`]), dan is
    /// het die; anders een app-core, zoals vóór 30-09.
    ///
    /// Geen klasse meer: Hop woont waar de kern woont, en welke klasse dát
    /// is, kiest de bootparameter van de OS-core (`hopos.oscore`), niet de
    /// jobspec.
    pub fn hop() -> Result<Placement> {
        let mut name = GroupName::new();
        for b in HOP_GROUP {
            name.push(*b).map_err(|_| Error::TooLarge {
                len: HOP_GROUP.len(),
                max: MAX_GROUP_NAME,
            })?;
        }
        Ok(Placement {
            group: Some(name),
            pool_cores: 1,
            cores: 1,
            class: None,
        })
    }
}

#[derive(Clone, Debug)]
struct Group {
    name: GroupName,
    cores: BoundedVec<Core, MAX_GROUP_CORES>,
}

#[derive(Copy, Clone, Debug)]
struct CagePlace {
    primary: Core,
    span: usize,
    group: Option<usize>,
}

/// De plaatsing van alle kooien op de app-cores en de OS-core.
pub struct CorePool {
    hop_reserved: usize,
    os_groups: BoundedVec<GroupName, MAX_OS_GROUPS>,
    groups: [Option<Group>; MAX_GROUPS],
    core_group: [Option<u8>; CORE_CAP + 1],
    core_apps: [u16; CORE_CAP + 1],
    /// Cores die een startschot weigerden: tot een koude boot geen plaatsing
    /// meer, anders landt elke herstart van Hop op dezelfde dode core.
    retired: [bool; CORE_CAP + 1],
    cages: [Option<CagePlace>; SLOT_CAP + 1],
}

impl CorePool {
    /// Een lege plaatsing. App-cores zijn `hop_reserved + 1 ..= app_cores`:
    /// de eerste `hop_reserved` cores draaien de HOP-runtime.
    #[must_use]
    pub fn new(hop_reserved: usize) -> CorePool {
        CorePool {
            hop_reserved,
            os_groups: BoundedVec::new(),
            groups: [const { None }; MAX_GROUPS],
            core_group: [None; CORE_CAP + 1],
            core_apps: [0; CORE_CAP + 1],
            retired: [false; CORE_CAP + 1],
            cages: [None; SLOT_CAP + 1],
        }
    }

    /// Haalt core `c` uit de plaatsing tot een koude boot: hij weigerde een
    /// startschot en liep nooit.
    pub fn retire(&mut self, c: Core) {
        if let Some(r) = self.retired.get_mut(c.get()) {
            *r = true;
        }
    }

    /// Laat sharegroup `name` de OS-core delen: vanaf nu plaatst elke kooi
    /// van die groep op [`Core::OS`]. Beleid van de kern bij boot (Hop, en de
    /// groepen die de config vertrouwt); alleen op een board waar de kern
    /// zijn core kan delen (de rotatie van `cpu::el2`).
    pub fn share_os_core(&mut self, name: &[u8]) -> Result {
        if self.os_group(name) {
            return Ok(());
        }
        let mut g = GroupName::new();
        for b in name {
            g.push(*b).map_err(|_| Error::TooLarge {
                len: name.len(),
                max: MAX_GROUP_NAME,
            })?;
        }
        self.os_groups
            .push(g)
            .map_err(|_| Error::Full { cap: MAX_OS_GROUPS })
    }

    /// Mag groep `name` de OS-core delen?
    #[must_use]
    pub fn os_group(&self, name: &[u8]) -> bool {
        self.os_groups.iter().any(|g| g.as_slice() == name)
    }

    fn is_app_core(&self, cores: &impl Cores, c: usize) -> bool {
        c > self.hop_reserved && c <= cores.app_cores().min(CORE_CAP)
    }

    fn class_ok(cores: &impl Cores, c: usize, class: Option<CoreClass>) -> bool {
        match (class, Core::new(c)) {
            (None, _) => true,
            (Some(want), Some(core)) => cores.class(core) == Some(want),
            (Some(_), None) => false,
        }
    }

    /// Een kooi van een groep die de OS-core deelt: altijd [`Core::OS`], één
    /// core, en een gevraagde klasse moet die van de OS-core zijn.
    fn place_os(
        &mut self,
        cores: &impl Cores,
        slot: Slot,
        name: &GroupName,
        spec: &Placement,
    ) -> Result<Core> {
        if spec.cores.max(1) > 1 || spec.pool_cores.max(1) > 1 {
            return Err(Error::PoolSize {
                have: 1,
                want: spec.cores.max(spec.pool_cores),
            });
        }
        if let Some(want) = spec.class
            && cores.class(Core::OS) != Some(want)
        {
            return Err(Error::ClassMismatch { core: 0 });
        }
        let gid = match self.group_id(name) {
            Some(g) => g,
            None => {
                let gid = self
                    .groups
                    .iter()
                    .position(Option::is_none)
                    .ok_or(Error::Full { cap: MAX_GROUPS })?;
                let mut members = BoundedVec::new();
                members.push(Core::OS).map_err(|_| Error::Full {
                    cap: MAX_GROUP_CORES,
                })?;
                if let Some(g) = self.groups.get_mut(gid) {
                    *g = Some(Group {
                        name: name.clone(),
                        cores: members,
                    });
                }
                gid
            }
        };
        Ok(self.reserve(slot, Core::OS, 1, Some(gid)))
    }

    /// Een app-core zonder groepsclaim, zonder levende kooi en niet
    /// uitgeschakeld ([`Self::retire`]).
    #[must_use]
    pub fn core_free(&self, c: usize) -> bool {
        self.core_group.get(c).is_some_and(Option::is_none)
            && self.core_apps.get(c).is_some_and(|n| *n == 0)
            && self.retired.get(c).is_some_and(|r| !r)
    }

    /// Staan er `n` opeenvolgende vrije app-cores vanaf `primary`?
    pub fn run_free(
        &self,
        cores: &impl Cores,
        primary: usize,
        n: usize,
        class: Option<CoreClass>,
    ) -> bool {
        (primary..primary + n).all(|c| {
            self.is_app_core(cores, c) && self.core_free(c) && Self::class_ok(cores, c, class)
        })
    }

    /// Boekt de HELE core-run van een kooi. Alle cores van een SMP-app gaan
    /// erin, niet alleen de primaire: gemeten 05-09 op de M4 landde een
    /// 1-core app op de tweede core van een SMP-buur en deed 3144 µs per
    /// system call in plaats van 23. Geen fout, geen logregel, 137x trager.
    fn reserve(&mut self, slot: Slot, primary: Core, span: usize, group: Option<usize>) -> Core {
        for c in primary.get()..primary.get() + span {
            if let Some(n) = self.core_apps.get_mut(c) {
                *n = n.saturating_add(1);
            }
        }
        if let Some(p) = self.cages.get_mut(slot.get()) {
            *p = Some(CagePlace {
                primary,
                span,
                group,
            });
        }
        primary
    }

    /// Kiest de fysieke core(s) voor `slot`. Idempotent per kooi: een tweede
    /// aanvraag geeft dezelfde core terug.
    pub fn place(&mut self, cores: &impl Cores, slot: Slot, spec: &Placement) -> Result<Core> {
        if let Some(Some(p)) = self.cages.get(slot.get()) {
            return Ok(p.primary);
        }
        let pool_cores = spec.pool_cores.max(1);
        let n = spec.cores.max(1);
        let Some(name) = &spec.group else {
            return self.place_dedicated(cores, slot, n, spec.class);
        };
        if self.os_group(name.as_slice()) {
            return self.place_os(cores, slot, name, spec);
        }
        if n > 1 {
            // Een gedeelde kooi draait per definitie op één core: de pool ís
            // het deelmechanisme.
            return Err(Error::PoolSize { have: 1, want: n });
        }
        let gid = match self.group_id(name) {
            Some(g) => g,
            None => self.new_group(cores, name, pool_cores, spec.class)?,
        };
        let group = self
            .groups
            .get(gid)
            .and_then(Option::as_ref)
            .ok_or(Error::Full { cap: MAX_GROUPS })?;
        // De poolgrootte van een bestaande groep is NIET "first wins": het was
        // dat stil, en de tweede job kreeg de helft van zijn hart-budget.
        if group.cores.len() != pool_cores {
            return Err(Error::PoolSize {
                have: group.cores.len(),
                want: pool_cores,
            });
        }
        // Bestaande pools houden hun fysieke cores, ook na adoptie.
        if let Some(bad) = group
            .cores
            .iter()
            .find(|c| !Self::class_ok(cores, c.get(), spec.class))
        {
            return Err(Error::ClassMismatch { core: bad.get() });
        }
        let mut best: Option<Core> = None;
        for c in group.cores.iter() {
            let load = |x: Core| self.core_apps.get(x.get()).copied().unwrap_or(u16::MAX);
            if best.is_none_or(|b| load(*c) < load(b)) {
                best = Some(*c);
            }
        }
        let core = best.ok_or(Error::NoCores { cores: 1 })?;
        Ok(self.reserve(slot, core, 1, Some(gid)))
    }

    fn group_id(&self, name: &GroupName) -> Option<usize> {
        self.groups.iter().position(|g| {
            g.as_ref()
                .is_some_and(|g| g.name.as_slice() == name.as_slice())
        })
    }

    fn new_group(
        &mut self,
        cores: &impl Cores,
        name: &GroupName,
        pool_cores: usize,
        class: Option<CoreClass>,
    ) -> Result<usize> {
        let gid = self
            .groups
            .iter()
            .position(Option::is_none)
            .ok_or(Error::Full { cap: MAX_GROUPS })?;
        let mut members = BoundedVec::new();
        for c in self.hop_reserved + 1..=cores.app_cores().min(CORE_CAP) {
            if members.len() == pool_cores {
                break;
            }
            if self.core_free(c)
                && Self::class_ok(cores, c, class)
                && let Some(core) = Core::new(c)
            {
                members.push(core).map_err(|_| Error::Full {
                    cap: MAX_GROUP_CORES,
                })?;
            }
        }
        if members.len() < pool_cores {
            return Err(Error::NoCores { cores: pool_cores });
        }
        for c in members.iter() {
            if let Some(g) = self.core_group.get_mut(c.get()) {
                *g = u8::try_from(gid).ok();
            }
        }
        if let Some(g) = self.groups.get_mut(gid) {
            *g = Some(Group {
                name: name.clone(),
                cores: members,
            });
        }
        Ok(gid)
    }

    /// De eerste vrije fysieke run, los van het kooinummer.
    fn place_dedicated(
        &mut self,
        cores: &impl Cores,
        slot: Slot,
        n: usize,
        class: Option<CoreClass>,
    ) -> Result<Core> {
        for c in self.hop_reserved + 1..=cores.app_cores().min(CORE_CAP) {
            if self.run_free(cores, c, n, class)
                && let Some(core) = Core::new(c)
            {
                return Ok(self.reserve(slot, core, n, None));
            }
        }
        Err(Error::NoCores { cores: n })
    }

    /// Geeft de core(s) van een gestopte kooi terug. Een pool-core blijft van
    /// de groep tot zijn laatste kooi weg is; dan komt de hele pool vrij.
    pub fn release(&mut self, slot: Slot) {
        let Some(p) = self.cages.get_mut(slot.get()).and_then(Option::take) else {
            return;
        };
        for c in p.primary.get()..p.primary.get() + p.span.max(1) {
            if let Some(n) = self.core_apps.get_mut(c) {
                *n = n.saturating_sub(1);
            }
        }
        let Some(gid) = p.group else { return };
        let empty = self
            .groups
            .get(gid)
            .and_then(Option::as_ref)
            .is_some_and(|g| {
                g.cores
                    .iter()
                    .all(|c| self.core_apps.get(c.get()).is_none_or(|n| *n == 0))
            });
        if empty && let Some(g) = self.groups.get_mut(gid).and_then(Option::take) {
            for c in g.cores.iter() {
                if let Some(x) = self.core_group.get_mut(c.get()) {
                    *x = None;
                }
            }
        }
    }

    /// De primaire core en de span van `slot`.
    #[must_use]
    pub fn placement_of(&self, slot: Slot) -> Option<(Core, usize)> {
        self.cages
            .get(slot.get())
            .copied()
            .flatten()
            .map(|p| (p.primary, p.span))
    }

    /// De naam en de VOLLEDIGE pool van de groep van `slot`, ook cores
    /// zonder bewoner (voor de flip).
    #[must_use]
    pub fn group_of(&self, slot: Slot) -> Option<(&GroupName, &[Core])> {
        let gid = self.cages.get(slot.get()).copied().flatten()?.group?;
        let g = self.groups.get(gid)?.as_ref()?;
        Some((&g.name, g.cores.as_slice()))
    }

    /// Herstelt een plaatsing die de adoptie al toetste (E8).
    pub fn adopt(
        &mut self,
        slot: Slot,
        primary: Core,
        span: usize,
        group: Option<(&GroupName, &[Core])>,
    ) -> Result {
        let gid = match group {
            None => None,
            Some((name, members)) => Some(match self.group_id(name) {
                Some(g) => g,
                None => {
                    let gid = self
                        .groups
                        .iter()
                        .position(Option::is_none)
                        .ok_or(Error::Full { cap: MAX_GROUPS })?;
                    let mut cores = BoundedVec::new();
                    for c in members {
                        cores.push(*c).map_err(|_| Error::Full {
                            cap: MAX_GROUP_CORES,
                        })?;
                        if let Some(x) = self.core_group.get_mut(c.get()) {
                            *x = u8::try_from(gid).ok();
                        }
                    }
                    if let Some(g) = self.groups.get_mut(gid) {
                        *g = Some(Group {
                            name: name.clone(),
                            cores,
                        });
                    }
                    gid
                }
            }),
        };
        self.reserve(slot, primary, span, gid);
        Ok(())
    }

    /// Het aantal cores dat de HOP-runtime zelf houdt.
    #[must_use]
    pub fn hop_reserved(&self) -> usize {
        self.hop_reserved
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::cage::Power;

    /// Een nep-board: `n` app-cores met optionele klassen.
    pub(crate) struct FakeCores {
        pub(crate) n: usize,
        pub(crate) classes: [Option<CoreClass>; 16],
        pub(crate) power: [Power; 16],
        pub(crate) kicks: [u32; 16],
    }

    impl FakeCores {
        pub(crate) fn new(n: usize) -> FakeCores {
            FakeCores {
                n,
                classes: [None; 16],
                power: [Power::Off; 16],
                kicks: [0; 16],
            }
        }
        pub(crate) fn with(n: usize, classes: &[(usize, CoreClass)]) -> FakeCores {
            let mut f = FakeCores::new(n);
            for (c, k) in classes {
                f.classes[*c] = Some(*k);
            }
            f
        }
    }

    impl Cores for FakeCores {
        fn app_cores(&self) -> usize {
            self.n
        }
        fn phys(&self, core: Core) -> Option<u32> {
            (core.get() <= self.n).then_some(core.get() as u32)
        }
        fn class(&self, core: Core) -> Option<CoreClass> {
            self.classes.get(core.get()).copied().flatten()
        }
        fn power(&self, core: Core) -> Power {
            self.power[core.get()]
        }
        fn kick(&mut self, core: Core) {
            self.kicks[core.get()] += 1;
        }
    }

    fn s(i: usize) -> Slot {
        Slot::new(i).unwrap()
    }

    fn name(n: &str) -> GroupName {
        let mut g = GroupName::new();
        for b in n.bytes() {
            g.push(b).unwrap();
        }
        g
    }

    fn ded(cores: usize, class: Option<CoreClass>) -> Placement {
        Placement {
            group: None,
            pool_cores: 1,
            cores,
            class,
        }
    }

    fn shared(g: &str, pool: usize, class: Option<CoreClass>) -> Placement {
        Placement {
            group: Some(name(g)),
            pool_cores: pool,
            cores: 1,
            class,
        }
    }

    #[test]
    fn pool_dedicated_eigen_core() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        let mut seen = [false; 5];
        for cage in 1..=4 {
            let c = p.place(&b, s(cage), &ded(1, None)).unwrap().get();
            assert!((1..=4).contains(&c) && !seen[c]);
            seen[c] = true;
        }
        // Geen stille terugval naar delen: dat is een vertrouwensbeslissing.
        assert!(p.place(&b, s(5), &ded(1, None)).is_err());
    }

    #[test]
    fn pool_sharegroup_balanceert() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        let mut got = [0; 5];
        for cage in 4..=7 {
            got[p.place(&b, s(cage), &shared("web", 2, None)).unwrap().get()] += 1;
        }
        assert_eq!(got.iter().filter(|n| **n > 0).count(), 2);
        assert!(got.iter().all(|n| *n == 0 || *n == 2));
        p.place(&b, s(8), &shared("db", 2, None)).unwrap();
        assert!(p.place(&b, s(9), &shared("cache", 1, None)).is_err());
    }

    #[test]
    fn pool_release_geeft_pool_terug() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        for cage in 4..=7 {
            p.place(&b, s(cage), &shared("web", 2, None)).unwrap();
        }
        p.release(s(4));
        p.release(s(5));
        p.release(s(6));
        assert!(p.place(&b, s(10), &shared("other", 3, None)).is_err());
        p.release(s(7));
        p.place(&b, s(11), &shared("other", 4, None)).unwrap();
    }

    #[test]
    fn pool_grootte_mismatch_wordt_geweigerd() {
        let b = FakeCores::new(6);
        let mut p = CorePool::new(0);
        p.place(&b, s(4), &shared("web", 2, None)).unwrap();
        assert!(matches!(
            p.place(&b, s(5), &shared("web", 4, None)),
            Err(Error::PoolSize { have: 2, want: 4 })
        ));
        p.place(&b, s(6), &shared("web", 2, None)).unwrap();
        p.place(&b, s(7), &shared("solo", 0, None)).unwrap();
        p.place(&b, s(8), &shared("solo", 1, None)).unwrap();
    }

    #[test]
    fn place_cage_idempotent() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        let c1 = p.place(&b, s(4), &shared("web", 2, None)).unwrap();
        let c2 = p.place(&b, s(4), &shared("web", 2, None)).unwrap();
        assert_eq!(c1, c2);
    }

    #[test]
    fn pool_respecteert_hop_reserved() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(1);
        for cage in 1..=3 {
            assert!(p.place(&b, s(cage), &ded(1, None)).unwrap().get() >= 2);
        }
        assert!(p.place(&b, s(4), &ded(1, None)).is_err());
    }

    #[test]
    fn pool_smp_reserves_independent_core_run() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        assert_eq!(p.place(&b, s(8), &ded(2, None)).unwrap().get(), 1);
        assert_eq!(p.place(&b, s(2), &ded(1, None)).unwrap().get(), 3);
        p.release(s(8));
        assert!(p.core_free(1) && p.core_free(2) && !p.core_free(3));
    }

    #[test]
    fn pool_smp_finds_run_after_shared_residents() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        for cage in 1..=6 {
            let c = p.place(&b, s(cage), &shared("trusted", 1, None)).unwrap();
            assert_eq!(c.get(), 1);
        }
        assert_eq!(p.place(&b, s(7), &ded(2, None)).unwrap().get(), 2);
        assert!(p.place(&b, s(8), &ded(2, None)).is_err());
        p.release(s(7));
        assert_eq!(p.place(&b, s(7), &ded(3, None)).unwrap().get(), 2);
    }

    #[test]
    fn pool_sharegroup_met_smp_wordt_geweigerd() {
        let b = FakeCores::new(4);
        let mut p = CorePool::new(0);
        let mut spec = shared("web", 2, None);
        spec.cores = 2;
        assert!(matches!(
            p.place(&b, s(1), &spec),
            Err(Error::PoolSize { .. })
        ));
    }

    #[test]
    fn pool_class_sharing_and_smp() {
        use CoreClass::Big;
        let b = FakeCores::with(9, &[(7, Big), (8, Big), (9, Big)]);
        let mut p = CorePool::new(0);
        for cage in [10, 11] {
            assert_eq!(
                p.place(&b, s(cage), &shared("trusted", 1, Some(Big)))
                    .unwrap()
                    .get(),
                7
            );
        }
        assert!(!p.run_free(&b, 7, 2, None) && p.run_free(&b, 8, 2, None));
        assert_eq!(p.place(&b, s(8), &ded(2, Some(Big))).unwrap().get(), 8);
        p.release(s(10));
        assert!(!p.run_free(&b, 7, 1, None), "live neighbor lost pool");
        p.release(s(11));
        assert!(
            p.run_free(&b, 7, 1, None),
            "last neighbor did not release pool"
        );
    }

    #[test]
    fn pool_one_core_class_sharing() {
        let b = FakeCores::with(1, &[(1, CoreClass::Big)]);
        let mut p = CorePool::new(0);
        for cage in [2, 3] {
            let c = p.place(&b, s(cage), &shared("trusted", 1, Some(CoreClass::Big)));
            assert_eq!(c.unwrap().get(), 1);
        }
    }

    #[test]
    fn pool_class_mismatch_and_dedicated_fallback() {
        use CoreClass::{Big, Small};
        let b = FakeCores::with(3, &[(1, Small), (2, Big), (3, Big)]);
        let mut p = CorePool::new(0);
        p.place(&b, s(4), &shared("mixed", 1, None)).unwrap();
        assert!(matches!(
            p.place(&b, s(5), &shared("mixed", 1, Some(Big))),
            Err(Error::ClassMismatch { .. })
        ));
        p.release(s(4));
        assert_eq!(p.place(&b, s(4), &ded(1, Some(Big))).unwrap().get(), 2);
        assert_eq!(p.place(&b, s(5), &ded(1, Some(Big))).unwrap().get(), 3);
        assert!(
            p.place(&b, s(6), &ded(1, Some(Big))).is_err(),
            "fell back to small core"
        );
    }

    // PORT.md beslissing 2 (30-09): Hop deelt de OS-core met de kern, en een
    // vertrouwde groep mag erbij; een dedicated job en een gewone groep
    // komen er nooit.
    #[test]
    fn hop_and_trusted_groups_share_the_os_core() {
        let b = FakeCores::new(1);
        let mut p = CorePool::new(0);
        p.share_os_core(HOP_GROUP).unwrap();
        p.share_os_core(b"trusted").unwrap();
        p.share_os_core(HOP_GROUP).unwrap(); // idempotent
        let hop = p.place(&b, s(1), &Placement::hop().unwrap()).unwrap();
        assert_eq!(hop, Core::OS, "Hop did not land on the OS core");
        assert_eq!(
            p.place(&b, s(2), &shared("trusted", 1, None)).unwrap(),
            Core::OS
        );
        // De enige app-core blijft vrij voor een dedicated job.
        assert_eq!(p.place(&b, s(3), &ded(1, None)).unwrap().get(), 1);
        assert!(p.place(&b, s(4), &ded(1, None)).is_err());
        // Een gewone groep vindt geen app-core meer, en neemt nooit de OS-core.
        assert!(p.place(&b, s(5), &shared("web", 1, None)).is_err());
        // Een OS-groep is één core, geen SMP.
        let mut smp = shared("trusted", 1, None);
        smp.cores = 2;
        assert!(p.place(&b, s(6), &smp).is_err());
        assert_eq!(p.placement_of(s(1)), Some((Core::OS, 1)));
        p.release(s(1));
        p.release(s(2));
        assert_eq!(
            p.place(&b, s(1), &Placement::hop().unwrap()).unwrap(),
            Core::OS
        );
    }

    // Zonder OS-core-deling (een board zonder de rotatie van cpu::el2) is Hop
    // een gewone gedeelde groep op een app-core, zoals vóór 30-09.
    #[test]
    fn hop_without_os_sharing_takes_an_app_core() {
        use CoreClass::{Big, Small};
        let b = FakeCores::with(4, &[(1, Big), (2, Big), (3, Small), (4, Small)]);
        let mut p = CorePool::new(0);
        let hop = p.place(&b, s(1), &Placement::hop().unwrap()).unwrap();
        assert_eq!(hop.get(), 1);
        let buddy = p.place(&b, s(2), &Placement::hop().unwrap()).unwrap();
        assert_eq!(buddy, hop, "a trusted buddy did not share Hop's core");
        assert_eq!(p.place(&b, s(3), &ded(1, Some(Small))).unwrap().get(), 3);
    }

    #[test]
    fn a_core_run_keeps_the_os_core() {
        let run: Vec<usize> = Core::OS.run(1).map(Core::get).collect();
        assert_eq!(run, [0]);
        let run: Vec<usize> = Core::new(2).unwrap().run(3).map(Core::get).collect();
        assert_eq!(run, [2, 3, 4]);
    }
}
