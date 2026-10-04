//! Legt het image van de eerste bewoner klaar: de LicheeRV heeft geen
//! QEMU die een image in het RAM legt, en de FSBL laadt alleen de FIP. Dus
//! gaat het image IN de kern (`slots::staged_image`), als
//! `image/licheerv-agent.sh` het vraagt (`STAGE=` of `APP=`, dat zet
//! `HOPOS_EMBED` op de gestripte ELF). Zonder die variabele een leeg
//! bestand: de kern plaatst dan niets (`HOPOS_SLOT_NONE`). De rol komt uit
//! `HOPOS_EMBED_ROLE`: `hop` (de eerste bewoner is Hop, zoals op elk board)
//! of `app` (een gewone app, het ABI-bewijs); de kern leest hem als cfg.
//! `HOPOS_EMBED` is dezelfde stap als op de Mac mini (board/apple/build.rs),
//! dus dezelfde naam.
//!
//! Dit script leest alleen een lokaal bestand dat de aanroeper noemt; het
//! praat met niemand (handboek §8).

use std::env;
use std::fs;
use std::path::PathBuf;

fn main() {
    let out = PathBuf::from(env::var_os("OUT_DIR").unwrap_or_default()).join("stage.bin");
    println!("cargo:rerun-if-env-changed=HOPOS_EMBED");
    println!("cargo:rerun-if-env-changed=HOPOS_EMBED_ROLE");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rustc-check-cfg=cfg(lrv_stage_hop)");
    let role = env::var("HOPOS_EMBED_ROLE").unwrap_or_default();
    assert!(
        matches!(role.as_str(), "" | "app" | "hop"),
        "HOPOS_EMBED_ROLE={role}: hop or app"
    );
    if role == "hop" {
        println!("cargo:rustc-cfg=lrv_stage_hop");
    }
    // Een build.rs is boot-code in de zin van het handboek (§6): falen is
    // hier een build die stopt, met de reden erbij.
    #[expect(
        clippy::expect_used,
        reason = "een build zonder het gevraagde image moet luid stoppen"
    )]
    match env::var_os("HOPOS_EMBED").filter(|p| !p.is_empty()) {
        Some(p) => {
            println!("cargo:rerun-if-changed={}", PathBuf::from(&p).display());
            fs::copy(&p, &out).expect("copy HOPOS_EMBED into OUT_DIR");
        }
        None => fs::write(&out, []).expect("write an empty stage.bin"),
    }
}
