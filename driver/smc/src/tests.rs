//! De SMC tegen een nep-coprocessor in RAM (`driver/rtkit/src/fake.rs`)
//! met een SMC-persoonlijkheid op endpoint 0x20: een sleuteltabel, het
//! gedeelde geheugen in een eigen SRAM-buffer, en antwoorden met het id van
//! de vraag.

use super::*;
use fake::{Fake, clock, with};
use std::string::ToString;
use std::vec;
use std::vec::Vec;

/// De SMC zelf: INITIALIZE geeft het adres, READ_KEY de waarde (klein in
/// het antwoord, groot in het gedeelde geheugen), GET_KEY_BY_INDEX de
/// sleutel. Een onbekende sleutel is code 0x84 (m1n1: `SMC_ERR_KEYNOTFOUND`).
fn smc(f: &mut Fake, ep: u8, msg: u64) {
    let cmd = (msg & 0xff) as u8;
    let id = (msg >> 12) & 0xf;
    let arg = (msg >> 32) as u32;
    match cmd {
        CMD_INITIALIZE => {
            // Het eerste bericht is het adres; daarna een eigen melding.
            let sh = f.shmem;
            f.say(ep, sh);
            f.say(ep, u64::from(CMD_NOTIFICATION));
        }
        CMD_READ_KEY => {
            let Some((_, v)) = f.keys.iter().find(|(k, _)| *k == arg) else {
                f.say(ep, (id << 12) | 0x84);
                return;
            };
            let v = v.clone();
            let size = v.len() as u64;
            let mut r = (id << 12) | (size << 16);
            if v.len() <= 4 {
                let mut b = [0u8; 4];
                b[..v.len()].copy_from_slice(&v);
                r |= u64::from(u32::from_le_bytes(b)) << 32;
            } else {
                dev::copy_in(Pa(f.shmem), &v);
            }
            f.say(ep, r);
        }
        CMD_GET_KEY_BY_INDEX => match f.keys.get(arg as usize) {
            Some(&(k, _)) => f.say(ep, (id << 12) | (u64::from(k) << 32)),
            None => f.say(ep, (id << 12) | 0x84),
        },
        _ => {}
    }
}

struct Mem {
    _pool: Vec<u64>,
    _sram: Vec<u64>,
    sram: Pa,
}

fn machine(keys: &[(&str, Vec<u8>)]) -> (Mem, Rtkit) {
    let base = fake::install(Some(smc));
    let mut pool = vec![0u64; 0x2_0000 / 8];
    let pool_pa = Pa((pool.as_mut_ptr() as usize as u64).next_multiple_of(0x4000));
    let mut sram = vec![0u64; (2 * SHMEM_SIZE / 8) as usize];
    let sram_pa = Pa(sram.as_mut_ptr() as usize as u64);
    with(|f| {
        f.shmem = sram_pa.0 + 0x100;
        f.keys = keys.iter().map(|(k, v)| (key(k).0, v.clone())).collect();
    });
    // SAFETY: blok en regio liggen in RAM dat de test overleeft.
    let rt = unsafe { Rtkit::new(base, "smc", pool_pa, 0x1_0000, clock) }.unwrap();
    (
        Mem {
            _pool: pool,
            _sram: sram,
            sram: sram_pa,
        },
        rt,
    )
}

fn open(m: &Mem, rt: Rtkit) -> Result<Smc> {
    // SAFETY: de SRAM ligt in `m`, dat de test overleeft.
    unsafe { Smc::open(rt, m.sram, 2 * SHMEM_SIZE) }
}

fn be(v: u32) -> Vec<u8> {
    v.to_be_bytes().to_vec()
}

fn flt(c: f32) -> Vec<u8> {
    c.to_le_bytes().to_vec()
}

#[test]
fn key_encoding() {
    assert_eq!(key("TC0P"), Key(0x5443_3050));
    assert_eq!(key("#KEY"), Key(0x234b_4559));
    // Korter: alleen wat er is (de Go-vorm); langer: de eerste vier.
    assert_eq!(key("ab"), Key(0x6162));
    assert_eq!(key("TC0PX"), key("TC0P"));
    assert_eq!(key("TC0P").to_string(), "TC0P");
    assert_eq!(Key(0x5443_0001).to_string(), "TC??");
}

#[test]
fn open_reads_count_keys_and_floats() {
    let (m, rt) = machine(&[
        ("#KEY", be(4)),
        ("TC0P", flt(51.5)),
        ("F0Ac", flt(1200.0)),
        ("Tp01", flt(63.25)),
    ]);
    let mut s = open(&m, rt).unwrap();
    assert_eq!(s.st.shmem, Some(m.sram.add(0x100)));
    // De melding na het adres is geteld en niet als antwoord gezien.
    assert_eq!(s.count().unwrap(), 4);
    assert_eq!(s.notifications(), 1);
    assert_eq!(s.key_at(1).unwrap(), key("TC0P"));
    assert_eq!(s.float(key("TC0P")).unwrap(), 51.5);
    assert_eq!(
        s.float(key("TXXX")),
        Err(Error::Failed {
            cmd: CMD_READ_KEY,
            key: key("TXXX"),
            code: 0x84
        })
    );
    // Endpoint 0x20 is door de driver gestart, na het opstartgesprek.
    with(|f| assert!(f.started.contains(&ENDPOINT)));
}

#[test]
fn large_values_come_from_shared_memory() {
    let long: Vec<u8> = (1..=12).collect();
    let (m, rt) = machine(&[("RGEN", long.clone())]);
    let mut s = open(&m, rt).unwrap();
    let mut out = [0u8; 16];
    assert_eq!(s.read(key("RGEN"), &mut out).unwrap(), 12);
    assert_eq!(&out[..12], &long[..]);
    // Een buffer die te klein is, is een fout, geen afgekapte waarde.
    let mut small = [0u8; 8];
    assert_eq!(
        s.read(key("RGEN"), &mut small),
        Err(Error::Size {
            key: key("RGEN"),
            size: 12
        })
    );
    assert!(matches!(
        s.float(key("RGEN")),
        Err(Error::Size { .. } | Error::NotFloat { .. })
    ));
}

#[test]
fn sensors_and_hottest() {
    let (m, rt) = machine(&[
        ("#KEY", be(5)),
        ("TC0P", flt(51.5)),
        ("F0Ac", flt(1200.0)),
        ("Tp01", flt(63.25)),
        ("TB0T", vec![1, 2]),
    ]);
    let mut s = open(&m, rt).unwrap();
    let mut seen = Vec::new();
    // "#KEY" zelf staat op index 0 en begint niet met T; TB0T is geen float.
    assert_eq!(s.sensors(|x| seen.push(x.key)).unwrap(), 2);
    assert_eq!(seen, [key("TC0P"), key("Tp01")]);
    let hot = s.hottest().unwrap().unwrap();
    assert_eq!((hot.key, hot.celsius), (key("Tp01"), 63.25));
}

#[test]
fn shared_memory_outside_sram_is_refused_and_the_smc_sleeps() {
    let (m, rt) = machine(&[]);
    with(|f| f.shmem = 0x10);
    assert_eq!(open(&m, rt).err(), Some(Error::BadShmem { pa: 0x10 }));
    // Na de weigering is hij in slaap gepraat.
    with(|f| {
        assert_eq!(f.iop, 1);
        assert_eq!(f.ap, 0x10);
    });
}

#[test]
fn no_answer_is_sticky() {
    let (m, rt) = machine(&[("TC0P", flt(40.0))]);
    let mut s = open(&m, rt).unwrap();
    with(|f| f.mute = true);
    let e = Error::Timeout {
        cmd: CMD_READ_KEY,
        key: key("TC0P"),
    };
    assert_eq!(s.float(key("TC0P")), Err(e));
    let id = s.st.msgid;
    with(|f| f.mute = false);
    // Ook als hij weer praat: geen tweede opdracht op een onbevestigd id.
    assert_eq!(s.float(key("TC0P")), Err(e));
    assert_eq!(s.st.msgid, id);
}

/// Go: `TestUnconfirmedCommandCannotReuseID`.
#[test]
fn unconfirmed_command_cannot_reuse_id() {
    let (m, rt) = machine(&[("TC0P", flt(40.0))]);
    let mut s = open(&m, rt).unwrap();
    let want = Error::NoShmem;
    s.st.failed = Some(want);
    s.st.msgid = 16;
    let mut b = [0u8; 4];
    assert_eq!(s.read(key("TC0P"), &mut b), Err(want));
    assert_eq!(s.st.msgid, 16, "reused id after timeout");
    // Het nep-SMC-antwoord bewijst dat er niets verstuurd is.
    with(|f| {
        assert!(
            !f.seen
                .iter()
                .any(|&(ep, m)| ep == ENDPOINT && m & 0xff == 0x10)
        )
    });
}
