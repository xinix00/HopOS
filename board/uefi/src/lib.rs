//! Het generieke UEFI-board: elke arm64-machine waar UEFI-firmware ons als
//! PE-app start en ACPI de hardware beschrijft. QEMU virt onder EDK2 is de
//! proeftuin; de Orion O6N en de Ampere Altra bouwen erop.
//!
//! Wat dit crate bezit:
//!
//! - de EFI-stub ([`boot`], `entry`): de PE-header, de zelf-relocatie, de
//!   firmware-diensten tot `ExitBootServices`, en de sprong naar dezelfde
//!   `kmain` als op virt;
//! - de feiten die de stub leerde ([`facts`]): de cores met hun MPIDR en
//!   klasse, de GIC (MADT), de ECAM-vensters (MCFG), de console (SPCR), de
//!   timer-PPI (GTDT), de memory map, `hopos.cfg` en het gestagede image;
//! - de identity map van 48 bits ([`mmu`]) en het kernvenster
//!   ([`WINDOW_PA`]): heap, DMA, het kooi-venster en de staging;
//! - de bedrading: de SPCR-UART (PL011 of 16550), de GICv3, PCIe via de
//!   ECAM en virtio-net en virtio-blk over PCI.
//!
//! Wat het niet bezit: de drivers zelf, en board-specifieke kennis (een
//! vroege UART, een NIC die geen virtio is, klokken): dat is van de
//! O6N- en Altra-crates die hierop bouwen.

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

mod arch;
pub mod boot;
mod efi;
// De vorm van het EL2-regime van de kern: nVHE, of VHE met de feature
// `vhe` (de O6N, en de proef op QEMU met neoverse-n1).
mod el2;
mod entry;
pub mod facts;
mod flip; // FLIP: de feitenpagina voor een geflipte kern (het flip-spoor)
pub mod irq;
// De framebuffer van de firmware (GOP), alleen in de gui-smaak; kaal een
// stub met dezelfde signatuur (handboek §7, docs/gui.md).
#[cfg(feature = "gui")]
mod gop;
#[cfg(not(feature = "gui"))]
mod gop {
    //! Kaal gebouwd: geen GOP, headless.
    use crate::efi::Efi;
    use crate::mmu::{self, Mmu};

    /// Niets te lezen.
    pub(crate) fn discover(_efi: &Efi) {}
    /// Geen beeld.
    pub(crate) fn framebuffer() -> Option<driver_fb::Desc> {
        None
    }
    /// Niets te mappen.
    pub(crate) fn map(_m: &mut Mmu) -> Result<(), mmu::Error> {
        Ok(())
    }
}
mod memmap;
mod mmu;
pub mod slots;
// De xHCI's op PCIe (usb.rs), alleen in de gui-smaak; het DMA-stuk ook
// voor de O6N.
pub mod usb;
pub mod watchdog;

use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, Plan, Region};
use core::sync::atomic::{AtomicBool, AtomicPtr, AtomicU32, Ordering::Relaxed};
use dev::Pa;
use driver_gicv3::Gic;
use driver_ns16550::Ns16550;
use driver_pcie::{Ecam, Function};
use driver_pl011::Pl011;
use driver_virtioblk::VirtioBlk;
use driver_virtionet::VirtioNet;
use driver_virtiopci::pci::{self, Pci};
use sync::Signal;

/// De schijf die `probe_disk` geeft: de virtio-blk over PCI. De binary noemt
/// hem `vboard::Disk`, zodat de geprobede schijf van de bench naar de opslag
/// gaat zonder dat de binary het type per board kent.
pub type Disk = VirtioBlk<Pci>;

/// Het kernvenster: één allocatie van de stub op een vast adres, zodat de
/// kern-flip en de slot-lijm er constanten van kunnen maken (zoals op
/// virt). Faalt de claim, dan zegt de stub welke regio's wél vrij zijn
/// (de Go-meting "RAM WINDOW BUSY", 13-07) en gaat hij terug naar de
/// firmware.
///
/// 0x5000_0000: vrij op QEMU virt onder EDK2 met `-m 3G` (gemeten 29-09,
/// zie `tools/qemu-uefi-test.sh`). Een board met DRAM elders (de Altra en
/// de O6N op 0x8000_0000) kiest een eigen venster met de feature
/// `window-8000` (0x8800_0000, Go's Altra-kandidaat).
#[cfg(not(feature = "window-8000"))]
pub const WINDOW_PA: u64 = 0x5000_0000;
/// Zie de standaardversie hierboven.
#[cfg(feature = "window-8000")]
pub const WINDOW_PA: u64 = 0x8800_0000;

/// Draait de kern van dit board onder E2H = 1 (feature `vhe`, zie
/// `el2`)? De binary toetst bij het bouwen dat de switcher-smaak van zijn
/// app-cores en zijn OS-core hiermee klopt (`hopos/src/cage.rs` `FLAVOR`).
pub const KERN_VHE: bool = el2::VHE;

/// De maat van het kernvenster: 320 MB.
pub const WINDOW: u64 = 0x1400_0000;

/// De heap van de kern: 220 MB vanaf het begin van het venster.
pub const HEAP: Region = Region {
    base: Pa(WINDOW_PA),
    size: 0x0dc0_0000,
};

/// De tabellen van de identity map: 4 MB (1024 tabellen) na de heap, in
/// Normal-WB-geheugen (de walker leest cacheable).
pub const TABLES: Region = Region {
    base: Pa(WINDOW_PA + 0x0dc0_0000),
    size: 0x0040_0000,
};

/// De kern-RAM: heap en tabellen, Normal WB.
pub const KERN_RAM: Region = Region {
    base: Pa(WINDOW_PA),
    size: 0x0e00_0000,
};

/// De DMA-regio: 16 MB, Normal non-cacheable (een controller leest zonder
/// cache-onderhoud, zoals op virt).
pub const DMA: Region = Region {
    base: Pa(WINDOW_PA + 0x0e00_0000),
    size: 0x0100_0000,
};

/// De NIC-helft van de DMA-regio, min de laatste MB (de ITS).
pub const NET_DMA: Region = Region {
    base: Pa(WINDOW_PA + 0x0e00_0000),
    size: 0x0070_0000,
};

/// De tabellen van de GICv3-ITS en de LPI's ([`irq`]): 1 MB aan het eind
/// van de NIC-helft, 64 KB-gealigneerd en Normal-NC zoals alle DMA, zodat
/// de GIC en wij hetzelfde zien zonder cache-onderhoud. Een NIC-driver
/// vraagt hoogstens 2 MB plus 640 KB (rtl8126, igb: `DMA_NEED`).
pub const ITS_DMA: Region = Region {
    base: Pa(WINDOW_PA + 0x0e70_0000),
    size: driver_gicv3::its::MEM_LEN,
};

/// De schijf-helft van de DMA-regio.
pub const BLK_DMA: Region = Region {
    base: Pa(WINDOW_PA + 0x0e80_0000),
    size: 0x0080_0000,
};

/// Het kooi-venster: 16 MB Device (control-pages en kooien; de app-cores
/// lezen het op EL2 met de MMU uit).
pub const ADMIN: Region = Region {
    base: Pa(WINDOW_PA + 0x0f00_0000),
    size: 0x0100_0000,
};

/// De loader-regio: boot-scratch, het staging-woord en het gestagede
/// image; 64 MB Normal.
pub const LOADER: Region = Region {
    base: Pa(WINDOW_PA + 0x1000_0000),
    size: 0x0400_0000,
};

const _: () = {
    assert!(HEAP.end().0 == TABLES.base.0 && TABLES.end().0 == KERN_RAM.end().0);
    assert!(KERN_RAM.end().0 == DMA.base.0 && DMA.end().0 == ADMIN.base.0);
    assert!(NET_DMA.end().0 == ITS_DMA.base.0 && ITS_DMA.end().0 == BLK_DMA.base.0);
    assert!(BLK_DMA.end().0 == DMA.end().0 && ITS_DMA.base.0.is_multiple_of(0x1_0000));
    assert!(ADMIN.end().0 == LOADER.base.0 && LOADER.end().0 == WINDOW_PA + WINDOW);
    assert!(driver_virtioblk::DMA_NEED <= BLK_DMA.size);
    assert!(WINDOW_PA.is_multiple_of(2 << 20));
};

/// De kick van de OS-core: SGI 1, zoals de Go-generatie (`KickSGI`).
///
/// Tot 30-09 stond hier 8, "zoals op virt", met de omgekeerde reden: niet
/// de niet-beveiligde firmware houdt 0..=7, TF-A houdt 8..=15
/// (`driver_gicv3::NS_SGI_COUNT`). Op QEMU (geen EL3, DS=1) merkte niemand
/// het; op de O6N (TF-A op EL3, DS=0) kwam de kick nooit aan en ving de
/// timer hem in de zelftest (`kick=(Timer, 100000 us, try 2)`). De Altra
/// draait ook TF-A en zou in dezelfde val lopen. 0..=7 zijn van de
/// niet-beveiligde wereld, en na ExitBootServices zijn wij daar de enige.
pub const KICK_SGI: u32 = 1;

// De kick moet een SGI zijn die wij mogen sturen; een secure SGI verdwijnt
// stil (zie hierboven).
const _: () = assert!(driver_gicv3::is_ns_sgi(KICK_SGI));

/// Hoe lang [`Uefi::kick_self`] wacht tot zijn SGI pending staat: 1 ms. Een
/// SGI naar de eigen core staat binnen microseconden in de redistributor;
/// wat er na een milliseconde nog niet staat, komt niet.
const KICK_SEEN_NS: u64 = 1_000_000;

/// De poll-grens naast [`KICK_SEEN_NS`], voor een teller die niet loopt.
const KICK_SEEN_POLLS: u32 = 100_000;

/// Zoveel verloren kicks krijgen een regel; daarna telt [`KICKS_LOST`].
const KICK_LOST_LINES: u32 = 2;

/// Meetlat: kicks van [`Uefi::kick_self`] die niet pending kwamen.
pub static KICKS_LOST: AtomicU32 = AtomicU32::new(0);

/// Zoveel eigen lijnen kan een board aanzetten ([`Uefi::enable_line`]).
pub const LINES: usize = 8;

/// De lijnen van [`Uefi::enable_line`]: INTID (0 = vrij), bel en ack. Eén
/// schrijver bij boot, daarna gelezen door de dispatch.
static LINE_IDS: [AtomicU32; LINES] = [const { AtomicU32::new(0) }; LINES];
static LINE_BELLS: [AtomicPtr<Signal>; LINES] =
    [const { AtomicPtr::new(core::ptr::null_mut()) }; LINES];
static LINE_ACKS: [AtomicPtr<()>; LINES] = [const { AtomicPtr::new(core::ptr::null_mut()) }; LINES];

/// Een eigen lijn in de dispatch: ack, bel. `false` = niet van ons.
fn own_line(id: u32) -> bool {
    let Some(i) = LINE_IDS.iter().position(|l| l.load(Relaxed) == id) else {
        return false;
    };
    let ack = LINE_ACKS
        .get(i)
        .map_or(core::ptr::null_mut(), |a| a.load(Relaxed));
    if !ack.is_null() {
        // SAFETY: `LINE_ACKS` wordt alleen door `enable_line` geschreven, met
        // een geldige `fn()`; een functiepointer en een datapointer zijn op
        // onze targets even groot.
        let f = unsafe { core::mem::transmute::<*mut (), fn()>(ack) };
        f();
    }
    let bell = LINE_BELLS
        .get(i)
        .map_or(core::ptr::null_mut(), |b| b.load(Relaxed));
    // SAFETY: `enable_line` zette hier een `&'static Signal`.
    if let Some(b) = unsafe { bell.as_ref() } {
        b.set();
    }
    true
}

/// De OS-core uit de config-waarde `v` op een board met `cores` cores en
/// klassen `class` (dezelfde regel als op virt).
fn os_core_of(
    v: &str,
    cores: usize,
    class: impl Fn(usize) -> CoreClass,
) -> (usize, Option<&'static str>) {
    let want = match v {
        "" => return (0, None),
        "small" => Some(CoreClass::Small),
        "mid" => Some(CoreClass::Mid),
        "big" => Some(CoreClass::Big),
        _ => None,
    };
    if let Some(k) = want {
        return match (0..cores).find(|c| class(*c) == k) {
            Some(c) => (c, None),
            None => (0, Some("no core of that class")),
        };
    }
    match v.parse::<usize>() {
        Ok(n) if n < cores => (n, None),
        Ok(_) => (0, Some("no such core")),
        Err(_) => (0, Some("not small, mid, big or a core number")),
    }
}

/// De PCIe-segmenten die we afzoeken, en hoeveel functies per segment we
/// in de bootlog noemen.
const PCI_LOG_MAX: usize = 32;

/// De bel van de NIC-lijn (MSI-X of INTx), zie [`Uefi::probe_nic`].
static NIC_BELL: Signal = Signal::new();

/// Is de NIC al geprobed? `probe_nic` mag één keer.
static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);
/// Is de schijf al geprobed? `probe_disk` mag één keer.
static DISK_CLAIMED: AtomicBool = AtomicBool::new(false);
/// Is de console dood verklaard (de UART meldde nooit ruimte)?
static CONSOLE_DEAD: AtomicBool = AtomicBool::new(false);

/// De console na de exit: de SPCR-UART, per schrijf opgebouwd uit de
/// feiten (de adressen zijn pas bij boot bekend, dus geen `static` met
/// een vaste UART zoals op virt).
fn console_write(b: &[u8]) {
    if CONSOLE_DEAD.load(Relaxed) {
        return;
    }
    let base = facts::CONSOLE_BASE.load(Relaxed);
    if base == 0 {
        return;
    }
    let ty = facts::CONSOLE_TYPE.load(Relaxed);
    let shift = facts::CONSOLE_SHIFT.load(Relaxed);
    if is_16550(ty) {
        // SAFETY: de SPCR wees dit blok aan; de identity map mapt het als
        // Device (`boot::build_map`).
        let u = unsafe { Ns16550::new(Pa(base), shift) };
        u.write_bytes(b);
        CONSOLE_DEAD.store(u.is_dead(), Relaxed);
    } else {
        // SAFETY: zie hierboven; een PL011 (of SBSA-subset: dezelfde DR- en
        // FR-offsets).
        let u = unsafe { Pl011::new(Pa(base)) };
        u.write(b);
        CONSOLE_DEAD.store(u.is_dead(), Relaxed);
    }
}

/// SPCR Interface Type: de 16550-familie (16550, 16450, MAX311xE, 16550
/// met GAS).
fn is_16550(ty: u8) -> bool {
    matches!(ty, 0x00 | 0x01 | 0x02 | 0x12)
}

/// De GIC, uit de feiten.
fn gic() -> Gic<arch::SysRegIcc> {
    // SAFETY: GICD en het redistributor-frame van core 0 komen uit de MADT
    // en de identity map mapt ze als Device (onder 1 TB standaard, daarboven
    // expliciet in `boot::build_map`).
    unsafe {
        Gic::new(
            Pa(facts::GICD.load(Relaxed)),
            Pa(facts::GICR_BASE.load(Relaxed)),
            arch::SysRegIcc,
        )
    }
}

/// De ECAM-vensters als config-space.
fn ecams() -> impl Iterator<Item = (Ecam, u16, u8)> {
    facts::ecams().map(|(base, seg, start, end)| {
        // SAFETY: het venster komt uit de MCFG en de identity map mapt het
        // als Device (`boot::build_map`).
        (unsafe { Ecam::new(Pa(base), start, end) }, seg, start)
    })
}

/// De eerste virtio-functie van type `ty` op een van de segmenten, met
/// het segment en de eerste bus van zijn venster.
fn find_virtio(ty: u32) -> Option<(Ecam, Function, u16, u8)> {
    ecams().find_map(|(e, seg, start)| {
        driver_pcie::find(&e, start, |f| pci::device_type(f) == Some(ty))
            .map(|f| (e, f, seg, start))
    })
}

/// Het generieke UEFI-board.
pub struct Uefi;

impl Uefi {
    /// Het board. Alle staat staat in statics; dit is het handvat.
    #[must_use]
    pub const fn new() -> Self {
        Self
    }

    /// Vindt en initialiseert de schijf (virtio-blk over PCI) in de
    /// schijf-helft van de DMA-regio. `Ok(None)` = geen schijf; één keer.
    /// Geen methode van [`Board`] (het blokcontract is van `kern::hopfs`),
    /// net als op virt.
    pub fn probe_disk(&self) -> Result<Option<VirtioBlk<Pci>>, Error> {
        if DISK_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_disk"));
        }
        let Some((e, f, _, _)) = find_virtio(2) else {
            return Ok(None);
        };
        // SAFETY: de BAR's van een door de firmware geconfigureerde functie
        // liggen onder 1 TB of in een expliciet gemapt venster (Device).
        let t = unsafe { Pci::new(&e, &f) }.map_err(|_| Error::Disk("virtio-pci transport"))?;
        // SAFETY: BLK_DMA is van deze driver alleen, Normal-NC gemapt.
        let disk =
            unsafe { VirtioBlk::with_transport(t, BLK_DMA.base, BLK_DMA.size, cpu::idle::now) }
                .map_err(|_| Error::Disk("virtio-blk init failed"))?;
        cpu::println!(
            "disk: virtio-blk-pci at {}, {} sectors, flush {}",
            f.bdf,
            disk.sectors(),
            if disk.can_flush() { "yes" } else { "no" }
        );
        Ok(Some(disk))
    }

    /// De fysieke index van de core waar dit draait.
    #[must_use]
    pub fn this_core(&self) -> usize {
        slots::core_of(arch::mpidr())
    }

    /// De kick van de OS-core voor de rotatie van `cpu::el2`: de SGI naar
    /// deze core, en de peek waarmee de kern na een terugkeer ziet of het de
    /// kick was. Zoals op virt (dezelfde GICv3-systeemregisters).
    #[must_use]
    pub fn os_bell(&self) -> cpu::el2::Bell {
        cpu::el2::Bell {
            sgi1r: driver_gicv3::sgi1r(arch::mpidr(), KICK_SGI),
            sgir: 0,
            intid: KICK_SGI,
            pending: arch::hppir1,
        }
    }

    /// Stuurt de kick naar deze core zelf: de zelftest van het IPI-pad.
    /// Komt hij niet pending (GICR_ISPENDR0, of ICC_HPPIR1 zegt hem), dan
    /// één luide regel met het woord, het doel en wat de redistributor zei:
    /// de zelftest ziet daarna alleen nog `Timer` en weet niet waarom.
    pub fn kick_self(&self) {
        let mpidr = arch::mpidr();
        let word = driver_gicv3::sgi1r(mpidr, KICK_SGI);
        arch::sgi1r(word);
        let gic = gic();
        let hz = cpu::idle::freq();
        let limit = cpu::idle::counter().wrapping_add(cpu::idle::ns_to_ticks(KICK_SEEN_NS, hz));
        for _ in 0..KICK_SEEN_POLLS {
            if gic.local(KICK_SGI).is_some_and(|l| l.is_pending()) || arch::hppir1() == KICK_SGI {
                return;
            }
            if cpu::idle::counter().wrapping_sub(limit) as i64 >= 0 {
                break;
            }
        }
        if KICKS_LOST.fetch_add(1, Relaxed) < KICK_LOST_LINES {
            let seen = gic.local(KICK_SGI);
            cpu::println!(
                "irq: kick SGI {KICK_SGI} did not arrive: ICC_SGI1R {word:#x} to MPIDR {mpidr:#x}, {}, ICC_HPPIR1 {} HOPOS_KICK_LOST",
                Seen(seen),
                arch::hppir1()
            );
        }
    }

    /// De OS-core die `hopos.cfg` vraagt (`hopos.oscore=<small|mid|big|N>`),
    /// met een reden als de vraag niet kon: dan de boot-core (0), luid. Een
    /// klasse is de eerste core van die klasse (MADT-efficiëntieklasse).
    #[must_use]
    pub fn os_core(&self) -> (usize, Option<&'static str>) {
        let v = fw::bootcfg::first(fw::bootcfg::all(self.config(), "hopos.oscore"));
        os_core_of(v, self.cores(), |c| self.core_class(c))
    }

    /// Een ACPI-tabel met signature `sig` (de eerste; `DSDT` via de FADT),
    /// na de toets van lengte en checksum. Voor de O6N (`_CPC` in de
    /// DSDT/SSDT) en de Altra (de PCCT). Na `kmain`.
    #[must_use]
    pub fn acpi_table(&self, sig: &[u8; 4]) -> Option<&'static [u8]> {
        facts::tables(*sig).next()
    }

    /// De OEM-ID van de XSDT (zes bytes): de toets van een board-crate dat
    /// dit werkelijk zijn machine is vóór er board-kennis in MMIO gaat.
    #[must_use]
    pub fn oem_id(&self) -> [u8; 6] {
        facts::oem_id()
    }

    /// Alle ACPI-tabellen met signature `sig` (de SSDT's).
    pub fn acpi_tables(&self, sig: &[u8; 4]) -> impl Iterator<Item = &'static [u8]> {
        facts::tables(*sig)
    }

    /// Zet SPI, PPI of LPI `intid` aan, naar deze core, en luidt `bell` als
    /// hij komt; `ack` draait in de dispatch vóór de EOI (de device-kant van
    /// een level-lijn, zodat hij valt). Een LPI komt van de ITS
    /// ([`irq::wire_msix`]) en gaat aan in zijn configuratietabel, niet in
    /// de distributor. Hoogstens [`LINES`] lijnen.
    pub fn enable_line(&self, intid: u32, bell: &'static Signal, ack: fn()) -> Result<(), Error> {
        let slot = LINE_IDS
            .iter()
            .position(|l| l.compare_exchange(0, intid, Relaxed, Relaxed).is_ok())
            .ok_or(Error::Irq("no free interrupt line slot"))?;
        if let (Some(b), Some(a)) = (LINE_BELLS.get(slot), LINE_ACKS.get(slot)) {
            b.store(core::ptr::from_ref(bell).cast_mut(), Relaxed);
            a.store(ack as *mut (), Relaxed);
        }
        if irq::is_lpi(intid) {
            return irq::enable_lpi(intid);
        }
        gic()
            .enable(intid, arch::mpidr())
            .map_err(|_| Error::Irq("line refused"))
    }

    /// `hopos.cfg` van de ESP, als tekst (leeg als er geen was).
    #[must_use]
    pub fn config(&self) -> &'static str {
        let (pa, len) = (facts::CFG[0].load(Relaxed), facts::CFG[1].load(Relaxed));
        if pa == 0 || len == 0 {
            return "";
        }
        let Ok(len) = usize::try_from(len) else {
            return "";
        };
        // SAFETY: de stub las het bestand in EfiLoaderData die van ons
        // blijft, Normal gemapt, en niemand schrijft er daarna in.
        let b = unsafe { core::slice::from_raw_parts(pa as usize as *const u8, len) };
        core::str::from_utf8(b).unwrap_or("")
    }

    /// De PCIe-functies in de bootlog: de meting dat de ECAM leeft en wat
    /// de firmware toewees.
    fn log_pcie(&self) {
        for (e, seg, start) in ecams() {
            let mut n = 0;
            driver_pcie::walk(&e, start, |f| {
                n += 1;
                if n <= PCI_LOG_MAX {
                    let link = f.link(&e).map(|l| (l.speed, l.width)).unwrap_or((0, 0));
                    cpu::println!(
                        "pcie: {seg:04x}:{f} bar0 {:#x} link gen{} x{}",
                        f.bar_addr(&e, 0),
                        link.0,
                        link.1
                    );
                }
                true
            });
            cpu::println!(
                "pcie: segment {seg} at {:#x} buses {start}-{}: {n} functions",
                e.base().0,
                e.buses().1
            );
        }
    }
}

impl Default for Uefi {
    fn default() -> Self {
        Self::new()
    }
}

/// De GOP-framebuffer van de stub, voor de boards op dit board (O6N,
/// Altra): hun `Board::framebuffer` geeft deze door. Kaal altijd `None`.
#[must_use]
pub fn gop_framebuffer() -> Option<board::fb::Desc> {
    gop::framebuffer()
}

impl Board for Uefi {
    type Nic = VirtioNet<Pci>;
    type Sleeper = cpu::idle::ArmSleeper;

    const NAME: &'static str = "uefi";

    fn usb_hosts(&self) -> board::UsbHosts {
        usb::hosts()
    }

    fn console(&self) -> fn(&[u8]) {
        if !is_16550(facts::CONSOLE_TYPE.load(Relaxed)) && facts::CONSOLE_BASE.load(Relaxed) != 0 {
            // SAFETY: zie `console_write`.
            unsafe { Pl011::new(Pa(facts::CONSOLE_BASE.load(Relaxed))) }.init();
        }
        console_write
    }

    fn firmware(&self) -> &'static str {
        "boot: UEFI (PE stub, ExitBootServices), ACPI discovery, 48-bit identity map"
    }

    fn init_heap(&self, heap: &Heap) {
        // SAFETY: de heap is het begin van het kernvenster dat de stub claimde
        // (EfiLoaderData), Normal gemapt, en niemand anders gebruikt het.
        unsafe { heap.init(HEAP.base.as_usize(), HEAP.end().as_usize()) };
    }

    /// Op UEFI is er geen DTB: de stub las de ACPI vóór de exit. Hier de
    /// samenvatting op de echte console.
    fn discover(&self, _dtb: u64) {
        let oem = facts::oem_id();
        cpu::println!(
            "uefi: booted at EL{} from image {:#x}+{:#x}, window {WINDOW_PA:#x}+{WINDOW:#x}, {} page tables, OEM {:?}",
            facts::BOOT_EL.load(Relaxed),
            facts::IMAGE[0].load(Relaxed),
            facts::IMAGE[1].load(Relaxed),
            facts::MMU_TABLES.load(Relaxed),
            core::str::from_utf8(&oem)
                .unwrap_or("?")
                .trim_end_matches(['\0', ' ']),
        );
        el2::report();
        // De DRBG van de kern uit de standaardbronnen van de CPU: RNDR op
        // de O6N (Cortex-A720, FEAT_RNG), de SMCCC TRNG van TF-A op de
        // Altra, anders jitter (EDK2 op QEMU), met één luide regel. Tot 30-09
        // zaaide dit board niets en bleef de DRBG stil ongeseed; nu krijgt
        // elk slot zijn zaad (`CTRL_RNG_SEED`) uit hardware waar die er is.
        cpu::println!("{}", cpu::drbg::seed_from_cpu(cpu::idle::counter));
        let cfg = self.config();
        if !cfg.is_empty() {
            cpu::println!("cfg: hopos.cfg from the ESP, {} bytes HOPOS_CFG", cfg.len());
        }
        self.log_pcie();
    }

    fn clock(&self) -> executor::Clock {
        cpu::idle::now
    }

    /// WFI op de timer-PPI, zoals op virt (dezelfde GIC-bedrading, en de
    /// PPI komt uit de GTDT). Onder VHE is de timer van de kern de CNTHP
    /// (PPI 26): `cntp_*_el0` vanaf EL2 met E2H = 1 is die van EL2, en de
    /// dispatch zet hem uit via `hyp_timer_off`.
    fn sleeper(&self) -> Self::Sleeper {
        let s = cpu::idle::ArmSleeper::new(cpu::idle::Mode::Wfi);
        // Onder E2H = 1 wiste de event-stream-write van `new` de
        // EL1-timerbits van CNTHCTL_EL2: terug, anders trapt de teller in
        // elke bewoner. Onder nVHE niets.
        el2::el1_timer_access();
        s
    }

    fn mem_total(&self) -> u64 {
        slots::final_map().ram_bytes()
    }

    fn cores(&self) -> usize {
        facts::CORES.load(Relaxed).max(1)
    }

    /// De klasse uit de efficiëntieklasse van de MADT: gelijk overal = big;
    /// anders de laagste small, de hoogste big, de rest mid (de O6N:
    /// A520, A720, A720-boost).
    fn core_class(&self, core: usize) -> CoreClass {
        let n = self.cores().min(facts::MAX_CORES);
        let class = |c: usize| facts::CORE_CLASS.get(c).map_or(0, |a| a.load(Relaxed));
        let (lo, hi) = (0..n).fold((u8::MAX, 0u8), |(lo, hi), c| {
            (lo.min(class(c)), hi.max(class(c)))
        });
        let me = class(core);
        if lo == hi || me == hi {
            CoreClass::Big
        } else if me == lo {
            CoreClass::Small
        } else {
            CoreClass::Mid
        }
    }

    fn plan(&self) -> Plan {
        Plan {
            kern_ram: KERN_RAM,
            dma: DMA,
            net_dma: NET_DMA,
        }
    }

    fn start_interrupts(&self) -> Result<&'static Signal, Error> {
        let mpidr = arch::mpidr();
        if facts::GICD.load(Relaxed) == 0 {
            return Err(Error::Irq("no GICD in the MADT"));
        }
        if facts::GICR_BASE.load(Relaxed) == 0 {
            let (base, len) = (
                facts::GICR_RANGE[0].load(Relaxed),
                facts::GICR_RANGE[1].load(Relaxed),
            );
            // SAFETY: de reeks komt uit de MADT en is Device-gemapt.
            let rd = unsafe { driver_gicv3::find_redistributor(Pa(base), len, mpidr) }
                .ok_or(Error::Irq("no redistributor frame for core 0"))?;
            facts::GICR_BASE.store(rd.0, Relaxed);
        }
        let gic = gic();
        gic.init()
            .map_err(|_| Error::Irq("redistributor stays asleep"))?;
        gic.enable(facts::TIMER_PPI.load(Relaxed), mpidr)
            .map_err(|_| Error::Irq("timer PPI refused"))?;
        // De OS-core: de EL2-timer (de deadline tijdens de beurt van een
        // bewoner) en de kick van de app-cores, zoals op virt.
        gic.enable(facts::HYP_TIMER_PPI.load(Relaxed), mpidr)
            .map_err(|_| Error::Irq("hyp timer PPI refused"))?;
        gic.enable(KICK_SGI, mpidr)
            .map_err(|_| Error::Irq("kick SGI refused"))?;
        // Leest de groep of de enable terug als 0, dan is de SGI niet van
        // ons (secure, RAZ/WI) en hoort de OS-core de app-cores alleen op
        // zijn timer. Geen weigering: de boot kan verder, maar luid.
        if let Some(l) = gic.local(KICK_SGI)
            && !l.is_ours()
        {
            cpu::println!(
                "irq: kick SGI {KICK_SGI} is not ours ({l}), the OS core hears app cores only on its timer HOPOS_KICK_SECURE"
            );
        }
        cpu::println!("irq: {}", gic.describe());
        irq::start_its(&gic);
        arch::irq_unmask();
        Ok(&cpu::irq::IRQ_PENDING)
    }

    fn dispatch_interrupts(&self) -> Dispatched {
        let gic = gic();
        let timer = facts::TIMER_PPI.load(Relaxed);
        let hyp = facts::HYP_TIMER_PPI.load(Relaxed);
        let mut d = Dispatched::default();
        while let Some(id) = gic.claim() {
            if id == timer {
                arch::timer_off();
                d.timer += 1;
            } else if id == hyp {
                arch::hyp_timer_off();
                d.timer += 1;
            } else if id == KICK_SGI {
                cpu::el2::OS_STATS.kicks.fetch_add(1, Relaxed);
            } else if own_line(id) {
                d.nic += 1;
            } else {
                d.other += 1;
            }
            gic.eoi(id);
        }
        arch::irq_unmask();
        d
    }

    fn framebuffer(&self) -> Option<board::fb::Desc> {
        gop::framebuffer()
    }

    /// De eerste virtio-net over PCI, op MSI-X via de ITS (vector 0 voor
    /// beide queues), anders gepold op de vangrail (`hopos.nicirq=off`
    /// kiest het tweede).
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error> {
        if NIC_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_nic"));
        }
        let Some((e, f, seg, root_bus)) = find_virtio(1) else {
            return Ok(None);
        };
        // SAFETY: zie `probe_disk`.
        let mut t = unsafe { Pci::new(&e, &f) }.map_err(|_| Error::Nic("virtio-pci transport"))?;
        let mode = irq::nic_mode(irq::Mode::Auto);
        if matches!(mode, irq::Mode::Auto | irq::Mode::Msix) {
            // De queues krijgen vector 0 bij hun opzet; zonder MSI-X aan
            // op de functie negeert het device hem (dan INTx of pollen).
            t.use_msix(0);
        }
        // SAFETY: NET_DMA is van deze driver alleen, Normal-NC gemapt.
        let mut nic = unsafe { VirtioNet::with_transport(t, NET_DMA.base, NET_DMA.size) }
            .map_err(|_| Error::Nic("virtio-net init failed"))?;
        // Alleen MSI-X: INTx op virtio-pci vraagt een ack die het
        // ISR-register leest, en QEMU's `_PRT` loopt via link-devices die
        // `fw::aml` luid weigert. Zonder MSI-X pollt de pomp.
        let mode = match mode {
            irq::Mode::Auto | irq::Mode::Msix if nic.transport().msix_ok() => irq::Mode::Msix,
            _ => irq::Mode::Off,
        };
        let at = irq::At {
            ecam: &e,
            seg,
            root_bus,
            f: &f,
        };
        let wired = irq::wire(&at, mode, &NIC_BELL, irq::no_ack, irq::no_ack);
        if !matches!(wired, irq::Wired::Polled(_)) {
            nic.set_irq(&NIC_BELL);
        }
        cpu::println!(
            "net: virtio-net-pci at {}, {wired}, queue {} HOPOS_NIC_IRQ",
            f.bdf,
            nic.queue_size()
        );
        Ok(Some(nic))
    }
}

/// De redistributor-woorden van de kick voor een logregel, of "no
/// redistributor view" als er geen waren.
struct Seen(Option<driver_gicv3::Local>);

impl core::fmt::Display for Seen {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self.0 {
            Some(l) => write!(f, "{l}"),
            None => f.write_str("no redistributor view"),
        }
    }
}

/// Een config-space-functie als tekst, voor wie een driver op een eigen
/// segment zoekt (de O6N- en Altra-crates).
pub fn pcie_segments() -> impl Iterator<Item = (Ecam, u16, u8)> {
    ecams()
}

/// Mapt `[pa, pa + size)` als Device in de identity map, voor een BAR
/// boven de standaard-span van 1 TB (Altra: BAR's en ECAM's boven de
/// 512 GB). Onder 1 TB is alles al Device en is dit een no-op.
pub fn map_device(pa: u64, size: u64) -> bool {
    slots::map_device(pa, size)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_window_layout_is_consistent() {
        assert_eq!(KERN_RAM.end(), DMA.base);
        assert!(DMA.contains(NET_DMA.base) && DMA.contains(BLK_DMA.base));
        assert_eq!(slots::STAGE_PA + slots::STAGE_MAX, LOADER.end().0);
        assert_eq!(slots::DEVICE_WINDOW.base, ADMIN.base.0);
    }

    #[test]
    fn the_os_core_follows_the_config() {
        // Een O6N-achtige indeling: 0-3 small, 4-7 mid, 8-11 big.
        let class = |c: usize| match c {
            0..=3 => CoreClass::Small,
            4..=7 => CoreClass::Mid,
            _ => CoreClass::Big,
        };
        assert_eq!(os_core_of("", 12, class), (0, None));
        assert_eq!(os_core_of("mid", 12, class), (4, None));
        assert_eq!(os_core_of("big", 12, class), (8, None));
        assert_eq!(os_core_of("11", 12, class), (11, None));
        assert_eq!(os_core_of("12", 12, class), (0, Some("no such core")));
        assert_eq!(
            os_core_of("fast", 12, class),
            (0, Some("not small, mid, big or a core number"))
        );
        assert_eq!(
            os_core_of("small", 4, |_| CoreClass::Big),
            (0, Some("no core of that class"))
        );
    }

    #[test]
    fn the_kick_of_the_o6n_os_core_is_a_non_secure_sgi() {
        // Core 0 van de O6N (MPIDR 0x8100_0000) en core 8, de eerste big
        // core (Aff1 = 8): TargetList-bit 0, Aff1 op 23:16, SGI 1 op 27:24.
        assert_eq!(driver_gicv3::sgi1r(0x8100_0000, KICK_SGI), (1 << 24) | 1);
        assert_eq!(
            driver_gicv3::sgi1r(0x8100_0800, KICK_SGI),
            (1 << 24) | (8 << 16) | 1
        );
        assert!(driver_gicv3::is_ns_sgi(KICK_SGI));
    }

    #[test]
    fn spcr_types() {
        assert!(is_16550(0x00) && is_16550(0x12));
        assert!(!is_16550(0x03) && !is_16550(0x0e));
    }
}
