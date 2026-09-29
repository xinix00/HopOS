//! De fouten van de kern: één kleine enum, met de getallen erin.

use core::fmt;

/// `Result` met de kernfout als standaard (handboek §6).
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Waarom een kernoperatie niet lukte.
///
/// Elke variant draagt de getallen die een operator nodig heeft (slot, adres,
/// maat, core); de `Display` zet ze in één Engelse regel.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[expect(
    missing_docs,
    reason = "de velden zijn de getallen die de variant-doc noemt"
)]
pub enum Error {
    /// Het slotnummer ligt buiten `1..=max`.
    SlotRange { slot: usize, max: usize },
    /// Het corenummer ligt buiten het app-bereik.
    CoreRange { core: usize, max: usize },
    /// Een partitiemaat die nul is, omloopt of het app-venster overschrijdt.
    PartitionSize { size: u64 },
    /// De partitie past nu niet in de pool: een CAPACITEITSTOESTAND, geen
    /// defect. Hij verdwijnt zodra een buur vrijkomt.
    NoPartition { size: u64 },
    /// Het slot bezit nog een partitie; eerst een bevestigde stop.
    StillOwned { slot: usize },
    /// Het slot bezit geen partitie.
    NotOwned { slot: usize },
    /// Het slot staat in quarantaine: uitvoering niet bevestigd beëindigd.
    Quarantined { slot: usize },
    /// Een bereik dat vrij had moeten zijn, is (deels) van een ander.
    NotFree { base: u64, size: u64 },
    /// Er staat al een geleend kernvenster uit.
    WindowBusy { base: u64, size: u64 },
    /// De tabel of lijst is vol (een vaste maat uit de bron).
    Full { cap: usize },
    /// De heap kon de allocatie niet plaatsen.
    OutOfMemory { bytes: usize },
    /// Een sharegroup-spec die niet bij de bestaande groep past: geen
    /// capaciteit, dus niet "pending" behandelen.
    PoolSize { have: usize, want: usize },
    /// Geen vrije run van `cores` app-cores (van de gevraagde klasse).
    NoCores { cores: usize },
    /// Een core van een bestaande groep heeft een andere klasse dan gevraagd.
    ClassMismatch { core: usize },
    /// Het startschot faalde: de uitkomst is ONBEKEND, de core kan alsnog
    /// aangaan. De eigenaar blijft volledig gereserveerd.
    Dispatch { slot: usize, core: usize },
    /// Een core draait nog waar hij stil had moeten staan.
    CoreBusy { core: usize },
    /// Stop kon de beëindiging niet bevestigen; de eigenaar blijft staan.
    NotStopped { slot: usize, core: usize },
    /// De kooi weigerde (bouwen, bereik, venster).
    Cage { slot: usize, code: u32 },
    /// Een ongeldig bereik voor een stage-2-map of grant.
    Range { base: u64, size: u64 },
    /// Een record (handoff, boom, frame) is kapot op deze byte-positie.
    Corrupt { at: usize },
    /// Een versie of magic klopt niet.
    Version { have: u64, want: u64 },
    /// Een lengte past niet in de grens.
    TooLarge { len: usize, max: usize },
    /// Het pad bestaat niet.
    NoEnt,
    /// Het pad is een directory waar een bestand hoorde (of andersom).
    Kind,
    /// De directory is niet leeg.
    NotEmpty,
    /// Een pad met `..` of een lege naam.
    BadPath,
    /// Een pad buiten het zicht van de taak: buiten de eigen root en de
    /// volumes (`errDenied` in Go).
    Denied,
    /// Het blokapparaat meldde een fout op dit LBA.
    Io { lba: u64 },
    /// De schijf (het venster) is vol.
    DiskFull { blocks: u64 },
    /// De verbinding is weg of meldde een fout.
    Conn,
    /// Deze operatie vraagt de bevoegdheid van Hop.
    Privilege { slot: usize },
    /// De brievenbus van de actor zit vol.
    Busy,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::SlotRange { slot, max } => write!(f, "slot {slot} out of range 1..{max}"),
            Self::CoreRange { core, max } => write!(f, "core {core} out of range 1..{max}"),
            Self::PartitionSize { size } => write!(f, "invalid partition size {size:#x}"),
            Self::NoPartition { size } => write!(
                f,
                "no partition space: {} MiB does not fit the pool",
                size >> 20
            ),
            Self::StillOwned { slot } => write!(f, "slot {slot} still owns a partition"),
            Self::NotOwned { slot } => write!(f, "slot {slot} owns no partition"),
            Self::Quarantined { slot } => {
                write!(f, "slot {slot}: owner retained, execution unconfirmed")
            }
            Self::NotFree { base, size } => {
                write!(f, "range {base:#x}+{size:#x} is not free in this pool")
            }
            Self::WindowBusy { base, size } => {
                write!(f, "kernel window {base:#x}+{size:#x} already borrowed")
            }
            Self::Full { cap } => write!(f, "table full ({cap} entries)"),
            Self::OutOfMemory { bytes } => write!(f, "out of memory ({bytes} bytes)"),
            Self::PoolSize { have, want } => write!(
                f,
                "sharegroup pool size differs: group has {have} core(s), spec asks {want}"
            ),
            Self::NoCores { cores } => write!(f, "no free run of {cores} app core(s)"),
            Self::ClassMismatch { core } => write!(f, "core {core} has another class"),
            Self::Dispatch { slot, core } => {
                write!(
                    f,
                    "slot {slot}: dispatch on core {core} failed, outcome unknown"
                )
            }
            Self::CoreBusy { core } => write!(f, "core {core} still running"),
            Self::NotStopped { slot, core } => {
                write!(f, "slot {slot}: core {core} not stopped after revocation")
            }
            Self::Cage { slot, code } => write!(f, "slot {slot}: cage refused (code {code})"),
            Self::Range { base, size } => write!(f, "invalid range {base:#x}+{size:#x}"),
            Self::Corrupt { at } => write!(f, "corrupt record at byte {at}"),
            Self::Version { have, want } => write!(f, "version {have:#x}, want {want:#x}"),
            Self::TooLarge { len, max } => write!(f, "length {len} exceeds {max}"),
            Self::NoEnt => f.write_str("does not exist"),
            Self::Kind => f.write_str("wrong kind (file or directory)"),
            Self::NotEmpty => f.write_str("directory not empty"),
            Self::BadPath => f.write_str("invalid path"),
            Self::Denied => f.write_str("outside the task's root and volumes"),
            Self::Io { lba } => write!(f, "block I/O failed at LBA {lba}"),
            Self::DiskFull { blocks } => write!(f, "disk full ({blocks} blocks)"),
            Self::Conn => f.write_str("connection closed"),
            Self::Privilege { slot } => {
                write!(f, "slot {slot}: privileged operation without privilege")
            }
            Self::Busy => f.write_str("actor mailbox full"),
        }
    }
}
