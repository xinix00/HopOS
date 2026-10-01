//! De mailbox, het opstartgesprek en de SART tegen nep-blokken in RAM; de
//! klok van de test speelt de coprocessor (zie `fake.rs`).

use super::*;
use crate::fake::{self, clock, typed, with};
use std::string::ToString;
use std::vec;
use std::vec::Vec;

/// Een bufferregio op 16 KB in RAM.
struct Pool {
    _mem: Vec<u64>,
    pa: Pa,
    size: u64,
}

fn pool(size: u64) -> Pool {
    let mut mem = vec![0u64; ((size + BUF_ALIGN) / 8) as usize];
    let pa = Pa((mem.as_mut_ptr() as usize as u64).next_multiple_of(BUF_ALIGN));
    Pool {
        _mem: mem,
        pa,
        size,
    }
}

fn rtkit(p: &Pool) -> Rtkit {
    let base = fake::install(None);
    // SAFETY: het ASC-blok en de regio liggen in RAM dat de test overleeft.
    unsafe { Rtkit::new(base, "test", p.pa, p.size, clock) }.unwrap()
}

#[test]
fn handshake_against_a_coprocessor() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    rt.boot(&mut ignore).unwrap();
    assert_eq!(rt.iop_power, POWER_ON);
    assert_eq!(rt.ap_power, POWER_ON);
    let cpu = with(|f| dev::read32(f.base.add(fake::CPU_CONTROL)));
    assert_ne!(cpu & CPU_START, 0, "the core runs");
    with(|f| {
        assert_eq!(f.agreed, 12, "the highest common version");
        // De systeem-endpoints uit de kaart gestart, 0x20 niet (dat doet de
        // driver erboven).
        let mut s = f.started.clone();
        s.sort_unstable();
        assert_eq!(s, [1, 2, 3, 4, 8]);
        // Drie buffers, 16 KB-gealigneerd uit de regio, met het gevraagde
        // aantal pagina's terug.
        assert_eq!(f.bufs.len(), 3);
        for &(_, a, _) in &f.bufs {
            assert!(a >= p.pa.0 && a < p.pa.0 + p.size && a % BUF_ALIGN == 0);
        }
        assert_eq!(f.bufs[0].2, 4);
        // De syslogregel is bevestigd met hetzelfde bericht.
        assert!(f.seen.contains(&(2, typed(5) | 0x77)));
    });
    assert_eq!(rt.crash_buf().size, BUF_ALIGN);
}

#[test]
fn sleep_quiesces_then_sleeps_then_stops_the_core() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    rt.boot(&mut ignore).unwrap();
    rt.sleep(&mut ignore).unwrap();
    assert_eq!(rt.ap_power, POWER_QUIESCED);
    assert_eq!(rt.iop_power, POWER_SLEEP);
    let cpu = with(|f| dev::read32(f.base.add(fake::CPU_CONTROL)));
    assert_eq!(cpu & CPU_START, 0);
    // De volgorde: eerst de AP-kant, dan de coprocessor.
    with(|f| {
        let tail: Vec<_> = f.seen.iter().rev().take(2).rev().copied().collect();
        assert_eq!(
            tail,
            [(0, typed(0xb) | 0x10), (0, typed(6) | 0x1)],
            "quiesce before sleep"
        );
    });
}

#[test]
fn version_mismatch_is_refused() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    with(|f| f.versions = (13, 14));
    assert_eq!(
        rt.boot(&mut ignore),
        Err(Error::Version {
            name: "test",
            min: 13,
            max: 14
        })
    );
}

#[test]
fn silent_coprocessor_times_out() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    with(|f| f.mute = true);
    assert_eq!(
        rt.boot(&mut ignore),
        Err(Error::Silent {
            name: "test",
            waiting: Wait::Hello
        })
    );
}

#[test]
fn full_mailbox_times_out() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    with(|f| f.full = true);
    let a2i = with(|f| f.base.add(fake::A2I_CONTROL));
    dev::write32(a2i, fake::FULL);
    assert_eq!(rt.send(EP_APP, 1), Err(Error::MailboxFull { name: "test" }));
}

#[test]
fn buffers_run_out_loudly() {
    // Eén buffer van 16 KB past (de crashlog), de tweede (de syslog: twee
    // pagina's, naar boven op 16 KB) niet.
    let p = pool(BUF_ALIGN);
    let mut rt = rtkit(&p);
    assert_eq!(
        rt.boot(&mut ignore),
        Err(Error::NoRoom {
            name: "test",
            ep: EP_SYSLOG,
            size: BUF_ALIGN
        })
    );
}

#[test]
fn unaligned_pool_is_refused() {
    let base = fake::install(None);
    // SAFETY: het blok ligt in RAM; de regio wordt geweigerd voor gebruik.
    let r = unsafe { Rtkit::new(base, "test", Pa(0x1000), 0x4000, clock) };
    assert!(matches!(r, Err(Error::Pool { base: 0x1000, .. })));
}

/// Go: `TestPollDeliversApplicationEndpoint`.
#[test]
fn poll_delivers_application_endpoint() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    with(|f| f.say(EP_APP, 0x1234));
    let mut got = Vec::new();
    rt.poll(&mut |ep, m| got.push((ep, m))).unwrap();
    assert_eq!(got, [(EP_APP, 0x1234)]);
    assert!(rt.has_heard(EP_APP));
    assert!(!rt.has_heard(EP_APP + 1));
}

#[test]
fn send_refuses_system_endpoints() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    assert_eq!(
        rt.send(EP_SYSLOG, 1),
        Err(Error::Endpoint {
            name: "test",
            ep: EP_SYSLOG
        })
    );
}

#[test]
fn second_crashlog_request_is_a_crash_with_its_text() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    rt.boot(&mut ignore).unwrap();
    let crash = rt.crash_buf().pa;
    // De coprocessor schrijft zijn melding en vraagt een tweede buffer.
    dev::write32(crash, MAGIC_CLHE);
    let e = crash.add(CRASH_HDR);
    dev::write32(e, MAGIC_CSTR);
    dev::write32(e.add(12), 64);
    dev::copy_in(e.add(20), b"NVME_PERM_ERR OP 60\0");
    dev::write32(e.add(64), 0x4b45_5953); // Een entry die geen tekst is.
    dev::write32(e.add(64 + 12), 16);
    with(|f| f.say(EP_CRASHLOG, typed(1) | (4 << 44)));
    assert_eq!(rt.poll(&mut ignore), Err(Error::Crashed { name: "test" }));
    assert_eq!(rt.crashlog().to_string(), "NVME_PERM_ERR OP 60");
}

/// Go: `TestCrashlogBounds`.
#[test]
fn crashlog_bounds() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    let mut buf = [0u64; 4];
    buf[0] = u64::from(MAGIC_CLHE);
    rt.bufs[usize::from(EP_CRASHLOG)] = Buf {
        pa: Pa(buf.as_mut_ptr() as usize as u64),
        size: 32,
    };
    assert_eq!(rt.crashlog().to_string(), "crashlog without a text entry");
    // Een adres van de firmware heeft geen grens van ons en wordt niet
    // gelezen.
    rt.bufs[usize::from(EP_CRASHLOG)] = Buf { pa: Pa(1), size: 0 };
    assert_eq!(rt.crashlog().to_string(), "no crashlog buffer");
}

#[test]
fn crashlog_entry_length_is_bounded_by_the_buffer() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    let mut buf = [0u64; 8];
    let pa = Pa(buf.as_mut_ptr() as usize as u64);
    dev::write32(pa, MAGIC_CLHE);
    dev::write32(pa.add(32), MAGIC_CSTR);
    dev::write32(pa.add(44), 4096); // Langer dan het buffer.
    rt.bufs[usize::from(EP_CRASHLOG)] = Buf { pa, size: 64 };
    assert_eq!(rt.crashlog().to_string(), "crashlog without a text entry");
}

#[test]
fn register_offsets() {
    assert_eq!(MMIO_LEN, 0x8840);
    assert_eq!(kind(typed(0xb) | 0x20), 0xb);
}

// ---------------------------------------------------------------------------
// SART.
// ---------------------------------------------------------------------------

fn sart_block() -> (Vec<u32>, sart::Sart) {
    let mut regs = vec![0u32; (sart::MMIO_LEN / 4) as usize];
    let base = Pa(regs.as_mut_ptr() as usize as u64);
    // SAFETY: het blok ligt in RAM dat de test overleeft.
    let s = unsafe { sart::Sart::new(base, sart::VERSION) }.unwrap();
    (regs, s)
}

#[test]
fn sart_window_lands_in_a_free_entry() {
    let (mut regs, mut s) = sart_block();
    // De firmware bezet 0 en 1.
    for i in 0..2 {
        regs[i] = 0xff;
        regs[16 + i] = 0x100 + i as u32;
        regs[32 + i] = 1;
    }
    let i = s.allow(Pa(0x8_0000_0000), 0x40_0000).unwrap();
    assert_eq!(i, 2);
    assert_eq!(regs[2], 0xff);
    assert_eq!(regs[16 + 2], 0x80_0000);
    assert_eq!(regs[32 + 2], 0x400);
    // De vensters van de firmware zijn niet aangeraakt.
    assert_eq!(regs[16], 0x100);
    // Dezelfde vraag nog eens (een tweede kern): hetzelfde venster, geen
    // nieuw.
    assert_eq!(s.allow(Pa(0x8_0000_0000), 0x40_0000), Ok(2));
    assert_eq!(regs[..16].iter().filter(|&&c| c != 0).count(), 3);
}

#[test]
fn sart_refuses_full_unaligned_and_other_versions() {
    let (mut regs, mut s) = sart_block();
    assert!(matches!(
        s.allow(Pa(0x1234), 0x1000),
        Err(sart::Error::Unaligned { .. })
    ));
    assert!(matches!(
        s.allow(Pa(0x1000), 0),
        Err(sart::Error::Unaligned { .. })
    ));
    assert!(matches!(
        s.allow(Pa(1 << 45), 0x1000),
        Err(sart::Error::TooHigh { .. })
    ));
    regs[..16].fill(0xff);
    assert!(matches!(
        s.allow(Pa(0x1000), 0x1000),
        Err(sart::Error::Full { .. })
    ));
    // SAFETY: het blok ligt in RAM; de versie wordt geweigerd voor gebruik.
    let r = unsafe { sart::Sart::new(Pa(regs.as_mut_ptr() as usize as u64), 2) };
    assert!(matches!(r, Err(sart::Error::Version(2))));
}

#[test]
fn boot_again_after_sleep_starts_clean() {
    let p = pool(0x10_0000);
    let mut rt = rtkit(&p);
    rt.boot(&mut ignore).unwrap();
    let first = rt.crash_buf().pa;
    rt.sleep(&mut ignore).unwrap();
    // Na een slaap (of een power-reset) vraagt hij opnieuw om buffers; de
    // crashlog-vraag is dan geen crash, en het geheugen is vers.
    rt.boot(&mut ignore).unwrap();
    assert_eq!(rt.iop_power, POWER_ON);
    assert_ne!(rt.crash_buf().pa, first);
}
