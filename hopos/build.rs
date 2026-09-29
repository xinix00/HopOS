//! Het bouwscript van de kern-binary: het linkscript erbij, en de versie
//! van de compiler als tekst voor de `runtime`-regel op de console.
//!
//! Het praat niet naar buiten (handboek §8): het leest `$RUSTC --version`
//! van de gepinde toolchain en schrijft twee regels voor cargo.

use std::env;
use std::process::Command;

fn main() {
    let dir = env::var("CARGO_MANIFEST_DIR").unwrap_or_else(|_| ".".into());
    println!("cargo:rustc-link-search={dir}");
    println!("cargo:rustc-link-arg-bins=-Tlink.ld");
    println!("cargo:rerun-if-changed=link.ld");
    println!("cargo:rerun-if-changed=build.rs");

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
