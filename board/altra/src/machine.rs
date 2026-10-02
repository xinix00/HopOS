//! De Altra als [`Board`]: het UEFI-board met de igb, de NVMe en de SMpro.
//! Wat niet anders is dan op elke UEFI-machine, gaat door naar [`Uefi`].

use crate::{LINK_TIMEOUT_NS, is_nic};
use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, Plan};
use board_uefi::{NET_DMA, Uefi, pcie};
use core::cell::RefCell;
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use dev::Pa;
use driver_igb::Igb;
use driver_nvme::Nvme;
use driver_smpro::{HWMON_CHANNEL, Smpro};
use sync::{Local, Signal};

/// Leeft er een NIC uit `probe_nic`? Pas gezet na een gelukte probe: een
/// mislukte (geen link) liet niets achter en mag opnieuw (hopos `nic_retry`).
static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);
static DISK_CLAIMED: AtomicBool = AtomicBool::new(false);

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

    /// Vindt en initialiseert de eerste NVMe (het hele device) in de
    /// schijf-helft van de DMA-regio. `Ok(None)` = geen NVMe; één keer.
    pub fn probe_disk(&self) -> Result<Option<Nvme>, Error> {
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

    /// De eerste igb: BAR0, reset en MAC, ringen, dan de link. Gepold, en
    /// dat is het profiel (zie de crate-doc: de INTx doodt de SoC).
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
        // driver alleen, 2 MB-gealigneerd en Normal-NC gemapt.
        let mut nic = unsafe { Igb::new(Pa(hit.bar), NET_DMA.base, NET_DMA.size, cpu::idle::now) }
            .map_err(|e| {
                cpu::println!("net: {e} at {} bar0 {:#x}", hit.f.bdf, hit.bar);
                Error::Nic("igb init failed")
            })?;
        let link = nic.link_up(LINK_TIMEOUT_NS).map_err(|e| {
            cpu::println!("net: {e}");
            Error::Nic("igb has no link")
        })?;
        cpu::println!(
            "net: igb {:04x}:{:04x} at {} bar0 {:#x} link {link}, polled",
            hit.f.vendor,
            hit.f.device,
            hit.f.bdf,
            hit.bar
        );
        NIC_CLAIMED.store(true, Relaxed);
        Ok(Some(nic))
    }
}
