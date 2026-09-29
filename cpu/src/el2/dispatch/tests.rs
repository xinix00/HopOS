//! Host-tests van de switcher-API: de descriptor en de som, de thunks, het
//! startschot en de ctx-lezers, over buffers. De assembly bewijst het board.

use super::*;
use abi::Region;
use abi::checksum::fnv64;
use abi::layout::{CTX_BOOT_PC, CTX_NEXT_PA, CTX_SLEEPS, PlanSpec};

/// Een buffer met een gegarandeerde uitlijning; het adres is de `Pa`.
struct Buf {
    mem: Vec<u64>,
    base: u64,
}

impl Buf {
    fn new(len: usize, align: u64) -> Buf {
        let mut mem = vec![0u64; (len + align as usize) / 8 + 1];
        let raw = mem.as_mut_ptr() as usize as u64;
        let base = (raw + align - 1) & !(align - 1);
        Buf { mem, base }
    }
    fn pa(&self) -> Pa {
        Pa(self.base)
    }
}

/// Een plan over een host-buffer: drie slots, drie app-cores. De
/// kooi-regio, de node-pages en de boot-scratch liggen in dezelfde buffer,
/// de pool er ver voorbij (hij wordt nooit aangeraakt).
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
        app_cores: 3,
        ..PlanSpec::default()
    };
    let p = Plan::new(spec).unwrap();
    (buf, p)
}

fn core(i: usize) -> Core {
    Core::new(i).unwrap()
}

fn slot(i: usize) -> Slot {
    Slot::new(i).unwrap()
}

#[test]
fn magic_spells_hopswtc1() {
    assert_eq!(&SWITCH_MAGIC.to_le_bytes(), b"HOPSWTC1");
}

#[test]
fn descriptor_layout_and_sum() {
    let buf = Buf::new(SWITCH_CODE_MAX as usize, 64);
    let a = [0x11u8; 100];
    let b = [0x22u8; 64];
    let c = [0x33u8; 3];
    let blobs: [&[u8]; 3] = [&a, &b, &c];
    let got = install_blobs(buf.pa(), &blobs).unwrap();

    // Elke blob op een cacheline, na een kop van 64 bytes.
    let base = buf.pa();
    assert_eq!(got.entry, base.add(0x40));
    assert_eq!(got.tramp, base.add(0x40 + 128));
    assert_eq!(got.smp_tramp, base.add(0x40 + 128 + 64));
    assert_eq!(got.len, 0x40 + 128 + 64 + 64);
    assert_eq!(dev::read64(base), SWITCH_MAGIC);
    assert_eq!(dev::read64(base.add(SW_LEN)), got.len);
    assert_eq!(dev::read64(base.add(SW_ENTRY)), 0x40);
    assert_eq!(dev::read64(base.add(SW_TRAMP)), 0xC0);
    assert_eq!(dev::read64(base.add(SW_SMP)), 0x100);

    // De som is FNV-1a-64 over de blobs aaneen, in kopieervolgorde: dezelfde
    // som die een kern-flip over een nieuwe bundel rekent.
    let mut flat = Vec::new();
    flat.extend_from_slice(&a);
    flat.extend_from_slice(&b);
    flat.extend_from_slice(&c);
    assert_eq!(got.hash, fnv64(&flat));
    assert_eq!(dev::read64(base.add(SW_HASH)), got.hash);

    // De bytes staan er echt.
    let mut out = [0u8; 100];
    dev::copy_out(&mut out, got.entry);
    assert_eq!(out, a);
    let mut out = [0u8; 3];
    dev::copy_out(&mut out, got.smp_tramp);
    assert_eq!(out, c);
}

#[test]
fn adoption_reads_without_writing_and_refuses_foreign_code() {
    let buf = Buf::new(SWITCH_CODE_MAX as usize, 64);
    let a = [1u8; 40];
    let b = [2u8; 40];
    let c = [3u8; 40];
    let ours: [&[u8]; 3] = [&a, &b, &c];
    let installed = install_blobs(buf.pa(), &ours).unwrap();
    let before = buf.mem.clone();
    assert_eq!(adopt_blobs(buf.pa(), &ours), Ok(installed));
    assert_eq!(before, buf.mem, "adoption wrote to the resident copy");

    // Andere code: weigeren, met beide sommen in de fout.
    let d = [4u8; 40];
    let theirs: [&[u8]; 3] = [&a, &b, &d];
    match adopt_blobs(buf.pa(), &theirs) {
        Err(Error::SwitchCodeMismatch { resident, ours }) => {
            assert_eq!(resident, installed.hash);
            assert_ne!(ours, installed.hash);
        }
        other => panic!("foreign code adopted: {other:?}"),
    }

    // Geen magic: niets te adopteren.
    dev::write64(buf.pa(), 0);
    assert!(matches!(
        adopt_blobs(buf.pa(), &ours),
        Err(Error::SwitchCodeMismatch { .. })
    ));
}

#[test]
fn switch_code_that_does_not_fit_is_refused() {
    let big = vec![0u8; SWITCH_CODE_MAX as usize];
    let blobs: [&[u8]; 3] = [&big, &[1], &[2]];
    assert!(matches!(place(&blobs), Err(Error::SwitchCodeFull { .. })));
}

#[test]
fn host_has_no_switch_code() {
    let (_buf, p) = plan();
    assert_eq!(
        install_switch_code(&p, Flavor::Nvhe),
        Err(Error::NoSwitchCode)
    );
    assert_eq!(image_hash(Flavor::AppleVhe), Err(Error::NoSwitchCode));
    assert_eq!(installed_hash(&p), None);
}

#[test]
fn thunk_matches_the_go_generator() {
    // stage2.go: stp x2, x3, [sp, #16]; movz x2, #v; a64.Mov64(3, entry);
    // br x3. Nulhelften worden overgeslagen.
    let (w, n) = thunk(8, Pa(0x4000_A040));
    assert_eq!(
        &w[..n],
        &[
            0xA901_0FE2,
            0xD280_0102,
            0xD294_0803,
            0xF2A8_0003,
            0xD61F_0060
        ]
    );
    // Een adres boven de 4 GB (de Altra-pool): alle vier de helften.
    let (w, n) = thunk(15, Pa(0x0001_2345_6789_ABCD));
    assert_eq!(n, 7);
    assert_eq!(w[1], 0xD280_01E2);
    assert_eq!(w[2], movz(3, 0xABCD, 0));
    assert_eq!(w[3], movk(3, 0x6789, 16));
    assert_eq!(w[4], movk(3, 0x2345, 32));
    assert_eq!(w[5], movk(3, 0x0001, 48));
    assert_eq!(w[6], BR_X3);
}

#[test]
fn init_writes_thunks_park_schedules_and_empty_contexts() {
    let (_buf, p) = plan();
    // Vuil, zoals verse DRAM.
    dev::write64(p.park_mbox_pa(core(2)).unwrap().add(SCHED_COUNT), 77);
    dev::write64(p.ctx_pa(slot(3)).unwrap(), 0xdead);
    let entry = p.switch_code_pa().add(SW_HEAD);
    let park = [0xAAu8; 48];
    init_region(&p, entry, Some(&park)).unwrap();

    let vecs = p.vec_base_pa();
    for v in 0..VEC_COUNT {
        let (w, n) = thunk(v, entry);
        for (i, ins) in w.iter().take(n).enumerate() {
            assert_eq!(dev::read32(vecs.add(v * VEC_STRIDE + i as u64 * 4)), *ins);
        }
    }
    let mut out = [0u8; 48];
    dev::copy_out(&mut out, p.park_code_pa());
    assert_eq!(out, park);
    for c in 0..=3 {
        let mb = p.park_mbox_pa(core(c)).unwrap();
        assert_eq!(dev::read64(mb.add(SCHED_S2_PA)), vecs.0);
        assert_eq!(dev::read64(mb.add(SCHED_COUNT)), 0);
    }
    for s in 1..=3 {
        let block = p.cage_table_pa(slot(s)).unwrap();
        assert_eq!(dev::read64(block.add(CTX_OFF)), CtxState::Empty.raw());
        assert_eq!(dev::read64(block.add(SMP_CTX_OFF)), CtxState::Empty.raw());
    }
}

#[test]
fn a_kept_park_loop_keeps_mailbox_word_zero() {
    let (_buf, p) = plan();
    let mb = p.park_mbox_pa(core(1)).unwrap();
    dev::write64(mb, PARK_PARKED);
    dev::write64(p.park_code_pa(), 0x1234);
    init_region(&p, Pa(0x4000_0000), None).unwrap();
    assert_eq!(
        dev::read64(mb),
        PARK_PARKED,
        "the park loop would read 0 as a dispatch"
    );
    assert_eq!(dev::read64(p.park_code_pa()), 0x1234);
    assert!(check_parked(&p).is_ok());
    dev::write64(p.park_mbox_pa(core(2)).unwrap(), 0x5000_1000);
    assert_eq!(
        check_parked(&p),
        Err(Error::UnparkedCore {
            core: 2,
            mbox: 0x5000_1000
        })
    );
}

#[test]
fn context_ids_round_trip_like_the_switcher() {
    let (_buf, p) = plan();
    for id in 1..=3u8 {
        let ctx = context_pa(&p, id).unwrap();
        assert_eq!(ctx, p.ctx_pa(slot(usize::from(id))).unwrap());
        assert_eq!(context_id(&p, ctx), Some(id));
    }
    // Secundaire core 2 en 3: id 129 en 130 (hopos_el2_ctx_of: id - 127).
    for c in 2..=3 {
        let id = core(c).smp_context_id().unwrap();
        let ctx = context_pa(&p, id).unwrap();
        assert_eq!(ctx, p.smp_ctx_pa(core(c)).unwrap());
        assert_eq!(context_id(&p, ctx), Some(id));
    }
    assert_eq!(context_pa(&p, 0), Err(Error::BadContextId { id: 0 }));
    assert!(context_pa(&p, 4).is_err());
    assert_eq!(context_id(&p, p.vec_base_pa().add(CTX_OFF)), None);
    assert_eq!(context_id(&p, p.ctx_pa(slot(1)).unwrap().add(8)), None);
}

#[test]
fn dispatch_resets_the_rotation_and_fires_the_mailbox() {
    let (_buf, p) = plan();
    init_region(&p, Pa(0x4000_0000), Some(&[0; 4])).unwrap();
    let ctx = p.ctx_pa(slot(2)).unwrap();
    let mb = p.park_mbox_pa(core(1)).unwrap();
    dev::write64(mb.add(SCHED_LIST + 8), 0x0909);

    let start = dispatch(&p, core(1), ctx, Pa(0x4000_A100), 0x5100_0000).unwrap();
    assert_eq!(start, Start::Cold);
    assert_eq!(dev::read64(mb.add(SCHED_MBOX_CTX)), 0x5100_0000);
    assert_eq!(dev::read64(mb.add(SCHED_MBOX_PC)), 0x4000_A100);
    assert_eq!(dev::read64(mb.add(SCHED_CURRENT)), 2);
    assert_eq!(dev::read64(mb.add(SCHED_CURSOR)), 0);
    assert_eq!(dev::read64(mb.add(SCHED_COUNT)), 1);
    assert_eq!(dev::read8(mb.add(SCHED_LIST)), 2);
    assert_eq!(dev::read64(mb.add(SCHED_LIST + 8)), 0);
    assert_eq!(ctx_state(ctx), Some(CtxState::Running));
    assert_eq!(core_state(&p, core(1)), Ok(CoreState::Running(0x5100_0000)));

    // Een draaiende core wordt niet gekaapt.
    assert_eq!(
        dispatch(&p, core(1), ctx, Pa(0x4000_A100), 0x5100_0000),
        Err(Error::CoreRunning {
            core: 1,
            mbox: 0x5100_0000
        })
    );
    // Geparkeerd: de SEV wekt hem.
    dev::write64(mb, PARK_PARKED);
    assert_eq!(core_state(&p, core(1)), Ok(CoreState::Parked));
    assert_eq!(
        dispatch(&p, core(1), ctx, Pa(0x4000_A100), 0x5100_0000),
        Ok(Start::Woken)
    );
    // x0 = 0 of 1 leest de lus als koud of geparkeerd; een vreemd adres is
    // geen ctx-blok.
    assert_eq!(
        dispatch(&p, core(2), ctx, Pa(0), PARK_PARKED),
        Err(Error::BadArg { arg: PARK_PARKED })
    );
    assert_eq!(
        dispatch(&p, core(2), ctx.add(8), Pa(0), 0x5100_0000),
        Err(Error::BadContext { pa: ctx.0 + 8 })
    );
}

#[test]
fn smp_handoff_owns_privileges_and_freezes_request() {
    // smp_context_test.go, geport.
    let request = Buf::new(256, 64);
    let handoff = Buf::new(256, 64);
    let (src, dst) = (request.pa(), handoff.pa());
    dev::write64(src.add(CTRL_SMP_SP), 0x5001_2000);
    dev::write64(src.add(CTRL_S2_TABLE), 0xBAD);
    prepare_smp(dst, src, 0x8000_0000, 3, Pa(0x8100_0000), Pa(0x8200_0000));
    dev::write64(src.add(CTRL_SMP_SP), 0x5002_2000);
    for (off, want) in [
        (CTRL_SMP_SP, 0x5001_2000),
        (CTRL_S2_TABLE, 0x8000_0000),
        (CTRL_SLOT, 3),
        (CTRL_SMP_MBOX, 0x8100_0000),
        (CTRL_VEC_PA, 0x8200_0000),
    ] {
        assert_eq!(dev::read64(dst.add(off)), want, "handoff[{off:#x}]");
    }
    // Een node-core gebruikt hetzelfde mechanisme in zijn eigen page.
    prepare_smp(src, src, 0, 0, Pa(0), Pa(0x8300_0000));
    assert_eq!(dev::read64(src.add(CTRL_S2_TABLE)), 0);
    assert_eq!(dev::read64(src.add(CTRL_SMP_SP)), 0x5002_2000);
    // De handoff past in de eigen cachelines van het ctx-blok.
    const { assert!(abi::layout::CTX_SMP.is_multiple_of(64)) };
    const { assert!(abi::layout::CTX_SMP + 256 <= CTX_LEN) };
}

#[test]
fn wake_due_mirrors_the_rotation() {
    let ctx = Buf::new(CTX_LEN as usize, 64);
    let page = Buf::new(0x1000, 64);
    let head = Buf::new(64, 64);
    let c = ctx.pa();
    // Nu, en een verstreken wektijd.
    assert!(wake_due(c, 100));
    ctx_write(c, CTX_WAKE, 500);
    assert!(!wake_due(c, 499));
    assert!(wake_due(c, 500));
    // RX: gewapend en de kop voorbij de drempel.
    arm_context(c, page.pa(), slot(1), head.base);
    assert_eq!(ctx_read(c, CTX_KICK_TARGET), CTX_KICK_NONE);
    dev::write64(page.pa().add(CTRL_RX_DOOR), RX_DOOR_ARMED | 10);
    dev::write64(head.pa(), 10);
    assert!(
        !wake_due(c, 0),
        "a head that did not pass the threshold is no traffic"
    );
    dev::write64(head.pa(), 11);
    assert!(wake_due(c, 0));
    // Ongewapend: geen peek.
    dev::write64(page.pa().add(CTRL_RX_DOOR), 10);
    assert!(!rx_due(c));
    // Een wachter zonder P: alleen zijn wektijd.
    dev::write64(page.pa().add(CTRL_RX_DOOR), RX_DOOR_ARMED | 10);
    ctx_write(c, CTX_WAKE, CTX_WAKE_NO_PEEK | 500);
    assert!(!wake_due(c, 0));
    assert!(wake_due(c, 500));
    // Geen control-page: nooit RX.
    ctx_write(c, CTX_CTRL_PA, 0);
    assert!(!rx_due(c));
}

#[test]
fn ctx_state_reads_what_the_switcher_writes() {
    let ctx = Buf::new(CTX_LEN as usize, 64);
    for st in [
        CtxState::Empty,
        CtxState::BootPending,
        CtxState::Saved,
        CtxState::Running,
        CtxState::Dead,
    ] {
        ctx_write(ctx.pa(), CTX_STATE, st.raw());
        assert_eq!(ctx_state(ctx.pa()), Some(st));
    }
    ctx_write(ctx.pa(), CTX_STATE, 9);
    assert_eq!(ctx_state(ctx.pa()), None);
    ctx_write(ctx.pa(), CTX_SLEEPS, 42);
    assert_eq!(ctx_read(ctx.pa(), CTX_SLEEPS), 42);
    ctx_write(ctx.pa(), CTX_BOOT_PC, 7);
    ctx_write(ctx.pa(), CTX_NEXT_PA, ctx.base);
    assert_eq!(ctx_read(ctx.pa(), CTX_NEXT_PA), ctx.base);
}

#[test]
fn apple_kick_targets_core_and_cluster() {
    // aff0 = core 3, aff1 = cluster 1 (een P-core van de M4).
    assert_eq!(apple_ipi_target(0x8000_0103), 0x1_0003);
    assert_eq!(apple_ipi_target(0x0000_0000), 0);
}

#[test]
fn revoke_clears_the_tables_of_the_slot_only() {
    let (_buf, p) = plan();
    let block = p.cage_table_pa(slot(2)).unwrap();
    dev::write64(block, 0x1234_0003);
    dev::write64(block.add(CTX_OFF), CtxState::Saved.raw());
    revoke(&p, slot(2)).unwrap();
    assert_eq!(dev::read64(block), 0);
    assert_eq!(
        dev::read64(block.add(CTX_OFF)),
        CtxState::Saved.raw(),
        "a half-saved context must survive the kill"
    );
}
