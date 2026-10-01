//! De Sipeed LicheeRV Nano (Sophgo SG2002 / CV181x, XuanTie C906) in machine
//! mode: ns16550, de dwmac met de interne 100M-ePHY, CLINT, PLIC, de
//! T-Head-caches en de DW-watchdog. Het eerste RISC-V-board van HopOS, en
//! het tweede van v3 na QEMU virt riscv64.
//!
//! Registeradressen uit de vendor-DTS (LicheeRV-Nano-Build, cv181x_riscv en
//! sg2002_licheervnano_sd), in Go geverifieerd op het bordje:
//!
//! ```text
//! PLIC     0x7000_0000
//! CLINT    0x7400_0000   c900, SiFive-indeling, GEEN mtime, GEEN 64-bit MMIO
//! UART0    0x0414_0000   DW-APB-16550, reg-shift 2, door de FSBL op 115200
//! GMAC     0x0407_0000   DWMAC1000 (versie 0x1037), interne ePHY via RMII
//! WDT      0x0301_0000   Synopsys DW-WDT, 25 MHz pclk
//! DRAM     0x8000_0000   256 MB
//! RTCCLK   25 MHz        de timebase van de TIME-CSR
//! ```
//!
//! Twee harts: hart 0 is de C906B (1 GHz, de firmware-core, hier de kern),
//! hart 1 de C906L (700 MHz, het app-hart). De C906L komt via het resetblok
//! op ([`LicheeRv::start_little`]): reset vast, boot-vector zetten, reset los.
//! De Go-lottery (de kern verhuist naar de kleine core) is niet geport: de
//! kern blijft op hart 0 (docs/boards-riscv.md).
//!
//! Wat hier NIET is: een hardware-TRNG (luid, `cpu::riscv::trng`) en een
//! SD-driver (geen opslag; hopfs draait zonder schijf).

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

pub mod cfg;
mod ephy;
pub mod slots;
pub mod watchdog;

use board::heap::Heap;
use board::{Board, CoreClass, Dispatched, Error, NoDisk, Plan, Region};
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use cpu::irq::{Controller, Line};
use cpu::riscv::clint::Clint;
use cpu::riscv::csr;
use cpu::riscv::plic::{Plic, machine_context};
use dev::Pa;
use driver_dwmac::{Dwmac, Probe};
use driver_ns16550::Ns16550;
use netdev::Mac;
use sync::Signal;

/// De schijf die `probe_disk` geeft: geen, want er is nog geen SD-driver
/// ([`board::NoDisk`]). De binary noemt hem `vboard::Disk`, zodat de
/// geprobede schijf van de bench naar de opslag gaat zonder dat de binary het
/// type per board kent.
pub type Disk = NoDisk;

/// UART0.
pub const UART0: Pa = Pa(0x0414_0000);
/// De CLINT.
pub const CLINT: Pa = Pa(0x7400_0000);
/// De PLIC.
pub const PLIC: Pa = Pa(0x7000_0000);
/// De PLIC-bronnen van de CV181x (`riscv,ndev = <101>`), plus bron 0.
pub const PLIC_SOURCES: u32 = 102;
/// De dwmac.
pub const GMAC: Pa = Pa(0x0407_0000);
/// De DW-watchdog.
pub const WDT: Pa = Pa(0x0301_0000);
/// De timebase: de vaste 25 MHz-osc, exact 40 ns per tik.
pub const TIMEBASE_HZ: u64 = 25_000_000;

/// De kern-RAM: image, stack en heap (`link-riscv.ld` met de basis en maat
/// van dit board uit build.rs), tot de DMA-regio.
pub const KERN_RAM: Region = Region {
    base: Pa(0x8400_0000),
    size: 0x0280_0000,
};
/// De DMA-regio: de laatste 8 MB van het kernvenster. Op de C906 GECACHET:
/// in machine mode is er geen MMU en bepaalt de sysmap de attributen. De
/// dwmac doet daarom cache-onderhoud per overdracht (dev::push/pull met
/// `thead`), en elke descriptor en buffer staat op een eigen regel (de les
/// van 30-07).
pub const DMA: Region = Region {
    base: Pa(0x8680_0000),
    size: 0x0080_0000,
};
/// De NIC-helft: wat de dwmac vraagt, ruim.
pub const NET_DMA: Region = Region {
    base: Pa(0x8680_0000),
    size: 0x0008_0000,
};

const _: () = {
    assert!(KERN_RAM.end().0 == DMA.base.0);
    assert!(driver_dwmac::NEED_BYTES <= NET_DMA.size);
    assert!(DMA.end().0 == 0x8700_0000);
};

/// Het resetblok van de C906L: SOFT_CPU_RSTN, bit 6.
const C906L_RESET: Pa = Pa(0x0300_3024);
const RESET_BIT: u32 = 1 << 6;
/// SEC_SYS: bit 13 is de override van de boot-vector.
const SEC_SYS_CTRL: Pa = Pa(0x020B_0004);
const SEC_SYS_VEC_LO: Pa = Pa(0x020B_0020);
const SEC_SYS_VEC_HI: Pa = Pa(0x020B_0024);

/// Het app-hart.
pub const HART_LITTLE: usize = 1;

// SAFETY: de DW-APB-16550 van de SG2002 met 32-bit-stride; in machine mode
// altijd bereikbaar, en de FSBL zette hem op 115200.
static UART: Ns16550 = unsafe { Ns16550::new(UART0, 2) };
// SAFETY: de PLIC van de CV181x.
static PLIC_DEV: Plic = unsafe { Plic::new(PLIC, PLIC_SOURCES) };
// SAFETY: de c900-CLINT, SiFive-indeling (msip en mtimecmp; geen mtime).
const CLINT_DEV: Clint = unsafe { Clint::new(CLINT) };

static CLINT_OK: AtomicBool = AtomicBool::new(false);
static NIC_CLAIMED: AtomicBool = AtomicBool::new(false);

fn console_write(b: &[u8]) {
    UART.write_bytes(b);
}

/// De eerste waarde van een boot-sleutel uit `hopos.cfg`, het venster in
/// het image ([`cfg`]); "" als hij er niet is. De FSBL geeft geen DTB en
/// geen bootargs, dus dit is het enige kanaal.
#[must_use]
pub fn boot_param(key: &'static str) -> &'static str {
    fw::bootcfg::first(fw::bootcfg::all(cfg::text(), key))
}

/// Wacht `us` microseconden op de TIME-CSR.
fn wait_us(us: u64) {
    let until = cpu::riscv::idle::now().saturating_add(us.saturating_mul(1000));
    while cpu::riscv::idle::now() < until {
        core::hint::spin_loop();
    }
}

/// De LicheeRV Nano als board.
pub struct LicheeRv;

/// Dit board onder de naam die de kern-binary kiest (`vboard::Machine`).
pub type Machine = LicheeRv;

impl LicheeRv {
    /// Het board.
    #[must_use]
    pub const fn new() -> Self {
        Self
    }

    /// Het hart waar dit draait.
    #[must_use]
    pub fn this_core(&self) -> usize {
        csr::mhartid() as usize
    }

    /// De OS-core: hart 0. De lottery van de Go-kern (de kern naar de kleine
    /// core) is niet geport; een `hopos.oscore` is er dus niet.
    #[must_use]
    pub fn os_core(&self) -> (usize, Option<&'static str>) {
        (0, None)
    }

    /// De bel van de arm64-rotatie: een stub, zie `board-qemuvirt-riscv`.
    #[must_use]
    pub fn os_bell(&self) -> cpu::el2::Bell {
        cpu::el2::Bell {
            sgi1r: 0,
            sgir: 0,
            intid: 0,
            pending: || 1023,
        }
    }

    /// De kick naar dit hart zelf.
    pub fn kick_self(&self) {
        CLINT_DEV.set_msip(self.this_core(), true);
    }

    /// De CLINT van dit board.
    #[must_use]
    pub const fn clint(&self) -> Clint {
        CLINT_DEV
    }

    /// De tekst van `hopos.cfg` (het venster in het image, [`cfg`]), voor
    /// wie meer dan één sleutel leest (de watchdog-taak: `hopos.wd`).
    #[must_use]
    pub fn config(&self) -> &'static str {
        cfg::text()
    }

    /// Wat de kooi van app-hart `hart` moet weten (Go,
    /// board/licheerv/hop/hart.go `HartTimer`). Het app-hart is de C906L:
    ///
    /// - de CLINT is per core en beide cores noemen zichzelf hart 0
    ///   (gemeten 01-08, boot 8): er is GEEN bel van de kern naar de C906L,
    ///   en zijn comparator is voor hem `mtimecmp(0)`;
    /// - "alle stille doden staan op naam van de C906L" (01-08, de
    ///   wfi-klasse): slapen op zijn wekker is daar nooit bewezen, dus de
    ///   switcher spint (geen wekker, geen slaap, geen tick);
    /// - het resetblok is het mes: de harde intrekking is reset vast (dat
    ///   wist ook zijn PMP, gemeten 30-07) en opnieuw de parkeerlus in.
    ///
    /// Wie de C906L wil laten slapen, probet eerst zijn wekker op dat hart
    /// en zet dan `mtimecmp(0)` en een slaapgrens hier (docs/boards-riscv.md).
    #[must_use]
    pub fn app_hart(&self, hart: usize) -> cpu::riscv::switch::AppHart {
        cpu::riscv::switch::AppHart {
            mtimecmp: Pa(0),
            msip: Pa(0),
            sleep_cap: 0,
            tick: 0,
            attrs: cpu::riscv::sv39::Attrs::Thead,
            pmp: cpu::riscv::pmp::C906,
            resettable: hart == HART_LITTLE,
        }
    }

    /// Brengt app-hart `hart` naar de parkeerlus van de boot-stub: de C906L
    /// uit reset op de reset-ingang, met zijn logische hart-id erbij (zijn
    /// `mhartid` leest 0, net als dat van de C906B).
    pub fn start_app_hart(&self, hart: usize) {
        if hart == HART_LITTLE {
            cpu::riscv::boot::set_reset_hart(hart);
            self.start_little(cpu::riscv::boot::reset_pc());
        }
    }

    /// Zet app-hart `hart` in reset; `true` als dat kon (alleen de C906L).
    pub fn hold_app_hart(&self, hart: usize) -> bool {
        if hart == HART_LITTLE {
            self.hold_little();
        }
        hart == HART_LITTLE
    }

    /// Geen schijf: er is (nog) geen SD-driver.
    pub fn probe_disk(&self) -> Result<Option<NoDisk>, Error> {
        Ok(None)
    }

    /// Start de C906L op `entry` via het resetblok: reset vast, de
    /// boot-vector-override aan, de vector, reset los. Een reset-start kent
    /// geen argument; het hart begint kaal (caches uit, T-Head-regime nul),
    /// en de boot-stub zet het regime (cpu/riscv/boot.rs, `thead`).
    pub fn start_little(&self, entry: u64) {
        dev::write32(C906L_RESET, dev::read32(C906L_RESET) & !RESET_BIT);
        dev::write32(SEC_SYS_CTRL, dev::read32(SEC_SYS_CTRL) | 1 << 13);
        dev::write32(SEC_SYS_VEC_LO, entry as u32);
        dev::write32(SEC_SYS_VEC_HI, (entry >> 32) as u32);
        dev::write32(C906L_RESET, dev::read32(C906L_RESET) | RESET_BIT);
    }

    /// Zet de C906L in reset: de harde intrekking op dit hart (het wist ook
    /// zijn PMP, gemeten 30-07: na een hart-reset leest pmpcfg0 weer 0).
    pub fn hold_little(&self) {
        dev::write32(C906L_RESET, dev::read32(C906L_RESET) & !RESET_BIT);
    }

    /// De watchdog-probe (Go, `WatchdogProbe`): CCVR aanraken (de lees die
    /// bus-fault als het blok dood is), TORR schrijven en teruglezen. TORR is
    /// inert zolang CR.enable uit staat, en de waarde is exact wat het
    /// wapenen er straks toch in zet. Vanaf het hart van de kern is dit een
    /// gok die de node kan kosten (een bus-fout overleeft de kern niet),
    /// daarom gaat de regel ervoor naar de console: stilte daarna wijst de
    /// dader aan.
    pub fn watchdog_probe(&self) -> bool {
        const TOP: u32 = 15; // 2^31 cycli op 25 MHz, ~86 s
        cpu::println!(
            "watchdog: probing the DW-WDT at {:#x} (silence after this line = the block is not there)",
            WDT.0
        );
        let _ = dev::read32(WDT.add(0x08));
        dev::write32(WDT.add(0x04), TOP | TOP << 4);
        dev::read32(WDT.add(0x04)) == TOP | TOP << 4
    }

    /// Het MAC-adres uit `hopos.mac`, anders afgeleid van `hopos.node`
    /// (`net::nodemac`), allebei uit het config-venster; zonder beide het
    /// ingebouwde adres, luid.
    fn node_mac() -> Mac {
        let (m, src) = net::nodemac::identity(boot_param("hopos.mac"), boot_param("hopos.node"));
        if src == net::nodemac::Source::Fallback {
            cpu::println!(
                "net: WARNING no hopos.mac and no hopos.node: the built-in MAC; a second LicheeRV on this LAN will collide HOPOS_MAC_FIXED"
            );
        }
        Mac(m)
    }
}

impl Default for LicheeRv {
    fn default() -> Self {
        Self::new()
    }
}

impl Board for LicheeRv {
    type Nic = Dwmac;
    type Sleeper = cpu::riscv::idle::RvSleeper;

    const NAME: &'static str = "licheerv";

    fn console(&self) -> fn(&[u8]) {
        console_write
    }

    fn privilege(&self, el: u8) -> Result<(), Error> {
        if el == 3 {
            Ok(())
        } else {
            Err(Error::Privilege { el })
        }
    }

    fn firmware(&self) -> &'static str {
        "boot: LicheeRV Nano (SG2002), machine mode monitor from the FIP (no SBI), app hart 1 (C906L)"
    }

    fn init_heap(&self, heap: &Heap) {
        let (start, end) = arch::heap_bounds();
        // SAFETY: `link-riscv.ld` (basis 0x8400_0000, maat 48 MB voor dit
        // board) legt de heap achter image en stack; tot `__heap_end`, het
        // begin van de DMA-regio, gebruikt niemand anders dat bereik.
        unsafe { heap.init(start, end) };
    }

    fn discover(&self, _dtb: u64) {
        cpu::riscv::idle::set_hz(TIMEBASE_HZ);
        match CLINT_DEV.probe(self.this_core(), csr::rdtime()) {
            Ok(()) => {
                CLINT_OK.store(true, Relaxed);
                cpu::println!("board: CLINT: mtimecmp writable, the kern sleeps on it (wfi)");
            }
            Err(e) => cpu::println!(
                "board: CLINT: {e}, hart sleep stays disabled, the kern polls HOPOS_CLINT_FAIL"
            ),
        }
        cpu::println!("{}", cpu::riscv::trng::WARNING);
        let ok = self.watchdog_probe();
        watchdog::probed(ok);
        cpu::println!(
            "watchdog: DW-WDT {}",
            if ok {
                "answers, the watchdog task may arm it"
            } else {
                "readback differs, it stays unarmed HOPOS_WD_PROBE_FAIL"
            }
        );
        match cfg::len() {
            Some(0) => cpu::println!(
                "config: no hopos.cfg in the image window (CFG= of image/licheerv-agent.sh), defaults HOPOS_CFG_NONE"
            ),
            Some(n) if cfg::text().is_empty() => cpu::println!(
                "config: {n} bytes in the image window are not UTF-8, ignored HOPOS_CFG_BAD"
            ),
            Some(n) => {
                cpu::println!("config: hopos.cfg from the image window, {n} bytes HOPOS_CFG_UP")
            }
            None => cpu::println!(
                "config: the image window claims more than {} bytes, ignored HOPOS_CFG_BAD",
                cfg::WINDOW - cfg::TEXT_OFF
            ),
        }
    }

    fn clock(&self) -> executor::Clock {
        cpu::riscv::idle::now
    }

    fn sleeper(&self) -> Self::Sleeper {
        let clint = CLINT_OK.load(Relaxed).then_some(CLINT_DEV);
        cpu::riscv::idle::RvSleeper::new(clint, self.this_core())
    }

    fn mem_total(&self) -> u64 {
        256 << 20
    }

    fn cores(&self) -> usize {
        2
    }

    fn core_class(&self, core: usize) -> CoreClass {
        if core == HART_LITTLE {
            CoreClass::Small
        } else {
            CoreClass::Big
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
        PLIC_DEV.set_context(machine_context(self.this_core()));
        cpu::println!("irq: {}", PLIC_DEV.describe());
        csr::mie_set(csr::MIP_MEIP | csr::MIP_MSIP);
        csr::restore(csr::MSTATUS_MIE);
        Ok(&cpu::irq::IRQ_PENDING)
    }

    fn dispatch_interrupts(&self) -> Dispatched {
        let mut d = Dispatched::default();
        cpu::riscv::trap::take_irq();
        CLINT_DEV.set_msip(self.this_core(), false);
        while let Some(l) = PLIC_DEV.claim() {
            // Niemand heeft een lijn: de dwmac pollt. Wat vuurt, gaat uit.
            PLIC_DEV.disable(l);
            PLIC_DEV.complete(Line(l.0));
            d.other += 1;
        }
        csr::mie_set(csr::MIP_MEIP | csr::MIP_MSIP);
        d
    }

    /// De ethernet-keten (Go, board/licheerv/hop/net.go): klokken, leeft de
    /// MAC, de ePHY aan, PHY-scan, autonegotiatie, DMA. Elke stap meldt zich
    /// met het getal erbij: een boot-cyclus is hier duur (kaart eruit, in de
    /// Mac, terug).
    fn probe_nic(&self) -> Result<Option<Self::Nic>, Error> {
        if NIC_CLAIMED.swap(true, Relaxed) {
            return Err(Error::Twice("probe_nic"));
        }
        if !ephy::clocks_on() {
            ephy::clocks_enable();
            wait_us(1_000);
            if !ephy::clocks_on() {
                return Err(Error::Nic("ethernet clock gates stay closed"));
            }
        }
        // SAFETY: GMAC is de dwmac van de SG2002 met open klokgates.
        let mut probe =
            unsafe { Probe::new(GMAC, driver_dwmac::CSR_250_300M, cpu::riscv::idle::now) };
        let v = probe.version();
        if v == 0 || v == 0xffff_ffff {
            return Err(Error::Nic(
                "no MAC at 0x04070000 (version reads 0 or all-ones)",
            ));
        }
        ephy::init(wait_us);
        wait_us(50_000);
        let Some(phy) = driver_mdio::scan(&mut probe) else {
            return Err(Error::Nic(
                "no PHY on the MDIO bus (ePHY init did not take)",
            ));
        };
        let link = driver_mdio::autoneg(
            &mut probe,
            phy.addr,
            false,
            cpu::riscv::idle::now,
            8_000_000_000,
        )
        .map_err(|_| Error::Nic("autonegotiation failed (cable plugged in?)"))?;
        cpu::println!(
            "net: dwmac version {v:#x}, PHY {} id {:04x}:{:04x}, link {link}",
            phy.addr,
            phy.id1,
            phy.id2
        );
        // SAFETY: NET_DMA is van deze driver alleen, lijn-gealigneerd en
        // onder 4 GB; de driver doet het cache-onderhoud (dev met `thead`).
        let nic = unsafe {
            probe.start(
                NET_DMA.base,
                NET_DMA.size,
                Self::node_mac(),
                link.mbps,
                link.full_duplex,
            )
        }
        .map_err(|_| Error::Nic("dwmac start failed"))?;
        Ok(Some(nic))
    }
}

mod arch {
    //! De grenzen van de heap uit het linkscript; op de host een stub.
    #[cfg(all(target_arch = "riscv64", target_os = "none"))]
    pub(crate) fn heap_bounds() -> (usize, usize) {
        unsafe extern "C" {
            /// Het begin van de heap.
            static __heap_start: u8;
            /// Het einde van de heap.
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
    fn classes_and_mode() {
        let b = LicheeRv::new();
        assert_eq!(b.core_class(1), CoreClass::Small);
        assert_eq!(b.core_class(0), CoreClass::Big);
        assert!(b.privilege(3).is_ok());
    }
}
