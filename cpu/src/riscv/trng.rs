//! De TRNG van RISC-V: er is er geen.
//!
//! De SG2002 heeft een TRNG in zijn security-subsysteem, maar de
//! registerkaart ontbreekt in de vendor-tree (geen hwrng-driver, geen
//! DTS-knoop), en adressen gokken op ijzer is precies hoe je een board
//! stilzet (Go, board/licheerv/rng.go). QEMU virt heeft er ook geen (geen
//! Zkr). De DRBG seedt dan op timing-jitter uit de teller: veel beter dan
//! een boottijd-seed, maar geen hardware-entropie. Daarom is dit LUID, bij
//! elke boot, één regel die een operator niet kan missen: geen geheimen van
//! waarde op deze node.

/// Is er een hardware-TRNG? Op RISC-V (vandaag): nee.
#[must_use]
pub const fn available() -> bool {
    false
}

/// De bootregel die het zegt, met de marker uit de Go-kern.
pub const WARNING: &str = "trng: WARNING no hardware TRNG on this board: crypto (TLS keys, nonces) runs on a jitter-seeded DRBG, not hardware entropy; avoid high-value secrets on this node HOPOS_RNG_INSECURE";

/// Jitter-bytes uit de teller: de laagste bit van het verschil tussen twee
/// lezingen rond een korte, geheugenafhankelijke lus. Voor het seeden van de
/// DRBG, niet als bron op zich.
pub fn jitter(out: &mut [u8]) {
    let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
    for b in out.iter_mut() {
        let mut v = 0u8;
        for bit in 0..8 {
            let a = super::csr::rdtime();
            for _ in 0..16 {
                x = x.rotate_left(7) ^ x.wrapping_mul(0xbf58_476d_1ce4_e5b9);
            }
            let d = super::csr::rdtime().wrapping_sub(a) ^ x;
            v |= ((d & 1) as u8) << bit;
        }
        *b = v;
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn it_says_so() {
        assert!(!super::available());
        assert!(super::WARNING.contains("HOPOS_RNG_INSECURE"));
        let mut b = [0u8; 4];
        super::jitter(&mut b);
    }
}
