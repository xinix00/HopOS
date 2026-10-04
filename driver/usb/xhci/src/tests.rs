//! De Go-tests van `gui/driver/usb/xhci` (bounds, ownership, recovery),
//! plus de parse- en ringtoetsen die de Rust-vorm erbij vraagt.
//!
//! Op de host schrijven `dev::read32`/`write32` vluchtig naar gewoon
//! geheugen, dus een `Vec<u64>` is een nep-registerblok of een
//! nep-DMA-regio. De klok is een teller die bij elke blik een milliseconde
//! verder staat en bij elke slaap de hele slaap, zodat elke wachtlus
//! eindigt; de slaap zelf is meteen klaar, dus één poll draait een
//! `async` pad tot het eind ([`block`]).

use super::*;
use crate::device::{Claim, descriptor_mps0, interval_exponent, parse_config};
use crate::host::SlotRes;
use core::future::{Future, ready};
use core::pin::pin;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use core::task::{Context, Poll, Waker};
use core::time::Duration;
use std::vec;
use std::vec::Vec;

static NOW: AtomicU64 = AtomicU64::new(0);

/// De klok van de tests: elke blik een milliseconde, elke slaap zijn duur.
struct Clock;

impl Timer for Clock {
    fn now(&self) -> u64 {
        NOW.fetch_add(1_000_000, Relaxed)
    }
    fn sleep(&self, d: Duration) -> impl Future<Output = ()> {
        NOW.fetch_add(u64::try_from(d.as_nanos()).unwrap_or(u64::MAX), Relaxed);
        ready(())
    }
}

/// Draait een `async` pad dat nooit echt wacht: de slaap van [`Clock`] is
/// meteen klaar.
fn block<F: Future>(f: F) -> F::Output {
    let mut f = pin!(f);
    match f.as_mut().poll(&mut Context::from_waker(Waker::noop())) {
        Poll::Ready(v) => v,
        Poll::Pending => panic!("a test future waited"),
    }
}

/// Nep-geheugen: 8-uitgelijnd en zo lang de test loopt.
struct Mem(Vec<u64>);

impl Mem {
    fn new(bytes: usize) -> Self {
        Self(vec![0; bytes.div_ceil(8)])
    }
    fn pa(&self) -> Pa {
        Pa(self.0.as_ptr() as u64)
    }
}

/// Een controller met `n` slots aan software-kant, zonder één register.
fn ownership_hc(n: usize) -> Hc {
    let mut h = Hc::at(Pa(0), "test", 0);
    h.n_slots = n;
    for i in 1..=n {
        h.res[i] = Some(SlotRes {
            dev_ctx: Pa(0),
            in_ctx: Pa(0),
            ctrl: Ring::default(),
            intr: [Ring::default(); MAX_HID_IFACES],
            buf: Pa(0),
            in_use: false,
            quarantined: false,
            dev: None,
        });
    }
    h
}

// --- bounds_test.go ---------------------------------------------------------

// Go: TestArenaRejectsWrappedAllocation
#[test]
fn arena_rejects_wrapped_allocation() {
    for mut a in [
        Arena(dev::Bump::new(u64::MAX - 8, 8)),
        Arena(dev::Bump::new(16, 16)),
    ] {
        assert!(
            a.alloc(u64::MAX, 4096).is_err(),
            "wrapped allocation accepted"
        );
    }
    let mut a = Arena(dev::Bump::new(16, 16));
    assert!(a.alloc(8, 3).is_err(), "alignment that is no power of two");
}

// Go: TestControlRefusesUnconfirmedOwnerBeforeTouchingDMA
#[test]
fn control_refuses_unconfirmed_owner_before_touching_dma() {
    let mut h = Hc::at(Pa(0), "test", 0);
    h.poisoned = Some(Poison::DisableUnconfirmed { slot: 1 });
    assert!(matches!(
        block(h.control(1, 0, 0, 0, 0, 0, &Clock)),
        Err(Error::Poisoned(_))
    ));
    assert!(matches!(
        block(h.command(&Clock, 0, 0, 0, 0, "test")),
        Err(Error::Poisoned(_))
    ));
    h.poisoned = None;
    assert_eq!(
        block(h.control(1, 0, 0, 0, 0, device::BUF_CTRL_SIZE as u16 + 1, &Clock)),
        Err(Error::ControlTooLong {
            len: device::BUF_CTRL_SIZE as u16 + 1
        })
    );
}

// Go: TestControllerTimeoutKeepsDMAOwned
#[test]
fn controller_timeout_keeps_dma_owned() {
    let regs = Mem::new(64);
    let cmd_mem = Mem::new(4096);
    let p = regs.pa();
    let mut h = Hc::at(p, "test", 0);
    h.probed = true;
    h.op = p;
    h.db = p;
    h.evt = Some(EvRing::new(p, p.0, 1));
    h.cmd = Some(Ring::new(cmd_mem.pa(), cmd_mem.pa().0, 4096));
    // Een transfer die uitblijft is een apparaat dat hapert: de controller
    // blijft van ons, en de aanroeper reset alleen die endpoint.
    let r = block(h.wait_event(&Clock, |_| false, 0, "test"));
    assert!(matches!(r, Err(Error::EventTimeout { .. })), "{r:?}");
    assert_eq!(h.poisoned, None);
    // Een commando dat uitblijft is de controller zelf: ownership onbekend.
    let r = block(h.command(&Clock, 0, 0, 0, 0, "test"));
    assert!(matches!(r, Err(Error::Poisoned(_))), "{r:?}");
    assert!(h.poisoned.is_some(), "command timeout lost ownership");
    h.running = true;
    assert!(
        block(h.start(Pa(4096), 4096, &Clock)).is_err(),
        "running controller can overwrite DMA"
    );
    h.poisoned = None;
    assert_eq!(block(h.start(Pa(4096), 4096, &Clock)), Err(Error::Running));
}

// Go: TestEP0DescriptorPacketSize
#[test]
fn ep0_descriptor_packet_size() {
    assert_eq!(descriptor_mps0(Speed::SUPER, 9), Ok(512));
    assert!(descriptor_mps0(Speed::FULL, 9).is_err());
    assert_eq!(descriptor_mps0(Speed::FULL, 32), Ok(32));
    assert!(descriptor_mps0(Speed::LOW, 64).is_err());
}

// Go: TestTenRootHostsFitExistingDMAWindow
//
// De echte `start` in tien ongelijke stukken van één DMA-regio. Een
// root-only host heeft hooguit één slot per fysieke poort nodig.
#[test]
fn ten_root_hosts_fit_existing_dma_window() {
    const SIZE: usize = 2 << 20;
    let memory = Mem::new(SIZE);
    let base = memory.pa().0;
    let span = (SIZE / 10) as u64;
    for i in 0..10u64 {
        let regs = Mem::new(8192);
        let p = regs.pa();
        let mut h = Hc::at(p, "test", 0);
        h.probed = true;
        h.op = p.add(0x40);
        h.rt = p.add(0x200);
        h.db = p.add(0x1000);
        h.max_slots = 16;
        h.max_ports = 1 + (i % 2) as u8;
        h.ctx64 = true;
        h.ac64 = true;
        dev::write32(h.op.add(0x08), 1); // PAGESIZE: 4KB
        let start = base + i * span;
        dev::write8(Pa(start + span - 1), 0xab);
        block(h.start(Pa(start), span, &Clock)).unwrap_or_else(|e| panic!("host{i}: {e}"));
        assert_eq!(h.n_slots, usize::from(h.max_ports), "host{i}");
        assert!(h.res[h.n_slots].is_some() && h.res[h.n_slots + 1].is_none());
        assert!(
            h.arena.0.next() <= start + span,
            "host{i} crossed DMA slice"
        );
        assert_eq!(
            dev::read8(Pa(start + span - 1)),
            0xab,
            "host{i} crossed DMA slice"
        );
        // Wat er overbleef, werd bouncebuffer: 64KB-uitgelijnd, binnen het
        // stuk, en tussen de grenzen.
        if h.max_transfer() > 0 {
            assert!(h.bulk_buf.is_aligned(TRB_MAX as u64));
            assert!(h.bulk_size >= BULK_BUF_MIN && h.bulk_size <= BULK_BUF_MAX);
            assert!(h.bulk_buf.0 + h.bulk_size <= start + span);
        }
        assert!(h.running);
    }
}

// --- ownership_test.go ------------------------------------------------------

/// Een controller met twee slots waarvan slot 1 bezet is, en een DCBAA in
/// nep-geheugen met een device context op [1].
fn releasing_hc(dcbaa: &Mem) -> Hc {
    let mut h = ownership_hc(2);
    h.res[1].as_mut().unwrap().in_use = true;
    h.dcbaa = dcbaa.pa();
    dev::write64(dcbaa.pa().add(8), 0xfeed000);
    h
}

// Go: TestReleaseSlotClearsOwnershipOnlyAfterConfirmedDisable
#[test]
fn release_slot_clears_ownership_only_after_confirmed_disable() {
    let dcbaa = Mem::new(3 * 8);
    let mut h = releasing_hc(&dcbaa);
    assert_eq!(h.release_begin(1), Ok(true), "Disable Slot moet naar 1");
    assert_eq!(h.release_end(1, Ok(())), Ok(()));
    let s = h.res[1].as_ref().unwrap();
    assert!(!s.in_use && !s.quarantined);
    assert_eq!(dev::read64(dcbaa.pa().add(8)), 0, "DCBAA[1] niet gewist");
    assert_eq!(h.poisoned, None, "controller ten onrechte poisoned");
    // Idempotent: een vrij slot vraagt geen Disable Slot meer.
    assert_eq!(h.release_begin(1), Ok(false));
}

// Go: TestReleaseSlotFailureQuarantinesWithoutClearingState
#[test]
fn release_slot_failure_quarantines_without_clearing_state() {
    let dcbaa = Mem::new(3 * 8);
    let mut h = releasing_hc(&dcbaa);
    let timeout = Err(Error::EventTimeout {
        what: "disable slot",
        usbsts: 0,
    });
    assert_eq!(
        h.release_end(1, timeout),
        Err(Error::Poisoned(Poison::DisableUnconfirmed { slot: 1 })),
        "release-fout zonder expliciete reset-eis"
    );
    let s = h.res[1].as_ref().unwrap();
    assert!(s.in_use && s.quarantined, "onbevestigd slot werd vergeten");
    assert_eq!(
        dev::read64(dcbaa.pa().add(8)),
        0xfeed000,
        "DCBAA[1] gewist zonder disable-bevestiging"
    );
    assert!(h.poisoned.is_some());
    // Een slot dat software niet kent, quarantaint ook.
    let mut h = ownership_hc(2);
    assert_eq!(
        h.release_begin(3),
        Err(Error::Poisoned(Poison::UnknownRelease { slot: 3 }))
    );
}

// Go: TestOutOfRangeEnabledSlotIsDisabledOrControllerPoisoned
#[test]
fn out_of_range_enabled_slot_is_disabled_or_controller_poisoned() {
    // Bevestigde cleanup: precies het gemelde slot gaat naar Disable Slot.
    let mut h = ownership_hc(2);
    assert_eq!(h.claim_enabled_slot(3), Ok(Claim::Stray(3)));
    assert_eq!(
        h.stray_released(3, Ok(())),
        Error::SlotOutOfRange {
            slot: 3,
            n_slots: 2
        }
    );
    assert_eq!(
        h.poisoned, None,
        "bevestigd opgeruimd slot poisonde de controller"
    );

    // Cleanup faalt.
    let mut h = ownership_hc(2);
    assert_eq!(h.claim_enabled_slot(3), Ok(Claim::Stray(3)));
    let e = h.stray_released(3, Err(Error::NotRunning));
    assert!(matches!(e, Error::Poisoned(_)) && h.poisoned.is_some());

    // Slot 0 kan niet gedisabled worden: geen Stray, dus geen Disable Slot.
    let mut h = ownership_hc(2);
    assert_eq!(
        h.claim_enabled_slot(0),
        Err(Error::Poisoned(Poison::SlotZero))
    );

    // Een slot in bereik dat al bezet is.
    let mut h = ownership_hc(2);
    assert_eq!(h.claim_enabled_slot(1), Ok(Claim::Owned));
    assert!(h.res[1].as_ref().unwrap().in_use);
    assert_eq!(
        h.claim_enabled_slot(1),
        Err(Error::Poisoned(Poison::SlotBusy { slot: 1 }))
    );
}

// Go: TestPoisonedControllerRefusesNewAttachBeforeMMIO
#[test]
fn poisoned_controller_refuses_new_attach_before_mmio() {
    let mut h = ownership_hc(2);
    h.running = true;
    h.poisoned = Some(Poison::DisableUnconfirmed { slot: 1 });
    assert_eq!(
        block(h.attach(1, &Clock)),
        Err(Error::Poisoned(Poison::DisableUnconfirmed { slot: 1 }))
    );
}

// --- recovery_test.go -------------------------------------------------------

fn poisoned_hc() -> Hc {
    let mut h = Hc::at(Pa(0), "test", 0);
    h.dma_base = Pa(0x12_0000);
    h.dma_size = 0x20_0000;
    h.poisoned = Some(Poison::DisableUnconfirmed { slot: 1 });
    h
}

// Go: TestRecoverRebuildsRetainedDMAWindow
#[test]
fn recover_rebuilds_retained_dma_window() {
    let mut h = poisoned_hc();
    assert_eq!(h.recover_begin(), Ok(Some((Pa(0x12_0000), 0x20_0000))));
    assert_eq!(h.recover_reset(Ok(())), Ok(()));
    assert_eq!(h.recover_start(Ok(())), Ok(()));
    assert_eq!(h.recovery_needed(), None);
}

// Go: TestRecoverStartFailureStaysPoisonedAndCanRetry
#[test]
fn recover_start_failure_stays_poisoned_and_can_retry() {
    let mut h = poisoned_hc();
    h.running = true;
    let fail = Error::DmaFull { want: 1, left: 0 };
    assert_eq!(h.recover_reset(Ok(())), Ok(()));
    assert_eq!(h.recover_start(Err(fail)), Err(fail));
    assert_eq!(h.recovery_needed(), Some(Poison::RecoveryStart));
    assert!(!h.running, "Start-fout liet running staan");

    // Opnieuw: de poison van de mislukte start vraagt weer een herstel in
    // hetzelfde venster, en dat lukt nu.
    assert_eq!(h.recover_begin(), Ok(Some((Pa(0x12_0000), 0x20_0000))));
    assert_eq!(h.recover_reset(Ok(())), Ok(()));
    assert_eq!(h.recover_start(Ok(())), Ok(()));
    assert_eq!(h.recovery_needed(), None);
}

// Go: TestRecoverResetFailureDoesNotStart
#[test]
fn recover_reset_failure_does_not_start() {
    let mut h = poisoned_hc();
    let want = Error::RegTimeout {
        what: "HCRST clear",
        value: 2,
        mask: 2,
        want: 0,
    };
    assert_eq!(h.recover_reset(Err(want)), Err(want));
    assert_eq!(h.recovery_needed(), Some(Poison::RecoveryReset));
}

// Go: TestRecoverHealthyControllerDoesNothing
#[test]
fn recover_healthy_controller_does_nothing() {
    let mut h = poisoned_hc();
    h.poisoned = None;
    assert_eq!(
        h.recover_begin(),
        Ok(None),
        "gezonde controller werd gereset"
    );
    // Zonder bewaard venster valt er niets te herbouwen.
    let mut h = poisoned_hc();
    h.dma_size = 0;
    assert!(matches!(h.recover_begin(), Err(Error::Dma { .. })));
}

// --- Rust-eigen toetsen -------------------------------------------------------

/// Een configuratiedescriptor van een Logi Bolt-achtige combo: interface 0
/// toetsenbord, interface 1 muis, en een derde (vendor) interface ertussen
/// waarvan de endpoint niet bij de muis mag belanden.
#[test]
fn parse_config_binds_one_endpoint_per_role() {
    #[rustfmt::skip]
    let cfg: &[u8] = &[
        9, 2, 59, 0, 3, 1, 0, 0xA0, 50,
        9, 4, 0, 0, 1, 3, 1, 1, 0,          // iface 0: boot keyboard
        7, 5, 0x81, 3, 8, 0, 8,             // EP1 IN interrupt, 8 bytes
        9, 4, 2, 0, 1, 0xFF, 0, 0, 0,       // iface 2: vendor
        7, 5, 0x83, 3, 64, 0, 1,            // EP3 IN: hoort nergens bij
        9, 4, 1, 0, 1, 3, 1, 2, 0,          // iface 1: boot mouse
        7, 5, 0x82, 3, 4, 0, 10,            // EP2 IN interrupt, 4 bytes
    ];
    let p = parse_config(cfg);
    assert_eq!(p.conf_val, 1);
    assert!(p.bulk.is_none());
    let got = format!("{p:?}");
    assert!(
        got.contains("proto: 1") && got.contains("proto: 2"),
        "{got}"
    );
    assert!(got.contains("dci: 3") && got.contains("dci: 5"), "{got}");
    assert!(
        !got.contains("dci: 7"),
        "vendor-endpoint belandde bij een rol: {got}"
    );
}

#[test]
fn parse_config_finds_bulk_only_storage() {
    #[rustfmt::skip]
    let cfg: &[u8] = &[
        9, 2, 32, 0, 1, 1, 0, 0x80, 50,
        9, 4, 0, 0, 2, 8, 6, 0x50, 0,       // mass storage, SCSI, bulk-only
        7, 5, 0x81, 2, 0, 2, 0,             // EP1 IN bulk, 512
        7, 5, 0x02, 2, 0, 2, 0,             // EP2 OUT bulk, 512
    ];
    let p = parse_config(cfg);
    let b = p.bulk.expect("bulk-interface");
    let got = format!("{b:?}");
    assert!(
        got.contains("in_dci: 3") && got.contains("out_dci: 4"),
        "{got}"
    );
    // Afgekapte of lege ketens zijn geen paniek.
    assert!(parse_config(&cfg[..20]).bulk.is_none());
    assert!(parse_config(&[]).bulk.is_none());
    assert!(parse_config(&[0, 0, 0]).bulk.is_none());
}

#[test]
fn interval_exponent_follows_linux() {
    assert_eq!(interval_exponent(Speed::LOW, 10), 6); // 80 microframes: 2^6
    assert_eq!(interval_exponent(Speed::FULL, 1), 3);
    assert_eq!(interval_exponent(Speed::FULL, 255), 10);
    assert_eq!(interval_exponent(Speed::HIGH, 4), 3);
    assert_eq!(interval_exponent(Speed::HIGH, 0), 0);
    assert_eq!(interval_exponent(Speed::SUPER, 200), 15);
}

/// De producer-ring over een omloop: de link-TRB krijgt de oude cycle, de
/// verwachting klapt om, en elk TRB draagt de cycle van zijn ronde.
#[test]
fn ring_wraps_with_link_and_toggles_cycle() {
    let mem = Mem::new(4 * 16);
    let mut r = Ring::new(mem.pa(), 0x1000, 64);
    assert_eq!(r.deq_ptr(), 0x1000 | 1);
    for i in 0..3u64 {
        assert_eq!(r.push(0, 0, 0, 0), 0x1000 + i * 16);
    }
    // Omgeslagen: de link staat met cycle 1 en toggle, en we schrijven nu 0.
    let link = dev::read32(mem.pa().add(3 * 16 + 12));
    assert_eq!(link & 1, 1);
    assert_eq!(link >> ring::TRB_TYPE_SHIFT & 0x3F, ring::TRB_LINK);
    assert_eq!(r.deq_ptr(), 0x1000);
    r.push(0, 0, 0, 0);
    assert_eq!(dev::read32(mem.pa().add(12)) & 1, 0);
}

/// De event-ring leest alleen plekken met de verwachte cycle.
#[test]
fn event_ring_respects_cycle() {
    let mem = Mem::new(2 * 16);
    let mut e = EvRing::new(mem.pa(), 0x2000, 2);
    assert!(e.poll().is_none());
    dev::write32(mem.pa(), 0x1234_5670);
    dev::write32(mem.pa().add(8), 13 << 24 | 5);
    dev::write32(
        mem.pa().add(12),
        3 << 24 | ring::TRB_TRANSFER_EVT << ring::TRB_TYPE_SHIFT | 1,
    );
    let ev = e.poll().unwrap();
    assert_eq!(
        (ev.kind, ev.ptr, ev.comp, ev.rem, ev.slot),
        (ring::TRB_TRANSFER_EVT, 0x1234_5670, 13, 5, 3)
    );
    assert!(e.poll().is_none());
    assert_eq!(e.deq_bus(), 0x2010);
}

/// Port Status Change-events komen niet in de wachtrij: niemand haalt ze
/// op, dus honderd poortresets mogen hem niet vullen en geen transfer eruit
/// duwen.
#[test]
fn port_status_events_skip_the_queue() {
    const PSC: u32 = 34; // xHCI tabel 6-91
    let mem = Mem::new(128 * 16);
    let mut h = Hc::at(Pa(0), "test", 0);
    h.evt = Some(EvRing::new(mem.pa(), 0x2000, 128));
    for i in 0..100u64 {
        let a = mem.pa().add(i * 16);
        dev::write32(a, (i as u32 % 4 + 1) << 24);
        dev::write32(a.add(12), PSC << ring::TRB_TYPE_SHIFT | 1);
    }
    let a = mem.pa().add(100 * 16);
    dev::write32(a, 0x4000);
    dev::write32(a.add(8), ring::CC_SUCCESS << 24);
    dev::write32(
        a.add(12),
        1 << 24 | ring::TRB_TRANSFER_EVT << ring::TRB_TYPE_SHIFT | 1,
    );
    h.pump();
    assert_eq!(h.pending.len(), 1);
    let ev = h.take(|e| e.kind == ring::TRB_TRANSFER_EVT).unwrap();
    assert_eq!((ev.ptr, ev.slot), (0x4000, 1));
}

/// Een handvat van een vorig leven wordt geweigerd, ook als het slot weer
/// bezet is.
#[test]
fn stale_device_handle_is_detached() {
    let h = ownership_hc(1);
    let d = Device {
        slot: 1,
        port: 1,
        speed: Speed::LOW,
        vendor_id: 0,
        product_id: 0,
        generation: 7,
        protos: [PROTO_KEYBOARD, PROTO_NONE],
        n_protos: 1,
        boot_refused: 0,
        mass_storage: false,
    };
    assert_eq!(h.live(&d), Err(Error::Detached));
    assert_eq!(h.device_err(&d), Some(Error::Detached));
    assert_eq!(
        format!("{d}"),
        "keyboard 0000:0000 on port 1 (low-speed, slot 1)"
    );
}

// --- de Pi 4 van 03-10 -------------------------------------------------------

/// De 64-bit registers zijn twee dwords: laag op +0, hoog op +4, en een
/// lezing is die twee dwords en niets anders (de VL805 gaf op één native
/// 64-bit lezing van CRCR 0x8_0000_0008: het lage woord twee keer).
#[test]
fn reg64_is_two_dwords_low_at_the_register() {
    let m = Mem::new(8);
    // SAFETY: `m` is 8 bytes, 8-uitgelijnd, en leeft tot het eind.
    let r: &Reg64 = unsafe { dev::regs(m.pa()) };
    r.write(0x0000_0010_14a3_f001);
    assert_eq!(dev::read32(m.pa()), 0x14a3_f001);
    assert_eq!(dev::read32(m.pa().add(4)), 0x10);
    // CRCR zoals de spec hem teruggeeft: pointer nul, alleen CRR.
    dev::write32(m.pa(), 0x8);
    dev::write32(m.pa().add(4), 0);
    assert_eq!(r.read(), 0x8);
}

/// Wat `start` in de registers en de ERST zet, met een verschoven bus (de
/// RP1 van de Pi 5): CRCR, DCBAAP, ERSTBA en ERDP dragen BUSadressen, laag
/// en hoog op hun eigen dword, RCS en EHB in het lage, en de ERST wijst
/// met de segmentmaat naar het segment van de event ring. Hetzelfde komt
/// terug uit `diagnostic`.
#[test]
fn start_programs_bus_addresses_in_both_dwords() {
    const OFF: u64 = 0x10_0000_0000;
    let memory = Mem::new(256 << 10);
    let regs = Mem::new(8192);
    let p = regs.pa();
    let mut h = Hc::at(p, "test", OFF);
    h.probed = true;
    h.op = p.add(0x40);
    h.rt = p.add(0x200);
    h.db = p.add(0x1000);
    h.max_slots = 32;
    h.max_ports = 1;
    h.ac64 = true;
    dev::write32(h.op.add(0x08), 1); // PAGESIZE: 4KB
    block(h.start(memory.pa(), 256 << 10, &Clock)).unwrap();

    let lo_hi = |pa: Pa| (dev::read32(pa), dev::read32(pa.add(4)));
    let bus = |pa: Pa| pa.0 + OFF;
    let cmd = h.cmd.unwrap();
    let evt = h.evt.unwrap();
    assert_eq!(cmd.bus, bus(cmd.base));
    assert_eq!(evt.bus, bus(evt.base));
    let split = |v: u64| (v as u32, (v >> 32) as u32);
    assert_eq!(lo_hi(h.op.add(0x18)), split(cmd.bus | 1), "CRCR met RCS");
    assert_eq!(lo_hi(h.op.add(0x30)), split(bus(h.dcbaa)), "DCBAAP");
    let ir = h.rt.add(RT_IR0);
    assert_eq!(dev::read32(ir.add(0x08)), 1, "ERSTSZ");
    assert_eq!(lo_hi(ir.add(0x10)), split(h.erst_bus), "ERSTBA");
    assert_eq!(lo_hi(ir.add(0x18)), split(evt.bus | ERDP_EHB), "ERDP");

    // De ERST zelf: het segment (busadres) en zijn maat in TRB's.
    let erst = Pa(h.erst_bus - OFF);
    assert_eq!(lo_hi(erst), split(evt.bus), "ERST-segment");
    assert_eq!(dev::read32(erst.add(8)), 4096 / 16, "ERST-segmentmaat");
    assert_eq!(dev::read32(erst.add(12)), 0);

    let d = h.diagnostic();
    assert_eq!(d.crcr, cmd.bus | 1);
    assert_eq!((d.cmd_bus, d.evt_bus), (cmd.bus, evt.bus));
    assert_eq!((d.erstba, d.erst_bus), (h.erst_bus, h.erst_bus));
    assert_eq!(d.erdp, evt.bus | ERDP_EHB);
    assert_eq!(d.trb0, [0; 4], "nog geen event");
}
