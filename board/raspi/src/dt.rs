//! Eén vraag aan de DTB die `fw::fdt` (nog) niet stelt: staat het device
//! met deze `compatible` aan?
//!
//! Waarom het board dat vraagt: een driver die een blok aanraakt dat er
//! niet is, krijgt een synchrone external abort, en dat is het einde van
//! de boot. Gemeten 29-09 op QEMU `raspi4b`: QEMU zet de GENET-node op
//! `status = "disabled"` (hij emuleert hem niet), en de eerste lees op
//! 0xFD58_0000 gaf ESR 0x96000010. Op de echte Pi 4 staat hij op "okay";
//! de firmware zet een device dat een overlay uitschakelt ook zo uit.
//!
//! Een kleine eigen lezer van het structure-blok (tokens 1 BEGIN_NODE, 2
//! END_NODE, 3 PROP, 4 NOP, 9 END), begrensd op elke offset, geen
//! allocatie.

/// Hoe diep de lezer nodes volgt; dieper wordt niet bekeken.
const MAX_DEPTH: usize = 16;

fn be32(b: &[u8], off: usize) -> Option<u32> {
    let w = b.get(off..off.checked_add(4)?)?;
    Some(u32::from_be_bytes([w[0], w[1], w[2], w[3]]))
}

fn cstr(b: &[u8], off: usize) -> Option<&[u8]> {
    let rest = b.get(off..)?;
    rest.get(..rest.iter().position(|&c| c == 0)?)
}

/// Staat de eerste node met `compatible` aan? `None` = geen zo'n node (of
/// een kromme blob); `Some(true)` = geen `status` of "okay"/"ok".
#[must_use]
pub fn enabled(blob: &[u8], compatible: &str) -> Option<bool> {
    if be32(blob, 0)? != 0xd00d_feed {
        return None;
    }
    let total = (be32(blob, 4)? as usize).min(blob.len());
    let blob = blob.get(..total)?;
    let structs = be32(blob, 8)? as usize;
    let strings = blob.get(be32(blob, 12)? as usize..)?;
    // Per diepte: past de compatible, en wat zegt status.
    let mut hit = [false; MAX_DEPTH];
    let mut ok = [true; MAX_DEPTH];
    let mut depth = 0usize;
    let mut p = structs;
    loop {
        let tok = be32(blob, p)?;
        p += 4;
        match tok {
            1 => {
                let name = cstr(blob, p)?;
                p = (p + name.len() + 1).next_multiple_of(4);
                depth += 1;
                if let (Some(h), Some(o)) = (hit.get_mut(depth), ok.get_mut(depth)) {
                    *h = false;
                    *o = true;
                }
            }
            2 => {
                if hit.get(depth).copied().unwrap_or(false) {
                    return ok.get(depth).copied();
                }
                depth = depth.checked_sub(1)?;
            }
            3 => {
                let len = be32(blob, p)? as usize;
                let name = cstr(strings, be32(blob, p + 4)? as usize)?;
                let val = blob.get(p + 8..(p + 8).checked_add(len)?)?;
                p = (p + 8 + len).next_multiple_of(4);
                if name == b"compatible"
                    && val.split(|&c| c == 0).any(|c| c == compatible.as_bytes())
                    && let Some(h) = hit.get_mut(depth)
                {
                    *h = true;
                }
                if name == b"status"
                    && let Some(o) = ok.get_mut(depth)
                {
                    let v = val.split(|&c| c == 0).next().unwrap_or(b"");
                    *o = v == b"okay" || v == b"ok";
                }
            }
            4 => {}
            _ => return None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const BLOB: &[u8] = include_bytes!("../testdata/nodes.dtb");

    #[test]
    fn status_decides_and_children_do_not_leak() {
        assert_eq!(enabled(BLOB, "brcm,bcm2711-genet-v5"), Some(false));
        assert_eq!(enabled(BLOB, "brcm,genet-mdio-v5"), Some(true));
        assert_eq!(enabled(BLOB, "brcm,bcm2711-pcie"), Some(true));
        assert_eq!(enabled(BLOB, "brcm,bcm2835-mbox"), Some(true));
        assert_eq!(enabled(BLOB, "brcm,bcm2712-pcie"), None);
        // De root-compatible is een lijst: het tweede woord telt ook.
        assert_eq!(enabled(BLOB, "brcm,bcm2711"), Some(true));
    }

    #[test]
    fn a_broken_blob_is_none() {
        assert_eq!(enabled(&BLOB[..40], "brcm,bcm2711-pcie"), None);
        assert_eq!(enabled(&[0; 64], "x"), None);
        let mut b = BLOB.to_vec();
        b[4..8].copy_from_slice(&0x40u32.to_be_bytes());
        assert_eq!(enabled(&b, "brcm,bcm2711-pcie"), None);
    }
}
