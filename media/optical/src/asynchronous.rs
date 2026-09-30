//! Async BOT voor de USB-eigenaartaak. Iedere await verplaatst een transfer
//! naar die eigenaar; de drive blijft één opdracht tegelijk bezitten.
use super::{
    CSW_LEN, CSW_SIGNATURE, Data, Error, OP_REQUEST_SENSE, Result, Sense, Status, UsbError,
    build_cbw,
};
use core::future::Future;
/// Het async transport; nooit een controllerlening over een await.
pub trait Transport {
    /// Stuurt een complete bulkbuffer.
    fn out(&mut self, data: &[u8]) -> impl Future<Output = Result<(), UsbError>>;
    /// Ontvangt maximaal de aangeboden lengte.
    fn input(&mut self, data: &mut [u8]) -> impl Future<Output = Result<usize, UsbError>>;
    /// Herstelt BOT en beide endpoints.
    fn reset_recovery(&mut self) -> impl Future<Output = Result<(), UsbError>>;
    /// Grootste transfer.
    fn max_transfer(&self) -> usize;
}
/// Ruwe, begrensde SCSI-uitkomst.
pub struct Completion {
    /// Aantal databytes.
    pub transferred: usize,
    /// Nul of SCSI CHECK CONDITION (2).
    pub status: u8,
    /// Sensebuffer, ongebruikte bytes nul.
    pub sense: [u8; 64],
    /// Geldige sensebytes.
    pub sense_len: usize,
}
/// Eén asynchroon BOT-gesprek.
pub struct Bot<T> {
    t: T,
    tag: u32,
}
impl<T: Transport> Bot<T> {
    /// Een gesprek over `t`.
    pub fn new(t: T) -> Bot<T> {
        Bot { t, tag: 0 }
    }

    /// De transport, voor wie hem terug wil.
    pub fn transport(&mut self) -> &mut T {
        &mut self.t
    }

    /// Wisselt één SCSI-commando uit; geeft de bytes van de datafase, met de
    /// sense als fout bij CHECK CONDITION. Hier wordt nooit herhaald: een
    /// vendor-commando kan de staat van de drive veranderen ook als zijn
    /// antwoord verloren gaat.
    pub async fn execute(&mut self, cdb: &[u8], data: Data<'_>) -> Result<usize> {
        if cdb.is_empty() || cdb.len() > 16 {
            return Err(Error::Invalid);
        }
        let max = self.t.max_transfer();
        if data.len() > max {
            return Err(Error::TooLarge {
                len: data.len(),
                max,
            });
        }
        let (n, status) = self.exchange(cdb, data).await?;
        match status {
            Status::Passed => Ok(n),
            Status::Failed => Err(Error::Check(self.sense().await?)),
        }
    }

    /// Ruwe status en sense voor het device-ABI; nooit een commando herhalen.
    pub async fn command(&mut self, cdb: &[u8], data: Data<'_>) -> Result<Completion> {
        if cdb.is_empty() || cdb.len() > 16 || data.len() > self.t.max_transfer() {
            return Err(Error::Invalid);
        }
        let (n, status) = self.exchange(cdb, data).await?;
        let mut r = Completion {
            transferred: n,
            status: 0,
            sense: [0; 64],
            sense_len: 0,
        };
        if status == Status::Failed {
            r.status = 2;
            let cdb = [OP_REQUEST_SENSE, 0, 0, 0, 64, 0];
            let (n, status) = self.exchange(&cdb, Data::In(&mut r.sense)).await?;
            if status != Status::Passed || Sense::parse(&r.sense[..n]).is_none() {
                return Err(Error::NoSense);
            }
            r.sense_len = n;
        }
        Ok(r)
    }

    /// REQUEST SENSE: waarom zei de drive nee?
    pub async fn sense(&mut self) -> Result<Sense> {
        let mut buf = [0u8; 64];
        let cdb = [OP_REQUEST_SENSE, 0, 0, 0, buf.len() as u8, 0];
        let (n, status) = self.exchange(&cdb, Data::In(&mut buf)).await?;
        if status != Status::Passed || n < 8 {
            return Err(Error::NoSense);
        }
        Sense::parse(&buf[..n]).ok_or(Error::NoSense)
    }

    /// Reset en geeft `e` terug; faalt de reset ook, dan die fout.
    async fn reset(&mut self, e: Error) -> Error {
        match self.t.reset_recovery().await {
            Ok(()) => e,
            Err(r) => Error::Reset(r),
        }
    }

    /// De drie fasen van één commando.
    async fn exchange(&mut self, cdb: &[u8], data: Data<'_>) -> Result<(usize, Status)> {
        self.tag = self.tag.wrapping_add(1);
        let tag = self.tag;
        let len = data.len();
        let input = matches!(data, Data::In(_));
        if let Err(e) = self.t.out(&build_cbw(tag, len as u32, input, cdb)).await {
            // Het commando kwam niet eens weg: opnieuw beginnen is het enige
            // eerlijke antwoord, anders wacht de drive op data die nooit komt.
            return Err(self.reset(Error::Command(e)).await);
        }
        // Een gestalde datafase is hoe een drive "dat commando ken ik niet"
        // zegt; de reden staat in de status die hierna komt.
        let (mut n, data_err) = match data {
            Data::None => (0, None),
            Data::Out(b) => match self.t.out(b).await {
                Ok(()) => (b.len(), None),
                Err(e) => (0, Some(e)),
            },
            Data::In(b) => match self.t.input(b).await {
                Ok(k) => (k.min(b.len()), None),
                Err(e) => (0, Some(e)),
            },
        };
        let (status, residue) = match self.status(tag).await {
            Ok(s) => s,
            Err(e) => return Err(self.reset(e).await),
        };
        if residue as usize > len {
            return Err(self
                .reset(Error::Status {
                    at: 8,
                    got: residue,
                })
                .await);
        }
        n = n.min(len - residue as usize);
        match (status, data_err) {
            (Some(Status::Passed), Some(e)) => Err(Error::Data(e)),
            (Some(s), _) => Ok((n, s)),
            (None, _) => Err(self.reset(Error::Phase).await),
        }
    }

    /// Leest en toetst de CSW; `None` als status is phase error.
    async fn status(&mut self, tag: u32) -> Result<(Option<Status>, u32)> {
        let mut csw = [0u8; CSW_LEN];
        // Eén herkansing: de spec schrijft voor dat een gestalde
        // status-endpoint vrijgemaakt wordt en de CSW daarna alsnog komt.
        let n = match self.t.input(&mut csw).await {
            Ok(n) => n,
            Err(_) => self
                .t
                .input(&mut csw)
                .await
                .map_err(Error::StatusTransport)?,
        };
        let le = |i: usize| u32::from_le_bytes([csw[i], csw[i + 1], csw[i + 2], csw[i + 3]]);
        if n < CSW_LEN {
            return Err(Error::Status {
                at: 0,
                got: n as u32,
            });
        }
        if le(0) != CSW_SIGNATURE {
            return Err(Error::Status { at: 0, got: le(0) });
        }
        if le(4) != tag {
            return Err(Error::Status { at: 4, got: le(4) });
        }
        let status = match csw[12] {
            0 => Some(Status::Passed),
            1 => Some(Status::Failed),
            2 => None,
            s => {
                return Err(Error::Status {
                    at: 12,
                    got: u32::from(s),
                });
            }
        };
        Ok((status, le(8)))
    }
}
