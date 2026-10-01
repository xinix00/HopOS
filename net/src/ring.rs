//! De naad naar de frame-ringen: wat de switch van een ring vraagt, als twee
//! kleine traits.
//!
//! De echte ring is `abi::ring` (SPSC over device-geheugen, monotone
//! indexen, `push`/`pull` in de ring-crate): [`AbiTx`] en
//! `abi::ring::Writer` dragen deze traits. De host-tests draaien over
//! buffers (de `mem`-module onder `cfg(test)`), met dezelfde contracten.
//!
//! SPSC-regels: de switch-actor is de enige consument van elke TX-ring en de
//! enige producer van elke RX-ring. Dat staat niet in een slot maar in het
//! eigendom: de actor bezit de helft van elke ring als waarde.

/// Recordtype: één rauw Ethernet-frame (Go: `ring.TypeFrame`).
pub const KIND_FRAME: u32 = abi::ring::Kind::FRAME.raw();

/// Recordtype op poort 0 alleen: een frame van of voor de uplink-NIC. De
/// host-ringen zijn kern-intern (geen ABI), dus dit type bestaat alleen
/// tussen de switch en HOP's eigen stack: zo reist extern verkeer van de
/// node-stack door dezelfde twee ringen als zijn interne LAN-verkeer, en
/// blijft de switch de enige die de uplink voedt.
pub const KIND_UPLINK: u32 = 64;

use abi::ring::{Coherence, Kind};
use core::fmt;
use dev::Pa;

/// De consumentkant van een ring (TX van een app, gezien vanuit de switch).
pub trait Reader {
    /// Haalt het volgende record op in `buf`: `(soort, lengte)`, of `None`
    /// als de ring leeg (of corrupt) is. Een record dat niet in `buf` past,
    /// maakt de ring corrupt: de inhoud komt van de producer en is
    /// onvertrouwd.
    fn read_into(&mut self, buf: &mut [u8]) -> Option<(u32, usize)>;

    /// Als [`read_into`](Self::read_into), zonder kopie: `f` leest het
    /// record in de ring zelf (hoogstens `max` bytes), daarna gaat de ruimte
    /// terug. Alleen voor een ring waarvan de lezer de producer vertrouwt:
    /// poort 0, waar beide kanten de kern zijn. Een app kan haar record
    /// tijdens `f` herschrijven, dus haar ringen gaan via `read_into`.
    fn read_in_place<T>(&mut self, max: usize, f: impl FnOnce(u32, &[u8]) -> T) -> Option<T>;

    /// Mapt de kern deze ring Normal ([`abi::ring::Coherence::Hardware`])?
    /// Alleen dan mag een record in de ring zelf met `memcpy` gelezen of
    /// gevuld worden; op Device (de pool van de Radxa, een geweigerde remap
    /// op Apple) abort een ongealigneerde toegang.
    fn is_normal(&self) -> bool;

    /// Waarom de ring corrupt verklaard is; `None` = gezond. Een corrupte
    /// TX-ring oogt van buiten identiek aan een lege (boot 9, 17-08:
    /// slot-net dood na ~30 s, app gezond, en niets op de console), dus de
    /// switch meldt het één keer per leven van de poort.
    fn corrupt(&self) -> Option<Self::Why>;

    /// De reden van een corrupt-verklaring, voor de logregel.
    type Why: fmt::Display;

    /// Het handvat waarmee [`probe`](Self::probe) deze ring zonder de actor
    /// kan lezen: voor een ABI-ring het fysieke adres van de kop.
    fn probe_handle(&self) -> u64;

    /// Liggen er ongelezen records? Alleen kale loads: dit draait in de
    /// idle-ronde van de executor tussen twee WFE's, en een CAS zet op de M4
    /// het event-register, waarna de volgende WFE meteen terugkeert (HOP
    /// spinde op 1,7M rondes/s, 04-09). Mag racen: een verouderd handvat
    /// geeft hooguit een overbodige bel.
    fn probe(handle: u64) -> bool;
}

/// De producerkant van een ring (RX van een app, gezien vanuit de switch).
pub trait Writer {
    /// Plaatst een record. `Some(notify)` bij succes, waarbij `notify` waar
    /// is als de ring vóór deze schrijf leeg was (de enige overgang die een
    /// wek waard is); `None` als hij vol is.
    fn write_notify(&mut self, kind: u32, p: &[u8]) -> Option<bool>;

    /// Als [`write_notify`](Self::write_notify), zonder kopie: `f` bouwt het
    /// record in de ring zelf (hoogstens `max` bytes) en geeft de soort en de
    /// lengte, of `None` voor geen record. Is er nu geen plaats voor `max`
    /// bytes, dan [`InPlace::Full`] zonder `f` te roepen.
    fn write_in_place(
        &mut self,
        max: usize,
        f: impl FnOnce(&mut [u8]) -> Option<(u32, usize)>,
    ) -> InPlace;

    /// Zie [`Reader::is_normal`].
    fn is_normal(&self) -> bool;

    /// Cleant de kop naar het geheugen voor een lezer zonder cache (de
    /// EL2-switcher peekt hem bij de rotatie). Eén keer per burst, niet per
    /// frame. Standaard niets.
    fn publish_head(&mut self) {}
}

/// De uitkomst van [`Writer::write_in_place`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InPlace {
    /// Een record geschreven; `true` als de ring daarvoor leeg was (de
    /// overgang die een wek waard is).
    Written(bool),
    /// `f` gaf niets: geen record.
    Nothing,
    /// Geen plaats voor `max` bytes; `f` is niet geroepen.
    Full,
}

/// De leeskant van een ABI-TX-ring, met zijn geometrie erbij voor de
/// probe: `abi::ring::Reader` houdt basis en maat privé, en de deur van de
/// executor moet de kop kunnen lezen zonder de actor.
#[derive(Debug)]
pub struct AbiTx {
    ring: abi::ring::Reader,
    base: Pa,
}

impl AbiTx {
    /// Opent de TX-ring op `base` met datacapaciteit `size` (uit de layout)
    /// en de belofte van deze kant ([`abi::ring::Coherence`]).
    pub fn open(base: Pa, size: u64, local: abi::ring::Coherence) -> abi::Result<Self> {
        Ok(Self {
            ring: abi::ring::Reader::open_with(base, size, local)?,
            base,
        })
    }
}

impl Reader for AbiTx {
    type Why = abi::ring::Corrupt;

    fn read_into(&mut self, buf: &mut [u8]) -> Option<(u32, usize)> {
        let r = self.ring.read_into(buf)?;
        Some((r.kind.raw(), r.payload.len()))
    }

    fn read_in_place<T>(&mut self, max: usize, f: impl FnOnce(u32, &[u8]) -> T) -> Option<T> {
        self.ring.read_with(max, |kind, p| f(kind.raw(), p))
    }

    fn is_normal(&self) -> bool {
        self.ring.coherence() == Coherence::Hardware
    }

    fn corrupt(&self) -> Option<Self::Why> {
        self.ring.corrupt()
    }

    fn probe_handle(&self) -> u64 {
        self.base.0
    }

    fn probe(handle: u64) -> bool {
        // Kop en staart met kale loads (Go: `HeadPending` zonder de ring te
        // openen). De maat komt hier uit het gedeelde maatwoord: dat mag de
        // tegenpartij wijzigen, maar het stuurt alleen een bel, nooit een
        // geheugentoegang. Hoort als vrije functie in `abi::ring`; tot die er
        // is staat hij hier.
        let base = Pa(handle);
        let at = |off: u64| {
            dev::pull(base.add(off), 8);
            dev::read64(base.add(off))
        };
        let n = at(abi::ring::HEAD_OFF).wrapping_sub(at(abi::ring::TAIL_OFF));
        n != 0 && n <= at(abi::ring::SIZE_OFF)
    }
}

impl Writer for abi::ring::Writer {
    fn write_notify(&mut self, kind: u32, p: &[u8]) -> Option<bool> {
        // `write` publiceert zijn kop zelf (push na elke kop), dus
        // `publish_head` heeft hier niets meer te doen.
        self.write(Kind::new(kind)?, p).ok()
    }

    fn write_in_place(
        &mut self,
        max: usize,
        f: impl FnOnce(&mut [u8]) -> Option<(u32, usize)>,
    ) -> InPlace {
        let r = self.write_with_kind(max, |p| {
            let (kind, n) = f(p)?;
            Some((Kind::new(kind)?, n))
        });
        match r {
            Ok(Some(was_empty)) => InPlace::Written(was_empty),
            Ok(None) => InPlace::Nothing,
            Err(_) => InPlace::Full,
        }
    }

    fn is_normal(&self) -> bool {
        self.coherence() == Coherence::Hardware
    }
}

#[cfg(test)]
pub(crate) mod mem {
    //! Een ring over een host-buffer: dezelfde contracten, zonder
    //! device-geheugen. Beide helften delen één `Rc`; de probe leest een
    //! thread-lokaal register, want de tests draaien parallel.
    use super::{InPlace, Reader, Writer};
    use std::cell::RefCell;
    use std::collections::VecDeque;
    use std::rc::Rc;

    pub(crate) struct Inner {
        pub(crate) q: VecDeque<(u32, Vec<u8>)>,
        pub(crate) cap: usize,
        pub(crate) used: usize,
        pub(crate) corrupt: Option<&'static str>,
        /// Hoeveel schrijfpogingen nog "vol" moeten zien: het model van een
        /// consument op een andere core die even achterloopt.
        pub(crate) refuse: usize,
        pub(crate) writes: usize,
    }

    pub(crate) type Shared = Rc<RefCell<Inner>>;

    thread_local! {
        static REG: RefCell<Vec<Shared>> = const { RefCell::new(Vec::new()) };
    }

    /// Een ring van `cap` databytes; geeft beide helften.
    pub(crate) fn pair(cap: usize) -> (MemReader, MemWriter) {
        let s: Shared = Rc::new(RefCell::new(Inner {
            q: VecDeque::new(),
            cap,
            used: 0,
            corrupt: None,
            refuse: 0,
            writes: 0,
        }));
        let h = REG.with(|r| {
            r.borrow_mut().push(s.clone());
            r.borrow().len() as u64 - 1
        });
        (MemReader(s.clone(), h), MemWriter(s))
    }

    /// De leeskant.
    pub(crate) struct MemReader(pub(crate) Shared, u64);
    /// De schrijfkant.
    pub(crate) struct MemWriter(pub(crate) Shared);

    fn rec_len(n: usize) -> usize {
        8 + n.div_ceil(8) * 8
    }

    impl Reader for MemReader {
        type Why = &'static str;

        fn read_into(&mut self, buf: &mut [u8]) -> Option<(u32, usize)> {
            let mut s = self.0.borrow_mut();
            if s.corrupt.is_some() {
                return None;
            }
            let (kind, p) = s.q.pop_front()?;
            s.used -= rec_len(p.len());
            if p.len() > buf.len() {
                s.corrupt = Some("record larger than the reader's buffer");
                return None;
            }
            buf[..p.len()].copy_from_slice(&p);
            Some((kind, p.len()))
        }
        fn read_in_place<T>(&mut self, max: usize, f: impl FnOnce(u32, &[u8]) -> T) -> Option<T> {
            let mut buf = vec![0u8; max];
            let (kind, n) = self.read_into(&mut buf)?;
            Some(f(kind, &buf[..n]))
        }
        fn is_normal(&self) -> bool {
            true
        }
        fn corrupt(&self) -> Option<&'static str> {
            self.0.borrow().corrupt
        }
        fn probe_handle(&self) -> u64 {
            self.1
        }
        fn probe(handle: u64) -> bool {
            REG.with(|r| {
                r.borrow()
                    .get(handle as usize)
                    .is_some_and(|s| !s.borrow().q.is_empty())
            })
        }
    }

    impl Writer for MemWriter {
        fn write_notify(&mut self, kind: u32, p: &[u8]) -> Option<bool> {
            let mut s = self.0.borrow_mut();
            if s.refuse > 0 {
                s.refuse -= 1;
                return None;
            }
            // Een record mag hoogstens de halve ring zijn (zoals de ABI-ring).
            if rec_len(p.len()) > s.cap / 2 || s.used + rec_len(p.len()) > s.cap {
                return None;
            }
            let was_empty = s.q.is_empty();
            s.used += rec_len(p.len());
            s.q.push_back((kind, p.to_vec()));
            s.writes += 1;
            Some(was_empty)
        }
        fn write_in_place(
            &mut self,
            max: usize,
            f: impl FnOnce(&mut [u8]) -> Option<(u32, usize)>,
        ) -> InPlace {
            {
                // Zoals de ABI-ring: plaats voor `max` vóór `f`, anders vol.
                let mut s = self.0.borrow_mut();
                if s.refuse > 0 {
                    s.refuse -= 1;
                    return InPlace::Full;
                }
                if rec_len(max) > s.cap / 2 || s.used + rec_len(max) > s.cap {
                    return InPlace::Full;
                }
            }
            let mut buf = vec![0u8; max];
            match f(&mut buf) {
                Some((kind, n)) if n > 0 => match self.write_notify(kind, &buf[..n.min(max)]) {
                    Some(e) => InPlace::Written(e),
                    None => InPlace::Full,
                },
                _ => InPlace::Nothing,
            }
        }
        fn is_normal(&self) -> bool {
            true
        }
    }

    impl MemReader {
        pub(crate) fn pop(&mut self) -> Option<(u32, Vec<u8>)> {
            let mut s = self.0.borrow_mut();
            let r = s.q.pop_front()?;
            s.used -= rec_len(r.1.len());
            Some(r)
        }
        /// Het volgende frame (soort `KIND_FRAME`), of `None`.
        pub(crate) fn frame(&mut self) -> Option<Vec<u8>> {
            match self.pop() {
                Some((super::KIND_FRAME, f)) => Some(f),
                _ => None,
            }
        }
    }

    impl MemWriter {
        pub(crate) fn push(&mut self, kind: u32, p: &[u8]) -> bool {
            self.write_notify(kind, p).is_some()
        }
    }
}
