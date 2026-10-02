//! De lezer van het boot_args-blok dat iBoot op elke Apple-SoC in x0
//! meegeeft: het RAM-contract, het framebuffer en het adres van de device
//! tree.
//!
//! Naast [`crate::fdt`] en [`crate::adt`] omdat het net zo goed een
//! firmware-formaat is, en omdat het hier host-testbaar is. Het is de reden
//! dat het param-blok van de Go-loader kon verdwijnen: x0 is de enige
//! ingang, en alles hangt eraan (boot_args naar RAM, beeld en de boom; de
//! boom naar dockchannel, opslag, cores en MAC).
//!
//! Formaat: m1n1 `src/xnuboot.h`. De lengte van het cmdline-veld hangt aan
//! de revisie (1: 256, 2: 608, 3: 1024 bytes), en daarachter staan
//! `boot_flags` en de échte RAM-grootte. De Mac mini M4 meldt revisie 3.
//!
//! Op ijzer (29-08), en het ADT-adres is exact wat de Python-loader
//! uitrekende:
//!
//! ```text
//! xnuboot: rev 3.2  RAM 0x10001374000+24053MB (fysiek 24576MB)  firmware tot 0x10003478000
//! xnuboot: ADT 0x10002be0000+0x70000  framebuffer 0x105e5304000 640x1136 stride 2560
//! ```
//!
//! De Go-lezer las elk 64-bit-veld in twee helften omdat het blok op dat
//! moment van de boot ongecachet benaderd werd; deze lezer werkt op een
//! slice uit Normal-geheugen en leest bytegewijs, dus een scheve woordlees
//! bestaat hier niet. Revisie en versie delen één woord (u16 + u16).

use crate::bytes::{le16, le32, le64};

const OFF_REVISION: usize = 0x00;
const OFF_VERSION: usize = 0x02;
const OFF_VIRT_BASE: usize = 0x08;
const OFF_PHYS_BASE: usize = 0x10;
const OFF_MEM_SIZE: usize = 0x18;
const OFF_TOP_OF_KERN: usize = 0x20;
const OFF_VIDEO_BASE: usize = 0x28;
const OFF_VIDEO_W: usize = 0x40;
const OFF_VIDEO_H: usize = 0x48;
const OFF_DEV_TREE: usize = 0x60;
const OFF_DEV_TREE_SIZE: usize = 0x68;
const OFF_CMDLINE: usize = 0x70;

/// De lengte van het cmdline-veld per revisie.
const fn cmdline_len(rev: u16) -> usize {
    match rev {
        2 => 608,
        3 => 1024,
        _ => 256,
    }
}

/// Zoveel bytes moet de slice minstens hebben voor revisie 3 (de langste):
/// tot en met `mem_size_actual`.
pub const MAX_LEN: usize = OFF_CMDLINE + 1024 + 16;

const _: () = {
    assert!(OFF_VERSION == OFF_REVISION + 2);
    assert!(OFF_CMDLINE == OFF_DEV_TREE_SIZE + 8);
    assert!(MAX_LEN == 0x480);
};

/// Het framebuffer dat iBoot aanzette, zoals de bootregel het meldt.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Fb {
    /// Het fysieke adres van de pixels.
    pub base: u64,
    /// Breedte in pixels.
    pub width: u64,
    /// Hoogte in pixels.
    pub height: u64,
}

/// Wat we uit boot_args gebruiken.
#[derive(Copy, Clone, PartialEq, Eq, Debug, Default)]
pub struct Args {
    /// De revisie (1 tot en met 3).
    pub revision: u16,
    /// De versie.
    pub version: u16,
    /// De virtuele basis van de kernel van de firmware.
    pub virt_base: u64,
    /// Het begin van het RAM dat van ons is.
    pub phys_base: u64,
    /// ... en hoeveel.
    pub mem_size: u64,
    /// Tot waar de firmware het zelf gevuld heeft (kernel-image, ADT,
    /// trust cache): dit stuk is niet van ons.
    pub top_of_kernel_data: u64,
    /// Het fysiek aanwezige RAM.
    pub mem_size_actual: u64,
    /// Het framebuffer.
    pub fb: Fb,
    /// Het FYSIEKE adres van de device tree; 0 als het niet in het RAM
    /// van de firmware valt.
    pub adt: u64,
    /// De maat van de device tree.
    pub adt_size: u32,
    /// Het adres zoals het er staat: VIRTUEEL, ongecorrigeerd.
    pub dev_tree: u64,
}

impl Args {
    /// Leest het blok. `None` als het er niet plausibel uitziet (revisie
    /// buiten 1..=3, geen RAM-contract, of een te korte slice): dit is
    /// firmware-input, en een verkeerde pointer moet een lege waarde geven,
    /// geen halve waarheid.
    #[must_use]
    pub fn read(b: &[u8]) -> Option<Args> {
        let revision = le16(b, OFF_REVISION)?;
        if revision == 0 || revision > 3 {
            return None;
        }
        let mut a = Args {
            revision,
            version: le16(b, OFF_VERSION)?,
            virt_base: le64(b, OFF_VIRT_BASE)?,
            phys_base: le64(b, OFF_PHYS_BASE)?,
            mem_size: le64(b, OFF_MEM_SIZE)?,
            top_of_kernel_data: le64(b, OFF_TOP_OF_KERN)?,
            ..Args::default()
        };
        if a.phys_base == 0 || a.mem_size == 0 || a.top_of_kernel_data < a.phys_base {
            return None;
        }
        a.fb = Fb {
            base: le64(b, OFF_VIDEO_BASE)?,
            width: le64(b, OFF_VIDEO_W)?,
            height: le64(b, OFF_VIDEO_H)?,
        };
        a.dev_tree = le64(b, OFF_DEV_TREE)?;
        a.adt_size = le32(b, OFF_DEV_TREE_SIZE)?;
        a.adt = adt_phys(a.dev_tree, a.virt_base, a.phys_base, a.mem_size);
        // Achter het cmdline-veld: boot_flags, dan de échte RAM-grootte.
        a.mem_size_actual = le64(b, OFF_CMDLINE + cmdline_len(revision) + 8)?;
        Some(a)
    }
}

/// Het fysieke adres van de device tree: virtueel min virt_base plus
/// phys_base, MODULO 2^64 en zonder volgorde-aanname.
///
/// Hier stond in Go `if dt >= virt_base`, en dat leek redelijk. Maar iBoot
/// geeft virt_base niet altijd in dezelfde vorm; gemeten 30-08 op de M4,
/// twee boots van hetzelfde image:
///
/// ```text
/// virt_base 0x5374000          devtree 0x6388000   → ADT 0x10002388000
/// virt_base 0xffffffffff374000 devtree 0x1614000   → guard faalde, ADT 0
/// ```
///
/// Dezelfde lage bits, de tweede keer mét de hoge kernelbits erin. Het
/// verschil klopt in beide gevallen zodra je het laat wrappen; alleen de
/// vergelijking was fout. Dat kostte een boot waarin álles wegviel wat aan
/// de boom hangt (PCIe, dus ook netwerk en NVMe): het "1 van de 4
/// chain-boots"-raadsel. De toets die ervoor in de plaats kwam zegt wél
/// iets: het resultaat ligt in het RAM dat de firmware zelf beschrijft.
#[must_use]
pub const fn adt_phys(dev_tree: u64, virt_base: u64, phys_base: u64, mem_size: u64) -> u64 {
    let adt = dev_tree.wrapping_sub(virt_base).wrapping_add(phys_base);
    if adt >= phys_base && adt - phys_base < mem_size {
        adt
    } else {
        0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Het blok met de getallen die deze mini écht meldt (gemeten 29-08 via
    /// m1n1).
    fn m4() -> Vec<u8> {
        let mut b = vec![0u8; 0x800];
        let mut put = |off: usize, v: &[u8]| b[off..off + v.len()].copy_from_slice(v);
        put(OFF_REVISION, &3u16.to_le_bytes());
        put(OFF_VERSION, &2u16.to_le_bytes());
        put(OFF_VIRT_BASE, &0x1d37_4000u64.to_le_bytes());
        put(OFF_PHYS_BASE, &0x100_0137_4000u64.to_le_bytes());
        put(OFF_MEM_SIZE, &0x5_df56_c000u64.to_le_bytes());
        put(OFF_TOP_OF_KERN, &0x100_03b1_4000u64.to_le_bytes());
        put(OFF_VIDEO_BASE, &0x105_e530_4000u64.to_le_bytes());
        put(OFF_VIDEO_W, &640u64.to_le_bytes());
        put(OFF_VIDEO_H, &1136u64.to_le_bytes());
        put(OFF_DEV_TREE, &0x1f27_c000u64.to_le_bytes());
        put(OFF_DEV_TREE_SIZE, &0x7_0000u32.to_le_bytes());
        put(OFF_CMDLINE + 1024 + 8, &0x6_0000_0000u64.to_le_bytes());
        b
    }

    #[test]
    fn reads_the_m4_block() {
        let a = Args::read(&m4()).unwrap();
        assert_eq!((a.revision, a.version), (3, 2));
        assert_eq!((a.phys_base, a.mem_size), (0x100_0137_4000, 0x5_df56_c000));
        assert_eq!(a.top_of_kernel_data, 0x100_03b1_4000);
        // Revisie 3: cmdline van 1024 bytes.
        assert_eq!(a.mem_size_actual, 0x6_0000_0000);
        // De device tree staat er virtueel in; wij willen het fysieke adres.
        assert_eq!(a.adt, 0x1f27_c000 - 0x1d37_4000 + 0x100_0137_4000);
        assert_eq!(a.adt_size, 0x7_0000);
        assert_eq!(
            (a.fb.base, a.fb.width, a.fb.height),
            (0x105_e530_4000, 640, 1136)
        );
        // Het blok is precies zo lang als nodig: één byte minder is None.
        assert!(Args::read(&m4()[..MAX_LEN]).is_some());
        assert!(Args::read(&m4()[..MAX_LEN - 1]).is_none());
    }

    /// De twee vormen van virt_base van 30-08: allebei een adres in het RAM.
    #[test]
    fn virt_base_with_kernel_bits_wraps() {
        let phys = 0x100_0137_4000;
        let size = 0x5_df56_c000;
        // De gewone vorm: exact het adres uit de bootlog van die dag.
        assert_eq!(
            adt_phys(0x638_8000, 0x537_4000, phys, size),
            0x100_0238_8000
        );
        // Met de hoge kernelbits: de oude guard gaf hier 0.
        let wrapped = adt_phys(0x161_4000, 0xffff_ffff_ff37_4000, phys, size);
        assert_eq!(wrapped, phys + 0x22a_0000);
        // Buiten het RAM van de firmware: 0, geen halve waarheid.
        assert_eq!(adt_phys(0, 0x1000, phys, size), 0);
    }

    #[test]
    fn refuses_nonsense() {
        assert!(Args::read(&[]).is_none());
        let mut b = vec![0u8; 0x800];
        assert!(Args::read(&b).is_none(), "revisie 0");
        b[0] = 3;
        assert!(Args::read(&b).is_none(), "geen RAM-contract");
        b[0] = 4;
        assert!(Args::read(&b).is_none(), "revisie 4");
    }
}
