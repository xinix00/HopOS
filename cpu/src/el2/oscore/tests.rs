//! Host-tests van de OS-core: de bewonerslijst, de eerste beurt en de
//! rotatie, over buffers. Op de host komt elke beurt terug op een IRQ; de
//! overgang zelf bewijst het board (de zelftest bij boot).

use super::*;
use abi::Region;
use abi::layout::{CTX_LEN, PlanSpec, Slot};

/// Een buffer met een gegarandeerde uitlijning; het adres is de `Pa`.
struct Buf {
    _mem: Vec<u64>,
    base: u64,
}

impl Buf {
    fn new(len: usize, align: u64) -> Buf {
        let mut mem = vec![0u64; (len + align as usize) / 8 + 1];
        let raw = mem.as_mut_ptr() as usize as u64;
        let base = (raw + align - 1) & !(align - 1);
        Buf { _mem: mem, base }
    }
}

/// Een plan over een host-buffer: drie slots, twee app-cores.
fn plan() -> (Buf, Plan) {
    let slots = 3u64;
    let cage = (slots + 1) * CAGE_STRIDE;
    let ctrl = (slots + 1) * 0x1000;
    let buf = Buf::new((cage + ctrl + 0x1000) as usize, CAGE_STRIDE);
    let end = buf.base + cage + ctrl + 0x1000;
    let mut pool = abi::layout::Pool::new();
    let grain = 2u64 << 20;
    pool.push(Region::new((end + 2 * grain) & !(grain - 1), grain))
        .unwrap();
    let spec = PlanSpec {
        node_ctrl_pa: buf.base + cage,
        cage_pa: buf.base,
        boot_scratch_pa: buf.base + cage + ctrl,
        pool,
        max_slots: slots as usize,
        app_cores: 2,
        ..PlanSpec::default()
    };
    (buf, Plan::new(spec).unwrap())
}

fn ctx(plan: &Plan, i: usize) -> Pa {
    plan.ctx_pa(Slot::new(i).unwrap()).unwrap()
}

fn list(plan: &Plan) -> Vec<u8> {
    let sched = os_sched(plan).unwrap();
    let n = dev::read64(sched.add(SCHED_COUNT)) as usize;
    (0..n)
        .map(|i| dev::read8(sched.add(SCHED_LIST + i as u64)))
        .collect()
}

#[test]
fn host_prepares_the_first_turn_and_joins_the_list() {
    let (_b, plan) = plan();
    let c = ctx(&plan, 1);
    // Rommel in het blok: verse DRAM is geen nul.
    for off in (0..CTX_LEN).step_by(8) {
        dev::write64(c.add(off), 0xdead_beef);
    }
    host(&plan, c, 0x5001_0000, 0xbfe0_0000).unwrap();
    assert_eq!(ctx_state(c), Some(CtxState::BootPending));
    assert_eq!(ctx_read(c, CTX_GPRS), 0xbfe0_0000);
    assert!((1..31).all(|r| ctx_read(c, CTX_GPRS + 8 * r) == 0));
    assert_eq!(ctx_read(c, CTX_RESUME), 0x5001_0000);
    assert_eq!(ctx_read(c, CTX_RESUME + 8), SPSR_EL1H_MASKED);
    assert_eq!(ctx_read(c, CTX_REGIME), SCTLR_EL1_CLEAN);
    assert!((1..CTX_REGIME_ARM_WORDS).all(|w| ctx_read(c, CTX_REGIME + 8 * w) == 0));
    assert_eq!(ctx_read(c, CTX_WAKE), 0);
    assert_eq!(list(&plan), [1]);
    assert!(hosts(&plan, c));
    // Idempotent: een tweede keer is geen tweede plek.
    host(&plan, c, 0x5001_0000, 0xbfe0_0000).unwrap();
    assert_eq!(list(&plan), [1]);
}

#[test]
fn unhost_leaves_a_gap_that_the_next_host_reuses() {
    let (_b, plan) = plan();
    host(&plan, ctx(&plan, 1), 1, 0x1000).unwrap();
    host(&plan, ctx(&plan, 2), 1, 0x2000).unwrap();
    assert_eq!(list(&plan), [1, 2]);
    assert!(unhost(&plan, ctx(&plan, 1)).unwrap());
    assert!(!unhost(&plan, ctx(&plan, 1)).unwrap());
    assert!(!hosts(&plan, ctx(&plan, 1)));
    assert_eq!(list(&plan), [0, 2]);
    host(&plan, ctx(&plan, 3), 1, 0x3000).unwrap();
    assert_eq!(list(&plan), [3, 2]);
    // Een adres dat geen kooi-context is, weigert.
    assert!(host(&plan, Pa(0x1234), 1, 0x1000).is_err());
}

#[test]
fn the_rotation_turns_round_robin_and_sleeps_on_the_earliest_wake() {
    let (_b, plan) = plan();
    let mut os = OsCore::new(&plan, Flavor::Nvhe, None).unwrap();
    assert_eq!(os.run(0), Turn::Idle { wake: None });
    let (c1, c2) = (ctx(&plan, 1), ctx(&plan, 2));
    host(&plan, c1, 1, 0x1000).unwrap();
    host(&plan, c2, 1, 0x2000).unwrap();
    // Op de host komt elke beurt terug op een IRQ: Saved en meteen weer aan
    // de beurt. Round-robin vanaf de plek ná de cursor (de laatst
    // geplande, 0 bij de start), zoals de switcher: eerst plek 1, dan 0.
    assert_eq!(os.run(0), Turn::Ran(Back::Irq));
    assert_eq!(ctx_state(c2), Some(CtxState::Saved));
    assert_eq!(ctx_state(c1), Some(CtxState::BootPending));
    assert_eq!(os.run(0), Turn::Ran(Back::Irq));
    assert_eq!(ctx_state(c1), Some(CtxState::Saved));
    // Beide geyield met een wektijd in de toekomst (de host-teller staat
    // op 0): niemand aan de beurt, en de kern slaapt tot de vroegste.
    ctx_write(c1, CTX_WAKE, 500);
    ctx_write(c2, CTX_WAKE, 300);
    assert_eq!(os.run(0), Turn::Idle { wake: Some(300) });
    // Een kick wint van de wektijd.
    ctx_write(c1, CTX_KICK_PENDING, 1);
    assert_eq!(os.run(0), Turn::Ran(Back::Irq));
    assert_eq!(ctx_read(c1, CTX_KICK_PENDING), 0);
    // Een dode of uit de lijst gehaalde bewoner draait nooit meer.
    ctx_write(c1, CTX_STATE, CtxState::Dead.raw());
    unhost(&plan, c2).unwrap();
    assert_eq!(os.run(0), Turn::Idle { wake: None });
}

#[test]
fn the_kick_is_told_apart_from_an_irq() {
    let (_b, plan) = plan();
    fn kick() -> u32 {
        9
    }
    let bell = Bell {
        sgi1r: 1 << 24,
        sgir: 0,
        intid: 9,
        pending: kick,
    };
    let mut os = OsCore::new(&plan, Flavor::Nvhe, Some(bell)).unwrap();
    host(&plan, ctx(&plan, 1), 1, 0x1000).unwrap();
    assert_eq!(os.run(0), Turn::Ran(Back::Ipi));
    // Buiten een beurt staat de kick niet scherp.
    let sched = os_sched(&plan).unwrap();
    assert_eq!(dev::read64(sched.add(SCHED_OS_KICK)), 0);
    assert_eq!(dev::read64(sched.add(SCHED_OS_KICK_PA)), 0);
    os.listen(true);
    assert_eq!(dev::read64(sched.add(SCHED_OS_KICK)), 1 << 24);
    os.listen(false);
    assert_eq!(dev::read64(sched.add(SCHED_OS_KICK)), 0);
}

#[test]
fn apple_cannot_share_its_os_core_yet() {
    let (_b, plan) = plan();
    assert!(matches!(
        OsCore::new(&plan, Flavor::AppleVhe, None),
        Err(Error::OsCoreFlavor)
    ));
}

#[test]
fn a_gicv2_bell_publishes_its_mmio_address() {
    let (_b, plan) = plan();
    fn none() -> u32 {
        1023
    }
    let bell = Bell {
        sgi1r: (1 << 16) | 8,
        sgir: 0xff84_1f00,
        intid: 8,
        pending: none,
    };
    let os = OsCore::new(&plan, Flavor::Nvhe, Some(bell)).unwrap();
    let sched = os_sched(&plan).unwrap();
    assert_eq!(dev::read64(sched.add(SCHED_OS_KICK_PA)), 0xff84_1f00);
    os.listen(true);
    assert_eq!(dev::read64(sched.add(SCHED_OS_KICK)), (1 << 16) | 8);
}
