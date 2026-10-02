//! Het image zelf: de MBR, de raw blobs en de FAT16-partitie, in geheugen.
//! Bezit niets buiten de aanroep; de bestanden leest `main`.
//!
//! De geometrie is die van het Sipeed-donorimage, want dat is wat de BROM
//! van de LicheeRV aantoonbaar leest (Go, gemeten 30-07): partitietype 0x0C,
//! 512-byte sectoren, 2 KB-clusters, 4 gereserveerde sectoren, 2 FAT's en
//! 512 root-entries. Type 0x0C (FAT32 LBA, dat vendors ook voor een FAT16
//! zetten) is meteen waarom macOS en Windows de partitie na het flashen
//! mounten. Namen die niet in 8.3 passen krijgen VFAT-LFN-entries (U-Boot,
//! de Pi-firmware en EDK2 lezen die); een naam die wel past krijgt er geen,
//! zodat het image van de LicheeRV byte voor byte blijft wat zijn BROM
//! bewezen leest. Alles is deterministisch: vaste tijdstempel, vaste
//! volume-id, clusters in argumentvolgorde.

use std::collections::HashSet;
use std::fmt;

/// De sectormaat; alles wat firmware leest is LBA.
const SECTOR: usize = 512;
/// Sectoren per cluster: 2 KB-clusters.
const SEC_PER_CLUS: usize = 4;
/// De bootsector plus drie.
const RESERVED: usize = 4;
/// Twee FAT-kopieën; de tweede is een exacte kopie van de eerste.
const NUM_FATS: usize = 2;
/// De vaste rootdirectory.
const ROOT_ENTRIES: usize = 512;
/// Een directory-entry.
const ENTRY: usize = 32;
/// De sectoren van de rootdirectory.
const ROOT_SECS: usize = ROOT_ENTRIES * ENTRY / SECTOR;
/// Een cluster in bytes.
const CLUSTER: usize = SEC_PER_CLUS * SECTOR;
/// De mediabyte: een vaste schijf.
const MEDIA: u8 = 0xF8;
/// FAT16 is per definitie 4085 tot en met 65524 clusters; daarbuiten leest
/// een driver hem als FAT12 of FAT32 en vindt hij niets.
const CLUSTERS: std::ops::RangeInclusive<usize> = 4085..=65524;
/// De vaste tijd, 00:00:00. Een image dat twee bouwen twee verschillende
/// bytes geeft, is niet te verifiëren.
const FAT_TIME: u16 = 0;
/// De vaste datum, 2026-01-01.
const FAT_DATE: u16 = ((2026 - 1980) << 9) | (1 << 5) | 1;
/// De volume-id: "HOP" en een nul.
const VOLUME_ID: u32 = 0x484F_5000;
/// De leestekens die een 8.3-naam mag dragen naast A-Z en 0-9.
const SHORT_EXTRA: &str = "!#$%&'()-@^_`{}~";
/// De offsets van de dertien UTF-16-tekens in een LFN-entry.
const LFN_SLOTS: [usize; 13] = [1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30];

/// Het resultaat van deze module.
pub(crate) type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Waarom er geen image komt.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum Error {
    /// De partitie geeft een clustertal buiten FAT16.
    Clusters {
        /// De partitie in MB.
        size_mb: usize,
        /// Wat eruit kwam.
        clusters: usize,
    },
    /// `-size` en `-start` maken een image dat een MBR niet adresseert.
    TooLarge {
        /// De partitie in MB.
        size_mb: usize,
        /// De start-LBA.
        start: usize,
    },
    /// Een raw blob ligt over de MBR.
    RawOverMbr {
        /// Het bestand.
        name: String,
        /// Het byte-offset.
        off: usize,
    },
    /// Een raw blob steekt in de partitie.
    RawInPartition {
        /// Het bestand.
        name: String,
        /// Zijn lengte.
        len: usize,
        /// Het byte-offset.
        off: usize,
        /// De start-LBA van de partitie.
        start: usize,
    },
    /// De bestanden passen niet in de partitie.
    Full {
        /// Het bestand dat niet meer paste.
        name: String,
        /// De clusters die het vraagt.
        need: usize,
        /// De clusters die nog vrij zijn.
        free: usize,
    },
    /// Een pad met een lege component (`a//b`, `a/`).
    EmptyComponent {
        /// De naam op de kaart.
        name: String,
    },
    /// Geen vrij `NAAM~N`-alias meer.
    NoAlias {
        /// De naam op de kaart.
        name: String,
    },
    /// Twee namen met dezelfde 8.3-vorm in één directory.
    Collision {
        /// De naam op de kaart.
        name: String,
        /// De 8.3-vorm.
        short: String,
    },
    /// Een directory-tabel is vol.
    DirFull {
        /// De naam die niet meer paste.
        name: String,
        /// De entries die hij vraagt.
        need: usize,
        /// De entries die nog vrij zijn.
        free: usize,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Clusters { size_mb, clusters } => write!(
                f,
                "a {size_mb} MB partition gives {clusters} clusters, FAT16 needs {}..={} (pick another -size)",
                CLUSTERS.start(),
                CLUSTERS.end()
            ),
            Self::TooLarge { size_mb, start } => write!(
                f,
                "-size {size_mb} at -start {start} does not fit a 32-bit MBR"
            ),
            Self::RawOverMbr { name, off } => {
                write!(f, "raw blob {name} at {off} overlaps the MBR")
            }
            Self::RawInPartition {
                name,
                len,
                off,
                start,
            } => write!(
                f,
                "raw blob {name} ({len} bytes at {off}) reaches into the partition at LBA {start}"
            ),
            Self::Full { name, need, free } => write!(
                f,
                "{name} does not fit: {need} clusters needed, {free} free"
            ),
            Self::EmptyComponent { name } => write!(f, "{name:?}: empty path component"),
            Self::NoAlias { name } => write!(f, "{name:?}: no free 8.3 alias"),
            Self::Collision { name, short } => write!(
                f,
                "{name:?}: 8.3 name {short} collides with an earlier file"
            ),
            Self::DirFull { name, need, free } => write!(
                f,
                "{name:?} does not fit the directory: {need} entries needed, {free} free"
            ),
        }
    }
}

/// De vorm van de kaart.
#[derive(Clone, Debug)]
pub(crate) struct Card {
    /// De bootpartitie in MB.
    pub(crate) size_mb: usize,
    /// De start-LBA van de partitie.
    pub(crate) start: usize,
    /// Het volumelabel (hoogstens 11 tekens, de rest valt weg).
    pub(crate) label: String,
    /// Het label ook als root-entry (de mountnaam). Niet voor de LicheeRV:
    /// de BROM-parser is niet van ons.
    pub(crate) vol_entry: bool,
}

/// Een blob die raw op een vast byte-offset vóór de partitie landt: wat
/// een BootROM op een vaste LBA leest (de Rockchip-idbloader op LBA 64).
#[derive(Clone, Debug)]
pub(crate) struct Blob {
    /// Het bestand, voor de fouten.
    pub(crate) name: String,
    /// Het byte-offset, sector-uitgelijnd.
    pub(crate) off: usize,
    /// De inhoud.
    pub(crate) data: Vec<u8>,
}

/// Een bestand in de FAT; `/` in de naam nest directories.
#[derive(Clone, Debug)]
pub(crate) struct File {
    /// De naam op de kaart, case behouden.
    pub(crate) name: String,
    /// De inhoud.
    pub(crate) data: Vec<u8>,
}

/// Een bestand of directory in de boom.
#[derive(Debug)]
struct Node {
    /// De naam, case behouden (de LFN draagt hem als dat moet).
    name: String,
    /// De inhoud; `None` is een directory.
    data: Option<Vec<u8>>,
    /// De kinderen van een directory, in argumentvolgorde.
    children: Vec<Node>,
    /// Het startcluster, na de toewijzing.
    cluster: usize,
}

impl Node {
    /// Een lege directory.
    fn dir(name: &str) -> Self {
        Self {
            name: name.to_owned(),
            data: None,
            children: Vec::new(),
            cluster: 0,
        }
    }
}

/// De FAT16-maten van een partitie.
#[derive(Copy, Clone, Debug)]
struct Geometry {
    /// De sectoren van de partitie.
    secs: usize,
    /// Sectoren per FAT.
    spf: usize,
    /// De dataclusters.
    clusters: usize,
}

/// De maten van een partitie van `size_mb` MB. Iteratief, want de
/// FAT-grootte bepaalt zelf hoeveel dataclusters er overblijven; elke
/// cluster kost twee bytes in de tabel.
fn geometry(size_mb: usize) -> Result<Geometry> {
    let secs = size_mb
        .checked_mul((1 << 20) / SECTOR)
        .ok_or(Error::TooLarge { size_mb, start: 0 })?;
    let clusters_for =
        |spf: usize| secs.saturating_sub(RESERVED + NUM_FATS * spf + ROOT_SECS) / SEC_PER_CLUS;
    let mut spf = 1;
    loop {
        let need = (clusters_for(spf) + 2) * 2 / SECTOR + 1;
        if need <= spf {
            break;
        }
        spf = need;
    }
    let clusters = clusters_for(spf);
    if !CLUSTERS.contains(&clusters) {
        return Err(Error::Clusters { size_mb, clusters });
    }
    Ok(Geometry {
        secs,
        spf,
        clusters,
    })
}

/// Bouwt het hele image: de MBR, de raw blobs, dan de FAT16-partitie.
pub(crate) fn build(card: &Card, raws: &[Blob], files: Vec<File>) -> Result<Vec<u8>> {
    let geo = geometry(card.size_mb)?;
    let too_large = Error::TooLarge {
        size_mb: card.size_mb,
        start: card.start,
    };
    if u32::try_from(card.start).is_err() || u32::try_from(geo.secs).is_err() {
        return Err(too_large);
    }
    let total = card
        .start
        .checked_add(geo.secs)
        .and_then(|s| s.checked_mul(SECTOR))
        .ok_or(too_large)?;
    let root = tree(files)?;
    let mut img = vec![0; total];
    write_mbr(&mut img, card.start, geo.secs);
    let part_off = card.start * SECTOR;
    for r in raws {
        if r.off < SECTOR {
            return Err(Error::RawOverMbr {
                name: r.name.clone(),
                off: r.off,
            });
        }
        if r.off.saturating_add(r.data.len()) > part_off {
            return Err(Error::RawInPartition {
                name: r.name.clone(),
                len: r.data.len(),
                off: r.off,
                start: card.start,
            });
        }
        img[r.off..r.off + r.data.len()].copy_from_slice(&r.data);
    }
    write_fat16(&mut img[part_off..], card, geo, root)?;
    Ok(img)
}

/// De boom uit de bestanden: `/` scheidt (geneste) directories,
/// `extlinux/extlinux.conf` één diep, `EFI/BOOT/BOOTAA64.EFI` twee. Een
/// directory ontstaat waar hij het eerst nodig is; dat houdt de
/// clustertoewijzing deterministisch.
fn tree(files: Vec<File>) -> Result<Node> {
    let mut root = Node::dir("");
    for f in files {
        let mut parts: Vec<&str> = f.name.split('/').collect();
        if parts.len() > 1 && parts.iter().any(|p| p.is_empty()) {
            return Err(Error::EmptyComponent {
                name: f.name.clone(),
            });
        }
        let leaf = parts.pop().unwrap_or_default().to_owned();
        let mut dir = &mut root;
        for p in parts {
            let at = dir
                .children
                .iter()
                .position(|c| c.data.is_none() && c.name == p);
            let i = at.unwrap_or_else(|| {
                dir.children.push(Node::dir(p));
                dir.children.len() - 1
            });
            dir = &mut dir.children[i];
        }
        dir.children.push(Node {
            name: leaf,
            data: Some(f.data),
            children: Vec::new(),
            cluster: 0,
        });
    }
    Ok(root)
}

/// Zet één partitie-entry (type 0x0C, actief) en de boot-signature. De
/// CHS-velden zijn legacy-vulling: bij LBA-start 1 de donorbytes van de
/// LicheeRV (het bewezen image), elders de standaard "kijk naar LBA".
fn write_mbr(img: &mut [u8], start: usize, secs: usize) {
    let e = &mut img[446..462];
    e[0] = 0x80;
    e[4] = 0x0C;
    let (first, last) = if start == 1 {
        ([0x00, 0x02, 0x00], [0x0A, 0x09, 0x02])
    } else {
        ([0xFE, 0xFF, 0xFF], [0xFE, 0xFF, 0xFF])
    };
    e[1..4].copy_from_slice(&first);
    e[5..8].copy_from_slice(&last);
    put32(e, 8, start);
    put32(e, 12, secs);
    img[510] = 0x55;
    img[511] = 0xAA;
}

/// Bouwt de bootsector, de FAT's, de rootdirectory, de subdirectories en
/// de clusterketens in partitie `p`.
fn write_fat16(p: &mut [u8], card: &Card, geo: Geometry, mut root: Node) -> Result {
    let (boot, rest) = p.split_at_mut(RESERVED * SECTOR);
    let (fats, rest) = rest.split_at_mut(NUM_FATS * geo.spf * SECTOR);
    let (rootdir, data) = rest.split_at_mut(ROOT_SECS * SECTOR);
    let (fat0, fat1) = fats.split_at_mut(geo.spf * SECTOR);
    write_boot(boot, card, geo);
    // Entry 0 en 1 zijn gereserveerd: de mediabyte en eind-van-keten.
    put16(fat0, 0, 0xFF00 | usize::from(MEDIA));
    put16(fat0, 2, 0xFFFF);
    let mut next = Alloc {
        next: 2,
        end: geo.clusters + 2,
    };
    place(&mut root.children, &mut next, fat0, data)?;
    let mut used = 0;
    if card.vol_entry {
        let e = &mut rootdir[..ENTRY];
        pad(&mut e[..11], &card.label.to_ascii_uppercase());
        e[11] = 0x08;
        stamp(e);
        used = 1;
    }
    write_dir(rootdir, &mut used, &root.children)?;
    for d in root.children.iter().filter(|c| c.data.is_none()) {
        write_table(data, d, 0)?;
    }
    fat1.copy_from_slice(fat0);
    Ok(())
}

/// De bootsector met de BPB van FAT16.
fn write_boot(bs: &mut [u8], card: &Card, geo: Geometry) {
    // De jump wordt nooit uitgevoerd: firmware parseert de FAT.
    bs[..3].copy_from_slice(&[0xEB, 0x3C, 0x90]);
    bs[3..11].copy_from_slice(b"HOPOS   ");
    put16(bs, 11, SECTOR);
    bs[13] = SEC_PER_CLUS as u8;
    put16(bs, 14, RESERVED);
    bs[16] = NUM_FATS as u8;
    put16(bs, 17, ROOT_ENTRIES);
    if geo.secs < 0x1_0000 {
        put16(bs, 19, geo.secs);
    } else {
        put32(bs, 32, geo.secs);
    }
    bs[21] = MEDIA;
    put16(bs, 22, geo.spf);
    // Sectoren per spoor en koppen: legacy.
    put16(bs, 24, 32);
    put16(bs, 26, 2);
    put32(bs, 28, card.start);
    bs[36] = 0x80;
    // De extended boot signature: volume-id en label volgen.
    bs[38] = 0x29;
    bs[39..43].copy_from_slice(&VOLUME_ID.to_le_bytes());
    pad(&mut bs[43..54], &card.label);
    bs[54..62].copy_from_slice(b"FAT16   ");
    bs[510] = 0x55;
    bs[511] = 0xAA;
}

/// De clustertoewijzing: aaneengesloten, in boomvolgorde.
struct Alloc {
    /// Het eerstvolgende vrije cluster.
    next: usize,
    /// Eén voorbij het laatste cluster.
    end: usize,
}

impl Alloc {
    /// Neemt de clusters voor `len` bytes en ketent ze in de FAT; geeft het
    /// startcluster, of 0 voor een leeg bestand (de FAT-regel: een leeg
    /// bestand heeft geen keten; Go gaf hier het volgende vrije cluster).
    fn take(&mut self, fat: &mut [u8], len: usize, name: &str) -> Result<usize> {
        let n = len.div_ceil(CLUSTER);
        if n == 0 {
            return Ok(0);
        }
        if self.next + n > self.end {
            return Err(Error::Full {
                name: name.to_owned(),
                need: n,
                free: self.end - self.next,
            });
        }
        let first = self.next;
        for c in first..first + n {
            let link = if c == first + n - 1 { 0xFFFF } else { c + 1 };
            put16(fat, c * 2, link);
        }
        self.next += n;
        Ok(first)
    }
}

/// Wijst de clusters toe, depth-first in boomvolgorde: een directory
/// krijgt één cluster (64 entries, ruim voor elk bootpad) vóór zijn
/// kinderen. De inhoud van een bestand gaat meteen in `data`.
fn place(nodes: &mut [Node], a: &mut Alloc, fat: &mut [u8], data: &mut [u8]) -> Result {
    for n in nodes {
        let len = n.data.as_ref().map_or(CLUSTER, Vec::len);
        n.cluster = a.take(fat, len, &n.name)?;
        match &n.data {
            Some(d) if !d.is_empty() => {
                let off = (n.cluster - 2) * CLUSTER;
                data[off..off + d.len()].copy_from_slice(d);
            }
            Some(_) => {}
            None => place(&mut n.children, a, fat, data)?,
        }
    }
    Ok(())
}

/// De tabel van een subdirectory in zijn cluster: `.` (het eigen cluster),
/// `..` (dat van de ouder, 0 voor de root) en de kinderen, recursief.
fn write_table(data: &mut [u8], d: &Node, parent: usize) -> Result {
    let off = (d.cluster - 2) * CLUSTER;
    let tbl = &mut data[off..off + CLUSTER];
    for (i, (name, cluster)) in [(".", d.cluster), ("..", parent)].into_iter().enumerate() {
        let e = &mut tbl[i * ENTRY..(i + 1) * ENTRY];
        pad(&mut e[..11], name);
        e[11] = 0x10;
        stamp(e);
        put16(e, 26, cluster);
    }
    let mut used = 2;
    write_dir(tbl, &mut used, &d.children)?;
    for c in d.children.iter().filter(|c| c.data.is_none()) {
        write_table(data, c, d.cluster)?;
    }
    Ok(())
}

/// Schrijft de entries van één directory vanaf `*used`: per kind
/// eventueel LFN-entries en dan de 8.3-entry.
fn write_dir(tbl: &mut [u8], used: &mut usize, children: &[Node]) -> Result {
    let cap = tbl.len() / ENTRY;
    let mut seen = HashSet::new();
    for c in children {
        let (short, lfn) = short_name(&c.name, &mut seen)?;
        let n = 1 + if lfn {
            c.name.encode_utf16().count().div_ceil(13)
        } else {
            0
        };
        if *used + n > cap {
            return Err(Error::DirFull {
                name: c.name.clone(),
                need: n,
                free: cap.saturating_sub(*used),
            });
        }
        let at = &mut tbl[*used * ENTRY..(*used + n) * ENTRY];
        if lfn {
            write_lfn(at, &c.name, &short);
        }
        let e = &mut at[(n - 1) * ENTRY..];
        e[..11].copy_from_slice(&short);
        e[11] = if c.data.is_some() { 0x20 } else { 0x10 };
        stamp(e);
        put16(e, 26, c.cluster);
        if let Some(d) = &c.data {
            put32(e, 28, d.len());
        }
        *used += n;
    }
    Ok(())
}

/// De 11-byte 8.3-vorm van `name`, en of er LFN-entries vóór moeten. Past
/// de naam niet (te lang, meer punten, rare tekens), dan een uniek
/// `NAAM~N`-alias van de eerste zes bruikbare tekens, met de extensie na
/// de laatste punt (`_` als die er niet is, zoals Go).
fn short_name(name: &str, seen: &mut HashSet<String>) -> Result<([u8; 11], bool)> {
    let up = name.to_ascii_uppercase();
    let (base, ext) = up.split_once('.').unwrap_or((&up, ""));
    let fits = !base.is_empty()
        && base.len() <= 8
        && ext.len() <= 3
        && up.matches('.').count() <= 1
        && is_short(base)
        && is_short(ext);
    let (base, ext, lfn) = if fits {
        (base.to_owned(), ext.to_owned(), false)
    } else {
        let (stem, dot_ext) = up.rsplit_once('.').unwrap_or((&up, ""));
        let mut stem = sanitize(stem);
        stem.truncate(6);
        let mut ext = sanitize(dot_ext);
        ext.truncate(3);
        let mut n = 1;
        let alias = loop {
            let cand = format!("{stem}~{n}");
            if !seen.contains(&format!("{cand}.{ext}")) {
                break cand;
            }
            if n > 99 {
                return Err(Error::NoAlias {
                    name: name.to_owned(),
                });
            }
            n += 1;
        };
        (alias, ext, true)
    };
    let key = format!("{base}.{ext}");
    if !seen.insert(key.clone()) {
        return Err(Error::Collision {
            name: name.to_owned(),
            short: key,
        });
    }
    let mut short = [0; 11];
    pad(&mut short, &format!("{base:<8}{ext:<3}"));
    Ok((short, lfn))
}

/// Mag `s` in een 8.3-naam staan?
fn is_short(s: &str) -> bool {
    s.chars().all(is_short_char)
}

/// Mag `c` in een 8.3-naam staan?
fn is_short_char(c: char) -> bool {
    c.is_ascii_uppercase() || c.is_ascii_digit() || SHORT_EXTRA.contains(c)
}

/// Alleen de tekens die in een 8.3-naam mogen; `_` als er niets overblijft.
fn sanitize(s: &str) -> String {
    let out: String = s.chars().filter(|&c| is_short_char(c)).collect();
    if out.is_empty() { "_".to_owned() } else { out }
}

/// De VFAT-entries (attribuut 0x0F) vóór een 8.3-entry: dertien
/// UTF-16-tekens per entry, in omgekeerde volgorde, de laatste met bit
/// 0x40, elk met de som van het 8.3-alias. De terminator en de
/// 0xFFFF-vulling alleen als de laatste entry ruimte over heeft: een naam
/// van precies 13 tekens (extlinux.conf) krijgt er per de spec geen.
fn write_lfn(tbl: &mut [u8], name: &str, short: &[u8; 11]) {
    let sum = short.iter().fold(0u8, |s, &c| {
        ((s & 1) << 7).wrapping_add(s >> 1).wrapping_add(c)
    });
    let mut units: Vec<u16> = name.encode_utf16().collect();
    if !units.len().is_multiple_of(13) {
        units.push(0);
        units.resize(units.len().next_multiple_of(13), 0xFFFF);
    }
    let n = units.len() / 13;
    for (i, chunk) in units.chunks(13).enumerate() {
        let e = &mut tbl[(n - 1 - i) * ENTRY..(n - i) * ENTRY];
        e[0] = (i + 1) as u8 | if i == n - 1 { 0x40 } else { 0 };
        e[11] = 0x0F;
        e[13] = sum;
        for (&off, &u) in LFN_SLOTS.iter().zip(chunk) {
            e[off..off + 2].copy_from_slice(&u.to_le_bytes());
        }
    }
}

/// De vaste tijdstempel: aangemaakt en gewijzigd.
fn stamp(e: &mut [u8]) {
    for (off, v) in [
        (14, FAT_TIME),
        (16, FAT_DATE),
        (22, FAT_TIME),
        (24, FAT_DATE),
    ] {
        e[off..off + 2].copy_from_slice(&v.to_le_bytes());
    }
}

/// `s` links in `dst`, aangevuld met spaties en afgekapt op de maat.
fn pad(dst: &mut [u8], s: &str) {
    dst.fill(b' ');
    for (d, b) in dst.iter_mut().zip(s.bytes()) {
        *d = b;
    }
}

/// Een 16-bit veld; elke waarde hier is een FAT16-maat of clusternummer en
/// past (de clusters zijn begrensd op 65524).
fn put16(b: &mut [u8], off: usize, v: usize) {
    b[off..off + 2].copy_from_slice(&(v as u16).to_le_bytes());
}

/// Een 32-bit veld; `build` toetst vooraf dat de maten passen.
fn put32(b: &mut [u8], off: usize, v: usize) {
    b[off..off + 4].copy_from_slice(&(v as u32).to_le_bytes());
}

#[cfg(test)]
mod tests;
