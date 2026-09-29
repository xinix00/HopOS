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
    println!("cargo:rerun-if-env-changed=HOPOS_LINK_BASE");
    println!("cargo:rerun-if-env-changed=HOPOS_STAMP");

    // Het linkscript, met op verzoek een andere basis: de tweede link van
    // de flip-bundel (image/flip-bundle.sh). Zonder HOPOS_LINK_BASE is het
    // byte voor byte link.ld.
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
    let name = if uefi {
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
    let script = match env::var("HOPOS_LINK_BASE") {
        Ok(base) if !base.is_empty() => {
            script.replace("KERN_BASE = 0x40200000;", &format!("KERN_BASE = {base};"))
        }
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
