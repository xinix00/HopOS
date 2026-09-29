//! De naad tussen board en dienst: de controllers zoals een board ze
//! aanbiedt, en het opbrengen ervan.
//!
//! De boards zelf kennen `gui` niet (alleen de binary importeert gui terug),
//! dus de bedrading gebeurt in de binary: die zet in een [`Registry`] wat op
//! dít bordje een xHCI is (Go's `usb_<board>.go` met `Register`) en roept
//! daarna [`Registry::bring_up`]. Een `Registry` is gewone boot-staat in de
//! binary, geen globale lijst: eenmalige initialisatie gebeurt in `main` vóór
//! de eerste `spawn`.

use crate::deliver::InputAddr;
use crate::{MAX_HOSTS, Manager, Sink};
use bounded::{BoundedVec, Full};
use core::fmt;
use dev::Pa;
use driver_xhci::Hc;

/// Waarom de board-voorbereiding van een controller faalde.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PrepareError {
    /// Wat er misging, in het Engels.
    pub what: &'static str,
    /// Het getal erbij (een register, een adres); 0 als er geen is.
    pub value: u64,
}

impl fmt::Display for PrepareError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} ({:#x})", self.what, self.value)
    }
}

/// De board-voorbereiding van een controller: geeft het (eventueel
/// gecorrigeerde) basisadres, of `None` om het geregistreerde te houden.
pub type Prepare = fn() -> Result<Option<Pa>, PrepareError>;

/// Eén controller zoals een board hem aanbiedt.
#[derive(Clone, Copy, Debug)]
pub struct HostSpec {
    /// Komt in élke logregel van deze controller. Een node met drie
    /// controllers moet leesbaar zijn.
    pub name: &'static str,
    /// Het xHCI-capabilityvenster. Vast op een SoC, een BAR achter PCIe.
    /// Nul is toegestaan als `prepare` hem invult (de PCIe-gevallen weten
    /// hem pas ná de enumeratie).
    pub base: Pa,
    /// Wat de controller bij een CPU-fysiek adres optelt (zie
    /// `driver_xhci::Hc`): nul op een SoC, 0x10_0000_0000 achter de RP1 van
    /// de Pi 5.
    pub bus_off: u64,
    /// De board-specifieke voorbereiding: PCIe-link trainen, een DWC3-core
    /// in hostmodus zetten (RK3566), een klok aan. `None` = niets te doen.
    /// Geeft het (eventueel gecorrigeerde) basisadres terug, of `None` om
    /// `base` te houden.
    pub prepare: Option<Prepare>,
}

/// De controllers van deze node, in registratievolgorde.
#[derive(Debug, Default)]
pub struct Registry {
    hosts: BoundedVec<HostSpec, MAX_HOSTS>,
}

impl Registry {
    /// Een lege lijst.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            hosts: BoundedVec::new(),
        }
    }

    /// Meldt een controller aan. Alleen bij boot, vóór [`Registry::bring_up`].
    pub fn register(&mut self, h: HostSpec) -> Result<(), Full<HostSpec>> {
        self.hosts.push(h)
    }

    /// De aangemelde controllers.
    #[must_use]
    pub fn hosts(&self) -> &[HostSpec] {
        &self.hosts
    }

    /// Het stuk van de DMA-regio voor controller `i`: elke controller krijgt
    /// zijn eigen stuk, want ze draaien tegelijk en delen niets. De driver
    /// lijnt zelf op pagina's uit binnen zijn stuk.
    #[must_use]
    pub fn dma_slice(&self, i: usize, dma: Pa, size: u64) -> (Pa, u64) {
        let n = self.hosts.len().max(1) as u64;
        let span = size / n;
        (dma.add(i as u64 * span), span)
    }

    /// Brengt alle aangemelde controllers op en geeft terug hoeveel er
    /// draaien. Geen harde eis: een node zonder werkende USB draait door,
    /// alleen typ je er niet op. Elke controller die het niet doet is één
    /// logregel, en die regel ís de meting, want dit pad is per bord anders
    /// bedraad.
    ///
    /// `dma` is de USB-regio uit het layout-plan (`None`: het board plande
    /// er geen) van `size` bytes. `make` maakt de [`Hc`] voor een venster;
    /// dat is `unsafe` (`Hc::new` vertrouwt het adres) en dus van de binary.
    ///
    /// De binary opent de luisterpost pas als dit meer dan nul geeft, en dán
    /// komt [`InputAddr`] in de fb-grant. Een display die het veld niet ziet
    /// weet dus dat er niets te bellen valt, in plaats van in een
    /// reconnect-lus te gaan zitten voor een toetsenbord dat niet bestaat.
    pub fn bring_up(
        &self,
        mgr: &mut Manager,
        dma: Option<Pa>,
        size: u64,
        sink: &mut impl Sink,
        mut make: impl FnMut(&HostSpec, Pa) -> Hc,
    ) -> usize {
        if self.hosts.is_empty() {
            return 0;
        }
        let Some(dma) = dma else {
            sink.log(format_args!(
                "usb: this board has no USB DMA region in its plan, input disabled"
            ));
            return 0;
        };
        let mut live = 0;
        for (i, h) in self.hosts.iter().enumerate() {
            let mut base = h.base;
            if let Some(prepare) = h.prepare {
                match prepare() {
                    Ok(Some(b)) if b.0 != 0 => base = b,
                    Ok(_) => {}
                    Err(e) => {
                        sink.log(format_args!("usb: {}: {e}", h.name));
                        continue;
                    }
                }
            }
            if base.0 == 0 {
                sink.log(format_args!("usb: {}: no register window, skipped", h.name));
                continue;
            }
            let (slice, span) = self.dma_slice(i, dma, size);
            if let Err(e) = mgr.add(make(h, base), slice, span, sink) {
                sink.log(format_args!("usb: {}: {e}", h.name));
                continue;
            }
            live += 1;
        }
        if live == 0 {
            sink.log(format_args!(
                "usb: no working controller on this node, input stays off"
            ));
        } else {
            sink.log(format_args!(
                "usb: {live} controller(s), input served on {InputAddr} (goes out with the fb grant)"
            ));
        }
        live
    }
}
