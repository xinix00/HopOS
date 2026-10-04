//! De distributor die de GIC-400 (v2, `driver-gicv2`) en de GICv3 delen:
//! de registers tot en met GICD_ICFGR (IHI 0048B tabel 4-1, IHI 0069
//! tabel 12-25: dezelfde plekken) en wat beide ermee doen. De vorm van
//! Linux' `irq-gic-common.c`: één keer, en elke versie zet er zijn eigen
//! deel omheen (v2: ITARGETSR en GICD_SGIR; v3: IGROUPR en IROUTER).

use dev::Reg;

/// De INTID van SPI 0.
pub const FIRST_SPI: u32 = 32;
/// Het aantal SGI's (INTID 0..=15): de IPI's van de GIC.
pub const SGI_COUNT: u32 = 16;
/// De eerste speciale INTID (1020-1023): "niets" bij een claim.
pub const FIRST_SPECIAL: u32 = 1020;

/// De prioriteit die wij onze lijnen geven (Linux' GICD_INT_DEF_PRI):
/// midden in het bereik, zodat een PMR van 0xff ze doorlaat en de firmware
/// er nog boven en onder kan.
pub const PRIORITY: u8 = 0xa0;

/// De gedeelde registers van de distributor, 0x000 tot 0xd00.
#[repr(C)]
pub struct Gicd {
    /// GICD_CTLR.
    pub ctlr: Reg<u32>,
    /// GICD_TYPER.
    pub typer: Reg<u32>,
    /// GICD_IIDR.
    pub iidr: Reg<u32>,
    _r0: [u32; 29],
    /// GICD_IGROUPR.
    pub igroupr: [Reg<u32>; 32],
    /// GICD_ISENABLER.
    pub isenabler: [Reg<u32>; 32],
    /// GICD_ICENABLER (write-1-to-clear).
    pub icenabler: [Reg<u32>; 32],
    /// GICD_ISPENDR.
    pub ispendr: [Reg<u32>; 32],
    /// GICD_ICPENDR (write-1-to-clear).
    pub icpendr: [Reg<u32>; 32],
    /// GICD_ISACTIVER.
    pub isactiver: [Reg<u32>; 32],
    /// GICD_ICACTIVER (write-1-to-clear).
    pub icactiver: [Reg<u32>; 32],
    /// GICD_IPRIORITYR, een byte per INTID.
    pub ipriorityr: [Reg<u8>; 1020],
    _r1: u32,
    /// GICD_ITARGETSR, een byte per INTID: v2. Op een v3 met affinity
    /// routing RES0 (de route staat in IROUTER).
    pub itargetsr: [Reg<u8>; 1020],
    _r2: u32,
    /// GICD_ICFGR, twee bits per INTID.
    pub icfgr: [Reg<u32>; 64],
}

const _: () = {
    use core::mem::offset_of;
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
    assert!(core::mem::size_of::<Gicd>() == 0xd00);
};

impl Gicd {
    /// Zet lijn `id` scherp: onze prioriteit, dan pas de enable. Wat de
    /// versie vóór de enable moet zetten (de route, de groep), heeft de
    /// aanroeper al geschreven. `false` = geen lijn van de distributor.
    pub fn enable(&self, id: u32) -> bool {
        let i = id as usize;
        let (Some(pri), Some(en)) = (self.ipriorityr.get(i), self.isenabler.get(i / 32)) else {
            return false;
        };
        pri.write(PRIORITY);
        dev::mb();
        en.write(1 << (id % 32));
        dev::mb();
        true
    }

    /// Zet lijn `id` uit. ICENABLER is write-1-to-clear: nooit
    /// lezen-aanpassen-schrijven, dat zou de buren raken.
    pub fn disable(&self, id: u32) {
        if let Some(r) = self.icenabler.get((id / 32) as usize) {
            r.write(1 << (id % 32));
            dev::mb();
        }
    }

    /// ICFGR van SPI `id`: bit 1 van zijn paar is de flank (IHI 0048B
    /// 4.3.13; bit 0 is gereserveerd). Een SGI is altijd een flank en een
    /// PPI is vast: die blijven zoals ze zijn.
    pub fn set_edge(&self, id: u32, edge: bool) {
        if !(FIRST_SPI..FIRST_SPECIAL).contains(&id) {
            return;
        }
        if let Some(r) = self.icfgr.get((id / 16) as usize) {
            let bit = 2 << (2 * (id % 16));
            r.update(|v| if edge { v | bit } else { v & !bit });
            dev::mb();
        }
    }
}
