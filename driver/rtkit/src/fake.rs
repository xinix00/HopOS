//! Een nep-RTKit-coprocessor in RAM, gedeeld door de tests van
//! `driver-rtkit` en `driver-smc` (die laatste haalt dit bestand binnen met
//! `#[path]`).
//!
//! Het ASC-blok is een buffer in RAM en de klok van de test speelt de
//! coprocessor: bij elke blik op de klok (1) is het bericht dat hij de vorige
//! keer postte gelezen, (2) ziet hij wat de driver in A2I zette en reageert
//! erop, (3) post hij het volgende bericht uit zijn rij. RAM ziet geen
//! lezingen en een FIFO heeft RAM niet; het werkt omdat de driver de klok
//! alleen vlak vóór een blik in de inbox leest en na elk eigen bericht de
//! inbox leegtrekt (zie de crate-doc). De getallen hier komen uit m1n1, niet
//! uit de driver: de nep-chip is de datasheet.

// Gedeeld bestand: welke velden en functies gebruikt worden, verschilt per
// crate die het binnenhaalt, dus een `expect` zou in één van de twee falen.
#![allow(dead_code)]

use dev::Pa;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::vec;
use std::vec::Vec;

pub(crate) const CPU_CONTROL: u64 = 0x44;
const MBOX: u64 = 0x8000;
pub(crate) const A2I_CONTROL: u64 = MBOX + 0x110;
const I2A_CONTROL: u64 = MBOX + 0x114;
const A2I_SEND0: u64 = MBOX + 0x800;
const A2I_SEND1: u64 = MBOX + 0x808;
const I2A_RECV0: u64 = MBOX + 0x830;
const I2A_RECV1: u64 = MBOX + 0x838;
pub(crate) const FULL: u32 = 1 << 16;
const EMPTY: u32 = 1 << 17;
/// Wat in SEND1 staat als de driver niets nieuws zette.
const NOTHING: u64 = u64::MAX;
const LEN: u64 = MBOX + 0x840;

pub(crate) const fn typed(t: u64) -> u64 {
    t << 52
}

pub(crate) const fn kind(m: u64) -> u64 {
    (m >> 52) & 0xff
}

/// Wat een applicatie-endpoint doet met een bericht van de driver.
pub(crate) type AppHook = fn(&mut Fake, u8, u64);

pub(crate) struct Fake {
    _regs: Vec<u64>,
    pub(crate) base: Pa,
    pub(crate) now: u64,
    /// Wat de coprocessor nog wil zeggen: (endpoint, bericht).
    pub(crate) out: VecDeque<(u8, u64)>,
    posted: bool,
    /// Wat de driver zei, in volgorde.
    pub(crate) seen: Vec<(u8, u64)>,
    /// De buffers die hij kreeg: (endpoint, adres, pagina's).
    pub(crate) bufs: Vec<(u8, u64, u64)>,
    /// De endpoints die gestart werden.
    pub(crate) started: Vec<u8>,
    /// De versies in HELLO, en wat de driver koos.
    pub(crate) versions: (u64, u64),
    pub(crate) agreed: u64,
    /// Zegt niets meer.
    pub(crate) mute: bool,
    /// Houdt de uitgaande mailbox vol.
    pub(crate) full: bool,
    pub(crate) iop: u64,
    pub(crate) ap: u64,
    pub(crate) app: Option<AppHook>,
    /// Vrij voor de haak (de SMC: zijn gedeelde geheugen).
    pub(crate) shmem: u64,
    pub(crate) keys: Vec<(u32, Vec<u8>)>,
}

thread_local! {
    static FAKE: RefCell<Option<Fake>> = const { RefCell::new(None) };
}

/// Zet een verse coprocessor neer en geeft zijn ASC-basis.
pub(crate) fn install(app: Option<AppHook>) -> Pa {
    let mut regs = vec![0u64; LEN as usize / 8];
    let base = Pa(regs.as_mut_ptr() as usize as u64);
    dev::write32(base.add(I2A_CONTROL), EMPTY);
    dev::write64(base.add(A2I_SEND1), NOTHING);
    FAKE.with(|f| {
        *f.borrow_mut() = Some(Fake {
            _regs: regs,
            base,
            now: 0,
            out: VecDeque::new(),
            posted: false,
            seen: Vec::new(),
            bufs: Vec::new(),
            started: Vec::new(),
            versions: (11, 12),
            agreed: 0,
            mute: false,
            full: false,
            iop: 0,
            ap: 0,
            app,
            shmem: 0,
            keys: Vec::new(),
        });
    });
    base
}

/// Doet iets met de coprocessor van deze test.
pub(crate) fn with<R>(f: impl FnOnce(&mut Fake) -> R) -> R {
    FAKE.with(|c| f(c.borrow_mut().as_mut().expect("fake installed")))
}

/// De klok van de test, en de hartslag van de coprocessor.
pub(crate) fn clock() -> u64 {
    with(|f| {
        f.now += 1_000;
        f.tick();
        f.now
    })
}

impl Fake {
    fn tick(&mut self) {
        let b = self.base;
        dev::write32(b.add(A2I_CONTROL), if self.full { FULL } else { 0 });
        if self.mute {
            return;
        }
        if self.posted {
            dev::write32(b.add(I2A_CONTROL), EMPTY);
            self.posted = false;
        }
        let ep = dev::read64(b.add(A2I_SEND1));
        if ep != NOTHING {
            let msg = dev::read64(b.add(A2I_SEND0));
            dev::write64(b.add(A2I_SEND1), NOTHING);
            self.seen.push((ep as u8, msg));
            self.react(ep as u8, msg);
        }
        if let Some((ep, msg)) = self.out.pop_front() {
            dev::write64(b.add(I2A_RECV0), msg);
            dev::write64(b.add(I2A_RECV1), u64::from(ep));
            dev::write32(b.add(I2A_CONTROL), 0);
            self.posted = true;
        }
    }

    pub(crate) fn say(&mut self, ep: u8, msg: u64) {
        self.out.push_back((ep, msg));
    }

    fn react(&mut self, ep: u8, msg: u64) {
        match ep {
            0 => self.mgmt(msg),
            0x20.. => {
                if let Some(h) = self.app {
                    h(self, ep, msg);
                }
            }
            _ if kind(msg) == 1 => {
                self.bufs
                    .push((ep, msg & ((1 << 42) - 1), (msg >> 44) & 0xff));
            }
            _ => {}
        }
    }

    fn mgmt(&mut self, msg: u64) {
        match kind(msg) {
            // IOP_PWR_STATE: het wekbericht geeft HELLO, de rest een ack.
            6 => {
                let s = msg & 0xffff;
                if s == 0x220 {
                    let (min, max) = self.versions;
                    self.say(0, typed(1) | (max << 16) | min);
                } else {
                    self.iop = s;
                    self.say(0, typed(7) | s);
                }
            }
            // HELLO_ACK: de endpointkaart in twee stukken; het eerste met
            // crashlog, syslog, debug, ioreport en oslog.
            2 => {
                self.agreed = msg & 0xffff;
                let bits = (1 << 1) | (1 << 2) | (1 << 3) | (1 << 4) | (1 << 8);
                self.say(0, typed(8) | bits);
            }
            // EPMAP-bevestiging: na het eerste stuk het laatste (0x20 op
            // basis 1), na het laatste de opstart.
            8 if msg & (1 << 51) == 0 => self.say(0, typed(8) | (1 << 32) | 1 | (1 << 51)),
            8 => {
                self.say(1, typed(1) | (4 << 44));
                self.say(2, typed(1) | (2 << 44));
                self.say(4, typed(1) | (1 << 44));
                self.say(2, typed(8));
                self.say(2, typed(5) | 0x77);
                self.say(0, typed(7) | 0x20);
            }
            5 => self.started.push(((msg >> 32) & 0xff) as u8),
            // AP_PWR_STATE: bevestigd met hetzelfde type.
            0xb => {
                self.ap = msg & 0xffff;
                self.say(0, typed(0xb) | self.ap);
            }
            _ => {}
        }
    }
}
