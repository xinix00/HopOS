//! De stage-2-map van een partitie (ARMv8, VMSAv8-64), als pure
//! geheugenrekenkunde over [`PhysMem`].
//!
//! Dit is de isolatiebelofte: de app op EL1 kan mappen wat hij wil, maar de
//! IPA-naar-PA-vertaling die HOP hier vastlegt laat alleen zijn eigen
//! partitie door. De map is tevens de relocatie: een image is canoniek
//! gelinkt en de stage-2 legt dat IPA-bereik op de fysieke partitie van dít
//! slot.
//!
//! 4 KB-granule, 39-bit IPA (VTCR.T0SZ=25, startlevel 1): elke L1-entry
//! kiest één L2-tabel van 2 MB-blokken. De registers (VTTBR, VTCR, de
//! HVC voor de TLBI, de vectoren) blijven bij `cpu`; hier staan alleen de
//! tabellen.
//!
//! Per slot leeft het tabelblok op `cage_pa + slot * CAGE_STRIDE`, met op
//! [`CTX_OFF`] het switch-contextblok en op [`SMP_CTX_OFF`] de secundaire
//! context van de gelijknamige core (die van een ANDERE kooi kan zijn).

use crate::cage::PhysMem;
use crate::{Error, Result, SLOT_CAP, Slot};

const DESC_TABLE: u64 = 0x3;
const DESC_BLOCK: u64 = 0x1;
const DESC_PAGE: u64 = 0x3;
const ATTR_AF: u64 = 1 << 10;
const ATTR_SH_INNER: u64 = 0x3 << 8;
const ATTR_RW: u64 = 0x3 << 6;
const ATTR_NORMAL: u64 = 0xF << 2;
/// Normal non-cacheable: de framebuffer-grant (de scanout leest mee).
const ATTR_NORM_NC: u64 = 0x5 << 2;
const BLOCK_RW: u64 = DESC_BLOCK | ATTR_AF | ATTR_SH_INNER | ATTR_RW | ATTR_NORMAL;
const BLOCK_RW_NC: u64 = DESC_BLOCK | ATTR_AF | ATTR_SH_INNER | ATTR_RW | ATTR_NORM_NC;
const PAGE_RW_NC: u64 = DESC_PAGE | ATTR_AF | ATTR_SH_INNER | ATTR_RW | ATTR_NORM_NC;

/// De exclusieve grens van het 39-bit stage-2-regime.
pub const IPA_LIMIT: u64 = 1 << 39;
/// Beide EL2-trampolines kappen VTCR.PS op 44 bits.
pub const PA_LIMIT: u64 = 1 << 44;
/// De afstand tussen twee tabelblokken (`layout.CageStride`).
pub const CAGE_STRIDE: u64 = 0x10000;
/// Het switch-contextblok van de primaire context (`layout.CtxOff`).
pub const CTX_OFF: u64 = 0x6000;
/// De secundaire context van de gelijknamige core (`layout.SMPCtxOff`).
pub const SMP_CTX_OFF: u64 = 0x6800;
/// De lengte van een contextblok (`layout.CtxLen`).
pub const CTX_LEN: u64 = 1024;
/// Het vaste IPA van de framebuffer-grant (`layout.FbIPA`): GB0, in het
/// canonieke beeld van niemand.
pub const FB_IPA: u64 = 0x2000_0000;

const _: () = {
    use abi::layout as l;
    assert!(CAGE_STRIDE == l::CAGE_STRIDE && CTX_OFF == l::CTX_OFF);
    assert!(SMP_CTX_OFF == l::SMP_CTX_OFF && CTX_LEN == l::CTX_LEN && FB_IPA == l::FB_IPA);
};

const L1_OFF: u64 = 0x0000;
/// Elf bestaande tabelpagina's (+1000..5000 en +a000..f000) dekken kleine
/// apps zonder extra reservering.
const INLINE_L2_PAGES: u64 = 11;
const L2_FB_OFF: u64 = 0x7000;
const L3_FB_HEAD_OFF: u64 = 0x8000;
const L3_FB_TAIL_OFF: u64 = 0x9000;
const BLOCK: u64 = 2 << 20;
const GB: u64 = 1 << 30;

/// De tabelbouwer voor één node: waar de tabelblokken liggen.
#[derive(Copy, Clone, Debug)]
pub struct Stage2 {
    /// `layout.CagePA`: het blok van slot 0 (de gedeelde vectoren).
    pub cage_pa: u64,
    /// Het hoogste slot dat dit board kent.
    pub max_slots: usize,
}

/// De node-eigen bytes achter de zichtbare partitie voor de tabellen. Kleine
/// partities gebruiken de bestaande tabelpagina's van de kooi; één 2 MB-korrel
/// draagt elke L2 die het 39-bit regime ooit nodig heeft.
#[must_use]
pub fn table_reserve(ipa_base: u64, size: u64) -> u64 {
    if size == 0 || ipa_base >= IPA_LIMIT || size > IPA_LIMIT - ipa_base {
        return 0;
    }
    let pages = ((ipa_base & (GB - 1)) + size + GB - 1) >> 30;
    if pages > INLINE_L2_PAGES { BLOCK } else { 0 }
}

impl Stage2 {
    /// Het tabelblok van `slot`.
    #[must_use]
    pub fn table_pa(&self, slot: Slot) -> u64 {
        self.cage_pa + slot.get() as u64 * CAGE_STRIDE
    }

    fn check(&self, slot: usize) -> Result<Slot> {
        Slot::new(slot)
            .filter(|s| s.get() <= self.max_slots.min(SLOT_CAP))
            .ok_or(Error::SlotRange {
                slot,
                max: self.max_slots,
            })
    }

    /// Schrijft de tabellen van `slot` en geeft het L1-adres (voor VTTBR).
    ///
    /// `[ipa_base, ipa_base+size)` gaat op `[pa_base, pa_base+size)`; de
    /// aanroeper bezit ook [`table_reserve`] bytes op `pa_base+size`. Ongeldige
    /// invoer wordt geweigerd VÓÓR er één byte geschreven is: een bestaande
    /// tabel of context mag niet beschadigen.
    pub fn build(
        &self,
        mem: &mut impl PhysMem,
        slot: usize,
        ipa_base: u64,
        pa_base: u64,
        size: u64,
    ) -> Result<u64> {
        let slot = self.check(slot)?;
        let bad = Error::Range {
            base: ipa_base,
            size,
        };
        if size == 0 || (ipa_base | pa_base | size) & (BLOCK - 1) != 0 {
            return Err(bad);
        }
        if !(GB..IPA_LIMIT).contains(&ipa_base) || size > IPA_LIMIT - ipa_base {
            return Err(bad);
        }
        let extra = table_reserve(ipa_base, size);
        if pa_base >= PA_LIMIT || size > PA_LIMIT - pa_base || extra > PA_LIMIT - pa_base - size {
            return Err(Error::Range {
                base: pa_base,
                size,
            });
        }
        let base = self.table_pa(slot);
        // De secundaire context kan van een andere kooi zijn; al het andere
        // is van deze gestopte kooi en begint schoon.
        mem.clear(base, SMP_CTX_OFF);
        mem.clear(
            base + SMP_CTX_OFF + CTX_LEN,
            CAGE_STRIDE - SMP_CTX_OFF - CTX_LEN,
        );
        if extra != 0 {
            mem.clear(pa_base + size, extra);
        }
        let first_gb = ipa_base >> 30;
        let mut off = 0;
        while off < size {
            let ipa = ipa_base + off;
            let gb = ipa >> 30;
            let table = gb - first_gb;
            let l2 = if extra == 0 {
                // Sla de contexten en de framebuffer-pagina's over.
                let page = if table >= 5 { table + 5 } else { table + 1 };
                base + page * 0x1000
            } else {
                pa_base + size + table * 0x1000
            };
            mem.write64(base + L1_OFF + gb * 8, l2 | DESC_TABLE);
            mem.write64(l2 + ((ipa >> 21) & 511) * 8, (pa_base + off) | BLOCK_RW);
            off += BLOCK;
        }
        // De walker van de app leest cacheable; HOP schreef ongecached. Vegen
        // vóór de dispatch, er draait nu geen walker op dit blok.
        mem.clean_inv(base, CAGE_STRIDE);
        if extra != 0 {
            mem.clean_inv(pa_base + size, extra);
        }
        Ok(base + L1_OFF)
    }

    /// Het geheugendeel van de hard-kill: nul de tabellen (tot [`CTX_OFF`],
    /// het contextblok blijft), en veeg daarna. De volgorde is heilig: eerst
    /// nullen, dan vegen. Andersom kon een nog lopende walker de oude tabel
    /// tussen veeg en nullen opnieuw cachen, en overleefde de app de kill op
    /// echt silicium (QEMU verhult dit). De TLBI via de HVC doet `cpu` erna.
    pub fn revoke_tables(&self, mem: &mut impl PhysMem, slot: Slot) {
        let base = self.table_pa(slot);
        mem.clear(base, CTX_OFF);
        mem.clean_inv(base, CTX_OFF);
    }

    /// Mapt een fysiek venster Normal-NC op [`FB_IPA`] in de kooi van
    /// `slot`: de framebuffer-grant. PAGINA-PRECIES aan de randen: met alleen
    /// 2 MB-blokken kreeg de houder tot ongeveer 4 MB firmware-geheugen rond
    /// de buffer erbij, RW. Het middenstuk gaat als blokken, de randen via
    /// één L3 elk.
    pub fn grant_window(&self, mem: &mut impl PhysMem, slot: usize, pa: u64, size: u64) -> Result {
        let slot = self.check(slot)?;
        let bad = Error::Range { base: pa, size };
        if pa == 0 || size == 0 || pa.checked_add(size).is_none() {
            return Err(bad);
        }
        let lo = pa & !(BLOCK - 1);
        let pg_lo = pa & !0xFFF;
        let pg_hi = pa.checked_add(size + 0xFFF).ok_or(bad)? & !0xFFF;
        if pg_hi - lo > GB - (FB_IPA & (GB - 1)) {
            return Err(bad);
        }
        let base = self.table_pa(slot);
        let l2fb = base + L2_FB_OFF;
        let gb = FB_IPA >> 30;
        let l1e = base + L1_OFF + gb * 8;
        match mem.read64(l1e) {
            0 => mem.write64(l1e, l2fb | DESC_TABLE),
            v if v == l2fb | DESC_TABLE => {}
            _ => return Err(bad),
        }
        mem.clear(l2fb, 0x1000);
        mem.clear(base + L3_FB_HEAD_OFF, 0x1000);
        mem.clear(base + L3_FB_TAIL_OFF, 0x1000);
        let gb_base = gb << 30;
        let ipa_of = |p: u64| FB_IPA + (p - lo);
        let b_start = (pg_lo + BLOCK - 1) & !(BLOCK - 1);
        let b_end = pg_hi & !(BLOCK - 1);
        let mut off = b_start;
        while off < b_end {
            let idx = (ipa_of(off) - gb_base) >> 21;
            mem.write64(l2fb + idx * 8, off | BLOCK_RW_NC);
            off += BLOCK;
        }
        let mut edge = |l3: u64, from: u64, to: u64| {
            if from >= to {
                return;
            }
            let blk = ipa_of(from) & !(BLOCK - 1);
            mem.write64(l2fb + ((blk - gb_base) >> 21) * 8, l3 | DESC_TABLE);
            let mut p = from;
            while p < to {
                mem.write64(l3 + ((ipa_of(p) - blk) >> 12) * 8, p | PAGE_RW_NC);
                p += 0x1000;
            }
        };
        if b_start > pg_lo {
            edge(base + L3_FB_HEAD_OFF, pg_lo, b_start.min(pg_hi));
        }
        if b_end >= b_start && b_end < pg_hi {
            edge(base + L3_FB_TAIL_OFF, b_end.max(pg_lo), pg_hi);
        }
        for t in [L1_OFF, L2_FB_OFF, L3_FB_HEAD_OFF, L3_FB_TAIL_OFF] {
            mem.clean_inv(base + t, 0x1000);
        }
        Ok(())
    }

    /// Toetst een geërfde framebuffer-map zonder hem te wijzigen (de flip).
    /// Alleen de eigen vaste tabellen van de kooi worden gevolgd; app-geheugen
    /// is nooit een tabelpointer. Een bredere oude grant is geen bewijs van
    /// exclusief eigendom van déze framebuffer.
    pub fn has_grant_window(
        &self,
        mem: &impl PhysMem,
        slot: usize,
        pa: u64,
        size: u64,
    ) -> Result<bool> {
        let slot = self.check(slot)?;
        let bad = Error::Range { base: pa, size };
        if pa == 0 || size == 0 || pa >= 1 << 48 || size > (1 << 48) - pa {
            return Err(bad);
        }
        let lo = pa & !(BLOCK - 1);
        let (pg_lo, pg_hi) = (pa & !0xFFF, (pa + size + 0xFFF) & !0xFFF);
        if pg_hi - lo > GB - (FB_IPA & (GB - 1)) {
            return Err(bad);
        }
        let base = self.table_pa(slot);
        let root = mem.read64(base + L1_OFF + (FB_IPA >> 30) * 8);
        if root == 0 {
            return Ok(false);
        }
        if root != (base + L2_FB_OFF) | DESC_TABLE {
            return Err(bad);
        }
        let first = ((FB_IPA + pg_lo - lo) >> 21) & 511;
        let last = ((FB_IPA + pg_hi - lo - 1) >> 21) & 511;
        for idx in 0..512u64 {
            let e = mem.read64(base + L2_FB_OFF + idx * 8);
            if idx < first || idx > last {
                if e != 0 {
                    return Err(bad);
                }
                continue;
            }
            let p = (lo + (idx << 21)).wrapping_sub(FB_IPA & (GB - 1));
            if p >= pg_lo && p + BLOCK <= pg_hi && e == p | BLOCK_RW_NC {
                continue;
            }
            let table = if e == (base + L3_FB_HEAD_OFF) | DESC_TABLE {
                base + L3_FB_HEAD_OFF
            } else if e == (base + L3_FB_TAIL_OFF) | DESC_TABLE {
                base + L3_FB_TAIL_OFF
            } else {
                return Err(bad);
            };
            for j in 0..512u64 {
                let page = p + j * 0x1000;
                let want = if page >= pg_lo && page < pg_hi {
                    page | PAGE_RW_NC
                } else {
                    0
                };
                if mem.read64(table + j * 8) != want {
                    return Err(bad);
                }
            }
        }
        Ok(true)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::vec::Vec;

    /// IJl fysiek geheugen: alleen wat geschreven is, bestaat.
    #[derive(Default, Clone, PartialEq, Debug)]
    pub(crate) struct SparseMem(pub(crate) HashMap<u64, u64>);

    impl PhysMem for SparseMem {
        fn read64(&self, pa: u64) -> u64 {
            self.0.get(&pa).copied().unwrap_or(0)
        }
        fn write64(&mut self, pa: u64, v: u64) {
            if v == 0 {
                self.0.remove(&pa);
            } else {
                self.0.insert(pa, v);
            }
        }
        fn clear(&mut self, pa: u64, len: u64) {
            self.0.retain(|a, _| *a < pa || *a >= pa + len);
        }
        fn clean_inv(&mut self, _: u64, _: u64) {}
    }

    const CAGE_PA: u64 = 0x1_2000_0000;
    const POOL_PA: u64 = 0x2_0000_0000;
    const SLOT_BASE_1: u64 = 0x5000_0000;
    const FB_PA: u64 = 0x3E10_8000;

    fn s2() -> Stage2 {
        Stage2 {
            cage_pa: CAGE_PA,
            max_slots: 127,
        }
    }

    fn pa_of(d: u64) -> u64 {
        d & 0x0000_FFFF_FFFF_F000
    }

    /// Loopt de tabellen af zoals de MMU; tabellen mogen alleen in de vaste
    /// metadata van de kooi of in zijn extra reservering liggen.
    fn walk(mem: &SparseMem, slot: usize, extra: Option<u64>) -> Vec<(u64, u64, u64, bool)> {
        let base = CAGE_PA + slot as u64 * CAGE_STRIDE;
        let mut out = Vec::new();
        fn go(
            mem: &SparseMem,
            base: u64,
            extra: Option<u64>,
            tbl: u64,
            ipa: u64,
            level: u32,
            out: &mut Vec<(u64, u64, u64, bool)>,
        ) {
            let in_cage = tbl >= base && tbl + 4096 <= base + CAGE_STRIDE;
            let in_extra = extra.is_some_and(|e| tbl >= e && tbl + 4096 <= e + BLOCK);
            assert!(
                (in_cage || in_extra) && tbl & 4095 == 0,
                "table {tbl:#x} outside cage"
            );
            let shift = 39 - level * 9;
            for idx in 0..512u64 {
                let d = mem.read64(tbl + idx * 8);
                let addr = ipa + (idx << shift);
                match (level, d & 3) {
                    _ if d == 0 => {}
                    (l, DESC_TABLE) if l < 3 => go(mem, base, extra, pa_of(d), addr, l + 1, out),
                    (2, DESC_BLOCK) => out.push((addr, pa_of(d), BLOCK, (d >> 6) & 3 == 3)),
                    (3, DESC_PAGE) => out.push((addr, pa_of(d), 4096, (d >> 6) & 3 == 3)),
                    _ => panic!("unexpected descriptor {d:#x} at level {level}"),
                }
            }
        }
        go(mem, base, extra, base + L1_OFF, 0, 1, &mut out);
        out
    }

    fn assert_partition(
        mem: &SparseMem,
        slot: usize,
        ipa: u64,
        pa: u64,
        size: u64,
        extra: Option<u64>,
    ) {
        let leaves = walk(mem, slot, extra);
        assert_eq!(leaves.len() as u64, size >> 21);
        for (n, l) in leaves.iter().enumerate() {
            let off = (n as u64) << 21;
            assert_eq!(*l, (ipa + off, pa + off, BLOCK, true), "leaf {n}");
        }
    }

    fn build(mem: &mut SparseMem, slot: usize, ipa: u64, size: u64) -> (u64, Option<u64>) {
        // De extra tabelopslag ligt direct achter de partitie.
        let pa = POOL_PA;
        let extra = (table_reserve(ipa, size) != 0).then_some(pa + size);
        let l1 = s2().build(mem, slot, ipa, pa, size).unwrap();
        assert_eq!(l1, CAGE_PA + slot as u64 * CAGE_STRIDE + L1_OFF);
        (pa, extra)
    }

    #[test]
    fn build_private_partition() {
        for (ipa, size) in [
            (SLOT_BASE_1, 64 << 20),
            (SLOT_BASE_1, (768 << 20) + BLOCK),
            (SLOT_BASE_1, 2 << 30),
            (SLOT_BASE_1, 5 << 30),
            (SLOT_BASE_1, (11 << 30) - (SLOT_BASE_1 & (GB - 1))),
            (SLOT_BASE_1, 20 << 30),
            (SLOT_BASE_1, IPA_LIMIT - SLOT_BASE_1),
            (SLOT_BASE_1 + 2 * 0x2000_0000, 1 << 30),
            (1 << 30, BLOCK),
            (IPA_LIMIT - BLOCK, BLOCK),
        ] {
            let mut mem = SparseMem::default();
            let (pa, extra) = build(&mut mem, 3, ipa, size);
            assert_partition(&mem, 3, ipa, pa, size, extra);
        }
    }

    #[test]
    fn build_rejects_invalid_range_without_writes() {
        let mut mem = SparseMem::default();
        s2().build(&mut mem, 3, SLOT_BASE_1, POOL_PA, 2 << 30)
            .unwrap();
        let before = mem.clone();
        let ipa = SLOT_BASE_1;
        for (slot, ipa, pa, size) in [
            (0, ipa, POOL_PA, BLOCK),
            (128, ipa, POOL_PA, BLOCK),
            (3, ipa, POOL_PA, 0),
            (3, ipa + 4096, POOL_PA, BLOCK),
            (3, ipa, POOL_PA + 4096, BLOCK),
            (3, ipa, POOL_PA, BLOCK + 4096),
            (3, FB_IPA, POOL_PA, BLOCK),
            (3, ipa, POOL_PA, IPA_LIMIT - ipa + BLOCK),
            (3, IPA_LIMIT, POOL_PA, BLOCK),
            (3, !(BLOCK - 1), POOL_PA, BLOCK),
            (3, ipa, POOL_PA, !(BLOCK - 1)),
            (3, ipa, !(BLOCK - 1), BLOCK),
            (3, ipa, PA_LIMIT - BLOCK, 2 * BLOCK),
            (3, ipa, PA_LIMIT - (20 << 30), 20 << 30),
        ] {
            assert!(s2().build(&mut mem, slot, ipa, pa, size).is_err());
            assert_eq!(mem, before, "rejected input changed an existing cage");
        }
    }

    #[test]
    fn build_isolates_large_neighbor_partitions() {
        let mut mem = SparseMem::default();
        let size = 5u64 << 30;
        s2().build(&mut mem, 1, SLOT_BASE_1, POOL_PA, size).unwrap();
        let neighbor: HashMap<u64, u64> = mem
            .0
            .iter()
            .filter(|(a, _)| **a >= CAGE_PA + CAGE_STRIDE && **a < CAGE_PA + 2 * CAGE_STRIDE)
            .map(|(a, v)| (*a, *v))
            .collect();
        s2().build(&mut mem, 2, SLOT_BASE_1, POOL_PA + size, size)
            .unwrap();
        for (a, v) in &neighbor {
            assert_eq!(
                mem.read64(*a),
                *v,
                "large cage build overwrote its neighbor"
            );
        }
        assert_partition(&mem, 1, SLOT_BASE_1, POOL_PA, size, None);
        assert_partition(&mem, 2, SLOT_BASE_1, POOL_PA + size, size, None);
    }

    #[test]
    fn build_high_cage_uses_canonical_ipa() {
        let mut mem = SparseMem::default();
        s2().build(&mut mem, 105, SLOT_BASE_1, POOL_PA, 2 << 30)
            .unwrap();
        assert_partition(&mem, 105, SLOT_BASE_1, POOL_PA, 2 << 30, None);
    }

    #[test]
    fn rebuild_clears_old_g_bs_and_framebuffer() {
        let mut mem = SparseMem::default();
        build(&mut mem, 1, SLOT_BASE_1, 20 << 30);
        s2().grant_window(&mut mem, 1, FB_PA, 8 << 20).unwrap();
        s2().build(&mut mem, 1, SLOT_BASE_1, POOL_PA, 8 << 20)
            .unwrap();
        assert_partition(&mem, 1, SLOT_BASE_1, POOL_PA, 8 << 20, None);
    }

    // De secundaire context van de gelijknamige core kan live zijn.
    #[test]
    fn build_preserves_independent_secondary_context() {
        let mut mem = SparseMem::default();
        let base = CAGE_PA + 3 * CAGE_STRIDE;
        for off in (0..CTX_LEN).step_by(8) {
            mem.write64(base + SMP_CTX_OFF + off, 0x5350_4d00_0000_0000 | off);
        }
        mem.write64(base + CTX_OFF, 4); // CtxDead
        s2().build(&mut mem, 3, SLOT_BASE_1 + 2 * 0x2000_0000, POOL_PA, 5 << 30)
            .unwrap();
        for off in (0..CTX_LEN).step_by(8) {
            assert_eq!(
                mem.read64(base + SMP_CTX_OFF + off),
                0x5350_4d00_0000_0000 | off
            );
        }
        assert_eq!(
            mem.read64(base + CTX_OFF),
            0,
            "new cage retained old primary state"
        );
    }

    #[test]
    fn table_reservation_boundary() {
        let ipa = SLOT_BASE_1;
        let inline_max = (11u64 << 30) - (ipa & (GB - 1));
        for (size, want) in [
            (0, 0),
            (5 << 30, 0),
            (inline_max, 0),
            (inline_max + BLOCK, BLOCK),
            (20 << 30, BLOCK),
            (IPA_LIMIT - ipa, BLOCK),
        ] {
            assert_eq!(table_reserve(ipa, size), want, "size {size:#x}");
        }
    }

    #[test]
    fn revoke_zeroes_tables_but_keeps_contexts() {
        let mut mem = SparseMem::default();
        s2().build(&mut mem, 2, SLOT_BASE_1, POOL_PA, 64 << 20)
            .unwrap();
        let base = CAGE_PA + 2 * CAGE_STRIDE;
        mem.write64(base + CTX_OFF, 2);
        s2().revoke_tables(&mut mem, Slot::new(2).unwrap());
        assert!(
            walk(&mem, 2, None).is_empty(),
            "revoked cage still maps memory"
        );
        assert_eq!(
            mem.read64(base + CTX_OFF),
            2,
            "revoke clobbered a saving context"
        );
    }

    /// Waar mapt de kooi fysieke pagina `p` heen (0 = niet)? Controleert
    /// onderweg Normal-NC.
    fn grant_mapped(mem: &SparseMem, slot: usize, lo: u64, p: u64) -> u64 {
        let base = CAGE_PA + slot as u64 * CAGE_STRIDE;
        let gb_base = (FB_IPA >> 30) << 30;
        let ipa = FB_IPA + (p - lo);
        let e = mem.read64(base + L2_FB_OFF + ((ipa - gb_base) >> 21) * 8);
        match e & 3 {
            0 => 0,
            DESC_BLOCK => {
                assert!(e & ATTR_AF != 0 && (e >> 2) & 0xF == 0x5);
                pa_of(e) + (ipa & (BLOCK - 1))
            }
            _ => {
                let pe = mem.read64(pa_of(e) + ((ipa & (BLOCK - 1)) >> 12) * 8);
                if pe == 0 {
                    return 0;
                }
                assert!(pe & 3 == DESC_PAGE && pe & ATTR_AF != 0 && (pe >> 2) & 0xF == 0x5);
                pa_of(pe) + (ipa & 0xFFF)
            }
        }
    }

    #[test]
    fn grant_window() {
        let mut mem = SparseMem::default();
        let slot = 7;
        s2().build(&mut mem, slot, SLOT_BASE_1, POOL_PA, 4 << 20)
            .unwrap();
        let size = 1920 * 4 * 1080 - 3;
        s2().grant_window(&mut mem, slot, FB_PA, size).unwrap();
        let base = CAGE_PA + slot as u64 * CAGE_STRIDE;
        let l1e = mem.read64(base + L1_OFF + (FB_IPA >> 30) * 8);
        assert_eq!(l1e, (base + L2_FB_OFF) | DESC_TABLE);
        let lo = FB_PA & !(BLOCK - 1);
        let (pg_lo, pg_hi) = (FB_PA & !0xFFF, (FB_PA + size + 0xFFF) & !0xFFF);
        let mut p = pg_lo;
        while p < pg_hi {
            assert_eq!(grant_mapped(&mem, slot, lo, p), p, "page {p:#x}");
            p += 0x1000;
        }
        assert_eq!(
            grant_mapped(&mem, slot, lo, pg_lo - 0x1000),
            0,
            "overmapping before"
        );
        assert_eq!(grant_mapped(&mem, slot, lo, pg_hi), 0, "overmapping after");
        s2().grant_window(&mut mem, slot, FB_PA, size).unwrap();
        let high = 0x1_BC7A_0000;
        s2().grant_window(&mut mem, slot, high, size).unwrap();
        let h_lo = high & !(BLOCK - 1);
        assert_eq!(grant_mapped(&mem, slot, h_lo, high), high);
        assert_eq!(grant_mapped(&mem, slot, h_lo, high - 0x1000), 0);
        s2().build(&mut mem, 8, SLOT_BASE_1, POOL_PA + (64 << 20), 4 << 20)
            .unwrap();
        let other = CAGE_PA + 8 * CAGE_STRIDE;
        assert_eq!(mem.read64(other + L1_OFF + (FB_IPA >> 30) * 8), 0);
    }

    #[test]
    fn inherited_grant() {
        let slot = 7;
        let size = (8u64 << 20) - 3;
        let setup = || {
            let mut mem = SparseMem::default();
            s2().build(&mut mem, slot, SLOT_BASE_1, POOL_PA, 4 << 20)
                .unwrap();
            mem
        };
        let mem = setup();
        assert_eq!(s2().has_grant_window(&mem, slot, FB_PA, size), Ok(false));
        for pa in [FB_PA, 0x1_bc7a_0000, 0x4000_0000] {
            let mut mem = setup();
            s2().grant_window(&mut mem, slot, pa, size).unwrap();
            assert_eq!(s2().has_grant_window(&mem, slot, pa, size), Ok(true));
            assert!(
                s2().has_grant_window(&mem, slot, pa, size - 0x1000)
                    .is_err()
            );
            assert!(
                s2().has_grant_window(&mem, slot, pa, size + 0x1000)
                    .is_err()
            );
        }
        let base = CAGE_PA + slot as u64 * CAGE_STRIDE;
        let head = L3_FB_HEAD_OFF + ((FB_PA & (BLOCK - 1)) >> 12) * 8;
        for (off, value) in [
            (L1_OFF + (FB_IPA >> 30) * 8, 0x1003),
            (L2_FB_OFF + (FB_IPA >> 21) * 8, 0x1003),
            (head, 0),
            (head, FB_PA | (PAGE_RW_NC & !ATTR_RW)),
            (L2_FB_OFF, 0x4000_0000 | BLOCK_RW_NC),
        ] {
            let mut mem = setup();
            s2().grant_window(&mut mem, slot, FB_PA, size).unwrap();
            mem.write64(base + off, value);
            assert!(!matches!(
                s2().has_grant_window(&mem, slot, FB_PA, size),
                Ok(true)
            ));
        }
        for pa in [0, u64::MAX - 1, 1 << 48] {
            assert!(s2().has_grant_window(&mem, slot, pa, 4096).is_err());
        }
    }
}
