//! De Altra als [`Board`]: het UEFI-board met de igb, de NVMe en de SMpro.
//! Wat niet anders is dan op elke UEFI-machine, gaat door naar [`Uefi`].

use crate::{LINK_TIMEOUT_NS, is_nic, nic_irq_mode};
use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, Plan};
use board_uefi::irq::{At, Mode, Wired};
use board_uefi::{NET_BUF, NET_DMA, Uefi, pcie};
use core::cell::{Cell, RefCell};
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use dev::Pa;
use driver_igb::{Igb, IrqAck};
use driver_nvme::{Nvme, pci::Pci};
use driver_smpro::{HWMON_CHANNEL, Smpro};
use sync::{Local, Signal};

// Het cacheable bufferblok van de stub (`net-wb`) is precies dat van de
// igb; zijn ringen liggen ervoor en blijven NC.
const _: () = assert!(
    NET_BUF.base.0 == NET_DMA.base.0 + driver_igb::BUF_OFF
        && driver_igb::DMA_NEED <= NET_BUF.end().0 - NET_DMA.base.0
);

/// Leeft er een NIC uit `probe_nic`? Pas gezet na een gelukte probe: een
/// mislukte (geen link) liet niets achter en mag opnieuw (hopos `nic_retry`).
static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);
static DISK_CLAIMED: AtomicBool = AtomicBool::new(false);

/// De bel van de NIC-lijn.
static NIC_BELL: Signal = Signal::new();

/// De ack van de igb (EIMC), voor de dispatch op de kern-core; gezet vóór
/// de lijn scherp gaat.
static NIC_ACK: Local<Cell<Option<IrqAck>>> = Local::new(Cell::new(None));

/// Hoe lang de zelftest op de afgevuurde vector wacht (zoals de tg3 op de
/// M4).
const NIC_TEST_NS: u64 = 50_000_000;

/// Wat een `INT` vanuit de ITS deed (de proef zonder device).
enum IntTest {
    /// De LPI kwam, na zoveel microseconden.
    Arrived(u64),
    /// Geen LPI binnen [`NIC_TEST_NS`].
    Silent,
    /// Het commando liep niet.
    Refused(&'static str),
}

impl core::fmt::Display for IntTest {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::Arrived(us) => write!(
                f,
                "arrived after {us} us (the ITS side works; the igb write does not reach it)"
            ),
            Self::Silent => f.write_str(
                "silent within 50 ms (the ITS, redistributor or collection side is broken)",
            ),
            Self::Refused(why) => write!(f, "not run: {why}"),
        }
    }
}

/// De ack van de NIC-lijn, uit de dispatch vóór de EOI.
fn igb_ack() {
    if let Some(a) = NIC_ACK.get().get() {
        a.ack();
    }
}

/// De SMpro, zodra iemand het PCC-kanaal gaf ([`Altra::open_hwmon`]).
/// Alleen de executor van core 0 raakt hem aan.
static HWMON: Local<RefCell<Option<Smpro>>> = Local::new(RefCell::new(None));

/// De Altra.
pub struct Altra {
    uefi: Uefi,
}

impl Altra {
    /// Het board. Alle staat staat in statics; dit is het handvat.
    #[must_use]
    pub const fn new() -> Self {
        Self { uefi: Uefi::new() }
    }

    /// De OS-core die `hopos.oscore` vraagt, met de klassen van dit board
    /// (homogeen, [`crate::core_class`]) en niet die van de MADT, zoals de
    /// O6N.
    #[must_use]
    pub fn os_core(&self) -> (usize, Option<&'static str>) {
        self.uefi.os_core_by(crate::core_class)
    }

    /// Vindt en initialiseert de eerste NVMe (het hele device) in de
    /// schijf-helft van de DMA-regio. `Ok(None)` = geen NVMe; één keer.
    pub fn probe_disk(&self) -> Result<Option<Nvme<Pci>>, Error> {
        if DISK_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_disk"));
        }
        pcie::probe_nvme()
    }

    /// Opent de SMpro op subkanaal [`HWMON_CHANNEL`] van de PCCT-tabel
    /// `pcct` (de bytes van de tabel) en doet één proeflees. Geen PCCT of
    /// geen antwoord: één regel en verder zonder; telemetrie is nooit een
    /// boot-blokker.
    pub fn open_hwmon(&self, pcct: &[u8]) {
        let Some(p) = driver_smpro::pcc_from(pcct, HWMON_CHANNEL) else {
            cpu::println!("hwmon: no PCC hwmon channel in the PCCT - temperature telemetry off");
            return;
        };
        // Shmem ligt in gereserveerd DRAM, de doorbell kan in hoge SoC-MMIO
        // wonen: zelfde luik als de hoge BAR's.
        if !board_uefi::map_device(p.shmem.0, p.shmem_len)
            || !board_uefi::map_device(p.doorbell.0, 8)
        {
            cpu::println!("hwmon: PCC channel unreachable - temperature telemetry off");
            return;
        }
        // SAFETY: shmem en doorbell komen uit de PCCT en zijn nu Device-
        // gemapt; alleen deze driver en de SMpro gebruiken kanaal 14.
        let mut d = unsafe { Smpro::new(HWMON_CHANNEL, p, cpu::idle::now) };
        match d.soc_temp_milli_c() {
            Some(m) => cpu::println!(
                "hwmon: SoC {}.{}C (SMpro, PCC channel {HWMON_CHANNEL})",
                m / 1000,
                (m % 1000) / 100
            ),
            None => {
                cpu::println!(
                    "hwmon: SMpro not answering on PCC channel {HWMON_CHANNEL} - temperature telemetry off"
                );
                return;
            }
        }
        *HWMON.get().borrow_mut() = Some(d);
    }

    /// De lijn van de igb: MSI-X vector 0 via de ITS (`board_uefi::irq`),
    /// en dan een zelftest: de driver vuurt de vector met de hand (EICS) en
    /// de dispatch moet hem binnen [`NIC_TEST_NS`] zien. Een route die niet
    /// klopt (de tabel, de DeviceID uit de IORT, de ITS) is stil; zonder
    /// zelftest zou de pomp dan op zijn vangrail van 10 ms leven, trager dan
    /// pollen. Nooit INTx: dat doodt deze SoC ([`nic_irq_mode`]). Geeft de
    /// lijn en de microseconden tot de eerste aflevering, of de reden om te
    /// pollen.
    fn wire_nic(
        &self,
        segs: &pcie::Segments,
        hit: &pcie::Found,
        nic: &mut Igb,
    ) -> Result<(Wired, u64), &'static str> {
        let mode = nic_irq_mode(board_uefi::irq::nic_mode(Mode::Msix))?;
        let (e, _) = segs.get(hit.win).ok_or("no config window for the NIC")?;
        // Het segment van het venster: `segments` en `pcie_segments` lopen
        // allebei de MCFG in volgorde af (op de Altra beginnen ze allemaal
        // op bus 0, dus de bus zegt het niet).
        let seg = board_uefi::pcie_segments()
            .nth(hit.win)
            .map_or(0, |(_, s, _)| s);
        NIC_ACK.get().set(Some(nic.irq_ack()));
        let at = At {
            ecam: e,
            seg,
            root_bus: hit.root_bus,
            f: &hit.f,
        };
        let wired = board_uefi::irq::wire(&at, mode, &NIC_BELL, igb_ack, igb_ack);
        if let Wired::Polled(why) = wired {
            return Err(why);
        }
        let _ = NIC_BELL.take();
        nic.set_irq(&NIC_BELL);
        nic.fire_irq();
        if let Some(us) = self.bell_within(NIC_TEST_NS) {
            // De ack sloot de vector; de eerste lege ring van de pomp
            // heropent hem.
            return Ok((wired, us));
        }
        if let Wired::Msix { dev_id, .. } = wired {
            self.diagnose(&at, dev_id, nic);
        }
        nic.clear_irq();
        Err("the forced interrupt (EICS) did not arrive within 50 ms")
    }

    /// Draait de dispatch tot de NIC-bel gaat (de microseconden tot dan)
    /// of `ns` verstreken is.
    fn bell_within(&self, ns: u64) -> Option<u64> {
        let t0 = cpu::idle::now();
        let rang = dev::poll_until(cpu::idle::now, ns, || {
            let _ = self.uefi.dispatch_interrupts();
            NIC_BELL.take()
        });
        rang.then(|| cpu::idle::now().saturating_sub(t0) / 1_000)
    }

    /// Eén regel na een zelftest die niet aankwam, om te kiezen tussen de
    /// verdachten: eerst wat de functie en de NIC zeggen (de PBA en EICR
    /// vóór iets ze verandert), de IORT-weg met de SMMU en de ITS, en dan
    /// een `INT` vanuit de ITS zelf. Komt die wel, dan is de ITS-kant goed
    /// en zit de fout tussen de igb en de ITS (doorbell, DeviceID, SMMU).
    fn diagnose(&self, at: &At<'_>, dev_id: u32, nic: &Igb) {
        let d = board_uefi::irq::msix_diag(at, dev_id);
        let regs = nic.irq_regs();
        let _ = NIC_BELL.take();
        let int = match board_uefi::irq::its_fire(dev_id) {
            Ok(()) => match self.bell_within(NIC_TEST_NS) {
                Some(us) => IntTest::Arrived(us),
                None => IntTest::Silent,
            },
            Err(why) => IntTest::Refused(why),
        };
        cpu::println!("net: igb MSI-X diag: {d}; igb {regs}; ITS INT {int} HOPOS_NIC_IRQ_DIAG");
    }

    /// De SoC-temperatuur in milligraden, 0 = geen meting.
    #[must_use]
    pub fn temp_milli_c(&self) -> i32 {
        HWMON
            .get()
            .borrow_mut()
            .as_mut()
            .and_then(Smpro::soc_temp_milli_c)
            .unwrap_or(0)
    }
}

/// Wat de binary buiten [`Board`] om van het UEFI-board vraagt (de
/// core-0-lijm: `this_core`, `os_bell`, `kick_self`, `config`), is hier
/// hetzelfde; de eigen methodes van dit board ([`Altra::probe_disk`]) gaan
/// voor.
impl core::ops::Deref for Altra {
    type Target = Uefi;

    fn deref(&self) -> &Uefi {
        &self.uefi
    }
}

impl Default for Altra {
    fn default() -> Self {
        Self::new()
    }
}

impl Board for Altra {
    type Nic = Igb;
    type Sleeper = <Uefi as Board>::Sleeper;

    const NAME: &'static str = "altra";

    fn console(&self) -> fn(&[u8]) {
        self.uefi.console()
    }

    fn console_nowait(&self) -> Option<fn(&[u8]) -> usize> {
        self.uefi.console_nowait()
    }

    /// De GOP van de eigen firmware, zoals het UEFI-board hem las (alleen
    /// in de gui-smaak; kaal `None`, docs/gui.md). Een Altra-server heeft
    /// meestal een BMC-VGA; de GOP daarvan is een gewone lineaire buffer.
    fn framebuffer(&self) -> Option<board::fb::Desc> {
        board_uefi::gop_framebuffer()
    }

    /// De xHCI's van de firmware, op klasse uit de MCFG-segmenten, zoals
    /// het generieke UEFI-board ze vindt (docs/gui.md).
    fn usb_hosts(&self) -> board::UsbHosts {
        self.uefi.usb_hosts()
    }

    fn firmware(&self) -> &'static str {
        "boot: Ampere Altra over UEFI: PE stub, ACPI discovery, 48-bit identity map"
    }

    fn init_heap(&self, heap: &Heap) {
        self.uefi.init_heap(heap);
    }

    fn discover(&self, dtb: u64) {
        self.uefi.discover(dtb);
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
        crate::core_class(core)
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

    /// De eerste igb: BAR0, reset en MAC, ringen, dan de link, en dan de
    /// lijn: MSI-X via de ITS met een zelftest, anders gepold
    /// ([`Altra::wire_nic`]; `hopos.nicirq=off` pollt zonder herbouw).
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error> {
        if NIC_CLAIMED.load(Relaxed) {
            return Err(Error::Twice("probe_nic"));
        }
        let segs = pcie::segments();
        let Some(hit) = pcie::first_in(&segs, 0, is_nic) else {
            return Ok(None);
        };
        if !board_uefi::map_device(hit.bar, driver_igb::MMIO_LEN) {
            return Err(Error::Nic("igb BAR0 unreachable"));
        }
        if let Some((e, _)) = segs.get(hit.win) {
            hit.f.enable(e);
        }
        // SAFETY: BAR0 is door de firmware toegewezen en nu Device-gemapt;
        // memory-decode en bus-mastering staan aan. NET_DMA is van deze
        // driver alleen, 2 MB-gealigneerd en Normal-NC gemapt, met het
        // bufferblok Normal-WB (`NET_BUF`): dat veegt de driver zelf.
        let mut nic = unsafe { Igb::new(Pa(hit.bar), NET_DMA.base, NET_DMA.size, cpu::idle::now) }
            .map_err(|e| {
                cpu::println!("net: {e} at {} bar0 {:#x}", hit.f.bdf, hit.bar);
                Error::Nic("igb init failed")
            })?;
        let link = nic.link_up(LINK_TIMEOUT_NS).map_err(|e| {
            cpu::println!("net: {e}");
            Error::Nic("igb has no link")
        })?;
        let (vendor, device, bdf, bar) = (hit.f.vendor, hit.f.device, hit.f.bdf, hit.bar);
        match self.wire_nic(&segs, &hit, &mut nic) {
            Ok((wired, us)) => cpu::println!(
                "net: igb {vendor:04x}:{device:04x} at {bdf} bar0 {bar:#x} link {link}, {wired}, first interrupt after {us} us, pump on the line with a 10 ms guard HOPOS_NIC_IRQ"
            ),
            Err(why) => cpu::println!(
                "net: igb {vendor:04x}:{device:04x} at {bdf} bar0 {bar:#x} link {link}, polled ({why}) HOPOS_NIC_IRQ"
            ),
        }
        NIC_CLAIMED.store(true, Relaxed);
        Ok(Some(nic))
    }
}
