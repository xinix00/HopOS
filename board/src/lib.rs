//! Het board-contract: wat de kern van een machine vraagt.
//!
//! De Go-kern had één `board.Board`-interface met een register (`Use`,
//! `Current`) dat bij runtime gevuld werd. Hier is het een trait met
//! statische dispatch: de binary kiest precies één board via een feature,
//! en een gemiste methode is een compilefout in plaats van een verrassing
//! op het bord (handboek §7).
//!
//! De trait is met opzet klein. Wat de Go-interface verder droeg (de
//! wandklok, PCIe, de framebuffer, het netplan, de core-start via PSCI)
//! komt erbij zodra een laag erom vraagt; een methode zonder gebruiker is
//! een methode die niemand test.
//!
//! Naast de trait staat [`heap`]: de allocator met een plafond die het
//! board over zijn kern-RAM legt.

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

pub mod heap;

/// De framebuffer-beschrijving van [`Board::framebuffer`], zodat een board
/// hem noemt zonder eigen dependency.
pub use driver_fb as fb;

use bounded::BoundedVec;
use core::fmt;
use dev::Pa;
use sync::Signal;

/// Een fysiek geheugenbereik.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Region {
    /// Het beginadres.
    pub base: Pa,
    /// De grootte in bytes.
    pub size: u64,
}

impl Region {
    /// Het eerste adres na het bereik.
    #[must_use]
    pub const fn end(&self) -> Pa {
        self.base.add(self.size)
    }

    /// Ligt `pa` in het bereik?
    #[must_use]
    pub const fn contains(&self, pa: Pa) -> bool {
        pa.0 >= self.base.0 && pa.0 < self.base.0 + self.size
    }
}

/// Het PA-plan van een board: waar de kern woont en waar de DMA-regio's
/// liggen. De tegenhanger van `layout.Plan` uit de Go-kern, voor zover de
/// kern het nu gebruikt.
#[derive(Copy, Clone, Debug)]
pub struct Plan {
    /// De kern-RAM: image, stack en heap. Normal, gecached.
    pub kern_ram: Region,
    /// De DMA-regio van alle drivers samen, buiten de kern-RAM en niet
    /// gecached gemapt.
    pub dma: Region,
    /// Het deel van `dma` dat de NIC krijgt.
    pub net_dma: Region,
}

/// Hoeveel USB-hostcontrollers een board kan aanbieden. De O6N meldt er in
/// zijn DSDT tot tien (de firmware-upgrade van zes naar tien, Go 18-09), de
/// rest één of twee; gelijk aan `gui_usbin::MAX_HOSTS`.
pub const MAX_USB_HOSTS: usize = 10;

/// Wat voor controller achter een [`UsbHost`] zit.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum UsbKind {
    /// Een xHCI die klaarstaat: het venster is het capability-blok (PCIe,
    /// de RP1 van de Pi 5).
    Xhci,
    /// Een Synopsys DWC3-core: eerst in hostmodus zetten (de globale
    /// registers op +0xC100 van hetzelfde venster), daarna is het venster
    /// een xHCI (de RK3566).
    Dwc3,
}

/// Eén USB-hostcontroller zoals het board hem kent: het venster, de lijn,
/// het soort, en het stuk DMA-geheugen dat het board voor hem plande. Een
/// board dat hem achter PCIe heeft, heeft de link en de BAR al opgebracht
/// voor het hem noemt.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct UsbHost {
    /// Komt in elke logregel van deze controller.
    pub name: &'static str,
    /// Het soort.
    pub kind: UsbKind,
    /// Het registervenster (het xHCI-capability-blok, of de DWC3-core).
    /// Device gemapt, voor altijd.
    pub regs: Region,
    /// De interruptlijn (GIC INTID), als het board hem weet. De driver
    /// pollt vandaag; de lijn staat hier voor de dag dat hij dat niet meer
    /// doet, en voor de logregel.
    pub irq: Option<u32>,
    /// Wat de controller bij een CPU-fysiek adres optelt: nul op een SoC,
    /// het inbound-venster van de root-complex achter PCIe (de RP1 van de
    /// Pi 5: 0x10_0000_0000).
    pub bus_off: u64,
    /// Het DMA-geheugen van deze controller alleen: Normal-NC en buiten
    /// elke RAM-declaratie, zoals de NIC-ringen.
    pub dma: Region,
}

/// Het `i`-de van `n` gelijke, op 4 KB gealigneerde stukken van `r`: de
/// verdeling van één USB-DMA-regio over meer controllers. Leeg voor een
/// `i` buiten `0..n`.
#[must_use]
pub const fn usb_dma_slice(r: Region, i: usize, n: usize) -> Region {
    if n == 0 || i >= n {
        return Region {
            base: r.base,
            size: 0,
        };
    }
    let span = (r.size / n as u64) & !0xfff;
    Region {
        base: r.base.add(i as u64 * span),
        size: span,
    }
}

/// De USB-hosts van een board: begrensd en zonder heap.
pub type UsbHosts = BoundedVec<UsbHost, MAX_USB_HOSTS>;

/// De clusterklasse van een core ("small", "mid", "big"). HOP's plaatsing
/// doet exact-match op klasse.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum CoreClass {
    /// Een zuinige core.
    Small,
    /// Het middensegment.
    Mid,
    /// De beste (of enige) klasse.
    Big,
}

impl fmt::Display for CoreClass {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Small => "small",
            Self::Mid => "mid",
            Self::Big => "big",
        })
    }
}

/// Waarom een board iets weigert.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// Het privilege-niveau kan geen kooi dragen (EL2 op ARM).
    Privilege {
        /// Het niveau waarop we booten.
        el: u8,
    },
    /// De NIC is gevonden maar zijn initialisatie faalde.
    Nic(&'static str),
    /// De schijf is gevonden maar haar initialisatie faalde.
    Disk(&'static str),
    /// De interruptcontroller kwam niet op.
    Irq(&'static str),
    /// Een methode die maar één keer mag, werd twee keer aangeroepen.
    Twice(&'static str),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Privilege { el } => write!(
                f,
                "booted at EL{el}: HopOS requires EL2 (QEMU: virtualization=on)"
            ),
            Self::Nic(why) => write!(f, "nic init: {why}"),
            Self::Disk(why) => write!(f, "disk init: {why}"),
            Self::Irq(why) => write!(f, "interrupt controller: {why}"),
            Self::Twice(what) => write!(f, "{what} called twice"),
        }
    }
}

/// Geen schijf: het blokapparaat van een board zonder blokdriver (de Pi's,
/// de Radxa, de LicheeRV). Een lege enum, dus er bestaat nooit een waarde
/// van; hun `probe_disk` geeft altijd `Ok(None)` en de bestandscalls
/// weigeren luid.
#[derive(Debug)]
pub enum NoDisk {}

impl NoDisk {
    /// Het aantal sectoren (bestaat niet).
    #[must_use]
    pub fn sectors(&self) -> u64 {
        match *self {}
    }

    /// Het model (bestaat niet).
    #[must_use]
    pub fn model(&self) -> &'static str {
        match *self {}
    }
}

impl blkdev::AsyncBlockDevice for NoDisk {
    fn max_transfer(&self) -> usize {
        match *self {}
    }
    fn start(&mut self, _op: blkdev::Op<'_>) -> blkdev::Result {
        match *self {}
    }
    fn poll_done(&mut self, _into: &mut [u8]) -> core::task::Poll<blkdev::Result> {
        match *self {}
    }
}

/// De eis van ARM: HopOS draait op EL2. De Go-zin `board.RequireEL2`.
pub fn require_el2(el: u8) -> Result<(), Error> {
    if el < 2 {
        return Err(Error::Privilege { el });
    }
    Ok(())
}

/// Wat de interrupt-dispatch in één ronde deed; de meetlat van de
/// IRQ-taak.
#[derive(Copy, Clone, Default, Debug, PartialEq, Eq)]
pub struct Dispatched {
    /// Timer-interrupts.
    pub timer: u32,
    /// NIC-interrupts.
    pub nic: u32,
    /// Lijnen die niemand kent (gemeld en afgesloten).
    pub other: u32,
}

/// Het board-contract. Alle methodes draaien op HOP's core; de binary
/// houdt het board in een `static` en geeft `&'static` door.
pub trait Board: Sync {
    /// De NIC-driver van dit board.
    type Nic: netdev::Device;
    /// De slaap van de executor op dit board.
    type Sleeper: executor::Sleeper;

    /// De naam, voor de bootlog.
    const NAME: &'static str;

    /// Eén keer, als eerste: de UART op. Geeft de console-haak.
    fn console(&self) -> fn(&[u8]);

    /// Het exception level waarop we booten, zoals de stub het las.
    fn privilege(&self, el: u8) -> Result<(), Error> {
        require_el2(el)
    }

    /// Eén consoleregel over wie ons bootte en wat die kan. Diagnose, geen
    /// contract.
    fn firmware(&self) -> &'static str;

    /// Geeft `heap` het deel van de kern-RAM dat na image en stack over is.
    /// Eén keer, vóór de eerste allocatie.
    fn init_heap(&self, heap: &heap::Heap);

    /// Leest de firmware-beschrijving (op ARM de FDT uit x0 of een vaste
    /// plek) en onthoudt wat de kern later vraagt. `dtb` is x0 bij boot.
    fn discover(&self, dtb: u64);

    /// De klok: monotone nanoseconden sinds boot.
    fn clock(&self) -> executor::Clock;

    /// De slaap van de executor.
    fn sleeper(&self) -> Self::Sleeper;

    /// DRAM in bytes zoals de firmware het meldt; 0 = onbekend.
    fn mem_total(&self) -> u64;

    /// Het aantal cores, HOP's eigen core meegeteld.
    fn cores(&self) -> usize;

    /// De clusterklasse van `core`.
    fn core_class(&self, core: usize) -> CoreClass;

    /// Het PA-plan.
    fn plan(&self) -> Plan;

    /// Zet de interruptcontroller op HOP's core en de timer-interrupt aan.
    /// Geeft de bel die de IRQ-deur luidt: een taak wacht erop en roept
    /// dan [`dispatch_interrupts`](Board::dispatch_interrupts).
    fn start_interrupts(&self) -> Result<&'static Signal, Error>;

    /// Claimt, behandelt en sluit elke wachtende interrupt. Draait in een
    /// taak, nooit in exception-context.
    fn dispatch_interrupts(&self) -> Dispatched;

    /// Vindt en initialiseert de NIC. `Ok(None)` = geen NIC; één keer.
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error>;

    /// De lineaire framebuffer van dit board, als er een beeld loopt (GOP,
    /// de VideoCore-mailbox, `ramfb`, of de eigen scanout van de RK3566).
    /// `None` = headless, en dat is geen fout. Alleen met de feature `gui`
    /// levert een board er een (docs/gui.md).
    fn framebuffer(&self) -> Option<fb::Desc> {
        None
    }

    /// De USB-hostcontrollers van dit board, klaar voor de xHCI-driver:
    /// PCIe-link en BAR's opgebracht, de firmware-handshake gedaan. Eén keer,
    /// na het netwerk (de invoer gaat over de switch naar de display-app).
    /// Leeg = geen USB-invoer, en dat is geen fout. Alleen met de feature
    /// `gui` levert een board er een (docs/gui.md); elke stap die faalt, is
    /// één logregel van het board.
    fn usb_hosts(&self) -> UsbHosts {
        UsbHosts::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn el2_is_required() {
        assert_eq!(require_el2(1), Err(Error::Privilege { el: 1 }));
        assert!(require_el2(2).is_ok());
        assert_eq!(
            Error::Privilege { el: 1 }.to_string(),
            "booted at EL1: HopOS requires EL2 (QEMU: virtualization=on)"
        );
    }

    #[test]
    fn region_bounds() {
        let r = Region {
            base: Pa(0x1000),
            size: 0x1000,
        };
        assert!(r.contains(Pa(0x1000)));
        assert!(r.contains(Pa(0x1fff)));
        assert!(!r.contains(Pa(0x2000)));
        assert_eq!(r.end(), Pa(0x2000));
        assert_eq!(CoreClass::Big.to_string(), "big");
    }

    #[test]
    fn usb_dma_is_split_on_pages() {
        let r = Region {
            base: Pa(0x4fe0_0000),
            size: 0x20_0000,
        };
        assert_eq!(usb_dma_slice(r, 0, 1), r);
        let b = usb_dma_slice(r, 2, 3);
        assert_eq!(b.base, Pa(0x4fe0_0000 + 2 * 0xa_a000));
        assert_eq!(b.size, 0xa_a000);
        assert!(b.end().0 <= r.end().0);
        assert_eq!(usb_dma_slice(r, 3, 3).size, 0);
        assert_eq!(usb_dma_slice(r, 0, 0).size, 0);
    }
}
