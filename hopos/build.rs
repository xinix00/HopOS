//! Het bouwscript van de kern-binary: het linkscript erbij (op verzoek op
//! een schaduwbasis, voor de flip-bundel), het versie-stempel, en de versie
//! van de compiler als tekst voor de `runtime`-regel op de console.
//!
//! Het praat niet naar buiten (handboek §8): het leest `$RUSTC --version`
//! van de gepinde toolchain en schrijft twee regels voor cargo.

use std::env;
use std::process::Command;

fn main() {
    let dir = env::var("CARGO_MANIFEST_DIR").unwrap_or_else(|_| ".".into());
    println!("cargo:rerun-if-changed=link.ld");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-env-changed=HOPOS_LINK_SHIFT");
    println!("cargo:rerun-if-env-changed=HOPOS_STAMP");

    // Het linkscript, met op verzoek een verschoven basis: de tweede link
    // van de flip-bundel (image/flip-bundle.sh). Zonder HOPOS_LINK_SHIFT is
    // het byte voor byte het script van het board.
    // De Pi's laden het image rauw op 0x80000: een eigen linkscript.
    let pi = env::var_os("CARGO_FEATURE_BOARD_RPI4").is_some()
        || env::var_os("CARGO_FEATURE_BOARD_RPI5").is_some();
    // De Radxa Zero 3E: een arm64-Image voor U-Boot's booti op 0x0220_0000.
    let rk3566 = env::var_os("CARGO_FEATURE_BOARD_RK3566").is_some();
    // UEFI (board-uefi en wat erop bouwt): een PIE op basis 0 dat de stub
    // zelf reloceert (board/uefi/src/boot.rs), met de PE-header in de eerste
    // pagina. PIE vraagt dat ELKE crate met relocation-model=pie gebouwd is
    // (anders staan pointers in .rodata, die EDK2 read-only mapt):
    // image/uefi-run.sh zet dat; zonder weigert de build luid.
    let uefi = env::var_os("CARGO_FEATURE_BOARD_UEFI").is_some()
        || env::var_os("CARGO_FEATURE_BOARD_O6N").is_some()
        || env::var_os("CARGO_FEATURE_BOARD_ALTRA").is_some();
    println!("cargo:rerun-if-changed=efi.ld");
    if uefi {
        let flags = env::var("CARGO_ENCODED_RUSTFLAGS").unwrap_or_default();
        if !flags.contains("relocation-model=pie") {
            println!(
                "cargo:warning=board-uefi needs RUSTFLAGS=\"-C relocation-model=pie\" (image/uefi-run.sh sets it)"
            );
            std::process::exit(1);
        }
        println!("cargo:rustc-link-arg-bins=-pie");
        println!("cargo:rustc-link-arg-bins=--no-dynamic-linker");
    }
    // RISC-V (board-qemuvirt-riscv, board-licheerv): `link-riscv.ld`, met de
    // basis van het board. QEMU `-bios none` springt elk hart naar
    // 0x8000_0000; de FSBL van de LicheeRV laadt het MONITOR-slot op
    // 0x8400_0000 (HopBase in Go: daaronder draait de FSBL zelf nog, 19-08).
    let licheerv = env::var_os("CARGO_FEATURE_BOARD_LICHEERV").is_some();
    let riscv = licheerv || env::var_os("CARGO_FEATURE_BOARD_QEMUVIRT_RISCV").is_some();
    println!("cargo:rerun-if-changed=link-riscv.ld");
    // De Mac mini M4: een raw image op 1 TiB + 4 GB met de bootstub vooraan
    // (board/apple/src/head.rs), `link-apple.ld`.
    let apple = env::var_os("CARGO_FEATURE_BOARD_APPLE").is_some();
    println!("cargo:rerun-if-changed=link-apple.ld");
    let name = if riscv {
        "link-riscv.ld"
    } else if apple {
        "link-apple.ld"
    } else if uefi {
        "efi.ld"
    } else if pi {
        "link-raspi.ld"
    } else if rk3566 {
        "link-rk3566.ld"
    } else {
        "link.ld"
    };
    println!("cargo:rerun-if-changed=link-raspi.ld");
    println!("cargo:rerun-if-changed=link-rk3566.ld");
    let script = std::fs::read_to_string(format!("{dir}/{name}")).unwrap_or_default();
    // De LicheeRV linkt op zijn eigen basis (hierboven): één riscv-script,
    // twee boards.
    let script = if licheerv {
        script
            .replace("KERN_BASE = 0x80000000;", "KERN_BASE = 0x84000000;")
            .replace("KERN_SIZE = 0x0f000000;", "KERN_SIZE = 0x02800000;")
    } else {
        script
    };
    let script = match env::var("HOPOS_LINK_SHIFT") {
        Ok(shift) if !shift.is_empty() => shifted(&script, &shift),
        _ => script,
    };
    let out = env::var("OUT_DIR").unwrap_or_else(|_| dir.clone());
    if std::fs::write(format!("{out}/link.ld"), script).is_err() {
        println!("cargo:warning=cannot write {out}/link.ld");
    }
    println!("cargo:rustc-link-search={out}");
    println!("cargo:rustc-link-arg-bins=-T{out}/link.ld");

    // Het versie-stempel op de boot-regel: twee kernen uit dezelfde bron
    // zijn na een flip alleen zo uit elkaar te houden (tools/qemu-test-flip.sh).
    let stamp = env::var("HOPOS_STAMP").unwrap_or_else(|_| "dev".into());
    println!("cargo:rustc-env=HOPOS_STAMP={stamp}");

    let rustc = env::var("RUSTC").unwrap_or_else(|_| "rustc".into());
    let version = Command::new(rustc)
        .arg("--version")
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_owned())
        .unwrap_or_else(|| "rustc unknown".into());
    println!("cargo:rustc-env=HOPOS_RUSTC={version}");
}

/// Het linkscript met zijn basis `shift` (hex) hoger: de schaduwlink van de
/// flip-bundel. De basis is de eerste regel `IMAGE_BASE = 0x...;` (de
/// Radxa: `KERN_BASE` en `KERN_END` rekenen daarvandaan) of anders
/// `KERN_BASE = 0x...;` (virt en de Pi's). Een script zonder zo'n regel,
/// of een verschuiving die geen getal is, laat de bouw hard falen: een
/// schaduwlink op dezelfde basis zou de relocatietabel leeg en de bundel
/// fout maken.
fn shifted(script: &str, shift: &str) -> String {
    let hex = |v: &str| u64::from_str_radix(v.trim().trim_start_matches("0x"), 16).ok();
    let Some(delta) = hex(shift) else {
        println!("cargo:warning=HOPOS_LINK_SHIFT={shift} is not a hex number");
        std::process::exit(1);
    };
    for name in ["IMAGE_BASE", "KERN_BASE"] {
        let head = format!("{name} = 0x");
        let Some(line) = script
            .lines()
            .find(|l| l.starts_with(&head) && l.ends_with(';'))
        else {
            continue;
        };
        let value = line
            .strip_prefix(&format!("{name} = "))
            .and_then(|v| v.strip_suffix(';'))
            .and_then(hex);
        if let Some(base) = value.and_then(|b| b.checked_add(delta)) {
            return script.replacen(line, &format!("{name} = {base:#x};"), 1);
        }
    }
    println!("cargo:warning=no IMAGE_BASE or KERN_BASE line to shift in the link script");
    std::process::exit(1);
}
