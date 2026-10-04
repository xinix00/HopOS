//! Het contract tussen een blok-driver en hopfs.
//!
//! Precies de grens die hopfs nodig heeft: `buf.len()` bytes lezen of
//! schrijven vanaf een LBA van 512 bytes, en alles wat geschreven is
//! duurzaam maken. De virtio-blk-driver op QEMU en de NVMe-driver op ijzer
//! leveren hem; de tests een schijf in RAM. Het trait woont hier en niet in
//! `kern`, omdat een driver de kern niet kent (handboek §7, dezelfde reden
//! als `netdev`).
//!
//! Eén vorm: [`AsyncBlockDevice`]. Eén opdracht tegelijk,
//! [`submit`](AsyncBlockDevice::submit) geeft een [`InFlight`], en
//! [`InFlight::done`] wacht op de completion: op de bel van de IRQ-lijn met
//! een vangrail van [`IRQ_GUARD`], of zonder lijn door te pollen (eerst per
//! ronde, daarna op [`POLL_PERIOD`]). Tijdens het wachten draait de executor
//! door. [`Paced`] maakt er de [`BlockIo`] van die hopfs gebruikt. Wie vóór
//! de executor iets met de schijf doet (de mount bij de boot, de meetbank),
//! draait precies die futures af met [`block_on`] en een [`Pace`] die pollt
//! ([`Spin`]): dezelfde driver, dezelfde `start` en `poll_done`, geen
//! tweede pad.
//!
//! # Meer opdrachten tegelijk: tickets en de [`Queue`]
//!
//! Een driver met tags (de ANS: zestien) zegt dat met
//! [`depth`](AsyncBlockDevice::depth) en levert per opdracht een ticket
//! ([`start_tag`](AsyncBlockDevice::start_tag),
//! [`poll_tag`](AsyncBlockDevice::poll_tag), en één
//! [`reap`](AsyncBlockDevice::reap) die alle completions ophaalt). Een
//! driver zonder tags hoeft niets: de standaard is zijn ene opdracht als
//! ticket 0. De [`Queue`] is de voorkant (Linux blk-mq in het klein): wie
//! I/O wil, krijgt een ticket en wacht op zijn eigen completion; precies één
//! wachter tegelijk pollt het device (de "pacer", op het ritme van hierboven)
//! en wekt de anderen waarvan de completion binnenkwam. Zo kost een
//! node met zestien opdrachten in de lucht één poll per ronde, niet
//! zestien. GEMETEN 01-10 op de M4 (`hopos.nvmebench=1`): willekeurig
//! 4 KiB lezen haalt 11.888 per seconde met één opdracht tegelijk (~84 us
//! per lees) en 175.055 met zestien tegelijk door deze wachtrij. De
//! wachtrij is er voor de node, niet voor de app.
//!
//! # Waarom er één pad is
//!
//! Les van 30-09 (de soak, ruim 1100 runs): tot alpha.14 was de grens
//! synchroon (`read`, `write`, `flush` die terugkeerden als het klaar was).
//! De periodieke hopfs-commit (FLUSH, de boom, FLUSH) wachtte op de executor
//! van de OS-core tot 5 s per verzoek op het device; op macOS is een FLUSH
//! van QEMU een F_FULLFSYNC van 3 tot 786 ms, en zolang stonden de kern, Hop
//! en de switch stil: geen tik, geen verkeer. Met 2,5 s per verzoek
//! nagebootst was dat een stilte van 7 s (`late_ms=6611` op de tik).
//!
//! In de Go-kern was synchroon wachten gratis: elke wachter was een
//! goroutine, en een goroutine is een eigen stack. Wie op de NVMe wachtte,
//! parkeerde zijn stack, en TamaGo's scheduler gaf de core aan de volgende.
//! Rust heeft hier geen goroutines en geen threadscheduler: één executor per
//! core, en een functie die wacht zonder terug te keren, houdt die core vast
//! met alles erop. Wachten is dus `.await`, en de enige manier om zonder
//! scheduler te wachten zonder de core vast te houden is submit plus await
//! (PORT.md §3: "de NVMe-actor: zijn lus ís één tegelijk, `submit(buf) ->
//! InFlight`").
//!
//! Een synchrone vorm ernaast was daarom geen gemak maar een tweede
//! driverpad (post plus spin) dat de stilte terugbrengt zodra iemand hem
//! vanaf de executor aanroept, en dat apart getest moet worden. Hij is weg:
//! wat vóór de executor moet wachten, wacht met [`block_on`]
//! over dezelfde async code, en daar houdt het de core terecht vast, want er
//! draait nog niets anders.

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]
#![forbid(unsafe_code)]

use core::fmt;
use core::future::Future;
use core::pin::Pin;
use core::task::{Context, Poll, Waker};
use core::time::Duration;
use sync::Signal;

/// De maat van een LBA van het contract: altijd 512 bytes, ook als het
/// device 4096-byte-blokken heeft (dan rekent de driver om).
pub const LBA_SIZE: u64 = 512;

/// De vangrail op de IRQ-lijn: een verloren flank mag nooit een hang
/// worden, dus wie op de bel wacht, kijkt na deze tijd toch (dezelfde
/// 10 ms als de RX-pomp).
pub const IRQ_GUARD: Duration = Duration::from_millis(10);

/// Zonder lijn: zo lang na de submit pollt [`InFlight::done`] per ronde
/// van de executor (een yield, geen timer). Een 4 KB-lees op NVMe is binnen
/// een paar tientallen microseconden klaar; die hoort geen timer te kosten.
pub const POLL_SPIN_NS: u64 = 100_000;

/// Zonder lijn, na [`POLL_SPIN_NS`]: de pollperiode. Een timer, geen yield:
/// een executor die per ronde yieldt slaapt nooit, en op de OS-core krijgt
/// een bewoner (Hop) alleen tijd als de executor slaapt. Een FLUSH van
/// honderden milliseconden mag Hop niet uithongeren.
pub const POLL_PERIOD: Duration = Duration::from_micros(200);

/// Waarom een blok-verzoek niet lukte. Elke variant draagt de LBA, want
/// "I/O failed" zonder plek is niets waard op een headless node.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// Het device meldde een fout, of antwoordde niet binnen zijn grens.
    Io {
        /// De LBA waar het verzoek begon.
        lba: u64,
    },
    /// Het verzoek valt buiten de schijf.
    OutOfRange {
        /// De LBA waar het verzoek begon.
        lba: u64,
        /// De lengte in bytes.
        len: usize,
    },
    /// Het device is blijvend dood (een stil device kan nog in zijn
    /// DMA-buffer schrijven, dus na één stilte is er geen weg terug).
    Dead,
    /// Er loopt nog een opdracht waarvan de wachter wegging (een gedropte
    /// [`Done`]): tot die terug is, gaat er niets nieuws naar het device,
    /// want de DMA-buffer is nog van de controller (handboek §1.2).
    Busy,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io { lba } => write!(f, "block I/O failed at LBA {lba}"),
            Self::OutOfRange { lba, len } => {
                write!(
                    f,
                    "block request {len} bytes at LBA {lba} falls outside the disk"
                )
            }
            Self::Dead => f.write_str("block device is dead"),
            Self::Busy => f.write_str("block device still busy with an abandoned request"),
        }
    }
}

/// Het resultaat van een blok-verzoek.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén opdracht aan het device, hoogstens
/// [`max_transfer`](AsyncBlockDevice::max_transfer) bytes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Op<'a> {
    /// `len` bytes lezen vanaf `lba`; de bytes komen bij de completion.
    Read {
        /// De eerste LBA.
        lba: u64,
        /// Het aantal bytes.
        len: usize,
    },
    /// `data` schrijven vanaf `lba`. De driver neemt de bytes bij de
    /// submit over (naar zijn DMA-buffer): daarna is `data` weer vrij.
    Write {
        /// De eerste LBA.
        lba: u64,
        /// De bytes.
        data: &'a [u8],
    },
    /// Alles wat geschreven is duurzaam maken.
    Flush,
}

/// Een blokapparaat in de vorm van de driver: één opdracht tegelijk, met
/// een submit en een completion.
///
/// De driver bezit zijn DMA-buffer; een opdracht die loopt, is van de
/// controller tot de completion hem teruggeeft. [`submit`] geeft daarom een
/// [`InFlight`] die de driver leent: een tweede submit kan pas als de eerste
/// klaar is, en dat bewijst de compiler (handboek §1.2).
///
/// [`submit`]: AsyncBlockDevice::submit
pub trait AsyncBlockDevice {
    /// De grootste transfer van één opdracht in bytes (een veelvoud van
    /// [`LBA_SIZE`]).
    fn max_transfer(&self) -> usize;

    /// Zet `op` op het device en luidt de doorbell; keert meteen terug. Een
    /// fout hier (buiten de schijf, dood, [`Error::Busy`]) betekent dat er
    /// niets naar het device ging.
    fn start(&mut self, op: Op<'_>) -> Result;

    /// Kijkt of de opdracht van [`start`](Self::start) klaar is. Bij een
    /// lees komen de bytes in `into` (vooraan, zoveel als gelezen). De
    /// time-out van het device is van de driver: daarna is het
    /// `Ready(Err(Dead))`, nooit eeuwig `Pending`.
    fn poll_done(&mut self, into: &mut [u8]) -> Poll<Result>;

    /// De bel van de IRQ-lijn van het device, als het board er een
    /// bedraadt. `None` = pollen.
    fn irq(&self) -> Option<&'static Signal> {
        None
    }

    /// Zonder lijn: hoe lang de wachter na de submit per ronde pollt, en
    /// daarna op welke periode. Een driver die zijn opdrachten kent, kiest
    /// zelf (de ANS: 20 us en 20 us); anders [`POLL_SPIN_NS`] en
    /// [`POLL_PERIOD`].
    fn poll_pace(&self) -> (u64, Duration) {
        (POLL_SPIN_NS, POLL_PERIOD)
    }

    /// Hoeveel opdrachten het device tegelijk aanneemt: tickets
    /// `0..depth`. Eén voor een driver zonder tags; dan is de standaard van
    /// de drie methoden hieronder zijn ene opdracht als ticket 0.
    fn depth(&self) -> usize {
        1
    }

    /// Zet `op` op het device onder een eigen ticket en keert meteen terug.
    /// [`Error::Busy`] = nu geen plaats (alle tags of de DMA-ruimte bezet):
    /// probeer het na een completion opnieuw.
    fn start_tag(&mut self, op: Op<'_>) -> Result<usize> {
        self.start(op).map(|()| 0)
    }

    /// Is ticket `t` klaar? Een driver met tags kijkt hier alleen naar wat
    /// [`reap`](Self::reap) al ophaalde. Bij `Ready` is het ticket weer vrij
    /// en staan de bytes van een lees vooraan in `into`.
    fn poll_tag(&mut self, t: usize, into: &mut [u8]) -> Poll<Result> {
        let _ = t;
        self.poll_done(into)
    }

    /// Haalt alle completions op die er zijn en toetst de time-outs; geeft
    /// de tickets die daarbij klaar kwamen (bit `t`). Een fout is een dood
    /// device: daarna geeft elke [`poll_tag`](Self::poll_tag) de fout.
    fn reap(&mut self) -> Result<u64> {
        Ok(0)
    }

    /// De meetlat van de driver als tekst voor één consoleregel (opdrachten,
    /// read-ahead, de traagste), en een teller die verandert zodra er iets
    /// gebeurde. Leeg en nul voor een driver zonder meetlat.
    fn stats(&self, out: &mut dyn fmt::Write) -> Result<u64, fmt::Error> {
        let _ = out;
        Ok(0)
    }

    /// Zet `op` op het device; de [`InFlight`] wacht op de completion.
    fn submit(&mut self, op: Op<'_>) -> Result<InFlight<'_, Self>>
    where
        Self: Sized,
    {
        self.start(op)?;
        Ok(InFlight { dev: self })
    }
}

/// Een geleende driver is ook een driver: zo leent de meetbank de schijf
/// (`Paced::new(&mut disk, Spin)`) en geeft hij hem daarna terug aan de
/// opslag.
impl<D: AsyncBlockDevice + ?Sized> AsyncBlockDevice for &mut D {
    fn max_transfer(&self) -> usize {
        (**self).max_transfer()
    }
    fn start(&mut self, op: Op<'_>) -> Result {
        (**self).start(op)
    }
    fn poll_done(&mut self, into: &mut [u8]) -> Poll<Result> {
        (**self).poll_done(into)
    }
    fn irq(&self) -> Option<&'static Signal> {
        (**self).irq()
    }
    fn poll_pace(&self) -> (u64, Duration) {
        (**self).poll_pace()
    }
    fn depth(&self) -> usize {
        (**self).depth()
    }
    fn start_tag(&mut self, op: Op<'_>) -> Result<usize> {
        (**self).start_tag(op)
    }
    fn poll_tag(&mut self, t: usize, into: &mut [u8]) -> Poll<Result> {
        (**self).poll_tag(t, into)
    }
    fn reap(&mut self) -> Result<u64> {
        (**self).reap()
    }
    fn stats(&self, out: &mut dyn fmt::Write) -> Result<u64, fmt::Error> {
        (**self).stats(out)
    }
}

/// De klok en de wekker van wie op een device wacht: de executor levert
/// hem (in de binary `exec.after`).
pub trait Pace {
    /// De future van [`sleep`](Pace::sleep).
    type Sleep: Future<Output = ()> + Unpin;
    /// Monotone nanoseconden.
    fn now(&self) -> u64;
    /// Een future die na `d` klaar is.
    fn sleep(&self, d: Duration) -> Self::Sleep;
}

/// Een [`Pace`] die pollt: nooit slapen, altijd opnieuw kijken. Voor
/// [`block_on`] vóór de executor (de meetbank) en de tests. De klok staat
/// stil, dus [`InFlight::done`] blijft in zijn yield-venster en toetst het
/// device bij elke ronde van `block_on`; de time-out van een verzoek is van
/// de driver (zijn eigen klok), dus ook zo wordt een stil device nooit een
/// eeuwige lus.
#[derive(Debug, Clone, Copy, Default)]
pub struct Spin;

impl Pace for Spin {
    type Sleep = core::future::Ready<()>;
    fn now(&self) -> u64 {
        0
    }
    fn sleep(&self, _d: Duration) -> Self::Sleep {
        core::future::ready(())
    }
}

/// Een opdracht die op het device staat. Hij leent de driver: zolang hij
/// leeft, kan er geen tweede opdracht bij.
#[must_use = "een opdracht die niemand afwacht, houdt de driver bezet"]
pub struct InFlight<'d, D: AsyncBlockDevice> {
    dev: &'d mut D,
}

impl<'d, D: AsyncBlockDevice> InFlight<'d, D> {
    /// Wacht op de completion; bij een lees komen de bytes in `into`.
    /// Tijdens het wachten draait de executor door: op de bel van de lijn
    /// (met de vangrail [`IRQ_GUARD`]), of pollend.
    pub fn done<'b, P: Pace>(self, into: &'b mut [u8], pace: &'b P) -> Done<'d, 'b, D, P> {
        Done {
            t0: pace.now(),
            dev: self.dev,
            into,
            pace,
            sleep: None,
        }
    }
}

/// De future van [`InFlight::done`].
///
/// Hij toetst het device bij élke poll, ook als niemand hem wekte: zo werkt
/// hij onder [`block_on`] (die pollt zonder wekker) precies als op de
/// executor. Gedropt vóór de completion is de opdracht niet weg; de driver
/// ruimt haar op bij de volgende submit, of weigert die met
/// [`Error::Busy`] tot het device klaar is.
#[must_use = "een future doet niets tot hij gepolld wordt"]
pub struct Done<'d, 'b, D: AsyncBlockDevice, P: Pace> {
    dev: &'d mut D,
    into: &'b mut [u8],
    pace: &'b P,
    t0: u64,
    sleep: Option<P::Sleep>,
}

impl<D: AsyncBlockDevice, P: Pace> Future for Done<'_, '_, D, P> {
    type Output = Result;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result> {
        // `Done` is `Unpin`: verwijzingen, een getal en een `Unpin`-slaap.
        let this = self.get_mut();
        loop {
            if let Poll::Ready(r) = this.dev.poll_done(this.into) {
                return Poll::Ready(r);
            }
            let period = match this.dev.irq() {
                Some(bell) => {
                    // Level-triggered: een bel van een vorige opdracht geeft
                    // hoogstens één ronde te veel kijken.
                    if Pin::new(&mut bell.wait()).poll(cx).is_ready() {
                        this.sleep = None;
                        continue;
                    }
                    IRQ_GUARD
                }
                None => {
                    let (spin, period) = this.dev.poll_pace();
                    if this.pace.now().saturating_sub(this.t0) < spin {
                        cx.waker().wake_by_ref();
                        return Poll::Pending;
                    }
                    period
                }
            };
            let pace = this.pace;
            let s = this.sleep.get_or_insert_with(|| pace.sleep(period));
            if Pin::new(s).poll(cx).is_pending() {
                return Poll::Pending;
            }
            this.sleep = None;
        }
    }
}

/// Eén lees van een batch ([`BlockIo::read_batch`]): `into.len()` bytes
/// vanaf `lba`, hoogstens één opdracht van het device groot
/// ([`max_transfer`](AsyncBlockDevice::max_transfer)).
#[derive(Debug)]
pub struct BatchRead<'b> {
    /// De eerste LBA.
    pub lba: u64,
    /// De bestemming.
    pub into: &'b mut [u8],
    /// De uitkomst, gezet door [`BlockIo::read_batch`].
    pub result: Result,
    /// Het ticket op het device, zolang de lees daar staat.
    ticket: Option<usize>,
    /// De uitkomst staat er.
    done: bool,
}

impl<'b> BatchRead<'b> {
    /// Een lees van `into.len()` bytes vanaf `lba`, nog niet gedaan.
    pub fn new(lba: u64, into: &'b mut [u8]) -> Self {
        Self {
            lba,
            into,
            result: Err(Error::Busy),
            ticket: None,
            done: false,
        }
    }
}

/// Het blokapparaat zoals hopfs het ziet: lezen, schrijven en flushen als
/// futures, van elke lengte (de brokken zijn van de implementatie).
pub trait BlockIo {
    /// Leest `buf.len()` bytes vanaf `lba`.
    fn read(&mut self, lba: u64, buf: &mut [u8]) -> impl Future<Output = Result>;
    /// Schrijft `buf` vanaf `lba`.
    fn write(&mut self, lba: u64, buf: &[u8]) -> impl Future<Output = Result>;
    /// Maakt alles wat geschreven is duurzaam.
    fn flush(&mut self) -> impl Future<Output = Result>;
    /// Leest alle `ops` en zet bij elk de uitkomst. Een device met tickets
    /// ([`Queue`]) zet ze allemaal tegelijk op het device en wacht er één
    /// keer op (Linux: de plug van blk-mq, io_uring in het klein); zonder
    /// tickets gaan ze na elkaar. Een fout in de ene laat de andere staan.
    fn read_batch(&mut self, ops: &mut [BatchRead<'_>]) -> impl Future<Output = ()> {
        async move {
            for op in ops.iter_mut() {
                op.result = self.read(op.lba, op.into).await;
                op.done = true;
            }
        }
    }
}

impl<D: BlockIo + ?Sized> BlockIo for &mut D {
    fn read(&mut self, lba: u64, buf: &mut [u8]) -> impl Future<Output = Result> {
        (**self).read(lba, buf)
    }
    fn read_batch(&mut self, ops: &mut [BatchRead<'_>]) -> impl Future<Output = ()> {
        (**self).read_batch(ops)
    }
    fn write(&mut self, lba: u64, buf: &[u8]) -> impl Future<Output = Result> {
        (**self).write(lba, buf)
    }
    fn flush(&mut self) -> impl Future<Output = Result> {
        (**self).flush()
    }
}

/// Een [`AsyncBlockDevice`] met zijn [`Pace`] als [`BlockIo`]: een verzoek
/// in brokken van `max_transfer`, elk brok een submit en een await.
pub struct Paced<D, P> {
    dev: D,
    pace: P,
}

impl<D: AsyncBlockDevice, P: Pace> Paced<D, P> {
    /// De driver `dev`, wachtend op `pace`.
    pub fn new(dev: D, pace: P) -> Self {
        Self { dev, pace }
    }

    /// De driver, veranderlijk.
    pub fn dev_mut(&mut self) -> &mut D {
        &mut self.dev
    }

    /// De brokmaat: de grootste transfer in hele LBA's, minstens één.
    fn step(&self) -> usize {
        let lba = LBA_SIZE as usize;
        let m = self.dev.max_transfer();
        (m - m % lba).max(lba)
    }
}

impl<D: AsyncBlockDevice, P: Pace> BlockIo for Paced<D, P> {
    async fn read(&mut self, lba: u64, buf: &mut [u8]) -> Result {
        let step = self.step();
        let mut l = lba;
        for chunk in buf.chunks_mut(step) {
            let len = chunk.len();
            self.dev
                .submit(Op::Read { lba: l, len })?
                .done(chunk, &self.pace)
                .await?;
            l += len as u64 / LBA_SIZE;
        }
        Ok(())
    }

    async fn write(&mut self, lba: u64, buf: &[u8]) -> Result {
        let step = self.step();
        let mut l = lba;
        for chunk in buf.chunks(step) {
            self.dev
                .submit(Op::Write {
                    lba: l,
                    data: chunk,
                })?
                .done(&mut [], &self.pace)
                .await?;
            l += chunk.len() as u64 / LBA_SIZE;
        }
        Ok(())
    }

    async fn flush(&mut self) -> Result {
        self.dev.submit(Op::Flush)?.done(&mut [], &self.pace).await
    }
}

/// Draait een future af zonder executor: pollen tot hij klaar is. Voor wat
/// vóór `exec.run` op de schijf wacht (de mount bij de boot, de meetbank) en
/// voor de tests; op de executor hoort hij nooit, want hij houdt de core
/// vast. Het is geen tweede driverpad: dezelfde [`Paced`]-futures, dezelfde
/// `start` en `poll_done`, alleen zonder iemand die intussen iets anders
/// doet.
pub fn block_on<F: Future>(f: F) -> F::Output {
    let mut f = core::pin::pin!(f);
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Poll::Ready(v) = f.as_mut().poll(&mut cx) {
            return v;
        }
        core::hint::spin_loop();
    }
}

mod queue;
pub use queue::{MAX_DEPTH, Queue};

#[cfg(test)]
mod tests;
