//! QEMU virt (riscv64) in machine mode: ns16550, CLINT, PLIC, virtio-mmio
//! net en blk, FDT; de proefbank van de LicheeRV Nano.
//!
//! De machine is `-M virt -bios none -m 1G`: geen OpenSBI, want HopOS is op
//! RISC-V zelf de machine-mode-monitor (zie `cpu::riscv::boot`), zoals op de
//! LicheeRV waar het image het MONITOR-slot van de FIP inneemt. QEMU start
//! elk hart op 0x8000_0000 met a0 = hart-id en a1 = DTB.
//!
//! De adressen zijn die van `hw/riscv/virt.c` (stabiel): UART0 0x1000_0000
//! (IRQ 10), CLINT 0x0200_0000, PLIC 0x0c00_0000 (96 bronnen), acht
//! virtio-mmio-transports vanaf 0x1000_1000 (stride 0x1000, IRQ 1 + n).
//! De timebase is 10 MHz (`RISCV_ACLINT_DEFAULT_TIMEBASE_FREQ`).
//!
//! Dit crate bezit de adressen, de klok, de slaap en de bedrading van de
//! drivers. De drivers zelf kennen geen adres.

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

pub mod cage;
pub mod slots;

use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, Plan, Region};
use core::cell::Cell;
use core::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering::Relaxed};
use cpu::irq::{Controller, Line};
use cpu::riscv::clint::Clint;
use cpu::riscv::csr;
use cpu::riscv::plic::{Plic, machine_context};
use dev::Pa;
use driver_ns16550::Ns16550;
use driver_virtioblk::VirtioBlk;
use driver_virtionet::{IrqAck, VirtioNet};
use fw::fdt::Fdt;
use sync::{Local, Signal};

/// De schijf die `probe_disk` geeft: de virtio-blk op de mmio-bus. De binary
/// noemt hem `vboard::Disk`, zodat de geprobede schijf van de bench naar de
/// opslag gaat zonder dat de binary het type per board kent.
pub type Disk = VirtioBlk;

/// De ns16550 van virt (byte-stride).
pub const UART0: Pa = Pa(0x1000_0000);
/// De CLINT.
pub const CLINT: Pa = Pa(0x0200_0000);
/// De PLIC.
pub const PLIC: Pa = Pa(0x0c00_0000);
/// Het aantal PLIC-bronnen van virt (`VIRT_IRQCHIP_NUM_SOURCES`).
pub const PLIC_SOURCES: u32 = 96;
/// De timebase van de TIME-CSR op QEMU virt.
pub const TIMEBASE_HZ: u64 = 10_000_000;
/// Het begin van het DRAM.
pub const DRAM: Pa = Pa(0x8000_0000);

/// De kern-RAM: image, stack en heap. 240 MB, zoals op arm64.
pub const KERN_RAM: Region = Region {
    base: Pa(0x8000_0000),
    size: 0x0f00_0000,
};
/// De DMA-regio: 16 MB achter de kern-RAM. QEMU is coherent, dus gecachet
/// is hier goed; op de C906 gaat alles door `dev::push`/`pull`.
pub const DMA: Region = Region {
    base: Pa(0x8f00_0000),
    size: 0x0100_0000,
};
/// De NIC-helft van de DMA-regio.
pub const NET_DMA: Region = Region {
    base: Pa(0x8f00_0000),
    size: 0x0080_0000,
};
/// De schijf-helft van de DMA-regio.
pub const BLK_DMA: Region = Region {
    base: Pa(0x8f80_0000),
    size: 0x0080_0000,
};

const _: () = {
    assert!(KERN_RAM.end().0 == DMA.base.0);
    assert!(NET_DMA.end().0 == BLK_DMA.base.0 && BLK_DMA.end().0 == DMA.end().0);
    assert!(driver_virtioblk::DMA_NEED <= BLK_DMA.size);
    assert!(DMA.end().0 <= slots::POOL[0].base);
};

/// Het eerste virtio-mmio-transport.
pub const VIRTIO_MMIO: Pa = Pa(0x1000_1000);
const VIRTIO_STRIDE: u64 = 0x1000;
const VIRTIO_SLOTS: u64 = 8;
/// De PLIC-bron van transport 0; transport n heeft `VIRTIO_IRQ + n`.
const VIRTIO_IRQ: u32 = 1;

/// Zonder FDT: het aantal harts dat `image/qemu-riscv-run.sh` vraagt.
const CORES_DEFAULT: usize = 2;

// SAFETY: 0x1000_0000 is de ns16550 van QEMU virt (byte-stride); machine
// mode heeft geen vertaling, dus hij is altijd bereikbaar.
static UART: Ns16550 = unsafe { Ns16550::new(UART0, 0) };

// SAFETY: de PLIC van QEMU virt, 96 bronnen, altijd bereikbaar in M-mode.
static PLIC_DEV: Plic = unsafe { Plic::new(PLIC, PLIC_SOURCES) };

// SAFETY: de CLINT van QEMU virt in de SiFive-indeling.
const CLINT_DEV: Clint = unsafe { Clint::new(CLINT) };

/// De bel van de NIC.
static NIC_BELL: Signal = Signal::new();
/// De NIC-lijn en zijn ack.
static NIC_IRQ: Local<Cell<Option<(u32, IrqAck)>>> = Local::new(Cell::new(None));

/// Het adres van een geldige DTB, 0 = geen.
static DTB: AtomicU64 = AtomicU64::new(0);
/// Het bij boot gevonden DRAM.
static MEM_TOTAL: AtomicU64 = AtomicU64::new(0);
/// Het aantal harts uit de FDT.
static CORES: AtomicUsize = AtomicUsize::new(0);
/// Is de CLINT-probe geslaagd?
static CLINT_OK: AtomicBool = AtomicBool::new(false);
static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);
static DISK_CLAIMED: AtomicBool = AtomicBool::new(false);

fn console_write(b: &[u8]) {
    UART.write_bytes(b);
}

/// De DTB op `pa` als er een geldige header staat, binnen de eerste 4 GB
/// DRAM (QEMU legt hem bovenin het RAM).
fn dtb_at(pa: u64) -> Option<Fdt<'static>> {
    let dram = Region {
        base: DRAM,
        size: 1 << 32,
    };
    if pa == 0 || !dram.contains(Pa(pa)) || !pa.is_multiple_of(8) {
        return None;
    }
    let mut head = [0u8; 8];
    dev::copy_out(&mut head, Pa(pa));
    let total = fw::fdt::total_size(&head)?;
    if !dram.contains(Pa(pa).add(total as u64 - 1)) {
        return None;
    }
    // SAFETY: `[pa, pa+total)` ligt in het DRAM (hierboven getoetst) en
    // wordt door niemand beschreven: QEMU legt de DTB boven alles wat het
    // plan uitdeelt (slots.rs), buiten de heap. Alleen lezen.
    let blob = unsafe { core::slice::from_raw_parts(pa as usize as *const u8, total) };
    Fdt::new(blob).ok()
}

fn fdt() -> Option<Fdt<'static>> {
    dtb_at(DTB.load(Relaxed))
}

/// De eerste waarde van een boot-sleutel uit de FDT-bootargs (QEMU
/// `-append`); leeg als hij er niet is.
#[must_use]
pub fn boot_param(key: &'static str) -> &'static str {
    fw::bootcfg::first(fw::bootcfg::cmdline(bootargs(), key))
}

/// De FDT-bootargs (QEMU `-append`); "" zonder.
#[must_use]
pub fn bootargs() -> &'static str {
    fdt().and_then(|f| f.bootargs()).unwrap_or("")
}

/// QEMU virt (riscv64) als board.
pub struct QemuVirtRiscv;

impl QemuVirtRiscv {
    /// Het board. Alle staat staat in statics.
    #[must_use]
    pub const fn new() -> Self {
        Self
    }

    /// Het eerste virtio-mmio-transport waarop `is` ja zegt, met zijn
    /// PLIC-bron. Een vaste scan: de acht plekken van virt.
    fn find_virtio(is: impl Fn(Pa) -> bool) -> Option<(Pa, u32)> {
        (0..VIRTIO_SLOTS)
            .map(|i| (VIRTIO_MMIO.add(i * VIRTIO_STRIDE), VIRTIO_IRQ + i as u32))
            .find(|&(pa, _)| is(pa))
    }

    /// Het hart waar dit draait.
    #[must_use]
    pub fn this_core(&self) -> usize {
        csr::mhartid() as usize
    }

    /// De OS-core: op riscv64 altijd het boot-hart (0). Een
    /// `hopos.oscore`-vraag wordt luid genegeerd: de verhuizing bestaat hier
    /// nog niet (de lottery van de Go-generatie is niet geport).
    #[must_use]
    pub fn os_core(&self) -> (usize, Option<&'static str>) {
        let asked = fdt()
            .and_then(|f| f.bootargs())
            .is_some_and(|a| a.contains("hopos.oscore="));
        (
            0,
            asked.then_some("the riscv64 kern stays on its boot hart"),
        )
    }

    /// De bel van de OS-core-rotatie van arm64 (een GIC-SGI). Op riscv64
    /// is de kick van de kern-hart zijn `msip` ([`Self::kick_self`],
    /// `cpu::riscv::oscore`); dit is de stub die de gedeelde lijm laat
    /// bouwen.
    #[must_use]
    pub fn os_bell(&self) -> cpu::el2::Bell {
        cpu::el2::Bell {
            sgi1r: 0,
            sgir: 0,
            intid: 0,
            pending: || 1023,
        }
    }

    /// De kick naar dit hart zelf: de `msip`.
    pub fn kick_self(&self) {
        CLINT_DEV.set_msip(self.this_core(), true);
    }

    /// De CLINT van dit board.
    #[must_use]
    pub const fn clint(&self) -> Clint {
        CLINT_DEV
    }

    /// Wat de kooi van app-hart `hart` moet weten: de SiFive-CLINT van virt
    /// is gedeeld (elk hart zijn eigen `msip` en `mtimecmp` op zijn index),
    /// QEMU wekt een `wfi` betrouwbaar op de wekker, en er is geen
    /// resetblok: de kill-tick is het mes. Zonder bewezen CLINT geen wekker
    /// (de switcher spint) en dus ook geen tick.
    #[must_use]
    pub fn app_hart(&self, hart: usize) -> cpu::riscv::switch::AppHart {
        let hz = cpu::riscv::idle::hz();
        let clint = CLINT_OK.load(Relaxed);
        cpu::riscv::switch::AppHart {
            mtimecmp: if clint {
                CLINT_DEV.mtimecmp(hart)
            } else {
                Pa(0)
            },
            msip: CLINT_DEV.msip(hart),
            sleep_cap: cpu::riscv::idle::ns_to_ticks(cpu::riscv::idle::WFI_CAP_QEMU_NS, hz),
            tick: cpu::riscv::idle::ns_to_ticks(cpu::riscv::switch::KILL_TICK_NS, hz),
            attrs: cpu::riscv::sv39::Attrs::Spec,
            pmp: cpu::riscv::pmp::QEMU,
            resettable: false,
        }
    }

    /// Brengt app-hart `hart` naar de parkeerlus: op QEMU niets, want elk
    /// hart begint daar bij de reset (`-bios none`).
    pub fn start_app_hart(&self, _hart: usize) {}

    /// Het resetblok van een app-hart: QEMU virt heeft er geen.
    pub fn hold_app_hart(&self, _hart: usize) -> bool {
        false
    }

    /// De zelftest van de riscv-kooi op hart 1 ([`cage`]), één regel met
    /// marker. Alleen met een tweede hart en een bewezen wekker.
    fn cage_selftest(&self) {
        if self.cores() < 2 || !CLINT_OK.load(Relaxed) {
            cpu::println!(
                "cage: riscv switcher self-test skipped (needs 2 harts and the CLINT) HOPOS_RV_CAGE_SKIP"
            );
            return;
        }
        let (Ok(plan), Some(core), Some(slot)) = (
            slots::plan(self.cores(), 0),
            abi::layout::Core::new(1),
            abi::layout::Slot::new(1),
        ) else {
            cpu::println!("cage: riscv switcher self-test: no plan HOPOS_RV_CAGE_FAIL");
            return;
        };
        let (Ok(sched), Ok(ctx)) = (plan.park_mbox_pa(core), plan.ctx_pa(slot)) else {
            cpu::println!(
                "cage: riscv switcher self-test: no sched or ctx block HOPOS_RV_CAGE_FAIL"
            );
            return;
        };
        let hart = plan.phys_core(core);
        match cage::selftest(
            slots::CAGE_PA,
            Pa(sched.0),
            Pa(ctx.0),
            hart,
            slots::POOL[0].base,
            CLINT_DEV,
        ) {
            Ok(r) => cpu::println!(
                "cage: riscv switcher on hart {hart}: yield resumed at {:#x}, exit state {}, escape state {} with mcause {} mtval {:#x} vec {}, spinner state {} after the kill tick ({} us), sleeper state {} -> {} {}",
                r.resume,
                r.first,
                r.second,
                r.fault.0,
                r.fault.1,
                r.fault.2,
                r.spin.0,
                r.spin.1,
                r.sleeper.1,
                r.sleeper.0,
                if r.ok() {
                    "HOPOS_RV_CAGE_UP"
                } else {
                    "HOPOS_RV_CAGE_FAIL"
                }
            ),
            Err(e) => cpu::println!("cage: riscv switcher self-test: {e} HOPOS_RV_CAGE_FAIL"),
        }
    }

    /// Vindt en initialiseert de schijf (virtio-blk). Eén keer.
    pub fn probe_disk(&self) -> Result<Option<VirtioBlk>, Error> {
        if DISK_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_disk"));
        }
        let Some((base, _irq)) = Self::find_virtio(|base| {
            // SAFETY: `find_virtio` geeft alleen adressen in het
            // virtio-mmio-venster van virt.
            unsafe { driver_virtioblk::is_modern_blk(base) }
        }) else {
            return Ok(None);
        };
        // SAFETY: `base` is een virtio-mmio-transport van virt, en BLK_DMA is
        // van deze driver alleen. De lijn blijft uit: de driver pollt.
        let disk =
            unsafe { VirtioBlk::new(base, BLK_DMA.base, BLK_DMA.size, cpu::riscv::idle::now) }
                .map_err(|_| Error::Disk("virtio-blk init failed"))?;
        cpu::println!(
            "disk: virtio-blk at {:#x}, {} sectors, flush {}",
            base.0,
            disk.sectors(),
            if disk.can_flush() { "yes" } else { "no" }
        );
        Ok(Some(disk))
    }
}

impl Default for QemuVirtRiscv {
    fn default() -> Self {
        Self::new()
    }
}

impl Board for QemuVirtRiscv {
    type Nic = VirtioNet;
    type Sleeper = cpu::riscv::idle::RvSleeper;

    const NAME: &'static str = "qemuvirt-riscv";

    fn console(&self) -> fn(&[u8]) {
        console_write
    }

    /// Het "EL" op riscv is de privilege-modus: 3 = machine mode. De kooi
    /// (PMP) vraagt machine mode, zoals hij op ARM EL2 vraagt.
    fn privilege(&self, el: u8) -> Result<(), Error> {
        if el == 3 {
            Ok(())
        } else {
            Err(Error::Privilege { el })
        }
    }

    fn firmware(&self) -> &'static str {
        "boot: QEMU virt riscv64, machine mode, -bios none (HopOS is the monitor, no OpenSBI below us)"
    }

    fn init_heap(&self, heap: &Heap) {
        let (start, end) = arch::heap_bounds();
        // SAFETY: `link-riscv.ld` legt `__heap_start` achter image en stack
        // en `__heap_end` op het begin van de DMA-regio; niemand anders
        // gebruikt dat bereik.
        unsafe { heap.init(start, end) };
    }

    fn discover(&self, dtb: u64) {
        cpu::riscv::idle::set_hz(TIMEBASE_HZ);
        match CLINT_DEV.probe(self.this_core(), csr::rdtime()) {
            Ok(()) => {
                CLINT_OK.store(true, Relaxed);
                cpu::println!("board: CLINT: mtimecmp writable, the kern sleeps on it (wfi)");
            }
            Err(e) => cpu::println!(
                "board: CLINT: {e}, the kern polls instead of sleeping HOPOS_CLINT_FAIL"
            ),
        }
        cpu::println!("{}", cpu::riscv::trng::WARNING);
        let Some(f) = dtb_at(dtb) else {
            cpu::println!(
                "WARNING HOPOS_RAM_CHECK_SKIPPED: no valid DTB (a1={dtb:#x}) - trusting the static layout"
            );
            return;
        };
        DTB.store(dtb, Relaxed);
        MEM_TOTAL.store(f.mem_total().unwrap_or(0), Relaxed);
        CORES.store(f.cpu_count().unwrap_or(0), Relaxed);
        cpu::println!(
            "fdt: {} bytes at {dtb:#x}, bootargs {:?}",
            f.size(),
            f.bootargs().unwrap_or("")
        );
        self.cage_selftest();
    }

    fn clock(&self) -> executor::Clock {
        cpu::riscv::idle::now
    }

    /// `wfi` op de eigen `mtimecmp` als de probe slaagde, anders pollen.
    fn sleeper(&self) -> Self::Sleeper {
        let clint = CLINT_OK.load(Relaxed).then_some(CLINT_DEV);
        cpu::riscv::idle::RvSleeper::new(clint, self.this_core())
            .with_cap(cpu::riscv::idle::WFI_CAP_QEMU_NS)
    }

    fn mem_total(&self) -> u64 {
        MEM_TOTAL.load(Relaxed)
    }

    fn cores(&self) -> usize {
        match CORES.load(Relaxed) {
            0 => CORES_DEFAULT,
            n => n,
        }
    }

    fn core_class(&self, _core: usize) -> CoreClass {
        CoreClass::Big
    }

    fn plan(&self) -> Plan {
        Plan {
            kern_ram: KERN_RAM,
            dma: DMA,
            net_dma: NET_DMA,
        }
    }

    fn start_interrupts(&self) -> Result<&'static Signal, Error> {
        PLIC_DEV.set_context(machine_context(self.this_core()));
        cpu::println!("irq: {}", PLIC_DEV.describe());
        // Vanaf hier mag de trap komen: MEIE en MSIE aan, MIE aan. De
        // ingang zet de bron weer dicht tot de dispatch-taak claimde.
        csr::mie_set(csr::MIP_MEIP | csr::MIP_MSIP);
        csr::restore(csr::MSTATUS_MIE);
        Ok(&cpu::irq::IRQ_PENDING)
    }

    fn dispatch_interrupts(&self) -> Dispatched {
        let nic = NIC_IRQ.get().get();
        let mut d = Dispatched::default();
        cpu::riscv::trap::take_irq();
        // De kick: de `msip` wissen, dan staat de bron weer open.
        CLINT_DEV.set_msip(self.this_core(), false);
        while let Some(Line(id)) = PLIC_DEV.claim() {
            match nic {
                Some((line, ack)) if id == line => {
                    ack.ack();
                    NIC_BELL.set();
                    d.nic += 1;
                }
                _ => {
                    PLIC_DEV.disable(Line(id));
                    d.other += 1;
                }
            }
            PLIC_DEV.complete(Line(id));
        }
        // De ingang liet MEIE en MSIE dicht; de ronde is klaar.
        csr::mie_set(csr::MIP_MEIP | csr::MIP_MSIP);
        d
    }

    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error> {
        if NIC_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_nic"));
        }
        let Some((base, irq)) = Self::find_virtio(|base| {
            // SAFETY: `find_virtio` geeft alleen adressen in het
            // virtio-mmio-venster van virt.
            unsafe { driver_virtionet::is_modern_net(base) }
        }) else {
            return Ok(None);
        };
        // SAFETY: `base` is een virtio-mmio-transport van virt, en NET_DMA
        // is van deze driver alleen.
        let mut nic = unsafe { VirtioNet::new(base, NET_DMA.base, NET_DMA.size) }
            .map_err(|_| Error::Nic("virtio-net init failed"))?;
        if PLIC_DEV.enable(Line(irq)).is_ok() {
            NIC_IRQ.get().set(Some((irq, nic.irq_ack())));
            nic.set_irq(&NIC_BELL);
        }
        cpu::println!(
            "net: virtio-net at {:#x}, plic source {irq}, queue {}",
            base.0,
            nic.queue_size()
        );
        Ok(Some(nic))
    }
}

mod arch {
    //! De grenzen van de heap uit het linkscript; op de host een stub.
    #[cfg(all(target_arch = "riscv64", target_os = "none"))]
    pub(crate) fn heap_bounds() -> (usize, usize) {
        unsafe extern "C" {
            /// Het begin van de heap (`hopos/link-riscv.ld`).
            static __heap_start: u8;
            /// Het einde van de heap: het begin van de DMA-regio.
            static __heap_end: u8;
        }
        (
            (&raw const __heap_start) as usize,
            (&raw const __heap_end) as usize,
        )
    }

    #[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
    pub(crate) fn heap_bounds() -> (usize, usize) {
        (0, 0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn virtio_slots_and_lines() {
        let found = QemuVirtRiscv::find_virtio(|pa| pa == VIRTIO_MMIO.add(7 * VIRTIO_STRIDE));
        assert_eq!(found, Some((Pa(0x1000_8000), 8)));
        assert_eq!(QemuVirtRiscv::new().privilege(3), Ok(()));
        assert_eq!(
            QemuVirtRiscv::new().privilege(1),
            Err(Error::Privilege { el: 1 })
        );
    }
}
