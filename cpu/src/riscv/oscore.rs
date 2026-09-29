//! De OS-core op riscv64 (PORT.md beslissing 2): het hart van de kern als
//! deelbare core. De kern is er de eerste bewoner van, en zijn idle is een
//! beurt voor de andere bewoners (Hop, en de groepen die de kern het
//! toestaat). De riscv-helft van `cpu::el2::OsCore`, en bedoeld om ernaast
//! gelezen te worden.
//!
//! Dit bezit: de overgang van de kern (machine mode) naar een bewoner
//! (supervisor mode achter PMP plus Sv39) en terug, de rotatie over de
//! bewonerslijst van logische core 0 (sched-blok 0 van het plan), de eigen
//! `mtimecmp` die de deadline van de executor bewaakt terwijl een bewoner
//! draait, en de meetlat (`cpu::el2::OS_STATS`, dezelfde getallen als op
//! arm64). Niet van hier: wie er bewoner wordt (de kooi-lijm,
//! `hopos/src/cage_riscv.rs`) en wat de kern doet als hij terug is (de
//! executor).
//!
//! Waarom niet de M-mode-switcher van de app-harts ([`super::switch`]): die
//! slaapt zelf op de CLINT en kent alleen bewoners; de kern draait op dit
//! hart met zijn eigen trap-ingang, stack en interrupts. Hier is het
//! omgekeerd en klein: de kern roept [`OsCore::run`] als zijn executor niets
//! te doen heeft, de bewoner draait tot hij yieldt, exit doet of faultt, of
//! tot een interrupt (de PLIC, de kick, de wekker op de deadline) naar
//! machine mode trapt. Een machine-interrupt wordt in S-mode altijd genomen,
//! ongeacht `mstatus.MIE`, dus de bewoner kan de kern nooit buitensluiten.
//! De trap landt op [`os_trap`](self) (niet op de trap van de kern), die de
//! bewoner in zijn ctx-blok bewaart en de kern hervat, alsof `enter` gewoon
//! terugkeerde. De interrupt blijft pending en wordt genomen zodra de kern
//! zijn masker opent, precies zoals uit een `wfi`.
//!
//! De ctx-blokken zijn die van de switcher, byte voor byte: alles wat de kern
//! van een bewoner leest (`ctx_state`, `wake_due`, het fault-rapport) klopt
//! hier ook.
//!
//! FP: de kern gebruikt geen f-registers (gemeten 29-09: nul
//! f-instructies in het image; `tools/qemu-riscv-test.sh` toetst het bij
//! elke run). Daarom bewaart deze overgang f0..f31 niet: wat de bewoner erin
//! had, staat er bij zijn volgende beurt nog. Gaat de kern ooit FP
//! gebruiken, dan hoort het bewaren hier eerst.

use super::clint::{Clint, NEVER};
use super::csr;
use super::pmp;
use crate::el2::{self, Back, OS_STATS as STATS, Turn, ctx_read, ctx_state, ctx_write, rx_due};
use abi::hopabi::{CTRL_FAULT_ESR, CTRL_FAULT_FAR, CTRL_FAULT_VEC};
use abi::layout::{
    CAGE_STRIDE, CTX_BOOT_PC, CTX_CTRL_PA, CTX_GPRS, CTX_LEN, CTX_OFF, CTX_REGIME, CTX_RESUME,
    CTX_REVOKE, CTX_STATE, CTX_WAKE, CTX_WAKE_NO_PEEK, Core, CtxState, Plan, SCHED_COUNT,
    SCHED_CURRENT, SCHED_CURSOR, SCHED_LIST, SLOT_CAP,
};
use core::sync::atomic::{AtomicU32, AtomicU64, Ordering::Relaxed};
use dev::Pa;

/// `mcause` van een `ecall` uit S-mode.
const CAUSE_ECALL_S: u64 = 9;
/// De interruptbit van `mcause`.
const CAUSE_INTERRUPT: u64 = 1 << 63;
/// Interrupt-codes: de kick (`msip`), de wekker, de PLIC.
const IRQ_MSI: u64 = 3;
const IRQ_MTI: u64 = 7;
/// Wat `enter` teruggeeft als `pmpcfg0` anders terugleest dan de kern hem
/// schreef: de bewoner is nooit binnengelaten. Geen geldige `mcause`
/// (bit 62 bestaat daar niet).
const CAUSE_CAGE_VERIFY: u64 = 1 << 62;

/// Het woord van `a7` (x17) en `a0` (x10) in de GPR's van het ctx-blok:
/// xN staat op `CTX_GPRS + 8 * (N - 1)`.
const CTX_A7: u64 = CTX_GPRS + 8 * 16;
const CTX_A0: u64 = CTX_GPRS + 8 * 9;

/// De bewaarplaats van de kern tijdens een beurt (`mscratch` wijst erheen):
/// ra, sp, gp, tp, s0..s11, het ctx-blok, `mtvec`, `mscratch`, `mtval` van
/// de trap, en drie werkwoorden. Eén OS-core per node, dus één static; de
/// kern is de enige die hem aanraakt, en alleen binnen `enter`.
static SAVE: [AtomicU64; SAVE_WORDS] = [const { AtomicU64::new(0) }; SAVE_WORDS];
const SAVE_WORDS: usize = 24;
/// De offset van `mtval` in [`SAVE`] (bytes); de rest van de indeling
/// staat bij de assembly (`arch`).
const SAVE_MTVAL: u64 = 152;

/// De rotatie van de OS-core. Eén, eigendom van de slaap van de executor op
/// dat hart (`RvSleeper::host`).
pub struct OsCore {
    sched: Pa,
    cage: Pa,
    clint: Clint,
    hart: usize,
    pmp: pmp::Profile,
}

impl OsCore {
    /// De rotatie over het plan `plan`, op hart `hart` met zijn wekker op
    /// `clint`. `pmp` is de PMP van de CPU, voor de zelftest.
    pub fn new(
        plan: &Plan,
        clint: Clint,
        hart: usize,
        pmp: pmp::Profile,
    ) -> Result<OsCore, el2::Error> {
        let core0 = Core::new(0).ok_or(el2::Error::BadContextId { id: 0 })?;
        let sched = plan.park_mbox_pa(core0).map_err(el2::Error::Plan)?;
        Ok(OsCore {
            sched,
            cage: plan.vec_base_pa(),
            clint,
            hart,
            pmp,
        })
    }

    /// Eén beurt: de volgende bewoner die aan de beurt is (round-robin vanaf
    /// de cursor: een verse, of een geyielde wiens wektijd verstreek of wiens
    /// RX-ring groeide) draait tot hij het hart teruggeeft of tot `deadline`
    /// (TIME-tikken). Aanroepen met `mstatus.MIE` uit, ná de laatste
    /// `ready()`-toets van de executor.
    ///
    /// Een ingetrokken bewoner die slaapt of nog niet draaide, gaat hier
    /// dood zonder nog één instructie (zoals de rotatie van de switcher).
    pub fn run(&mut self, deadline: u64) -> Turn {
        let now = csr::rdtime();
        let count = usize::try_from(dev::read64(self.sched.add(SCHED_COUNT)))
            .unwrap_or(0)
            .min(SLOT_CAP);
        let cursor = usize::try_from(dev::read64(self.sched.add(SCHED_CURSOR))).unwrap_or(0);
        let mut earliest: Option<u64> = None;
        for k in 1..=count {
            let i = (cursor + k) % count;
            let id = list_get(self.sched, i);
            if id == 0 || usize::from(id) > SLOT_CAP {
                continue;
            }
            let ctx = self.ctx(id);
            let state = ctx_state(ctx);
            if matches!(state, Some(CtxState::BootPending | CtxState::Saved))
                && ctx_read(ctx, CTX_REVOKE) != 0
            {
                ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
                continue;
            }
            match state {
                Some(CtxState::BootPending) => {
                    return Turn::Ran(self.turn(i, id, ctx, deadline, true));
                }
                Some(CtxState::Saved) => match due(ctx, now) {
                    None => return Turn::Ran(self.turn(i, id, ctx, deadline, false)),
                    Some(t) => earliest = Some(earliest.map_or(t, |e| e.min(t))),
                },
                _ => {}
            }
        }
        STATS.idle.fetch_add(1, Relaxed);
        Turn::Idle { wake: earliest }
    }

    /// Het ctx-blok van kooi-context `id` (1..=SLOT_CAP).
    fn ctx(&self, id: u8) -> Pa {
        self.cage.add(u64::from(id) * CAGE_STRIDE + CTX_OFF)
    }

    /// De beurt van bewoner `id` op lijstplek `i`.
    fn turn(&mut self, i: usize, id: u8, ctx: Pa, deadline: u64, fresh: bool) -> Back {
        dev::write64(self.sched.add(SCHED_CURSOR), i as u64);
        dev::write64(self.sched.add(SCHED_CURRENT), u64::from(id));
        ctx_write(ctx, CTX_STATE, CtxState::Running.raw());
        STATS.entries.fetch_add(1, Relaxed);
        let t0 = csr::rdtime();
        let (cause, mtval) = self.enter(ctx, fresh, deadline);
        STATS
            .ticks
            .fetch_add(csr::rdtime().wrapping_sub(t0), Relaxed);
        dev::write64(self.sched.add(SCHED_CURRENT), 0);
        settle(ctx, cause, mtval, true)
    }

    /// De overgang zelf: de wekker op `deadline`, de drie bronnen aan die de
    /// kern terughalen (PLIC, kick, wekker), de sprong, en alles terug zoals
    /// het stond. Geeft `mcause` en `mtval` van de trap.
    fn enter(&self, ctx: Pa, fresh: bool, deadline: u64) -> (u64, u64) {
        const BACK: u64 = csr::MIP_MEIP | csr::MIP_MSIP | csr::MIP_MTIP;
        let mie = csr::mie();
        self.clint.set_timecmp(self.hart, deadline);
        csr::mie_set(BACK);
        let cause = arch::enter(ctx.0, u64::from(fresh), SAVE.as_ptr() as u64);
        csr::mie_clear(BACK);
        csr::mie_set(mie & BACK);
        self.clint.set_timecmp(self.hart, NEVER);
        (cause, SAVE[(SAVE_MTVAL / 8) as usize].load(Relaxed))
    }

    /// De zelftest bij boot: een bewoner zonder vertaling, alleen de
    /// whitelist over een pagina met een stub in het kern-image, die spint,
    /// yieldt of exit doet, met `kick` vlak vóór de overgang en de wekker op
    /// `ticks` na nu. Geeft waardoor de kern terugkwam en na hoeveel tikken;
    /// `None` als de kooi van de stub niet te coderen was.
    pub fn selftest(&mut self, probe: Probe, ticks: u64, kick: &dyn Fn()) -> Option<(Back, u64)> {
        let code: &[u32] = match probe {
            Probe::Spin => &[0x0000_006f],
            Probe::Yield => &[0x0000_0513, 0x0000_0893, 0x0000_0073, 0x0000_006f],
            Probe::Exit => &[0x0010_0893, 0x0000_0073, 0x0000_006f],
        };
        for (w, c) in STUB.0.iter().zip(code) {
            w.store(*c, Relaxed);
        }
        let base = STUB.0.as_ptr() as u64;
        let win = pmp::Window {
            base,
            size: 4096,
            r: true,
            w: false,
            x: true,
        };
        let enc = pmp::encode(&[win], self.pmp).ok()?;
        let ctx = Pa(SCRATCH_CTX.0.as_ptr() as u64);
        dev::clear(ctx, CTX_LEN as usize);
        let regime = ctx.add(CTX_REGIME);
        dev::write64(regime.add(super::switch::REGIME_PMPCFG0), enc.cfg);
        for (k, a) in enc.addr.iter().enumerate() {
            dev::write64(
                regime.add(super::switch::REGIME_PMPADDR0 + 8 * k as u64),
                *a,
            );
        }
        dev::write64(ctx.add(CTX_BOOT_PC), base);
        ctx_write(ctx, CTX_STATE, CtxState::Running.raw());
        let prev = csr::mask();
        kick();
        let t0 = csr::rdtime();
        let (cause, mtval) = self.enter(ctx, true, t0.wrapping_add(ticks));
        let dt = csr::rdtime().wrapping_sub(t0);
        // De kick staat nog: de dispatch van de kern wist hem pas later.
        self.clint.set_msip(self.hart, false);
        csr::restore(prev);
        Some((settle(ctx, cause, mtval, false), dt))
    }
}

/// Wat de zelftest van [`OsCore::selftest`] doet.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Probe {
    /// `j .`: alleen de wekker of de kick haalt de kern terug.
    Spin,
    /// `li a0,0; li a7,0; ecall`: de yield.
    Yield,
    /// `li a7,1; ecall`: de exit.
    Exit,
}

/// De stub van de zelftest: één pagina, zodat de whitelist hem precies
/// beschrijft. Atomics en geen `[u32]`: de kern schrijft er code in.
#[repr(C, align(4096))]
struct Page([AtomicU32; 1024]);
static STUB: Page = Page([const { AtomicU32::new(0) }; 1024]);

/// Het ctx-blok van de zelftest.
#[repr(C, align(64))]
struct CtxScratch([AtomicU64; (CTX_LEN / 8) as usize]);
static SCRATCH_CTX: CtxScratch = CtxScratch([const { AtomicU64::new(0) }; (CTX_LEN / 8) as usize]);

/// Byte `i` van de bewonerslijst van `sched` (woordgewijs gelezen).
fn list_get(sched: Pa, i: usize) -> u8 {
    let w = dev::read64(sched.add(SCHED_LIST + (i as u64 & !7))).to_le_bytes();
    w.get(i & 7).copied().unwrap_or(0)
}

/// Is de geyielde bewoner van `ctx` aan de beurt op `now`? `None` = ja;
/// anders zijn wektijd. Dezelfde vraag als de rotatie van de switcher:
/// de wektijd, of RX voorbij de gewapende drempel (tenzij no-peek).
fn due(ctx: Pa, now: u64) -> Option<u64> {
    let w = ctx_read(ctx, CTX_WAKE);
    let t = w & !CTX_WAKE_NO_PEEK;
    if t == 0 || now >= t {
        return None;
    }
    if w & CTX_WAKE_NO_PEEK == 0 && rx_due(ctx) {
        return None;
    }
    Some(t)
}

/// Zet de staat van een bewoner na zijn beurt, en zegt waardoor de kern
/// terug is. `count` = meetellen in de meetlat (niet voor de zelftest).
fn settle(ctx: Pa, cause: u64, mtval: u64, count: bool) -> Back {
    let tally = |c: &AtomicU64| {
        if count {
            c.fetch_add(1, Relaxed);
        }
    };
    if cause == CAUSE_CAGE_VERIFY {
        report(ctx, super::switch::FAULT_CAGE_VERIFY, mtval, 0);
        tally(&STATS.faults);
        return Back::Fault;
    }
    if cause & CAUSE_INTERRUPT != 0 {
        // Onderbroken midden in zijn werk: meteen weer aan de beurt, op
        // mepc zelf (een interrupt wijst naar de instructie die nog moet).
        ctx_write(ctx, CTX_WAKE, 0);
        ctx_write(ctx, CTX_STATE, CtxState::Saved.raw());
        return match cause & !CAUSE_INTERRUPT {
            IRQ_MTI => {
                tally(&STATS.timer);
                Back::Timer
            }
            IRQ_MSI => {
                tally(&STATS.ipi);
                Back::Ipi
            }
            _ => {
                tally(&STATS.irq);
                Back::Irq
            }
        };
    }
    if cause != CAUSE_ECALL_S {
        report(ctx, 1, cause, mtval);
        tally(&STATS.faults);
        return Back::Fault;
    }
    // Een ecall wijst met mepc naar zichzelf: hervatten op + 4.
    ctx_write(ctx, CTX_RESUME, ctx_read(ctx, CTX_RESUME).wrapping_add(4));
    if ctx_read(ctx, CTX_A7) != 0 {
        ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
        tally(&STATS.exits);
        return Back::Exit;
    }
    if ctx_read(ctx, CTX_REVOKE) != 0 {
        // De intrekking bij de yield, zoals de switcher.
        ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
        tally(&STATS.exits);
        return Back::Exit;
    }
    ctx_write(ctx, CTX_WAKE, ctx_read(ctx, CTX_A0));
    ctx_write(ctx, CTX_STATE, CtxState::Saved.raw());
    tally(&STATS.yields);
    Back::Yield
}

/// Het fault-rapport op de control-page (vec, mcause, mtval, zoals de
/// switcher), en de bewoner dood.
fn report(ctx: Pa, vec: u64, esr: u64, far: u64) {
    let cp = ctx_read(ctx, CTX_CTRL_PA);
    if cp != 0 {
        for (off, v) in [
            (CTRL_FAULT_VEC, vec),
            (CTRL_FAULT_ESR, esr),
            (CTRL_FAULT_FAR, far),
        ] {
            dev::write64(Pa(cp).add(off), v);
            dev::push(Pa(cp).add(off), 8);
        }
    }
    ctx_write(ctx, CTX_STATE, CtxState::Dead.raw());
}

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
mod arch {
    use super::{
        CAUSE_CAGE_VERIFY, CTX_BOOT_PC, CTX_GPRS, CTX_REGIME, CTX_RESUME, SAVE_MTVAL, SAVE_WORDS,
    };
    use crate::riscv::switch::{REGIME_PMPADDR0, REGIME_PMPCFG0};
    use abi::layout::CTX_BOOT_ARG;

    /// Offsets in `SAVE` (bytes): ra, sp, gp, tp en s0..s11 op 0..128, dan
    /// het ctx-blok, `mtvec`, `mscratch`, `mtval` en drie werkwoorden.
    const SAVE_CTX: u64 = 128;
    const SAVE_MTVEC: u64 = 136;
    const SAVE_MSCRATCH: u64 = 144;
    const SAVE_SCRATCH: u64 = 160;
    const _: () = assert!(SAVE_MTVAL == 152 && SAVE_SCRATCH + 24 <= 8 * SAVE_WORDS as u64);

    unsafe extern "C" {
        /// De overgang (hieronder): a0 = ctx-blok, a1 = koud (1) of
        /// hervatten (0), a2 = de bewaarplaats. Geeft `mcause`.
        fn __hopos_os_enter(ctx: u64, fresh: u64, save: u64) -> u64;
    }

    /// De overgang naar de bewoner van `ctx` en terug.
    pub(super) fn enter(ctx: u64, fresh: u64, save: u64) -> u64 {
        // SAFETY: `ctx` is een ctx-blok van het plan met een kooi die de
        // kern bouwde (of die van de zelftest); `save` is de static `SAVE`.
        // De assembly bewaart ra, sp, gp, tp en s0..s11 van de kern en zet
        // ze terug, zet `mtvec` en `mscratch` terug, en keert als een gewone
        // C-functie terug; de caller-saved registers zijn volgens de ABI
        // verloren. De kern gebruikt geen f-registers (de kop van dit
        // bestand), dus die hoeven niet mee.
        unsafe { __hopos_os_enter(ctx, fresh, save) }
    }

    core::arch::global_asm!(
        r#"
    .section .text.hopos_os, "ax"
    .balign 4
    .global __hopos_os_enter
__hopos_os_enter:
    sd ra, 0(a2)
    sd sp, 8(a2)
    sd gp, 16(a2)
    sd tp, 24(a2)
    sd s0, 32(a2)
    sd s1, 40(a2)
    sd s2, 48(a2)
    sd s3, 56(a2)
    sd s4, 64(a2)
    sd s5, 72(a2)
    sd s6, 80(a2)
    sd s7, 88(a2)
    sd s8, 96(a2)
    sd s9, 104(a2)
    sd s10, 112(a2)
    sd s11, 120(a2)
    sd a0, {sctx}(a2)
    csrr t0, mtvec
    sd t0, {smtvec}(a2)
    csrr t0, mscratch
    sd t0, {smscratch}(a2)
    // De kooi: pmpaddr0..7, dan pmpcfg0, en teruglezen. Een kooi die niet
    // aantoonbaar staat, wordt niet betreden.
    ld t0, {regime}+{pa0}+0(a0)
    csrw pmpaddr0, t0
    ld t0, {regime}+{pa0}+8(a0)
    csrw pmpaddr1, t0
    ld t0, {regime}+{pa0}+16(a0)
    csrw pmpaddr2, t0
    ld t0, {regime}+{pa0}+24(a0)
    csrw pmpaddr3, t0
    ld t0, {regime}+{pa0}+32(a0)
    csrw pmpaddr4, t0
    ld t0, {regime}+{pa0}+40(a0)
    csrw pmpaddr5, t0
    ld t0, {regime}+{pa0}+48(a0)
    csrw pmpaddr6, t0
    ld t0, {regime}+{pa0}+56(a0)
    csrw pmpaddr7, t0
    ld t0, {regime}+{cfg}(a0)
    csrw pmpcfg0, t0
    csrr t1, pmpcfg0
    beq t0, t1, 1f
    sd t1, {smtval}(a2)
    li a0, {verify}
    j 9f
1:
    // Geen delegatie (elke trap van de bewoner komt hier, niet in S-mode),
    // en de TIME-CSR voor zijn klok (mcounteren.TM), zoals `parkenter` op de
    // app-harts. De firmware kan beide anders achterlaten op dit hart.
    csrw medeleg, zero
    csrw mideleg, zero
    li t0, 2
    csrw mcounteren, t0
    csrw mscratch, a2
    la t0, __hopos_os_trap
    csrw mtvec, t0
    ld t0, {regime}+0(a0)
    csrw satp, t0
    sfence.vma
    ld t0, {regime}+8(a0)
    csrw stvec, t0
    ld t0, {regime}+16(a0)
    csrw sscratch, t0
    beqz a1, 2f

    // Koud: de ingang, a0 = het argument, alle andere registers nul (geen
    // kern-adres lekt de kooi in).
    ld t0, {bootpc}(a0)
    csrw mepc, t0
    li t0, 3 << 11
    csrc mstatus, t0
    li t0, 1 << 11
    csrs mstatus, t0
    li t0, 1 << 7
    csrc mstatus, t0
    ld a0, {bootarg}(a0)
    li ra, 0
    li sp, 0
    li gp, 0
    li tp, 0
    li t0, 0
    li t1, 0
    li t2, 0
    li s0, 0
    li s1, 0
    li a1, 0
    li a2, 0
    li a3, 0
    li a4, 0
    li a5, 0
    li a6, 0
    li a7, 0
    li s2, 0
    li s3, 0
    li s4, 0
    li s5, 0
    li s6, 0
    li s7, 0
    li s8, 0
    li s9, 0
    li s10, 0
    li s11, 0
    li t3, 0
    li t4, 0
    li t5, 0
    li t6, 0
    fence.i
    mret

    // Hervatten: sstatus vóór de MPP-bits (sstatus is een venster op
    // mstatus), dan de GPR's met x31 als basis, x31 als laatste.
2:
    ld t0, {resume}+0(a0)
    csrw mepc, t0
    ld t0, {resume}+8(a0)
    csrw sstatus, t0
    li t0, 3 << 11
    csrc mstatus, t0
    li t0, 1 << 11
    csrs mstatus, t0
    mv x31, a0
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

    // Elke trap tijdens een beurt: de bewoner bewaren in zijn ctx-blok (de
    // volgorde van de switcher), mepc rauw (de Rust-kant telt 4 bij een
    // ecall), en de kern terug alsof `enter` terugkeerde, met mcause.
    .balign 4
    .global __hopos_os_trap
__hopos_os_trap:
    csrrw sp, mscratch, sp
    sd t0, {sscratch}+0(sp)
    sd t1, {sscratch}+8(sp)
    sd t2, {sscratch}+16(sp)
    ld t0, {sctx}(sp)
    sd x1, {gprs}+0(t0)
    csrr t1, mscratch
    sd t1, {gprs}+8(t0)
    sd x3, {gprs}+16(t0)
    sd x4, {gprs}+24(t0)
    ld t1, {sscratch}+0(sp)
    sd t1, {gprs}+32(t0)
    ld t1, {sscratch}+8(sp)
    sd t1, {gprs}+40(t0)
    ld t1, {sscratch}+16(sp)
    sd t1, {gprs}+48(t0)
    sd x8, {gprs}+56(t0)
    sd x9, {gprs}+64(t0)
    sd x10, {gprs}+72(t0)
    sd x11, {gprs}+80(t0)
    sd x12, {gprs}+88(t0)
    sd x13, {gprs}+96(t0)
    sd x14, {gprs}+104(t0)
    sd x15, {gprs}+112(t0)
    sd x16, {gprs}+120(t0)
    sd x17, {gprs}+128(t0)
    sd x18, {gprs}+136(t0)
    sd x19, {gprs}+144(t0)
    sd x20, {gprs}+152(t0)
    sd x21, {gprs}+160(t0)
    sd x22, {gprs}+168(t0)
    sd x23, {gprs}+176(t0)
    sd x24, {gprs}+184(t0)
    sd x25, {gprs}+192(t0)
    sd x26, {gprs}+200(t0)
    sd x27, {gprs}+208(t0)
    sd x28, {gprs}+216(t0)
    sd x29, {gprs}+224(t0)
    sd x30, {gprs}+232(t0)
    sd x31, {gprs}+240(t0)
    csrr t1, mepc
    sd t1, {resume}+0(t0)
    csrr t1, sstatus
    sd t1, {resume}+8(t0)
    csrr t1, satp
    sd t1, {regime}+0(t0)
    csrr t1, stvec
    sd t1, {regime}+8(t0)
    csrr t1, sscratch
    sd t1, {regime}+16(t0)
    csrr t1, mtval
    sd t1, {smtval}(sp)
    csrr a0, mcause
    mv a2, sp

    // De kern terug (a2 = de bewaarplaats, a0 = wat enter geeft).
9:
    csrw satp, zero
    sfence.vma
    ld t0, {smtvec}(a2)
    csrw mtvec, t0
    ld t0, {smscratch}(a2)
    csrw mscratch, t0
    ld ra, 0(a2)
    ld gp, 16(a2)
    ld tp, 24(a2)
    ld s0, 32(a2)
    ld s1, 40(a2)
    ld s2, 48(a2)
    ld s3, 56(a2)
    ld s4, 64(a2)
    ld s5, 72(a2)
    ld s6, 80(a2)
    ld s7, 88(a2)
    ld s8, 96(a2)
    ld s9, 104(a2)
    ld s10, 112(a2)
    ld s11, 120(a2)
    ld sp, 8(a2)
    ret
"#,
        sctx = const SAVE_CTX,
        smtvec = const SAVE_MTVEC,
        smscratch = const SAVE_MSCRATCH,
        smtval = const SAVE_MTVAL,
        sscratch = const SAVE_SCRATCH,
        regime = const CTX_REGIME,
        pa0 = const REGIME_PMPADDR0,
        cfg = const REGIME_PMPCFG0,
        bootpc = const CTX_BOOT_PC,
        bootarg = const CTX_BOOT_ARG,
        gprs = const CTX_GPRS,
        resume = const CTX_RESUME,
        verify = const CAUSE_CAGE_VERIFY,
    );
}

#[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
mod arch {
    //! Host-stub: er is geen bewoner; een beurt is meteen een yield.
    pub(super) fn enter(_ctx: u64, _fresh: u64, _save: u64) -> u64 {
        super::CAUSE_ECALL_S
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn block() -> (Vec<u64>, Pa) {
        let mut v = vec![0u64; (CTX_LEN / 8) as usize];
        let pa = Pa(v.as_mut_ptr() as u64);
        (v, pa)
    }

    #[test]
    fn settle_reads_the_trap_like_the_switcher() {
        let (_v, ctx) = block();
        // Een yield met wektijd 500: Saved, hervat op + 4.
        ctx_write(ctx, CTX_A0, 500);
        ctx_write(ctx, CTX_RESUME, 0x5000_0010);
        assert_eq!(settle(ctx, CAUSE_ECALL_S, 0, false), Back::Yield);
        assert_eq!(ctx_read(ctx, CTX_WAKE), 500);
        assert_eq!(ctx_read(ctx, CTX_RESUME), 0x5000_0014);
        assert_eq!(ctx_state(ctx), Some(CtxState::Saved));
        // De wekker: Saved, meteen weer aan de beurt, mepc blijft.
        assert_eq!(
            settle(ctx, CAUSE_INTERRUPT | IRQ_MTI, 0, false),
            Back::Timer
        );
        assert_eq!(ctx_read(ctx, CTX_WAKE), 0);
        assert_eq!(ctx_read(ctx, CTX_RESUME), 0x5000_0014);
        assert_eq!(settle(ctx, CAUSE_INTERRUPT | IRQ_MSI, 0, false), Back::Ipi);
        assert_eq!(settle(ctx, CAUSE_INTERRUPT | 11, 0, false), Back::Irq);
        // Exit (a7 = 1): dood.
        ctx_write(ctx, CTX_A7, 1);
        assert_eq!(settle(ctx, CAUSE_ECALL_S, 0, false), Back::Exit);
        assert_eq!(ctx_state(ctx), Some(CtxState::Dead));
    }

    #[test]
    fn a_fault_is_reported_on_the_control_page() {
        let (_v, ctx) = block();
        let mut page = vec![0u64; 512];
        ctx_write(ctx, CTX_CTRL_PA, page.as_mut_ptr() as u64);
        assert_eq!(settle(ctx, 7, 0x8000_0000, false), Back::Fault);
        assert_eq!(page[(CTRL_FAULT_VEC / 8) as usize], 1);
        assert_eq!(page[(CTRL_FAULT_ESR / 8) as usize], 7);
        assert_eq!(page[(CTRL_FAULT_FAR / 8) as usize], 0x8000_0000);
        assert_eq!(ctx_state(ctx), Some(CtxState::Dead));
        assert_eq!(settle(ctx, CAUSE_CAGE_VERIFY, 0x42, false), Back::Fault);
        assert_eq!(
            page[(CTRL_FAULT_VEC / 8) as usize],
            super::super::switch::FAULT_CAGE_VERIFY
        );
    }

    #[test]
    fn due_follows_the_wake_time() {
        let (_v, ctx) = block();
        ctx_write(ctx, CTX_WAKE, 0);
        assert_eq!(due(ctx, 10), None);
        ctx_write(ctx, CTX_WAKE, 100);
        assert_eq!(due(ctx, 10), Some(100));
        assert_eq!(due(ctx, 100), None);
        ctx_write(ctx, CTX_WAKE, 100 | CTX_WAKE_NO_PEEK);
        assert_eq!(due(ctx, 10), Some(100));
    }
}
