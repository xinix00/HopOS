//! Eén hardware-sessie (Go: `session.go`): de adresruimte, de ringen, de
//! boekhouding van de buffers en de vaste plekken, en de descriptors.
//!
//! Een [`Ses`] hoort bij één LSID en wordt bij de probe één keer gereserveerd
//! en daarna per sessie in place hergebruikt: de kern-heap is een
//! bump-allocator, en een allocatie per open zou lekken. Alle tabellen zijn
//! begrensd (`BoundedVec`); vol is een fout met een getal, geen groei.

use crate::arena::Arena;
use crate::fwbin::{self, Header};
use crate::hwreg::{
    ALLOC_FREE, ALLOC_NON_PROTECTED, Block, CTRL_MAX_CORES_SHIFT, EMPTY_JOB_QUEUE, JOB_INVALID,
    JOB_SLOTS, job_slot_lsid, set_job_slot,
};
use crate::mmu::{ACCESS_RW, ATTR_PRIVATE, Mmu, PAGE, PAGE_SHIFT, Span, pte};
use crate::proto::*;
use crate::queue::Ring;
use bounded::BoundedVec;
use driver_codec::{Buffer, Config, Direction, Error, Event, Flags, Kind, Layout, Pixel, Result};

/// Hoeveel buffers er tegelijk bij de firmware kunnen liggen. Een decoder
/// wil er zestien referenties plus een pool van een handvol; 64 is ruim.
pub(crate) const MAX_HELD: usize = 64;
/// Hoeveel vaste plekken een sessie onthoudt.
pub(crate) const MAX_PLACED: usize = 64;
/// Hoeveel events er klaar kunnen staan. Vol is tegendruk: de pomp laat de
/// rest in de ringen van de firmware staan tot de aanroeper ophaalt.
pub(crate) const MAX_EVENTS: usize = 32;
/// Hoeveel geheugenblokken de firmware tegelijk mag vragen.
pub(crate) const MAX_RPC: usize = 64;
/// Hoeveel fysieke stukken één blok na resizes mag hebben.
pub(crate) const MAX_PARTS: usize = 4;
/// Hoeveel vrijgekomen gaten één regio onthoudt.
pub(crate) const MAX_HOLES: usize = 64;

/// De klok en de hardware die een sessie aanraakt; gedeeld door alle
/// sessies van één device, en net als zij van de ene eigenaar-taak.
pub(crate) struct Hw {
    pub(crate) regs: &'static Block,
    pub(crate) arena: Arena,
    /// Nanoseconden; ook de wachtlus van TERMINATE loopt erop.
    pub(crate) now: fn() -> u64,
}

/// Een buffer die bij de firmware ligt; `id` is de `host_handle` die de
/// firmware ongewijzigd teruggeeft.
#[derive(Copy, Clone, Debug)]
pub(crate) struct Held {
    pub(crate) id: u64,
    pub(crate) buf: Buffer,
    pub(crate) va: u32,
    pub(crate) pages: u32,
    pub(crate) tag: u64,
}

/// De vaste plek van één fysieke buffer in de adresruimte van de firmware.
#[derive(Copy, Clone, Debug)]
pub(crate) struct Placed {
    pub(crate) pa: u64,
    pub(crate) va: u32,
    pub(crate) pages: u32,
    pub(crate) protected: bool,
    /// Ligt hij nu bij het ijzer?
    pub(crate) live: bool,
}

/// Geheugen dat de firmware zelf vroeg (referentieframes, werkruimte). Eén
/// blok kan uit meer fysieke stukken bestaan: een resize hangt een nieuw
/// stuk achter wat er stond, en elk stuk geeft alleen zijn eigen pagina's
/// terug.
#[derive(Debug)]
pub(crate) struct Owned {
    pub(crate) va: u32,
    /// De gereserveerde virtuele ruimte (max_size).
    pub(crate) span: u32,
    /// De gemapte pagina's, alle stukken samen.
    pub(crate) pages: u32,
    pub(crate) parts: BoundedVec<Span, MAX_PARTS>,
}

impl Owned {
    fn free(&mut self, a: &mut Arena) {
        for p in self.parts.iter() {
            a.free(p.pa, p.pages);
        }
        self.parts.clear();
    }
}

/// Een gat dat vrijkwam.
#[derive(Copy, Clone, Debug)]
pub(crate) struct Hole {
    pub(crate) va: u32,
    pub(crate) pages: u32,
}

/// De virtuele ruimte van één firmware-regio: eerst wat vrijkwam, daarna
/// verse ruimte. Hergebruik op EXACTE maat: de aanroeper werkt met een pool
/// van gelijke buffers, dus deze lijst fragmenteert niet.
#[derive(Debug, Default)]
pub(crate) struct VaRegion {
    pub(crate) beg: u32,
    pub(crate) end: u32,
    pub(crate) next: u32,
    pub(crate) free: BoundedVec<Hole, MAX_HOLES>,
}

impl VaRegion {
    pub(crate) fn reset(&mut self, beg: u32, end: u32) {
        self.beg = beg;
        self.end = end;
        self.next = beg;
        self.free.clear();
    }

    pub(crate) fn alloc(&mut self, pages: u32) -> Option<u32> {
        if let Some(i) = self.free.iter().position(|h| h.pages == pages) {
            return self.free.swap_remove(i).map(|h| h.va);
        }
        // In 64 bits: pages * PAGE loopt in 32 bits over, en dan past een
        // aanvraag van 4 GB "makkelijk" in de paar pagina's die resten.
        let size = u64::from(pages) * PAGE;
        if size == 0 || u64::from(self.next) + size > u64::from(self.end) {
            return None;
        }
        let va = self.next;
        self.next = (u64::from(self.next) + size) as u32;
        Some(va)
    }

    /// Geeft ruimte terug. Een volle gatenlijst laat het gat vallen: dat
    /// kost adresruimte tot de sessie sluit, geen correctheid.
    pub(crate) fn put(&mut self, va: u32, pages: u32) {
        let _ = self.free.push(Hole { va, pages });
    }
}

/// Wat de firmware over de stream meldde (RESP_SEQ_PARAMS).
#[derive(Copy, Clone, Debug, Default)]
pub(crate) struct Seq {
    pub(crate) known: bool,
    pub(crate) min_buffers: u32,
}

/// Hoe groot een framebuffer moet zijn (RESP_FRAME_ALLOC_PARAM).
#[derive(Copy, Clone, Debug, Default)]
pub(crate) struct AllocParams {
    pub(crate) known: bool,
    pub(crate) width: u16,
    pub(crate) height: u16,
}

/// Tellers voor de bring-up: een beeld dat onderweg verdwijnt is aan de
/// buitenkant niet te onderscheiden van een beeld dat er nooit was.
#[derive(Copy, Clone, Debug, Default)]
pub(crate) struct Stat {
    pub(crate) decode_only: u32,
    pub(crate) corrupt: u32,
    pub(crate) rejected: u32,
    pub(crate) unknown: u32,
    pub(crate) ref_frame: u32,
    pub(crate) stream_corrupt: u32,
    pub(crate) flushes: u32,
    pub(crate) flush_back: u32,
    pub(crate) eos: u32,
    pub(crate) rpc_allocs: u32,
    pub(crate) rpc_pages: u32,
    pub(crate) offers: u32,
    pub(crate) returns: u32,
}

/// De staat van één hardware-sessie.
pub(crate) struct Ses {
    pub(crate) open: bool,
    pub(crate) cfg: Config,
    pub(crate) lsid: u8,
    pub(crate) pixel: u16,
    pub(crate) mmu: Mmu,
    pub(crate) msg: Ring,
    pub(crate) bin: Ring,
    pub(crate) bout: Ring,
    pub(crate) rpc: u64,
    pub(crate) reg: Regions,
    pub(crate) va_frame: VaRegion,
    pub(crate) va_prot: VaRegion,
    pub(crate) placed: BoundedVec<Placed, MAX_PLACED>,
    pub(crate) bufs: BoundedVec<Held, MAX_HELD>,
    pub(crate) rpc_mem: BoundedVec<Owned, MAX_RPC>,
    pub(crate) handle: u64,
    pub(crate) events: BoundedVec<Event, MAX_EVENTS>,
    pub(crate) layout: Layout,
    pub(crate) seq: Seq,
    pub(crate) alloc: AllocParams,
    pub(crate) failed: Option<Error>,
    /// Is de Fault van `failed` al als event naar buiten?
    pub(crate) fault_posted: bool,
    /// Draait er een beurt bij de firmware?
    pub(crate) job_live: bool,
    /// Na SEQUENCE_PARAMETERS zet de firmware zijn UITVOERPOORT stil en komt
    /// er pas weer uit na een output-flush: hij geeft dan alle
    /// uitvoerbuffers terug, meldt OUTPUT_FLUSHED, en pas daarna mag de host
    /// opnieuw aanbieden (`mve_protocol_def.h`). Wie dat overslaat ziet een
    /// decoder die de stream herkent en daarna niets meer doet.
    pub(crate) out_hold: bool,
    /// OUTPUT_FLUSHED gezien; buffers mogen weer.
    pub(crate) out_ack: bool,
    /// Wat de aanroeper intussen aanbood.
    pub(crate) out_pend: BoundedVec<Buffer, MAX_HELD>,
    pub(crate) stat: Stat,
}

impl Ses {
    /// Een lege sessie met haar tabel-index gereserveerd (boot).
    pub(crate) fn reserve(lsid: u8) -> Result<Ses> {
        Ok(Ses {
            open: false,
            cfg: Config::default(),
            lsid,
            pixel: 0,
            mmu: Mmu::reserve()?,
            msg: Ring::default(),
            bin: Ring::default(),
            bout: Ring::default(),
            rpc: 0,
            reg: Regions::default(),
            va_frame: VaRegion::default(),
            va_prot: VaRegion::default(),
            placed: BoundedVec::new(),
            bufs: BoundedVec::new(),
            rpc_mem: BoundedVec::new(),
            handle: 0,
            events: BoundedVec::new(),
            layout: Layout::default(),
            seq: Seq::default(),
            alloc: AllocParams::default(),
            failed: None,
            fault_posted: false,
            job_live: false,
            out_hold: false,
            out_ack: false,
            out_pend: BoundedVec::new(),
            stat: Stat::default(),
        })
    }

    /// Zet de boekhouding terug voor een nieuwe sessie op dit slot.
    fn reset(&mut self, cfg: Config, pixel: u16) {
        self.cfg = cfg;
        self.pixel = pixel;
        self.msg = Ring::default();
        self.bin = Ring::default();
        self.bout = Ring::default();
        self.rpc = 0;
        self.placed.clear();
        self.bufs.clear();
        self.rpc_mem.clear();
        self.handle = 0;
        self.events.clear();
        self.layout = Layout::default();
        self.seq = Seq::default();
        self.alloc = AllocParams::default();
        self.failed = None;
        self.fault_posted = false;
        self.job_live = false;
        self.out_hold = false;
        self.out_ack = false;
        self.out_pend.clear();
        self.stat = Stat::default();
    }

    /// Bouwt de adresruimte en zet de sessie op haar hardware-slot. Faalt
    /// het halverwege, dan ruimt [`Ses::release`] alles op.
    pub(crate) fn start(
        &mut self,
        hw: &mut Hw,
        cfg: Config,
        pixel: u16,
        bin: &[u8],
        h: &Header,
    ) -> Result {
        self.reset(cfg, pixel);
        self.open = true;
        self.mmu.build(&mut hw.arena)?;
        self.reg = regions_for(h.protocol_major);
        self.va_frame.reset(self.reg.frame_beg, self.reg.frame_end);
        self.va_prot.reset(self.reg.prot_beg, self.reg.prot_end);
        fwbin::load(&mut self.mmu, &mut hw.arena, bin, h)?;

        // De zeven communicatiepagina's op hun vaste adressen: de firmware is
        // hierop gelinkt, deze nummers zijn geen keuze van ons.
        let mut comm = [0u64; 7];
        let vas = [
            VA_MSG_IN_Q,
            VA_MSG_OUT_Q,
            VA_BUF_IN_Q,
            VA_BUF_IN_RQ,
            VA_BUF_OUT_Q,
            VA_BUF_OUT_RQ,
            VA_RPC,
        ];
        for (pa, va) in comm.iter_mut().zip(vas) {
            *pa = self.mmu.alloc(&mut hw.arena, va, 1, ACCESS_RW)?;
        }
        // De checksum lezen we uit de blob in plaats van hem aan te nemen:
        // een toekomstige firmware zonder die eis werkt dan ook.
        let csum = h.has_sum();
        let ring = |host, mve| Ring {
            host,
            mve,
            sum: 0,
            csum,
        };
        self.msg = ring(comm[0], comm[1]);
        self.bin = ring(comm[2], comm[3]);
        self.bout = ring(comm[4], comm[5]);
        self.rpc = comm[6];

        self.map_lsid(hw)?;
        // GO alleen laat de firmware nog niets doen: hij wacht op een JOB
        // die zegt met hoeveel cores en hoeveel beelden (nul = tot ik je
        // stop). Gemeten op ijzer 22-09: zonder dit bericht leest de
        // firmware zijn berichtenring maar raakt hij de invoer niet aan.
        self.msg.send(REQ_GO, &[])?;
        self.start_job();
        self.schedule(hw);
        Ok(())
    }

    /// Geeft de firmware een beurt als er nog geen loopt. Een JOB is
    /// eenmalig: na afloop meldt de firmware JOB_DEQUEUED of IDLE en doet
    /// hij niets tot er een nieuwe komt (gemeten 22-09).
    pub(crate) fn start_job(&mut self) {
        if self.job_live {
            return;
        }
        let mut b = [0u8; 8];
        put(&mut b, 0, &1u16.to_le_bytes()); // één core
        put(&mut b, 2, &0u16.to_le_bytes()); // nul beelden: onbeperkt
        if self.msg.send(REQ_JOB, &b).is_ok() {
            self.job_live = true;
        }
    }

    /// Zet de sessie op haar hardware-slot. De volgorde is die van het ijzer:
    /// claimen en afbreken wat er stond, de tabel aanwijzen, en pas als alles
    /// staat het inplannen aanzetten.
    fn map_lsid(&mut self, hw: &mut Hw) -> Result {
        let l = hw
            .regs
            .lsid
            .get(usize::from(self.lsid))
            .ok_or(Error::Terminate { lsid: self.lsid })?;
        l.alloc.write(ALLOC_NON_PROTECTED);
        l.terminate.write(1);
        // Het ijzer wist dit bit zelf, in microseconden. Tien milliseconden
        // is ruim; daarna is het slot vast (het beeld van 27-09: "session
        // slot 0 will not terminate", opgelost door de stroomcyclus van het
        // board, zie board-o6n).
        if !dev::poll_until(hw.now, TERMINATE_NS, || l.terminate.read() == 0) {
            return Err(Error::Terminate { lsid: self.lsid });
        }
        // Eén core per sessie: geen core verboden, hoogstens één tegelijk.
        // Spreiden kost een firmwarekopie per extra core en is voor 8K.
        l.ctrl.write(1 << CTRL_MAX_CORES_SHIFT);
        // MMU_CTRL krijgt een VOLLEDIGE PTE, niet het paginanummer. Alleen
        // het nummer laat de VPU op een adres kijken dat vier keer te klein is
        // met toegangsrecht nul: hij start en zwijgt, want hij vindt zijn
        // eigen code niet (gemeten 22-09).
        l.mmu_ctrl
            .write(pte(ATTR_PRIVATE, self.mmu.table(), ACCESS_RW));
        l.flush_all.write(0);
        l.nprot.write(1);
        l.stream_id.write(0);
        for b in &l.bus_attr {
            b.write(0);
        }
        l.lirq_ve.write(0);
        l.irq_host.write(0);
        dev::mb();
        l.sched.write(1);
        Ok(())
    }

    /// Zet de sessie in de job-queue en tikt de firmware aan. Het inplannen
    /// gaat uit terwijl de queue verandert: dat wil het ijzer.
    pub(crate) fn schedule(&self, hw: &Hw) {
        let r = hw.regs;
        r.enable.write(0);
        let q = r.job_queue.read();
        // Altijd opnieuw inschrijven, nooit "staat er al" concluderen: de
        // hardware laat een opgepakte job achter als 0x00, en dat is voor
        // een sessie op slot 0 niet te onderscheiden van een wachtende job.
        // Wie daarop vertrouwt schrijft nooit een tweede beurt (22-09).
        let me = u32::from(self.lsid);
        let slot = (0..JOB_SLOTS)
            .find(|&i| job_slot_lsid(q, i) == me)
            .or_else(|| (0..JOB_SLOTS).find(|&i| job_slot_lsid(q, i) == JOB_INVALID));
        if let Some(i) = slot {
            r.job_queue.write(set_job_slot(q, i, me, 1));
        }
        r.enable.write(1);
        self.kick(hw);
    }

    /// De deurbel (Linux: `send_irq`). Elk bericht in een ring komt met zo'n
    /// tik; zonder ligt het er tot de firmware toevallig zelf kijkt.
    pub(crate) fn kick(&self, hw: &Hw) {
        dev::mb();
        if let Some(l) = hw.regs.lsid.get(usize::from(self.lsid)) {
            l.irq_host.write(1);
        }
    }

    /// Haalt de sessie uit de job-queue.
    fn unschedule(&self, hw: &Hw) {
        let r = hw.regs;
        r.enable.write(0);
        let q = r.job_queue.read();
        let mut out = EMPTY_JOB_QUEUE;
        let mut j = 0;
        for i in 0..JOB_SLOTS {
            if job_slot_lsid(q, i) != u32::from(self.lsid) {
                out = (out & !(0xff << (j * 8))) | (((q >> (i * 8)) & 0xff) << (j * 8));
                j += 1;
            }
        }
        r.job_queue.write(out);
        r.enable.write(1);
    }

    /// Voert bitstream (decode) of een frame (encode) in.
    pub(crate) fn feed(&mut self, hw: &mut Hw, b: Buffer, n: u64, f: Flags, tag: u64) -> Result {
        self.usable()?;
        let h = self.attach(hw, b, tag, true)?;
        let sent = if self.cfg.dir == Direction::Decode {
            let d = self.bitstream_desc(&h, n, f);
            self.bin.send(BUF_BITSTREAM, &d)
        } else {
            let d = self.frame_desc(&h, f, true);
            self.bin.send(BUF_FRAME, &d)
        };
        if let Err(e) = sent {
            self.detach(h.id, b.pa);
            return Err(e);
        }
        // Geen nieuwe JOB hier: er loopt er één met frames=0 en de volgende
        // komt pas na JOB_DEQUEUED. Een job per buffer loopt de wachtrij vol
        // ("no space in job queue", gemeten 22-09).
        self.schedule(hw);
        Ok(())
    }

    /// Biedt een lege buffer aan voor het resultaat.
    pub(crate) fn offer(&mut self, hw: &mut Hw, b: Buffer) -> Result {
        self.usable()?;
        if self.out_hold {
            // De firmware kijkt nu niet in de uitvoerring: vasthouden en
            // aanbieden zodra de flush rond is.
            return self.out_pend.push(b).map_err(|_| Error::Full {
                cap: MAX_HELD as u32,
            });
        }
        self.send_output(hw, b)
    }

    /// Hangt één lege buffer in de uitvoerring.
    pub(crate) fn send_output(&mut self, hw: &mut Hw, b: Buffer) -> Result {
        let h = self.attach(hw, b, 0, false)?;
        let sent = if self.cfg.dir == Direction::Decode {
            let d = self.frame_desc(&h, Flags(0), false);
            self.bout.send(BUF_FRAME, &d)
        } else {
            let d = self.bitstream_desc(&h, 0, Flags(0));
            self.bout.send(BUF_BITSTREAM, &d)
        };
        if let Err(e) = sent {
            self.detach(h.id, b.pa);
            return Err(e);
        }
        self.schedule(hw);
        Ok(())
    }

    /// Het volgende event. Na close doet dit niets meer: de ringen en de
    /// RPC-pagina zijn dan terug in de arena en misschien al van een andere
    /// sessie.
    pub(crate) fn next(&mut self, hw: &mut Hw) -> Option<Event> {
        if !self.open {
            return None;
        }
        self.pump(hw);
        if !self.events.is_empty() {
            return self.events.remove(0);
        }
        if let Some(e) = self.failed
            && !self.fault_posted
        {
            self.fault_posted = true;
            let mut ev = Event::of(Kind::Fault);
            ev.fault = Some(e);
            return Some(ev);
        }
        None
    }

    /// Breekt de sessie af en geeft alles terug.
    pub(crate) fn close(&mut self, hw: &mut Hw) {
        if !self.open {
            return;
        }
        let _ = self.msg.send(REQ_STOP, &[]);
        self.unschedule(hw);
        self.release(hw);
    }

    /// Geeft het hardware-slot en al het geheugen terug; ook het pad van een
    /// half opgestarte sessie.
    pub(crate) fn release(&mut self, hw: &mut Hw) {
        if let Some(l) = hw.regs.lsid.get(usize::from(self.lsid)) {
            l.sched.write(0);
            l.terminate.write(1);
            l.alloc.write(ALLOC_FREE);
        }
        // Eerst wat de firmware zelf vroeg: die pagina's staan niet in de
        // boekhouding van de tabel. Bij 4K is dit het leeuwendeel van de
        // arena; vergeten betekent dat de tweede film geen geheugen vindt.
        for o in self.rpc_mem.iter_mut() {
            o.free(&mut hw.arena);
        }
        self.rpc_mem.clear();
        self.mmu.destroy(&mut hw.arena);
        self.bufs.clear();
        self.placed.clear();
        self.events.clear();
        self.out_pend.clear();
        self.open = false;
    }

    /// Neemt de sessie nog aanvragen aan?
    pub(crate) fn usable(&self) -> Result {
        if !self.open {
            return Err(Error::Closed);
        }
        match self.failed {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }

    /// Hangt een buffer van de aanroeper in de adresruimte van de firmware.
    fn attach(&mut self, hw: &mut Hw, b: Buffer, tag: u64, input: bool) -> Result<Held> {
        let bad = Error::Buffer {
            pa: b.pa,
            size: b.size,
        };
        if b.pa == 0 || b.size == 0 || b.pa & (PAGE - 1) != 0 {
            return Err(bad);
        }
        // De firmware ziet 32-bit adressen; groter past nergens, en het
        // paginatal zou in 32 bits afgekapt worden tot iets wat wél past.
        if b.size > (1u64 << 32) - PAGE {
            return Err(bad);
        }
        if self.bufs.is_full() {
            return Err(Error::Full {
                cap: MAX_HELD as u32,
            });
        }
        let pages = b.size.div_ceil(PAGE) as u32;
        let va = self.place(hw, b.pa, pages, self.carries_bitstream(input))?;
        self.handle += 1;
        self.stat.offers += 1;
        let h = Held {
            id: self.handle,
            buf: b,
            va,
            pages,
            tag,
        };
        let _ = self.bufs.push(h);
        Ok(h)
    }

    /// Het ijzer is klaar met deze buffer. De MAPPING blijft staan: zie
    /// [`Ses::place`].
    fn detach(&mut self, id: u64, pa: u64) {
        if let Some(p) = self.placed.iter_mut().find(|p| p.pa == pa) {
            p.live = false;
        }
        if let Some(i) = self.bufs.iter().position(|h| h.id == id) {
            let _ = self.bufs.swap_remove(i);
        }
    }

    /// Haalt een buffer uit de boekhouding bij zijn terugkeer; de plek
    /// blijft.
    pub(crate) fn take(&mut self, handle: u64) -> Option<Held> {
        let i = self.bufs.iter().position(|h| h.id == handle)?;
        let h = self.bufs.swap_remove(i)?;
        if let Some(p) = self.placed.iter_mut().find(|p| p.pa == h.buf.pa) {
            p.live = false;
        }
        Some(h)
    }

    /// Geeft een fysieke buffer zijn plek in de adresruimte van de firmware,
    /// en dezelfde buffer krijgt altijd DEZELFDE plek.
    ///
    /// Geen optimalisatie maar een eis. De aanroeper draait rond op een pool
    /// van een handvol buffers; een roterende toewijzer vult per 4K-beeld
    /// 6075 PTE's opnieuw én is na tachtig beelden door zijn regio heen,
    /// begint dan vooraan en deelt adressen uit die nog bij een levende
    /// buffer horen. Gemeten op ijzer 22-09: MMU ABORT na precies 83 beelden
    /// op 4K.
    fn place(&mut self, hw: &mut Hw, pa: u64, pages: u32, protected: bool) -> Result<u32> {
        if let Some(i) = self.placed.iter().position(|p| p.pa == pa) {
            let p = self.placed.as_mut_slice().get_mut(i).ok_or(Error::Closed)?;
            if p.pages == pages && p.protected == protected {
                p.live = true;
                return Ok(p.va);
            }
            self.unplace(i); // Van maat of kant gewisseld: opnieuw.
        }
        let va = match self.region(protected).alloc(pages) {
            Some(va) => va,
            None => {
                // Alleen wat NIET bij het ijzer ligt mag wijken.
                self.evict(protected);
                self.region(protected)
                    .alloc(pages)
                    .ok_or(Error::AddressSpace {
                        pages: u64::from(pages),
                    })?
            }
        };
        if self.placed.is_full() {
            self.evict(protected);
            self.evict(!protected);
        }
        if let Err(e) = self.mmu.map_range(&mut hw.arena, va, pa, pages, ACCESS_RW) {
            self.region(protected).put(va, pages);
            return Err(e);
        }
        let placed = Placed {
            pa,
            va,
            pages,
            protected,
            live: true,
        };
        if self.placed.push(placed).is_err() {
            self.mmu.unmap_range(va, pages);
            self.region(protected).put(va, pages);
            return Err(Error::Full {
                cap: MAX_PLACED as u32,
            });
        }
        Ok(va)
    }

    /// Haalt plek `i` weg en geeft de ruimte terug.
    fn unplace(&mut self, i: usize) {
        if let Some(p) = self.placed.swap_remove(i) {
            self.mmu.unmap_range(p.va, p.pages);
            self.region(p.protected).put(p.va, p.pages);
        }
    }

    /// Ruimt de plekken op die nergens meer bij het ijzer liggen.
    fn evict(&mut self, protected: bool) {
        let mut i = 0;
        while i < self.placed.len() {
            let gone = self
                .placed
                .get(i)
                .is_some_and(|p| !p.live && p.protected == protected);
            if gone {
                self.unplace(i);
            } else {
                i += 1;
            }
        }
    }

    pub(crate) fn region(&mut self, protected: bool) -> &mut VaRegion {
        if protected {
            &mut self.va_prot
        } else {
            &mut self.va_frame
        }
    }

    /// Dragen de buffers aan deze kant bitstream in plaats van pixels? Bij
    /// decode de invoer, bij encode de uitvoer; en dát bepaalt de regio. De
    /// firmware controleert dat: pixels in de protected-regio negeert hij
    /// zonder een woord.
    fn carries_bitstream(&self, input: bool) -> bool {
        (self.cfg.dir == Direction::Decode) == input
    }

    /// Bouwt een `mve_buffer_frame`. Bij INVOER (encode) zegt visible_* hoe
    /// groot het beeld is; bij UITVOER (decode) blijven die velden NUL, want
    /// de decoder bepaalt wat zichtbaar wordt. max_frame_* is de CAPACITEIT
    /// van de buffer, niet de beeldmaat.
    pub(crate) fn frame_desc(&self, h: &Held, f: Flags, input: bool) -> [u8; BF_SIZE] {
        let mut d = [0u8; BF_SIZE];
        put(&mut d, BF_HOST_HANDLE, &h.id.to_le_bytes());
        put(&mut d, BF_USER_TAG, &h.tag.to_le_bytes());
        let mut flags = 0u32;
        if f.has(Flags::EOS) {
            flags |= FR_FLAG_EOS;
        }
        if f.has(Flags::KEY_FRAME) {
            flags |= FR_FLAG_FORCE_IDR;
        }
        put(&mut d, BF_FLAGS, &flags.to_le_bytes());
        let (w, hgt) = self.frame_geometry();
        let stride = self.stride(w);
        if input {
            put(&mut d, BF_VISIBLE_WIDTH, &(w as u16).to_le_bytes());
            put(&mut d, BF_VISIBLE_HEIGHT, &(hgt as u16).to_le_bytes());
        }
        put(&mut d, BF_FORMAT, &self.pixel.to_le_bytes());
        // Hoeveel regels passen er werkelijk in het lumavlak? Bij 4:2:0 kost
        // een regel anderhalve stride (chroma erbij), bij Y8 precies één.
        let per_row = if self.cfg.pixel == Pixel::Y8 {
            u64::from(stride)
        } else {
            u64::from(stride) * 3 / 2
        };
        let mut rows = hgt;
        if per_row > 0 {
            let n = h.buf.size / per_row;
            if n > 0 && n < u64::from(rows) {
                rows = n as u32;
            }
        }
        put(&mut d, BF_MAX_WIDTH, &(w as u16).to_le_bytes());
        put(&mut d, BF_MAX_HEIGHT, &(rows as u16).to_le_bytes());
        // Y op het begin, chroma erachter; I420 splitst chroma in tweeën.
        let luma = stride.wrapping_mul(rows);
        put(&mut d, BF_PLANE_TOP, &h.va.to_le_bytes());
        put(&mut d, BF_STRIDE, &stride.to_le_bytes());
        match self.cfg.pixel {
            Pixel::I420 => {
                put(
                    &mut d,
                    BF_PLANE_TOP + 4,
                    &h.va.wrapping_add(luma).to_le_bytes(),
                );
                put(
                    &mut d,
                    BF_PLANE_TOP + 8,
                    &h.va.wrapping_add(luma + luma / 4).to_le_bytes(),
                );
                put(&mut d, BF_STRIDE + 4, &(stride / 2).to_le_bytes());
                put(&mut d, BF_STRIDE + 8, &(stride / 2).to_le_bytes());
            }
            Pixel::Y8 => {}
            _ => {
                put(
                    &mut d,
                    BF_PLANE_TOP + 4,
                    &h.va.wrapping_add(luma).to_le_bytes(),
                );
                put(&mut d, BF_STRIDE + 4, &stride.to_le_bytes());
            }
        }
        d
    }

    /// Bouwt een `mve_buffer_bitstream`.
    pub(crate) fn bitstream_desc(&self, h: &Held, filled: u64, f: Flags) -> [u8; BS_SIZE] {
        let mut d = [0u8; BS_SIZE];
        put(&mut d, BS_HOST_HANDLE, &h.id.to_le_bytes());
        put(&mut d, BS_USER_TAG, &h.tag.to_le_bytes());
        let mut flags = 0u32;
        if f.has(Flags::EOS) {
            flags |= BS_FLAG_EOS;
        }
        if f.has(Flags::HEADERS) {
            flags |= BS_FLAG_CODEC_CONFIG;
        }
        if filled > 0 {
            flags |= BS_FLAG_END_OF_FRAME;
        }
        put(&mut d, BS_FLAGS, &flags.to_le_bytes());
        put(
            &mut d,
            BS_ALLOC_BYTES,
            &h.pages.wrapping_mul(PAGE as u32).to_le_bytes(),
        );
        put(&mut d, BS_OFFSET, &0u32.to_le_bytes());
        put(&mut d, BS_FILLED_LEN, &(filled as u32).to_le_bytes());
        put(&mut d, BS_BUF_ADDR, &h.va.to_le_bytes());
        d
    }

    /// De maat waarop buffers beschreven worden: wat de firmware vroeg zodra
    /// hij de stream kent, anders wat de aanroeper opgaf.
    pub(crate) fn frame_geometry(&self) -> (u32, u32) {
        if self.alloc.known {
            (u32::from(self.alloc.width), u32::from(self.alloc.height))
        } else {
            (self.cfg.width, self.cfg.height)
        }
    }

    /// De regelafstand in bytes.
    pub(crate) fn stride(&self, width: u32) -> u32 {
        if self.cfg.pixel == Pixel::P010 {
            width * 2
        } else {
            width
        }
    }

    /// Zet een event klaar; vol wordt door de pomp voorkomen.
    pub(crate) fn post(&mut self, e: Event) {
        let _ = self.events.push(e);
    }

    /// Markeert de sessie als verloren: één Fault, en daarna neemt ze niets
    /// meer aan. Doorgaan op een firmware die zijn staat kwijt is levert
    /// alleen stille beeldfouten op.
    pub(crate) fn fail(&mut self, e: Error) {
        if self.failed.is_some() {
            return;
        }
        self.failed = Some(e);
        if !self.events.is_full() {
            let mut ev = Event::of(Kind::Fault);
            ev.fault = Some(e);
            self.post(ev);
            self.fault_posted = true;
        }
    }

    /// Hoeveel pagina's een bytegrootte van de firmware kost, in 64 bits:
    /// vlak onder 4 GB loopt de optelling in 32 bits over naar bijna niets.
    pub(crate) fn pages_for(size: u32) -> u32 {
        (u64::from(size).div_ceil(PAGE)) as u32
    }

    /// De grootte van één pagina in bits, voor de uitlijning.
    pub(crate) const PAGE_SHIFT: u8 = PAGE_SHIFT as u8;
}

/// De wachttijd op TERMINATE: tien milliseconden.
pub(crate) const TERMINATE_NS: u64 = 10_000_000;
