//! Bulk-endpoints: de tweede soort apparaat die deze controller bedient,
//! naast boot-HID. Een optische drive (Blu-ray, DVD) meldt zich als mass
//! storage met bulk-only transport: één BULK-OUT en één BULK-IN, en daarover
//! een SCSI/MMC-gesprek. De commando's zelf staan bewust NIET hier maar in
//! de optical-driver: dit bestand levert alleen de twee pijpen.
//!
//! Waarom dat mag in dezelfde driver: een bulk-endpoint is voor de
//! controller eenvoudiger dan een interrupt-endpoint (geen interval, geen
//! ESIT-payload) en hij gebruikt exact dezelfde ring, doorbell en transfer
//! events. De hele winst van de HID-kant (ring-reset, endpoint-herstel, de
//! event-matcher) geldt hier één op één.
//!
//! Eén apparaat is in onze wereld óf HID óf opslag, dus de twee ringen die
//! een slot voor zijn boot-interfaces heeft worden hier hergebruikt. Dat
//! kost geen byte extra DMA.

use crate::device::{ADD_SLOT, DESC_ENDPOINT, DESC_INTERFACE, Device, REQ_SET_CONFIG, byte, walk};
use crate::host::RingId;
use crate::ring::{
    CC_SHORT_PACKET, CC_STALL, CC_SUCCESS, TRB_CHAIN, TRB_CONFIG_EP, TRB_IOC, TRB_ISP, TRB_NORMAL,
    TRB_TRANSFER_EVT, TRB_TYPE_SHIFT,
};
use crate::{Error, Hc, Poison, Result, Timer};

// Mass storage, bulk-only transport (USB Mass Storage Class, Bulk-Only
// Transport 1.0). SubClass 6 is "SCSI transparent command set": een
// optische drive spreekt daarbinnen MMC.
const CLASS_MASS_STORAGE: u8 = 0x08;
const SUBCLASS_SCSI: u8 = 0x06;
const PROTO_BULK_ONLY: u8 = 0x50;

// Endpoint-types in het endpoint-context (xHCI tabel 6-9), bulk-helft.
const EP_TYPE_BULK_OUT: u32 = 2;
const EP_TYPE_BULK_IN: u32 = 6;

// CLEAR_FEATURE(ENDPOINT_HALT) op de endpoint (USB 2.0 §9.4.1).
const REQ_CLEAR_FEATURE: u8 = 1;
const FEAT_ENDPOINT_HALT: u16 = 0;

/// De ring van de bulk-IN-endpoint in het slot.
const IN_RING: RingId = RingId::Intr(0);
/// De ring van de bulk-OUT-endpoint in het slot.
const OUT_RING: RingId = RingId::Intr(1);

/// Hoeveel bytes er in ÉÉN Normal-TRB passen. Het lengteveld is 17 bits, en
/// een TRB-buffer mag bovendien geen 64KB-grens kruisen (xHCI 4.11.7.1).
/// Grotere overdrachten worden dus geketend: alle TRB's op één na krijgen de
/// chain-bit, en alleen de laatste vraagt een event aan.
///
/// GEMETEN 22-09, en het kostte twee flips: met 140KB in één TRB antwoordde
/// de controller niet meer en las de share nul bytes. De 64KB-grens is hard.
pub const TRB_MAX: usize = 64 << 10;

/// De mass-storage-interface van een apparaat: twee endpoints die altijd
/// samen horen.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct BulkIface {
    /// bInterfaceNumber (nodig voor de reset-request van BOT).
    num: u8,
    in_dci: u32,
    out_dci: u32,
    in_mps: u32,
    out_mps: u32,
}

/// Zoekt de bulk-only interface in de configuratiedescriptor.
pub(crate) fn parse_bulk(b: &[u8]) -> Option<BulkIface> {
    let mut cur = false;
    let mut found = BulkIface::default();
    walk(b, |d| match byte(d, 1) {
        DESC_INTERFACE => {
            cur = false;
            // bAlternateSetting 0, en de drie bytes die bulk-only maken.
            if d.len() >= 9
                && byte(d, 3) == 0
                && byte(d, 5) == CLASS_MASS_STORAGE
                && byte(d, 6) == SUBCLASS_SCSI
                && byte(d, 7) == PROTO_BULK_ONLY
            {
                cur = true;
                found = BulkIface {
                    num: byte(d, 2),
                    ..BulkIface::default()
                };
            }
        }
        DESC_ENDPOINT => {
            // bmAttributes[1:0] == 2 = bulk; bit 7 van het adres = IN.
            let addr = byte(d, 2);
            let ep = u32::from(addr & 0xF);
            let mps = (u32::from(byte(d, 4)) | u32::from(byte(d, 5)) << 8) & 0x7FF;
            if !cur || d.len() < 7 || byte(d, 3) & 0x3 != 2 || ep == 0 || mps == 0 {
                return;
            }
            if addr & 0x80 != 0 {
                if found.in_dci == 0 {
                    (found.in_dci, found.in_mps) = (2 * ep + 1, mps); // IN: DCI = 2N+1
                }
            } else if found.out_dci == 0 {
                (found.out_dci, found.out_mps) = (2 * ep, mps); // OUT: DCI = 2N
            }
        }
        _ => {}
    });
    (found.in_dci != 0 && found.out_dci != 0).then_some(found)
}

/// Eén lopende bulk-transfer: een handvat, geen verwijzing. Er loopt er
/// hoogstens één per controller tegelijk, want de bouncebuffer is gedeeld:
/// de eigenaar van de controller (`usbin`) zorgt daarvoor, en ook voor de
/// deadline. Deze laag weet niet hoe lang een drive mag nadenken.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BulkTd {
    dev: Device,
    is_in: bool,
    n: usize,
    dci: u32,
    ring: RingId,
    /// Het laatste TRB van de TD, het enige met IOC.
    trb: u64,
}

impl BulkTd {
    /// Het apparaat.
    #[must_use]
    pub fn device(&self) -> Device {
        self.dev
    }

    /// Of dit een IN-transfer is.
    #[must_use]
    pub fn is_in(&self) -> bool {
        self.is_in
    }
}

impl Hc {
    /// Programmeert de twee bulk-endpoints en zet de configuratie. De
    /// tegenhanger van `configure` voor een opslagapparaat: geen
    /// SET_PROTOCOL, geen armeren, want bulk werkt op verzoek en niet uit
    /// zichzelf.
    pub(crate) async fn configure_bulk(&mut self, slot: usize, t: &impl Timer) -> Result {
        let f = self.dev_ref(slot)?.bulk.ok_or(Error::NoBulk)?;
        let (inp, in_deq, out_deq) = {
            let r = self.res_mut(slot)?;
            let [ri, ro] = &mut r.intr;
            ri.reset();
            ro.reset();
            (r.in_ctx, ri.deq_ptr(), ro.deq_ptr())
        };
        let max_dci = f.in_dci.max(f.out_dci);
        self.build_input(slot, max_dci, ADD_SLOT | 1 << f.in_dci | 1 << f.out_dci)?;
        for (dci, ep_type, mps, deq) in [
            (f.in_dci, EP_TYPE_BULK_IN, f.in_mps, in_deq),
            (f.out_dci, EP_TYPE_BULK_OUT, f.out_mps, out_deq),
        ] {
            let i = dci + 1;
            // DW0 is nul: een bulk-endpoint heeft geen interval, geen mult en
            // geen streams. Verder dezelfde velden als bij interrupt: CErr 3,
            // het type, de pakketgrootte, en de dequeue-pointer met cycle 1.
            dev::write32(self.ctx_dw(inp, i, 0), 0);
            dev::write32(self.ctx_dw(inp, i, 1), 3 << 1 | ep_type << 3 | mps << 16);
            dev::write32(self.ctx_dw(inp, i, 2), deq as u32);
            dev::write32(self.ctx_dw(inp, i, 3), (deq >> 32) as u32);
            // Average TRB Length is een hint voor de scheduler; Max ESIT
            // Payload blijft nul want dat veld is alleen voor periodiek
            // verkeer.
            dev::write32(self.ctx_dw(inp, i, 4), mps);
        }
        dev::mb();
        let p = inp.0 + self.bus_off;
        self.command(
            t,
            p as u32,
            (p >> 32) as u32,
            0,
            TRB_CONFIG_EP << TRB_TYPE_SHIFT | (slot as u32) << 24,
            "configure endpoint",
        )
        .await?;
        let conf_val = self.dev_ref(slot)?.conf_val_of();
        self.control(slot, 0x00, REQ_SET_CONFIG, u16::from(conf_val), 0, 0, t)
            .await?;
        Ok(())
    }

    /// Hoeveel bytes één transfer in één keer kan dragen: de gedeelde
    /// bouncebuffer van deze controller. De aanroeper knipt zijn lees- en
    /// schrijfopdrachten hierop. Nul: deze controller draagt alleen HID.
    #[must_use]
    pub fn max_transfer(&self) -> usize {
        self.bulk_size as usize
    }

    /// Start een OUT-transfer van `data`: de bytes gaan meteen de
    /// bouncebuffer in; wachten doet deze functie niet.
    pub fn start_bulk_out(&mut self, d: &Device, data: &[u8]) -> Result<BulkTd> {
        self.start_bulk(d, false, data.len(), data)
    }

    /// Start een IN-transfer van hoogstens `len` bytes; [`Hc::poll_bulk`]
    /// kopieert ze naar de aanroeper zodra hij klaar is.
    pub fn start_bulk_in(&mut self, d: &Device, len: usize) -> Result<BulkTd> {
        self.start_bulk(d, true, len, &[])
    }

    fn start_bulk(&mut self, d: &Device, is_in: bool, n: usize, data: &[u8]) -> Result<BulkTd> {
        let slot = self.live(d)?;
        let f = self.dev_ref(slot)?.bulk.ok_or(Error::NoBulk)?;
        if let Some(p) = self.poisoned {
            return Err(Error::Poisoned(p));
        }
        if n == 0 {
            return Err(Error::EmptyTransfer);
        }
        let max = self.max_transfer();
        if n > max {
            return Err(Error::TooLarge { len: n, max });
        }
        let (dci, ring, mps) = if is_in {
            (f.in_dci, IN_RING, f.in_mps)
        } else {
            (f.out_dci, OUT_RING, f.out_mps)
        };
        if !is_in {
            dev::copy_in(self.bulk_buf, data);
            dev::push(self.bulk_buf, n);
        }
        let base = self.bulk_buf.0 + self.bus_off;
        let r = self.res_mut(slot)?.ring(ring).ok_or(Error::Detached)?;
        let mut trb = 0;
        let mut off = 0usize;
        while off < n {
            let len = (n - off).min(TRB_MAX);
            let last = off + len >= n;
            let flags =
                TRB_NORMAL << TRB_TYPE_SHIFT | if last { TRB_ISP | TRB_IOC } else { TRB_CHAIN };
            // TD Size: hoeveel pakketten er ná dit TRB nog komen, afgetopt op
            // 31. De controller plant zijn bursts ermee; op het laatste TRB
            // is hij nul (xHCI 4.11.2.4, dezelfde rekensom als
            // xhci_td_remainder in Linux).
            let tds = if last || mps == 0 {
                0
            } else {
                ((n - off - len).div_ceil(mps as usize)).min(31) as u32
            };
            let bus = base + off as u64;
            trb = r.push_trb(
                bus as u32,
                (bus >> 32) as u32,
                len as u32 | tds << 17,
                flags,
                !last,
            );
            off += len;
        }
        self.doorbell(slot, dci);
        Ok(BulkTd {
            dev: *d,
            is_in,
            n,
            dci,
            ring,
            trb,
        })
    }

    /// Kijkt of de transfer klaar is. `Ok(None)`: nog onderweg, vraag het
    /// straks weer. `Ok(Some(n))`: wat er werkelijk overkwam, bij IN
    /// gekopieerd naar `dst`; een korte IN is normaal en geen fout, de drive
    /// mag minder geven dan gevraagd. Wacht alleen bij een stall (het
    /// vrijmaken van de endpoint).
    pub async fn poll_bulk(
        &mut self,
        td: &BulkTd,
        dst: &mut [u8],
        t: &impl Timer,
    ) -> Result<Option<usize>> {
        let slot = self.live(&td.dev)?;
        self.pump();
        let Some(ev) = self.take(|e| e.kind == TRB_TRANSFER_EVT && e.ptr == td.trb) else {
            // Deze controller moet eerst gereset worden; daar komt deze TD
            // nooit meer uit.
            return match self.poisoned {
                Some(p) => Err(Error::Poisoned(p)),
                None => Ok(None),
            };
        };
        match ev.comp {
            CC_SUCCESS | CC_SHORT_PACKET => {
                if ev.rem as usize > td.n {
                    return Err(self.quarantine(Poison::Overrun {
                        what: "bulk",
                        rem: ev.rem,
                    }));
                }
                let got = td.n - ev.rem as usize;
                if td.is_in && got > 0 {
                    dev::pull(self.bulk_buf, got);
                    let n = got.min(dst.len());
                    if let Some(d) = dst.get_mut(..n) {
                        dev::copy_out(d, self.bulk_buf);
                    }
                }
                Ok(Some(got))
            }
            CC_STALL => {
                // Een gestalde bulk-endpoint is bij mass storage geen ongeluk
                // maar een gespreksvorm: de drive stalt de datafase als hij
                // een commando niet aankan en verwacht dat de host de
                // endpoint vrijmaakt en de status ophaalt. Dus herstellen en
                // de aanroeper laten beslissen.
                self.clear_halt(slot, td.dci, td.ring, td.is_in, t).await?;
                Err(Error::Stalled)
            }
            code => Err(Error::Transfer { what: "bulk", code }),
        }
    }

    /// Breekt een transfer af die zijn deadline haalde: die ene endpoint
    /// stoppen en op een verse ring zetten, zoals Linux een verlopen URB
    /// annuleert. De controller blijft gewoon draaien: dat een drive niet
    /// antwoordt zegt niets over de bus, en het toetsenbord ernaast heeft er
    /// geen last van. De BOT-laag doet daarna zelf zijn reset.
    pub async fn abort_bulk(&mut self, td: &BulkTd, t: &impl Timer) -> Result {
        let slot = self.live(&td.dev)?;
        self.reset_ep(slot, td.dci, td.ring, t).await
    }

    /// Haalt een endpoint uit halted aan beide kanten: eerst de controller
    /// (`reset_ep`), dan het apparaat zelf (CLEAR_FEATURE op de endpoint, dat
    /// ook zijn data-toggle terugzet). Die volgorde is die van de USB-spec en
    /// van Linux.
    async fn clear_halt(
        &mut self,
        slot: usize,
        dci: u32,
        ring: RingId,
        is_in: bool,
        t: &impl Timer,
    ) -> Result {
        self.reset_ep(slot, dci, ring, t).await?;
        let ep = (dci / 2) as u16 | if is_in { 0x80 } else { 0 };
        self.control(slot, 0x02, REQ_CLEAR_FEATURE, FEAT_ENDPOINT_HALT, ep, 0, t)
            .await?;
        Ok(())
    }

    /// De mass-storage-reset van BOT: het apparaat gooit zijn
    /// commandostaat weg en beide endpoints komen uit halted. De uitweg als
    /// host en drive het spoor bijster zijn.
    pub async fn reset_recovery(&mut self, d: &Device, t: &impl Timer) -> Result {
        let slot = self.live(d)?;
        let f = self.dev_ref(slot)?.bulk.ok_or(Error::NoBulk)?;
        // Class-specific request 0xFF op de interface (BOT 1.0 §3.1).
        self.control(slot, 0x21, 0xFF, 0, u16::from(f.num), 0, t)
            .await?;
        self.clear_halt(slot, f.in_dci, IN_RING, true, t).await?;
        self.clear_halt(slot, f.out_dci, OUT_RING, false, t).await
    }
}
