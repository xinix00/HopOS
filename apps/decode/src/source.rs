//! Waar de bitstream vandaan komt: een bestand in het zicht van de app (een
//! volume uit de jobspec) of een URL over `appnet`.
//!
//! Beide schrijven rechtstreeks in de invoerbuffer die straks naar de
//! decoder gaat: geen tussenkopie, want die buffer IS het geheugen dat de
//! kern als grant aan het ijzer geeft. Een bestand gaat in happen van
//! [`MAX_CHUNK`] over de system-API; een URL is één GET met leanhttp over
//! een TCP-verbinding van de app, de body in stukken zoals hij binnenkomt.

use crate::Sys;
use applib::appnet::{self, TcpStream};
use applib::rt::Exec;
use applib::sys::{self, MAX_CHUNK};
use applib::tcp::TcpConn;
use core::fmt;
use core::time::Duration;
use leanhttp::{Dial, Response, Target};

/// Hoe lang een verbinding naar de server van de stream mag duren.
const CONNECT: Duration = Duration::from_secs(10);

/// Waarom de stream niet verder kwam.
#[derive(Debug)]
pub(crate) enum SourceError {
    /// De system-API (een bestand).
    Sys(sys::Error),
    /// HTTP (een URL).
    Http(leanhttp::Error),
    /// De server zei geen 200.
    Status(u16),
}

impl fmt::Display for SourceError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            SourceError::Sys(e) => write!(f, "{e}"),
            SourceError::Http(e) => write!(f, "http: {e}"),
            SourceError::Status(s) => write!(f, "http status {s}"),
        }
    }
}

/// De bron van de bitstream.
#[expect(
    clippy::large_enum_variant,
    reason = "één bron per meting, op de stack van de taak; een Box is een allocatie om niets"
)]
pub(crate) enum Source {
    /// Een bestand in het zicht van de app, gelezen vanaf `off`.
    File {
        /// Het pad (een volume uit de jobspec, zoals `/data/clip.hevc`).
        path: &'static str,
        /// Hoeveel er al gelezen is.
        off: u64,
    },
    /// De body van een GET.
    Url(Response<TcpConn>),
}

/// Is `name` een URL (en geen pad)?
pub(crate) fn is_url(name: &str) -> bool {
    name.starts_with("http://") || name.starts_with("https://")
}

/// Het deel van een naam waar de extensie in staat: een URL zonder query en
/// fragment (`http://x/film.hevc?t=1` is `film.hevc`).
pub(crate) fn file_part(name: &str) -> &str {
    let end = name.find(['?', '#']).unwrap_or(name.len());
    let name = name.get(..end).unwrap_or(name);
    name.rsplit('/').next().unwrap_or(name)
}

impl Source {
    /// Opent de bron: een bestand is er meteen (de eerste lezing zegt of het
    /// bestaat), een URL is één GET met redirects.
    pub(crate) async fn open(
        exec: &'static Exec,
        name: &'static str,
    ) -> Result<Source, SourceError> {
        if !is_url(name) {
            return Ok(Source::File { path: name, off: 0 });
        }
        let mut d = Dialer { exec };
        let r = leanhttp::get(&mut d, name)
            .await
            .map_err(SourceError::Http)?;
        if r.status != 200 {
            return Err(SourceError::Status(r.status));
        }
        Ok(Source::Url(r))
    }

    /// Vult `dst` zo ver als de bron gaat; minder dan `dst.len()` is het
    /// einde van de stream.
    pub(crate) async fn fill(
        &mut self,
        sys: &mut Sys,
        dst: &mut [u8],
    ) -> Result<usize, SourceError> {
        let mut n = 0;
        while n < dst.len() {
            let room = dst.get_mut(n..).unwrap_or_default();
            let got = match self {
                Source::File { path, off } => {
                    let take = room.len().min(MAX_CHUNK);
                    let slice = room.get_mut(..take).unwrap_or_default();
                    let got = sys
                        .read_into(path, *off, slice)
                        .await
                        .map_err(SourceError::Sys)?;
                    *off += got as u64;
                    got
                }
                Source::Url(r) => r.read(room).await.map_err(SourceError::Http)?,
            };
            if got == 0 {
                break;
            }
            n += got;
        }
        Ok(n)
    }
}

/// Verbindingen voor leanhttp over de stack van de app: de naam via de
/// DNS-server uit de env (een adres meteen), dan TCP met een termijn.
struct Dialer {
    exec: &'static Exec,
}

impl Dial for Dialer {
    type Conn = TcpConn;

    async fn dial(&mut self, t: Target<'_>) -> leanhttp::Result<TcpConn> {
        let ip = appnet::resolve(t.host)
            .await
            .map_err(|_| leanhttp::Error::Connect)?;
        let s = TcpStream::connect_timeout(ip, t.port, CONNECT)
            .await
            .map_err(|_| leanhttp::Error::Connect)?;
        Ok(TcpConn::new(s, self.exec))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_url_is_told_from_a_path_and_keeps_its_extension() {
        assert!(is_url("http://10.0.2.2:8000/clip.hevc"));
        assert!(!is_url("/data/clip.hevc"));
        assert_eq!(file_part("http://h:1/a/film.hevc?t=1#x"), "film.hevc");
        assert_eq!(file_part("/data/clip.h264"), "clip.h264");
        assert_eq!(file_part("clip.av1"), "clip.av1");
    }
}
