//! Apparaatcalls via expliciete mounts en een eigen actor. USB wacht nooit
//! in de HopFS-actor. De verbinding verplaatst haar buffers en wacht tot de
//! eigenaar ze teruggeeft, ook als de peer intussen verdween.
use crate::{
    Error, Result, Slot,
    rpc::{self, FsCall, FsDone, PathBuf},
    slots::{Reply, Servicers},
};
use sync::mpsc::Mailbox;

/// Begrensde apparaatwachtrij.
pub type Inbox<'a> = Mailbox<Request<'a>, 8>;
/// Eén aanvraag met antwoordplek van de verbinding.
pub struct Request<'a> {
    /// De buffers en identiteit van de aanvrager.
    pub call: FsCall,
    reply: &'a Reply,
}
impl Request<'_> {
    /// Geeft ook bij fouten beide buffers terug.
    pub fn finish(self, result: Result<(u64, usize)>) {
        self.reply.put_fs(FsDone {
            buf: self.call.buf,
            out: self.call.out,
            result,
        });
    }
}
/// Alleen een expliciete mount op `/devices/discN` geeft toegang. Een
/// gelijknamig bestand in een eigen taakroot geeft geen apparaatbevoegdheid.
pub fn target(svc: &Servicers, slot: Slot, generation: u32, path: &[u8]) -> Result<Option<usize>> {
    if svc.current(slot) != Some(generation) {
        return Err(Error::Denied);
    }
    let mut resolved = PathBuf::new();
    let mounted = svc
        .with_mounts(slot, |m| rpc::resolve(slot, m, path, &mut resolved))
        .ok_or(Error::Denied)??;
    let path = resolved.as_bytes();
    if mounted.is_none() || !path.starts_with(b"/devices/") {
        return Ok(None);
    }
    let tail = path.strip_prefix(b"/devices/disc").ok_or(Error::Denied)?;
    if tail.len() != 1 || !(b'0'..=b'7').contains(&tail[0]) {
        return Err(Error::Denied);
    }
    Ok(Some(usize::from(tail[0] - b'0')))
}
/// Eén uitwisseling met de eigenaar. Er is geen timeout die uitgeleende
/// buffers kan laten hergebruiken; het apparaat bewaakt zijn eigen deadline.
pub async fn call<'a>(
    inbox: &Inbox<'a>,
    reply: &'a Reply,
    call: FsCall,
) -> core::result::Result<FsDone, FsCall> {
    let _ = reply.done.take();
    if let Err(sync::Full(req)) = inbox.try_send(Request { call, reply }) {
        return Err(req.call);
    }
    loop {
        reply.done.wait().await;
        if let Some(done) = reply.take_fs() {
            return Ok(done);
        }
    }
}
