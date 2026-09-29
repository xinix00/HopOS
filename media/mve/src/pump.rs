//! De pomp (Go: `pump.go`): de enige plek waar de firmware iets van ons
//! gedaan krijgt. Hij bedient het geheugenverzoek, leest de berichten en
//! haalt de buffers op die klaar zijn. Alles loopt vanuit `next_event`; de
//! driver heeft geen interrupthandler, zodat er geen codec-werk in
//! interruptcontext draait en de aanroeper zijn eigen tempo houdt, net als
//! bij onze NIC's.

use crate::arena::MAX_LOG2_ALIGN;
use crate::mmu::{ACCESS_RW, PAGE};
use crate::proto::*;
use crate::session::{Hw, MAX_EVENTS, Owned, Ses};
use bounded::BoundedVec;
use dev::Pa;
use driver_codec::{Direction, Error, Event, Kind, Layout, Pixel, Plane};

/// De RpcOom-soorten.
const OOM_VA: u8 = 0;
const OOM_ARENA: u8 = 1;
const OOM_TABLES: u8 = 2;
const OOM_SPAN: u8 = 3;

impl Ses {
    /// Eén ronde: geheugenverzoek, berichten, invoer terug, uitvoer terug.
    pub(crate) fn pump(&mut self, hw: &mut Hw) {
        self.serve_rpc(hw);
        self.drain_messages(hw);
        self.drain_buffers(true);
        self.drain_buffers(false);
        // Pas hier, na de bufferringen: de firmware geeft eerst alle
        // uitvoerbuffers terug en meldt dán OUTPUT_FLUSHED. Wie op het
        // bericht afgaat vóór hij de ring leegt, ziet die teruggegeven
        // buffers aan voor gedecodeerde beelden.
        if self.out_ack && self.failed.is_none() {
            self.out_ack = false;
            self.out_hold = false;
            while let Some(b) = self.out_pend.pop_front() {
                if let Err(e) = self.send_output(hw, b) {
                    self.fail(e);
                    return;
                }
            }
        }
    }

    /// Past er nog een event (plus een EOS-splitsing en een Fault) bij?
    fn room(&self) -> bool {
        self.events.len() + 3 <= MAX_EVENTS
    }

    /// De handdruk die de firmware na SEQUENCE_PARAMETERS eist: "will not
    /// make any progress until the host sends an output-flush command"
    /// (`mve_protocol_def.h`). Flush sturen, zijn buffers laten terugkomen,
    /// en pas na OUTPUT_FLUSHED opnieuw aanbieden.
    fn hold_output(&mut self, hw: &mut Hw) {
        if self.cfg.dir != Direction::Decode || self.out_hold {
            return;
        }
        self.out_hold = true;
        self.out_ack = false;
        self.stat.flushes += 1;
        if let Err(e) = self.msg.send(REQ_OUTPUT_FLUSH, &[]) {
            self.fail(e);
            return;
        }
        self.start_job();
        self.schedule(hw);
    }

    fn drain_messages(&mut self, hw: &mut Hw) {
        let mut buf = [0u8; 256];
        while self.room() {
            let mut r = self.msg;
            match r.recv(&mut buf) {
                Err(e) => return self.fail(e),
                Ok(None) => return,
                Ok(Some((code, n))) => {
                    let body = buf.get(..n).unwrap_or(&[]);
                    if let Some(t) = hw.trace {
                        t(code, body);
                    }
                    self.handle_message(hw, code, body);
                }
            }
        }
    }

    /// Vertaalt één bericht naar wat de aanroeper moet merken. Het meeste is
    /// boekhouding; alleen de streamparameters, het einde en de fouten gaan
    /// naar buiten.
    fn handle_message(&mut self, hw: &mut Hw, code: u16, body: &[u8]) {
        match code {
            RESP_SEQ_PARAMS => {
                if body.len() < 7 {
                    return;
                }
                self.seq.known = true;
                self.seq.min_buffers = u32::from(body[4]);
                self.publish_layout();
                self.hold_output(hw);
            }
            RESP_FRAME_ALLOC_PARAM => {
                if body.len() < 20 {
                    return;
                }
                self.alloc.known = true;
                self.alloc.width = get16(body, 0);
                self.alloc.height = get16(body, 2);
                self.publish_layout();
            }
            RESP_ERROR => {
                // De tekst achter de code is voor de trace; de code gaat mee.
                self.fail(Error::Firmware {
                    code: get32(body, 0),
                });
            }
            RESP_STATE_CHANGE => {
                // 0 = gestopt: alles is terug, en na een EOS is dit het punt
                // waarop de stream werkelijk af is.
                if body.len() >= 4 && get32(body, 0) == 0 {
                    self.post(Event::of(Kind::Done));
                }
            }
            RESP_IDLE => {
                // De firmware gaat slapen. Alleen bevestigen: zijn job blijft
                // staan (IDLE is geen einde van een beurt), en wie hier een
                // nieuwe job stuurt loopt de wachtrij vol. De volgende buffer
                // wekt hem via de deurbel.
                let _ = self.msg.send(REQ_IDLE_ACK, &[]);
                self.kick(hw);
            }
            RESP_JOB_DEQUEUED => {
                // Dít is het einde van een beurt en het enige moment waarop
                // er een nieuwe bij mag: meteen, zodat er precies één open
                // staat.
                self.job_live = false;
                self.start_job();
                self.schedule(hw);
            }
            RESP_EVENT => {
                if body.len() >= 4 {
                    match get32(body, 0) {
                        EV_STREAM_CORRUPT => self.stat.stream_corrupt += 1,
                        EV_STREAM_UNSUPPORTED => self.fail(Error::StreamUnsupported),
                        _ => {}
                    }
                }
            }
            RESP_OUTPUT_FLUSHED => {
                // Niet hier: de buffers van deze flush staan nog in de ring.
                self.out_ack = true;
            }
            RESP_OPTION_FAIL => self.fail(Error::OptionRejected),
            RESP_INPUT
            | RESP_OUTPUT
            | RESP_SWITCHED_IN
            | RESP_SWITCHED_OUT
            | RESP_OPTION_CONFIRM
            | RESP_PONG
            | RESP_INPUT_FLUSHED
            | RESP_REF_FRAME_UNUSED => {
                // Seintjes; het werk staat in de bufferringen.
            }
            _ => {}
        }
    }

    /// Meldt het beeldformaat zodra beide helften bekend zijn: wat de stream
    /// is en hoe groot een buffer moet zijn. Daarvóór weet niemand, ook de
    /// firmware niet, hoeveel geheugen een frame kost.
    fn publish_layout(&mut self) {
        if !self.seq.known || !self.alloc.known || self.cfg.dir != Direction::Decode {
            return;
        }
        let (w, h) = (u32::from(self.alloc.width), u32::from(self.alloc.height));
        let stride = self.stride(w);
        let luma = u64::from(stride) * u64::from(h);
        // Hier staat hoe groot een BUFFER moet zijn; wat er zichtbaar van is
        // komt per beeld in de descriptor.
        let mut l = Layout {
            width: w,
            height: h,
            alloc_width: w,
            alloc_height: h,
            pixel: self.cfg.pixel,
            planes: [Plane::default(); 3],
            frame_size: 0,
            min_buffers: self.seq.min_buffers.max(1),
        };
        l.planes[0] = Plane { off: 0, stride };
        match self.cfg.pixel {
            Pixel::I420 => {
                l.planes[1] = Plane {
                    off: luma,
                    stride: stride / 2,
                };
                l.planes[2] = Plane {
                    off: luma + luma / 4,
                    stride: stride / 2,
                };
                l.frame_size = luma * 3 / 2;
            }
            Pixel::Y8 => l.frame_size = luma,
            _ => {
                l.planes[1] = Plane { off: luma, stride };
                l.frame_size = luma * 3 / 2;
            }
        }
        if self.layout == l {
            return;
        }
        self.layout = l;
        let mut ev = Event::of(Kind::Format);
        ev.layout = l;
        self.post(ev);
    }

    /// Haalt de buffers op die de firmware teruggeeft: `input` is de ring
    /// met verwerkte invoer, anders die met resultaten.
    fn drain_buffers(&mut self, input: bool) {
        let mut buf = [0u8; BF_SIZE + 32];
        while self.room() && self.failed.is_none() {
            let mut r = if input { self.bin } else { self.bout };
            let (code, n) = match r.recv(&mut buf) {
                Err(e) => return self.fail(e),
                Ok(None) => return,
                Ok(Some(m)) => m,
            };
            if n < 24 {
                continue;
            }
            let body = &buf[..n];
            self.stat.returns += 1;
            let Some(h) = self.take(get64(body, BF_HOST_HANDLE)) else {
                self.stat.unknown += 1;
                continue; // Al opgeruimd, bijvoorbeeld door een flush.
            };
            if !input && self.out_hold {
                // Teruggegeven door de flush, niet gedecodeerd: terug in de
                // wachtrij, straks opnieuw op zijn vaste plek.
                self.stat.flush_back += 1;
                if self.out_pend.push(h.buf).is_err() {
                    self.fail(Error::Full {
                        cap: crate::session::MAX_HELD as u32,
                    });
                }
                continue;
            }
            let mut ev = Event::of(Kind::Consumed);
            ev.buf = Some(h.buf);
            ev.tag = h.tag;
            if input {
                self.post(ev);
                continue;
            }
            if code == BUF_FRAME && n >= BF_SIZE {
                // De firmware neemt de tijdstempel van de invoer mee naar het
                // beeld; `h.tag` hoort bij de lege uitvoerbuffer.
                ev.tag = get64(body, BF_USER_TAG);
                let flags = get32(body, BF_FLAGS);
                if flags & FR_FLAG_CORRUPT != 0 {
                    self.stat.corrupt += 1;
                    return self.fail(Error::CorruptFrame);
                }
                if flags & FR_FLAG_DEC_ONLY != 0 {
                    self.stat.decode_only += 1;
                } else if flags & FR_FLAG_REJECTED != 0 {
                    self.stat.rejected += 1;
                }
                if flags & FR_FLAG_REF_FRAME != 0 {
                    self.stat.ref_frame += 1;
                }
                ev.kind = Kind::Produced;
                ev.layout = self.layout;
                ev.layout.width = u32::from(get16(body, BF_VISIBLE_WIDTH));
                ev.layout.height = u32::from(get16(body, BF_VISIBLE_HEIGHT));
                ev.bytes = self.layout.frame_size;
                // Gedecodeerd maar niet om te tonen: de buffer gaat terug met
                // nul bytes, zodat een archiveerder hem overslaat.
                if flags & (FR_FLAG_DEC_ONLY | FR_FLAG_REJECTED) != 0 {
                    ev.bytes = 0;
                }
                // EOS op een frame is "dit is het LAATSTE beeld", niet "dit
                // is geen beeld".
                if flags & FR_FLAG_EOS != 0 {
                    self.stat.eos += 1;
                    ev = self.split_eos(ev);
                }
            } else if code == BUF_BITSTREAM && n >= BS_SIZE {
                ev.kind = Kind::Produced;
                ev.bytes = u64::from(get32(body, BS_FILLED_LEN));
                let flags = get32(body, BS_FLAGS);
                ev.key = flags & BS_FLAG_SYNC_FRAME != 0;
                if flags & BS_FLAG_EOS != 0 {
                    self.stat.eos += 1;
                    ev = self.split_eos(ev);
                }
            } else {
                continue;
            }
            self.post(ev);
        }
    }

    /// Knipt een laatste buffer in twee: draagt hij inhoud, dan gaat hij als
    /// resultaat naar buiten en is de Done erna leeg; is hij leeg, dan reist
    /// hij mee met de Done zodat de aanroeper zijn geheugen terugkrijgt. Wie
    /// alleen Done post, gooit elke stream precies één beeld weg.
    fn split_eos(&mut self, ev: Event) -> Event {
        let mut done = Event::of(Kind::Done);
        done.tag = ev.tag;
        if ev.bytes == 0 {
            done.buf = ev.buf;
            return done;
        }
        self.post(ev);
        done
    }

    /// Bedient het geheugenverzoek van de firmware. Een decoder vraagt zelf
    /// om zijn referentieframes zodra hij de stream kent: bij 4K HEVC
    /// honderden MB's.
    fn serve_rpc(&mut self, hw: &mut Hw) {
        let rpc = self.rpc;
        if rpc == 0 || dev::read32(Pa(rpc + RPC_STATE)) != RPC_STATE_PARAM {
            return;
        }
        let p = |i: u64| dev::read32(Pa(rpc + RPC_PARAMS + 4 * i));
        let ret = match dev::read32(Pa(rpc + RPC_CALL_ID)) {
            RPC_ALLOC => {
                // mem_alloc: size u32 | max_size u32 | region u8 | log2 u8.
                let tail = p(2);
                self.rpc_allocate(
                    hw,
                    p(0),
                    p(1),
                    (tail >> 8) as u8,
                    tail as u8 == RPC_REGION_PROTECTED,
                )
            }
            RPC_RESIZE => self.rpc_resize(hw, p(0), p(1)),
            RPC_FREE => {
                self.rpc_release(hw, p(0));
                0
            }
            // De firmware praat; alleen bij bring-up nuttig.
            RPC_PRINTF => 0,
            _ => 0,
        };
        dev::write32(Pa(rpc + RPC_PARAMS), ret);
        dev::write32(Pa(rpc + RPC_SIZE), 4);
        dev::mb();
        dev::write32(Pa(rpc + RPC_STATE), RPC_STATE_RETURN);
        dev::mb();
        self.kick(hw);
    }

    /// Een geweigerd verzoek zichtbaar maken. De firmware krijgt nul terug
    /// en schrijft erin: een halve seconde later een MMU ABORT zonder dat
    /// iemand weet waarom. Dít is waarom.
    fn fail_rpc(&mut self, what: u8, size: u32) {
        let mb = u64::from(size).div_ceil(1 << 20) as u32;
        self.fail(Error::RpcOom { mb, what });
    }

    /// Geeft de firmware geheugen en zegt op welk virtueel adres; 0 is "niet
    /// gelukt" (de firmware meldt dan OUT_OF_MEMORY, geen paniekpad).
    fn rpc_allocate(&mut self, hw: &mut Hw, size: u32, max: u32, log2: u8, prot: bool) -> u32 {
        let max = max.max(size);
        let pages = Ses::pages_for(size);
        let reserve = Ses::pages_for(max);
        if pages == 0 {
            return 0;
        }
        if log2 > MAX_LOG2_ALIGN {
            self.fail(Error::Alignment { log2 });
            return 0;
        }
        let log2 = log2.max(Ses::PAGE_SHIFT);
        // Ruimte voor het uitlijnen ERBIJ reserveren, niet erin knabbelen:
        // anders loopt het staartje over het volgende blok heen, en dat is
        // stille corruptie in het referentiegeheugen (groene blokken).
        let align = (1u64 << log2) - 1;
        let slack = (align / PAGE) as u32;
        let Some(va) = self.region(prot).alloc(reserve.saturating_add(slack)) else {
            self.fail_rpc(OOM_VA, max);
            return 0;
        };
        let va = ((u64::from(va) + align) & !align) as u32;
        let pa = match hw.arena.alloc(pages, log2) {
            Ok(pa) => pa,
            Err(_) => {
                self.fail_rpc(OOM_ARENA, size);
                return 0;
            }
        };
        if self
            .mmu
            .map_range(&mut hw.arena, va, pa, pages, ACCESS_RW)
            .is_err()
        {
            hw.arena.free(pa, pages);
            self.fail_rpc(OOM_TABLES, size);
            return 0;
        }
        let mut parts = BoundedVec::new();
        let _ = parts.push(crate::mmu::Span { pa, pages });
        let o = Owned {
            va,
            span: reserve,
            pages,
            parts,
        };
        if let Err(bounded::Full(mut o)) = self.rpc_mem.push(o) {
            self.mmu.unmap_range(va, pages);
            o.parts.clear();
            hw.arena.free(pa, pages);
            self.fail(Error::Full {
                cap: crate::session::MAX_RPC as u32,
            });
            return 0;
        }
        self.stat.rpc_allocs += 1;
        self.stat.rpc_pages += pages;
        va
    }

    /// Laat een blok groeien binnen zijn gereserveerde span. Krimpen
    /// negeren we: dat versnippert de arena en de firmware vraagt er zelden
    /// om. De groei is een eigen stuk arena achter het blok.
    fn rpc_resize(&mut self, hw: &mut Hw, va: u32, new_size: u32) -> u32 {
        let Some(i) = self.rpc_mem.iter().position(|o| o.va == va) else {
            return 0;
        };
        let want = Ses::pages_for(new_size);
        let (have, span) = match self.rpc_mem.get(i) {
            Some(o) => (o.pages, o.span),
            None => return 0,
        };
        if want <= have {
            return va;
        }
        if want > span {
            self.fail_rpc(OOM_SPAN, new_size);
            return 0;
        }
        let extra = want - have;
        let Ok(pa) = hw.arena.alloc(extra, Ses::PAGE_SHIFT) else {
            self.fail_rpc(OOM_ARENA, new_size);
            return 0;
        };
        let at = va.wrapping_add(have.wrapping_mul(PAGE as u32));
        if self
            .mmu
            .map_range(&mut hw.arena, at, pa, extra, ACCESS_RW)
            .is_err()
        {
            hw.arena.free(pa, extra);
            self.fail_rpc(OOM_TABLES, new_size);
            return 0;
        }
        let Some(o) = self.rpc_mem.as_mut_slice().get_mut(i) else {
            return 0;
        };
        if o.parts.push(crate::mmu::Span { pa, pages: extra }).is_err() {
            self.mmu.unmap_range(at, extra);
            hw.arena.free(pa, extra);
            self.fail(Error::Full {
                cap: crate::session::MAX_PARTS as u32,
            });
            return 0;
        }
        o.pages = want;
        self.stat.rpc_pages += extra;
        va
    }

    /// Geeft een blok terug.
    fn rpc_release(&mut self, hw: &mut Hw, va: u32) {
        let Some(i) = self.rpc_mem.iter().position(|o| o.va == va) else {
            return;
        };
        if let Some(mut o) = self.rpc_mem.swap_remove(i) {
            self.mmu.unmap_range(o.va, o.pages);
            for p in o.parts.iter() {
                hw.arena.free(p.pa, p.pages);
            }
            o.parts.clear();
        }
    }
}

/// `BoundedVec` als rij: de oudste eerst.
trait Front<T> {
    fn pop_front(&mut self) -> Option<T>;
}

impl<T, const N: usize> Front<T> for BoundedVec<T, N> {
    fn pop_front(&mut self) -> Option<T> {
        if self.is_empty() {
            None
        } else {
            self.remove(0)
        }
    }
}
