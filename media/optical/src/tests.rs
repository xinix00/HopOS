//! De BOT-toetsen uit `optical_test.go` op de async [`Bot`], tegen een drive
//! van een paar dozijn regels: hij spreekt bulk-only transport en antwoordt
//! op INQUIRY, REQUEST SENSE en een commando met data naar de drive.

use super::asynchronous::{Bot, Transport};
use super::*;
use core::pin::pin;
use core::task::{Context, Poll, Waker};
use std::collections::VecDeque;

const OP_INQUIRY: u8 = 0x12;
const OP_SET_STREAMING: u8 = 0xb6;
const PERIPHERAL_OPTICAL: u8 = 0x05;

#[derive(Default)]
struct Fake {
    max: usize,
    tag: u32,
    /// Wat de drive op de IN-pijp klaarzet (data, dan de CSW).
    rx: VecDeque<Vec<u8>>,
    /// Wat er naar de drive ging na de CBW (data-out).
    got: Vec<u8>,
    /// Wacht de drive op data-out van deze lengte?
    want_out: usize,
    resets: u32,
    /// Het volgende commando faalt met deze sense.
    fail_next: Option<Sense>,
    sense: Sense,
    /// Het residu dat de CSW meldt (anders nul).
    residue: Option<u32>,
    /// De data-out faalt op de USB-kant.
    data_out_fails: bool,
    /// De CSW draagt een andere tag.
    wrong_tag: bool,
    /// De CSW meldt phase error.
    phase: bool,
    /// De eerste lees van de CSW staalt één keer.
    stall_status: bool,
    /// De CBW zelf komt niet weg.
    cbw_fails: bool,
    /// Het aantal CBW's dat binnenkwam.
    cbws: u32,
}

impl Fake {
    fn new() -> Fake {
        Fake {
            max: 64 << 10,
            ..Fake::default()
        }
    }

    fn csw(&self, tag: u32, status: u8) -> Vec<u8> {
        let mut c = Vec::new();
        c.extend_from_slice(&0x5342_5355u32.to_le_bytes());
        let tag = if self.wrong_tag { tag + 1 } else { tag };
        c.extend_from_slice(&tag.to_le_bytes());
        c.extend_from_slice(&self.residue.unwrap_or(0).to_le_bytes());
        c.push(if self.phase { 2 } else { status });
        c
    }

    fn command(&mut self, cbw: &[u8]) {
        assert_eq!(cbw.len(), CBW_LEN);
        self.cbws += 1;
        let tag = u32::from_le_bytes(cbw[4..8].try_into().unwrap());
        let len = u32::from_le_bytes(cbw[8..12].try_into().unwrap()) as usize;
        let op = cbw[15];
        let failed = self.fail_next.take();
        if let Some(s) = failed {
            self.sense = s;
        }
        let status = u8::from(failed.is_some());
        match op {
            OP_INQUIRY => {
                let mut d = vec![0u8; len];
                d[0] = PERIPHERAL_OPTICAL;
                self.rx.push_back(d);
            }
            OP_REQUEST_SENSE => {
                let mut d = vec![0u8; 18];
                d[0] = 0x70;
                d[2] = self.sense.key;
                d[12] = self.sense.asc;
                d[13] = self.sense.ascq;
                self.rx.push_back(d);
            }
            _ if cbw[12] & 0x80 == 0 => self.want_out = len,
            _ => {}
        }
        self.tag = tag;
        let csw = self.csw(tag, status);
        if self.stall_status {
            self.rx.push_back(Vec::new()); // de stall
        }
        self.rx.push_back(csw);
    }
}

impl Transport for &mut Fake {
    async fn out(&mut self, data: &[u8]) -> core::result::Result<(), UsbError> {
        if self.want_out > 0 {
            self.want_out = 0;
            if self.data_out_fails {
                return Err(UsbError(4));
            }
            self.got = data.to_vec();
            return Ok(());
        }
        if self.cbw_fails {
            return Err(UsbError(5));
        }
        self.command(data);
        Ok(())
    }

    async fn input(&mut self, buf: &mut [u8]) -> core::result::Result<usize, UsbError> {
        let d = self.rx.pop_front().ok_or(UsbError(6))?;
        if d.is_empty() {
            self.stall_status = false;
            return Err(UsbError(7)); // gestald; de transport maakt vrij
        }
        let n = d.len().min(buf.len());
        buf[..n].copy_from_slice(&d[..n]);
        Ok(n)
    }

    async fn reset_recovery(&mut self) -> core::result::Result<(), UsbError> {
        self.resets += 1;
        self.rx.clear();
        self.want_out = 0;
        Ok(())
    }

    fn max_transfer(&self) -> usize {
        self.max
    }
}

/// Draait één commando; de nep-drive wacht nooit, dus één poll volstaat
/// (zoals `block_on` in `kern/src/testutil.rs`).
fn exec(b: &mut Bot<&mut Fake>, cdb: &[u8], data: Data<'_>) -> Result<usize> {
    let mut f = pin!(b.execute(cdb, data));
    match f.as_mut().poll(&mut Context::from_waker(Waker::noop())) {
        Poll::Ready(r) => r,
        Poll::Pending => panic!("the fake drive never waits"),
    }
}

fn inquiry(b: &mut Bot<&mut Fake>) -> Result<usize> {
    let mut buf = [0u8; 36];
    let n = exec(b, &[OP_INQUIRY, 0, 0, 0, 36, 0], Data::In(&mut buf))?;
    assert_eq!(buf[0], PERIPHERAL_OPTICAL);
    Ok(n)
}

#[test]
fn build_cbw_is_the_wrapper_the_spec_describes() {
    let b = build_cbw(7, 2048, true, &[0x28, 0, 0, 0, 0, 5, 0, 0, 1, 0]);
    assert_eq!(b.len(), CBW_LEN);
    assert_eq!(u32::from_le_bytes(b[0..4].try_into().unwrap()), 0x4342_5355);
    assert_eq!(u32::from_le_bytes(b[4..8].try_into().unwrap()), 7);
    assert_eq!(u32::from_le_bytes(b[8..12].try_into().unwrap()), 2048);
    assert_eq!(
        (b[12], b[13], b[14]),
        (0x80, 0, 10),
        "vlaggen, lun, cdb-lengte"
    );
    assert_eq!((b[15], b[20]), (0x28, 5), "het commandoblok in de wrapper");
}

#[test]
fn execute_bounds_and_directions() {
    let mut f = Fake::new();
    let max = f.max;
    let mut b = Bot::new(&mut f);
    let big = vec![0u8; max + 1];
    let mut bigin = vec![0u8; max + 1];
    assert_eq!(exec(&mut b, &[], Data::None), Err(Error::Invalid));
    assert_eq!(exec(&mut b, &[0; 17], Data::None), Err(Error::Invalid));
    assert!(matches!(
        exec(&mut b, &[OP_INQUIRY], Data::In(&mut bigin)),
        Err(Error::TooLarge { .. })
    ));
    assert!(matches!(
        exec(&mut b, &[OP_SET_STREAMING], Data::Out(&big)),
        Err(Error::TooLarge { .. })
    ));
    assert_eq!(b.transport().cbws, 0, "een ongeldig commando bereikte USB");
    assert_eq!(inquiry(&mut b), Ok(36));
    let payload = [1u8, 2, 3, 4];
    assert_eq!(
        exec(&mut b, &[OP_SET_STREAMING], Data::Out(&payload)),
        Ok(4)
    );
    assert_eq!(b.transport().got, payload);
    let s = Sense {
        key: 5,
        asc: 0x24,
        ascq: 0,
    };
    b.transport().fail_next = Some(s);
    assert_eq!(
        inquiry(&mut b),
        Err(Error::Check(s)),
        "de sense ging verloren"
    );
    assert_eq!(
        format!("{}", Error::Check(s)),
        "sense 5/24/00 (invalid field in command)"
    );
}

#[test]
fn execute_uses_the_drive_transfer_residue() {
    let mut f = Fake::new();
    f.residue = Some(2);
    let mut b = Bot::new(&mut f);
    assert_eq!(
        exec(&mut b, &[OP_SET_STREAMING], Data::Out(&[1, 2, 3, 4])),
        Ok(2)
    );
    b.transport().residue = Some(5);
    let r = exec(&mut b, &[OP_SET_STREAMING], Data::Out(&[1, 2, 3, 4]));
    assert_eq!(r, Err(Error::Status { at: 8, got: 5 }));
    assert_eq!(
        b.transport().resets,
        1,
        "een onmogelijk residu hoort een reset"
    );
}

#[test]
fn execute_does_not_hide_a_data_transport_failure() {
    let mut f = Fake::new();
    f.data_out_fails = true;
    let mut b = Bot::new(&mut f);
    let r = exec(&mut b, &[OP_SET_STREAMING], Data::Out(&[1, 2, 3, 4]));
    assert_eq!(r, Err(Error::Data(UsbError(4))));
    // De status is wel gelezen: het volgende commando loopt gewoon.
    b.transport().data_out_fails = false;
    assert_eq!(inquiry(&mut b), Ok(36));
    assert_eq!(b.transport().resets, 0);
}

#[test]
fn a_lost_track_resets_the_drive() {
    let mut f = Fake::new();
    f.wrong_tag = true;
    let mut b = Bot::new(&mut f);
    assert!(matches!(inquiry(&mut b), Err(Error::Status { at: 4, .. })));
    assert_eq!(b.transport().resets, 1);
    b.transport().wrong_tag = false;
    b.transport().phase = true;
    assert_eq!(inquiry(&mut b), Err(Error::Phase));
    assert_eq!(b.transport().resets, 2);
    b.transport().phase = false;
    b.transport().cbw_fails = true;
    assert_eq!(inquiry(&mut b), Err(Error::Command(UsbError(5))));
    assert_eq!(b.transport().resets, 3);
    b.transport().cbw_fails = false;
    assert_eq!(inquiry(&mut b), Ok(36), "na de reset praat de drive weer");
}

#[test]
fn a_stalled_status_gets_one_second_chance() {
    let mut f = Fake::new();
    f.stall_status = true;
    let mut b = Bot::new(&mut f);
    assert_eq!(exec(&mut b, &[OP_SET_STREAMING], Data::None), Ok(0));
    assert_eq!(b.transport().resets, 0);
}

#[test]
fn sense_reads_both_formats_and_names_the_common_ones() {
    let mut fixed = [0u8; 18];
    fixed[0] = 0x70;
    fixed[2] = 2;
    fixed[12] = 0x3a;
    let s = Sense::parse(&fixed).unwrap();
    assert_eq!((s.key, s.asc, s.what()), (2, 0x3a, "no medium"));
    let desc = [0x72, 2, 0x04, 0x01];
    assert_eq!(Sense::parse(&desc).unwrap().what(), "becoming ready");
    assert_eq!(Sense::parse(&[0x70, 0, 0]), None);
    assert_eq!(Sense::parse(&[0x10; 20]), None);
}
