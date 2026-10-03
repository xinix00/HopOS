//! De client tegen een nep-platform: de shmem in RAM, en de klok van de
//! test speelt de SCP. Staat het kanaal bezet en is de bel geluid, dan leest
//! het platform het bericht, schrijft een antwoord en geeft het kanaal vrij.

use super::*;
use std::cell::RefCell;
use std::vec;
use std::vec::Vec;

type Answer = fn(proto: u8, msg: u8, req: &[u32]) -> Vec<u32>;

#[derive(Default)]
struct Scp {
    shm: Pa,
    answer: Option<Answer>,
    /// Antwoord nooit.
    mute: bool,
    /// Zet de foutbit.
    error: bool,
    seen: Vec<(u8, u8, Vec<u32>, u32)>,
    now: u64,
}

thread_local! {
    static SCP: RefCell<Scp> = RefCell::new(Scp::default());
}

fn clock() -> u64 {
    SCP.with(|s| {
        let mut s = s.borrow_mut();
        s.now += 1_000_000;
        let base = s.shm;
        if base.0 == 0 || s.mute {
            return s.now;
        }
        let status = dev::read32(base.add(4));
        let bell = dev::read32(base.add(0x80));
        if status & STATUS_FREE == 0 && bell != 0 {
            dev::write32(base.add(0x80), 0);
            let hdr = dev::read32(base.add(0x18));
            let (proto, msg) = (((hdr >> 10) & 0xff) as u8, (hdr & 0xff) as u8);
            let n = (dev::read32(base.add(0x14)) as usize - 4) / 4;
            let req: Vec<u32> = (0..n)
                .map(|i| dev::read32(base.add(0x1c + 4 * i as u64)))
                .collect();
            let sign = dev::read32(base.add(0x0c));
            s.seen.push((proto, msg, req.clone(), sign));
            let resp = (s.answer.unwrap())(proto, msg, &req);
            for (i, w) in resp.iter().enumerate() {
                dev::write32(base.add(0x1c + 4 * i as u64), *w);
            }
            dev::write32(base.add(0x14), 4 + 4 * resp.len() as u32);
            let err = if s.error { STATUS_ERROR } else { 0 };
            dev::write32(base.add(4), status | STATUS_FREE | err);
        }
        s.now
    })
}

fn channel(answer: Answer) -> (Channel, Vec<u64>) {
    let mut mem = vec![0u64; SHMEM_LEN as usize / 8 + 1];
    let base = Pa(mem.as_mut_ptr() as usize as u64);
    dev::write32(base.add(4), STATUS_FREE);
    SCP.with(|s| {
        *s.borrow_mut() = Scp {
            shm: base,
            answer: Some(answer),
            ..Scp::default()
        };
    });
    // SAFETY: de shmem ligt in `mem`, dat de test overleeft.
    (unsafe { Channel::new(base, clock) }, mem)
}

fn seen() -> Vec<(u8, u8, Vec<u32>, u32)> {
    SCP.with(|s| s.borrow().seen.clone())
}

/// Twee Celsius-sensoren (exponent 0 en -3) en een spanning.
fn board_scp(proto: u8, msg: u8, req: &[u32]) -> Vec<u32> {
    let name = |s: &str| {
        let mut b = [0u8; 16];
        b[..s.len()].copy_from_slice(s.as_bytes());
        b.chunks(4)
            .map(|c| u32::from_le_bytes(c.try_into().unwrap()))
            .collect::<Vec<u32>>()
    };
    match (proto, msg) {
        (proto::SENSOR, MSG_VERSION) => vec![0, 0x0003_0000],
        (proto::SENSOR, SENSOR_DESCRIPTION_GET) => {
            let i = req[0];
            let (attr_hi, n) = match i {
                0 => (u32::from(CELSIUS), name("CPU_B0")),
                1 => ((0x1d << 11) | u32::from(CELSIUS), name("GPU")),
                _ => (5, name("VDD")),
            };
            let remaining = 2u32.saturating_sub(i);
            let mut v = vec![0, 1 | (remaining << 16), 10 + i, 0, attr_hi];
            v.extend(n);
            v
        }
        (proto::SENSOR, SENSOR_READING_GET) => match req[0] {
            10 => vec![0, 47, 0],
            11 => vec![0, 51_250, 0],
            _ => vec![(-4i32) as u32],
        },
        (proto::BASE, BASE_LIST_PROTOCOLS) => vec![0, 3, u32::from_le_bytes([0x11, 0x13, 0x15, 0])],
        (proto::POWER, POWER_STATE_GET) => vec![0], // te kort
        (proto::CLOCK, CLOCK_RATE_GET) => vec![0, 0x4b8e_0000, 1],
        _ => vec![0],
    }
}

#[test]
fn call_follows_the_aml_sequence() {
    let (mut c, _m) = channel(board_scp);
    assert_eq!(c.version(proto::SENSOR).unwrap(), 0x0003_0000);
    let s = seen();
    assert_eq!(s.len(), 1);
    assert_eq!((s[0].0, s[0].1), (proto::SENSOR, MSG_VERSION));
    assert_eq!(s[0].3, CIX_SIGNATURE, "the AML signature");
    assert_eq!(
        header(0x15, 0x06, 0x3ff),
        0x06 | (0x15 << 10) | (0x3ff << 18)
    );
}

#[test]
fn sensors_are_listed_and_read_in_milli_c() {
    let (mut c, _m) = channel(board_scp);
    let mut out = [Sensor::default(); 8];
    let n = c.sensors(&mut out).unwrap();
    assert_eq!(n, 3);
    assert_eq!(
        (out[0].name(), out[0].kind, out[0].exponent),
        ("CPU_B0", CELSIUS, 0)
    );
    assert_eq!((out[1].name(), out[1].exponent), ("GPU", -3));
    assert_eq!(out[2].kind, 5);
    let cpu = out[0].milli_c(c.reading(10).unwrap());
    let gpu = out[1].milli_c(c.reading(11).unwrap());
    assert_eq!((cpu, gpu), (47_000, 51_250));
    assert_eq!(
        c.reading(99),
        Err(Error::Status {
            proto: proto::SENSOR,
            msg: SENSOR_READING_GET,
            status: -4
        })
    );
}

#[test]
fn short_replies_are_errors_not_zeroes() {
    let (mut c, _m) = channel(board_scp);
    assert_eq!(c.power_state(3), Err(Error::Short { got: 1, want: 2 }));
    assert_eq!(c.clock_rate(1).unwrap(), 0x1_4b8e_0000);
    let mut p = [0u8; 8];
    assert_eq!(c.protocols(&mut p).unwrap(), 3);
    assert_eq!(&p[..3], &[0x11, 0x13, 0x15]);
    c.power_set(7, POWER_ON).unwrap();
    assert_eq!(seen().last().unwrap().2, vec![0, 7, POWER_ON]);
    // Synchroon (flags 0), laag woord eerst: de vorm van Linux.
    c.set_clock_rate(0, 0x1_6b49_d200).unwrap();
    assert_eq!(
        seen().last().unwrap().2,
        vec![0, 0, 0x6b49_d200, 1],
        "flags, id, rate"
    );
}

#[test]
fn a_silent_or_failing_platform_is_named() {
    let (mut c, _m) = channel(board_scp);
    SCP.with(|s| s.borrow_mut().mute = true);
    assert!(matches!(c.version(proto::BASE), Err(Error::Busy { .. })));
    // Het kanaal staat nu bezet: ook het volgende bericht wacht vergeefs, en
    // schrijft niets.
    assert!(matches!(c.version(proto::BASE), Err(Error::Busy { .. })));
    let (mut c, _m) = channel(board_scp);
    SCP.with(|s| s.borrow_mut().error = true);
    assert_eq!(
        c.version(proto::BASE),
        Err(Error::Channel {
            proto: proto::BASE,
            msg: MSG_VERSION
        })
    );
    assert_eq!(
        c.call(proto::BASE, 0, &[0; MAX_WORDS + 1]).err(),
        Some(Error::TooLarge(MAX_WORDS + 1))
    );
}

#[test]
fn the_tf_a_channel_rings_by_function_and_leaves_the_signature() {
    fn ring() {
        // De SMC: het platform antwoordt voor hij terugkeert.
        SCP.with(|s| {
            let b = s.borrow().shm;
            dev::write32(b.add(0x80), 1);
        });
        clock();
    }
    let (_c, _m) = channel(board_scp);
    let base = SCP.with(|s| s.borrow().shm);
    // SAFETY: de shmem ligt in `_m`.
    let mut c = unsafe { Channel::with_ring(base, clock, ring) };
    c.power_set(2, POWER_OFF).unwrap();
    assert_eq!(seen()[0].3, 0, "no Cix signature on the TF-A channel");
}

#[test]
fn milli_c_scales_by_the_exponent() {
    let s = |e| Sensor {
        exponent: e,
        ..Sensor::default()
    };
    assert_eq!(s(0).milli_c(45), 45_000);
    assert_eq!(s(-1).milli_c(455), 45_500);
    assert_eq!(s(-3).milli_c(45_123), 45_123);
    assert_eq!(s(-6).milli_c(45_123_456), 45_123);
}
