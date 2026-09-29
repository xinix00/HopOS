//! De identity map van de Mac mini: 48 bits VA, 4 KB-korrel, gebouwd vóór
//! de MMU aangaat (MMU uit, dus alles is dan Device-nGnRnE en elke
//! toegang is een gealigneerde `dev::write64`).
//!
//! Waarom 48 bits: Apple legt het DRAM op 1 TiB (`0x100_0000_0000`); een
//! 39-bit-wereld (die van `cpu::boot`, en tamago's) reikt tot 512 GB. De
//! Go-kern had er een tamago-fork voor nodig (`hopos-highram`); hier is het
//! gewoon een L0-tabel.
//!
//! De kaart:
//!
//! - L0[0] naar een L1 met 512 blokken van 1 GB Device: alle MMIO van deze
//!   SoC ligt onder 512 GB (AIC 0x3_8100_0000, APCIe-ECAM 0x1c_b000_0000,
//!   het 64-bit PCIe-venster 0xb_c000_0000);
//! - L0[2] naar een L1 met per GB DRAM een L2 van 2 MB-blokken. GEMETEN
//!   28-08 (boot 4 en 5): een 1 GB-blokdescriptor met een uitvoeradres
//!   boven 2^40 geeft op dit silicium een "address size fault, level 0",
//!   hoewel hij welgevormd is; dezelfde GB via een L2-tabel werkt. Regel
//!   voor dit board: het DRAM altijd via 2 MB-blokken ([`dram_block`]
//!   weigert een 1 GB-blok daar);
//! - binnen het DRAM is alles Device (zoals de Go-kern het mapte: geen
//!   speculatieve toegang naar carve-outs van iBoot, en de firmware-
//!   structuren boot_args en de ADT zijn met gealigneerde loads prima te
//!   lezen; de target heeft `+strict-align`), behalve het kernvenster: de
//!   kern-RAM en de loader-regio Normal WB, de DMA-regio Normal-NC (GEMETEN
//!   03-09: Device kost ~290 ns per 8-byte-load, 14-27 MB/s; NC is coherent
//!   met de tg3 en de ANS zonder onderhoud), het kooi-venster Device.
//!
//! De tabellen liggen in de BSS van het image (Normal WB zodra de MMU aan
//! is; de table-walker leest cacheable met IRGN/ORGN = WB). De bouw draait
//! met de MMU uit, dus ze staan meteen in het geheugen.

use crate::{DMA, DRAM_BASE, KERN_RAM, LOADER};
use cpu::boot::{ATTR_DEVICE, ATTR_NORMAL, ATTR_NORMAL_NC, block};
use dev::Pa;

/// Geldig, tabel.
const TABLE: u64 = 0b11;
/// Eén GB.
pub(crate) const GB: u64 = 1 << 30;
/// Twee MB.
pub(crate) const MB2: u64 = 2 << 20;
/// Zoveel GB DRAM mappen we hoogstens: de mini bestaat in 16, 24 en 32 GB;
/// 64 laat ruimte zonder de BSS op te blazen (64 L2-tabellen = 256 KB).
pub(crate) const MAX_DRAM_GB: u64 = 64;
/// Tabellen: L0, de lage L1, de DRAM-L1, en één L2 per GB.
pub(crate) const TABLES: usize = 3 + MAX_DRAM_GB as usize;

const _: () = {
    // Het DRAM is L0-ingang 2 en begint op een GB-grens.
    assert!(DRAM_BASE >> 39 == 2 && DRAM_BASE.is_multiple_of(GB));
    // De lage L1 dekt alles onder 2^39 en dus nooit een adres boven 2^40.
    assert!(512 * GB == 1 << 39);
};

/// TCR_EL2 onder E2H = 1 (de vorm van TCR_EL1): T0SZ 16 (48 bits), walks
/// WB-WA inner shareable, TG0 4 KB, EPD1 (geen TTBR1-walks), TG1 4 KB (een
/// geldige waarde, ook al is hij uit), en IPS uit `parange`.
///
/// Apple's EL2 is VHE-only: E2H is RES1 (GEMETEN 28-08, boot 4: een write
/// met E2H = 0 leest 1 terug). De niet-VHE-vorm van `cpu::boot` (PS op bit
/// 16, RES1 op 23 en 31) zou hier T1SZ en A1 zetten.
#[must_use]
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de ingang gebruikt hem (en de tests)
)]
pub(crate) const fn tcr(parange: u64) -> u64 {
    let ips = if parange > 5 { 5 } else { parange };
    16 | (1 << 8) | (1 << 10) | (3 << 12) | (1 << 23) | (2 << 30) | (ips << 32)
}

/// SCTLR_EL2 onder E2H = 1 (de vorm van SCTLR_EL1): M, C, SA, I, plus de
/// RES1-set van die vorm (EOS, TSCXT, EIS, SPAN, nTLSMD, LSMAOE) en nTWI,
/// nTWE. Uitlijncontrole uit, little-endian.
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de ingang gebruikt hem (en de tests)
)]
pub(crate) const SCTLR: u64 =
    0x30d0_0800 | (1 << 16) | (1 << 18) | 1 | (1 << 2) | (1 << 3) | (1 << 12);

/// Het attribuut van het 2 MB-blok op `pa` in het DRAM.
#[must_use]
pub(crate) const fn dram_attr(pa: u64) -> u64 {
    if in_region(pa, KERN_RAM.base.0, KERN_RAM.size) || in_region(pa, LOADER.base.0, LOADER.size) {
        ATTR_NORMAL
    } else if in_region(pa, DMA.base.0, DMA.size) {
        ATTR_NORMAL_NC
    } else {
        ATTR_DEVICE
    }
}

const fn in_region(pa: u64, base: u64, size: u64) -> bool {
    pa >= base && pa < base + size
}

/// De blokdescriptor van het 2 MB-blok op `pa` in het DRAM. `None` voor een
/// adres dat niet 2 MB-gealigneerd is.
#[must_use]
pub(crate) const fn dram_block(pa: u64) -> Option<u64> {
    if !pa.is_multiple_of(MB2) {
        return None;
    }
    Some(block(pa, dram_attr(pa)))
}

/// Hoeveel GB DRAM we mappen: het fysieke RAM uit boot_args, naar boven op
/// een GB, minstens tot het einde van het kernvenster en hoogstens
/// [`MAX_DRAM_GB`]. Zonder boot_args (0) alleen tot en met het venster.
#[must_use]
pub(crate) const fn dram_gb(mem_size_actual: u64) -> u64 {
    let need = (crate::WINDOW_END - DRAM_BASE).div_ceil(GB);
    let have = mem_size_actual.div_ceil(GB);
    let n = if have > need { have } else { need };
    if n > MAX_DRAM_GB { MAX_DRAM_GB } else { n }
}

/// Bouwt de map in de pool `[pool, pool + TABLES * 4096)` en geeft de L0
/// (voor TTBR0_EL2). Schrijft alleen met `dev::write64`: dit draait met de
/// MMU uit.
///
/// # Safety
///
/// De pool is 4 KB-gealigneerd, [`TABLES`] pagina's groot, van niemand
/// anders, en de MMU gebruikt hem nog niet.
pub(crate) unsafe fn build(pool: u64, mem_size_actual: u64) -> u64 {
    let page = |i: u64| Pa(pool + i * 4096);
    for i in 0..TABLES as u64 {
        for e in 0..512 {
            dev::write64(page(i).add(8 * e), 0);
        }
    }
    let (l0, lo, hi) = (page(0), page(1), page(2));
    dev::write64(l0, lo.0 | TABLE);
    dev::write64(l0.add(8 * (DRAM_BASE >> 39)), hi.0 | TABLE);
    for g in 0..512 {
        dev::write64(lo.add(8 * g), block(g * GB, ATTR_DEVICE));
    }
    for g in 0..dram_gb(mem_size_actual) {
        let l2 = page(3 + g);
        dev::write64(hi.add(8 * g), l2.0 | TABLE);
        for j in 0..512 {
            let pa = DRAM_BASE + g * GB + j * MB2;
            if let Some(d) = dram_block(pa) {
                dev::write64(l2.add(8 * j), d);
            }
        }
    }
    l0.0
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{ADMIN, WINDOW_END};

    fn walk(pool: &[u64], va: u64) -> Option<u64> {
        let base = pool.as_ptr() as u64;
        let at = |pa: u64, i: u64| pool[((pa - base) / 8 + i) as usize];
        let l0 = at(base, (va >> 39) & 511);
        if l0 & 3 != TABLE {
            return None;
        }
        let l1 = at(l0 & 0xffff_ffff_f000, (va >> 30) & 511);
        match l1 & 3 {
            0b01 => Some(l1),
            TABLE => {
                let l2 = at(l1 & 0xffff_ffff_f000, (va >> 21) & 511);
                (l2 & 3 == 0b01).then_some(l2)
            }
            _ => None,
        }
    }

    #[test]
    fn the_map_follows_the_plan() {
        let mut pool = vec![0u64; TABLES * 512 + 512];
        let start = (pool.as_ptr() as u64).next_multiple_of(4096);
        let off = ((start - pool.as_ptr() as u64) / 8) as usize;
        // SAFETY: de pool is een Vec van de test, 4 KB-gealigneerd gesneden.
        let root = unsafe { build(start, 24 << 30) };
        assert_eq!(root, start);
        let tables = &mut pool[off..];
        // MMIO: 1 GB Device, AIC en ECAM.
        let aic = walk(tables, 0x3_8100_0000).unwrap();
        assert_eq!(aic & 0xffff_c000_0000, 0x3_8000_0000 & !(GB - 1));
        assert_eq!((aic >> 2) & 7, ATTR_DEVICE);
        assert!(walk(tables, 0x1c_b000_0000).is_some());
        // Het DRAM: nooit een 1 GB-blok boven 2^40.
        let fw = walk(tables, DRAM_BASE + 0x137_4000).unwrap();
        assert_eq!(fw & 0xffff_ffe0_0000, DRAM_BASE + 0x120_0000);
        assert_eq!((fw >> 2) & 7, ATTR_DEVICE);
        assert_eq!(
            (walk(tables, KERN_RAM.base.0).unwrap() >> 2) & 7,
            ATTR_NORMAL
        );
        assert_eq!((walk(tables, DMA.base.0).unwrap() >> 2) & 7, ATTR_NORMAL_NC);
        assert_eq!((walk(tables, ADMIN.base.0).unwrap() >> 2) & 7, ATTR_DEVICE);
        assert_eq!((walk(tables, LOADER.base.0).unwrap() >> 2) & 7, ATTR_NORMAL);
        // De kern-RAM is uitvoerbaar, de rest niet.
        assert_eq!(walk(tables, KERN_RAM.base.0).unwrap() & (1 << 54), 0);
        assert_ne!(walk(tables, WINDOW_END).unwrap() & (1 << 54), 0);
        // 24 GB: de laatste 2 MB wel, de GB erna niet.
        assert!(walk(tables, DRAM_BASE + (24 << 30) - MB2).is_some());
        assert!(walk(tables, DRAM_BASE + (24 << 30)).is_none());
    }

    #[test]
    fn dram_size_is_clamped() {
        assert_eq!(dram_gb(24 << 30), 24);
        assert_eq!(dram_gb(0), (WINDOW_END - DRAM_BASE).div_ceil(GB));
        assert_eq!(dram_gb(1 << 40), MAX_DRAM_GB);
        assert_eq!(dram_block(DRAM_BASE + 4096), None);
    }

    #[test]
    fn the_vhe_registers() {
        // PARange 42 bits (M4, GEMETEN 28-08: 3).
        let t = tcr(3);
        assert_eq!(t & 0x3f, 16);
        assert_eq!((t >> 32) & 7, 3);
        assert_ne!(t & (1 << 23), 0, "EPD1: geen walks via TTBR1");
        assert_eq!((t >> 30) & 3, 2, "TG1 4 KB");
        assert_eq!((t >> 14) & 3, 0, "TG0 4 KB");
        assert_eq!(tcr(7) >> 32 & 7, 5);
        assert_eq!(SCTLR & 0b1101, 0b1101);
        assert_ne!(SCTLR & (1 << 12), 0);
        assert_eq!(SCTLR & (1 << 1), 0, "geen uitlijncontrole");
    }
}
