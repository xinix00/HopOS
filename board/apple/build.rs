//! De ingebakken stage: `HOPOS_EMBED=<pad>` (image/apple-m4.sh `EMBED=`)
//! bakt een gestripte app-ELF in het kernimage, zodat een Mac die zonder
//! loader boot (kmutil, het rauwe bootobject) toch een bewoner heeft: Hop.
//! Go deed dit met `go:embed` (cmd/hopos-embed). Zonder de variabele is
//! het bestand leeg en is er niets ingebakken.

use std::env;
use std::error::Error;
use std::fs;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    println!("cargo:rerun-if-env-changed=HOPOS_EMBED");
    let out = PathBuf::from(env::var("OUT_DIR")?);
    let dst = out.join("embed.elf");
    match env::var("HOPOS_EMBED") {
        Ok(p) if !p.is_empty() => {
            println!("cargo:rerun-if-changed={p}");
            let bytes = fs::read(&p).map_err(|e| format!("HOPOS_EMBED {p}: {e}"))?;
            fs::write(&dst, bytes)?;
        }
        _ => fs::write(&dst, [])?,
    }
    Ok(())
}
