//! mkcard: schrijft een compleet, dd-baar kaart-image op de host: een MBR,
//! één FAT16-bootpartitie met de bestanden erin, en raw blobs op vaste
//! byte-offsets vóór de partitie (waar een BootROM zijn firmware raw leest).
//! Eén bestand naar de kaart: geen donor-image van gigabytes, geen mount,
//! geen root, en twee keer bouwen geeft dezelfde bytes.
//!
//! ```text
//! mkcard -o hopos-rpi4.img -size 64 -start 8192 -label bootfs -vollabel \
//!     kernel8.img config.txt start4.elf ...
//! mkcard -o hopos-radxa-zero3.img -size 64 -start 32768 -label hopos -vollabel \
//!     -raw donor-boot.bin@32768 hopos.img hopos.ird extlinux.conf=extlinux/extlinux.conf
//! mkcard -o hopos-licheerv.img -size 64 fip-licheerv.bin=fip.bin
//! ```
//!
//! De port van de Go-mkcard (`image/mkcard/main.go` op tag v2.2.8), met
//! dezelfde opdrachtregel en byte voor byte hetzelfde image
//! (`tests/go.rs`). Twee verschillen: `-cfgwindow` is weg (het raw
//! patchbare venster van Go's `image/hopcfg`; v3 leest zijn config anders),
//! en een leeg bestand krijgt startcluster 0 zoals de FAT-spec zegt.
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

mod fat;

use std::fmt;
use std::io;
use std::path::Path;
use std::process::ExitCode;

/// De hulptekst.
const USAGE: &str = "usage: mkcard [-o img] [-size MB] [-start LBA] [-label name] [-vollabel] \
[-raw path@offset]... file[=name]...";

/// De opties, met de standaarden van Go.
#[derive(Clone, Debug)]
struct Opts {
    /// Het uitvoer-image.
    out: String,
    /// De vorm van de kaart.
    card: fat::Card,
    /// `pad@offset` per raw blob.
    raws: Vec<(String, usize)>,
    /// `bestand[=naam]` per bestand in de FAT.
    files: Vec<String>,
}

/// Waarom er geen image is.
#[derive(Debug)]
enum Error {
    /// Een bestand is niet te lezen.
    Read(String, io::Error),
    /// Het image is niet te schrijven.
    Write(String, io::Error),
    /// De kaart zelf.
    Card(fat::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Read(p, e) => write!(f, "read {p}: {e}"),
            Self::Write(p, e) => write!(f, "write {p}: {e}"),
            Self::Card(e) => e.fmt(f),
        }
    }
}

/// Een getal zoals Go's `flag.Int` het leest: decimaal, of hex met `0x`.
fn number(v: &str) -> Result<usize, String> {
    let n = match v.strip_prefix("0x").or_else(|| v.strip_prefix("0X")) {
        Some(h) => usize::from_str_radix(h, 16),
        None => v.parse(),
    };
    n.map_err(|_| format!("{v:?} is not a number"))
}

/// Leest de opdrachtregel zoals Go's `flag`: `-naam waarde`, `-naam=waarde`
/// of `--naam`, tot het eerste argument dat geen vlag is of `--`.
fn parse(args: &[String]) -> Result<Opts, String> {
    let mut o = Opts {
        out: "card.img".to_owned(),
        card: fat::Card {
            size_mb: 64,
            start: 1,
            label: "boot".to_owned(),
            vol_entry: false,
        },
        raws: Vec::new(),
        files: Vec::new(),
    };
    let mut it = args.iter().peekable();
    while let Some(a) = it.peek() {
        if *a == "--" {
            it.next();
            break;
        }
        let Some(flag) = a.strip_prefix("--").or_else(|| a.strip_prefix('-')) else {
            break;
        };
        if flag.is_empty() {
            break;
        }
        it.next();
        let (name, inline) = match flag.split_once('=') {
            Some((n, v)) => (n, Some(v)),
            None => (flag, None),
        };
        if name == "vollabel" {
            o.card.vol_entry = match inline {
                None | Some("true" | "1") => true,
                Some("false" | "0") => false,
                Some(v) => return Err(format!("-vollabel={v}: not a bool")),
            };
            continue;
        }
        let v = match inline {
            Some(v) => v,
            None => it
                .next()
                .ok_or(format!("flag needs an argument: -{name}"))?,
        };
        match name {
            "o" => o.out = v.to_owned(),
            "size" => o.card.size_mb = number(v)?,
            "start" => o.card.start = number(v)?,
            "label" => o.card.label = v.to_owned(),
            "raw" => {
                let (path, off) = v
                    .rsplit_once('@')
                    .ok_or(format!("-raw wants path@byteoffset, got {v:?}"))?;
                let off = number(off)?;
                if off % 512 != 0 {
                    return Err(format!("-raw offset {off} is not sector-aligned (512)"));
                }
                o.raws.push((path.to_owned(), off));
            }
            _ => return Err(format!("flag provided but not defined: -{name}")),
        }
    }
    o.files = it.cloned().collect();
    Ok(o)
}

/// Leest een bestand.
fn read(path: &str) -> Result<Vec<u8>, Error> {
    std::fs::read(path).map_err(|e| Error::Read(path.to_owned(), e))
}

/// Leest de bestanden, bouwt het image en schrijft het; geeft de maat.
fn run(o: &Opts) -> Result<usize, Error> {
    let raws = o
        .raws
        .iter()
        .map(|(path, off)| {
            Ok(fat::Blob {
                name: path.clone(),
                off: *off,
                data: read(path)?,
            })
        })
        .collect::<Result<Vec<_>, Error>>()?;
    let files = o
        .files
        .iter()
        .map(|arg| {
            let (path, name) = arg.split_once('=').unwrap_or((arg, ""));
            let name = match name {
                "" => Path::new(path)
                    .file_name()
                    .map_or(path.to_owned(), |n| n.to_string_lossy().into_owned()),
                n => n.to_owned(),
            };
            Ok(fat::File {
                name,
                data: read(path)?,
            })
        })
        .collect::<Result<Vec<_>, Error>>()?;
    let img = fat::build(&o.card, &raws, files).map_err(Error::Card)?;
    std::fs::write(&o.out, &img).map_err(|e| Error::Write(o.out.clone(), e))?;
    Ok(img.len())
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let o = match parse(&args) {
        Ok(o) => o,
        Err(e) => {
            eprintln!("mkcard: {e}\n{USAGE}");
            return ExitCode::from(2);
        }
    };
    if o.files.is_empty() {
        eprintln!("{USAGE}");
        return ExitCode::FAILURE;
    }
    match run(&o) {
        Ok(len) => {
            println!(
                "mkcard: {} ({} MB total, partition {} MB at LBA {}), {} file(s)",
                o.out,
                len >> 20,
                o.card.size_mb,
                o.card.start,
                o.files.len()
            );
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("mkcard: {e}");
            ExitCode::FAILURE
        }
    }
}
