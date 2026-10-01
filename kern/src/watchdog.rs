//! Het beleid van de node-watchdog, één keer voor elk board
//! (`OLD/metal/cmd/hopos/watchdog.go`, met zijn policy-tests).
//!
//! In Go waren er vier kopieën gegroeid en die liepen uit elkaar (12-08
//! gezien): de Pi's en UEFI wapenden vroeg maar aaiden blind, precies de
//! aaier die de gemeten doofheid van 02-08 (nieuwe verbindingen en ICMP
//! dood, alle interne lussen kerngezond) nooit gezien zou hebben. Beleid
//! hoort één keer te bestaan; per board blijven alleen de twee
//! hardware-werkwoorden over ([`Hardware`]: wapenen, aaien).
//!
//! Het ene beleid, in twee fasen:
//!
//! 1. **Boot-guard.** Wapenen zo vroeg als de hardware het toelaat, en
//!    blind aaien tot het eerste levensteken. Een node die tijdens de boot
//!    helemaal bevriest (ook de aai-taak), reset zichzelf. Een bring-up die
//!    leeft maar geen netwerk krijgt, blijft staan: het blinde aaien gaat
//!    door, en de wachtregel zegt periodiek waarom er nog geen echt vangnet
//!    is. Een flip-boot krijgt hoogstens twee minuten blind aaien, gemeten
//!    op de rauwe teller (een klokzet van Hop mag die grens niet
//!    verschuiven).
//! 2. **Levensteken.** Vanaf het eerste bewijs dat de node leeft (de
//!    binary bepaalt wat dat is: het net op en de heartbeat van Hop die
//!    loopt) aait het beleid alleen nog op bewijs. Stopt het bewijs, dan
//!    stopt het aaien en reset de hardware de node (HOP-leven = node-leven).
//!    Een verloren adres ([`Policy::request_reset`]) houdt de pets voorgoed
//!    in: de stack kan niet van adres wisselen, dus een koude boot is de
//!    weg terug.
//!
//! Dit is rekenwerk zonder klok en zonder ijzer: de binary geeft de
//! tellerstand en de gezondheid, en logt de [`Event`]s die terugkomen.

/// De gratie van een flip-boot in seconden: twee minuten blind aaien, dan
/// moet de nieuwe kern aantoonbaar leven. GEMETEN 06-09 op de M4: een
/// mislukte flip zonder deze grens liet de node zeven minuten donker in
/// plaats van binnen 30 s te resetten.
pub const FLIP_GRACE_SECS: u64 = 120;

/// De hardware-helft die een board levert. Het beleid roept `arm` één keer
/// en `pet` elke aai-ronde.
pub trait Hardware {
    /// Wapent de watchdog; `false` = onbruikbaar (de binary kent de reden).
    fn arm(&mut self) -> bool;
    /// Laadt de teller opnieuw.
    fn pet(&mut self);
}

/// In welke fase het beleid staat.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Phase {
    /// Nog niet gestart (of geen hardware).
    Idle,
    /// Gewapend, blind aaien tot het eerste levensteken.
    Guard,
    /// Aaien op bewijs.
    Live,
    /// Aaien ingehouden: de hardware reset de node.
    Withheld,
}

/// Wat het beleid meldt; de binary maakt er één consoleregel met marker van.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Event {
    /// De boot-guard van een flip-boot is gewapend.
    BootGuardArmed,
    /// De hardware liet zich niet wapenen: deze boot is onbewaakt.
    Unguarded,
    /// De gratie van de flip-boot liep af vóór het levensteken: geen pets
    /// meer (`HOPOS_BOOT_GUARD_EXPIRED`).
    GraceExpired,
    /// Gewapend; blind aaien tot het levensteken.
    Armed,
    /// Nog geen levensteken na zoveel rondes.
    Waiting(u64),
    /// Het levensteken kwam: vanaf nu aaien op bewijs
    /// (`HOPOS_CANARY_LIVE`).
    Live,
    /// Het bewijs ontbrak zoveel rondes op rij: pet ingehouden
    /// (`HOPOS_CANARY_MISS`).
    Miss(u64),
    /// Een reset is gevraagd: pets ingehouden (`HOPOS_RESET_REQUESTED`).
    ResetRequested(&'static str),
}

/// Het beleid. Eén eigenaar: de watchdog-taak van de binary.
#[derive(Debug)]
pub struct Policy {
    /// De rauwe tellerstand waarop de gratie van een flip-boot afloopt;
    /// 0 = koude boot, onbegrensd blind.
    deadline: u64,
    phase: Phase,
    attempts: u64,
    misses: u64,
    reset: Option<&'static str>,
    /// Om de hoeveel rondes de wachtregel komt (~5 minuten).
    loud_every: u64,
}

impl Policy {
    /// Een beleid dat nog niets deed. `loud_every`: om de hoeveel
    /// aai-rondes de wachtregel van fase 1 komt.
    #[must_use]
    pub const fn new(loud_every: u64) -> Self {
        Self {
            deadline: 0,
            phase: Phase::Idle,
            attempts: 0,
            misses: 0,
            reset: None,
            loud_every: if loud_every == 0 { 1 } else { loud_every },
        }
    }

    /// De fase.
    #[must_use]
    pub const fn phase(&self) -> Phase {
        self.phase
    }

    /// De deadline van de flip-gratie op de rauwe teller (0 = geen).
    #[must_use]
    pub const fn deadline(&self) -> u64 {
        self.deadline
    }

    /// Is de gratie van de flip-boot op tellerstand `counter` op?
    fn expired(&self, counter: u64) -> bool {
        self.deadline != 0 && counter >= self.deadline
    }

    /// Eén blinde pet, binnen de gratie van een flip-boot. `false` = de
    /// gratie is op, er is niet geaaid. Een bewezen levensteken heeft geen
    /// gratie nodig.
    fn pet_boot_guard(&self, hw: &mut impl Hardware, counter: u64) -> bool {
        if self.expired(counter) {
            return false;
        }
        hw.pet();
        true
    }

    /// Start het beleid op een flip-boot: als [`Policy::start`], met een
    /// gratie van [`FLIP_GRACE_SECS`] op de rauwe teller (`hz` tikken per
    /// seconde). De vorige kern had een gewapende watchdog die bij de
    /// landing stil werd gezet; sterft deze kern in zijn bring-up, dan waakt
    /// er anders niemand.
    pub fn arm_boot_guard(&mut self, hw: &mut impl Hardware, counter: u64, hz: u64) -> Event {
        self.deadline = counter
            .saturating_add(FLIP_GRACE_SECS.saturating_mul(hz))
            .max(1);
        match self.start(hw, counter) {
            Event::Armed => Event::BootGuardArmed,
            e => e,
        }
    }

    /// Start het beleid (de canary): wapent één keer. Is de gratie van een
    /// flip-boot al op, dan wapent hij níét (dat zou stil een tweede gratie
    /// geven) en aait hij nooit meer.
    pub fn start(&mut self, hw: &mut impl Hardware, counter: u64) -> Event {
        if self.expired(counter) {
            self.phase = Phase::Withheld;
            return Event::GraceExpired;
        }
        if !hw.arm() {
            self.phase = Phase::Idle;
            return Event::Unguarded;
        }
        self.phase = Phase::Guard;
        Event::Armed
    }

    /// Een reset gevraagd (een verloren adres): de volgende ronde houdt de
    /// pets voorgoed in.
    pub fn request_reset(&mut self, reason: &'static str) {
        self.reset.get_or_insert(reason);
    }

    /// Eén aai-ronde: `alive` is het bewijs van deze ronde.
    pub fn tick(&mut self, hw: &mut impl Hardware, counter: u64, alive: bool) -> Option<Event> {
        match self.phase {
            Phase::Idle | Phase::Withheld => return None,
            Phase::Guard | Phase::Live => {}
        }
        if let Some(r) = self.reset {
            self.phase = Phase::Withheld;
            return Some(Event::ResetRequested(r));
        }
        if self.phase == Phase::Guard {
            if alive {
                self.phase = Phase::Live;
                hw.pet();
                return Some(Event::Live);
            }
            self.attempts += 1;
            if !self.pet_boot_guard(hw, counter) {
                self.phase = Phase::Withheld;
                return Some(Event::GraceExpired);
            }
            let n = self.attempts;
            let loud = n == 3 || n.is_multiple_of(self.loud_every);
            return loud.then_some(Event::Waiting(n));
        }
        if alive {
            hw.pet();
            self.misses = 0;
            return None;
        }
        self.misses += 1;
        Some(Event::Miss(self.misses))
    }
}

#[cfg(test)]
mod tests {
    //! De policy-tests uit `watchdog_policy_test.go`, naam voor naam.
    use super::*;

    #[derive(Default)]
    struct Fake {
        pets: u32,
        arms: u32,
        refuse: bool,
    }

    impl Hardware for Fake {
        fn arm(&mut self) -> bool {
            self.arms += 1;
            !self.refuse
        }
        fn pet(&mut self) {
            self.pets += 1;
        }
    }

    /// `TestBlindPetsShareOneFlipDeadline`.
    #[test]
    fn blind_pets_share_one_flip_deadline() {
        let mut hw = Fake::default();
        let mut p = Policy::new(1);
        p.deadline = 120_000;
        assert!(p.pet_boot_guard(&mut hw, 60_000), "early pet refused");
        assert_eq!(p.start(&mut hw, 60_000), Event::Armed);
        assert_eq!(p.deadline, 120_000, "handover renewed grace");
        assert!(p.pet_boot_guard(&mut hw, 119_999), "phase1 pet refused");
        assert!(!p.pet_boot_guard(&mut hw, 120_000), "pet at deadline");
        assert!(!p.pet_boot_guard(&mut hw, 999_999));
        assert_eq!(hw.pets, 2, "pet after deadline");
    }

    /// `TestBootGuardUsesRawCounterDespiteClockEpochChange`.
    #[test]
    fn boot_guard_uses_raw_counter_despite_clock_epoch_change() {
        let mut hw = Fake::default();
        let mut p = Policy::new(1);
        let hz = 24_000_000;
        let ticks = 24_000_000;
        assert_eq!(p.arm_boot_guard(&mut hw, ticks, hz), Event::BootGuardArmed);
        assert_eq!(hw.arms, 1, "a flip boot armed twice");
        let want = 24_000_000 + 120 * 24_000_000;
        assert_eq!(p.deadline(), want, "deadline in raw ticks");
        // Een klokverschuiving ter grootte van een datum zou een deadline op
        // de wandklok meteen laten aflopen; de rauwe teller kent hem niet.
        let epoch_offset: u64 = 1_788_652_800_000_000_000; // 2026-09-06 in ns
        assert!(epoch_offset > want, "invalid clock-shift fixture");
        assert!(p.pet_boot_guard(&mut hw, ticks) && p.deadline() == want);
        assert!(p.pet_boot_guard(&mut hw, want - 1), "raw grace shortened");
        assert!(
            !p.pet_boot_guard(&mut hw, want),
            "raw deadline not enforced"
        );
        assert_eq!(hw.pets, 2);
    }

    /// `TestColdBootBlindPetRemainsUnlimited`.
    #[test]
    fn cold_boot_blind_pet_remains_unlimited() {
        let mut hw = Fake::default();
        let p = Policy::new(1);
        assert!(p.pet_boot_guard(&mut hw, 1), "cold pet refused");
        assert!(p.pet_boot_guard(&mut hw, 1 << 63));
        assert_eq!(hw.pets, 2, "cold bring-up lost unlimited grace");
    }

    /// `TestExpiredFlipCanaryDoesNotRearm`.
    #[test]
    fn expired_flip_canary_does_not_rearm() {
        let mut hw = Fake::default();
        let mut p = Policy::new(1);
        p.deadline = 100;
        assert_eq!(p.start(&mut hw, 100), Event::GraceExpired);
        assert_eq!(
            (hw.arms, hw.pets),
            (0, 0),
            "expired handover rearmed/petted"
        );
        assert_eq!(p.tick(&mut hw, 101, true), None);
        assert_eq!(hw.pets, 0);
    }

    /// `TestFlipCanaryWithoutAgentStopsBlindPets`.
    #[test]
    fn flip_canary_without_agent_stops_blind_pets() {
        let mut hw = Fake::default();
        let mut p = Policy::new(1);
        let mut raw = 0u64;
        let mut counter = || {
            raw += 1;
            raw
        };
        p.deadline = 4;
        assert_eq!(p.start(&mut hw, counter()), Event::Armed);
        // Geen agent (geen adres, geen heartbeat): nooit levend.
        let mut last = None;
        for _ in 0..10 {
            let e = p.tick(&mut hw, counter(), false);
            if e.is_some() {
                last = e;
            }
            if p.phase() == Phase::Withheld {
                break;
            }
        }
        assert_eq!(last, Some(Event::GraceExpired));
        let before = hw.pets;
        let now = counter();
        assert!(now >= 4);
        assert!(!p.pet_boot_guard(&mut hw, now));
        assert_eq!(p.tick(&mut hw, counter(), true), None);
        assert_eq!(hw.pets, before, "failed canary kept petting");
    }

    #[test]
    fn liveness_then_misses_then_a_requested_reset() {
        let mut hw = Fake::default();
        let mut p = Policy::new(100);
        assert_eq!(p.start(&mut hw, 1), Event::Armed);
        assert_eq!(p.tick(&mut hw, 2, false), None);
        assert_eq!(p.tick(&mut hw, 3, false), None);
        assert_eq!(p.tick(&mut hw, 4, false), Some(Event::Waiting(3)));
        assert_eq!(hw.pets, 3, "blind pets in phase 1");
        assert_eq!(p.tick(&mut hw, 5, true), Some(Event::Live));
        assert_eq!(p.tick(&mut hw, 6, false), Some(Event::Miss(1)));
        assert_eq!(p.tick(&mut hw, 7, false), Some(Event::Miss(2)));
        assert_eq!(hw.pets, 4, "a miss withholds the pet");
        assert_eq!(p.tick(&mut hw, 8, true), None);
        assert_eq!(hw.pets, 5);
        p.request_reset("address lost");
        assert_eq!(
            p.tick(&mut hw, 9, true),
            Some(Event::ResetRequested("address lost"))
        );
        assert_eq!(p.tick(&mut hw, 10, true), None);
        assert_eq!(hw.pets, 5, "pets after a reset request");
    }

    #[test]
    fn no_hardware_is_unguarded() {
        let mut hw = Fake {
            refuse: true,
            ..Fake::default()
        };
        let mut p = Policy::new(1);
        assert_eq!(p.arm_boot_guard(&mut hw, 1, 10), Event::Unguarded);
        assert_eq!(p.tick(&mut hw, 3, true), None);
        assert_eq!((hw.arms, hw.pets), (1, 0));
    }
}
