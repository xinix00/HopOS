//! Een allocatievrije lezer van de statische ACPI-tabellen die UEFI-firmware
//! achterlaat: de hardwarebeschrijving van servers (Ampere Altra), de Orion
//! O6N en QEMU virt onder EDK2.
//!
//! Waar de Pi's een DTB meegeven ([`crate::fdt`]), is dit het
//! ACPI-equivalent: RSDP, XSDT, en daarachter de MADT (cores en GIC), MCFG
//! (PCIe-ECAM), SPCR (console-UART), FADT (PSCI-conduit en de DSDT) en GTDT
//! (de SBSA-watchdog). Alleen lezen, geen AML-interpreter: de statische
//! tabellen dekken alles wat HopOS voor discovery nodig heeft.
//!
//! Deze module bezit geen geheugen en kent geen adressen als pointer. De
//! tabellen liggen in door de firmware gereserveerd geheugen
//! (EfiACPIReclaimMemory); het board leest ze via [`Phys`] (zijn eigen
//! plausibiliteitstoets plus `dev::copy_out`) en de aanroeper geeft de
//! buffer waar een tabel in komt. Alles is little-endian (ACPI-spec).
//!
//! Alles hier is onvertrouwde firmware-input: pointers worden gewogen
//! vóór ze gelezen worden (`plausible`), lengtes worden begrensd, en
//! indexeren gaat met `get`. Een kromme tabel is een [`Error`] of `None`,
//! nooit een panic en nooit een oneindige lus.
//!
//! De Go-voorganger (`metal/fw/acpi`) hield een map met gedecodeerde
//! tabellen per signature, omdat device-reads duur zijn en elke MCFG/MADT
//! anders twee keer per boot uit device-geheugen kwam. Hier houdt de
//! aanroeper de buffer vast: wie een tabel twee keer wil, laadt hem één keer.

use bounded::BoundedVec;
use core::fmt;

/// Leest fysiek geheugen voor de parser; het board levert hem
/// (`dev::copy_out` na zijn eigen plausibiliteitstoets). `false` = niet
/// leesbaar.
///
/// Waarom de lezer van het board is: de tabellen liggen buiten de
/// RAM-declaratie en zijn dus device-gemapt (nGnRnE), en daar moet élke
/// toegang uitgelijnd zijn. Go's memmove op een directe slice gaf een
/// alignment fault (gemeten 2026-07-13, EL1 exception in
/// `slicebytetostring`); de Go-kern las daarom per 32-bit-woord naar een
/// RAM-kopie. Hoe het board dat doet, is zijn zaak; deze module vraagt
/// alleen 4-gealigneerde adressen.
pub trait Phys {
    /// Kopieert `out.len()` bytes vanaf fysiek adres `pa` naar `out`;
    /// `false` als dat bereik niet leesbaar is.
    fn read(&self, pa: u64, out: &mut [u8]) -> bool;
}

/// Een tabelsignature van vier ASCII-tekens (`b"APIC"`).
pub type Sig = [u8; 4];

/// Zoveel XSDT-entries houden we bij. QEMU virt heeft er een stuk of tien,
/// de Altra een paar dozijn (vooral SSDT's).
pub const MAX_TABLES: usize = 64;

/// De grootste tabel die we laden (Go: 4 MB).
///
/// De lengte is firmware-input: te klein is geen geldige SDT, te groot is
/// een corrupte waarde die in de Go-kern `make()` in een boot-OOM liet lopen
/// (review #11). Hier vult hij geen heap maar begrenst hij het leeswerk.
pub const TABLE_MAX: usize = 1 << 22;

/// De vaste SDT-header: signature, lengte, revisie, checksum, OEM-velden.
pub const HEADER_LEN: usize = 36;

/// De EFI-configuratietabel-GUID van ACPI 2.0+ (`EFI_ACPI_20_TABLE_GUID`,
/// 8868e871-e4f1-11d3-bc22-0080c73c8881) in EFI_GUID-geheugenvolgorde: de
/// eerste drie velden little-endian, de laatste acht bytes zoals ze staan.
/// Hiermee vindt het board de RSDP.
pub const ACPI_20_GUID: [u8; 16] = [
    0x71, 0xe8, 0x68, 0x88, 0xf1, 0xe4, 0xd3, 0x11, 0xbc, 0x22, 0x00, 0x80, 0xc7, 0x3c, 0x88, 0x81,
];

/// De laagste plausibele tabelpointer: de nulpagina is nooit een ACPI-tabel.
const MIN_PA: u64 = 0x1000;
/// De PA-ruimte van deze ARM64-boards (en van onze mappings).
const MAX_PA: u64 = 1 << 48;

/// Waarom een tabel niet te lezen is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// Het RSDP-adres is 0: de firmware gaf geen ACPI.
    NoRsdp,
    /// Een pointer uit de firmware valt niet door de plausibiliteitstoets
    /// (nulpagina, niet 4-gealigneerd, of buiten de 48-bit PA-ruimte).
    BadAddress {
        /// Het adres.
        pa: u64,
        /// De lengte die er gelezen zou worden.
        len: u64,
    },
    /// [`Phys::read`] weigerde het bereik.
    Unreadable {
        /// Het adres.
        pa: u64,
        /// De lengte.
        len: usize,
    },
    /// Geen `"RSD PTR "` op het RSDP-adres.
    RsdpSignature(u64),
    /// Een RSDP-checksum klopt niet.
    RsdpChecksum {
        /// Het RSDP-adres.
        pa: u64,
        /// Welk deel: `"ACPI 1.0"` (20 bytes) of `"ACPI 2.0"` (36 bytes).
        part: &'static str,
    },
    /// RSDP-revisie onder 2: ACPI 1.0-firmware zonder XSDT.
    Revision(u8),
    /// Op een adres staat een andere tabel dan verwacht.
    Signature {
        /// Het adres.
        pa: u64,
        /// De verwachte signature.
        want: Sig,
        /// Wat er stond.
        got: Sig,
    },
    /// Een tabel-slice draagt een andere signature dan de parser leest.
    WrongTable {
        /// De verwachte signature.
        want: Sig,
        /// Wat er stond.
        got: Sig,
    },
    /// De gedeclareerde lengte valt buiten 36..=`max`: kapot, of groter dan
    /// de buffer, [`TABLE_MAX`] of de gegeven slice.
    Length {
        /// De tabel.
        sig: Sig,
        /// De gedeclareerde lengte.
        len: usize,
        /// De grens die gold.
        max: usize,
    },
    /// De tabelchecksum komt niet op 0 uit.
    Checksum {
        /// De tabel.
        sig: Sig,
        /// Zijn adres.
        pa: u64,
    },
    /// De XSDT (of de FADT, voor de DSDT) wijst deze tabel niet aan.
    Missing(Sig),
    /// De tabel is te kort voor het veld dat we lezen.
    TooShort {
        /// De tabel.
        sig: Sig,
        /// Zijn lengte.
        len: usize,
        /// Wat er nodig is.
        need: usize,
    },
}

/// Een signature als tekst voor de logregel; niet-printbare bytes worden `?`.
struct Ascii<'a>(&'a [u8]);

impl fmt::Display for Ascii<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        for &b in self.0 {
            let c = if b.is_ascii_graphic() || b == b' ' {
                char::from(b)
            } else {
                '?'
            };
            write!(f, "{c}")?;
        }
        Ok(())
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::NoRsdp => f.write_str("acpi: no RSDP"),
            Self::BadAddress { pa, len } => {
                write!(f, "acpi: implausible address {pa:#x} (len {len})")
            }
            Self::Unreadable { pa, len } => {
                write!(f, "acpi: {len} bytes at {pa:#x} not readable")
            }
            Self::RsdpSignature(pa) => write!(f, "acpi: RSDP signature missing at {pa:#x}"),
            Self::RsdpChecksum { pa, part } => {
                write!(f, "acpi: RSDP checksum failed at {pa:#x} ({part} part)")
            }
            Self::Revision(r) => {
                write!(f, "acpi: RSDP revision {r}, no XSDT (ACPI 1.0 firmware)")
            }
            Self::Signature { pa, want, got } => write!(
                f,
                "acpi: {} signature missing at {pa:#x} (found {})",
                Ascii(&want),
                Ascii(&got)
            ),
            Self::WrongTable { want, got } => {
                write!(f, "acpi: want {}, got {}", Ascii(&want), Ascii(&got))
            }
            Self::Length { sig, len, max } => {
                write!(f, "acpi: {} length {len} outside 36..={max}", Ascii(&sig))
            }
            Self::Checksum { sig, pa } => {
                write!(f, "acpi: {} checksum failed at {pa:#x}", Ascii(&sig))
            }
            Self::Missing(sig) => write!(f, "acpi: no {}", Ascii(&sig)),
            Self::TooShort { sig, len, need } => {
                write!(f, "acpi: {} too short ({len} < {need})", Ascii(&sig))
            }
        }
    }
}

/// De `Result` van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// De XSDT-inhoud: welke tabel waar staat, in XSDT-volgorde.
///
/// Meerdere tabellen met dezelfde signature bestaan (SSDT's); [`find`]
/// geeft de eerste, [`all`] allemaal.
///
/// [`find`]: Tables::find
/// [`all`]: Tables::all
#[derive(Clone, Debug)]
pub struct Tables {
    revision: u8,
    oem_id: [u8; 6],
    entries: BoundedVec<(Sig, u64), MAX_TABLES>,
    broken: usize,
    overflow: usize,
}

impl Tables {
    /// RSDP (rev >= 2, beide checksums), dan de XSDT (signature, lengte
    /// 36..=[`TABLE_MAX`], checksum), dan de entries: per entry een
    /// plausibel adres en vier bytes signature. Een kapotte entry wordt
    /// overgeslagen (zoals Go) en geteld in [`broken`](Tables::broken).
    ///
    /// Checksums worden hier streng getoetst: een corrupte pointer in de
    /// RSDP of XSDT betekent dat al het vervolg giswerk is, dan liever
    /// meteen een fout waar de main op kan terugvallen.
    pub fn parse(mem: &impl Phys, rsdp: u64) -> Result<Tables> {
        let (revision, xsdt) = read_rsdp(mem, rsdp)?;
        if !plausible(xsdt, HEADER_LEN as u64) {
            return Err(Error::BadAddress {
                pa: xsdt,
                len: HEADER_LEN as u64,
            });
        }
        let mut h = [0u8; HEADER_LEN];
        read(mem, xsdt, &mut h)?;
        let len = checked_header(&h, xsdt, b"XSDT", TABLE_MAX)?;
        if sum(mem, xsdt, len)? != 0 {
            return Err(Error::Checksum {
                sig: *b"XSDT",
                pa: xsdt,
            });
        }
        let mut t = Tables {
            revision,
            oem_id: [0; 6],
            entries: BoundedVec::new(),
            broken: 0,
            overflow: 0,
        };
        if let Some(id) = h.get(10..16) {
            t.oem_id.copy_from_slice(id);
        }
        t.walk_entries(mem, xsdt, len)?;
        Ok(t)
    }

    /// Loopt de 8-byte-pointers achter de XSDT-header af.
    ///
    /// De Go-kern volgde deze pointers eerst blind: één corrupte entry
    /// (garbage met hoge bits, of een handvol bytes) en de eerste
    /// signature-read was een data abort op core 0, een dode node bij boot
    /// in plaats van een nette fout. Daarom weegt `plausible` elke entry.
    fn walk_entries(&mut self, mem: &impl Phys, xsdt: u64, len: usize) -> Result {
        let mut off = HEADER_LEN;
        while off + 8 <= len {
            let mut raw = [0u8; 8];
            read(mem, xsdt + off as u64, &mut raw)?;
            off += 8;
            let pa = u64::from_le_bytes(raw);
            let mut sig = [0u8; 4];
            if !plausible(pa, HEADER_LEN as u64) || !mem.read(pa, &mut sig) {
                self.broken += 1;
                continue;
            }
            if self.entries.push((sig, pa)).is_err() {
                self.overflow += 1;
            }
        }
        Ok(())
    }

    /// De RSDP-revisie (>= 2: ACPI 2.0+, met XSDT).
    #[must_use]
    pub fn revision(&self) -> u8 {
        self.revision
    }

    /// De OEM-id uit de XSDT-header (`"QEMU"`, `"Ampere"`, ...), zonder
    /// NUL's en spaties aan het eind; `""` als het geen UTF-8 is.
    #[must_use]
    pub fn oem_id(&self) -> &str {
        let mut end = self.oem_id.len();
        while end > 0 && matches!(self.oem_id.get(end - 1), Some(0 | b' ')) {
            end -= 1;
        }
        self.oem_id
            .get(..end)
            .and_then(|b| core::str::from_utf8(b).ok())
            .unwrap_or("")
    }

    /// Het adres van de eerste tabel met signature `sig`.
    #[must_use]
    pub fn find(&self, sig: &Sig) -> Option<u64> {
        self.all(sig).next()
    }

    /// De adressen van alle tabellen met signature `sig` (de SSDT's).
    pub fn all<'a>(&'a self, sig: &'a Sig) -> impl Iterator<Item = u64> + 'a {
        self.entries
            .iter()
            .filter(move |(s, _)| s == sig)
            .map(|&(_, pa)| pa)
    }

    /// Alle signatures in XSDT-volgorde, voor de bootlog.
    pub fn sigs(&self) -> impl Iterator<Item = Sig> + '_ {
        self.entries.iter().map(|&(s, _)| s)
    }

    /// Zoveel XSDT-entries waren kapot (onplausibel adres of onleesbare
    /// signature) en zijn overgeslagen. Het board logt dit getal.
    #[must_use]
    pub fn broken(&self) -> usize {
        self.broken
    }

    /// Zoveel geldige entries pasten niet meer in [`MAX_TABLES`] en zijn
    /// weggelaten. Het board logt dit getal; 0 op elk bekend board.
    #[must_use]
    pub fn overflow(&self) -> usize {
        self.overflow
    }

    /// Laadt de tabel `sig` in `buf` (lengte 36..=min(`buf.len()`,
    /// [`TABLE_MAX`]), checksum) en geeft zijn bytes, precies de
    /// gedeclareerde lengte.
    pub fn load<'b>(&self, mem: &impl Phys, sig: &Sig, buf: &'b mut [u8]) -> Result<&'b [u8]> {
        let pa = self.find(sig).ok_or(Error::Missing(*sig))?;
        load_sig(mem, pa, Some(sig), buf)
    }

    /// Laadt de DSDT via de FADT: X_DSDT op offset 140 als de FADT minstens
    /// 148 bytes is en hij niet 0 is, anders de 32-bit-DSDT op 40. ACPI
    /// linkt de DSDT via de FADT, niet via de XSDT-entries.
    ///
    /// `buf` draagt eerst de FADT en daarna de DSDT.
    pub fn load_dsdt<'b>(&self, mem: &impl Phys, buf: &'b mut [u8]) -> Result<&'b [u8]> {
        let pa = dsdt_pa(self.load(mem, b"FACP", buf)?)?;
        load_sig(mem, pa, Some(b"DSDT"), buf)
    }
}

/// Laadt en toetst de SDT op `pa`: header, lengte 36..=min(`buf.len()`,
/// [`TABLE_MAX`]) en checksum. Geeft precies de gedeclareerde lengte.
pub fn load_at<'b>(mem: &impl Phys, pa: u64, buf: &'b mut [u8]) -> Result<&'b [u8]> {
    load_sig(mem, pa, None, buf)
}

/// [`load_at`] met een optionele signature-eis.
fn load_sig<'b>(
    mem: &impl Phys,
    pa: u64,
    want: Option<&Sig>,
    buf: &'b mut [u8],
) -> Result<&'b [u8]> {
    if !plausible(pa, HEADER_LEN as u64) {
        return Err(Error::BadAddress {
            pa,
            len: HEADER_LEN as u64,
        });
    }
    let mut h = [0u8; HEADER_LEN];
    read(mem, pa, &mut h)?;
    let sig = sig_of(&h).unwrap_or_default();
    let len = checked_header(&h, pa, want.unwrap_or(&sig), buf.len().min(TABLE_MAX))?;
    let out = buf.get_mut(..len).ok_or(Error::Length {
        sig,
        len,
        max: TABLE_MAX,
    })?;
    read(mem, pa, out)?;
    if checksum(out) != 0 {
        return Err(Error::Checksum { sig, pa });
    }
    Ok(out)
}

/// Leest de RSDP op `rsdp` en geeft (revisie, XSDT-adres).
fn read_rsdp(mem: &impl Phys, rsdp: u64) -> Result<(u8, u64)> {
    if rsdp == 0 {
        return Err(Error::NoRsdp);
    }
    // Het RSDP-adres komt uit de EFI-configuratietabel en is dus dezelfde
    // soort firmware-input als de XSDT-pointers. In Go ging de eerste read
    // er blind in: één verdwaalde pointer en dat was een data abort op
    // core 0 in plaats van een fout. Zelfde weging, één stap eerder.
    if !plausible(rsdp, 36) {
        return Err(Error::BadAddress { pa: rsdp, len: 36 });
    }
    let mut b = [0u8; 36];
    read(mem, rsdp, &mut b)?;
    if b.get(..8) != Some(b"RSD PTR ".as_slice()) {
        return Err(Error::RsdpSignature(rsdp));
    }
    if b.get(..20).map(checksum) != Some(0) {
        return Err(Error::RsdpChecksum {
            pa: rsdp,
            part: "ACPI 1.0",
        });
    }
    let rev = b.get(15).copied().unwrap_or(0);
    if rev < 2 {
        return Err(Error::Revision(rev));
    }
    if checksum(&b) != 0 {
        return Err(Error::RsdpChecksum {
            pa: rsdp,
            part: "ACPI 2.0",
        });
    }
    Ok((rev, le64(&b, 24).unwrap_or(0)))
}

/// Toetst een SDT-header op `pa`: signature `want`, lengte 36..=`max` en
/// een plausibel bereik. Geeft de lengte.
fn checked_header(h: &[u8], pa: u64, want: &Sig, max: usize) -> Result<usize> {
    let got = sig_of(h).unwrap_or_default();
    if got != *want {
        return Err(Error::Signature {
            pa,
            want: *want,
            got,
        });
    }
    let len = le32(h, 4).map_or(0, |v| v as usize);
    if !(HEADER_LEN..=max).contains(&len) {
        return Err(Error::Length { sig: got, len, max });
    }
    if !plausible(pa, len as u64) {
        return Err(Error::BadAddress {
            pa,
            len: len as u64,
        });
    }
    Ok(len)
}

/// De checksum over `len` bytes op `pa`, in brokken van 256 bytes: de XSDT
/// kan tot [`TABLE_MAX`] groot zijn en we alloceren niet.
fn sum(mem: &impl Phys, pa: u64, len: usize) -> Result<u8> {
    let mut chunk = [0u8; 256];
    let mut s = 0u8;
    let mut off = 0;
    while off < len {
        let n = (len - off).min(chunk.len());
        let c = chunk.get_mut(..n).unwrap_or_default();
        read(mem, pa + off as u64, c)?;
        s = c.iter().fold(s, |a, &b| a.wrapping_add(b));
        off += n;
    }
    Ok(s)
}

/// Het DSDT-adres uit een geladen FADT.
fn dsdt_pa(f: &[u8]) -> Result<u64> {
    let (sig, len) = (*b"FACP", f.len());
    let dsdt32 = le32(f, 40).ok_or(Error::TooShort { sig, len, need: 44 })?;
    let x = if len >= 148 { le64(f, 140) } else { None };
    match x {
        Some(pa) if pa != 0 => Ok(pa),
        _ if dsdt32 != 0 => Ok(u64::from(dsdt32)),
        _ => Err(Error::Missing(*b"DSDT")),
    }
}

/// Weegt een fysiek tabeladres uit de firmware vóór we het lezen.
///
/// Dit is een structurele toets, géén memory-map-validatie: deze module kent
/// de EFI-memmap niet, dus een pointer naar plausibel-maar-ongemapt RAM kan
/// nog faulten; daarvoor is de toets van het board in [`Phys`]. Hij vangt
/// wél de realistische corruptie: de nulpagina, niet-uitgelijnd (headers
/// liggen in de praktijk 4-byte-uitgelijnd), buiten de 48-bit PA-ruimte, of
/// een lengte die overloopt.
fn plausible(pa: u64, len: u64) -> bool {
    (MIN_PA..MAX_PA).contains(&pa)
        && pa.is_multiple_of(4)
        && len > 0
        && pa.checked_add(len).is_some_and(|end| end <= MAX_PA)
}

/// Leest via [`Phys`], met de fout erbij.
fn read(mem: &impl Phys, pa: u64, out: &mut [u8]) -> Result {
    if mem.read(pa, out) {
        Ok(())
    } else {
        Err(Error::Unreadable { pa, len: out.len() })
    }
}

/// ACPI-checksums tellen alle bytes op en moeten op 0 uitkomen.
fn checksum(b: &[u8]) -> u8 {
    b.iter().fold(0u8, |s, &c| s.wrapping_add(c))
}

/// De signature van een tabel-slice.
fn sig_of(t: &[u8]) -> Option<Sig> {
    t.get(..4)?.try_into().ok()
}

/// Toetst een geladen tabel-slice: signature `sig` en een gedeclareerde
/// lengte tussen `min` en de slice. Geeft de tabel op zijn gedeclareerde
/// lengte, zodat een parser nooit voorbij het einde leest.
fn checked_table<'a>(t: &'a [u8], sig: &Sig, min: usize) -> Result<&'a [u8]> {
    let got = sig_of(t).ok_or(Error::TooShort {
        sig: *sig,
        len: t.len(),
        need: min,
    })?;
    if got != *sig {
        return Err(Error::WrongTable { want: *sig, got });
    }
    let len = le32(t, 4).map_or(0, |v| v as usize);
    if len < min {
        return Err(Error::TooShort {
            sig: *sig,
            len,
            need: min,
        });
    }
    t.get(..len).ok_or(Error::Length {
        sig: *sig,
        len,
        max: t.len(),
    })
}

fn le16(b: &[u8], off: usize) -> Option<u16> {
    Some(u16::from_le_bytes(
        b.get(off..off.checked_add(2)?)?.try_into().ok()?,
    ))
}

fn le32(b: &[u8], off: usize) -> Option<u32> {
    Some(u32::from_le_bytes(
        b.get(off..off.checked_add(4)?)?.try_into().ok()?,
    ))
}

fn le64(b: &[u8], off: usize) -> Option<u64> {
    Some(u64::from_le_bytes(
        b.get(off..off.checked_add(8)?)?.try_into().ok()?,
    ))
}

/// Eén GICC uit de MADT: een core zoals de firmware hem kent.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Gicc {
    /// De ACPI processor UID (offset 8).
    pub uid: u32,
    /// De affiniteitsroute voor PSCI CPU_ON (offset 68).
    pub mpidr: u64,
    /// GICC-flags bit 0.
    pub enabled: bool,
    /// Het redistributor-frame van déze core (offset 60); 0 als de firmware
    /// het niet invult en de MADT alleen een GICR-range draagt. Dat is wat
    /// een GICv3-driver per core nodig heeft.
    pub gicr: u64,
    /// De "Processor Power Efficiency Class" (offset 76, ACPI 6.0+): lager
    /// is zuiniger. Op een tri-cluster-SoC (O6N: A520/A720/A720-boost) is
    /// dít de universele bron voor de clusterklasse van een core, geen
    /// MPIDR-tabel per board. Alle cores gelijk (of alles 0) = de firmware
    /// zegt niets; dan valt het board terug op eigen kennis.
    pub eff_class: u8,
}

/// De MADT ("APIC"): de cores (GICC) en de GIC-blokken. De bron voor
/// "hoeveel cores heeft dit ijzer": op de Altra 128, op QEMU wat `-smp` zegt.
#[derive(Copy, Clone, Debug)]
pub struct Madt<'a> {
    table: &'a [u8],
}

impl<'a> Madt<'a> {
    /// Toetst de signature `"APIC"` en een lengte van minstens 44 (header
    /// plus LocalInterruptControllerAddress en flags).
    pub fn new(table: &'a [u8]) -> Result<Madt<'a>> {
        Ok(Self {
            table: checked_table(table, b"APIC", 44)?,
        })
    }

    /// De interrupt-controller-structuren vanaf offset 44.
    fn entries(&self) -> Entries<'a> {
        Entries {
            b: self.table,
            off: 44,
        }
    }

    /// De cores: type 0x0b, minstens 76 bytes (ACPI 6.x: 80; MPIDR op 68).
    /// `eff_class` alleen als de entry minstens 77 bytes is. Stopt bij een
    /// kapotte entry.
    pub fn cpus(&self) -> impl Iterator<Item = Gicc> + 'a + use<'a> {
        self.entries().filter_map(|(typ, e)| {
            if typ != 0x0b || e.len() < 76 {
                return None;
            }
            Some(Gicc {
                uid: le32(e, 8)?,
                mpidr: le64(e, 68)?,
                enabled: le32(e, 12)? & 1 != 0,
                gicr: le64(e, 60)?,
                eff_class: e.get(76).copied().unwrap_or(0),
            })
        })
    }

    /// De distributor (type 0x0c: PhysicalBaseAddress op 8, GIC-versie op
    /// 20, 0 als de entry korter is); de eerste als er meer zijn.
    #[must_use]
    pub fn gicd(&self) -> Option<(u64, u8)> {
        self.entries().find_map(|(typ, e)| {
            if typ != 0x0c || e.len() < 16 {
                return None;
            }
            Some((le64(e, 8)?, e.get(20).copied().unwrap_or(0)))
        })
    }

    /// De GICR-discovery-bereiken (type 0x0e: base op 4, lengte op 12):
    /// alle redistributor-frames achter elkaar, stride per frame 128 KB, of
    /// 256 KB als de GIC VLPI-frames heeft (GIC-700 op de O6N).
    pub fn gicr_ranges(&self) -> impl Iterator<Item = (u64, u32)> + 'a + use<'a> {
        self.entries().filter_map(|(typ, e)| {
            if typ != 0x0e || e.len() < 16 {
                return None;
            }
            Some((le64(e, 4)?, le32(e, 12)?))
        })
    }

    /// De ITS-bases (type 0x0f: PhysicalBase op 8).
    pub fn its(&self) -> impl Iterator<Item = u64> + 'a + use<'a> {
        self.entries().filter_map(|(typ, e)| {
            if typ != 0x0f || e.len() < 16 {
                return None;
            }
            le64(e, 8)
        })
    }
}

/// Loopt MADT-structuren (type, lengte) af. Een entry met lengte < 2 of
/// voorbij het einde beëindigt de lus: verder lezen is gissen, en lengte 0
/// zou anders eeuwig op dezelfde plek blijven.
#[derive(Clone, Debug)]
struct Entries<'a> {
    b: &'a [u8],
    off: usize,
}

impl<'a> Iterator for Entries<'a> {
    type Item = (u8, &'a [u8]);

    fn next(&mut self) -> Option<Self::Item> {
        let typ = *self.b.get(self.off)?;
        let len = usize::from(*self.b.get(self.off + 1)?);
        let e = if len >= 2 {
            self.b.get(self.off..self.off + len)
        } else {
            None
        };
        match e {
            Some(e) => {
                self.off += len;
                Some((typ, e))
            }
            None => {
                self.off = self.b.len();
                None
            }
        }
    }
}

/// Eén MCFG-entry: een PCIe-configuratievenster.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Ecam {
    /// Het ECAM-basisadres.
    pub base: u64,
    /// De PCI-segmentgroep.
    pub segment: u16,
    /// De eerste bus.
    pub start_bus: u8,
    /// De laatste bus.
    pub end_bus: u8,
}

/// De PCIe-ECAM-vensters uit de MCFG (entries vanaf 44, stride 16). De
/// Altra heeft er meerdere (segments), QEMU virt één.
pub fn mcfg(table: &[u8]) -> Result<impl Iterator<Item = Ecam> + '_> {
    let t = checked_table(table, b"MCFG", 44)?;
    Ok(t.get(44..)
        .unwrap_or_default()
        .chunks_exact(16)
        .filter_map(|e| {
            Some(Ecam {
                base: le64(e, 0)?,
                segment: le16(e, 8)?,
                start_bus: *e.get(10)?,
                end_bus: *e.get(11)?,
            })
        }))
}

/// De SPCR-console zoals de firmware hem beschrijft.
///
/// Eén tabel, twee registerlayouts: de Orion O6N (Cix P1) heeft een
/// DesignWare 8250 op 32-bit-stride, de Altra en QEMU een PL011. De main
/// kiest de poke-laag op `if_type`.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Console {
    /// Het MMIO- (of I/O-)adres uit de GAS.
    pub base: u64,
    /// Het interfacetype (offset 36): 0x00/0x01/0x02/0x12 = 16550-familie,
    /// 0x03 = PL011, 0x0d/0x0e = SBSA (PL011-subset).
    pub if_type: u8,
    /// De GAS-adresruimte (0 = system memory).
    pub space: u8,
    /// De registerstap als macht van twee (0 = byte-stride, 2 =
    /// 32-bit-stride), afgeleid van de GAS access size (1 = byte, 2 = word,
    /// 3 = dword, 4 = qword; 0 = niet gezegd, dus byte).
    pub shift: u8,
    /// De baudrate, of 0: "zoals de firmware hem liet".
    pub baud: u32,
}

impl Console {
    /// Is dit een 16550-compatibele UART? (16550, 16450, MAX311xE, of
    /// 16550 met GAS-parameters.)
    #[must_use]
    pub fn is_16550(&self) -> bool {
        matches!(self.if_type, 0x00 | 0x01 | 0x02 | 0x12)
    }

    /// Is dit de PL011-familie? (PL011, of SBSA generic UART: een
    /// PL011-subset met dezelfde DR/FR-offsets.) De Altra en QEMU melden
    /// een van deze.
    #[must_use]
    pub fn is_pl011(&self) -> bool {
        matches!(self.if_type, 0x03 | 0x0d | 0x0e)
    }
}

/// Leest de SPCR (minstens 52 bytes): interfacetype op 36, Generic Address
/// Structure op 40 (space, bitbreedte, bitoffset, access size, adres op 44)
/// en de baudrate-code op 58.
pub fn spcr(table: &[u8]) -> Result<Console> {
    let t = checked_table(table, b"SPCR", 52)?;
    let short = Error::TooShort {
        sig: *b"SPCR",
        len: t.len(),
        need: 52,
    };
    let access = *t.get(43).ok_or(short)?;
    let baud = match t.get(58) {
        Some(3) => 9600,
        Some(4) => 19200,
        Some(6) => 57600,
        Some(7) => 115_200,
        _ => 0,
    };
    Ok(Console {
        base: le64(t, 44).ok_or(short)?,
        if_type: *t.get(36).ok_or(short)?,
        space: *t.get(40).ok_or(short)?,
        shift: if (2..=4).contains(&access) {
            access - 1
        } else {
            0
        },
        baud,
    })
}

/// De ARM-bootvlaggen uit de FADT.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Psci {
    /// Er is PSCI (ARM_BOOT_ARCH bit 0).
    pub compliant: bool,
    /// De conduit is HVC, anders SMC (bit 1).
    pub hvc: bool,
}

/// Leest ARM_BOOT_ARCH (u16 op 129) uit de FADT ("FACP", minstens 131
/// bytes). Een kortere FADT is van vóór ACPI 5.1 en zegt niets over PSCI.
pub fn fadt_psci(table: &[u8]) -> Result<Psci> {
    let t = checked_table(table, b"FACP", 131)?;
    let flags = le16(t, 129).ok_or(Error::TooShort {
        sig: *b"FACP",
        len: t.len(),
        need: 131,
    })?;
    Ok(Psci {
        compliant: flags & 1 != 0,
        hvc: flags & 2 != 0,
    })
}

/// De SBSA Generic Watchdog uit de GTDT.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Watchdog {
    /// Het refresh-frame (WRR).
    pub refresh: u64,
    /// Het control-frame (WCS/WOR).
    pub control: u64,
    /// De interrupt (GSIV).
    pub gsiv: u32,
}

/// De eerste niet-secure SBSA-watchdog uit de GTDT (platform-timer-
/// structuur type 1), of `None`: QEMU virt heeft er geen, de Altra wel
/// (servers zijn hier braaf SBSA).
#[must_use]
pub fn gtdt_watchdog(table: &[u8]) -> Option<Watchdog> {
    let t = checked_table(table, b"GTDT", 96).ok()?;
    let count = le32(t, 88)?;
    let mut off = le32(t, 92)? as usize;
    for _ in 0..count {
        // De lengte beslaat bytes off+1..off+3, dus die moeten er zijn
        // (review #8: de Go-guard dekte eerst alleen off+2).
        let typ = *t.get(off)?;
        let len = usize::from(le16(t, off + 1)?);
        let e = if len >= 4 {
            t.get(off..off + len)?
        } else {
            return None;
        };
        // Flags bit 2 = secure timer: die frames zijn vanuit NS-EL1
        // RAZ/WI of aborten. Overslaan en doorzoeken (review #13; Linux'
        // sbsa_gwdt doet hetzelfde).
        if typ == 1 && len >= 28 && le32(e, 24)? & 4 == 0 {
            return Some(Watchdog {
                refresh: le64(e, 4)?,
                control: le64(e, 12)?,
                gsiv: le32(e, 20)?,
            });
        }
        off += len;
    }
    None
}

#[cfg(test)]
mod tests;
