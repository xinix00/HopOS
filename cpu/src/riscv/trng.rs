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

/// De bootregel die het zegt, met de marker uit de Go-kern.
pub const WARNING: &str = "trng: WARNING no hardware TRNG on this board: crypto (TLS keys, nonces) runs on a jitter-seeded DRBG, not hardware entropy; avoid high-value secrets on this node HOPOS_RNG_INSECURE";

#[cfg(test)]
mod tests {
    #[test]
    fn it_says_so() {
        assert!(super::WARNING.contains("HOPOS_RNG_INSECURE"));
    }
}
