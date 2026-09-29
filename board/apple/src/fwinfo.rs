//! Wat de firmware ons vertelt, bij de bron: x0 is de enige ingang.
//!
//! Daar staat iBoot's boot_args-blok in ([`fw::xnuboot`]), dat het
//! RAM-contract draagt en zegt waar de device tree ligt; die boom
//! ([`fw::adt`]) draagt de rest: cores, UART's, opslag, MAC, serienummer.
//! Tot 29-08 las een Python-loader de boom op de laptop en gaf de adressen
//! door in een param-blok, met per accessor een terugval: twee bronnen voor
//! hetzelfde feit. Nu is de boom de bron, en draagt het param-blok alleen
//! nog wat de loader áls enige weet: m1n1's spin-table ([`release_addr`])
//! en de config-tekst ([`config_text`]).
//!
//! Alles hier wordt één keer bij `discover` gevuld (op de boot-core, vóór
//! er taken zijn) en daarna alleen gelezen: atomics, geen slot.

use core::sync::atomic::{AtomicU8, AtomicU32, AtomicU64, AtomicUsize, Ordering::Relaxed};
use dev::Pa;
use fw::adt::Adt;
use fw::xnuboot::{self, Args};

/// Zoveel cores houden we bij (de M4 heeft er tien).
pub const MAX_CPUS: usize = 16;

/// Het boot_args-blok, zoals gelezen (0 = geen).
static BOOT_ARGS: AtomicU64 = AtomicU64::new(0);
/// De ADT: fysiek adres en maat.
static ADT: [AtomicU64; 2] = [const { AtomicU64::new(0) }; 2];
/// Het RAM-contract: phys_base, mem_size, top_of_kernel_data,
/// mem_size_actual.
static RAM: [AtomicU64; 4] = [const { AtomicU64::new(0) }; 4];

/// De cores uit `/cpus`: het reg-woord (cluster << 8 | core), de
/// cpu-impl-reg, en de soort (b'E', b'P', 0 = onbekend).
static NCPU: AtomicUsize = AtomicUsize::new(0);
static CPU_REG: [AtomicU32; MAX_CPUS] = [const { AtomicU32::new(0) }; MAX_CPUS];
static CPU_IMPL: [AtomicU64; MAX_CPUS] = [const { AtomicU64::new(0) }; MAX_CPUS];
static CPU_KIND: [AtomicU8; MAX_CPUS] = [const { AtomicU8::new(0) }; MAX_CPUS];

/// Het fysieke RAM uit boot_args op `x0`, met de MMU nog uit: alleen voor
/// de maat van de vroege map. Gealigneerde loads (de target heeft
/// `+strict-align`), dus veilig op Device-nGnRnE. 0 = geen boot_args.
pub(crate) fn early_mem_size(x0: u64) -> u64 {
    read_args(x0).map_or(0, |a| a.mem_size_actual)
}

/// Leest boot_args op `x0`, als het in het DRAM ligt.
fn read_args(x0: u64) -> Option<Args> {
    // Buiten het DRAM van Apple silicon lezen we niet: een verkeerde x0 (0,
    // of m1n1's FDT onder een oude loader) mag geen fault worden.
    let dram = crate::DRAM_BASE..crate::DRAM_BASE + (crate::mmu::MAX_DRAM_GB << 30);
    if !dram.contains(&x0) || !x0.is_multiple_of(8) {
        return None;
    }
    let len = xnuboot::MAX_LEN;
    // SAFETY: `x0` ligt in het DRAM (net getoetst) en de firmware legde het
    // blok daar; het is gemapt (Device, of fysiek met de MMU uit), wordt
    // niet meer beschreven, en we lezen `MAX_LEN` bytes die in hetzelfde
    // DRAM liggen.
    let b = unsafe { core::slice::from_raw_parts(x0 as usize as *const u8, len) };
    Args::read(b)
}

/// Leest boot_args en de ADT en onthoudt wat de kern later vraagt. Eén keer,
/// vanuit `discover`. Geeft de boot_args als ze er zijn.
pub(crate) fn load(x0: u64) -> Option<Args> {
    let a = read_args(x0)?;
    BOOT_ARGS.store(x0, Relaxed);
    RAM[0].store(a.phys_base, Relaxed);
    RAM[1].store(a.mem_size, Relaxed);
    RAM[2].store(a.top_of_kernel_data, Relaxed);
    RAM[3].store(a.mem_size_actual, Relaxed);
    if a.adt != 0 && a.adt_size != 0 {
        ADT[0].store(a.adt, Relaxed);
        ADT[1].store(u64::from(a.adt_size), Relaxed);
    }
    load_cpus();
    Some(a)
}

/// Het RAM-contract: (phys_base, mem_size, top_of_kernel_data,
/// mem_size_actual); nullen als er geen boot_args waren.
#[must_use]
pub(crate) fn ram() -> (u64, u64, u64, u64) {
    (
        RAM[0].load(Relaxed),
        RAM[1].load(Relaxed),
        RAM[2].load(Relaxed),
        RAM[3].load(Relaxed),
    )
}

/// De ADT die de firmware achterliet, gewogen.
#[must_use]
pub fn adt() -> Option<Adt<'static>> {
    let (pa, size) = (ADT[0].load(Relaxed), ADT[1].load(Relaxed));
    if pa == 0 {
        return None;
    }
    let len = usize::try_from(size).ok()?;
    // SAFETY: het adres en de maat komen uit boot_args en `xnuboot`
    // toetste dat de boom in het DRAM van de firmware ligt; dat DRAM is
    // gemapt (Device) en de boom wordt na de boot door niemand beschreven,
    // dus een `'static` leesslice is waar.
    let b = unsafe { core::slice::from_raw_parts(pa as usize as *const u8, len) };
    Adt::new(b).ok()
}

/// Het `i`-de registervenster van `path`, vertaald naar een fysiek adres.
#[must_use]
pub fn reg(path: &str, i: usize) -> Option<(u64, u64)> {
    adt()?.reg(path, i)
}

/// Leest `/cpus`: per core het reg-woord, de impl-reg en de soort.
fn load_cpus() {
    let Some(t) = adt() else { return };
    let Some(cpus) = t.path("/cpus") else { return };
    let mut n = 0;
    for c in t.children(cpus) {
        let Some(reg) = t.u32(c, "reg") else { continue };
        let (Some(r), Some(i), Some(k)) = (CPU_REG.get(n), CPU_IMPL.get(n), CPU_KIND.get(n)) else {
            break;
        };
        r.store(reg, Relaxed);
        i.store(t.u64(c, "cpu-impl-reg").unwrap_or(0), Relaxed);
        k.store(
            kind(t.str(c, "cluster-type"), t.str(c, "compatible")),
            Relaxed,
        );
        n += 1;
    }
    NCPU.store(n, Relaxed);
}

/// De soort van een core: `cluster-type` ("E"/"P", GEMETEN in de ADT-dump
/// van 31-08), anders de compatible (`apple,sawtooth` E, `apple,everest`
/// P).
fn kind(cluster_type: Option<&str>, compatible: Option<&str>) -> u8 {
    match (cluster_type, compatible) {
        (Some("E"), _) => b'E',
        (Some("P"), _) => b'P',
        (_, Some("apple,sawtooth")) => b'E',
        (_, Some("apple,everest")) => b'P',
        _ => 0,
    }
}

/// Het aantal cores dat de firmware beschrijft (0 = geen ADT).
#[must_use]
pub fn cpus() -> usize {
    NCPU.load(Relaxed)
}

/// Het reg-woord van core `i`: cluster << 8 | core, precies wat stub_reset
/// uit zijn eigen MPIDR haalt (aff1:aff0).
#[must_use]
pub fn cpu_reg(i: usize) -> Option<u32> {
    (i < cpus()).then(|| CPU_REG.get(i).map_or(0, |r| r.load(Relaxed)))
}

/// De cpu-impl-reg van core `i`: het blok met onder meer RVBAR.
#[must_use]
pub fn cpu_impl(i: usize) -> Option<u64> {
    (i < cpus()).then(|| CPU_IMPL.get(i).map_or(0, |r| r.load(Relaxed)))
}

/// Is core `i` een P-core?
#[must_use]
pub fn is_p_core(i: usize) -> bool {
    CPU_KIND.get(i).is_some_and(|k| k.load(Relaxed) == b'P')
}

/// De core met aff1:aff0 == `mpidr & 0xffff` (Apple's MPIDR: aff0 core,
/// aff1 cluster, aff2 een vaste bit; GEMETEN 28-08: cpu6 = 0x80010100).
#[must_use]
pub fn core_of(mpidr: u64) -> Option<usize> {
    let aff = (mpidr & 0xffff) as u32;
    (0..cpus()).find(|&i| cpu_reg(i) == Some(aff))
}

/// Het MAC-adres van de ingebouwde NIC: dezelfde bron die m1n1 voor Linux in
/// de device tree patcht, en de enige, want na een PERST draagt de chip
/// alleen nog Broadcom's default.
#[must_use]
pub fn nic_mac() -> Option<[u8; 6]> {
    let t = adt()?;
    let n = t.path("/arm-io/apcie/pci-bridge2/lan-1gb")?;
    let v = t.prop(n, "local-mac-address")?;
    let m = v.get(..6)?;
    Some([m[0], m[1], m[2], m[3], m[4], m[5]])
}

/// Het serienummer uit de wortel van de boom: de identiteit die een node
/// zonder loader nergens anders vandaan haalt.
#[must_use]
pub fn serial() -> Option<&'static str> {
    adt()?.str(fw::adt::Node::ROOT, "serial-number")
}

// ---------------------------------------------------------------------------
// Het param-blok van de loader: wat alleen de loader weet.
// ---------------------------------------------------------------------------

/// "HOPAPPLE", little-endian als één woord (pariteit met
/// `image/apple/load-probe.py`).
pub const PARAM_MAGIC: u64 = 0x454C_5050_4150_4F48;
/// De versie van het blok.
pub const PARAM_VERSION: u64 = 5;
const PARAM_RELEASE: u64 = 0x30;
/// De config-tekst van de loader: 4 KB `key=waarde`-regels (het
/// hopos.cfg-formaat).
pub const CFG_PA: u64 = crate::RAM_BASE + 0xF000;
/// De maat van de config-plek.
pub const CFG_SIZE: usize = 0x1000;

/// Staat er een param-blok van de loader? Zonder is dat de normale toestand
/// van een node die zonder loader boot, geen fout.
#[must_use]
pub fn has_params() -> bool {
    let p = Pa(crate::PARAMS);
    dev::read64(p) == PARAM_MAGIC && dev::read64(p.add(8)) == PARAM_VERSION
}

/// Het spin-table-release-adres van core `i` onder m1n1 (0 = niet
/// gestart of geen loader). m1n1's `struct spin_table`: target op +0,
/// args[0..3] op +8.
#[must_use]
pub fn release_addr(i: usize) -> u64 {
    if !has_params() || i >= MAX_CPUS {
        return 0;
    }
    dev::read64(Pa(crate::PARAMS + PARAM_RELEASE + 8 * i as u64))
}

/// De config-tekst die de loader op [`CFG_PA`] legde ("" als er geen is).
#[must_use]
pub fn config_text() -> &'static str {
    if !has_params() {
        return "";
    }
    // SAFETY: [`CFG_PA`, +4 KB) ligt in het image (Normal WB, de kern-RAM),
    // de loader schreef er vóór de sprong, en daarna schrijft niemand er.
    let b = unsafe { core::slice::from_raw_parts(CFG_PA as usize as *const u8, CFG_SIZE) };
    let end = b.iter().position(|&c| c == 0).unwrap_or(b.len());
    core::str::from_utf8(b.get(..end).unwrap_or_default()).unwrap_or("")
}

/// De x0 waarmee we binnenkwamen (boot_args), 0 als er geen was.
#[must_use]
pub fn boot_args_pa() -> u64 {
    BOOT_ARGS.load(Relaxed)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn core_kinds() {
        assert_eq!(kind(Some("E"), None), b'E');
        assert_eq!(kind(None, Some("apple,everest")), b'P');
        assert_eq!(kind(Some("P"), Some("apple,sawtooth")), b'P');
        assert_eq!(kind(None, Some("ARM,v8")), 0);
    }

    #[test]
    fn nonsense_x0_is_not_read() {
        assert_eq!(early_mem_size(0), 0);
        assert_eq!(early_mem_size(0x4000_0000), 0);
        assert_eq!(early_mem_size(crate::DRAM_BASE + 4), 0);
    }
}
