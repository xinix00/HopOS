//! De USB-hostcontroller van HopOS: één xHCI-driver voor elk board dat een
//! toetsenbord (of een optische drive) moet kunnen zien.
//!
//! De Rust-vorm van `OLD/metal/gui/driver/usb/xhci`. Wat deze crate BEZIT:
//! het registervenster van één controller, zijn DMA-regio (DCBAA,
//! scratchpad, command- en event-ring, de vaste structuren per slot en de
//! bulk-bouncebuffer) en de staat van elk geadresseerd apparaat. Wat hij NIET
//! bezit: de keuze wanneer er gescand of gepold wordt (dat is `gui-usbin`),
//! de board-voorbereiding (PCIe-link, de DWC3-core van de RK3566 in
//! hostmodus: dat is het board) en de klok (die komt van buiten).
//!
//! WAAROM XHCI EN NIET OHCI. Een USB-toetsenbord is low-speed, en OHCI is
//! een fractie van het werk van XHCI, dus dat lijkt de goedkope route. Hij
//! is het niet, want de Pi's hebben helemaal geen OHCI: op de Pi 4 hangt de
//! USB aan een VL805 (XHCI over PCIe) en op de Pi 5 aan de RP1 (XHCI over
//! PCIe). Wie die twee wil bedienen schrijft sowieso XHCI. En de RK3566
//! heeft XHCI-poorten (achter een DWC3), dus met één driver zijn we er op
//! alle drie. Alle moderne XHCI-controllers spreken bovendien zelf met
//! low-speed apparaten: de hub-logica zit in de controller, dus wij zien een
//! poort met een snelheid en niet een companion-controller.
//!
//! REFERENTIE: xHCI 1.2 (Intel, mei 2019), hoofdstuk 4 (operational model)
//! en 5 (registers). Waar de spec meerdere kanten op kan, staat de gemaakte
//! keuze in het commentaar met het spec-nummer erbij.
//!
//! GEEN INTERRUPTS. Deze driver pollt: een toetsenbord is met
//! 8ms-intervallen ruim binnen wat pollen aankan. De interrupter wordt wél
//! opgezet (de event-ring hangt eraan), alleen staat IE uit.
//!
//! ÉÉN EIGENAAR. Een [`Hc`] en al zijn apparaten worden door precies één
//! taak bediend (de `usbin`-manager) via `&mut self`. Een apparaat is een
//! handvat ([`Device`], `Copy`), geen verwijzing: wie iets wil met slot 3
//! vraagt het aan de eigenaar van de controller. Daarom is bulk hier
//! asynchroon ([`Hc::start_bulk_in`] en [`Hc::poll_bulk`]): de eigenaar moet
//! tussen twee pakketten van een drive door gewoon het toetsenbord kunnen
//! blijven lezen.
//!
//! DE KLOK EN DE SLAAP. Elke wachtlus is `async` en slaapt tussen twee
//! blikken op de [`Timer`] van de eigenaar-taak (in de binary het timerwiel
//! van de executor). De lange wachten (poortreset tot 2 s, commando's tot
//! 1 s) zitten alleen in het koude pad: [`Hc::reset`], [`Hc::start`],
//! [`Hc::power_on`] en [`Hc::attach`]. Tot 29-09 spinde de driver op de
//! klok, zoals `driver-nvme`: een insteek hield dan de hele executor van de
//! kern tot 2 s per poortreset vast (Go sliep daar in een goroutine). Nu
//! draait de rest van de node door terwijl een poort traint. Het hete pad
//! ([`Hc::report`], [`Hc::poll_bulk`]) wacht alleen als een endpoint stalt
//! (de herstelcommando's) en alloceert niets.

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

use core::fmt;
use core::future::Future;
use core::mem::offset_of;
use dev::{Pa, Reg};

mod bulk;
mod device;
mod host;
mod ring;

pub use bulk::{BulkTd, TRB_MAX};
pub use device::{Device, MAX_HID_IFACES, PROTO_KEYBOARD, PROTO_MOUSE, PROTO_NONE, Report};
pub use host::{BULK_BUF_MAX, BULK_BUF_MIN, MAX_DEVICES, PENDING_CAP, SCRATCH_MAX};

use host::{Arena, SlotRes};
use ring::{EvRing, Ring, comp_name};

/// De capability-registers (xHCI 5.3): de eerste bytes van het venster.
/// Read-only; ze vertellen waar de rest ligt. De operational-registers
/// staan op een OFFSET die per controller verschilt, dus die moet je lezen
/// en niet aannemen.
#[repr(C)]
struct CapRegs {
    /// CAPLENGTH [7:0] (offset naar de operational-registers) en HCIVERSION
    /// [31:16] (BCD: 0x0100 = 1.0, 0x0110 = 1.1, 0x0120 = 1.2).
    caplength: Reg<u32>,
    /// Slots [7:0], interrupters [18:8], poorten [31:24].
    hcsparams1: Reg<u32>,
    /// Scratchpad-buffers (hi [25:21], lo [31:27]).
    hcsparams2: Reg<u32>,
    _hcsparams3: Reg<u32>,
    /// 64-bit adressering [0], contextgrootte [2].
    hccparams1: Reg<u32>,
    /// Offset naar de doorbell-array.
    dboff: Reg<u32>,
    /// Offset naar de runtime-registers.
    rtsoff: Reg<u32>,
    _hccparams2: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(CapRegs, caplength) == 0x00);
    assert!(offset_of!(CapRegs, hcsparams1) == 0x04);
    assert!(offset_of!(CapRegs, hcsparams2) == 0x08);
    assert!(offset_of!(CapRegs, hccparams1) == 0x10);
    assert!(offset_of!(CapRegs, dboff) == 0x14);
    assert!(offset_of!(CapRegs, rtsoff) == 0x18);
    assert!(offset_of!(CapRegs, _hccparams2) == 0x1C);
};

/// De operational-registers (xHCI 5.4), op `base + CAPLENGTH`.
///
/// De 64-bit registers (CRCR, DCBAAP, en ERSTBA en ERDP in [`IrRegs`])
/// zijn `Reg<u64>`: één native 64-bit-schrijf publiceert adres en
/// stuurbits samen. Hoog, laag, hoog verliest op de CIX (de O6N) de
/// ringstand; alleen laag-dan-hoog mist QEMU's latch op het hoge woord
/// (`hw/usb/hcd-xhci.c`). GEMETEN 30-09 op de O6N: tien xHCI's up en de
/// Blu-ray-drive over USB-BOT.
#[repr(C)]
struct OpRegs {
    usbcmd: Reg<u32>,
    usbsts: Reg<u32>,
    pagesize: Reg<u32>,
    _r0: [u32; 2],
    _dnctrl: Reg<u32>,
    /// Command ring control; het lage woord draagt RCS.
    crcr: Reg<u64>,
    _r1: [u32; 4],
    /// Device context base address array pointer.
    dcbaap: Reg<u64>,
    config: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(OpRegs, usbcmd) == 0x00);
    assert!(offset_of!(OpRegs, usbsts) == 0x04);
    assert!(offset_of!(OpRegs, pagesize) == 0x08);
    assert!(offset_of!(OpRegs, _dnctrl) == 0x14);
    assert!(offset_of!(OpRegs, crcr) == 0x18);
    assert!(offset_of!(OpRegs, dcbaap) == 0x30);
    assert!(offset_of!(OpRegs, config) == 0x38);
};

/// Poortregisters: per poort vier woorden vanaf `op + OP_PORT_BASE`.
const OP_PORT_BASE: u64 = 0x400;
const OP_PORT_STRIDE: u64 = 0x10;

/// Eén poort-registerset (xHCI 5.4.8 en verder).
#[repr(C)]
struct PortRegs {
    /// Status en control.
    portsc: Reg<u32>,
    _portpmsc: Reg<u32>,
    _portli: Reg<u32>,
    _porthlpmc: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(PortRegs, portsc) == 0x00);
    assert!(core::mem::size_of::<PortRegs>() as u64 == OP_PORT_STRIDE);
};

/// Interrupter-registerset 0 (xHCI 5.5.2), op `rt + RT_IR0`.
#[repr(C)]
struct IrRegs {
    _iman: Reg<u32>,
    imod: Reg<u32>,
    erstsz: Reg<u32>,
    _r0: u32,
    erstba: Reg<u64>,
    /// Bit 3 = EHB (event handler busy, write-1-to-clear).
    erdp: Reg<u64>,
}

const RT_IR0: u64 = 0x20;

const _: () = {
    assert!(offset_of!(IrRegs, imod) == 0x04);
    assert!(offset_of!(IrRegs, erstsz) == 0x08);
    assert!(offset_of!(IrRegs, erstba) == 0x10);
    assert!(offset_of!(IrRegs, erdp) == 0x18);
};

// USBCMD-bits (xHCI 5.4.1).
const CMD_RUN: u32 = 1 << 0;
const CMD_HCRST: u32 = 1 << 1;

// USBSTS-bits (xHCI 5.4.2).
const STS_HCH: u32 = 1 << 0;
const STS_CNR: u32 = 1 << 11;

const ERDP_EHB: u64 = 1 << 3;

// PORTSC-bits (xHCI 5.4.8). De schrijf-semantiek van dit register is een
// val: CSC/PEC/PRC enzovoort zijn write-1-to-clear terwijl PED
// write-1-to-DISABLE is, en ze zitten in hetzelfde woord. Een
// read-modify-write die de statusbits laat staan wist ze dus per ongeluk,
// en eentje die PED meeschrijft zet de poort uit. Daarom schrijft deze
// driver PORTSC nooit met een kale RMW: zie `port_write`.
const PSC_CCS: u32 = 1 << 0;
const PSC_PED: u32 = 1 << 1;
const PSC_PR: u32 = 1 << 4;
const PSC_PP: u32 = 1 << 9;
const PSC_CSC: u32 = 1 << 17;
const PSC_PEC: u32 = 1 << 18;
const PSC_WRC: u32 = 1 << 19;
const PSC_OCC: u32 = 1 << 20;
const PSC_PRC: u32 = 1 << 21;
const PSC_PLC: u32 = 1 << 22;
const PSC_CEC: u32 = 1 << 23;
const PSC_WPR: u32 = 1 << 31;
/// De w1c-bits bij elkaar: dit masker moet je bij élke PORTSC-schrijfactie
/// uitmaskeren als je ze niet wilt wissen.
const PSC_CHANGE_MASK: u32 = PSC_CSC | PSC_PEC | PSC_WRC | PSC_OCC | PSC_PRC | PSC_PLC | PSC_CEC;
/// De bits die een ACTIE starten in plaats van een stand te beschrijven.
/// Een read-modify-write moet ze altijd uitmaskeren; wie ze wél wil zetten
/// doet dat expliciet via `port_action`.
const PSC_ACTION_MASK: u32 = PSC_PED | PSC_PR | PSC_WPR;
const PSC_SPEED_SHIFT: u32 = 10;
const PSC_SPEED_MASK: u32 = 0xF;

/// Hoe lang een registerwacht (halt, HCRST, CNR, run) mag duren. De spec
/// noemt geen bovengrens, alleen "de driver moet wachten"; een halve seconde
/// is ruim en houdt een dode controller kort.
const REG_TIMEOUT_NS: u64 = 500_000_000;

/// De spec eist 20ms tussen poortvoeding en een betrouwbare CCS-lezing
/// (xHCI 4.19.3 verwijst naar USB2 9.1.2: de poort moet debouncen).
const POWER_SETTLE_NS: u64 = 20_000_000;

/// Hoe lang een wachtlus slaapt tussen twee blikken op een register of de
/// event-ring. Een commando is op echte hardware in tientallen
/// microseconden klaar en een poortreset in ~50 ms; een kwart milliseconde
/// houdt een enumeratie (een dozijn commando's en transfers) ruim onder de
/// 10 ms zonder dat de taak de executor ronde na ronde bezet houdt.
pub const POLL_STEP_NS: u64 = 250_000;

/// De klok en de slaap van de taak die de controller bezit. De binary geeft
/// het timerwiel van zijn executor (`after`); de host-tests een klok die
/// bij elke slaap vooruit springt.
pub trait Timer {
    /// Monotone nanoseconden.
    fn now(&self) -> u64;

    /// Slaapt `ns` nanoseconden. De executor draait intussen de andere
    /// taken: dit is waar de driver de core teruggeeft.
    fn sleep(&self, ns: u64) -> impl Future<Output = ()>;
}

/// De snelheid die de controller aan een poort meldt (xHCI 7.2.2.1.1: de
/// default speed-ID's; een controller mag ze via zijn extended capabilities
/// anders indelen, maar geen enkele die wij bedienen doet dat).
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub struct Speed(pub u8);

impl Speed {
    /// Niets aangesloten.
    pub const NONE: Speed = Speed(0);
    /// 12 Mbit.
    pub const FULL: Speed = Speed(1);
    /// 1,5 Mbit: waar een toetsenbord doorgaans zit.
    pub const LOW: Speed = Speed(2);
    /// 480 Mbit.
    pub const HIGH: Speed = Speed(3);
    /// 5 Gbit.
    pub const SUPER: Speed = Speed(4);
}

impl fmt::Display for Speed {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match *self {
            Self::FULL => "full-speed",
            Self::LOW => "low-speed",
            Self::HIGH => "high-speed",
            Self::SUPER => "super-speed",
            _ => "none",
        })
    }
}

/// De stand van één poort.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub struct Port {
    /// 1-gebaseerd, zoals de spec ze nummert.
    pub num: u8,
    /// Er hangt iets aan (CCS).
    pub connected: bool,
    /// De poort is enabled (PED).
    pub enabled: bool,
    /// De gemelde snelheid.
    pub speed: Speed,
    /// PORTSC zoals gelezen.
    pub raw: u32,
}

/// Waarom software niet meer kan bewijzen welke slots de controller bezit.
/// Vanaf dat moment mag er geen Enable Slot meer volgen: alleen een
/// geslaagde controllerreset (HCRST) maakt alle hardware-slots aantoonbaar
/// vrij.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Poison {
    /// Een commando kreeg geen completion: de command ring is van de
    /// controller zelf, en zwijgt hij daarop, dan weet niemand meer wat hij
    /// nog uitvoert.
    CommandTimeout {
        /// Welk commando.
        what: &'static str,
    },
    /// Enable Slot gaf een slot waar software geen structuren voor heeft.
    SlotNoResources {
        /// Het gemelde slot.
        slot: u8,
    },
    /// Enable Slot gaf een slot dat software al bezet of in quarantaine
    /// heeft.
    SlotBusy {
        /// Het gemelde slot.
        slot: u8,
    },
    /// Enable Slot gaf slot 0: dat kan niet veilig gedisabled worden.
    SlotZero,
    /// Enable Slot gaf een slot buiten CONFIG, en Disable Slot daarop werd
    /// niet bevestigd.
    SlotCleanup {
        /// Het gemelde slot.
        slot: u8,
        /// CONFIG.
        n_slots: u8,
    },
    /// Disable Slot werd niet bevestigd.
    DisableUnconfirmed {
        /// Het slot.
        slot: u8,
    },
    /// Software probeerde een slot vrij te geven dat het niet kent.
    UnknownRelease {
        /// Het slot.
        slot: usize,
    },
    /// De controller meldde meer resterende bytes dan de transfer lang was.
    Overrun {
        /// Welke transfer.
        what: &'static str,
        /// De gemelde rest.
        rem: u32,
    },
    /// HCHalted ging na RUN niet uit.
    RunTimeout,
    /// HCHalted kwam na Stop niet.
    HaltTimeout,
    /// De reset van een herstelpoging faalde.
    RecoveryReset,
    /// De start van een herstelpoging faalde.
    RecoveryStart,
}

impl fmt::Display for Poison {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::CommandTimeout { what } => write!(f, "no completion for {what}"),
            Self::SlotNoResources { slot } => {
                write!(f, "Enable Slot gave slot {slot} without software resources")
            }
            Self::SlotBusy { slot } => {
                write!(f, "Enable Slot gave busy or quarantined slot {slot}")
            }
            Self::SlotZero => f.write_str("Enable Slot gave slot 0, which cannot be disabled"),
            Self::SlotCleanup { slot, n_slots } => write!(
                f,
                "Enable Slot gave slot {slot} (CONFIG {n_slots}) and Disable Slot failed"
            ),
            Self::DisableUnconfirmed { slot } => {
                write!(f, "Disable Slot {slot} not confirmed")
            }
            Self::UnknownRelease { slot } => write!(f, "release of unknown slot {slot}"),
            Self::Overrun { what, rem } => {
                write!(
                    f,
                    "{what} completion reports {rem} bytes left, over its length"
                )
            }
            Self::RunTimeout => f.write_str("controller did not leave halt after RUN"),
            Self::HaltTimeout => f.write_str("controller did not halt after STOP"),
            Self::RecoveryReset => f.write_str("recovery reset failed"),
            Self::RecoveryStart => f.write_str("recovery start failed"),
        }
    }
}

/// Waarom de driver weigert.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// CAPLENGTH is onmogelijk: geen controller op dit adres (0 = niet
    /// geklokt, 0xFF.. = dode bus).
    NoController {
        /// Het venster.
        base: u64,
        /// Het rauwe eerste woord.
        raw: u32,
    },
    /// HCSPARAMS1 meldt nul poorten of nul slots.
    Implausible {
        /// HCSPARAMS1.
        hcsparams1: u32,
    },
    /// Een aanroep vóór [`Hc::probe`].
    NotProbed,
    /// Een aanroep die een draaiende controller vraagt, vóór [`Hc::start`].
    NotRunning,
    /// [`Hc::start`] op een controller die al loopt.
    Running,
    /// De DMA-regio is leeg of loopt om.
    Dma {
        /// De basis.
        base: u64,
        /// De maat.
        size: u64,
    },
    /// De DMA-regio is vol.
    DmaFull {
        /// Gevraagd.
        want: u64,
        /// Over.
        left: u64,
    },
    /// De paginagrootte van de controller valt buiten wat deze driver plant.
    PageSize {
        /// PAGESIZE.
        raw: u32,
    },
    /// De controller is 32-bit maar de DMA-regio eindigt daarboven.
    Dma32 {
        /// Het bus-einde van de regio.
        end: u64,
    },
    /// De controller vraagt meer scratchpad-buffers dan [`SCRATCH_MAX`].
    Scratchpad {
        /// Het aantal.
        n: u32,
        /// HCSPARAMS2.
        hcsparams2: u32,
    },
    /// Een registerwacht haalde zijn grens niet.
    RegTimeout {
        /// Waarop.
        what: &'static str,
        /// De laatste waarde.
        value: u32,
        /// Het masker.
        mask: u32,
        /// Wat we wilden.
        want: u32,
    },
    /// Een verwacht event kwam niet binnen de tijd. Bij een transfer is dat
    /// een apparaat dat hapert, niet de controller.
    EventTimeout {
        /// Waarop.
        what: &'static str,
        /// USBSTS op dat moment.
        usbsts: u32,
    },
    /// De controller wees een commando af.
    Rejected {
        /// Welk commando.
        what: &'static str,
        /// De completion code.
        code: u32,
    },
    /// Een transfer eindigde met een completion code die geen succes is.
    Transfer {
        /// Welke fase of soort.
        what: &'static str,
        /// De completion code.
        code: u32,
    },
    /// Slot-ownership is onbekend; een controllerreset is vereist.
    Poisoned(Poison),
    /// De poort kwam na de reset niet enabled terug.
    PortNotEnabled {
        /// De poort.
        port: u8,
        /// PORTSC.
        portsc: u32,
    },
    /// Het apparaat raakte tijdens de reset los.
    PortLost {
        /// De poort.
        port: u8,
        /// PORTSC.
        portsc: u32,
    },
    /// De poortreset werd niet klaar.
    PortResetTimeout {
        /// De poort.
        port: u8,
        /// PORTSC.
        portsc: u32,
    },
    /// Een control transfer groter dan de buffer van het slot.
    ControlTooLong {
        /// De gevraagde lengte.
        len: u16,
    },
    /// Een descriptor kwam korter binnen dan nodig.
    Descriptor {
        /// Welke.
        what: &'static str,
        /// Hoeveel bytes er kwamen.
        got: usize,
    },
    /// De configuratiedescriptor meldt een onmogelijke lengte.
    ConfigLength {
        /// wTotalLength.
        total: u16,
    },
    /// bMaxPacketSize0 past niet bij de snelheid.
    Mps0 {
        /// De snelheid.
        speed: Speed,
        /// De gemelde waarde.
        value: u8,
    },
    /// Enable Slot gaf een slot buiten CONFIG; het is bevestigd gedisabled
    /// en de controller blijft bruikbaar.
    SlotOutOfRange {
        /// Het slot.
        slot: u8,
        /// CONFIG.
        n_slots: u8,
    },
    /// Het apparaat heeft geen slot meer (losgekoppeld, of de controller
    /// herstelde zich).
    Detached,
    /// Het apparaat heeft geen bulk-endpoints.
    NoBulk,
    /// Een lege bulk-transfer.
    EmptyTransfer,
    /// Een bulk-transfer groter dan de bouncebuffer.
    TooLarge {
        /// De gevraagde lengte.
        len: usize,
        /// De buffer.
        max: usize,
    },
    /// Een gestalde endpoint die weer vrij is. Alleen de BOT-laag weet wat
    /// dat betekent: status ophalen en de fout van de drive zelf lezen.
    Stalled,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::NoController { base, raw } => write!(
                f,
                "xhci: CAPLENGTH {:#x} in raw word {raw:#010x}: no controller at {base:#x} \
                 (0 = not clocked, 0xFF.. = dead bus)",
                raw & 0xFF
            ),
            Self::Implausible { hcsparams1 } => write!(
                f,
                "xhci: {} ports / {} slots, HCSPARAMS1 {hcsparams1:#010x} is implausible",
                hcsparams1 >> 24,
                hcsparams1 & 0xFF
            ),
            Self::NotProbed => f.write_str("xhci: called before probe"),
            Self::NotRunning => f.write_str("xhci: called before start"),
            Self::Running => f.write_str("xhci: start requires a halted controller"),
            Self::Dma { base, size } => {
                write!(
                    f,
                    "xhci: DMA region {size:#x} at {base:#x} is empty or wraps"
                )
            }
            Self::DmaFull { want, left } => {
                write!(f, "xhci: DMA region full ({want} bytes asked, {left} left)")
            }
            Self::PageSize { raw } => {
                write!(f, "xhci: PAGESIZE {raw:#x} outside what this driver plans")
            }
            Self::Dma32 { end } => {
                write!(
                    f,
                    "xhci: controller is 32-bit but the DMA region ends at {end:#x}"
                )
            }
            Self::Scratchpad { n, hcsparams2 } => write!(
                f,
                "xhci: {n} scratchpad buffers asked (HCSPARAMS2 {hcsparams2:#010x}), limit {SCRATCH_MAX}"
            ),
            Self::RegTimeout {
                what,
                value,
                mask,
                want,
            } => write!(
                f,
                "xhci: timeout on {what} (reg = {value:#010x}, mask {mask:#x} wants {want:#x})"
            ),
            Self::EventTimeout { what, usbsts } => {
                write!(f, "xhci: no answer to {what} (USBSTS {usbsts:#010x})")
            }
            Self::Rejected { what, code } => {
                write!(f, "xhci: {what} rejected: {} ({code})", comp_name(code))
            }
            Self::Transfer { what, code } => {
                write!(f, "xhci: {what}: {} ({code})", comp_name(code))
            }
            Self::Poisoned(p) => {
                write!(
                    f,
                    "xhci: slot ownership unknown, controller reset required: {p}"
                )
            }
            Self::PortNotEnabled { port, portsc } => {
                write!(
                    f,
                    "xhci: port {port} not enabled after reset (PORTSC {portsc:#010x})"
                )
            }
            Self::PortLost { port, portsc } => {
                write!(
                    f,
                    "xhci: port {port} lost during reset (PORTSC {portsc:#010x})"
                )
            }
            Self::PortResetTimeout { port, portsc } => {
                write!(
                    f,
                    "xhci: port {port} reset not done (PORTSC {portsc:#010x})"
                )
            }
            Self::ControlTooLong { len } => {
                write!(
                    f,
                    "xhci: control transfer of {len} bytes exceeds the buffer"
                )
            }
            Self::Descriptor { what, got } => write!(f, "xhci: {what}: got {got} bytes"),
            Self::ConfigLength { total } => {
                write!(f, "xhci: config descriptor reports {total} bytes")
            }
            Self::Mps0 { speed, value } => {
                write!(f, "xhci: invalid EP0 packet size {value} for {speed}")
            }
            Self::SlotOutOfRange { slot, n_slots } => write!(
                f,
                "xhci: controller gave slot {slot} after Enable Slot (CONFIG {n_slots}); confirmed disabled"
            ),
            Self::Detached => f.write_str("usb: device is detached"),
            Self::NoBulk => f.write_str("usb: this device has no bulk endpoints"),
            Self::EmptyTransfer => f.write_str("usb: empty bulk transfer"),
            Self::TooLarge { len, max } => {
                write!(f, "usb: {len} bytes exceed the {max}-byte transfer buffer")
            }
            Self::Stalled => f.write_str("usb: endpoint stalled"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén hostcontroller.
pub struct Hc {
    /// Het begin van het MMIO-venster (de capability-registers). Op de
    /// Radxa een vast SoC-adres; op de Pi's een PCIe-BAR.
    base: Pa,
    /// Wat er in de log verschijnt: een node met drie controllers moet te
    /// lezen zijn.
    name: &'static str,
    /// Wat je bij een CPU-fysiek adres moet optellen om te krijgen wat de
    /// CONTROLLER als adres ziet. Nul op een SoC waar de xHCI direct op de
    /// geheugenbus hangt (Radxa); niet nul achter een PCIe-root-complex dat
    /// een inbound-window verschuift (de RP1 op de Pi 5: 0x10_0000_0000).
    ///
    /// Dit staat expliciet in het type en niet stil op nul, omdat een
    /// verkeerde waarde hier geen foutmelding geeft maar DMA naar het
    /// verkeerde stuk DRAM: het soort fout dat zich als willekeurige
    /// corruptie voordoet.
    bus_off: u64,

    // Gevuld door `probe`.
    probed: bool,
    op: Pa,
    db: Pa,
    rt: Pa,
    max_slots: u8,
    max_ports: u8,
    ctx64: bool,
    ac64: bool,
    ver: u16,

    // Gevuld door `start` (zie host.rs).
    arena: Arena,
    /// Vast board-venster; bewaard voor herstel na HCRST.
    dma_base: Pa,
    dma_size: u64,
    /// Paginagrootte van de controller (PAGESIZE-register).
    page: u64,
    /// Hoeveel slots we in CONFIG aanzetten.
    n_slots: usize,
    /// 32 of 64.
    ctx_size: u64,
    scratch: u32,
    dcbaa: Pa,
    /// 1-gebaseerd; `[0]` blijft leeg.
    res: [Option<SlotRes>; MAX_DEVICES + 1],
    cmd: Option<Ring>,
    evt: Option<EvRing>,
    /// De gedeelde bouncebuffer voor bulk-transfers (bulk.rs). Maat nul als
    /// er na de vaste structuren niets meer over was: dan draagt deze
    /// controller alleen HID en weigert elke bulk-transfer.
    bulk_buf: Pa,
    bulk_size: u64,
    pending: bounded::BoundedVec<ring::Event, PENDING_CAP>,
    running: bool,
    /// De volgende generatie voor een geclaimd slot: een handvat van vóór
    /// een herplug of een herstel past nooit op het slot van nu.
    next_gen: u32,

    /// Hardware- en software-ownership lopen niet meer bewezen gelijk
    /// (bijvoorbeeld Disable Slot zonder bevestiging). Nieuwe Enable
    /// Slot-opdrachten zijn dan verboden: alleen een geslaagde
    /// controllerreset maakt alle hardware-slots aantoonbaar vrij en wist
    /// deze toestand. `usbin` ziet dit via [`Hc::recovery_needed`] en
    /// herbouwt de controller met hetzelfde DMA-venster.
    poisoned: Option<Poison>,
}

impl Hc {
    /// Een controller op `base`, nog onaangeraakt: deze functie doet geen
    /// enkele registertoegang. De klok en de slaap komen per aanroep mee
    /// ([`Timer`]).
    ///
    /// # Safety
    ///
    /// `base` is het capability-venster van een xHCI-controller (of, voor
    /// een probe, een adres dat leesbaar gemapt is als Device), gemapt voor
    /// het hele venster (capability-, operational-, runtime- en
    /// doorbell-registers) en voor altijd. Elke DMA-regio die later aan
    /// [`Hc::start`] gaat, is gemapt geheugen dat alleen deze controller
    /// gebruikt.
    #[must_use]
    pub unsafe fn new(base: Pa, name: &'static str, bus_off: u64) -> Self {
        Self::at(base, name, bus_off)
    }

    /// Een controller zonder venster: elke hardwarestap weigert
    /// ([`Hc::probe`] geeft [`Error::NoController`]), en zonder probe raakt
    /// geen enkele functie een register. Voor de host-tests van de eigenaar
    /// (`usbin`) en voor een board zonder USB.
    #[must_use]
    pub fn unbound(name: &'static str) -> Self {
        Self::at(Pa(0), name, 0)
    }

    /// De staat zonder één registertoegang (ook voor de tests).
    fn at(base: Pa, name: &'static str, bus_off: u64) -> Self {
        Self {
            base,
            name,
            bus_off,
            probed: false,
            op: Pa(0),
            db: Pa(0),
            rt: Pa(0),
            max_slots: 0,
            max_ports: 0,
            ctx64: false,
            ac64: false,
            ver: 0,
            arena: Arena::default(),
            dma_base: Pa(0),
            dma_size: 0,
            page: 4096,
            n_slots: 0,
            ctx_size: 32,
            scratch: 0,
            dcbaa: Pa(0),
            res: [const { None }; MAX_DEVICES + 1],
            cmd: None,
            evt: None,
            bulk_buf: Pa(0),
            bulk_size: 0,
            pending: bounded::BoundedVec::new(),
            running: false,
            next_gen: 1,
            poisoned: None,
        }
    }

    /// De naam uit de logregels.
    #[must_use]
    pub fn name(&self) -> &'static str {
        self.name
    }

    fn cap(&self) -> &'static CapRegs {
        // SAFETY: de voorwaarde van `new`: `base` is het gemapte
        // capability-venster.
        unsafe { dev::regs(self.base) }
    }

    fn opr(&self) -> &'static OpRegs {
        debug_assert!(self.probed);
        // SAFETY: `op` is `base + CAPLENGTH`, door `probe` begrensd op
        // 0x20..=0x80 en dus binnen het venster van `new`.
        unsafe { dev::regs(self.op) }
    }

    fn port_regs(&self, n: u8) -> &'static PortRegs {
        debug_assert!(self.probed && n >= 1 && n <= self.max_ports);
        let pa = self
            .op
            .add(OP_PORT_BASE + u64::from(n.saturating_sub(1)) * OP_PORT_STRIDE);
        // SAFETY: `n` ligt in 1..=MaxPorts (HCSPARAMS1), en de spec legt
        // zoveel poortregistersets in het operational-blok van het venster.
        unsafe { dev::regs(pa) }
    }

    fn ir(&self) -> &'static IrRegs {
        // SAFETY: interrupter 0 ligt op `rt + 0x20`; `rt` komt uit RTSOFF
        // en ligt binnen het venster van `new`.
        unsafe { dev::regs(self.rt.add(RT_IR0)) }
    }

    /// Leest de capability-registers en vult de afgeleide adressen in. Raakt
    /// de controller verder NIET aan: dit is de goedkoopste manier om te
    /// weten of er überhaupt een xHCI achter dit adres zit, en het is het
    /// eerste dat op ijzer stukgaat als een klok of een power-domein niet
    /// aanstaat.
    ///
    /// Een venster dat niets terugpraat leest als 0x00000000 of 0xFFFFFFFF;
    /// allebei zijn onmogelijke CAPLENGTH-waarden, dus die vangen we hier af
    /// in plaats van verderop op een onzinnig offset te gaan schrijven.
    pub fn probe(&mut self) -> Result {
        if self.base.0 == 0 {
            // INVARIANT: een ongebonden controller (`unbound`) wordt nooit
            // geprobed, dus geen enkel registerpad wordt ooit open.
            return Err(Error::NoController { base: 0, raw: 0 });
        }
        let c = self.cap();
        let raw = c.caplength.read();
        let length = raw & 0xFF;
        if !(0x20..=0x80).contains(&length) {
            return Err(Error::NoController {
                base: self.base.0,
                raw,
            });
        }
        self.ver = (raw >> 16) as u16;
        let p1 = c.hcsparams1.read();
        let cp1 = c.hccparams1.read();
        let db = c.dboff.read() & !0x3;
        let rt = c.rtsoff.read() & !0x1F;
        self.max_slots = (p1 & 0xFF) as u8;
        self.max_ports = (p1 >> 24) as u8;
        if self.max_ports == 0 || self.max_slots == 0 {
            return Err(Error::Implausible { hcsparams1: p1 });
        }
        self.ac64 = cp1 & (1 << 0) != 0;
        self.ctx64 = cp1 & (1 << 2) != 0;
        self.op = self.base.add(u64::from(length));
        self.db = self.base.add(u64::from(db));
        self.rt = self.base.add(u64::from(rt));
        self.probed = true;
        Ok(())
    }

    /// Wat [`Hc::probe`] vond, voor één logregel: versie (BCD), slots,
    /// poorten en of de contexten 64 byte zijn.
    #[must_use]
    pub fn info(&self) -> (u16, u8, u8, bool) {
        (self.ver, self.max_slots, self.max_ports, self.ctx64)
    }

    /// Haalt de controller uit welke staat de bootloader hem ook achterliet
    /// en zet hem gehalteerd klaar.
    ///
    /// De volgorde is niet vrij (xHCI 4.2). Eerst STOPPEN en op HCHalted
    /// wachten: een HCRST terwijl de controller loopt is undefined
    /// behaviour, en U-Boot heeft hier net nog USB-apparaten gescand, dus
    /// hij lóópt. Pas daarna reset, en dan wachten tot zowel HCRST als CNR
    /// (Controller Not Ready) weg zijn: CNR is het bit dat zegt dat de
    /// interne staat nog niet bruikbaar is, en erop schrijven vóór die tijd
    /// wordt genegeerd of hangt de bus.
    pub async fn reset(&mut self, t: &impl Timer) -> Result {
        if !self.probed {
            return Err(Error::NotProbed);
        }
        let o = self.opr();
        let cmd = o.usbcmd.read();
        if cmd & CMD_RUN != 0 {
            o.usbcmd.write(cmd & !CMD_RUN);
        }
        self.wait(t, |o| o.usbsts.read(), STS_HCH, STS_HCH, "halt")
            .await?;
        o.usbcmd.update(|v| v | CMD_HCRST);
        dev::mb();
        self.wait(t, |o| o.usbcmd.read(), CMD_HCRST, 0, "HCRST clear")
            .await?;
        self.wait(t, |o| o.usbsts.read(), STS_CNR, 0, "controller ready")
            .await?;
        // INVARIANT: HCRST is bevestigd; geen hardware-slot is nog bezet.
        self.poisoned = None;
        self.running = false;
        Ok(())
    }

    /// Het aantal roothub-poorten (1-gebaseerd genummerd).
    #[must_use]
    pub fn num_ports(&self) -> u8 {
        if self.probed { self.max_ports } else { 0 }
    }

    /// Leest één poort uit. Puur lezen, geen reset, geen power: veilig als
    /// eerste ding dat je op ijzer draait.
    #[must_use]
    pub fn port(&self, n: u8) -> Port {
        if !self.probed || n == 0 || n > self.max_ports {
            return Port {
                num: n,
                ..Port::default()
            };
        }
        let v = self.port_regs(n).portsc.read();
        Port {
            num: n,
            connected: v & PSC_CCS != 0,
            enabled: v & PSC_PED != 0,
            speed: Speed(((v >> PSC_SPEED_SHIFT) & PSC_SPEED_MASK) as u8),
            raw: v,
        }
    }

    /// Zet PP op alle poorten die het nog niet hebben en wacht de
    /// debounce-tijd. Sommige controllers komen met de poortvoeding uit uit
    /// reset, en dan meldt een aangesloten toetsenbord zich nooit: CCS
    /// blijft 0 en je zoekt op de verkeerde plek.
    pub async fn power_on(&mut self, t: &impl Timer) {
        if !self.probed {
            return;
        }
        for n in 1..=self.max_ports {
            let v = self.port_regs(n).portsc.read();
            if v & PSC_PP == 0 {
                self.port_write(n, v | PSC_PP);
            }
        }
        t.sleep(POWER_SETTLE_NS).await;
    }

    /// Wist de w1c-statusbits van een poort. Nodig vóór je op een
    /// verandering gaat wachten: blijft er een oude change-bit staan, dan
    /// lees je die aan voor de nieuwe.
    ///
    /// Wissen doe je door er een 1 náár te schrijven: dus deze functie
    /// schrijft de change-bits juist WEL mee, precies andersom dan
    /// `port_write`. Dat is het hele verschil tussen de twee, en het is de
    /// reden dat ze allebei bestaan: één schrijfpad dat de statusbits met
    /// rust laat, en één dat ze wist. Wie ze samenvoegt krijgt een functie
    /// die het ene geval altijd verkeerd doet.
    pub fn clear_changes(&mut self, n: u8) {
        if !self.probed || n == 0 || n > self.max_ports {
            return;
        }
        let r = self.port_regs(n);
        let v = r.portsc.read();
        if v & PSC_CHANGE_MASK == 0 {
            return;
        }
        r.portsc.write(v & !PSC_ACTION_MASK);
        dev::mb();
    }

    /// Schrijft PORTSC mét één actiebit erbij (PR of WPR). Alles wat een
    /// stand beschrijft blijft staan, de w1c-bits en de ándere actiebits
    /// gaan eruit.
    fn port_action(&self, n: u8, action: u32) {
        let r = self.port_regs(n);
        let v = r.portsc.read();
        r.portsc
            .write(v & !(PSC_CHANGE_MASK | PSC_ACTION_MASK) | action);
        dev::mb();
    }

    /// Schrijft PORTSC veilig: de write-1-to-clear-statusbits worden
    /// uitgemaskeerd (anders wist een gewone RMW ze) en PED wordt
    /// uitgemaskeerd (want een 1 daar ZET de poort uit in plaats van hem aan
    /// te houden). Dit is de enige plek in deze driver die PORTSC gewoon
    /// schrijft, precies omdat die twee vallen bij een read-modify-write
    /// allebei stil zijn.
    fn port_write(&self, n: u8, v: u32) {
        self.port_regs(n)
            .portsc
            .write(v & !(PSC_CHANGE_MASK | PSC_ACTION_MASK));
        dev::mb();
    }

    /// Pollt een operational-register tot `(waarde & mask) == want`, en
    /// slaapt [`POLL_STEP_NS`] tussen twee blikken.
    async fn wait(
        &self,
        t: &impl Timer,
        read: impl Fn(&OpRegs) -> u32,
        mask: u32,
        want: u32,
        what: &'static str,
    ) -> Result {
        let o = self.opr();
        let deadline = t.now().saturating_add(REG_TIMEOUT_NS);
        loop {
            let v = read(o);
            if v & mask == want {
                return Ok(());
            }
            if t.now() >= deadline {
                return Err(Error::RegTimeout {
                    what,
                    value: v,
                    mask,
                    want,
                });
            }
            t.sleep(POLL_STEP_NS).await;
        }
    }

    /// De ownership-fout die een volledige controllerreset vereist, of
    /// `None` als [`Hc::attach`] veilig verder mag. `usbin` controleert dit
    /// vóór elke scanronde; zo is quarantaine een bereikbaar herstelpad en
    /// geen reboot-slot.
    #[must_use]
    pub fn recovery_needed(&self) -> Option<Poison> {
        self.poisoned
    }

    /// Een momentopname van de registers en ringen bij een mislukte
    /// enumeratie, vóór het herstel: USBSTS, CRCR, het busadres van de
    /// command ring en de stand van de event ring. Controllerstand, nooit
    /// apparaatdata.
    pub fn diagnostic(&self) -> [u64; 4] {
        if !self.probed {
            return [0; 4];
        }
        let o = self.opr();
        [
            u64::from(o.usbsts.read()),
            o.crcr.read(),
            self.cmd.map_or(0, |r| r.bus),
            self.evt
                .map_or(0, |r| u64::from(dev::read32(r.base.add(12)))),
        ]
    }
}

#[cfg(test)]
mod tests;
