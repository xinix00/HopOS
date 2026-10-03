//! Van "gereset" naar "draaiend": de datastructuren die xHCI verplicht in
//! DRAM wil zien (device-context-array, scratchpad, command ring, event
//! ring), en daarna de event-pomp waar al het antwoordverkeer doorheen komt.

use crate::device::DevState;
use crate::ring::{CC_SUCCESS, EvRing, Event, Ring, TRB_CMD_COMP_EVT, TRB_LEN, TRB_TRANSFER_EVT};
use crate::{CMD_RUN, ERDP_EHB, Error, Hc, POLL_STEP_NS, Poison, Result, STS_HCH, Timer};
use dev::{Pa, Reg};

/// Hoeveel apparaten we tegelijk geadresseerd kunnen hebben. De controller
/// kan er meestal 32 of 64; wij zetten CONFIG op hoogstens dit getal en
/// leggen per slot een vaste set structuren aan.
///
/// Waarom een cap en niet MaxSlots: de per-slot structuren worden ÉÉN keer
/// aangelegd en daarna hergebruikt, zodat in- en uitpluggen niets lekt. Dat
/// vooraf doen voor 64 slots is 1,3MB voor een node met één toetsenbord. 8
/// is ruim voor wat er fysiek in een Pi of een Radxa past: dit is een
/// invoerapparaat-pad, geen USB-hub-farm.
pub const MAX_DEVICES: usize = 8;

/// De bovengrens die we aan de scratchpad-eis van de controller stellen.
/// Echte hardware vraagt er een handvol; een controller die er honderden
/// vraagt is een verkeerd gelezen register en geen apparaat dat we willen
/// bedienen: dan liever hier stoppen dan de DMA-regio stil oplopen.
pub const SCRATCH_MAX: u32 = 64;

/// De diepte van de event-wachtrij. Ruim: er staan er hooguit een paar
/// tegelijk (één commando plus één interrupt-transfer per apparaat), en
/// alleen een apparaat dat niemand meer ophaalt vult hem.
pub const PENDING_CAP: usize = 64;

/// De bulk-bouncebuffer (bulk.rs): minstens 8KB om een SCSI-antwoord plus
/// een paar sectoren te dragen.
pub const BULK_BUF_MIN: u64 = 8 << 10;

/// GEMETEN 22-09: een Blu-ray door de WebDAV-share haalde met 64KB per
/// opdracht 3,6 MB/s, en dat is ongeveer wat de drive op zijn laagste
/// toerental levert: hij gaat pas sneller draaien als de host in grotere
/// happen leest. Dus zo groot als het venster toelaat, tot een kwart MB;
/// daarboven wint een optische drive niets meer terug.
pub const BULK_BUF_MAX: u64 = 256 << 10;

/// Hoe lang een commando mag duren.
const COMMAND_TIMEOUT_NS: u64 = 1_000_000_000;

/// De bump-allocator over de DMA-regio van het board. Geen free: alles wat
/// hier uitkomt leeft zo lang de node leeft (zie [`MAX_DEVICES`]).
///
/// ALLES WORDT OP PAGINAGRENS UITGEDEELD, en dat is geen luiheid. xHCI stelt
/// per structuur twee eisen (tabel 6-1): een alignment én een BOUNDARY die
/// hij niet mag kruisen. Die tweede is de stille: een ringsegment van 4KB
/// op een 64-byte-grens voldoet aan de alignment en kruist tóch een
/// 64KB-grens zodra het toevallig hoog in een blok valt. De controller loopt
/// dan van de ring af. Eén regel alignment koopt die hele klasse fouten af,
/// en dat kost hier ruimte die we hebben.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Arena {
    pub(crate) cur: u64,
    pub(crate) end: u64,
}

impl Arena {
    /// Wat er nog vrij is.
    pub(crate) fn left(&self) -> u64 {
        self.end.saturating_sub(self.cur)
    }

    /// Deelt `n` bytes uit op een veelvoud van `align` (een macht van twee)
    /// en wist ze.
    pub(crate) fn alloc(&mut self, n: u64, align: u64) -> Result<Pa> {
        let full = Error::DmaFull {
            want: n,
            left: self.left(),
        };
        if align == 0 || !align.is_power_of_two() {
            return Err(full);
        }
        let Some(p) = self.cur.checked_add(align - 1).map(|v| v & !(align - 1)) else {
            return Err(full);
        };
        if p > self.end || n > self.end - p {
            return Err(full);
        }
        self.cur = p + n;
        dev::clear(Pa(p), n as usize);
        Ok(Pa(p))
    }
}

/// Welke ring van een slot.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RingId {
    /// EP0, control transfers.
    Ctrl,
    /// Eén per boot-interface, of de twee bulk-endpoints.
    Intr(usize),
}

/// De vaste set structuren van één device-slot. Vooraf aangelegd,
/// hergebruikt bij herplug.
pub(crate) struct SlotRes {
    /// Door de controller beschreven device context.
    pub(crate) dev_ctx: Pa,
    /// Input context: wat wij de controller vertellen.
    pub(crate) in_ctx: Pa,
    pub(crate) ctrl: Ring,
    /// Eén per boot-interface: toetsenbord én muis kunnen op ÉÉN apparaat
    /// zitten (een draadloze combo op één dongle). Een opslagapparaat
    /// gebruikt ze als bulk-IN en bulk-OUT.
    pub(crate) intr: [Ring; crate::MAX_HID_IFACES],
    /// 4KB werkgeheugen (zie `BUF_CTRL`/`BUF_INTR`).
    pub(crate) buf: Pa,
    /// Enable Slot bevestigd, Disable Slot nog niet bevestigd.
    pub(crate) in_use: bool,
    /// Disable Slot faalde: ownership onbekend, reset vereist.
    pub(crate) quarantined: bool,
    /// Het apparaat op dit slot, zolang het geadresseerd is.
    pub(crate) dev: Option<DevState>,
}

impl SlotRes {
    pub(crate) fn ring(&mut self, id: RingId) -> Option<&mut Ring> {
        match id {
            RingId::Ctrl => Some(&mut self.ctrl),
            RingId::Intr(k) => self.intr.get_mut(k),
        }
    }
}

impl Hc {
    /// Bouwt de datastructuren op in de DMA-regio `[dma, dma+size)` en zet
    /// de controller aan. De regio moet buiten élke RAM-declaratie liggen
    /// (de USB-regio uit het layout-plan): dan is hij device-gemapt en dus
    /// coherent met de controller zonder cache-onderhoud, dezelfde eis als
    /// de NIC-ringen.
    ///
    /// Volgorde is dwingend (xHCI 4.2): eerst alles in DRAM, dan de pointers
    /// in de registers, dan pas RUN. Een controller die loopt terwijl DCBAAP
    /// nog naar nul wijst, leest device-contexten op adres 0.
    pub async fn start(&mut self, dma: Pa, size: u64, t: &impl Timer) -> Result {
        if !self.probed {
            return Err(Error::NotProbed);
        }
        if let Some(p) = self.poisoned {
            return Err(Error::Poisoned(p));
        }
        if self.running {
            return Err(Error::Running);
        }
        let bad = Error::Dma { base: dma.0, size };
        let Some(end) = dma.0.checked_add(size) else {
            return Err(bad);
        };
        if size == 0 || end.checked_add(self.bus_off).is_none() {
            return Err(bad);
        }
        // Het board-venster is vast voor de levensduur van de node. Na een
        // poisoned Disable Slot doet `recover` HCRST en bouwt exact in dit
        // venster alle tabellen opnieuw op; oude pointers worden pas
        // overschreven nadat de hardware gereset is.
        self.dma_base = dma;
        self.dma_size = size;
        self.arena = Arena { cur: dma.0, end };

        // Paginagrootte van de controller: bit n is 2^(n+12). Vrijwel altijd
        // 4KB, maar de scratchpad-buffers moeten er exact op passen dus we
        // lezen hem.
        let ps = self.opr().pagesize.read() & 0xFFFF;
        if ps == 0 || ps.trailing_zeros() > 4 {
            return Err(Error::PageSize { raw: ps });
        }
        self.page = 1 << (ps.trailing_zeros() + 12);

        // 64-bit adressering: als de controller die niet kan, mag geen enkel
        // adres dat we programmeren boven de 4GB liggen. Alle boards die wij
        // bedienen planten hun USB-regio laag, dus dit is een assertie en
        // geen fallback.
        if !self.ac64 && end + self.bus_off > 1 << 32 {
            return Err(Error::Dma32 {
                end: end + self.bus_off,
            });
        }

        // Alleen direct aangesloten apparaten: één slot per roothub-poort.
        // Extra hardware-slots zijn zonder hub-routing niet te gebruiken.
        self.n_slots = usize::from(self.max_slots)
            .min(usize::from(self.max_ports))
            .min(MAX_DEVICES);
        self.pending.clear();

        // Device Context Base Address Array: DCBAA[0] is de scratchpad, [i]
        // het device context van slot i.
        self.dcbaa = self.arena.alloc((self.n_slots as u64 + 1) * 8, self.page)?;
        self.setup_scratchpad()?;
        self.setup_slots()?;

        // Command ring: één segment van een pagina (255 bruikbare TRB's).
        let cr = self.arena.alloc(self.page, self.page)?;
        let cmd = Ring::new(cr, cr.0 + self.bus_off, self.page);
        self.cmd = Some(cmd);

        // Event ring: één segment plus de ERST die ernaar wijst.
        let er = self.arena.alloc(self.page, self.page)?;
        let erst = self.arena.alloc(64, self.page)?;
        let evt = EvRing::new(er, er.0 + self.bus_off, (self.page / TRB_LEN) as usize);
        dev::write32(erst, evt.bus as u32);
        dev::write32(erst.add(4), (evt.bus >> 32) as u32);
        dev::write32(erst.add(8), evt.n as u32); // segmentgrootte in TRB's
        dev::write32(erst.add(12), 0);
        self.evt = Some(evt);
        self.erst_bus = erst.0 + self.bus_off;

        // Nu de registers. CONFIG eerst: hoeveel slots we gaan gebruiken.
        let o = self.opr();
        o.config.write(self.n_slots as u32);
        o.dcbaap.write(self.dcbaa.0 + self.bus_off);
        // CRCR: het lage dword draagt RCS (Ring Cycle State); dat moet 1
        // zijn, want een nieuwe ring begint met cycle 1.
        o.crcr.write(cmd.bus | 1);

        // De interrupter: ERSTSZ vóór ERSTBA (het schrijven van ERSTBA
        // latcht de tabel), en ERDP ertussen zodat de leespositie klopt
        // vanaf het eerste event. IMAN blijft dicht: wij pollen.
        let ir = self.ir();
        ir.erstsz.write(1);
        ir.erdp.write(evt.bus | ERDP_EHB);
        ir.erstba.write(self.erst_bus);
        ir.imod.write(0);

        self.setup_bulk_buf();

        dev::mb();
        o.usbcmd.update(|v| v | CMD_RUN);
        if self
            .wait(t, |o| o.usbsts.read(), STS_HCH, 0, "run")
            .await
            .is_err()
        {
            return Err(self.quarantine(Poison::RunTimeout));
        }
        self.running = true;
        Ok(())
    }

    /// Wat er na alle vaste structuren over is, wordt de bulk-bouncebuffer:
    /// één per controller, want BOT is per definitie serieel (commando,
    /// data, status). Blijft er te weinig over, dan draagt deze controller
    /// alleen HID en weigert elke bulk-transfer: dit is geen reden om de
    /// controller niet te starten.
    ///
    /// Uitlijnen op 64KB: een TRB-buffer mag die grens niet kruisen, en met
    /// een uitgelijnd begin valt elk stuk van `TRB_MAX` er precies binnen.
    /// Wat er te vragen valt wordt dus geteld vanaf het UITGELIJNDE begin en
    /// niet vanaf de huidige stand: `alloc` rondt zelf omhoog, dus vragen om
    /// precies wat er over is mislukt, en een vaste reserve van 64KB
    /// weggooien kost net zo hard. Dat kostte 22-09 twee flips: eerst geen
    /// buffer, daarna 80KB waar er 144 lag.
    fn setup_bulk_buf(&mut self) {
        const ALIGN: u64 = crate::TRB_MAX as u64;
        self.bulk_buf = Pa(0);
        self.bulk_size = 0;
        let Some(start) = self
            .arena
            .cur
            .checked_add(ALIGN - 1)
            .map(|v| v & !(ALIGN - 1))
        else {
            return;
        };
        if start >= self.arena.end {
            return;
        }
        let want = ((self.arena.end - start) & !(self.page - 1)).min(BULK_BUF_MAX);
        if want >= BULK_BUF_MIN
            && let Ok(buf) = self.arena.alloc(want, ALIGN)
        {
            self.bulk_buf = buf;
            self.bulk_size = want;
        }
    }

    /// Wist alle hardware-slotstate met HCRST, bouwt command-, event- en
    /// device-tabellen opnieuw in het bij [`Hc::start`] bewaarde DMA-venster
    /// en zet de poorten weer aan. De eigenaar moet vóór deze aanroep zijn
    /// oude [`crate::Device`]-handvatten vergeten: HCRST maakt die per
    /// definitie ongeldig (en de generatie per slot bewijst het).
    pub async fn recover(&mut self, t: &impl Timer) -> Result {
        let Some((base, size)) = self.recover_begin()? else {
            return Ok(());
        };
        let r = self.reset(t).await;
        self.recover_reset(r)?;
        let r = self.start(base, size, t).await;
        self.recover_start(r)?;
        self.power_on(t).await;
        self.poisoned = None;
        Ok(())
    }

    /// De eerste helft van het herstel: `None` voor een gezonde controller
    /// (niets te doen), anders het bewaarde DMA-venster. De hardwarestappen
    /// zelf zijn `async` en staan in [`Hc::recover`]; de boekhouding eromheen
    /// is hier en in de twee helften hierna, zodat de host-tests haar zonder
    /// executor toetsen.
    pub(crate) fn recover_begin(&self) -> Result<Option<(Pa, u64)>> {
        if self.poisoned.is_none() {
            return Ok(None);
        }
        if self.dma_size == 0 {
            return Err(Error::Dma {
                base: self.dma_base.0,
                size: 0,
            });
        }
        Ok(Some((self.dma_base, self.dma_size)))
    }

    /// Na de reset van een herstel.
    pub(crate) fn recover_reset(&mut self, r: Result) -> Result {
        if let Err(e) = r {
            self.poisoned = Some(Poison::RecoveryReset);
            return Err(e);
        }
        // Reset heeft de oude poison na bevestigde HCRST gewist; een
        // Start-fout moet hem opnieuw zetten, anders zou de volgende scan een
        // half opgebouwde controller als gezond behandelen.
        self.poisoned = None;
        Ok(())
    }

    /// Na de start van een herstel.
    pub(crate) fn recover_start(&mut self, r: Result) -> Result {
        if let Err(e) = r {
            self.running = false;
            self.poisoned = Some(Poison::RecoveryStart);
            return Err(e);
        }
        Ok(())
    }

    /// Geeft de controller het krabbelgeheugen dat hij in HCSPARAMS2 opeist.
    /// Nul buffers is normaal (dan slaan we het over); DCBAA[0] blijft dan
    /// nul, zoals de spec voorschrijft.
    fn setup_scratchpad(&mut self) -> Result {
        let p2 = self.cap().hcsparams2.read();
        let n = (p2 >> 27 & 0x1F) | ((p2 >> 21 & 0x1F) << 5);
        self.scratch = n;
        if n == 0 {
            return Ok(());
        }
        if n > SCRATCH_MAX {
            return Err(Error::Scratchpad { n, hcsparams2: p2 });
        }
        let arr = self.arena.alloc(u64::from(n) * 8, self.page)?;
        for i in 0..u64::from(n) {
            let b = self.arena.alloc(self.page, self.page)?;
            dev::write64(arr.add(i * 8), b.0 + self.bus_off);
        }
        dev::write64(self.dcbaa, arr.0 + self.bus_off);
        Ok(())
    }

    /// Legt de vaste structuren van alle slots vooraf aan. Contexten zijn 32
    /// of 64 byte per stuk (HCCPARAMS1.CSZ) en een device context heeft er
    /// 32, een input context 33: vandaar het verschil in maat.
    fn setup_slots(&mut self) -> Result {
        let csz: u64 = if self.ctx64 { 64 } else { 32 };
        self.ctx_size = csz;
        self.res = [const { None }; MAX_DEVICES + 1];
        for i in 1..=self.n_slots {
            let dev_ctx = self.arena.alloc(csz * 32, self.page)?;
            let in_ctx = self.arena.alloc(csz * 33, self.page)?;
            let ctrl = self.new_ring()?;
            let intr = [self.new_ring()?, self.new_ring()?];
            let buf = self.arena.alloc(4096, self.page)?;
            if let Some(slot) = self.res.get_mut(i) {
                *slot = Some(SlotRes {
                    dev_ctx,
                    in_ctx,
                    ctrl,
                    intr,
                    buf,
                    in_use: false,
                    quarantined: false,
                    dev: None,
                });
            }
        }
        Ok(())
    }

    fn new_ring(&mut self) -> Result<Ring> {
        let p = self.arena.alloc(self.page, self.page)?;
        Ok(Ring::new(p, p.0 + self.bus_off, self.page))
    }

    /// Wekt de controller voor een ring. Slot 0 target 0 = de command ring;
    /// slot n target d = endpoint met DCI d van dat device.
    pub(crate) fn doorbell(&self, slot: usize, target: u32) {
        let pa = self.db.add(slot as u64 * 4);
        // SAFETY: de doorbell-array ligt op DBOFF binnen het venster van
        // `new` en telt MaxSlots+1 registers; `slot` <= n_slots <= MaxSlots.
        let r: &Reg<u32> = unsafe { dev::regs(pa) };
        r.write(target);
        dev::mb();
    }

    /// Haalt alles wat er in de event-ring klaarstaat op en zet het in de
    /// wachtrij. Dit is de ENIGE plek die de event-ring leest: één
    /// consument, en dat is meteen de reden dat alle wachtfuncties hier
    /// doorheen gaan in plaats van zelf te pollen.
    pub(crate) fn pump(&mut self) {
        let Some(mut evt) = self.evt else {
            return;
        };
        let mut got = false;
        while let Some(ev) = evt.poll() {
            got = true;
            // Alleen wat `take` ooit ophaalt. Een Port Status Change (elke
            // poortreset levert er een) is een wekker en geen antwoord: de
            // poortstaat zelf staat in PORTSC, en die leest de scan. In de
            // rij zou hij blijven staan tot hij er als oudste uit valt.
            if ev.kind != TRB_TRANSFER_EVT && ev.kind != TRB_CMD_COMP_EVT {
                continue;
            }
            if self.pending.is_full() {
                // Overloop kan alleen als niemand meer wacht op wat er
                // binnenkomt (een losgetrokken apparaat waarvan de transfers
                // blijven falen). De oudste laten vallen houdt het pad
                // levend.
                self.pending.remove(0);
            }
            let _ = self.pending.push(ev);
        }
        self.evt = Some(evt);
        if got && self.probed {
            // Onze leespositie publiceren. EHB moet mee als 1
            // (write-1-to-clear), anders blijft de controller denken dat we
            // nog bezig zijn.
            let ir = self.ir();
            ir.erdp.write(evt.deq_bus() | ERDP_EHB);
            dev::mb();
        }
    }

    /// Gooit wachtende transfer-events weg die naar deze ring van dit slot
    /// wijzen. Na een afgebroken transfer kan de controller er nog een
    /// hebben neergezet (de echte completion, of het Stopped-event van Stop
    /// Endpoint) en niemand haalt dat ooit nog op.
    pub(crate) fn drop_ring(&mut self, slot: usize, r: &Ring) {
        self.pending.retain(|ev| {
            !(ev.kind == TRB_TRANSFER_EVT && usize::from(ev.slot) == slot && r.holds(ev.ptr))
        });
    }

    /// Pakt het eerste event uit de wachtrij waarvoor `m` waar is.
    pub(crate) fn take(&mut self, m: impl Fn(&Event) -> bool) -> Option<Event> {
        let i = self.pending.iter().position(m)?;
        self.pending.remove(i)
    }

    /// Pompt tot er een event langskomt dat aan `m` voldoet. Andere events
    /// blijven in de wachtrij staan: een toetsaanslag die binnenkomt terwijl
    /// we op een commando wachten mag niet verdwijnen.
    ///
    /// Een timeout hier is [`Error::EventTimeout`]; wat dat betekent hangt
    /// af van wie wachtte. Een commando zonder antwoord is een controller die
    /// niet meer luistert (quarantaine, zie `command`), een transfer zonder
    /// antwoord is meestal alleen een apparaat dat hapert (die endpoint
    /// resetten).
    pub(crate) async fn wait_event(
        &mut self,
        t: &impl Timer,
        m: impl Fn(&Event) -> bool,
        timeout_ns: u64,
        what: &'static str,
    ) -> Result<Event> {
        let deadline = t.now().saturating_add(timeout_ns);
        loop {
            self.pump();
            if let Some(ev) = self.take(&m) {
                return Ok(ev);
            }
            if t.now() >= deadline {
                let usbsts = if self.probed {
                    self.opr().usbsts.read()
                } else {
                    0
                };
                return Err(Error::EventTimeout { what, usbsts });
            }
            t.sleep(POLL_STEP_NS).await;
        }
    }

    /// Zet één commando op de command ring, belt aan en wacht op het Command
    /// Completion Event dat naar precies dít TRB terugwijst. Sequentieel: er
    /// staat er nooit meer dan één uit.
    pub(crate) async fn command(
        &mut self,
        t: &impl Timer,
        p0: u32,
        p1: u32,
        p2: u32,
        ctrl: u32,
        what: &'static str,
    ) -> Result<Event> {
        if let Some(p) = self.poisoned {
            return Err(Error::Poisoned(p));
        }
        let Some(cmd) = self.cmd.as_mut() else {
            return Err(Error::NotRunning);
        };
        let trb = cmd.push(p0, p1, p2, ctrl);
        self.doorbell(0, 0);
        let ev = match self
            .wait_event(
                t,
                |e| e.kind == TRB_CMD_COMP_EVT && e.ptr == trb,
                COMMAND_TIMEOUT_NS,
                what,
            )
            .await
        {
            Ok(ev) => ev,
            // De command ring is van de controller zelf: zwijgt hij daarop,
            // dan weet niemand meer wat hij nog uitvoert of welke slots hij
            // bezit.
            Err(_) => return Err(self.quarantine(Poison::CommandTimeout { what })),
        };
        if ev.comp != CC_SUCCESS {
            return Err(Error::Rejected {
                what,
                code: ev.comp,
            });
        }
        Ok(ev)
    }

    /// Legt vast dat software niet meer kan bewijzen welke slots de
    /// controller bezit. Vanaf dit moment mag geen nieuwe Enable Slot meer
    /// volgen: alleen HCRST maakt alle hardware-state aantoonbaar leeg
    /// ([`Hc::reset`] wist dit pas nadat HCRST én CNR succesvol zijn
    /// afgerond). De eerste oorzaak blijft staan.
    pub(crate) fn quarantine(&mut self, p: Poison) -> Error {
        Error::Poisoned(*self.poisoned.get_or_insert(p))
    }

    /// Halteert de controller. Alleen nodig bij een herstart van de stack;
    /// de datastructuren blijven staan.
    pub async fn stop(&mut self, t: &impl Timer) {
        if !self.running {
            return;
        }
        self.opr().usbcmd.update(|v| v & !CMD_RUN);
        if self
            .wait(t, |o| o.usbsts.read(), STS_HCH, STS_HCH, "halt")
            .await
            .is_err()
        {
            self.quarantine(Poison::HaltTimeout);
            return;
        }
        self.running = false;
    }
}
