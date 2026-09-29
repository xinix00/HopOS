//! De GIC-400 (GICv2): distributor plus de memory-mapped CPU-interface van
//! de aanroepende core, als [`cpu::irq::Controller`]. De Pi 4 en de Pi 5
//! (BCM2711/BCM2712) hebben deze GIC; de v3-boards draaien `driver-gicv3`.
//! Registers en volgorde naar Linux' `drivers/irqchip/irq-gic.c` en
//! `include/linux/irqchip/arm-gic.h`.
//!
//! Dezelfde twee regels als de v3-driver: alleen de lijnen aanraken die wij
//! aanzetten (de firmware heeft er ook), en elke SPI expliciet naar déze
//! core routeren (GICD_ITARGETSR): een app-core is nooit een doel.
//! Non-secure Group 1: op de Pi's staat TF-A op EL3 en zijn de
//! Group-0-registers niet van ons; TF-A laat alle SPI's als Group 1 achter
//! (`gicv2_distif_init`) en heeft de CPU-interface van de boot-core al
//! aangezet. Wat [`Gic::init`] doet is idempotent: PMR open, EnableGrp1 in
//! GICC_CTLR en GICD_CTLR (NS-view bit 0).
//!
//! Geen SGI's: de kern wekt een app-core op een GIC-400 met SEV, want
//! GICD_ISENABLER0 is per core gebankt en alleen door die core zelf te
//! schrijven. Een PPI (de timer) kan HOP's core wel voor zichzelf aanzetten,
//! om dezelfde reden.

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
use core::mem::offset_of;
use core::sync::atomic::{AtomicU8, Ordering::Relaxed};
use cpu::irq::{Controller, Error, Line};
use dev::{Pa, Reg};

/// De eerste PPI.
pub const FIRST_PPI: u32 = 16;
/// De INTID van SPI 0.
pub const FIRST_SPI: u32 = 32;
/// IAR: 1020..1023 is "niets" (1023 = spurious).
pub const FIRST_SPECIAL: u32 = 1020;
/// GICD_INT_DEF_PRI: onder de firmware, boven de vloer.
const PRIORITY: u8 = 0xa0;
/// De NS-view van GICD_CTLR en GICC_CTLR: Group 1 aan.
const ENABLE_GRP1: u32 = 1;

/// De distributor (ARM IHI 0048B, tabel 4-1).
#[repr(C)]
struct Gicd {
    ctlr: Reg<u32>,
    typer: Reg<u32>,
    iidr: Reg<u32>,
    _r0: [u32; 29],
    igroupr: [Reg<u32>; 32],
    isenabler: [Reg<u32>; 32],
    icenabler: [Reg<u32>; 32],
    ispendr: [Reg<u32>; 32],
    icpendr: [Reg<u32>; 32],
    isactiver: [Reg<u32>; 32],
    icactiver: [Reg<u32>; 32],
    ipriorityr: [Reg<u8>; 1020],
    _r1: u32,
    itargetsr: [Reg<u8>; 1020],
    _r2: u32,
    icfgr: [Reg<u32>; 64],
}

const _: () = {
    assert!(offset_of!(Gicd, ctlr) == 0x000);
    assert!(offset_of!(Gicd, typer) == 0x004);
    assert!(offset_of!(Gicd, iidr) == 0x008);
    assert!(offset_of!(Gicd, igroupr) == 0x080);
    assert!(offset_of!(Gicd, isenabler) == 0x100);
    assert!(offset_of!(Gicd, icenabler) == 0x180);
    assert!(offset_of!(Gicd, ispendr) == 0x200);
    assert!(offset_of!(Gicd, icpendr) == 0x280);
    assert!(offset_of!(Gicd, isactiver) == 0x300);
    assert!(offset_of!(Gicd, icactiver) == 0x380);
    assert!(offset_of!(Gicd, ipriorityr) == 0x400);
    assert!(offset_of!(Gicd, itargetsr) == 0x800);
    assert!(offset_of!(Gicd, icfgr) == 0xc00);
};

/// De CPU-interface (ARM IHI 0048B, tabel 4-2).
#[repr(C)]
struct Gicc {
    ctlr: Reg<u32>,
    pmr: Reg<u32>,
    bpr: Reg<u32>,
    iar: Reg<u32>,
    eoir: Reg<u32>,
    rpr: Reg<u32>,
    hppir: Reg<u32>,
    _r0: [u32; 56],
    iidr: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Gicc, ctlr) == 0x00);
    assert!(offset_of!(Gicc, pmr) == 0x04);
    assert!(offset_of!(Gicc, iar) == 0x0c);
    assert!(offset_of!(Gicc, eoir) == 0x10);
    assert!(offset_of!(Gicc, hppir) == 0x18);
    assert!(offset_of!(Gicc, iidr) == 0xfc);
};

/// Eén GIC-400, gezien vanaf de core die hem opzet.
pub struct Gic {
    gicd: Pa,
    gicc: Pa,
    /// Het CPU-masker van deze core in GICD_ITARGETSR-termen; 0 = nog
    /// niet gelezen.
    mask: AtomicU8,
}

impl Gic {
    /// Een GIC met de distributor op `gicd` en de CPU-interface op `gicc`
    /// (op de Pi: basis + 0x1000 en + 0x2000).
    ///
    /// # Safety
    ///
    /// `gicd` (4 KB) en `gicc` (8 KB) zijn de gemapte GIC-400-blokken van
    /// dit board en blijven zolang het programma draait.
    #[must_use]
    pub const unsafe fn new(gicd: Pa, gicc: Pa) -> Self {
        Self {
            gicd,
            gicc,
            mask: AtomicU8::new(0),
        }
    }

    fn d(&self) -> &'static Gicd {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.gicd) }
    }

    fn c(&self) -> &'static Gicc {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.gicc) }
    }

    /// Zet de GIC op voor deze core: het eigen CPU-masker lezen (de
    /// gebankte target-bytes van SGI 0-3 geven elk het masker van de lezer,
    /// `gic_get_cpumask`), PMR open, Group 1 aan in CPU-interface en
    /// distributor. Idempotent. Een masker van 0 (een GIC die maar één
    /// interface heeft mag RAZ zijn) wordt interface 0.
    pub fn init(&self) {
        let d = self.d();
        let mut mask = 0u8;
        for t in d.itargetsr.iter().take(4) {
            mask |= t.read();
        }
        if mask == 0 {
            mask = 1;
        }
        self.mask.store(mask, Relaxed);
        let c = self.c();
        c.pmr.write(0xff);
        c.ctlr.update(|v| v | ENABLE_GRP1);
        d.ctlr.update(|v| v | ENABLE_GRP1);
        dev::mb();
    }

    /// Het CPU-masker van deze core (na [`init`](Self::init)).
    #[must_use]
    pub fn cpu_mask(&self) -> u8 {
        self.mask.load(Relaxed)
    }

    /// Maakt SPI `id` flankgevoelig (ICFGR bit 1 van zijn paar), vóór de
    /// enable: voor lijnen die een flank zijn (de MSI-vectoren van de MIP
    /// op de Pi 5).
    pub fn set_edge(&self, id: u32) {
        if !(FIRST_SPI..FIRST_SPECIAL).contains(&id) {
            return;
        }
        if let Some(r) = self.d().icfgr.get((id / 16) as usize) {
            r.update(|v| v | 2 << (2 * (id % 16)));
            dev::mb();
        }
    }

    /// Eén regel voor de bootlog.
    #[must_use]
    pub fn describe(&self) -> Describe {
        Describe {
            gicd: self.gicd,
            gicd_iidr: self.d().iidr.read(),
            gicd_ctlr: self.d().ctlr.read(),
            lines: (self.d().typer.read() & 0x1f).saturating_add(1) * 32,
            gicc: self.gicc,
            gicc_iidr: self.c().iidr.read(),
            gicc_ctlr: self.c().ctlr.read(),
            mask: self.cpu_mask(),
        }
    }
}

impl Controller for Gic {
    /// Prioriteit, route naar deze core, dan pas scherp: route en
    /// prioriteit staan vóór de enable, anders kan de lijn één keer
    /// verkeerd afgaan. Een PPI heeft geen route (hij is van de core zelf).
    fn enable(&self, l: Line) -> Result<(), Error> {
        let id = l.0;
        if !(FIRST_PPI..FIRST_SPECIAL).contains(&id) {
            return Err(Error::Rejected { line: id });
        }
        let d = self.d();
        let i = id as usize;
        let (Some(pri), Some(tgt), Some(en)) = (
            d.ipriorityr.get(i),
            d.itargetsr.get(i),
            d.isenabler.get(i / 32),
        ) else {
            return Err(Error::Rejected { line: id });
        };
        pri.write(PRIORITY);
        if id >= FIRST_SPI {
            tgt.write(self.cpu_mask().max(1));
        }
        dev::mb();
        en.write(1 << (id % 32));
        dev::mb();
        Ok(())
    }

    /// ICENABLER is write-1-to-clear: nooit lezen-aanpassen-schrijven, dat
    /// zou de buren raken.
    fn disable(&self, l: Line) {
        if !(FIRST_PPI..FIRST_SPECIAL).contains(&l.0) {
            return;
        }
        if let Some(r) = self.d().icenabler.get((l.0 / 32) as usize) {
            r.write(1 << (l.0 % 32));
            dev::mb();
        }
    }

    /// GICC_IAR: de claim. 1020 en hoger is "niets".
    fn claim(&self) -> Option<Line> {
        let id = self.c().iar.read() & 0x3ff;
        (id < FIRST_SPECIAL).then_some(Line(id))
    }

    /// GICC_EOIR met dezelfde INTID (EOImode 0: priority drop én
    /// deactivate). De CPUID-bits zijn alleen voor SGI's, en die claimen
    /// wij niet.
    fn complete(&self, l: Line) {
        self.c().eoir.write(l.0);
    }
}

/// De bootlog-regel van [`Gic::describe`].
#[derive(Debug, Clone, Copy)]
pub struct Describe {
    gicd: Pa,
    gicd_iidr: u32,
    gicd_ctlr: u32,
    lines: u32,
    gicc: Pa,
    gicc_iidr: u32,
    gicc_ctlr: u32,
    mask: u8,
}

impl fmt::Display for Describe {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "GIC-400: GICD {:#x} (IIDR {:#x}, CTLR {:#x}, {} lines), GICC {:#x} (IIDR {:#x}, CTLR {:#x}), cpu mask {:#x}",
            self.gicd.0,
            self.gicd_iidr,
            self.gicd_ctlr,
            self.lines,
            self.gicc.0,
            self.gicc_iidr,
            self.gicc_ctlr,
            self.mask
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mem() -> (Vec<u64>, Vec<u64>) {
        (vec![0; 0x1000 / 8], vec![0; 0x2000 / 8])
    }

    fn pa(v: &mut [u64]) -> Pa {
        Pa(v.as_mut_ptr() as usize as u64)
    }

    #[test]
    fn init_reads_the_cpu_mask_and_opens_group_1() {
        let (mut d, mut c) = mem();
        let (dp, cp) = (pa(&mut d), pa(&mut c));
        // De gebankte target-bytes van SGI 0-3 lezen "interface 2".
        dev::write32(dp.add(0x800), 0x0404_0404);
        // SAFETY: de vectoren leven de hele test.
        let g = unsafe { Gic::new(dp, cp) };
        g.init();
        assert_eq!(g.cpu_mask(), 4);
        assert_eq!(dev::read32(cp.add(0x04)), 0xff);
        assert_eq!(dev::read32(cp.add(0x00)), 1);
        assert_eq!(dev::read32(dp.add(0x000)), 1);
        // Een GIC die RAZ leest: interface 0.
        let (mut d, mut c) = mem();
        // SAFETY: zie hierboven.
        let g = unsafe { Gic::new(pa(&mut d), pa(&mut c)) };
        g.init();
        assert_eq!(g.cpu_mask(), 1);
    }

    #[test]
    fn spi_is_routed_prioritised_then_enabled() {
        let (mut d, mut c) = mem();
        let dp = pa(&mut d);
        dev::write32(dp.add(0x800), 0x0101_0101);
        // SAFETY: zie hierboven.
        let g = unsafe { Gic::new(dp, pa(&mut c)) };
        g.init();
        // De GEM op de Pi 5: MIP-vector 6 wordt SPI 134, INTID 166.
        g.enable(Line(166)).unwrap();
        assert_eq!(dev::read8(dp.add(0x400 + 166)), PRIORITY);
        assert_eq!(dev::read8(dp.add(0x800 + 166)), 1);
        assert_eq!(dev::read32(dp.add(0x100 + 4 * 5)), 1 << (166 % 32));
        g.set_edge(166);
        assert_eq!(dev::read32(dp.add(0xc00 + 4 * 10)), 2 << (2 * (166 % 16)));
        g.disable(Line(166));
        assert_eq!(dev::read32(dp.add(0x180 + 4 * 5)), 1 << (166 % 32));
    }

    #[test]
    fn ppi_has_no_route_and_sgis_are_refused() {
        let (mut d, mut c) = mem();
        let dp = pa(&mut d);
        // SAFETY: zie hierboven.
        let g = unsafe { Gic::new(dp, pa(&mut c)) };
        g.init();
        g.enable(Line(30)).unwrap();
        assert_eq!(dev::read8(dp.add(0x800 + 30)), 0);
        assert_eq!(dev::read32(dp.add(0x100)), 1 << 30);
        assert_eq!(g.enable(Line(5)), Err(Error::Rejected { line: 5 }));
        assert_eq!(g.enable(Line(1020)), Err(Error::Rejected { line: 1020 }));
        // Een SGI of speciale lijn raakt ICFGR niet.
        g.set_edge(5);
        assert_eq!(dev::read32(dp.add(0xc00)), 0);
    }

    #[test]
    fn claim_skips_specials_and_complete_writes_eoir() {
        let (mut d, mut c) = mem();
        let cp = pa(&mut c);
        // SAFETY: zie hierboven.
        let g = unsafe { Gic::new(pa(&mut d), cp) };
        dev::write32(cp.add(0x0c), 1023);
        assert_eq!(g.claim(), None);
        dev::write32(cp.add(0x0c), 0x0c00 | 97);
        assert_eq!(g.claim(), Some(Line(97)));
        g.complete(Line(97));
        assert_eq!(dev::read32(cp.add(0x10)), 97);
        assert!(g.describe().to_string().starts_with("GIC-400: GICD"));
    }
}
