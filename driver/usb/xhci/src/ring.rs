//! De TRB-ringen: de producer-ring (command en transfer) en de event-ring.
//!
//! Dit is de datastructuur waar xHCI helemaal op draait: de ring van
//! 16-byte TRB's (Transfer Request Blocks) met één eigendomsbit. Software en
//! controller lopen allebei rond in dezelfde ring; de CYCLE-bit zegt van wie
//! een plek is. Software schrijft de bit die de controller verwacht: de plek
//! is van de controller. Bij elke omloop klapt de verwachte waarde om, en
//! dát is hoe beide kanten zien waar de ander is zonder een enkele gedeelde
//! teller.
//!
//! De laatste plek van elk segment is een LINK-TRB terug naar het begin, met
//! de TOGGLE-CYCLE-bit. Daarom is de bruikbare capaciteit n-1 en niet n.

use dev::Pa;

// TRB-types (xHCI tabel 6-91). Alleen wat deze driver produceert of
// consumeert.
pub(crate) const TRB_NORMAL: u32 = 1;
pub(crate) const TRB_SETUP: u32 = 2;
pub(crate) const TRB_DATA: u32 = 3;
pub(crate) const TRB_STATUS: u32 = 4;
pub(crate) const TRB_LINK: u32 = 6;
pub(crate) const TRB_ENABLE_SLOT: u32 = 9;
pub(crate) const TRB_DISABLE_SLOT: u32 = 10;
pub(crate) const TRB_ADDRESS_DEV: u32 = 11;
pub(crate) const TRB_CONFIG_EP: u32 = 12;
pub(crate) const TRB_EVAL_CTX: u32 = 13;
pub(crate) const TRB_RESET_EP: u32 = 14;
pub(crate) const TRB_STOP_EP: u32 = 15;
pub(crate) const TRB_SET_TR_DEQ: u32 = 16;
pub(crate) const TRB_TRANSFER_EVT: u32 = 32;
pub(crate) const TRB_CMD_COMP_EVT: u32 = 33;
pub(crate) const TRB_PORT_STAT_EVT: u32 = 34;

// Vlaggen in het derde dword van een TRB (xHCI 6.4.1).
pub(crate) const TRB_CYCLE: u32 = 1 << 0;
/// Alleen op een Link-TRB: toggle cycle.
pub(crate) const TRB_TC: u32 = 1 << 1;
/// Interrupt on short packet.
pub(crate) const TRB_ISP: u32 = 1 << 2;
pub(crate) const TRB_CHAIN: u32 = 1 << 4;
/// Interrupt on completion.
pub(crate) const TRB_IOC: u32 = 1 << 5;
/// Immediate data: het TRB zélf is de payload.
pub(crate) const TRB_IDT: u32 = 1 << 6;
pub(crate) const TRB_TYPE_SHIFT: u32 = 10;

/// De maat van één TRB.
pub(crate) const TRB_LEN: u64 = 16;

// Completion codes (xHCI tabel 6-90). Alleen de codes waar deze driver een
// beslissing op neemt; de rest gaat als getal de fout in.
pub(crate) const CC_SUCCESS: u32 = 1;
pub(crate) const CC_STALL: u32 = 6;
pub(crate) const CC_SHORT_PACKET: u32 = 13;

/// De naam van een completion code, voor de logregel.
#[must_use]
pub fn comp_name(c: u32) -> &'static str {
    match c {
        CC_SUCCESS => "success",
        2 => "data buffer error",
        3 => "babble",
        4 => "USB transaction error",
        5 => "TRB error",
        CC_STALL => "stall",
        7 => "resource error",
        8 => "bandwidth error",
        9 => "no slots available",
        11 => "no ping response",
        CC_SHORT_PACKET => "short packet",
        19 => "missed service error",
        21 => "parameter error",
        _ => "completion code",
    }
}

/// Een producer-ring: wij schrijven, de controller leest. Command rings en
/// transfer rings zijn allebei dit.
#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct Ring {
    /// Fysiek adres van het segment.
    pub(crate) base: Pa,
    /// Wat de CONTROLLER als adres ziet (`base + bus_off`).
    pub(crate) bus: u64,
    /// TRB-plaatsen inclusief de link-TRB op de laatste plek.
    pub(crate) n: usize,
    /// Waar wij het volgende TRB schrijven.
    enq: usize,
    /// De bit die "van de controller" betekent.
    cycle: u32,
}

impl Ring {
    /// Legt een leeg segment aan met zijn link-TRB. `bytes` moet een
    /// veelvoud van 16 zijn en het segment mag geen 64KB-grens kruisen (xHCI
    /// 4.11.5.1); beide gelden vanzelf omdat de arena in pagina's uitdeelt.
    pub(crate) fn new(base: Pa, bus: u64, bytes: u64) -> Self {
        let mut r = Self {
            base,
            bus,
            n: (bytes / TRB_LEN) as usize,
            enq: 0,
            cycle: 1,
        };
        r.reset();
        r
    }

    /// Zet de ring terug op zijn beginstand. Nodig bij hergebruik van een
    /// slot (herplug): de controller begint na een Address Device weer met
    /// cycle 1 op het adres dat wij in het endpoint-context zetten, dus onze
    /// producerkant moet daarmee mee terug.
    pub(crate) fn reset(&mut self) {
        dev::clear(self.base, self.n * TRB_LEN as usize);
        self.enq = 0;
        self.cycle = 1;
        self.arm_link(false);
        dev::mb();
    }

    /// (Her)schrijft de link-TRB met de cycle-bit die de controller NU
    /// verwacht. Wordt bij elke omloop herhaald: de bit klapt om, dus de
    /// link moet mee.
    ///
    /// `mid` zegt dat de omloop MIDDEN in een TD valt. Een link-TRB zonder
    /// chain-bit sluit de TD af (xHCI 4.11.5.1), dus een geketende
    /// overdracht die over de ringgrens heen loopt zou halverwege afgeknipt
    /// worden. Met de bit erin loopt de TD gewoon door aan de andere kant.
    fn arm_link(&self, mid: bool) {
        if self.n < 2 {
            return;
        }
        let mut ctrl = TRB_LINK << TRB_TYPE_SHIFT | TRB_TC | self.cycle;
        if mid {
            ctrl |= TRB_CHAIN;
        }
        let l = self.base.add((self.n as u64 - 1) * TRB_LEN);
        dev::write32(l, self.bus as u32);
        dev::write32(l.add(4), (self.bus >> 32) as u32);
        dev::write32(l.add(8), 0);
        dev::write32(l.add(12), ctrl);
    }

    /// De huidige schrijfpositie als dequeue-pointer mét DCS-bit: precies
    /// wat er in een endpoint-context hoort. De HUIDIGE positie en niet de
    /// basis: een control-ring die al descriptors verstuurd heeft staat niet
    /// meer op nul, en een endpoint-context met de basis erin zou de
    /// controller terug laten lopen over TRB's die al af zijn.
    pub(crate) fn deq_ptr(&self) -> u64 {
        let p = self.bus + self.enq as u64 * TRB_LEN;
        p | u64::from(self.cycle != 0)
    }

    /// Of `ptr` (een bus-adres uit een event) in dit segment valt.
    pub(crate) fn holds(&self, ptr: u64) -> bool {
        ptr >= self.bus && ptr < self.bus + self.n as u64 * TRB_LEN
    }

    /// Schrijft één TRB (één TD) en geeft het BUS-adres ervan terug.
    pub(crate) fn push(&mut self, p0: u32, p1: u32, p2: u32, ctrl: u32) -> u64 {
        self.push_trb(p0, p1, p2, ctrl, false)
    }

    /// Schrijft één TRB, met de wetenschap of er nog een TRB van dezelfde TD
    /// volgt (alleen de bulk-kant ketent). Het bus-adres is waar een
    /// transfer- of command-completion-event straks naar wijst, en dus onze
    /// enige manier om een antwoord aan een vraag te koppelen.
    ///
    /// De cycle-bit gaat als laatste mee in het controlwoord: pas dáármee
    /// draagt het TRB over aan de controller, dus de payload moet er al
    /// staan. Dit geheugen is device-gemapt (nGnRnE), dus de stores landen
    /// in programmavolgorde; de barrière eronder is voor de doorbell die
    /// erop volgt.
    pub(crate) fn push_trb(&mut self, p0: u32, p1: u32, p2: u32, ctrl: u32, more: bool) -> u64 {
        if self.n < 2 {
            return 0;
        }
        let a = self.base.add(self.enq as u64 * TRB_LEN);
        let at = self.bus + self.enq as u64 * TRB_LEN;
        dev::write32(a, p0);
        dev::write32(a.add(4), p1);
        dev::write32(a.add(8), p2);
        dev::write32(a.add(12), ctrl & !TRB_CYCLE | self.cycle);

        self.enq += 1;
        if self.enq == self.n - 1 {
            // De link-TRB krijgt de OUDE cycle (hij is nu van de
            // controller), pas daarna klapt onze verwachting om.
            self.arm_link(more);
            self.enq = 0;
            self.cycle ^= 1;
        }
        dev::mb();
        at
    }
}

/// De consumer-kant: de controller schrijft, wij lezen. Eén segment, geen
/// link-TRB: een event-ring wikkelt op zijn ERST-grens en toggelt daar zelf
/// de cycle.
#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct EvRing {
    pub(crate) base: Pa,
    pub(crate) bus: u64,
    pub(crate) n: usize,
    deq: usize,
    cycle: u32,
}

/// Eén gelezen event-TRB, ontdaan van bitgefrommel.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Event {
    /// `TRB_TRANSFER_EVT`, `TRB_CMD_COMP_EVT` of `TRB_PORT_STAT_EVT`.
    pub(crate) kind: u32,
    /// Transfer/command: het bus-adres van het TRB dat dit veroorzaakte.
    pub(crate) ptr: u64,
    /// Completion code.
    pub(crate) comp: u32,
    /// Transfer: RESTERENDE bytes, niet de overgedragen.
    pub(crate) rem: u32,
    pub(crate) slot: u8,
    /// Alleen bij een port status change event.
    pub(crate) port: u8,
}

impl EvRing {
    pub(crate) fn new(base: Pa, bus: u64, n: usize) -> Self {
        Self {
            base,
            bus,
            n,
            deq: 0,
            cycle: 1,
        }
    }

    /// Leest één event als er een klaarstaat. De cycle-bit is het
    /// eigendomsbit: komt hij niet overeen met wat wij verwachten, dan is
    /// dit nog een oude plek en staat er niets nieuws.
    pub(crate) fn poll(&mut self) -> Option<Event> {
        if self.n == 0 {
            return None;
        }
        let a = self.base.add(self.deq as u64 * TRB_LEN);
        let ctrl = dev::read32(a.add(12));
        if ctrl & TRB_CYCLE != self.cycle {
            return None;
        }
        let p0 = dev::read32(a);
        let p1 = dev::read32(a.add(4));
        let p2 = dev::read32(a.add(8));
        let kind = ctrl >> TRB_TYPE_SHIFT & 0x3F;
        let ev = Event {
            kind,
            ptr: u64::from(p0) & !0xF | u64::from(p1) << 32,
            comp: p2 >> 24,
            rem: p2 & 0xFF_FFFF,
            slot: (ctrl >> 24) as u8,
            // Port Status Change: het poortnummer zit in [31:24] van dword 0.
            port: if kind == TRB_PORT_STAT_EVT {
                (p0 >> 24) as u8
            } else {
                0
            },
        };
        self.deq += 1;
        if self.deq == self.n {
            self.deq = 0;
            self.cycle ^= 1;
        }
        Some(ev)
    }

    /// Onze leespositie als bus-adres, voor ERDP.
    pub(crate) fn deq_bus(&self) -> u64 {
        self.bus + self.deq as u64 * TRB_LEN
    }
}
