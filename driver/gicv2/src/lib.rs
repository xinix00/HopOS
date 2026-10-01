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
//! Eén SGI: de kick van de OS-core (PORT.md beslissing 2). De switcher van
//! een app-core schrijft hem als 32-bit woord naar GICD_SGIR
//! ([`Gic::sgir_pa`], [`Gic::sgi_word`]), de OS-core zet hem voor zichzelf
//! scherp ([`Controller::enable`]: GICD_IPRIORITYR0..3 en ISENABLER0 zijn per
//! core gebankt, dus alleen de ontvanger kan dat) en ziet hem met
//! [`Gic::hppir`] zonder te claimen. De andere kant op (de kern wekt een
//! app-core) blijft SEV: een app-core is nooit een doel van de GIC. Een PPI
//! (de timers) zet de OS-core om dezelfde bank-reden zelf aan.

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
use core::sync::atomic::{AtomicU8, AtomicU32, Ordering::Relaxed};
use cpu::irq::{Controller, Error, Line, Trigger};
use dev::{Pa, Reg};

/// Het aantal SGI's (INTID 0..=15).
pub const SGI_COUNT: u32 = 16;
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
    _r3: [u32; 128],
    sgir: Reg<u32>,
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
    assert!(offset_of!(Gicd, sgir) == 0xf00);
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
    /// De laatste rauwe GICC_IAR van [`claim`](Controller::claim): bij een
    /// SGI dragen bits 12:10 de bron-core, en GICC_EOIR wil precies die
    /// waarde terug (IHI 0048B 4.4.5). Eén claimer (de OS-core), en claim
    /// en complete volgen elkaar in de dispatch-ronde op.
    iar: AtomicU32,
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
            iar: AtomicU32::new(FIRST_SPECIAL + 3),
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

    /// Veegt alle SPI's schoon vóór [`init`](Self::init), op de boot-core:
    /// enable, pending en actief gewist (write-1-to-clear), zodat niets van
    /// een vorige kern blijft hangen. GEMETEN 30-09 op de Pi 5, de flip
    /// naar generatie 2: de NIC-lijn (SPI 166, MSI-X via de MIP) gaf na de
    /// sprong precies één interrupt en daarna nooit meer, `irq(nic=1)`
    /// honderd tikken lang, DHCP kreeg niets en de watchdog resette de
    /// node. De vertrekkende kern zat op 800 MHz midden in een afhandeling
    /// en liet de lijn actief achter; een actieve flank-interrupt wordt
    /// niet opnieuw gemeld tot zijn EOI, en die EOI komt van een kern die
    /// er niet meer is. Alleen de SPI-woorden: de eerste 32 lijnen (SGI's,
    /// PPI's) zijn gebankt per core en van de app-cores die doordraaien.
    /// Woorden boven het lijnental van de GIC zijn WI.
    pub fn quiesce_spis(&self) {
        let d = self.d();
        for i in 1..32 {
            if let Some(r) = d.icenabler.get(i) {
                r.write(0xffff_ffff);
            }
            if let Some(r) = d.icpendr.get(i) {
                r.write(0xffff_ffff);
            }
            if let Some(r) = d.icactiver.get(i) {
                r.write(0xffff_ffff);
            }
        }
        dev::mb();
    }

    /// Het CPU-masker van deze core (na [`init`](Self::init)).
    #[must_use]
    pub fn cpu_mask(&self) -> u8 {
        self.mask.load(Relaxed)
    }

    /// De PA van GICD_SGIR: waar de switcher van een app-core (EL2, MMU
    /// uit) het woord van [`sgi_word`](Self::sgi_word) heen schrijft.
    #[must_use]
    pub fn sgir_pa(&self) -> Pa {
        self.gicd.add(offset_of!(Gicd, sgir) as u64)
    }

    /// Het GICD_SGIR-woord dat SGI `intid` naar precies deze core stuurt
    /// (aanroepen op de ontvanger, na [`init`](Self::init)):
    /// TargetListFilter 0 (de lijst), CPUTargetList in 23:16 = het eigen
    /// masker, dus `(1 << (16 + cpu)) | intid`, en NSATT 0.
    #[must_use]
    pub fn sgi_word(&self, intid: u32) -> u32 {
        (u32::from(self.cpu_mask().max(1)) << 16) | (intid % SGI_COUNT)
    }

    /// Stuurt een SGI met het woord van [`sgi_word`](Self::sgi_word).
    pub fn send_sgi(&self, word: u32) {
        dev::mb();
        self.d().sgir.write(word);
        dev::mb();
    }

    /// De hoogste pending INTID van deze core (GICC_HPPIR), zonder hem te
    /// claimen; 1023 = niets. Bij een SGI vallen de bron-bits (12:10) weg.
    #[must_use]
    pub fn hppir(&self) -> u32 {
        self.c().hppir.read() & 0x3ff
    }

    /// ICFGR van SPI `id`: bit 1 van zijn paar is de flank (IHI 0048B
    /// 4.3.13; bit 0 is gereserveerd). Een SGI is altijd een flank en een
    /// PPI is op de GIC-400 vast: die blijven zoals ze zijn.
    fn config(&self, id: u32, edge: bool) {
        if !(FIRST_SPI..FIRST_SPECIAL).contains(&id) {
            return;
        }
        if let Some(r) = self.d().icfgr.get((id / 16) as usize) {
            let bit = 2 << (2 * (id % 16));
            r.update(|v| if edge { v | bit } else { v & !bit });
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
    /// De soort van een SPI in GICD_ICFGR, vóór de enable (een wissel op
    /// een scherpe lijn is UNPREDICTABLE, IHI 0048B 4.3.13). Een SPI staat
    /// na reset op level; een MSI via een brug is een flank. Les van 30-09
    /// (de eerste Pi 5-boot): een level-SPI achter een MSI-puls blijft
    /// staan tot iemand hem laat zakken.
    fn set_trigger(&self, l: Line, t: Trigger) -> Result<(), Error> {
        if l.0 >= FIRST_SPECIAL {
            return Err(Error::Rejected { line: l.0 });
        }
        self.config(l.0, t == Trigger::Edge);
        Ok(())
    }

    /// Prioriteit, route naar deze core, dan pas scherp: route en
    /// prioriteit staan vóór de enable, anders kan de lijn één keer
    /// verkeerd afgaan. Een SGI of PPI heeft geen route (hij is van de core
    /// zelf, en zijn prioriteit en enable zijn gebankt: dit geldt alleen
    /// voor de aanroepende core). Op de GIC-400 zijn SGI's altijd scherp
    /// (ISENABLER0[15:0] is RAO/WI); de schrijf is dan onschadelijk.
    fn enable(&self, l: Line) -> Result<(), Error> {
        let id = l.0;
        if id >= FIRST_SPECIAL {
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
        if l.0 >= FIRST_SPECIAL {
            return;
        }
        if let Some(r) = self.d().icenabler.get((l.0 / 32) as usize) {
            r.write(1 << (l.0 % 32));
            dev::mb();
        }
    }

    /// GICC_IAR: de claim. 1020 en hoger is "niets". De rauwe waarde blijft
    /// bewaard voor [`complete`](Controller::complete).
    fn claim(&self) -> Option<Line> {
        let raw = self.c().iar.read();
        let id = raw & 0x3ff;
        if id >= FIRST_SPECIAL {
            return None;
        }
        self.iar.store(raw & 0x1fff, Relaxed);
        Some(Line(id))
    }

    /// GICC_EOIR (EOImode 0: priority drop én deactivate). Bij een SGI met
    /// de bron-core erbij, uit de laatste claim van diezelfde INTID: een
    /// EOI met de verkeerde CPUID deactiveert niets, en de SGI blijft dan
    /// actief tot de volgende boot.
    fn complete(&self, l: Line) {
        let raw = self.iar.load(Relaxed);
        let v = if l.0 < SGI_COUNT && raw & 0x3ff == l.0 {
            raw
        } else {
            l.0
        };
        self.c().eoir.write(v);
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
    fn quiesce_clears_the_spi_words_only() {
        let (mut d, mut c) = mem();
        let (dp, cp) = (pa(&mut d), pa(&mut c));
        // SAFETY: het nep-blok van `mem`, groot genoeg voor beide banken.
        let g = unsafe { Gic::new(dp, cp) };
        g.quiesce_spis();
        // Write-1-to-clear op ijzer; het nep-blok bewaart wat er geschreven is.
        assert_eq!(dev::read32(dp.add(0x184)), 0xffff_ffff, "ICENABLER[1]");
        assert_eq!(dev::read32(dp.add(0x284)), 0xffff_ffff, "ICPENDR[1]");
        assert_eq!(dev::read32(dp.add(0x384)), 0xffff_ffff, "ICACTIVER[1]");
        assert_eq!(dev::read32(dp.add(0x1fc)), 0xffff_ffff, "ICENABLER[31]");
        assert_eq!(
            dev::read32(dp.add(0x180)),
            0,
            "het SGI/PPI-woord is gebankt: niet aanraken"
        );
        assert_eq!(dev::read32(dp.add(0x280)), 0);
        assert_eq!(dev::read32(dp.add(0x380)), 0);
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
        g.set_trigger(Line(166), Trigger::Edge).unwrap();
        assert_eq!(dev::read32(dp.add(0xc00 + 4 * 10)), 2 << (2 * (166 % 16)));
        g.disable(Line(166));
        assert_eq!(dev::read32(dp.add(0x180 + 4 * 5)), 1 << (166 % 32));
    }

    // De Pi 5-weg van 30-09: het board registreert de GEM-lijn (MIP-vector
    // 6, INTID 166) als flank bij de dispatcher, en die zet ICFGR vóór de
    // enable; een level-lijn (de Pi 4-GENET, SPI 157) blijft level, ook
    // als hij eerder flank stond.
    #[test]
    fn the_dispatcher_sets_the_trigger_before_the_enable() {
        use cpu::irq::Dispatcher;
        let d: &'static mut [u64] = Vec::leak(vec![0; 0x1000 / 8]);
        let c: &'static mut [u64] = Vec::leak(vec![0; 0x2000 / 8]);
        let dp = pa(d);
        dev::write32(dp.add(0x800), 0x0101_0101);
        // SAFETY: de gelekte vectoren leven de hele test.
        let g: &'static Gic = Box::leak(Box::new(unsafe { Gic::new(dp, pa(c)) }));
        g.init();
        let disp: &'static Dispatcher = Box::leak(Box::new(Dispatcher::new()));
        disp.use_controller(g);
        disp.enable_as(Line(166), Trigger::Edge, None).unwrap();
        let icfgr10 = dev::read32(dp.add(0xc00 + 4 * 10));
        assert_eq!(icfgr10 & (2 << (2 * (166 % 16))), 2 << (2 * (166 % 16)));
        assert_eq!(dev::read32(dp.add(0x100 + 4 * 5)), 1 << (166 % 32));
        // SPI 157 (INTID 189): stond op flank, wordt level.
        dev::write32(dp.add(0xc00 + 4 * 11), u32::MAX);
        disp.enable(Line(189), None).unwrap();
        let icfgr11 = dev::read32(dp.add(0xc00 + 4 * 11));
        assert_eq!(icfgr11 & (2 << (2 * (189 % 16))), 0);
        assert_eq!(icfgr11 | (2 << (2 * (189 % 16))), u32::MAX);
        assert_eq!(
            g.set_trigger(Line(1023), Trigger::Edge),
            Err(Error::Rejected { line: 1023 })
        );
    }

    #[test]
    fn ppi_and_sgi_have_no_route_and_specials_are_refused() {
        let (mut d, mut c) = mem();
        let dp = pa(&mut d);
        // SAFETY: zie hierboven.
        let g = unsafe { Gic::new(dp, pa(&mut c)) };
        g.init();
        g.enable(Line(30)).unwrap();
        assert_eq!(dev::read8(dp.add(0x800 + 30)), 0);
        assert_eq!(dev::read32(dp.add(0x100)), 1 << 30);
        // De kick-SGI: prioriteit en enable, geen target.
        g.enable(Line(8)).unwrap();
        assert_eq!(dev::read8(dp.add(0x400 + 8)), PRIORITY);
        assert_eq!(dev::read8(dp.add(0x800 + 8)), 0);
        assert_eq!(dev::read32(dp.add(0x100)), 1 << 8);
        assert_eq!(g.enable(Line(1020)), Err(Error::Rejected { line: 1020 }));
        // Een SGI of speciale lijn raakt ICFGR niet.
        g.set_trigger(Line(5), Trigger::Edge).unwrap();
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

    #[test]
    fn sgi_word_aims_at_this_core_and_eoi_keeps_the_source() {
        let (mut d, mut c) = mem();
        let (dp, cp) = (pa(&mut d), pa(&mut c));
        // Deze core is interface 2 (masker 0x4).
        dev::write32(dp.add(0x800), 0x0404_0404);
        // SAFETY: zie hierboven.
        let g = unsafe { Gic::new(dp, cp) };
        g.init();
        assert_eq!(g.sgir_pa(), dp.add(0xf00));
        assert_eq!(g.sgi_word(8), (1 << (16 + 2)) | 8);
        g.send_sgi(g.sgi_word(8));
        assert_eq!(dev::read32(dp.add(0xf00)), 0x0004_0008);
        // HPPIR zonder de bron-bits.
        dev::write32(cp.add(0x18), (1 << 10) | 8);
        assert_eq!(g.hppir(), 8);
        // Een SGI van core 1: de EOI draagt de bron terug.
        dev::write32(cp.add(0x0c), (1 << 10) | 8);
        assert_eq!(g.claim(), Some(Line(8)));
        g.complete(Line(8));
        assert_eq!(dev::read32(cp.add(0x10)), (1 << 10) | 8);
        // Een SPI daarna: kale INTID.
        dev::write32(cp.add(0x0c), 97);
        assert_eq!(g.claim(), Some(Line(97)));
        g.complete(Line(97));
        assert_eq!(dev::read32(cp.add(0x10)), 97);
    }
}
