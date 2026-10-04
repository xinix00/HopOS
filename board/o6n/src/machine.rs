//! De O6N als [`Board`]: het UEFI-board met de O6N-drivers eroverheen. Wat
//! niet anders is dan op elke UEFI-machine (console, heap, klok, slaap,
//! GIC, plan), gaat door naar [`Uefi`].

use crate::class::{self, CoreFacts};
use crate::clock::{self, CpcKnob};
use crate::cpc::{self, Cpc, MAX_CPCS};
use crate::probe;
use crate::thermal::{self, SCMI_CHANNEL, Thermo};
use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, Plan};
use board_uefi::{BLK_DATA, BLK_DMA, NET_BUF, NET_DMA, Uefi, pcie};

// Het cacheable datablok van de stub is precies dat van de driver.
const _: () = assert!(
    BLK_DATA.base.0 == BLK_DMA.base.0 + driver_nvme::DATA_OFF
        && BLK_DATA.size == driver_nvme::DATA_SIZE
);
// Idem het bufferblok van de NIC (`net-wb`): de frames van de rtl8126 en
// niets anders; zijn ringen liggen ervoor en blijven NC.
const _: () = assert!(
    NET_BUF.base.0 == NET_DMA.base.0 + driver_rtl8126::BUF_OFF
        && driver_rtl8126::DMA_NEED <= NET_BUF.end().0 - NET_DMA.base.0
);
use bounded::BoundedVec;
use core::cell::{Cell, RefCell};
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use dev::Pa;
use driver_nvme::{Nvme, pci::Pci};
use driver_rtl8126::Rtl8126;
use driver_scmi::{Channel, Sensor};
use sync::{Local, Signal};

/// Hoe lang de Realtek op een link wacht: NBASE-T-autonegotiatie kan
/// seconden duren (het Go-board gaf 12 s, ruimer dan de igb).
pub const LINK_TIMEOUT_NS: u64 = 12_000_000_000;

/// De linkwacht van een tweede probe (hopos `nic_retry`). Die draait met de
/// SBSA-watchdog gewapend, en die valt op de 1 GHz-teller van de O6N op
/// 8,6 s (`board_uefi::watchdog`); de wacht spint de executor stil, dus
/// moet hij eronder blijven. Een autonegotiatie die langer duurt, haalt
/// de retry niet (elke poging herstart hem); de koude boot wacht 12 s.
const RETRY_LINK_NS: u64 = 7_000_000_000;

/// Is `probe_nic` al eens gelopen? Dan is dit de retry ([`RETRY_LINK_NS`]).
static NIC_TRIED: AtomicBool = AtomicBool::new(false);

/// Leeft er een NIC uit `probe_nic`? Pas gezet na een gelukte probe: een
/// mislukte (geen link) liet niets achter en mag opnieuw (hopos `nic_retry`).
static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);
static DISK_CLAIMED: AtomicBool = AtomicBool::new(false);

/// De thermometer: `None` tot de eerste vraag, daarna het kanaal of niets.
/// Alleen de executor van core 0 raakt hem aan (de heartbeat en de
/// telemetrie), en de lening loopt nooit over een `.await`.
static THERMO: Local<RefCell<Option<Option<Thermo>>>> = Local::new(RefCell::new(None));

/// De O6N.
pub struct O6n {
    uefi: Uefi,
}

impl O6n {
    /// Het board. Alle staat staat in statics; dit is het handvat.
    #[must_use]
    pub const fn new() -> Self {
        Self { uefi: Uefi::new() }
    }

    /// Vindt en initialiseert de NVMe (de eerste, het hele device) in de
    /// schijf-helft van de DMA-regio. `Ok(None)` = geen NVMe; één keer.
    /// Geen methode van [`Board`], net als op virt: het blokcontract is van
    /// hopfs, en de binary kent haar board concreet.
    pub fn probe_disk(&self) -> Result<Option<Nvme<Pci>>, Error> {
        if DISK_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_disk"));
        }
        pcie::probe_nvme()
    }

    /// Is dit werkelijk een Cix P1 (de XSDT-OEM-ID)? Het image draait ook
    /// op andere UEFI-machines (de Ampere, 19-09); mailbox- en
    /// fastchannel-adressen zijn alleen hier van ons.
    #[must_use]
    pub fn is_cix(&self) -> bool {
        self.uefi.oem_id() == crate::OEM_ID
    }

    /// De klokknop uit de `_CPC`'s van de DSDT en de SSDT's: één
    /// desired-perf-woord per domein, het plafond geklemd op `mhz` als die
    /// gegeven is (`hopos.mhz`). Of waarom niet.
    pub fn clock_knob(&self, mhz: Option<u32>) -> Result<CpcKnob, &'static str> {
        if !self.is_cix() {
            return Err("not a Cix P1 (OEM ID), no _CPC fastchannels");
        }
        let mut cpcs: BoundedVec<Cpc, MAX_CPCS> = BoundedVec::new();
        for t in self
            .uefi
            .acpi_tables(b"DSDT")
            .chain(self.uefi.acpi_tables(b"SSDT"))
        {
            cpc::scan(t, &mut cpcs);
        }
        let mut ds = clock::domains(cpcs.as_slice());
        if ds.is_empty() {
            return Err("no _CPC with a 32-bit desired-perf register");
        }
        if let Some(m) = mhz {
            clock::cap(ds.as_mut_slice(), m);
        }
        if !ds
            .as_slice()
            .iter()
            .all(|d| board_uefi::map_device(d.reg.0, 4))
        {
            return Err("a desired-perf register is unreachable");
        }
        // SAFETY: elk register komt uit de `_CPC` van deze firmware (de
        // OEM-toets hierboven) en is Device-gemapt (`map_device`); alleen
        // deze knop schrijft erin.
        Ok(unsafe { CpcKnob::new(ds) })
    }

    /// De heetste CPU-sensor van de SCP in milligraden, 0 = geen meting.
    /// De eerste vraag opent het SCMI-kanaal en kiest de sensoren (één
    /// regel op de console); daarna hoogstens één SCMI-ronde per seconde.
    /// Alleen op een Cix P1: elders is het SCMI-adres niet van ons.
    #[must_use]
    pub fn temp_milli_c(&self) -> i32 {
        if !self.is_cix() {
            return 0;
        }
        let mut t = THERMO.get().borrow_mut();
        let now = cpu::idle::now();
        t.get_or_insert_with(open_thermo)
            .as_mut()
            .map_or(0, |th| th.milli_c(now))
    }

    /// De klasse-indeling in één regel, voor de bootlog: de plaatsing moet
    /// kunnen zeggen waar ze op leunt.
    fn describe_classes(&self) {
        let n = self.cores();
        let (mut small, mut mid, mut big) = (0, 0, 0);
        for c in 1..n {
            match self.core_class(c) {
                CoreClass::Small => small += 1,
                CoreClass::Mid => mid += 1,
                CoreClass::Big => big += 1,
            }
        }
        cpu::println!(
            "o6n: {} app cores, classes from {:?} - small {small}, mid {mid}, big {big}",
            n.saturating_sub(1),
            self.classes().source
        );
    }

    /// De bronnen van de klasse-indeling: de MADT (via `board-uefi`), anders
    /// de vaste MPIDR-tabel. De `_CPC`-bron vraagt de DSDT, en die geeft
    /// `board-uefi` (nog) niet door.
    fn classes(&self) -> class::Classes {
        let n = self.cores().min(16);
        let mut facts: BoundedVec<CoreFacts, 16> = BoundedVec::new();
        for c in 0..n {
            // De MADT-klasse zoals `board-uefi` hem rangschikt, als getal.
            let eff = match self.uefi.core_class(c) {
                CoreClass::Small => 0,
                CoreClass::Mid => 1,
                CoreClass::Big => 2,
            };
            let _ = facts.push(CoreFacts {
                mpidr: board_uefi::slots::mpidr(c),
                eff,
                highest: 0,
            });
        }
        // De kern-core telt niet mee in de MADT-rangschikking; is er onder de
        // app-cores maar één klasse, dan is eff overal gelijk en valt de
        // keuze op de MPIDR-tabel. Die geldt alleen voor de twaalf cores van
        // de Cix P1 (het image is O6N-eigen).
        class::Classes::new(facts.as_slice(), n == 12)
    }
}

/// Opent het SCMI-kanaal van de DSDT en kiest de sensoren; `None` = geen
/// thermometer (één regel waarom).
fn open_thermo() -> Option<Thermo> {
    // SAFETY: 0x065d0000 is het SCMI-shmem-kanaal dat de DSDT van de Cix P1
    // zelf gebruikt; onder 1 TB mapt `board-uefi` alles als Device, en naast
    // de firmware-AML (die na ExitBootServices niet meer draait) schrijft
    // niemand erin.
    let mut ch = unsafe { Channel::new(Pa(SCMI_CHANNEL), cpu::idle::now) };
    if let Err(e) = ch.version(driver_scmi::proto::SENSOR) {
        cpu::println!("hwmon: SCMI sensor protocol not answering ({e}) - temperature off");
        return None;
    }
    let mut all = [Sensor::default(); 32];
    let n = ch.sensors(&mut all).unwrap_or(0);
    let picked = thermal::pick(all.get(..n).unwrap_or(&[]));
    if picked.is_empty() {
        cpu::println!("hwmon: SCMI lists {n} sensors, none in Celsius - temperature off");
        return None;
    }
    cpu::println!(
        "hwmon: {} SCMI sensors of {n} (first {}) - hottest goes on the heartbeat",
        picked.len(),
        picked.as_slice().first().map_or("?", Sensor::name)
    );
    Some(Thermo::new(ch, picked))
}

/// Wat de binary buiten [`Board`] om van het UEFI-board vraagt (de
/// core-0-lijm: `this_core`, `os_bell`, `kick_self`, `config`), is hier
/// hetzelfde; de eigen methodes van dit board ([`O6n::probe_disk`]) gaan
/// voor.
impl core::ops::Deref for O6n {
    type Target = Uefi;

    fn deref(&self) -> &Uefi {
        &self.uefi
    }
}

impl Default for O6n {
    fn default() -> Self {
        Self::new()
    }
}

impl Board for O6n {
    type Nic = Rtl8126;
    type Sleeper = <Uefi as Board>::Sleeper;

    const NAME: &'static str = "o6n";

    fn console(&self) -> fn(&[u8]) {
        self.uefi.console()
    }

    fn console_nowait(&self) -> Option<fn(&[u8]) -> usize> {
        self.uefi.console_nowait()
    }

    fn firmware(&self) -> &'static str {
        "boot: Radxa Orion O6N (Cix P1) over UEFI: PE stub, ACPI discovery, 48-bit identity map"
    }

    fn init_heap(&self, heap: &Heap) {
        self.uefi.init_heap(heap);
    }

    fn discover(&self, dtb: u64) {
        self.uefi.discover(dtb);
        self.describe_classes();
    }

    fn clock(&self) -> executor::Clock {
        self.uefi.clock()
    }

    fn sleeper(&self) -> Self::Sleeper {
        self.uefi.sleeper()
    }

    fn mem_total(&self) -> u64 {
        self.uefi.mem_total()
    }

    fn cores(&self) -> usize {
        self.uefi.cores()
    }

    fn core_class(&self, core: usize) -> CoreClass {
        let classes = self.classes();
        let facts = CoreFacts {
            mpidr: board_uefi::slots::mpidr(core),
            eff: match self.uefi.core_class(core) {
                CoreClass::Small => 0,
                CoreClass::Mid => 1,
                CoreClass::Big => 2,
            },
            highest: 0,
        };
        classes.of(&facts)
    }

    fn plan(&self) -> Plan {
        self.uefi.plan()
    }

    fn start_interrupts(&self) -> Result<&'static Signal, Error> {
        self.uefi.start_interrupts()
    }

    fn dispatch_interrupts(&self) -> Dispatched {
        self.uefi.dispatch_interrupts()
    }

    /// De GOP van de eigen firmware, zoals het UEFI-board hem las (alleen
    /// in de gui-smaak; kaal `None`, docs/gui.md).
    fn framebuffer(&self) -> Option<board::fb::Desc> {
        board_uefi::gop_framebuffer()
    }

    /// De native xHCI's die de firmware aanzette (usb.rs; kaal geen).
    fn usb_hosts(&self) -> board::UsbHosts {
        crate::usb::hosts(&self.uefi)
    }

    /// De eerste Realtek-poort (geen twee-poorts-aggregatie): BAR2 (het
    /// MMIO-blok; BAR0 is de I/O-alias), reset en MAC, ringen en MAC aan,
    /// dan PHY en autoneg, en dan de lijn (`board_uefi::irq`): MSI-X via de
    /// ITS als de IORT de DeviceID kent, anders INTx uit de `_PRT` van de
    /// root-poort (bus 0x30: GSI 0x1dd, INTID 477), anders gepold.
    /// `hopos.nicirq` kiest anders (`msix`, `intx`, `off`, een INTID).
    ///
    /// De ack is voor beide dezelfde (`IrqAck::ack`: masker dicht, status
    /// schoon); de driver heropent het masker in `flush`. Bij INTx is dat
    /// de level-lijn laten vallen (de freeze van 17/18-09), bij MSI-X de
    /// voorwaarde voor een volgende flank.
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error> {
        if NIC_CLAIMED.load(Relaxed) {
            return Err(Error::Twice("probe_nic"));
        }
        let segs = pcie::segments();
        let Some(hit) = pcie::first_in(&segs, 2, |f| driver_rtl8126::supported(f.vendor, f.device))
        else {
            return Ok(None);
        };
        if !board_uefi::map_device(hit.bar, driver_rtl8126::MMIO_LEN) {
            return Err(Error::Nic("rtl8126 BAR2 unreachable"));
        }
        if let Some((e, _)) = segs.as_slice().iter().find(|(_, s)| *s == hit.root_bus) {
            hit.f.enable(e);
        }
        // SAFETY: BAR2 is door de firmware toegewezen en nu Device-gemapt;
        // memory-decode en bus-mastering staan aan. NET_DMA is van deze
        // driver alleen, 2 MB-gealigneerd en Normal-NC gemapt, met het
        // bufferblok Normal-WB (`NET_BUF`): dat veegt de driver zelf.
        let mut nic =
            unsafe { Rtl8126::new(Pa(hit.bar), NET_DMA.base, NET_DMA.size, cpu::idle::now) }
                .map_err(|e| {
                    cpu::println!("net: {e} at {} bar2 {:#x}", hit.f.bdf, hit.bar);
                    Error::Nic("rtl8126 init failed")
                })?;
        let wait = if NIC_TRIED.swap(true, Relaxed) {
            RETRY_LINK_NS
        } else {
            LINK_TIMEOUT_NS
        };
        let link = nic.link_up(wait).map_err(|e| {
            cpu::println!("net: {} {e}", nic_name(&nic));
            Error::Nic("rtl8126 has no link")
        })?;
        let wired = match segs.as_slice().iter().find(|(_, s)| *s == hit.root_bus) {
            Some((e, _)) => {
                NIC_ACK.get().set(Some(nic.irq_ack()));
                let at = board_uefi::irq::At {
                    ecam: e,
                    seg: seg_of(hit.root_bus),
                    root_bus: hit.root_bus,
                    f: &hit.f,
                };
                let mode = board_uefi::irq::nic_mode(board_uefi::irq::Mode::Auto);
                board_uefi::irq::wire(&at, mode, &NIC_BELL, rtl_ack, rtl_ack)
            }
            None => board_uefi::irq::Wired::Polled("no config window for the root bus"),
        };
        if !matches!(wired, board_uefi::irq::Wired::Polled(_)) {
            nic.set_irq(&NIC_BELL);
        }
        cpu::println!(
            "net: {} {:04x}:{:04x} at {} xid {:#x} link {link}, {wired} (the DT table said INTID {}) HOPOS_NIC_IRQ",
            nic_name(&nic),
            hit.f.vendor,
            hit.f.device,
            hit.f.bdf,
            nic.xid(),
            probe::nic_intid(hit.root_bus).unwrap_or(0)
        );
        NIC_CLAIMED.store(true, Relaxed);
        Ok(Some(nic))
    }
}

/// De bel van de NIC-lijn.
static NIC_BELL: Signal = Signal::new();

/// De ack van de Realtek (masker dicht, status schoon), voor de dispatch
/// op de kern-core; gezet vóór de lijn scherp gaat.
static NIC_ACK: Local<Cell<Option<driver_rtl8126::IrqAck>>> = Local::new(Cell::new(None));

/// De ack van de NIC-lijn, uit de dispatch vóór de EOI.
fn rtl_ack() {
    if let Some(a) = NIC_ACK.get().get() {
        let _ = a.ack();
    }
}

/// Het PCI-segment van het venster dat op `root_bus` begint (op de O6N
/// allemaal 0: vijf vensters, elk een eigen root-poort).
fn seg_of(root_bus: u8) -> u16 {
    board_uefi::pcie_segments()
        .find(|(_, _, start)| *start == root_bus)
        .map_or(0, |(_, seg, _)| seg)
}

fn nic_name(n: &Rtl8126) -> &'static str {
    n.name()
}
