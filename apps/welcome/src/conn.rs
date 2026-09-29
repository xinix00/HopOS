//! Een `applib::appnet::TcpStream` als verbinding voor leanhttp.
//!
//! Bezit de stroom en zijn twee termijnen. De stroom heeft alleen async
//! methodes (`read`, `write`); leanhttp vraagt poll-methodes. De brug maakt
//! per poll de future van één `read` of `write` en pollt hem één keer: bij
//! `WouldBlock` zet die de waker van deze taak op het handvat in de stack en
//! geeft `Pending`, en wegvallen kost niets, want hij hield niets vast
//! buiten die registratie.
//!
//! Dezelfde vorm als `hop-http/src/conn.rs` in de hop-repo; die crate is
//! van Hop en hangt aan een HopOS-tag, dus een app in deze repo kan hem niet
//! gebruiken zonder een pad over de repo-grens. Komt er een derde app, dan
//! hoort dit in applib (achter een feature, want niet elke app praat HTTP).
//!
//! De termijnen van de server (verzoekkop, body, schrijven) lopen op het
//! timerwiel van de executor: een verbinding die zwijgt, wordt na haar
//! termijn gewekt en gesloten, en houdt geen taak uit de pool vast.

use alloc::boxed::Box;
use applib::appnet::{NetError, StackError, TcpStream};
use applib::rt::Exec;
use core::future::Future;
use core::pin::{Pin, pin};
use core::task::{Context, Poll};
use core::time::Duration;
use leanhttp::{AsyncRead, AsyncWrite, Close, IoError};

/// Een wekker op het timerwiel: de future van `Exec::until`.
type Alarm = Pin<Box<dyn Future<Output = ()>>>;

/// Eén richting: de deadline en de wekker die erbij hoort.
#[derive(Default)]
struct Deadline {
    at: Option<u64>,
    alarm: Option<Alarm>,
}

impl Deadline {
    /// Zet de termijn op `d` vanaf nu; `None` wist hem.
    fn set(&mut self, exec: &'static Exec, d: Option<Duration>) {
        self.at = d.map(|d| {
            let ns = u64::try_from(d.as_nanos()).unwrap_or(u64::MAX);
            exec.now().saturating_add(ns)
        });
        self.alarm = self.at.map(|at| -> Alarm { Box::pin(exec.until(at)) });
    }

    /// Is de termijn verstreken? Zo niet, dan staat de wekker op de waker
    /// van `cx`.
    fn is_expired(&mut self, exec: &'static Exec, cx: &mut Context<'_>) -> bool {
        let Some(at) = self.at else {
            return false;
        };
        if exec.now() >= at {
            return true;
        }
        match self.alarm.as_mut() {
            // De wekker registreert de waker van deze taak in het wiel; is
            // hij net afgelopen, dan is de termijn ook verstreken.
            Some(a) => a.as_mut().poll(cx).is_ready(),
            None => false,
        }
    }
}

/// Een TCP-verbinding van de app als leanhttp-verbinding.
pub(crate) struct TcpConn {
    stream: Option<TcpStream>,
    exec: &'static Exec,
    read: Deadline,
    write: Deadline,
    cap: Duration,
}

impl TcpConn {
    /// Neemt `stream` over; de termijnen lopen op `exec`, en elke
    /// leestermijn wordt afgekapt op `cap`.
    ///
    /// Waarom de kap: de keep-alive-stilte van leanhttp is 60 s, en een
    /// browser houdt zijn verbinding zolang open. Met een vaste pool van
    /// werkers houdt zo'n stille verbinding een werker vast; na `cap` gaat
    /// hij dicht en opent de browser gewoon een nieuwe.
    pub(crate) fn new(stream: TcpStream, exec: &'static Exec, cap: Duration) -> Self {
        Self {
            stream: Some(stream),
            exec,
            read: Deadline::default(),
            write: Deadline::default(),
            cap,
        }
    }
}

/// Een netfout als fout van de verbinding.
fn io_error(e: NetError) -> IoError {
    match e {
        NetError::Timeout | NetError::Stack(StackError::DeadlineExceeded) => IoError::TimedOut,
        NetError::Stack(StackError::Reset) => IoError::Reset,
        NetError::Stack(StackError::Closed | StackError::TcpClosed | StackError::StackClosed) => {
            IoError::Closed
        }
        _ => IoError::Other,
    }
}

impl AsyncRead for TcpConn {
    fn poll_read(&mut self, cx: &mut Context<'_>, buf: &mut [u8]) -> Poll<Result<usize, IoError>> {
        let Some(s) = self.stream.as_mut() else {
            return Poll::Ready(Err(IoError::Closed));
        };
        if let Poll::Ready(r) = pin!(s.read(buf)).poll(cx) {
            return Poll::Ready(r.map_err(io_error));
        }
        if self.read.is_expired(self.exec, cx) {
            return Poll::Ready(Err(IoError::TimedOut));
        }
        Poll::Pending
    }

    fn set_read_timeout(&mut self, timeout: Option<Duration>) -> Result<(), IoError> {
        // `None` (de server leest nu niet) blijft `None`.
        let timeout = timeout.map(|t| t.min(self.cap));
        self.read.set(self.exec, timeout);
        Ok(())
    }
}

impl AsyncWrite for TcpConn {
    fn poll_write(&mut self, cx: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize, IoError>> {
        let Some(s) = self.stream.as_mut() else {
            return Poll::Ready(Err(IoError::Closed));
        };
        if let Poll::Ready(r) = pin!(s.write(buf)).poll(cx) {
            return Poll::Ready(r.map_err(io_error));
        }
        if self.write.is_expired(self.exec, cx) {
            return Poll::Ready(Err(IoError::TimedOut));
        }
        Poll::Pending
    }

    fn set_write_timeout(&mut self, timeout: Option<Duration>) -> Result<(), IoError> {
        self.write.set(self.exec, timeout);
        Ok(())
    }
}

impl Close for TcpConn {
    fn poll_close(&mut self, _cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        // Synchroon: FIN na de gebufferde data, de pomp stuurt hem. Twee keer
        // sluiten is één keer sluiten.
        match self.stream.take() {
            Some(s) => Poll::Ready(s.close().map_err(io_error)),
            None => Poll::Ready(Ok(())),
        }
    }
}
