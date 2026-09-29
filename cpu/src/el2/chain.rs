//! De sprong van de kern-flip: de nieuwe kern relokeren, en er met de MMU
//! uit in springen (`OLD/metal/cpu/el2/chain.go` en `chain_arm64.s`).
//!
//! Dit bezit twee dingen en verder niets: [`relocate`], dat de
//! HOPRELO1-tabel van de bundel toepast op het platte beeld dat de kern al
//! neerlegde (`kern::kernflip::flatten`), en [`chain`], de sprong zelf. Wat
//! er wordt overgedragen (het handoff-blob), waar het beeld ligt en wanneer
//! er gesprongen wordt, is van de kern-binary (`hopos/src/flip.rs`).
//!
//! # De vorm van de sprong in v3
//!
//! De Go-kern leende een venster uit de pool en sprong daarin; de nieuwe
//! kern kreeg `RamStart`/`RamSize` gepatcht. Een kern van v3 linkt op een
//! vast adres met een vaste identity map, dus de nieuwe kern gaat op het
//! KOUDE adres van de oude: precies waar de firmware hem ook zou laden. Dat
//! kan de oude kern niet zelf (hij zou zijn eigen code overschrijven), dus
//! de laatste meters loopt een kleine trampoline buiten het kern-RAM:
//!
//! 1. interrupts dicht, MMU, D- en I-cache uit (lezen-en-maskeren, m1n1's
//!    `mmu_disable`, zoals de Go-sprong op de M4);
//! 2. het kern-RAM by-VA vegen (`dc civac`): elke dirty regel van de oude
//!    kern (stack, heap, .data) moet naar DRAM vóór de kopie, anders drukt
//!    een latere eviction oude bytes over de nieuwe kern heen;
//! 3. het platte beeld woordgewijs van de staging naar het koude adres, met
//!    de MMU uit (dus Device-nGnRnE: gealigneerd, ongecached, geen
//!    speculatie die een regel terugbrengt);
//! 4. de I-hygiëne (`ic iallu`, de les van de eerste vier ijzer-flips van
//!    01-09: het venster was net nog code van een ander), en `br` naar de
//!    entry met x0 = de firmware-x0 die de oude kern ooit kreeg.
//!
//! De nieuwe kern komt zo binnen op exact de conditie van een koude boot
//! (EL2, MMU uit, caches schoon), en zijn boot-stub zet alles zelf weer op.

use dev::Pa;

/// Waarom de sprong niet door kan gaan. Alles wat hier faalt, faalt VÓÓR
/// er iets onherroepelijks gebeurde: de oude kern draait gewoon door.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChainError {
    /// Een relocatie-offset die niet 8-uitgelijnd is of buiten het beeld
    /// valt.
    Reloc {
        /// De offset uit de tabel.
        off: u32,
        /// De maat van het platte beeld.
        size: u64,
    },
    /// Het beeld, de staging of de trampoline overlappen elkaar of het
    /// geveegde venster op de verkeerde manier.
    Layout {
        /// Wat er botst.
        what: &'static str,
        /// Het adres.
        pa: u64,
    },
    /// Deze build heeft geen trampoline (de host).
    NoTrampoline,
}

impl core::fmt::Display for ChainError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::Reloc { off, size } => write!(
                f,
                "relocation at {off:#x} is unaligned or outside the {size:#x}-byte image"
            ),
            Self::Layout { what, pa } => write!(f, "chain layout: {what} at {pa:#x}"),
            Self::NoTrampoline => write!(f, "this build carries no chain trampoline"),
        }
    }
}

/// Past een relocatietabel toe op het platte beeld op `flat` (`size`
/// bytes): elk woord op een offset draagt een absoluut adres op de
/// linkbasis van de bundel en krijgt `delta` erbij (modulo 2^64: een
/// nieuwe basis onder de linkbasis is een negatieve delta). Eerst wordt de
/// hele tabel getoetst, dan pas geschreven: een kapotte tabel laat het beeld
/// ongemoeid. Geeft het aantal relocaties.
///
/// Het beeld moet van de aanroeper zijn (de staging van de flip); `flat`
/// komt uit het plan van het board, zoals elk adres dat `dev` krijgt.
pub fn relocate(
    flat: Pa,
    size: u64,
    offs: impl Iterator<Item = u32> + Clone,
    delta: u64,
) -> Result<usize, ChainError> {
    let mut n = 0usize;
    for off in offs.clone() {
        if !off.is_multiple_of(8) || u64::from(off).saturating_add(8) > size {
            return Err(ChainError::Reloc { off, size });
        }
        n += 1;
    }
    if delta != 0 {
        for off in offs {
            let a = flat.add(u64::from(off));
            dev::write64(a, dev::read64(a).wrapping_add(delta));
        }
    }
    Ok(n)
}

/// Alles wat de sprong nodig heeft.
#[derive(Debug, Clone, Copy)]
pub struct Jump {
    /// Het koude adres van de kern: waar het beeld heen gaat.
    pub dst: Pa,
    /// De staging met het gerelokeerde platte beeld.
    pub src: Pa,
    /// De maat van het beeld (8-uitgelijnd).
    pub len: u64,
    /// Het fysieke entrypoint van de nieuwe kern (binnen `[dst, dst+len)`).
    pub entry: u64,
    /// x0 voor de nieuwe kern: wat de firmware óns gaf (de DTB-pointer).
    pub x0: u64,
    /// Het cacheable kern-RAM dat geveegd wordt: `[sweep.0, sweep.1)`. Het
    /// moet `dst` omvatten.
    pub sweep: (Pa, Pa),
    /// Waar de trampoline heen gaat: buiten het kern-RAM, buiten het beeld
    /// en de staging, in RAM dat de identity map uitvoerbaar mapt.
    pub tramp: Pa,
}

/// De trampoline als bytes, of `None` op de host.
#[must_use]
pub fn trampoline() -> Option<&'static [u8]> {
    arch::trampoline()
}

fn overlaps(a: u64, alen: u64, b: u64, blen: u64) -> bool {
    a < b.saturating_add(blen) && b < a.saturating_add(alen)
}

impl Jump {
    /// Toetst de indeling: dit is de laatste plek waar "nee" nog kan.
    pub fn check(&self) -> Result<(), ChainError> {
        let code = trampoline().ok_or(ChainError::NoTrampoline)?;
        let t = code.len() as u64;
        let bad = |what, pa| Err(ChainError::Layout { what, pa });
        if !self.len.is_multiple_of(8)
            || !self.dst.0.is_multiple_of(8)
            || !self.src.0.is_multiple_of(8)
        {
            return bad("unaligned image", self.dst.0);
        }
        if self.entry < self.dst.0 || self.entry >= self.dst.0.saturating_add(self.len) {
            return bad("entry outside the image", self.entry);
        }
        if self.dst.0 < self.sweep.0.0 || self.dst.0.saturating_add(self.len) > self.sweep.1.0 {
            return bad("image outside the swept kernel RAM", self.dst.0);
        }
        if overlaps(
            self.src.0,
            self.len,
            self.sweep.0.0,
            self.sweep.1.0 - self.sweep.0.0,
        ) {
            return bad("staging inside the swept kernel RAM", self.src.0);
        }
        if overlaps(
            self.tramp.0,
            t,
            self.sweep.0.0,
            self.sweep.1.0 - self.sweep.0.0,
        ) || overlaps(self.tramp.0, t, self.src.0, self.len)
        {
            return bad(
                "trampoline overlaps the kernel RAM or the staging",
                self.tramp.0,
            );
        }
        Ok(())
    }
}

/// Springt in de nieuwe kern. Keert alleen terug met een fout, en dan is er
/// niets veranderd behalve de kopie van de trampoline.
///
/// # Safety
///
/// De aanroeper maakt waar dat:
///
/// - dit core 0 is en er op deze core niets meer hoeft te gebeuren: na de
///   sprong bestaat de oude kern niet meer (geen executor, geen stack);
/// - `[src, src+len)` het complete, gerelokeerde beeld is van een bundel
///   waarvan de som getoetst is (`kern::kernflip::Bundle`, `sha256`), en dat
///   niemand anders dat bereik of `tramp` nog beschrijft;
/// - alles wat de nieuwe kern moet lezen buiten het beeld (het handoff-blob,
///   het pointer/magic-paar, de recorder) al naar DRAM geveegd is;
/// - geen andere core in `[sweep.0, sweep.1)` schrijft of er code uitvoert
///   (de app-cores draaien in hun partities en in de switch-code in de
///   plan-regio, nooit in het kern-RAM).
pub unsafe fn chain(j: &Jump) -> ChainError {
    if let Err(e) = j.check() {
        return e;
    }
    let Some(code) = trampoline() else {
        return ChainError::NoTrampoline;
    };
    // De trampoline naar zijn plek en naar DRAM: hij draait straks met de
    // I-cache uit, dus hij moet in het geheugen staan, niet alleen in een
    // regel. De staging idem: de kopie leest met de MMU uit.
    dev::copy_in(j.tramp, code);
    dev::push(j.tramp, code.len());
    if let Ok(n) = usize::try_from(j.len) {
        dev::push(j.src, n);
    }
    dev::mb();
    // SAFETY: de indeling is net getoetst (`check`); de rest is het
    // contract van deze functie, dat de aanroeper waarmaakt.
    unsafe { arch::jump(j) }
}

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod arch {
    use super::Jump;

    // De trampoline. Positie-onafhankelijk (alleen registers en relatieve
    // sprongen), want hij draait op een adres dat hij niet kent.
    //
    // In: x0 = dst, x1 = src, x2 = len (8-voud), x3 = entry, x4 = x0 van de
    // nieuwe kern, x5/x6 = het te vegen venster.
    core::arch::global_asm!(
        r#"
    .pushsection .text.hopos_chain, "ax"
    .balign 64
    .global hopos_chain_tramp
hopos_chain_tramp:
    msr daifset, #0xf
    mov x16, x3
    // MMU, D- en I-cache uit: lezen en maskeren, geen gegokte vaste waarde.
    mrs x9, sctlr_el2
    bic x9, x9, #1
    bic x9, x9, #(1 << 2)
    bic x9, x9, #(1 << 12)
    msr sctlr_el2, x9
    isb
    // Het kern-RAM naar DRAM en uit de cache, regelgrootte uit CTR_EL0.
    mrs x9, ctr_el0
    ubfx x9, x9, #16, #4
    mov x10, #4
    lsl x10, x10, x9
    sub x11, x10, #1
    bic x5, x5, x11
1:  cmp x5, x6
    b.hs 2f
    dc civac, x5
    add x5, x5, x10
    b 1b
2:  dsb sy
    // Het beeld naar het koude adres, woord voor woord.
3:  cbz x2, 4f
    ldr x9, [x1], #8
    str x9, [x0], #8
    sub x2, x2, #8
    b 3b
4:  dsb sy
    ic iallu
    dsb sy
    isb
    mov x0, x4
    mov x1, xzr
    mov x2, xzr
    mov x3, xzr
    br x16
    .global hopos_chain_tramp_end
hopos_chain_tramp_end:
    .popsection
"#
    );

    unsafe extern "C" {
        safe static hopos_chain_tramp: u8;
        safe static hopos_chain_tramp_end: u8;
    }

    pub(super) fn trampoline() -> Option<&'static [u8]> {
        let start = &raw const hopos_chain_tramp;
        let len = (&raw const hopos_chain_tramp_end as usize).wrapping_sub(start as usize);
        if len == 0 || len > 4096 {
            return None;
        }
        // SAFETY: twee labels in dezelfde `global_asm!` hierboven, in één
        // sectie, met het einde erachter (net getoetst): de bytes ertussen
        // zijn code in de eigen `.text`, leesbaar, `'static` en nooit
        // beschreven.
        Some(unsafe { core::slice::from_raw_parts(start, len) })
    }

    /// # Safety
    ///
    /// Het contract van [`super::chain`].
    pub(super) unsafe fn jump(j: &Jump) -> ! {
        // SAFETY: interrupts gaan hier dicht (een timer-IRQ tussen "MMU uit"
        // en de sprong zou in code landen die er straks niet meer is); de
        // I-cache wordt ongeldig gemaakt zodat de net gekopieerde
        // trampoline vers gehaald wordt; de sprong gaat naar code die
        // `chain` op `tramp` legde en naar DRAM veegde. Er komt niets terug.
        unsafe {
            core::arch::asm!(
                "msr daifset, #0xf",
                "dsb sy",
                "ic iallu",
                "dsb sy",
                "isb",
                "br {t}",
                t = in(reg) j.tramp.0,
                in("x0") j.dst.0,
                in("x1") j.src.0,
                in("x2") j.len,
                in("x3") j.entry,
                in("x4") j.x0,
                in("x5") j.sweep.0.0,
                in("x6") j.sweep.1.0,
                options(noreturn, nostack)
            )
        }
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod arch {
    //! Host-kant: geen trampoline en geen sprong; de tests bewijzen de
    //! relocatie en de indeling.
    use super::{ChainError, Jump};

    pub(super) fn trampoline() -> Option<&'static [u8]> {
        None
    }

    /// # Safety
    ///
    /// Op de host is er niets om in te springen.
    pub(super) unsafe fn jump(_j: &Jump) -> ChainError {
        ChainError::NoTrampoline
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::vec;

    #[test]
    fn relocate_adds_the_delta_to_every_listed_word() {
        let mut img = vec![0x4020_0000u64, 7, 0x4020_1234, 0x6000_0000];
        let base = Pa(img.as_mut_ptr() as u64);
        let delta = 0x4020_0000u64.wrapping_sub(0x6020_0000);
        // Het beeld is gelinkt op 0x6020_0000 en gaat naar 0x4020_0000.
        img[0] = 0x6020_0000;
        img[2] = 0x6020_1234;
        let n = relocate(base, 32, [0u32, 16].into_iter(), delta).unwrap();
        assert_eq!(n, 2);
        assert_eq!(img, [0x4020_0000, 7, 0x4020_1234, 0x6000_0000]);
    }

    #[test]
    fn a_bad_table_touches_nothing() {
        let mut img = vec![1u64, 2, 3];
        let base = Pa(img.as_mut_ptr() as u64);
        for bad in [[0u32, 4], [0, 24], [0, u32::MAX]] {
            assert!(relocate(base, 24, bad.into_iter(), 100).is_err());
        }
        assert_eq!(img, [1, 2, 3], "patched before the table was checked");
    }

    #[test]
    fn the_host_has_no_jump() {
        let j = Jump {
            dst: Pa(0x4020_0000),
            src: Pa(0xB020_0000),
            len: 0x1000,
            entry: 0x4020_0000,
            x0: 0,
            sweep: (Pa(0x4000_0000), Pa(0x4f00_0000)),
            tramp: Pa(0xB000_2000),
        };
        assert_eq!(j.check(), Err(ChainError::NoTrampoline));
        assert!(trampoline().is_none());
    }
}
