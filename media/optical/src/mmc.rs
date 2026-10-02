//! MMC-lezingen boven async BOT. Alleen expliciete toegang raakt de disc;
//! deze laag start geen achtergrondpoll of periodieke capaciteitslezing.
use crate::{
    Data, Error, Result,
    asynchronous::{Bot, Transport},
};

/// Blu-ray-logische sectormaat.
pub const SECTOR: usize = 2048;
/// Een leescommando blijft klein genoeg voor herstel en netwerkbeurten.
pub const READ_BYTES: usize = 32 << 10;
/// Eén drive.
pub struct Drive<T> {
    bot: Bot<T>,
}
impl<T: Transport> Drive<T> {
    /// Opent uitsluitend een optical peripheral (MMC type 5).
    pub async fn open(t: T) -> Result<Self> {
        let mut bot = Bot::new(t);
        let mut inquiry = [0; 36];
        let n = bot
            .execute(&[0x12, 0, 0, 0, 36, 0], Data::In(&mut inquiry))
            .await?;
        if n != 36 || inquiry[0] & 31 != 5 {
            return Err(Error::Invalid);
        }
        Ok(Self { bot })
    }
    /// De geserialiseerde BOT voor expliciete apparaatcommando's.
    pub fn bot(&mut self) -> &mut Bot<T> {
        &mut self.bot
    }
    /// Capaciteit, zonder overloop bij de READ CAPACITY(10)-sentinel.
    pub async fn size(&mut self) -> Result<u64> {
        let mut b = [0; 8];
        if self
            .bot
            .execute(&[0x25, 0, 0, 0, 0, 0, 0, 0, 0, 0], Data::In(&mut b))
            .await?
            != 8
        {
            return Err(Error::Invalid);
        }
        let last = u32::from_be_bytes([b[0], b[1], b[2], b[3]]);
        let size = u32::from_be_bytes([b[4], b[5], b[6], b[7]]);
        if last == u32::MAX || size != SECTOR as u32 {
            return Err(Error::Invalid);
        }
        Ok((u64::from(last) + 1) * u64::from(size))
    }
    /// Leest één begrensde sectorreeks. Geen verborgen retries na discwissels.
    pub async fn sectors(&mut self, lba: u32, dst: &mut [u8]) -> Result {
        let n = dst.len() / SECTOR;
        if n == 0
            || !dst.len().is_multiple_of(SECTOR)
            || dst.len() > READ_BYTES
            || n > u16::MAX as usize
            || u64::from(lba) + n as u64 > u64::from(u32::MAX) + 1
        {
            return Err(Error::Invalid);
        }
        let mut cdb = [0; 10];
        cdb[0] = 0x28;
        cdb[2..6].copy_from_slice(&lba.to_be_bytes());
        cdb[7..9].copy_from_slice(&(n as u16).to_be_bytes());
        if self.bot.execute(&cdb, Data::In(dst)).await? != dst.len() {
            return Err(Error::Invalid);
        }
        Ok(())
    }
    /// Onuitgelijnde bytelezing; READ(10) toetst zelf het bereik van het
    /// medium. Elke directe transfer is maximaal 32 KiB; een randsector staat
    /// op de stack.
    pub async fn read_at(&mut self, off: u64, dst: &mut [u8]) -> Result<usize> {
        let n = dst.len();
        let mut done = 0;
        while done < n {
            let at = off.checked_add(done as u64).ok_or(Error::Invalid)?;
            let lba = u32::try_from(at / SECTOR as u64).map_err(|_| Error::Invalid)?;
            let skip = (at % SECTOR as u64) as usize;
            let len = n - done;
            if skip != 0 || len < SECTOR {
                let mut sector = [0; SECTOR];
                self.sectors(lba, &mut sector).await?;
                let take = len.min(SECTOR - skip);
                dst[done..done + take].copy_from_slice(&sector[skip..skip + take]);
                done += take;
            } else {
                let take =
                    len.min(READ_BYTES).min(self.bot.transport().max_transfer()) / SECTOR * SECTOR;
                if take == 0 {
                    return Err(Error::Invalid);
                }
                self.sectors(lba, &mut dst[done..done + take]).await?;
                done += take;
            }
        }
        Ok(done)
    }
}
