//! De terugleesproef van `-verify`: het geschreven image teruggelezen zoals
//! een bootloader het leest (de MBR, de BPB, de root, de clusterketen, de
//! lange namen), los van de code die het schreef, en elk bestand en elke
//! raw blob vergeleken met wat erin ging. Een fout in de kaartbouwer is dan
//! hier rood, niet pas op de seriële console. Was de proef van
//! image/radxa-zero3.sh (in Python); nu geldt hij voor elke kaart.

use std::collections::HashMap;

/// Een entry in een directory: attribuut, eerste cluster, maat.
#[derive(Clone, Copy, Debug)]
struct Entry {
    /// 0x10 is een directory.
    attr: u8,
    /// De eerste cluster (0 voor een leeg bestand).
    first: usize,
    /// De maat in bytes.
    size: usize,
}

/// De FAT16-partitie van een image, zoals zijn BPB hem beschrijft.
struct Fat<'a> {
    /// Het hele image.
    img: &'a [u8],
    /// Het byte-offset van de eerste FAT.
    fat: usize,
    /// Het byte-offset van de rootdirectory.
    root: usize,
    /// Het byte-offset van cluster 2.
    data: usize,
    /// Een cluster in bytes.
    clus: usize,
}

/// `n` bytes vanaf `off`, of een fout als het image daar ophoudt.
fn bytes(img: &[u8], off: usize, n: usize) -> Result<&[u8], String> {
    off.checked_add(n)
        .and_then(|end| img.get(off..end))
        .ok_or(format!("the image ends before byte {off:#x} + {n}"))
}

/// Een little-endian u16 op `off`.
fn u16_at(img: &[u8], off: usize) -> Result<usize, String> {
    let b = bytes(img, off, 2)?;
    Ok(usize::from(u16::from_le_bytes([b[0], b[1]])))
}

/// Een little-endian u32 op `off`.
fn u32_at(img: &[u8], off: usize) -> Result<usize, String> {
    let b = bytes(img, off, 4)?;
    usize::try_from(u32::from_le_bytes([b[0], b[1], b[2], b[3]])).map_err(|e| e.to_string())
}

impl<'a> Fat<'a> {
    /// De partitie uit de eerste MBR-entry en haar bootsector.
    fn open(img: &'a [u8]) -> Result<Self, String> {
        if bytes(img, 510, 2)? != [0x55, 0xAA] {
            return Err("no MBR boot signature".into());
        }
        let start = u32_at(img, 446 + 8)? * 512;
        let sec = u16_at(img, start + 11)?;
        let spc = usize::from(bytes(img, start + 13, 1)?[0]);
        let reserved = u16_at(img, start + 14)?;
        let nfat = usize::from(bytes(img, start + 16, 1)?[0]);
        let root_entries = u16_at(img, start + 17)?;
        let spf = u16_at(img, start + 22)?;
        if sec == 0 || spc == 0 {
            return Err("the boot sector has no sector or cluster size".into());
        }
        let fat = start + reserved * sec;
        let root = fat + nfat * spf * sec;
        Ok(Self {
            img,
            fat,
            root,
            data: root + root_entries * 32,
            clus: sec * spc,
        })
    }

    /// De bytes van een clusterketen vanaf `first`, afgekapt op `size`
    /// (`None`: een directory, de hele keten).
    fn chain(&self, first: usize, size: Option<usize>) -> Result<Vec<u8>, String> {
        let mut out = Vec::new();
        let mut c = first;
        let mut seen = 0usize;
        while (2..0xFFF8).contains(&c) {
            seen += 1;
            if seen > self.img.len() / self.clus {
                return Err(format!("cluster loop in the chain from {first}"));
            }
            out.extend_from_slice(bytes(self.img, self.data + (c - 2) * self.clus, self.clus)?);
            c = u16_at(self.img, self.fat + 2 * c)?;
        }
        match size {
            Some(n) if out.len() < n => Err(format!(
                "the chain from cluster {first} holds {} bytes, the entry says {n}",
                out.len()
            )),
            Some(n) => {
                out.truncate(n);
                Ok(out)
            }
            None => Ok(out),
        }
    }

    /// De entries van een directory-tabel, op naam (klein geschreven: FAT
    /// kent geen hoofdletters, een 8.3-naam staat in kapitalen).
    fn entries(raw: &[u8]) -> HashMap<String, Entry> {
        let mut out = HashMap::new();
        let mut lfn: Vec<(u8, Vec<u16>)> = Vec::new();
        for e in raw.chunks_exact(32) {
            match (e[0], e[11]) {
                (0, _) => break,
                (0xE5, _) => lfn.clear(),
                (seq, 0x0F) => {
                    let units = [1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30]
                        .iter()
                        .map(|&o| u16::from_le_bytes([e[o], e[o + 1]]))
                        .collect();
                    lfn.push((seq & 0x1F, units));
                }
                (_, attr) if attr & 0x08 != 0 => lfn.clear(),
                (_, attr) => {
                    let name = if lfn.is_empty() {
                        let base = String::from_utf8_lossy(&e[0..8]).trim_end().to_owned();
                        let ext = String::from_utf8_lossy(&e[8..11]).trim_end().to_owned();
                        if ext.is_empty() {
                            base
                        } else {
                            format!("{base}.{ext}")
                        }
                    } else {
                        lfn.sort_by_key(|(seq, _)| *seq);
                        let units: Vec<u16> = lfn
                            .drain(..)
                            .flat_map(|(_, u)| u)
                            .take_while(|&u| u != 0 && u != 0xFFFF)
                            .collect();
                        String::from_utf16_lossy(&units)
                    };
                    let first = usize::from(u16::from_le_bytes([e[26], e[27]]));
                    let size = usize::try_from(u32::from_le_bytes([e[28], e[29], e[30], e[31]]))
                        .unwrap_or(usize::MAX);
                    out.insert(name.to_lowercase(), Entry { attr, first, size });
                }
            }
        }
        out
    }

    /// Het bestand op `path` (`/` tussen directories), teruggelezen.
    fn read(&self, path: &str) -> Result<Vec<u8>, String> {
        let mut dir = Self::entries(bytes(self.img, self.root, self.data - self.root)?);
        let mut parts = path.split('/').peekable();
        while let Some(p) = parts.next() {
            let e = *dir
                .get(&p.to_lowercase())
                .ok_or(format!("{path}: no {p} on the card"))?;
            if parts.peek().is_none() {
                if e.attr & 0x10 != 0 {
                    return Err(format!("{path} is a directory on the card"));
                }
                return self.chain(e.first, Some(e.size));
            }
            if e.attr & 0x10 == 0 {
                return Err(format!("{path}: {p} is no directory on the card"));
            }
            dir = Self::entries(&self.chain(e.first, None)?);
        }
        Err(format!("{path}: empty path"))
    }
}

/// Leest elk bestand (naam op de kaart, inhoud) en elke raw blob (naam,
/// byte-offset, inhoud) terug uit `img` en vergelijkt.
pub(crate) fn check(
    img: &[u8],
    raws: &[(&str, usize, &[u8])],
    files: &[(&str, &[u8])],
) -> Result<(), String> {
    for (name, off, data) in raws {
        if bytes(img, *off, data.len())? != *data {
            return Err(format!("raw {name} at byte {off:#x} differs on the card"));
        }
    }
    let fat = Fat::open(img)?;
    for (name, data) in files {
        let got = fat.read(name)?;
        if got != *data {
            return Err(format!(
                "{name} differs on the card ({} bytes back, {} in)",
                got.len(),
                data.len()
            ));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::check;
    use crate::fat;

    fn card() -> fat::Card {
        fat::Card {
            size_mb: 64,
            start: 8192,
            label: "bootfs".to_owned(),
            vol_entry: true,
        }
    }

    #[test]
    fn reads_back_what_went_in() {
        let files = vec![
            fat::File {
                name: "config.txt".to_owned(),
                data: b"arm_64bit=1\n".to_vec(),
            },
            fat::File {
                name: "extlinux/extlinux.conf".to_owned(),
                data: vec![0x41; 5000],
            },
            fat::File {
                name: "bcm2711-rpi-4-b.dtb".to_owned(),
                data: (0..70_000u32).map(|i| i as u8).collect(),
            },
            fat::File {
                name: "empty".to_owned(),
                data: Vec::new(),
            },
        ];
        let raw = fat::Blob {
            name: "donor".to_owned(),
            off: 32768,
            data: vec![7; 1024],
        };
        let want: Vec<(String, Vec<u8>)> = files
            .iter()
            .map(|f| (f.name.clone(), f.data.clone()))
            .collect();
        let img = fat::build(&card(), std::slice::from_ref(&raw), files).unwrap();
        let named: Vec<(&str, &[u8])> = want
            .iter()
            .map(|(n, d)| (n.as_str(), d.as_slice()))
            .collect();
        check(&img, &[("donor", 32768, &raw.data)], &named).unwrap();
    }

    #[test]
    fn a_changed_byte_is_red() {
        let data = vec![0x5A; 3000];
        let files = vec![fat::File {
            name: "kernel8.img".to_owned(),
            data: data.clone(),
        }];
        let mut img = fat::build(&card(), &[], files).unwrap();
        let at = img.iter().rposition(|&b| b == 0x5A).unwrap();
        img[at] = 0;
        let err = check(&img, &[], &[("kernel8.img", &data)]).unwrap_err();
        assert!(err.contains("differs"), "{err}");
        assert!(check(&img, &[], &[("missing.txt", b"x")]).is_err());
    }
}
