//! De EFI-stub: van de PE-header tot `kmain`.
//!
//! De firmware laadt het image (een PE32+ die `hopos/efi.ld` en
//! `image/uefi-run.sh` maken) op een willekeurig adres en roept
//! [`_start_efi`] aan als UEFI-app: x0 = ImageHandle, x1 = SystemTable, de
//! MMU van de firmware aan (identity), op de stack van de firmware, op EL2
//! op servers en op QEMU met `virtualization=on`. De stappen:
//!
//! 1. `_start_efi` (assembly): de relocaties toepassen (de kern is als PIE
//!    op 0 gelinkt; elke `R_AARCH64_RELATIVE` wordt basis + addend), BSS
//!    wissen, en [`hopos_efi_main`] aanroepen. Faalt die, dan keert de stub
//!    terug naar de firmware met een EFI-status: een stick die niet boot,
//!    valt terug in het bootmenu in plaats van stil te hangen.
//! 2. [`hopos_efi_main`] (Rust, boot services leven nog): banner op ConOut,
//!    de RSDP uit de configuratietabel, de ACPI-feiten, `hopos.cfg` en het
//!    gestagede image van de ESP, het kernvenster (heap, DMA, kooi), de
//!    identity map van 48 bits uit de memory map, en `ExitBootServices`.
//! 3. `uefi_enter_kernel` (assembly): MMU uit, EL2 saneren (HCR_EL2 vers
//!    met E2H = 0, de trap-registers op "geen traps"), onze map aan, de
//!    eigen stack en vectoren, en `kmain(0, el)`: dezelfde `kmain` als op
//!    virt.
//!
//! Waarom de relocatie van ons is en niet van de firmware: EDK2 past
//! PE-relocaties toe, maar dan moest een host-tool de ELF-relocaties naar
//! een `.reloc`-sectie vertalen. De Linux-arm64-weg is korter: een header
//! met een lege relocatie-directory (EDK2: "Cannot find relocations, good
//! just continue"), niet RELOCS_STRIPPED zodat de firmware hem overal mag
//! leggen, en de image verhuist zichzelf. Relocaties vallen alleen in de
//! RW-sectie: EDK2's image-protection mapt de code-sectie read-only (Go-les
//! 13-07: een store in één RWX-sectie gaf een data abort), en
//! `image/uefi-run.sh` weigert een image met een relocatie in de tekst.
//!
//! De EL-eis blijft: op EL1 (firmware zonder EL2) nemen we geen eigen MMU
//! (dat zijn andere registers) en laat `kmain` op de SPCR-console luid
//! weten dat HopOS EL2 eist, precies zoals op virt.

use crate::efi::{self, Efi, Status};
use crate::memmap::{self, Map};
use crate::mmu::{self, Mmu};
use crate::{ADMIN, DMA, TABLES, WINDOW, WINDOW_PA, facts, slots};
use core::fmt::Write as _;
use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use cpu::boot::{ATTR_DEVICE, ATTR_NORMAL, ATTR_NORMAL_NC};
use cpu::println;
use dev::Pa;

/// De SystemTable voor de console-haak zolang de boot services leven; 0 =
/// geen firmware-console meer.
static CON_ST: AtomicU64 = AtomicU64::new(0);

/// De console vóór de exit: ConOut van de firmware.
fn efi_console(b: &[u8]) {
    let st = CON_ST.load(Relaxed);
    if st == 0 {
        return;
    }
    let s = core::str::from_utf8(b).unwrap_or("?");
    // SAFETY: `CON_ST` staat alleen op een SystemTable zolang de boot
    // services leven; de stub zet hem op 0 vóór `ExitBootServices`.
    unsafe { efi::con_out(st, s) };
}

/// De naam van de config op de ESP-root.
const CFG_NAME: [u16; 10] = efi::ucs2(b"hopos.cfg\0");
/// De naam van een gestaged app-image op de ESP-root (de UEFI-tegenhanger
/// van QEMU's `-device loader` op virt).
const STAGE_NAME: [u16; 16] = efi::ucs2(b"hopos-stage.elf\0");
/// De grootste config: 16 KB (Go: bij 4 KB verdween alles na byte 4096
/// stil, 17-09).
const CFG_MAX: u64 = 16 << 10;
/// De buffer van de memory map: 64 KB (EDK2 op QEMU: ~60 descriptors van
/// 0x30; een server een paar honderd).
const MAP_CAP: usize = 64 << 10;
/// Scratch voor één ACPI-tabel tegelijk (de MADT van 128 cores is ~10 KB).
const ACPI_SCRATCH: usize = 64 << 10;

/// De UEFI-ingang in Rust. Keert alleen terug bij een fout (naar de
/// firmware); bij succes gaat hij de kern in.
#[unsafe(no_mangle)]
extern "C" fn hopos_efi_main(image: u64, st: u64) -> Status {
    // SAFETY: x0 en x1 van de UEFI-ingang, doorgegeven door `_start_efi`;
    // de boot services leven en dit is de enige `Efi`.
    let efi = unsafe { Efi::new(image, st) };
    CON_ST.store(st, Relaxed);
    cpu::console::set_sink(efi_console);
    let el = crate::arch::current_el();
    facts::BOOT_EL.store(el, Relaxed);
    println!("\r\nHopOS: UEFI stub, EL{el}");
    efi.watchdog_off();
    match prepare(&efi, el) {
        Ok(enter) => go(efi, enter),
        Err((what, st)) => {
            println!("FAIL uefi: {what} (status {st:#x}) HOPOS_UEFI_FAIL");
            st
        }
    }
}

/// Wat de sprong nodig heeft.
struct Enter {
    ttbr0: u64,
    tcr: u64,
    el: u8,
    map_buf: u64,
    tables: (u64, u64),
}

/// Alles vóór de exit. Een fout is een zin en een EFI-status.
fn prepare(efi: &Efi, el: u8) -> Result<Enter, (&'static str, Status)> {
    if let Some((base, size)) = efi.image() {
        facts::IMAGE[0].store(base, Relaxed);
        facts::IMAGE[1].store(size, Relaxed);
        println!(
            "uefi: image at {base:#x}, {} KB HOPOS_UEFI_STUB",
            size >> 10
        );
    }
    let rsdp = efi.config_table(&fw::acpi::ACPI_20_GUID).ok_or((
        "no ACPI 2.0 RSDP in the configuration table",
        efi::NOT_FOUND,
    ))?;
    let scratch = efi
        .allocate((ACPI_SCRATCH / 4096) as u64, 0)
        .map_err(|s| ("acpi scratch", s))?;
    // SAFETY: `[scratch, +ACPI_SCRATCH)` is net voor ons gealloceerd
    // (EfiLoaderData), identity-gemapt door de firmware, en alleen hier in
    // gebruik.
    let buf = unsafe { core::slice::from_raw_parts_mut(scratch as usize as *mut u8, ACPI_SCRATCH) };
    let tables = facts::discover(rsdp, buf, crate::arch::mpidr())
        .map_err(|_| ("ACPI tables unreadable", efi::LOAD_ERROR))?;
    acpi_line(&tables, rsdp);

    // Het kernvenster op zijn vaste plek: de vraag "is dit venster vrij op
    // dít board?". Nee is luid, met de vrije regio's erbij: één boot levert
    // zo het juiste venster op (Go, Altra 13-07: 0x9000_0000 was bezet).
    let map_buf = efi
        .allocate((MAP_CAP / 4096) as u64, 0)
        .map_err(|s| ("memory map buffer", s))?;
    if let Err(st) = efi.allocate_at(WINDOW_PA, WINDOW / 4096) {
        println!(
            "uefi: kernel window {WINDOW_PA:#x}+{WINDOW:#x} is taken; free regions of 32 MB or more:"
        );
        if let Ok(info) = efi.memory_map(map_buf, MAP_CAP) {
            let map = Map {
                pa: map_buf,
                size: info.size as u64,
                stride: info.desc_size as u64,
            };
            for d in map.iter().filter(|d| d.ty == memmap::ty::CONVENTIONAL) {
                if d.end() - d.base >= 32 << 20 {
                    println!("uefi:   {:#x}..{:#x}", d.base, d.end());
                }
            }
        }
        return Err(("kernel window busy HOPOS_UEFI_WINDOW", st));
    }
    // De staging-woorden op nul: QEMU begon met nul-RAM, echt ijzer niet.
    dev::write64(Pa(slots::STAGE_HDR_PA), 0);
    dev::write64(Pa(slots::STAGE_ROLE_PA), 0);

    if let Some(f) = efi.read_file(&CFG_NAME, CFG_MAX, None) {
        facts::CFG[0].store(f.pa, Relaxed);
        facts::CFG[1].store(f.len, Relaxed);
    }
    if let Some(f) = efi.read_file(&STAGE_NAME, slots::STAGE_MAX, Some(slots::STAGE_PA)) {
        facts::STAGE[0].store(f.pa, Relaxed);
        facts::STAGE[1].store(f.len, Relaxed);
        dev::write64(Pa(slots::STAGE_HDR_PA), f.len);
    }
    // Zonder image geen Hop om te missen: dan de app-rol, en de kern zegt
    // "nothing placed" in plaats van "Hop not started".
    let role = if facts::STAGE[1].load(Relaxed) == 0 {
        0
    } else {
        stage_role(cfg_text())
    };
    dev::write64(Pa(slots::STAGE_ROLE_PA), role);
    println!(
        "uefi: hopos.cfg {} bytes, hopos-stage.elf {} bytes (role {})",
        facts::CFG[1].load(Relaxed),
        facts::STAGE[1].load(Relaxed),
        if role == 0 { "app" } else { "hop" }
    );

    let info = efi
        .memory_map(map_buf, MAP_CAP)
        .map_err(|s| ("GetMemoryMap", s))?;
    let map = Map {
        pa: map_buf,
        size: info.size as u64,
        stride: info.desc_size as u64,
    };
    let mmu =
        build_map(&map).map_err(|_| ("identity map: out of tables", efi::OUT_OF_RESOURCES))?;
    facts::MMU_TABLES.store(mmu.tables(), Relaxed);
    println!(
        "uefi: window {WINDOW_PA:#x}+{WINDOW:#x}, {} MB DRAM, {} map entries, {} page tables",
        map.ram_bytes() >> 20,
        map.iter().count(),
        mmu.tables()
    );
    Ok(Enter {
        ttbr0: mmu.root(),
        tcr: tcr(),
        el,
        map_buf,
        tables: mmu.used_range(),
    })
}

/// De ACPI-samenvatting op de firmware-console: de meting waarmee een
/// stille boot in fases valt (Go: "W G P C" op de O6N, 09-09).
fn acpi_line(t: &fw::acpi::Tables, rsdp: u64) {
    let mut sigs = heapless_line();
    for s in t.sigs() {
        let _ = write!(sigs, "{} ", core::str::from_utf8(&s).unwrap_or("?"));
    }
    println!(
        "acpi: RSDP {rsdp:#x} rev {} OEM {:?}, tables {}",
        t.revision(),
        t.oem_id(),
        sigs.as_str().trim_end()
    );
    println!(
        "acpi: {} cores, GICD {:#x} v{}, GICR {:#x}, ITS {:#x}, console type {} at {:#x}, {} ECAM",
        facts::CORES.load(Relaxed),
        facts::GICD.load(Relaxed),
        facts::GIC_VERSION.load(Relaxed),
        facts::GICR_BASE
            .load(Relaxed)
            .max(facts::GICR_RANGE[0].load(Relaxed)),
        facts::ITS.load(Relaxed),
        facts::CONSOLE_TYPE.load(Relaxed),
        facts::CONSOLE_BASE.load(Relaxed),
        facts::ECAMS.load(Relaxed)
    );
}

/// Een regel tekst op de stack (geen heap vóór `kmain`).
struct Line {
    buf: [u8; 192],
    len: usize,
}

impl Line {
    fn as_str(&self) -> &str {
        core::str::from_utf8(self.buf.get(..self.len).unwrap_or(&[])).unwrap_or("")
    }
}

impl core::fmt::Write for Line {
    fn write_str(&mut self, s: &str) -> core::fmt::Result {
        let room = self.buf.len() - self.len;
        let n = s.len().min(room);
        if let (Some(dst), Some(src)) = (
            self.buf.get_mut(self.len..self.len + n),
            s.as_bytes().get(..n),
        ) {
            dst.copy_from_slice(src);
            self.len += n;
        }
        Ok(())
    }
}

fn heapless_line() -> Line {
    Line {
        buf: [0; 192],
        len: 0,
    }
}

/// De identity map uit de memory map; zie `mmu` voor de voorrang.
fn build_map(map: &Map) -> Result<Mmu, mmu::Error> {
    // SAFETY: de tabelpool ligt in het kernvenster dat de stub net
    // claimde, 2 MB-gealigneerd, en niemand anders gebruikt hem.
    let mut m = unsafe { Mmu::new(TABLES.base.0, TABLES.size / 4096) }?;
    let dev_bits = mmu::attrs(ATTR_DEVICE);
    m.map(0, mmu::DEVICE_SPAN, dev_bits)?;
    // Wat de firmware boven de standaard-span aanwijst.
    let mut high = |pa: u64, size: u64| -> Result<(), mmu::Error> {
        if (mmu::DEVICE_SPAN..mmu::VA_LIMIT).contains(&pa) && size != 0 {
            let lo = pa & !((1 << 30) - 1);
            let hi = pa
                .saturating_add(size)
                .next_multiple_of(1 << 30)
                .min(mmu::VA_LIMIT);
            m.map(lo, hi - lo, dev_bits)?;
        }
        Ok(())
    };
    high(facts::CONSOLE_BASE.load(Relaxed), 0x1000)?;
    high(facts::GICD.load(Relaxed), 0x1_0000)?;
    high(
        facts::GICR_RANGE[0].load(Relaxed),
        facts::GICR_RANGE[1].load(Relaxed),
    )?;
    high(facts::ITS.load(Relaxed), 0x2_0000)?;
    for (base, _, start, end) in facts::ecams() {
        let size = (u64::from(end) + 1).saturating_sub(u64::from(start)) << 20;
        high(base + (u64::from(start) << 20), size)?;
    }
    for d in map.iter().filter(|d| d.ty == memmap::ty::MMIO) {
        high(d.base, d.end() - d.base)?;
    }
    let ram_bits = mmu::attrs(ATTR_NORMAL);
    for d in map.iter().filter(memmap::Desc::is_ram) {
        m.map(d.base, d.end() - d.base, ram_bits)?;
    }
    m.map(DMA.base.0, DMA.size, mmu::attrs(ATTR_NORMAL_NC))?;
    m.map(ADMIN.base.0, ADMIN.size, dev_bits)?;
    Ok(m)
}

/// `hopos.cfg` zoals de stub hem las (leeg zonder bestand).
fn cfg_text() -> &'static str {
    crate::Uefi::new().config()
}

/// De rol van de staging uit `hopos.stage` (0 = app, 1 = Hop; zonder
/// sleutel Hop, zoals op de Pi's: op ijzer is wat er gestaged is Hop).
fn stage_role(cfg: &str) -> u64 {
    match fw::bootcfg::first(fw::bootcfg::all(cfg, "hopos.stage")) {
        "app" => 0,
        _ => 1,
    }
}

/// TCR_EL2 (niet-VHE) voor 48 bits: T0SZ = 16, walks WB-WA inner
/// shareable, 4 KB-korrel, PS uit ID_AA64MMFR0_EL1.PARange (tot 48 bits:
/// 52 bits vraagt een andere korrel), en de RES1-bits 23 en 31.
fn tcr() -> u64 {
    let ps = (crate::arch::mmfr0() & 0xf).min(5);
    (1 << 31) | (1 << 23) | (ps << 16) | (3 << 12) | (1 << 10) | (1 << 8) | 16
}

/// De exit en de sprong. Keert alleen terug als de firmware niet loslaat.
fn go(efi: Efi, e: Enter) -> Status {
    println!("uefi: exit boot services");
    // Vanaf hier geen ConOut meer: een print kan alloceren en de MapKey
    // ongeldig maken, en na de exit is er geen firmware-console.
    CON_ST.store(0, Relaxed);
    let final_map = match efi.exit_boot_services(e.map_buf, MAP_CAP) {
        Ok(m) => m,
        Err((efi, st)) => {
            CON_ST.store(efi_st(&efi), Relaxed);
            println!("FAIL uefi: ExitBootServices refused (status {st:#x}) HOPOS_UEFI_FAIL");
            return st;
        }
    };
    facts::MAP[0].store(e.map_buf, Relaxed);
    facts::MAP[1].store(final_map.size as u64, Relaxed);
    facts::MAP[2].store(final_map.desc_size as u64, Relaxed);
    // De sprong zet de MMU even uit: wat de core in dat venster ophaalt
    // (de code van de sprong) en wat de walker straks leest (de tabellen)
    // moet in het geheugen staan, niet alleen in de cache.
    let (img, len) = (facts::IMAGE[0].load(Relaxed), facts::IMAGE[1].load(Relaxed));
    dev::push(Pa(img), usize::try_from(len).unwrap_or(0));
    dev::push(Pa(e.tables.0), usize::try_from(e.tables.1).unwrap_or(0));
    crate::arch::enter_kernel(e.ttbr0, e.tcr, e.el, cnthctl())
}

/// CNTHCTL_EL2 (niet-VHE): de EL1-teller en -timer open (EL1PCTEN,
/// EL1PCEN), en de event-stream van EL2 aan met dezelfde EVNTI-keuze als
/// `cpu::idle` voor CNTKCTL_EL1 maakt: onder E2H = 0 bepaalt CNTHCTL_EL2 de
/// stream van EL2, en zonder stream wekt een WFE van de kern alleen op een
/// SEV of een interrupt (de Pi-ingang zet hetzelfde; bevinding van de
/// Pi-bring-up, 30-09). De bits van EVNTEN, EVNTI en EVNTIS staan in beide
/// registers op dezelfde plek. Op de Altra (25 MHz) is dat EVNTI 14.
fn cnthctl() -> u64 {
    let ecv = (crate::arch::mmfr0() >> 60) & 0xf >= 1;
    let stream = cpu::idle::event_stream(cpu::idle::freq(), ecv, cpu::idle::EVENT_STREAM_MAX_NS);
    0b11 | stream
}

/// De SystemTable terug uit een `Efi` die de exit weigerde (alleen voor de
/// foutregel).
fn efi_st(efi: &Efi) -> u64 {
    efi.system_table()
}
