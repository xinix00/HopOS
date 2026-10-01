//! De weg van "er hangt iets aan poort 3" naar "dit is een toetsenbord en
//! zijn rapporten komen binnen". In USB-termen: poortreset, slot toewijzen,
//! adresseren, descriptors lezen, de interrupt-endpoints configureren en ze
//! armeren.
//!
//! Wat we NIET doen: HID-report-descriptors parsen. Het boot-protocol (USB
//! HID 1.11 §B) legt de rapportvorm vast (8 bytes toetsenbord, 3 bytes
//! muis) en elk apparaat dat bInterfaceSubClass 1 meldt, moet het spreken.
//! Dat is precies de reden dat het bestaat: een BIOS moest kunnen typen
//! zonder parser. Wij zijn in dezelfde positie.

use crate::bulk::{BulkIface, parse_bulk};
use crate::host::{RingId, SlotRes};
use crate::ring::{
    CC_SHORT_PACKET, CC_STALL, CC_SUCCESS, Event, TRB_ADDRESS_DEV, TRB_CONFIG_EP, TRB_DATA,
    TRB_DISABLE_SLOT, TRB_ENABLE_SLOT, TRB_EVAL_CTX, TRB_IDT, TRB_IOC, TRB_ISP, TRB_NORMAL,
    TRB_RESET_EP, TRB_SET_TR_DEQ, TRB_SETUP, TRB_STATUS, TRB_STOP_EP, TRB_TRANSFER_EVT,
    TRB_TYPE_SHIFT,
};
use crate::{
    Error, Hc, POLL_STEP_NS, PSC_CCS, PSC_PED, PSC_PR, PSC_PRC, PSC_SPEED_MASK, PSC_SPEED_SHIFT,
    Poison, Result, Speed, Timer,
};
use bounded::BoundedVec;
use core::fmt;
use dev::Pa;

// USB-standaardrequests en descriptortypes (USB 2.0 §9.4, tabel 9-5).
pub(crate) const REQ_GET_DESCRIPTOR: u8 = 6;
pub(crate) const REQ_SET_CONFIG: u8 = 9;
const DESC_DEVICE: u16 = 1;
const DESC_CONFIG: u16 = 2;
pub(crate) const DESC_INTERFACE: u8 = 4;
pub(crate) const DESC_ENDPOINT: u8 = 5;

// HID-class requests (HID 1.11 §7.2), op de interface.
const HID_SET_IDLE: u8 = 0x0A;
const HID_SET_PROTOCOL: u8 = 0x0B;

// bInterfaceClass/SubClass van een boot-HID-apparaat.
const CLASS_HID: u8 = 3;
const SUBCLASS_BOOT: u8 = 1;

/// bInterfaceProtocol: geen boot-HID (of geen interrupt-IN-endpoint).
pub const PROTO_NONE: u8 = 0;
/// bInterfaceProtocol van een boot-toetsenbord (HID 1.11 §4.3). Publiek
/// omdat de aanroeper moet weten welke decoder bij een rapport hoort.
pub const PROTO_KEYBOARD: u8 = 1;
/// bInterfaceProtocol van een boot-muis.
pub const PROTO_MOUSE: u8 = 2;

// Endpoint-types in het endpoint-context (xHCI tabel 6-9).
const EP_TYPE_CONTROL: u32 = 4;
const EP_TYPE_INTR_IN: u32 = 7;

// Add-flags van het input control context (xHCI 6.2.5.1). A0 = slot
// context, A1 = de default control endpoint, An = DCI n.
pub(crate) const ADD_SLOT: u32 = 1 << 0;
const ADD_EP0: u32 = 1 << 1;

// Endpoint-states in dword 0 van een endpoint context (xHCI 6.2.3).
const EP_RUNNING: u32 = 1;
const EP_HALTED: u32 = 2;

/// Hoeveel boot-interfaces we van één apparaat bedienen: één toetsenbord en
/// één muis. Meer bestaat niet in het boot-protocol (dat kent precies die
/// twee rollen), dus dit is de vorm van het probleem en geen willekeurige
/// grens.
pub const MAX_HID_IFACES: usize = 2;

// Verdeling van de 4KB werkbuffer per slot. Control-data en het HID-rapport
// mogen elkaar niet raken: de interrupt-endpoint staat ARMED terwijl wij een
// control transfer doen, dus de controller kan op elk moment in het
// rapportvenster schrijven.
const BUF_CTRL: u64 = 0;
/// Een config-descriptor van een HID-apparaat is < 100 bytes.
pub(crate) const BUF_CTRL_SIZE: usize = 1024;
/// Eerste interface; de volgende op `+BUF_INTR_SIZE`.
const BUF_INTR: u64 = 2048;
const BUF_INTR_SIZE: usize = 64;

/// GEMETEN op een Pi 5 (06-08): 750ms was net niet genoeg voor een Logi
/// Bolt-ontvanger (PORTSC 0x6f1, dus PR nog hoog en PLS op Polling). De scan
/// probeerde het een halve seconde later opnieuw en toen lukte het wél, dus
/// het was een trage poort en geen kapotte. USB2 wil ~50ms; wie meer nodig
/// heeft is een hub of een dongle die zich eerst zelf moet aanmelden.
const PORT_RESET_TIMEOUT_NS: u64 = 2_000_000_000;

/// Hoe lang één fase van een control transfer mag duren.
const CONTROL_TIMEOUT_NS: u64 = 2_000_000_000;

/// Eén boot-HID-interface van een apparaat, met de interrupt-IN endpoint
/// die erbij hoort.
#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct HidIface {
    /// bInterfaceNumber (nodig voor SET_PROTOCOL/SET_IDLE).
    num: u8,
    proto: u8,
    dci: u32,
    mps: u32,
    interval: u8,
    /// Waar in de slotbuffer zijn rapporten landen.
    buf_off: u64,
    armed: bool,
    arm_trb: u64,
}

/// De uitkomst van een Enable Slot-claim.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Claim {
    /// Het slot valt in CONFIG en is nu van dit apparaat.
    Owned,
    /// Het slot valt buiten CONFIG: Disable Slot erop, en dan
    /// [`Hc::stray_released`].
    Stray(usize),
}

/// Wat de controller van één geadresseerd apparaat weet.
#[derive(Clone, Debug)]
pub(crate) struct DevState {
    pub(crate) generation: u32,
    pub(crate) port: u8,
    pub(crate) speed: Speed,
    pub(crate) vendor: u16,
    pub(crate) product: u16,
    mps0: u32,
    conf_val: u8,
    /// MEER DAN ÉÉN, EN DAT IS DE NORMAAL. Een draadloze combo hangt aan één
    /// dongle die zich als ÉÉN apparaat meldt met twee boot-interfaces:
    /// nummer 0 het toetsenbord, nummer 1 de muis. De Go-driver bond eerst
    /// alleen de eerste, tot er op 06-08 een Logi Bolt (046d:c548) in een Pi
    /// 5 ging: toetsenbord gevonden, muis stil. De regel is dus niet "één
    /// interface per apparaat" maar "één endpoint per rol".
    ifaces: BoundedVec<HidIface, MAX_HID_IFACES>,
    pub(crate) bulk: Option<BulkIface>,
    pub(crate) last_err: Option<Error>,
    /// Per interface-index: SET_PROTOCOL(boot) werd geweigerd en we nemen
    /// aan dat het apparaat al boot spreekt.
    boot_refused: u8,
}

impl DevState {
    pub(crate) fn conf_val_of(&self) -> u8 {
        self.conf_val
    }
}

/// Eén geadresseerd USB-apparaat: een handvat, geen verwijzing. De staat
/// zelf is van de [`Hc`]; wie iets wil, geeft dit handvat aan de eigenaar.
/// Een handvat van vóór een herplug of een controllerherstel draagt een oude
/// generatie en wordt geweigerd met [`Error::Detached`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Device {
    /// Het hardware-slot.
    pub slot: u8,
    /// De roothub-poort (1-gebaseerd).
    pub port: u8,
    /// De snelheid van de poort.
    pub speed: Speed,
    /// idVendor.
    pub vendor_id: u16,
    /// idProduct.
    pub product_id: u16,
    pub(crate) generation: u32,
    pub(crate) protos: [u8; MAX_HID_IFACES],
    pub(crate) n_protos: u8,
    pub(crate) boot_refused: u8,
    pub(crate) mass_storage: bool,
}

impl Device {
    /// De rollen die dit apparaat levert ([`PROTO_KEYBOARD`],
    /// [`PROTO_MOUSE`]).
    #[must_use]
    pub fn protos(&self) -> &[u8] {
        self.protos
            .get(..usize::from(self.n_protos))
            .unwrap_or_default()
    }

    /// Of dit een bulk-only mass-storage-apparaat is.
    #[must_use]
    pub fn is_mass_storage(&self) -> bool {
        self.mass_storage
    }

    /// Of SET_PROTOCOL(boot) op interface-index `i` geweigerd werd. Geen
    /// fout: sommige apparaten stallen hem als ze maar één protocol kennen,
    /// en dan spreken ze al boot. Eén logregel waard, niet meer.
    #[must_use]
    pub fn boot_assumed(&self, i: usize) -> bool {
        i < MAX_HID_IFACES && self.boot_refused & (1 << i) != 0
    }
}

impl fmt::Display for Device {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.mass_storage {
            f.write_str("mass storage")?;
        } else if self.n_protos == 0 {
            f.write_str("HID device")?;
        }
        for (i, &p) in self.protos().iter().enumerate() {
            if i > 0 {
                f.write_str("+")?;
            }
            f.write_str(match p {
                PROTO_KEYBOARD => "keyboard",
                PROTO_MOUSE => "mouse",
                _ => "HID",
            })?;
        }
        write!(
            f,
            " {:04x}:{:04x} on port {} ({}, slot {})",
            self.vendor_id, self.product_id, self.port, self.speed, self.slot
        )
    }
}

/// Eén binnengekomen HID-rapport.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Report {
    /// Hoeveel bytes er in de buffer van de aanroeper staan.
    pub len: usize,
    /// Van welke rol het kwam: een combo-dongle levert toetsenbord én muis
    /// op hetzelfde slot, dus de aanroeper moet weten welke decoder erbij
    /// hoort.
    pub proto: u8,
}

/// De control-endpoint-pakketgrootte waarmee we beginnen. Bij low-speed is 8
/// de enige toegestane waarde en bij high/super ligt hij vast; bij
/// FULL-speed mag het 8, 16, 32 of 64 zijn en weten we het pas na de eerste
/// acht bytes van de device-descriptor. Daarom beginnen we daar met 8 (de
/// enige waarde die gegarandeerd werkt) en corrigeren we hem daarna.
fn default_mps0(s: Speed) -> u32 {
    match s {
        Speed::HIGH => 64,
        Speed::SUPER => 512,
        _ => 8,
    }
}

/// SuperSpeed codeert bMaxPacketSize0 als exponent; USB2 in bytes.
pub(crate) fn descriptor_mps0(speed: Speed, value: u8) -> Result<u32> {
    let ok = match speed {
        Speed::SUPER => (value == 9).then_some(512),
        Speed::HIGH => (value == 64).then_some(64),
        Speed::LOW => (value == 8).then_some(8),
        Speed::FULL => matches!(value, 8 | 16 | 32 | 64).then_some(u32::from(value)),
        _ => None,
    };
    ok.ok_or(Error::Mps0 { speed, value })
}

/// Zet bInterval om naar het exponentveld van het endpoint context. xHCI
/// telt in 125µs-microframes als macht van twee; USB telt bij low/full-speed
/// in hele frames (1ms) en bij high-speed al in machten van twee. De
/// omzetting en de grenzen zijn die van Linux (`xhci_get_endpoint_interval`).
pub(crate) fn interval_exponent(sp: Speed, b_interval: u8) -> u32 {
    let b = u32::from(b_interval.max(1));
    match sp {
        Speed::HIGH | Speed::SUPER => (b - 1).min(15),
        // Low/full-speed: bInterval frames = bInterval*8 microframes, naar
        // beneden afgerond op een macht van twee, en geklemd op [3,10]
        // (1ms..128ms).
        _ => (b * 8).ilog2().clamp(3, 10),
    }
}

/// Hoeveel we per beurt vragen: de pakketgrootte, begrensd door de ruimte
/// die dit slot per interface heeft.
fn report_len(mps: u32) -> usize {
    (mps as usize).min(BUF_INTR_SIZE)
}

/// Byte `k` van een descriptor, of 0 als hij er niet is.
pub(crate) fn byte(d: &[u8], k: usize) -> u8 {
    d.get(k).copied().unwrap_or(0)
}

/// Loopt de descriptorketen af en roept `f` voor elke descriptor aan.
/// Stopt bij een descriptor die korter dan 2 is of buiten de keten valt.
pub(crate) fn walk(b: &[u8], mut f: impl FnMut(&[u8])) {
    let mut i = 0usize;
    while let Some(&l) = b.get(i) {
        let l = usize::from(l);
        let Some(d) = b.get(i..i + l) else {
            break;
        };
        if l < 2 {
            break;
        }
        f(d);
        i += l;
    }
}

/// Wat de configuratiedescriptor zegt.
#[derive(Debug, Default)]
pub(crate) struct Parsed {
    pub(crate) conf_val: u8,
    ifaces: BoundedVec<HidIface, MAX_HID_IFACES>,
    pub(crate) bulk: Option<BulkIface>,
}

/// Zoekt eerst ELKE boot-HID-interface met een interrupt-IN-endpoint
/// (hooguit één per rol), en alleen als die er niet zijn de bulk-only
/// interface: een apparaat is in onze wereld óf boot-HID óf opslag, en de
/// HID-vraag is de goedkoopste van de twee.
///
/// De keten is plat: een interface-descriptor gevolgd door zijn endpoints,
/// dan de volgende interface. We onthouden dus welke interface we net zagen
/// en hangen de eerste bruikbare endpoint daaraan. Een interface die ons
/// niet interesseert zet `cur` op `None`, zodat zíjn endpoints niet per
/// ongeluk bij de vorige belanden: dat is de val in deze parse.
pub(crate) fn parse_config(b: &[u8]) -> Parsed {
    let mut p = Parsed {
        conf_val: byte(b, 5),
        ..Parsed::default()
    };
    let mut cur: Option<(u8, u8)> = None;
    walk(b, |d| match byte(d, 1) {
        DESC_INTERFACE => {
            cur = None;
            let proto = byte(d, 7);
            if d.len() >= 9
                && byte(d, 3) == 0
                && byte(d, 5) == CLASS_HID
                && byte(d, 6) == SUBCLASS_BOOT
                && (proto == PROTO_KEYBOARD || proto == PROTO_MOUSE)
                && !p.ifaces.iter().any(|f| f.proto == proto)
                && !p.ifaces.is_full()
            {
                cur = Some((byte(d, 2), proto));
            }
        }
        DESC_ENDPOINT => {
            // bmAttributes[1:0] == 3 = interrupt, bEndpointAddress bit 7 = IN.
            let addr = byte(d, 2);
            let mps = (u32::from(byte(d, 4)) | u32::from(byte(d, 5)) << 8) & 0x7FF;
            if let Some((num, proto)) = cur
                && d.len() >= 7
                && byte(d, 3) & 0x3 == 3
                && addr & 0x80 != 0
                && addr & 0xF != 0
                && mps != 0
            {
                let _ = p.ifaces.push(HidIface {
                    num,
                    proto,
                    dci: 2 * u32::from(addr & 0xF) + 1, // IN-endpoint: DCI = 2N+1
                    mps,
                    interval: byte(d, 6),
                    ..HidIface::default()
                });
                cur = None; // deze rol is vervuld; verdere endpoints negeren
            }
        }
        _ => {}
    });
    if p.ifaces.is_empty() {
        p.bulk = parse_bulk(b);
    }
    p
}

impl Hc {
    /// Het adres van dword `d` van context-index `i` (0 = input control
    /// context of slot context, afhankelijk van welke context je
    /// adresseert).
    pub(crate) fn ctx_dw(&self, base: Pa, i: u32, d: u32) -> Pa {
        base.add(u64::from(i) * self.ctx_size + u64::from(d) * 4)
    }

    pub(crate) fn res_mut(&mut self, slot: usize) -> Result<&mut SlotRes> {
        self.res
            .get_mut(slot)
            .and_then(Option::as_mut)
            .ok_or(Error::Detached)
    }

    fn res_ref(&self, slot: usize) -> Result<&SlotRes> {
        self.res
            .get(slot)
            .and_then(Option::as_ref)
            .ok_or(Error::Detached)
    }

    pub(crate) fn dev_mut(&mut self, slot: usize) -> Result<&mut DevState> {
        self.res_mut(slot)?.dev.as_mut().ok_or(Error::Detached)
    }

    pub(crate) fn dev_ref(&self, slot: usize) -> Result<&DevState> {
        self.res_ref(slot)?.dev.as_ref().ok_or(Error::Detached)
    }

    /// Het slot van `d` als het handvat nog geldt.
    pub(crate) fn live(&self, d: &Device) -> Result<usize> {
        let slot = usize::from(d.slot);
        match self.dev_ref(slot) {
            Ok(st) if st.generation == d.generation => Ok(slot),
            _ => Err(Error::Detached),
        }
    }

    /// Port-reset een poort en wacht tot hij enabled is. Op USB2 zet de
    /// controller PED pas ná een geslaagde reset; op USB3 gebeurt dat als
    /// deel van de link-training. In beide gevallen is "PED staat aan" het
    /// signaal dat er een bruikbaar apparaat aan hangt.
    async fn reset_port(&mut self, n: u8, t: &impl Timer) -> Result {
        if !self.probed {
            return Err(Error::NotProbed);
        }
        if n == 0 || n > self.max_ports {
            return Err(Error::PortLost { port: n, portsc: 0 });
        }
        self.clear_changes(n);
        self.port_action(n, PSC_PR);
        let deadline = t.now().saturating_add(PORT_RESET_TIMEOUT_NS);
        loop {
            let v = self.port_regs(n).portsc.read();
            if v & PSC_PRC != 0 {
                self.clear_changes(n);
                if v & PSC_PED == 0 {
                    return Err(Error::PortNotEnabled { port: n, portsc: v });
                }
                return Ok(());
            }
            if v & PSC_CCS == 0 {
                return Err(Error::PortLost { port: n, portsc: v });
            }
            if t.now() >= deadline {
                return Err(Error::PortResetTimeout { port: n, portsc: v });
            }
            t.sleep(POLL_STEP_NS).await;
        }
    }

    /// Een commando met het input context van `slot` als parameter (Address
    /// Device, Evaluate Context, Configure Endpoint).
    pub(crate) async fn ctx_command(
        &mut self,
        slot: usize,
        trb: u32,
        what: &'static str,
        t: &impl Timer,
    ) -> Result {
        let p = self.res_ref(slot)?.in_ctx.0 + self.bus_off;
        self.command(
            t,
            p as u32,
            (p >> 32) as u32,
            0,
            trb << TRB_TYPE_SHIFT | (slot as u32) << 24,
            what,
        )
        .await
        .map(|_| ())
    }

    /// Een endpoint-commando (Reset Endpoint, Stop Endpoint, Set TR
    /// Dequeue Pointer) op `dci` van `slot`, met `p` als parameter.
    async fn ep_command(
        &mut self,
        slot: usize,
        dci: u32,
        trb: u32,
        p: u64,
        what: &'static str,
        t: &impl Timer,
    ) -> Result {
        self.command(
            t,
            p as u32,
            (p >> 32) as u32,
            0,
            trb << TRB_TYPE_SHIFT | (slot as u32) << 24 | dci << 16,
            what,
        )
        .await
        .map(|_| ())
    }

    /// Vult endpoint-context `dci` in het input context `inp`: CErr 3, het
    /// type, de pakketgrootte, de dequeue-pointer en `dw4` (Average TRB
    /// Length en Max ESIT Payload). DW0 blijft wat [`Hc::build_input`]
    /// achterliet (nul) tenzij de aanroeper hem zelf zet.
    pub(crate) fn write_ep(&self, inp: Pa, dci: u32, ep_type: u32, mps: u32, deq: u64, dw4: u32) {
        let i = dci + 1;
        dev::write32(self.ctx_dw(inp, i, 1), 3 << 1 | ep_type << 3 | mps << 16);
        dev::write32(self.ctx_dw(inp, i, 2), deq as u32);
        dev::write32(self.ctx_dw(inp, i, 3), (deq >> 32) as u32);
        dev::write32(self.ctx_dw(inp, i, 4), dw4);
    }

    async fn disable_slot(&mut self, slot: usize, t: &impl Timer) -> Result {
        // Slot ID is het hoge byte van het command-TRB.
        let Ok(s) = u8::try_from(slot) else {
            return Err(self.quarantine(Poison::UnknownRelease { slot }));
        };
        self.command(
            t,
            0,
            0,
            0,
            TRB_DISABLE_SLOT << TRB_TYPE_SHIFT | u32::from(s) << 24,
            "disable slot",
        )
        .await
        .map(|_| ())
    }

    fn clear_slot(&mut self, slot: usize) {
        dev::write64(self.dcbaa.add(slot as u64 * 8), 0);
        dev::mb();
    }

    /// Boekt een bevestigd Enable Slot-resultaat in. Geeft de controller een
    /// slot buiten ons CONFIG-bereik terug, dan moet precies dát
    /// gerapporteerde hardware-slot meteen gedisabled worden
    /// ([`Claim::Stray`], afgerond door [`Hc::stray_released`]). Alleen een
    /// bevestigde disable laat de controller bruikbaar; slot 0, een
    /// softwarecollisie of een mislukte cleanup quarantaint de hele
    /// controller.
    pub(crate) fn claim_enabled_slot(&mut self, slot: u8) -> Result<Claim> {
        let s = usize::from(slot);
        if s >= 1 && s <= self.n_slots {
            let Some(r) = self.res.get_mut(s).and_then(Option::as_mut) else {
                return Err(self.quarantine(Poison::SlotNoResources { slot }));
            };
            if r.in_use || r.quarantined {
                return Err(self.quarantine(Poison::SlotBusy { slot }));
            }
            // Vanaf het bevestigde Enable-resultaat bestaat de hardwarelease,
            // dus nú boeken, niet pas na de descriptor/configuratiefase.
            r.in_use = true;
            return Ok(Claim::Owned);
        }
        if slot == 0 {
            return Err(self.quarantine(Poison::SlotZero));
        }
        Ok(Claim::Stray(s))
    }

    /// De afronding van een [`Claim::Stray`]: `disabled` is de uitkomst van
    /// Disable Slot op dat slot. Geeft altijd een fout, want het apparaat
    /// heeft geen slot.
    pub(crate) fn stray_released(&mut self, slot: u8, disabled: Result) -> Error {
        let n_slots = self.n_slots as u8;
        if disabled.is_err() {
            return self.quarantine(Poison::SlotCleanup { slot, n_slots });
        }
        Error::SlotOutOfRange { slot, n_slots }
    }

    /// Ruimt een half geënumereerd slot op. Faalt de cleanup, dan is dat de
    /// belangrijkere fout (de controller is dan gequarantaind).
    async fn abort_attach(&mut self, slot: usize, cause: Error, t: &impl Timer) -> Error {
        match self.release_slot(slot, t).await {
            Ok(()) => cause,
            Err(e) => e,
        }
    }

    /// Doorloopt de volledige enumeratie van de poort tot gearmeerde
    /// interrupt-endpoints (of geconfigureerde bulk-endpoints). Geeft
    /// `Ok(None)` als er wel een apparaat hangt maar het geen boot-HID of
    /// opslag is: een willekeurige dongle in de poort is geen fout, alleen
    /// niets voor ons; het slot is dan al teruggegeven.
    pub async fn attach(&mut self, port: u8, t: &impl Timer) -> Result<Option<Device>> {
        if !self.running {
            return Err(Error::NotRunning);
        }
        if let Some(p) = self.poisoned {
            return Err(Error::Poisoned(p));
        }
        self.reset_port(port, t).await?;
        let raw = self.port_regs(port).portsc.read();
        let speed = Speed(((raw >> PSC_SPEED_SHIFT) & PSC_SPEED_MASK) as u8);

        let ev = self
            .command(t, 0, 0, 0, TRB_ENABLE_SLOT << TRB_TYPE_SHIFT, "enable slot")
            .await?;
        if let Claim::Stray(s) = self.claim_enabled_slot(ev.slot)? {
            let r = self.disable_slot(s, t).await;
            return Err(self.stray_released(ev.slot, r));
        }
        let slot = usize::from(ev.slot);

        let generation = self.next_gen;
        self.next_gen = self.next_gen.wrapping_add(1).max(1);
        self.res_mut(slot)?.dev = Some(DevState {
            generation,
            port,
            speed,
            vendor: 0,
            product: 0,
            mps0: default_mps0(speed),
            conf_val: 0,
            ifaces: BoundedVec::new(),
            bulk: None,
            last_err: None,
            boot_refused: 0,
        });
        if let Err(e) = self.address(slot, t).await {
            return Err(self.abort_attach(slot, e, t).await);
        }
        if let Err(e) = self.read_descriptors(slot, t).await {
            return Err(self.abort_attach(slot, e, t).await);
        }
        let st = self.dev_ref(slot)?;
        if st.ifaces.is_empty() && st.bulk.is_none() {
            self.release_slot(slot, t).await?;
            return Ok(None);
        }
        if let Err(e) = self.configure(slot, t).await {
            return Err(self.abort_attach(slot, e, t).await);
        }
        self.handle_of(slot).map(Some)
    }

    /// Het handvat van het apparaat op `slot`.
    fn handle_of(&self, slot: usize) -> Result<Device> {
        let st = self.dev_ref(slot)?;
        let mut protos = [PROTO_NONE; MAX_HID_IFACES];
        for (p, f) in protos.iter_mut().zip(st.ifaces.iter()) {
            *p = f.proto;
        }
        Ok(Device {
            slot: slot as u8,
            port: st.port,
            speed: st.speed,
            vendor_id: st.vendor,
            product_id: st.product,
            generation: st.generation,
            protos,
            n_protos: st.ifaces.len() as u8,
            boot_refused: st.boot_refused,
            mass_storage: st.bulk.is_some(),
        })
    }

    /// Stap 1 t/m 3 van de enumeratie: device context aanhaken, het input
    /// context vullen met slot en EP0, en Address Device.
    async fn address(&mut self, slot: usize, t: &impl Timer) -> Result {
        let bus_off = self.bus_off;
        let csz = self.ctx_size;
        let r = self.res_mut(slot)?;
        r.ctrl.reset();
        dev::clear(r.dev_ctx, (csz * 32) as usize);
        let dev_ctx = r.dev_ctx;
        dev::write64(self.dcbaa.add(slot as u64 * 8), dev_ctx.0 + bus_off);
        dev::mb();

        self.build_input(slot, 1, ADD_SLOT | ADD_EP0)?;
        self.ctx_command(slot, TRB_ADDRESS_DEV, "address device", t)
            .await
    }

    /// Vult het input context met het slot context en EP0. `entries` is de
    /// hoogste DCI die geldig moet zijn (1 = alleen EP0); `add` zijn de
    /// flags die zeggen wélke contexten het commando mag lezen.
    ///
    /// De add-flags zijn een parameter en geen afgeleide van `entries`, en
    /// dat is geen smaak: een gezette flag naar een leeg endpoint-context is
    /// EP Type 0 (Not Valid) en dus een parameter error. Wie A0..A(entries)
    /// automatisch zou zetten, zet flags aan voor de endpoints tússen EP0 en
    /// de onze die we nooit invullen.
    ///
    /// Het input context wordt élke keer opnieuw opgebouwd in plaats van uit
    /// het device context gekopieerd. Dat kan omdat wij alles wat erin staat
    /// zelf bepalen (route 0: geen hub-diepte, geen TT), en het scheelt de
    /// klassieke fout waarbij een half gekopieerd context oude endpoints laat
    /// staan.
    pub(crate) fn build_input(&mut self, slot: usize, entries: u32, add: u32) -> Result {
        let csz = self.ctx_size;
        let r = self.res_ref(slot)?;
        let st = r.dev.as_ref().ok_or(Error::Detached)?;
        let inp = r.in_ctx;
        let deq = r.ctrl.deq_ptr();
        dev::clear(inp, (csz * 33) as usize);
        dev::write32(self.ctx_dw(inp, 0, 1), add);

        // Slot context (index 1 in het input context): route string 0
        // (direct op een roothub-poort), de snelheid en het poortnummer.
        dev::write32(
            self.ctx_dw(inp, 1, 0),
            u32::from(st.speed.0) << 20 | entries << 27,
        );
        dev::write32(self.ctx_dw(inp, 1, 1), u32::from(st.port) << 16);

        // EP0 (DCI 1): control, onze control ring, average TRB length 8.
        self.write_ep(inp, 1, EP_TYPE_CONTROL, st.mps0, deq, 8);
        dev::mb();
        Ok(())
    }

    /// Voert één control transfer uit. Setup-, Data- en Statusfase zijn in
    /// xHCI drie APARTE TD's (xHCI 4.11.2.2), dus een korte datafase
    /// beëindigt alleen zijn eigen TD en de statusfase loopt gewoon door:
    /// daarom kunnen we op allebei een completion vragen en het echte aantal
    /// bytes uit de datafase halen.
    #[expect(
        clippy::too_many_arguments,
        reason = "de vijf velden van het setup-pakket plus slot en timer; een struct zou alleen de aanroepplek verbergen"
    )]
    pub(crate) async fn control(
        &mut self,
        slot: usize,
        req_type: u8,
        req: u8,
        val: u16,
        idx: u16,
        len: u16,
        t: &impl Timer,
    ) -> Result<usize> {
        if let Some(p) = self.poisoned {
            return Err(Error::Poisoned(p));
        }
        if usize::from(len) > BUF_CTRL_SIZE {
            return Err(Error::ControlTooLong { len });
        }
        let bus_off = self.bus_off;
        let r = self.res_mut(slot)?;
        let is_in = req_type & 0x80 != 0;
        // TRT: 0 = geen datafase, 2 = OUT, 3 = IN.
        let trt: u32 = match (len, is_in) {
            (0, _) => 0,
            (_, true) => 3,
            (_, false) => 2,
        };
        r.ctrl.push(
            u32::from(req_type) | u32::from(req) << 8 | u32::from(val) << 16,
            u32::from(idx) | u32::from(len) << 16,
            8,
            TRB_IDT | TRB_SETUP << TRB_TYPE_SHIFT | trt << 16,
        );
        let mut data_trb = 0;
        if len > 0 {
            let bus = r.buf.0 + BUF_CTRL + bus_off;
            data_trb = r.ctrl.push(
                bus as u32,
                (bus >> 32) as u32,
                u32::from(len),
                TRB_ISP | TRB_IOC | TRB_DATA << TRB_TYPE_SHIFT | u32::from(is_in) << 16,
            );
        }
        // De statusfase gaat de ANDERE kant op dan de data; zonder data is
        // hij IN.
        let sdir = u32::from(!(is_in && len > 0));
        let stat_trb = r
            .ctrl
            .push(0, 0, 0, TRB_IOC | TRB_STATUS << TRB_TYPE_SHIFT | sdir << 16);
        self.doorbell(slot, 1);

        let mut got = usize::from(len);
        if len > 0 {
            let ev = self
                .wait_transfer(slot, data_trb, "control data stage", t)
                .await?;
            if ev.rem > u32::from(len) {
                return Err(self.quarantine(Poison::Overrun {
                    what: "control",
                    rem: ev.rem,
                }));
            }
            got = usize::from(len) - ev.rem as usize;
        }
        self.wait_transfer(slot, stat_trb, "control status stage", t)
            .await?;
        Ok(got)
    }

    /// Wacht op de completion van één control-fase. Loopt een fase mis (het
    /// apparaat stalt, of zwijgt) dan gaat EP0 terug op een verse ring. Een
    /// stall zet hem in de controller op halted en dan weigert hij elk
    /// volgend request; een half afgemaakte TD zou het volgende request
    /// achter zich laten wachten. Het apparaat is daarmee niet weg en de
    /// controller zeker niet: alleen als het resetten zelf niet lukt, is er
    /// meer aan de hand (en dat beslist `command`).
    async fn wait_transfer(
        &mut self,
        slot: usize,
        trb: u64,
        what: &'static str,
        t: &impl Timer,
    ) -> Result<Event> {
        let ev = self
            .wait_event(
                t,
                |e| e.kind == TRB_TRANSFER_EVT && e.ptr == trb,
                CONTROL_TIMEOUT_NS,
                what,
            )
            .await;
        let err = match ev {
            Ok(ev) if ev.comp == CC_SUCCESS || ev.comp == CC_SHORT_PACKET => return Ok(ev),
            Ok(ev) => Error::Transfer {
                what,
                code: ev.comp,
            },
            Err(e) => e,
        };
        self.reset_ep(slot, 1, RingId::Ctrl, t).await?;
        Err(err)
    }

    /// Leest `out.len()` bytes uit de control-databuffer van dit slot.
    fn ctrl_bytes(&self, slot: usize, out: &mut [u8]) -> Result {
        let r = self.res_ref(slot)?;
        dev::copy_out(out, r.buf.add(BUF_CTRL));
        Ok(())
    }

    /// GET_DESCRIPTOR van `len` bytes; korter dan `min` is een fout.
    async fn get_descriptor(
        &mut self,
        slot: usize,
        kind: u16,
        len: u16,
        min: usize,
        what: &'static str,
        t: &impl Timer,
    ) -> Result<usize> {
        let n = self
            .control(slot, 0x80, REQ_GET_DESCRIPTOR, kind << 8, 0, len, t)
            .await?;
        if n < min {
            return Err(Error::Descriptor { what, got: n });
        }
        Ok(n)
    }

    /// Haalt de device- en configuratiedescriptor op en zoekt de
    /// boot-HID-interfaces (of de bulk-only interface).
    async fn read_descriptors(&mut self, slot: usize, t: &impl Timer) -> Result {
        // Eerst acht bytes: bij full-speed staat de echte EP0-pakketgrootte
        // pas in byte 7, en tot we die weten mogen we niet meer dan één
        // pakket vragen.
        self.get_descriptor(slot, DESC_DEVICE, 8, 8, "device descriptor (8)", t)
            .await?;
        let mut dd = [0u8; 18];
        self.ctrl_bytes(slot, &mut dd[..8])?;
        let speed = self.dev_ref(slot)?.speed;
        let mps = descriptor_mps0(speed, dd[7])?;
        if mps != self.dev_ref(slot)?.mps0 {
            self.dev_mut(slot)?.mps0 = mps;
            // Evaluate Context: alleen EP0 aanpassen, het slot laten staan.
            self.build_input(slot, 1, ADD_EP0)?;
            self.ctx_command(slot, TRB_EVAL_CTX, "evaluate context (EP0 packet size)", t)
                .await?;
        }

        self.get_descriptor(slot, DESC_DEVICE, 18, 18, "device descriptor (18)", t)
            .await?;
        self.ctrl_bytes(slot, &mut dd)?;
        {
            let st = self.dev_mut(slot)?;
            st.vendor = u16::from_le_bytes([dd[8], dd[9]]);
            st.product = u16::from_le_bytes([dd[10], dd[11]]);
        }

        // Configuratiedescriptor: eerst de kop voor wTotalLength, dan het
        // geheel.
        self.get_descriptor(slot, DESC_CONFIG, 9, 9, "config descriptor (9)", t)
            .await?;
        let mut cd = [0u8; BUF_CTRL_SIZE];
        self.ctrl_bytes(slot, &mut cd[..9])?;
        let total = u16::from_le_bytes([cd[2], cd[3]]);
        if total < 9 {
            return Err(Error::ConfigLength { total });
        }
        let total = total.min(BUF_CTRL_SIZE as u16);
        let n = self
            .get_descriptor(slot, DESC_CONFIG, total, 0, "config descriptor", t)
            .await?;
        let cfg = cd.get_mut(..n).unwrap_or_default();
        self.ctrl_bytes(slot, cfg)?;
        let p = parse_config(cfg);
        let st = self.dev_mut(slot)?;
        st.conf_val = p.conf_val;
        st.ifaces = p.ifaces;
        st.bulk = p.bulk;
        Ok(())
    }

    /// Zet álle gevonden interrupt-endpoints aan, kiest de configuratie en
    /// schakelt elke interface naar het boot-protocol. Daarna staan ze
    /// gearmeerd.
    ///
    /// Eén Configure Endpoint voor allemaal: het input context draagt
    /// zoveel endpoint-contexten als je wilt, en de add-flags zeggen welke
    /// meedoen. Twee losse commando's zouden de tweede het werk van de eerste
    /// laten overschrijven, want elk commando vervangt de héle
    /// endpoint-configuratie van het slot.
    async fn configure(&mut self, slot: usize, t: &impl Timer) -> Result {
        if self.dev_ref(slot)?.bulk.is_some() {
            return self.configure_bulk(slot, t).await;
        }
        let (mut add, mut max_dci) = (ADD_SLOT, 0);
        {
            let r = self.res_mut(slot)?;
            let SlotRes { intr, dev, .. } = r;
            let st = dev.as_mut().ok_or(Error::Detached)?;
            for (i, (f, ring)) in st.ifaces.iter_mut().zip(intr.iter_mut()).enumerate() {
                f.buf_off = BUF_INTR + (i * BUF_INTR_SIZE) as u64;
                ring.reset();
                add |= 1 << f.dci;
                max_dci = max_dci.max(f.dci);
            }
        }
        // A1 blijft UIT: een Configure Endpoint mag de default control
        // endpoint niet aanraken (xHCI 4.6.6); die is al geregeld door
        // Address Device.
        self.build_input(slot, max_dci, add)?;
        let r = self.res_ref(slot)?;
        let inp = r.in_ctx;
        let st = r.dev.as_ref().ok_or(Error::Detached)?;
        for (f, ring) in st.ifaces.iter().zip(r.intr.iter()) {
            dev::write32(
                self.ctx_dw(inp, f.dci + 1, 0),
                interval_exponent(st.speed, f.interval) << 16,
            );
            // Average TRB length en Max ESIT Payload: bij een
            // interrupt-endpoint zonder burst is dat allebei gewoon de
            // pakketgrootte.
            let dw4 = f.mps | f.mps << 16;
            self.write_ep(inp, f.dci, EP_TYPE_INTR_IN, f.mps, ring.deq_ptr(), dw4);
        }
        let conf_val = st.conf_val;
        let n_if = st.ifaces.len();
        dev::mb();

        self.ctx_command(slot, TRB_CONFIG_EP, "configure endpoint", t)
            .await?;
        self.control(slot, 0x00, REQ_SET_CONFIG, u16::from(conf_val), 0, 0, t)
            .await?;
        for i in 0..n_if {
            let num = self
                .dev_ref(slot)?
                .ifaces
                .get(i)
                .map_or(0, |f| u16::from(f.num));
            // SET_PROTOCOL(0) = boot-protocol, PER INTERFACE. Sommige
            // apparaten stallen hem als ze maar één protocol kennen; dat is
            // geen fout, dan spreken ze al boot.
            if self
                .control(slot, 0x21, HID_SET_PROTOCOL, 0, num, 0, t)
                .await
                .is_err()
            {
                self.dev_mut(slot)?.boot_refused |= 1 << i;
            }
            // SET_IDLE(0) = alleen rapporteren bij verandering. Ook
            // optioneel.
            let _ = self.control(slot, 0x21, HID_SET_IDLE, 0, num, 0, t).await;
            if let Some(p) = self.poisoned {
                return Err(Error::Poisoned(p));
            }
            self.arm(slot, i)?;
        }
        Ok(())
    }

    /// Zet één Normal-TRB op de interrupt-ring van interface `i` en belt
    /// aan. De controller vult de buffer zodra het apparaat iets te melden
    /// heeft; tot die tijd kost het niets.
    fn arm(&mut self, slot: usize, i: usize) -> Result {
        let bus_off = self.bus_off;
        let r = self.res_mut(slot)?;
        let SlotRes { intr, dev, buf, .. } = r;
        let st = dev.as_mut().ok_or(Error::Detached)?;
        let (Some(f), Some(ring)) = (st.ifaces.get_mut(i), intr.get_mut(i)) else {
            return Err(Error::Detached);
        };
        let bus = buf.0 + f.buf_off + bus_off;
        f.arm_trb = ring.push(
            bus as u32,
            (bus >> 32) as u32,
            report_len(f.mps) as u32,
            TRB_ISP | TRB_IOC | TRB_NORMAL << TRB_TYPE_SHIFT,
        );
        f.armed = true;
        let dci = f.dci;
        self.doorbell(slot, dci);
        Ok(())
    }

    /// Haalt één binnengekomen HID-rapport op in `buf` en armeert die
    /// endpoint opnieuw. `None` als er niets klaarstaat: dit is het pollpad
    /// en het hoort meestal niets te doen. Wacht alleen als een endpoint
    /// stalt (Reset Endpoint en een verse ring) en alloceert niets.
    pub async fn report(&mut self, d: &Device, buf: &mut [u8], t: &impl Timer) -> Option<Report> {
        let slot = self.live(d).ok()?;
        self.pump();
        let n_if = self.dev_ref(slot).ok()?.ifaces.len();
        for i in 0..n_if {
            let f = *self.dev_ref(slot).ok()?.ifaces.get(i)?;
            if !f.armed {
                continue;
            }
            let Some(ev) = self.take(|e| e.kind == TRB_TRANSFER_EVT && e.ptr == f.arm_trb) else {
                continue;
            };
            if let Some(g) = self.dev_mut(slot).ok()?.ifaces.get_mut(i) {
                g.armed = false;
            }
            return self.handle(slot, i, f, ev, buf, t).await;
        }
        None
    }

    async fn handle(
        &mut self,
        slot: usize,
        i: usize,
        f: HidIface,
        ev: Event,
        buf: &mut [u8],
        t: &impl Timer,
    ) -> Option<Report> {
        let want = report_len(f.mps);
        match ev.comp {
            CC_SUCCESS | CC_SHORT_PACKET => {
                if ev.rem as usize > want {
                    let e = self.quarantine(Poison::Overrun {
                        what: "interrupt",
                        rem: ev.rem,
                    });
                    self.set_err(slot, e);
                    return None;
                }
                let n = (want - ev.rem as usize).min(buf.len());
                if n > 0
                    && let (Ok(r), Some(dst)) = (self.res_ref(slot), buf.get_mut(..n))
                {
                    dev::copy_out(dst, r.buf.add(f.buf_off));
                }
                if let Err(e) = self.arm(slot, i) {
                    self.set_err(slot, e);
                }
                (n > 0).then_some(Report {
                    len: n,
                    proto: f.proto,
                })
            }
            CC_STALL => {
                // Een gestalde interrupt-endpoint komt niet vanzelf terug:
                // hij moet gereset worden én zijn dequeue-pointer moet
                // opnieuw gezet, anders wijst de controller nog naar het TRB
                // dat de stall veroorzaakte.
                match self.reset_ep(slot, f.dci, RingId::Intr(i), t).await {
                    Ok(()) => {
                        if let Err(e) = self.arm(slot, i) {
                            self.set_err(slot, e);
                        }
                    }
                    Err(e) => self.set_err(slot, e),
                }
                None
            }
            code => {
                // Een losgetrokken apparaat geeft transaction errors tot de
                // poort het meldt. Niet opnieuw armeren: de scan ruimt hem
                // op.
                self.set_err(
                    slot,
                    Error::Transfer {
                        what: "interrupt",
                        code,
                    },
                );
                None
            }
        }
    }

    fn set_err(&mut self, slot: usize, e: Error) {
        if let Ok(st) = self.dev_mut(slot) {
            st.last_err = Some(e);
        }
    }

    /// De laatste transferfout van dit apparaat (`None` zolang alles loopt).
    #[must_use]
    pub fn device_err(&self, d: &Device) -> Option<Error> {
        match self.live(d) {
            Ok(slot) => self.dev_ref(slot).ok()?.last_err,
            Err(e) => Some(e),
        }
    }

    /// De state van een endpoint uit de device context die de controller
    /// bijhoudt. Index `dci` in die context is precies het endpoint (0 is
    /// het slot context).
    fn ep_state(&self, slot: usize, dci: u32) -> Result<u32> {
        let r = self.res_ref(slot)?;
        Ok(dev::read32(self.ctx_dw(r.dev_ctx, dci, 0)) & 7)
    }

    /// Zet een endpoint aan de controllerkant terug op een verse ring. Welk
    /// commando eerst gaat hangt af van de state: Reset Endpoint mag alleen
    /// op een gestalde endpoint en Stop Endpoint alleen op een draaiende
    /// (anders Context State Error, xHCI 4.6.8/4.6.9). Pas daarna mag de
    /// dequeue-pointer verzet worden, anders wijst hij nog naar het TRB waar
    /// het misging. Events die nog naar de oude ring wijzen gaan weg:
    /// niemand wacht er meer op.
    pub(crate) async fn reset_ep(
        &mut self,
        slot: usize,
        dci: u32,
        id: RingId,
        t: &impl Timer,
    ) -> Result {
        match self.ep_state(slot, dci)? {
            EP_HALTED => {
                self.ep_command(slot, dci, TRB_RESET_EP, 0, "reset endpoint", t)
                    .await?;
            }
            EP_RUNNING => {
                self.ep_command(slot, dci, TRB_STOP_EP, 0, "stop endpoint", t)
                    .await?;
            }
            _ => {}
        }
        let ring = self.res_mut(slot)?.ring(id).ok_or(Error::Detached)?;
        ring.reset();
        let ring = *ring;
        let deq = ring.deq_ptr();
        self.ep_command(slot, dci, TRB_SET_TR_DEQ, deq, "set TR dequeue pointer", t)
            .await?;
        self.drop_ring(slot, &ring);
        Ok(())
    }

    /// Geeft het slot terug aan de controller. Idempotent. Een fout betekent
    /// dat de hardware-disable niet bevestigd is; het slot blijft dan
    /// bewust eigenaar en quarantined, en de controller weigert nieuwe
    /// apparaten tot [`Hc::recover`].
    pub async fn detach(&mut self, d: &Device, t: &impl Timer) -> Result {
        let Ok(slot) = self.live(d) else {
            return Ok(());
        };
        let in_use = self.res_ref(slot).is_ok_and(|r| r.in_use);
        if !in_use {
            return Ok(());
        }
        if let Err(e) = self.release_slot(slot, t).await {
            self.set_err(slot, e);
            return Err(e);
        }
        Ok(())
    }

    /// Disable Slot, en pas ná de bevestigde completion het device context
    /// uit de DCBAA halen. Bij timeout of afwijzing blijft de softwarelease
    /// staan en gaat het slot in quarantaine; anders zouden volgende Enable
    /// Slot-pogingen ongemerkt de eindige hardware-slots kunnen opstapelen.
    async fn release_slot(&mut self, slot: usize, t: &impl Timer) -> Result {
        if !self.release_begin(slot)? {
            return Ok(());
        }
        let r = self.disable_slot(slot, t).await;
        self.release_end(slot, r)
    }

    /// Mag `slot` vrijgegeven worden, en moet er Disable Slot heen? `false`:
    /// niets te doen (idempotent).
    pub(crate) fn release_begin(&mut self, slot: usize) -> Result<bool> {
        if slot < 1 || slot > self.n_slots || self.res_ref(slot).is_err() {
            return Err(self.quarantine(Poison::UnknownRelease { slot }));
        }
        let (in_use, quarantined) = self
            .res_ref(slot)
            .map_or((false, false), |r| (r.in_use, r.quarantined));
        Ok(in_use || quarantined)
    }

    /// De boekhouding na Disable Slot: bevestigd is vrij (en DCBAA[slot]
    /// leeg), anders quarantaine.
    pub(crate) fn release_end(&mut self, slot: usize, disabled: Result) -> Result {
        if disabled.is_err() {
            if let Ok(r) = self.res_mut(slot) {
                r.in_use = true;
                r.quarantined = true;
            }
            return Err(self.quarantine(Poison::DisableUnconfirmed { slot: slot as u8 }));
        }
        self.clear_slot(slot);
        if let Ok(r) = self.res_mut(slot) {
            r.in_use = false;
            r.quarantined = false;
            r.dev = None;
        }
        Ok(())
    }
}
