//! De M-mode-switcher op een app-hart: de RISC-V-tegenhanger van
//! `cpu::el2::switch`, en bedoeld om ernaast gelezen te worden (Go:
//! `cpu/mmode/switch.s`, 810 regels, hier de kern ervan).
//!
//! Een app-hart draait deze code in machine mode met `mtvec` = [`entry_pc`]
//! en `mscratch` = zijn sched-blok (kern-eigen geheugen, buiten elke kooi).
//! Drie ingangen, één sched-blok:
//!
//! - **parkenter** (`a1` = sched-blok): de boot-intrek van een geparkeerd hart
//!   ([`super::boot::start_hart`]); daarna draait de rotatie hier voorgoed.
//! - **mentry**: elke trap van een bewoner. `csrrw sp, mscratch, sp` is het
//!   enige dat aan een bruikbare pointer komt (RISC-V heeft geen vrij
//!   register bij de trap, ARM heeft SP_EL2).
//! - **rotate**: round-robin over de bewonerslijst vanaf de rotor, de eerste
//!   die boot-pending is of saved en aan de beurt (zijn wektijd verstreken).
//!   Niemand: parkeren met `wfi` op de eigen `mtimecmp` (de vroegste wektijd,
//!   geklemd op `SCHED_SLEEP_CAP`), gewekt door de kick (`msip`).
//!
//! Het wisselmoment is een EXPLICIETE yield, net als op ARM: `ecall` uit
//! S-mode met a7 = 0 en de wektijd in a0 ("hervat me niet vóór deze tik";
//! zonder dat getal pingpongen twee lege apps op volle snelheid, gemeten
//! 31-07: allebei 36% van het hart). a7 = 1 is exit. Al het andere is een
//! fault: mcause en mtval naar de control-page, de bewoner dood.
//!
//! Drie dingen die anders zijn dan op ARM, alle drie architectuur:
//!
//! 1. `mepc + 4` bij een yield: mepc wijst naar de `ecall` zelf.
//! 2. De kooi is `satp` plus `pmpcfg0`/`pmpaddr0..7` (het regime,
//!    `CTX_REGIME`); bij een koude boot schrijft de switcher ze uit het
//!    ctx-blok dat de kern bouwde en leest `pmpcfg0` terug: een kooi die niet
//!    aantoonbaar staat, wordt niet betreden (de bewoner gaat dood).
//! 3. Cache-onderhoud tussen de kern en dit hart: op de C906 zijn de harts
//!    niet coherent, dus de regels die de kern schrijft (lijst, staat) worden
//!    vóór het lezen geïnvalideerd, en wat de kern leest (staat) na het
//!    schrijven weggeveegd (`th.dcache.cipa`, met de feature `thead`). Op
//!    QEMU zijn de macro's leeg.
//!
//! Wat hier (nog) NIET is: de kill-tick (`SCHED_TICK_TICKS`): een bewoner
//! wordt ingetrokken bij zijn volgende yield (`CTX_REVOKE`). Een bewoner die
//! nooit yieldt, houdt dit hart tot de kern het reset (C906L) of de node.

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
use abi::layout::{
    CTX_BOOT_ARG, CTX_BOOT_PC, CTX_CTRL_PA, CTX_GPRS, CTX_OFF, CTX_REGIME, CTX_RESUME, CTX_REVOKE,
    CTX_STATE, CTX_WAKE, CTX_WAKE_NO_PEEK, CtxState, SCHED_CLINT_PA, SCHED_COUNT, SCHED_CURRENT,
    SCHED_LIST, SCHED_MSIP_PA, SCHED_ROTOR, SCHED_S2_PA, SCHED_SCRATCH, SCHED_SLEEP_CAP,
};

/// De verschuiving van slot naar kooi-blok (`CAGE_STRIDE` = 64 KB).
pub const CAGE_SHIFT: u32 = 16;
const _: () = assert!(1 << CAGE_SHIFT == abi::layout::CAGE_STRIDE);

/// De regime-woorden in `CTX_REGIME`, in de volgorde van de switcher:
/// `satp`, `stvec`, `sscratch`, `pmpcfg0`, `pmpaddr0..7`.
pub const REGIME_SATP: u64 = 0;
/// `stvec`.
pub const REGIME_STVEC: u64 = 8;
/// `sscratch`.
pub const REGIME_SSCRATCH: u64 = 16;
/// `pmpcfg0`.
pub const REGIME_PMPCFG0: u64 = 24;
/// `pmpaddr0`; `pmpaddr k` op `+ 8k`.
pub const REGIME_PMPADDR0: u64 = 32;
const _: () = assert!(REGIME_PMPADDR0 + 8 * 8 == 8 * abi::layout::CTX_REGIME_RV_WORDS);

/// Waarde voor [`abi::hopabi::CTRL_FAULT_VEC`] bij een kooi die niet stond
/// (`pmpcfg0` las anders terug dan de kern schreef): de bewoner is nooit
/// binnengelaten.
pub const FAULT_CAGE_VERIFY: u64 = 0xFA11;

/// Het fysieke adres van de trap-ingang (`mentry`).
#[must_use]
pub fn entry_pc() -> u64 {
    imp::entry()
}

/// Het fysieke adres van `parkenter`: de ingang voor
/// [`super::boot::start_hart`], met het sched-blok als argument.
#[must_use]
pub fn park_pc() -> u64 {
    imp::park()
}

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
mod imp {
    pub(super) fn entry() -> u64 {
        unsafe extern "C" {
            /// De trap-ingang (hieronder).
            static __hopos_mentry: u8;
        }
        (&raw const __hopos_mentry) as u64
    }
    pub(super) fn park() -> u64 {
        unsafe extern "C" {
            /// De parkeer-ingang (hieronder).
            static __hopos_parkenter: u8;
        }
        (&raw const __hopos_parkenter) as u64
    }
}

#[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
mod imp {
    //! Host-stub: er is geen switcher.
    pub(super) fn entry() -> u64 {
        0
    }
    pub(super) fn park() -> u64 {
        0
    }
}

// Het cache-onderhoud van de switcher als assembler-macro's: met `thead`
// `th.dcache.cipa` (clean + invalidate op PA, rs2 = 11 in het custom-0-veld)
// en `th.sync.is`; zonder (QEMU, coherent) niets.
#[cfg(all(feature = "thead", target_arch = "riscv64", target_os = "none"))]
macro_rules! cache_macros {
    () => {
        r#"
    .macro HOPOS_RV_CIPA reg
    .insn r 0x0b, 0, 1, x0, \reg, x11
    .endm
    .macro HOPOS_RV_SYNC
    .4byte 0x01b0000b
    .endm
    .macro HOPOS_RV_CIALL
    .4byte 0x0030000b
    .4byte 0x01b0000b
    .endm
"#
    };
}

#[cfg(all(not(feature = "thead"), target_arch = "riscv64", target_os = "none"))]
macro_rules! cache_macros {
    () => {
        r#"
    .macro HOPOS_RV_CIPA reg
    .endm
    .macro HOPOS_RV_SYNC
    .endm
    .macro HOPOS_RV_CIALL
    .endm
"#
    };
}

// De switcher. Invariant in rotate en daarna: sp = het sched-blok van dit
// hart, mtvec = mentry, alle GPR's vrij. In de ingang: t0..t2 staan in de
// scratch van het sched-blok tot ze in het ctx-blok belanden.
#[cfg(all(target_arch = "riscv64", target_os = "none"))]
core::arch::global_asm!(
    cache_macros!(),
    r#"
    .section .text.hopos_mmode, "ax"
    .balign 4
    .global __hopos_parkenter
__hopos_parkenter:
    csrw mie, zero
    // De TIME-CSR voor de bewoners (mcounteren.TM): de klok van een app is
    // `rdtime`, en zonder dit bit is dat in S-mode een illegal instruction.
    li t0, 2
    csrw mcounteren, t0
    mv sp, a1
    la t0, __hopos_mentry
    csrw mtvec, t0
    li t0, 8 | 128
    csrs mie, t0
    j 50f

    .balign 4
    .global __hopos_mentry
__hopos_mentry:
    csrrw sp, mscratch, sp
    sd t0, {scratch}+0(sp)
    sd t1, {scratch}+8(sp)
    sd t2, {scratch}+16(sp)
    csrr t0, mcause
    bltz t0, 30f
    li t1, 9
    bne t0, t1, 40f

    // --- yield of exit (ecall uit S-mode) -----------------------------
    ld t0, {current}(sp)
    ld t1, {s2}(sp)
    slli t0, t0, {shift}
    add t1, t1, t0
    li t0, {ctxoff}
    add t1, t1, t0
    bnez a7, 45f

    // De GPR's: xN op CTX_GPRS + 8*(N-1); x2 uit mscratch, x5..x7 uit de
    // scratch.
    sd x1, {gprs}+0(t1)
    csrr t0, mscratch
    sd t0, {gprs}+8(t1)
    sd x3, {gprs}+16(t1)
    sd x4, {gprs}+24(t1)
    ld t0, {scratch}+0(sp)
    sd t0, {gprs}+32(t1)
    ld t0, {scratch}+8(sp)
    sd t0, {gprs}+40(t1)
    ld t0, {scratch}+16(sp)
    sd t0, {gprs}+48(t1)
    sd x8, {gprs}+56(t1)
    sd x9, {gprs}+64(t1)
    sd x10, {gprs}+72(t1)
    sd x11, {gprs}+80(t1)
    sd x12, {gprs}+88(t1)
    sd x13, {gprs}+96(t1)
    sd x14, {gprs}+104(t1)
    sd x15, {gprs}+112(t1)
    sd x16, {gprs}+120(t1)
    sd x17, {gprs}+128(t1)
    sd x18, {gprs}+136(t1)
    sd x19, {gprs}+144(t1)
    sd x20, {gprs}+152(t1)
    sd x21, {gprs}+160(t1)
    sd x22, {gprs}+168(t1)
    sd x23, {gprs}+176(t1)
    sd x24, {gprs}+184(t1)
    sd x25, {gprs}+192(t1)
    sd x26, {gprs}+200(t1)
    sd x27, {gprs}+208(t1)
    sd x28, {gprs}+216(t1)
    sd x29, {gprs}+224(t1)
    sd x30, {gprs}+232(t1)
    sd x31, {gprs}+240(t1)
    csrr t0, mepc
    addi t0, t0, 4
    sd t0, {resume}+0(t1)
    csrr t0, sstatus
    sd t0, {resume}+8(t1)
    csrr t0, satp
    sd t0, {regime}+0(t1)
    csrr t0, stvec
    sd t0, {regime}+8(t1)
    csrr t0, sscratch
    sd t0, {regime}+16(t1)
    // Het regime van de kooi zelf verandert niet (de bewoner kan PMP niet
    // aanraken); de kern schreef het, en het staat al in het ctx-blok.
    sd a0, {wake}(t1)
    // De intrekking: vers lezen, de kern schrijft hem vanaf zijn eigen hart.
    li t0, {revoke}
    add t0, t0, t1
    HOPOS_RV_CIPA t0
    HOPOS_RV_SYNC
    ld t0, 0(t0)
    bnez t0, 45f
    li t0, {saved}
    sd t0, {state}(t1)
    fence
    HOPOS_RV_CIPA t1
    HOPOS_RV_SYNC
    j 50f

    // --- een interrupt terwijl een bewoner draait -------------------------
    // Een bewoner draait met mie = 0 (geen kill-tick in deze versie), dus dit
    // hoort niet te gebeuren: bron dicht en terug de bewoner in, alsof er
    // niets was. mepc wijst al naar de volgende instructie.
30:
    csrw mie, zero
    ld t0, {scratch}+0(sp)
    ld t1, {scratch}+8(sp)
    ld t2, {scratch}+16(sp)
    csrrw sp, mscratch, sp
    mret

    // --- fault --------------------------------------------------------
    // mcause, mtval en vec 1 naar de control-page van de bewoner, mepc,
    // ra, sp en satp voor het post-mortem in het ctx-blok.
40:
    ld t0, {current}(sp)
    ld t1, {s2}(sp)
    slli t0, t0, {shift}
    add t1, t1, t0
    li t0, {ctxoff}
    add t1, t1, t0
    ld t2, {ctrl}(t1)
    beqz t2, 41f
    csrr t0, mcause
    sd t0, {fesr}(t2)
    csrr t0, mtval
    sd t0, {ffar}(t2)
    li t0, 1
    sd t0, {fvec}(t2)
    fence
    addi t0, t2, 0x40
    HOPOS_RV_CIPA t0
41:
    csrr t0, mepc
    sd t0, {resume}+0(t1)
    sd x1, {gprs}+0(t1)
    csrr t0, mscratch
    sd t0, {gprs}+8(t1)
    csrr t0, satp
    sd t0, {regime}+0(t1)

    // --- teardown: exit, fault, intrekking -----------------------------
    // De cache van de dode bewoner naar DRAM vóór de dood gemeld wordt: de
    // kern hergebruikt de partitie zodra hij Dead ziet.
45:
    HOPOS_RV_CIALL
    li t0, {dead}
    sd t0, {state}(t1)
    fence
    HOPOS_RV_CIPA t1
    HOPOS_RV_SYNC
    sd zero, {current}(sp)

    // --- rotate ---------------------------------------------------------
    // s1 = lijstlengte, s2 = index, s3 = pogingen, s4 = vroegste wektijd.
50:
    csrw satp, zero
    sfence.vma
    addi t0, sp, 64
    HOPOS_RV_CIPA t0
    addi t0, sp, 128
    HOPOS_RV_CIPA t0
    addi t0, sp, 192
    HOPOS_RV_CIPA t0
    HOPOS_RV_SYNC
    ld s1, {count}(sp)
    ld s2, {rotor}(sp)
    li s3, 0
    li s4, -1
    beqz s1, 60f
51:
    addi s2, s2, 1
    bltu s2, s1, 52f
    li s2, 0
52:
    addi t0, sp, {list}
    add t0, t0, s2
    lbu s5, 0(t0)
    beqz s5, 55f
    ld s6, {s2}(sp)
    slli t0, s5, {shift}
    add s6, s6, t0
    li t0, {ctxoff}
    add s6, s6, t0
    HOPOS_RV_CIPA s6
    HOPOS_RV_SYNC
    ld t0, {state}(s6)
    li t1, {bootpending}
    beq t0, t1, 70f
    li t1, {saved}
    bne t0, t1, 55f
    ld t0, {wake}(s6)
    li t1, {nopeek}
    not t1, t1
    and t0, t0, t1
    rdtime t1
    bgeu t1, t0, 80f
    bgeu t0, s4, 55f
    mv s4, t0
55:
    addi s3, s3, 1
    bltu s3, s1, 51b

    // --- park -----------------------------------------------------------
    // Niemand aan de beurt: slapen tot de vroegste wektijd, geklemd op de
    // slaapgrens, of tot de kick. Zonder wekker (CLINT_PA 0) of zonder
    // grens (SLEEP_CAP 0): meteen opnieuw rondkijken (spinnen kan niet
    // hangen, Go 30-07).
60:
    sd zero, {current}(sp)
    ld a3, {clint}(sp)
    ld a4, {cap}(sp)
    beqz a3, 50b
    beqz a4, 50b
    rdtime t0
    add t0, t0, a4
    bltu t0, s4, 61f
    mv t0, s4
61:
    // De wekkers aan: een bewoner draaide met mie = 0, en een `wfi` zonder
    // enable wekt nooit (MIE blijft uit: een wek, geen trap).
    li t1, 8 | 128
    csrs mie, t1
    li t1, -1
    sw t1, 0(a3)
    srli t2, t0, 32
    sw t2, 4(a3)
    sw t0, 0(a3)
    wfi
    li t1, -1
    sw t1, 0(a3)
    sw t1, 4(a3)
    ld a4, {msip}(sp)
    beqz a4, 50b
    sw zero, 0(a4)
    j 50b

    // --- koude boot van de bewoner in s6 (slot s5, index s2) ------------
    // De kooi uit het ctx-blok: pmpaddr0..7, dan pmpcfg0, en teruglezen.
70:
    ld t0, {regime}+{pa0}+0(s6)
    csrw pmpaddr0, t0
    ld t0, {regime}+{pa0}+8(s6)
    csrw pmpaddr1, t0
    ld t0, {regime}+{pa0}+16(s6)
    csrw pmpaddr2, t0
    ld t0, {regime}+{pa0}+24(s6)
    csrw pmpaddr3, t0
    ld t0, {regime}+{pa0}+32(s6)
    csrw pmpaddr4, t0
    ld t0, {regime}+{pa0}+40(s6)
    csrw pmpaddr5, t0
    ld t0, {regime}+{pa0}+48(s6)
    csrw pmpaddr6, t0
    ld t0, {regime}+{pa0}+56(s6)
    csrw pmpaddr7, t0
    ld t0, {regime}+{cfg}(s6)
    csrw pmpcfg0, t0
    csrr t1, pmpcfg0
    beq t0, t1, 71f
    // De kooi staat niet zoals de kern hem schreef: nooit binnenlaten.
    ld t2, {ctrl}(s6)
    beqz t2, 72f
    li t0, {verify}
    sd t0, {fvec}(t2)
    sd t1, {fesr}(t2)
    fence
    addi t0, t2, 0x40
    HOPOS_RV_CIPA t0
72:
    mv t1, s6
    j 45b
71:
    ld t0, {regime}+0(s6)
    csrw satp, t0
    sfence.vma
    ld t0, {regime}+8(s6)
    csrw stvec, t0
    ld t0, {regime}+16(s6)
    csrw sscratch, t0
    ld t0, {bootpc}(s6)
    csrw mepc, t0
    // mstatus: MPP = S (01), MPIE = 0, SUM en MXR uit.
    li t0, 3 << 11
    csrc mstatus, t0
    li t0, 1 << 11
    csrs mstatus, t0
    li t0, 1 << 7
    csrc mstatus, t0
    csrw medeleg, zero
    csrw mideleg, zero
    csrw mie, zero
    sd s2, {rotor}(sp)
    sd s5, {current}(sp)
    li t0, {running}
    sd t0, {state}(s6)
    fence
    HOPOS_RV_CIPA s6
    HOPOS_RV_SYNC
    csrw mscratch, sp
    ld a0, {bootarg}(s6)
    li a1, 0
    li ra, 0
    li sp, 0
    li gp, 0
    li tp, 0
    fence.i
    mret

    // --- hervatten van de bewoner in s6 ---------------------------------
80:
    ld t0, {regime}+{pa0}+0(s6)
    csrw pmpaddr0, t0
    ld t0, {regime}+{pa0}+8(s6)
    csrw pmpaddr1, t0
    ld t0, {regime}+{pa0}+16(s6)
    csrw pmpaddr2, t0
    ld t0, {regime}+{pa0}+24(s6)
    csrw pmpaddr3, t0
    ld t0, {regime}+{pa0}+32(s6)
    csrw pmpaddr4, t0
    ld t0, {regime}+{pa0}+40(s6)
    csrw pmpaddr5, t0
    ld t0, {regime}+{pa0}+48(s6)
    csrw pmpaddr6, t0
    ld t0, {regime}+{pa0}+56(s6)
    csrw pmpaddr7, t0
    ld t0, {regime}+{cfg}(s6)
    csrw pmpcfg0, t0
    ld t0, {regime}+0(s6)
    csrw satp, t0
    sfence.vma
    ld t0, {regime}+8(s6)
    csrw stvec, t0
    ld t0, {regime}+16(s6)
    csrw sscratch, t0
    ld t0, {resume}+0(s6)
    csrw mepc, t0
    ld t0, {resume}+8(s6)
    csrw sstatus, t0
    li t0, 3 << 11
    csrc mstatus, t0
    li t0, 1 << 11
    csrs mstatus, t0
    csrw mie, zero
    sd s2, {rotor}(sp)
    sd s5, {current}(sp)
    li t0, {running}
    sd t0, {state}(s6)
    fence
    HOPOS_RV_CIPA s6
    HOPOS_RV_SYNC
    csrw mscratch, sp
    mv x31, s6
    ld x1, {gprs}+0(x31)
    ld x2, {gprs}+8(x31)
    ld x3, {gprs}+16(x31)
    ld x4, {gprs}+24(x31)
    ld x5, {gprs}+32(x31)
    ld x6, {gprs}+40(x31)
    ld x7, {gprs}+48(x31)
    ld x8, {gprs}+56(x31)
    ld x9, {gprs}+64(x31)
    ld x10, {gprs}+72(x31)
    ld x11, {gprs}+80(x31)
    ld x12, {gprs}+88(x31)
    ld x13, {gprs}+96(x31)
    ld x14, {gprs}+104(x31)
    ld x15, {gprs}+112(x31)
    ld x16, {gprs}+120(x31)
    ld x17, {gprs}+128(x31)
    ld x18, {gprs}+136(x31)
    ld x19, {gprs}+144(x31)
    ld x20, {gprs}+152(x31)
    ld x21, {gprs}+160(x31)
    ld x22, {gprs}+168(x31)
    ld x23, {gprs}+176(x31)
    ld x24, {gprs}+184(x31)
    ld x25, {gprs}+192(x31)
    ld x26, {gprs}+200(x31)
    ld x27, {gprs}+208(x31)
    ld x28, {gprs}+216(x31)
    ld x29, {gprs}+224(x31)
    ld x30, {gprs}+232(x31)
    ld x31, {gprs}+240(x31)
    mret
"#,
    scratch = const SCHED_SCRATCH,
    current = const SCHED_CURRENT,
    rotor = const SCHED_ROTOR,
    count = const SCHED_COUNT,
    list = const SCHED_LIST,
    s2 = const SCHED_S2_PA,
    clint = const SCHED_CLINT_PA,
    cap = const SCHED_SLEEP_CAP,
    msip = const SCHED_MSIP_PA,
    shift = const CAGE_SHIFT,
    ctxoff = const CTX_OFF,
    state = const CTX_STATE,
    ctrl = const CTX_CTRL_PA,
    bootpc = const CTX_BOOT_PC,
    bootarg = const CTX_BOOT_ARG,
    gprs = const CTX_GPRS,
    resume = const CTX_RESUME,
    regime = const CTX_REGIME,
    pa0 = const REGIME_PMPADDR0,
    cfg = const REGIME_PMPCFG0,
    wake = const CTX_WAKE,
    nopeek = const CTX_WAKE_NO_PEEK,
    revoke = const CTX_REVOKE,
    bootpending = const CtxState::BootPending as u64,
    saved = const CtxState::Saved as u64,
    running = const CtxState::Running as u64,
    dead = const CtxState::Dead as u64,
    fesr = const abi::hopabi::CTRL_FAULT_ESR,
    ffar = const abi::hopabi::CTRL_FAULT_FAR,
    fvec = const abi::hopabi::CTRL_FAULT_VEC,
    verify = const FAULT_CAGE_VERIFY,
);

#[cfg(test)]
mod tests {
    use super::*;
    use abi::layout::{CTX_BOOT_ARG, CTX_GPRS, CTX_REGIME, CTX_RESUME, CTX_WAKE, SCHED_MSIP_PA};

    #[test]
    fn the_offsets_fit_the_immediates() {
        // Een `ld`/`sd`-offset is 12 bits met teken: alles wat de switcher
        // als immediate gebruikt, moet onder 2048 blijven.
        for off in [
            CTX_GPRS + 240,
            CTX_RESUME + 8,
            CTX_REGIME + REGIME_PMPADDR0 + 56,
            CTX_WAKE,
            CTX_BOOT_ARG,
            SCHED_MSIP_PA,
        ] {
            assert!(off < 2048, "{off}");
        }
        // De regime-woorden eindigen vóór de wektijd.
        const { assert!(CTX_REGIME + REGIME_PMPADDR0 + 64 <= CTX_WAKE) };
        // x31 ligt op CTX_GPRS + 8*30: de laatste van de 31 woorden.
        assert_eq!(CTX_GPRS + 240 + 8, abi::layout::CTX_SP);
        assert_eq!(entry_pc(), 0);
    }
}
