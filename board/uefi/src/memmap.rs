//! De EFI-memory-map: lezen, tellen, en het vrije DRAM voor de pool van
//! de slots eruit halen.
//!
//! De kaart ligt in door de stub gealloceerd geheugen (EfiLoaderData) en
//! wordt per descriptor met `dev` gelezen. De stap tussen twee descriptors
//! is wat de firmware zei (`DescriptorSize`), nooit `sizeof`: EDK2 geeft
//! 0x30 waar de struct 0x28 is (Go-les, 13-07).
//!
//! Wat na `ExitBootServices` van ons is: EfiConventionalMemory en de
//! BootServices-code en -data. De Loader-typen zijn ook van ons, maar dat
//! zijn precies onze eigen allocaties (image, kernvenster, tabellen,
//! staging): die tellen niet mee als vrij. Runtime-, ACPI-, gereserveerd en
//! MMIO blijven van de firmware.

use board::Region;
use bounded::BoundedVec;
use dev::Pa;

/// De paginamaat van de kaart.
pub(crate) const PAGE: u64 = 4096;

/// De korrel van de pool (en van `abi::layout::Plan`): 2 MB.
pub(crate) const GRAIN: u64 = 2 << 20;

/// De EFI-geheugentypen die we onderscheiden.
pub(crate) mod ty {
    /// EfiLoaderCode: onze image.
    pub(crate) const LOADER_CODE: u32 = 1;
    /// EfiLoaderData: onze allocaties.
    pub(crate) const LOADER_DATA: u32 = 2;
    /// EfiBootServicesCode.
    pub(crate) const BS_CODE: u32 = 3;
    /// EfiBootServicesData.
    pub(crate) const BS_DATA: u32 = 4;
    /// EfiRuntimeServicesCode.
    pub(crate) const RT_CODE: u32 = 5;
    /// EfiRuntimeServicesData.
    pub(crate) const RT_DATA: u32 = 6;
    /// EfiConventionalMemory.
    pub(crate) const CONVENTIONAL: u32 = 7;
    /// EfiACPIReclaimMemory: de ACPI-tabellen.
    pub(crate) const ACPI_RECLAIM: u32 = 9;
    /// EfiACPIMemoryNVS.
    pub(crate) const ACPI_NVS: u32 = 10;
    /// EfiMemoryMappedIO.
    pub(crate) const MMIO: u32 = 11;
    /// EfiPersistentMemory.
    pub(crate) const PERSISTENT: u32 = 14;
}

/// Eén descriptor.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) struct Desc {
    /// Het EFI-geheugentype.
    pub(crate) ty: u32,
    /// Het fysieke begin.
    pub(crate) base: u64,
    /// Het aantal pagina's van 4 KB.
    pub(crate) pages: u64,
}

impl Desc {
    /// Het eerste adres erna (verzadigend: een kromme descriptor loopt niet
    /// om).
    pub(crate) fn end(&self) -> u64 {
        self.base.saturating_add(self.pages.saturating_mul(PAGE))
    }

    /// Is dit RAM (Normal te mappen), van wie het ook is?
    pub(crate) fn is_ram(&self) -> bool {
        matches!(
            self.ty,
            ty::LOADER_CODE
                | ty::LOADER_DATA
                | ty::BS_CODE
                | ty::BS_DATA
                | ty::RT_CODE
                | ty::RT_DATA
                | ty::CONVENTIONAL
                | ty::ACPI_RECLAIM
                | ty::ACPI_NVS
                | ty::PERSISTENT
        )
    }

    /// Is dit na de exit vrij voor de slots?
    pub(crate) fn is_free(&self) -> bool {
        matches!(self.ty, ty::BS_CODE | ty::BS_DATA | ty::CONVENTIONAL)
    }
}

/// Een kaart zoals `GetMemoryMap` hem schreef.
#[derive(Copy, Clone, Debug, Default)]
pub(crate) struct Map {
    /// Waar hij staat.
    pub(crate) pa: u64,
    /// Hoeveel bytes.
    pub(crate) size: u64,
    /// De stap tussen twee descriptors.
    pub(crate) stride: u64,
}

impl Map {
    /// De descriptors, in kaartvolgorde. Een stap onder de 0x28 bytes van
    /// de struct is een kromme kaart: dan niets.
    pub(crate) fn iter(&self) -> impl Iterator<Item = Desc> + '_ {
        let n = if self.stride >= 0x28 {
            self.size / self.stride
        } else {
            0
        };
        (0..n).map(move |i| {
            let d = self.pa + i * self.stride;
            Desc {
                ty: dev::read32(Pa(d)),
                base: dev::read64(Pa(d + 8)),
                pages: dev::read64(Pa(d + 0x18)),
            }
        })
    }

    /// Het DRAM in bytes: alle RAM-typen samen.
    pub(crate) fn ram_bytes(&self) -> u64 {
        self.iter()
            .filter(Desc::is_ram)
            .fold(0u64, |s, d| s.saturating_add(d.end() - d.base))
    }
}

/// Het vrije DRAM na de exit, samengevoegd waar descriptors aansluiten en
/// naar binnen afgerond op [`GRAIN`]; de grootste `N` blijven over (de pool
/// van `abi::layout` heeft een plafond). Geeft ook het totaal dat daarbij
/// buiten de boot viel, voor de bootlog.
pub(crate) fn free_regions<const N: usize>(map: &Map) -> (BoundedVec<Region, N>, u64) {
    // Eerst alles wat vrij is, gesorteerd op basis: EDK2 levert de kaart
    // gesorteerd, maar de spec eist het niet.
    let mut raw: BoundedVec<(u64, u64), 512> = BoundedVec::new();
    let mut dropped = 0u64;
    for d in map.iter().filter(Desc::is_free) {
        if raw.push((d.base, d.end())).is_err() {
            dropped = dropped.saturating_add(d.end() - d.base);
        }
    }
    raw.sort_unstable_by_key(|r| r.0);
    // Samenvoegen en afronden.
    let mut merged: BoundedVec<(u64, u64), 512> = BoundedVec::new();
    for &(b, e) in raw.iter() {
        match merged.last_mut() {
            Some(last) if last.1 == b => last.1 = e,
            _ => {
                let _ = merged.push((b, e));
            }
        }
    }
    let mut out: BoundedVec<Region, N> = BoundedVec::new();
    for &(b, e) in merged.iter() {
        let lo = b.next_multiple_of(GRAIN);
        let hi = e & !(GRAIN - 1);
        if hi <= lo {
            continue;
        }
        let r = Region {
            base: Pa(lo),
            size: hi - lo,
        };
        if let Err(bounded::Full(r)) = out.push(r) {
            // Vol: de kleinste eruit als deze groter is.
            let small = out
                .iter()
                .enumerate()
                .min_by_key(|(_, x)| x.size)
                .map(|(i, x)| (i, x.size));
            match small {
                Some((i, size)) if size < r.size => {
                    dropped = dropped.saturating_add(size);
                    if let Some(slot) = out.get_mut(i) {
                        *slot = r;
                    }
                }
                _ => dropped = dropped.saturating_add(r.size),
            }
        }
    }
    out.sort_unstable_by_key(|r| r.base.0);
    (out, dropped)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Een kaart in een host-buffer, met de EDK2-stap van 0x30.
    fn map_of(descs: &[(u32, u64, u64)]) -> (Vec<u64>, Map) {
        let mut buf = vec![0u64; descs.len() * 6];
        for (i, &(t, base, pages)) in descs.iter().enumerate() {
            buf[i * 6] = u64::from(t);
            buf[i * 6 + 1] = base;
            buf[i * 6 + 3] = pages;
        }
        let map = Map {
            pa: buf.as_ptr() as usize as u64,
            size: (descs.len() * 0x30) as u64,
            stride: 0x30,
        };
        (buf, map)
    }

    const MB: u64 = 1 << 20;

    #[test]
    fn descriptors_follow_the_firmware_stride() {
        let (_buf, map) = map_of(&[(7, 0x4000_0000, 16), (11, 0x0900_0000, 1)]);
        let d: Vec<Desc> = map.iter().collect();
        assert_eq!(d.len(), 2);
        assert_eq!(d[0].end(), 0x4001_0000);
        assert!(d[0].is_free() && d[0].is_ram());
        assert!(!d[1].is_ram());
        assert_eq!(map.ram_bytes(), 16 * PAGE);
        // Een kromme stap: niets, geen onzin.
        let bad = Map { stride: 8, ..map };
        assert_eq!(bad.iter().count(), 0);
    }

    #[test]
    fn free_memory_is_merged_trimmed_and_capped() {
        let pages = |mb: u64| mb * MB / PAGE;
        let (_buf, map) = map_of(&[
            // 0x4000_0000: 1 MB conventional + 5 MB boot services data,
            // aansluitend: samen 6 MB, afgerond [0x4000_0000, 0x4060_0000).
            (7, 0x4000_0000, pages(1)),
            (4, 0x4010_0000, pages(5)),
            // Onze image: niet vrij.
            (1, 0x4060_0000, pages(2)),
            // Scheef begin: 0x4081_0000 + 7 MB -> [0x40a0_0000, 0x40e0_0000).
            (3, 0x4081_0000, pages(7) - 16),
            // Te klein na afronden.
            (7, 0x5000_1000, 100),
            // Runtime en ACPI blijven van de firmware.
            (6, 0x6000_0000, pages(64)),
            (9, 0x7000_0000, pages(64)),
            // Een grote hoge regio.
            (7, 0x1_0000_0000, pages(1024)),
        ]);
        let (r, dropped) = free_regions::<8>(&map);
        let got: Vec<(u64, u64)> = r.iter().map(|x| (x.base.0, x.size)).collect();
        assert_eq!(
            got,
            [
                (0x4000_0000, 6 * MB),
                (0x40a0_0000, 4 * MB),
                (0x1_0000_0000, 1024 * MB)
            ]
        );
        assert_eq!(dropped, 0);
        // Met plek voor twee blijven de twee grootste over.
        let (r, dropped) = free_regions::<2>(&map);
        let got: Vec<u64> = r.iter().map(|x| x.base.0).collect();
        assert_eq!(got, [0x4000_0000, 0x1_0000_0000]);
        assert_eq!(dropped, 4 * MB);
    }
}
