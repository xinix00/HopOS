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

    /// Het RAM als aaneengesloten stukken: `f` krijgt elk stuk waarin
    /// aansluitende of overlappende RAM-descriptors samen zijn gevoegd, in
    /// kaartvolgorde. Allemaal dezelfde bits in de map, dus alleen een echte
    /// grens tussen RAM en geen-RAM kost de identity map een L3-tabel (de
    /// Altra: duizenden descriptors om en om, Go 14-07).
    pub(crate) fn ram_runs<E>(
        &self,
        mut f: impl FnMut(u64, u64) -> Result<(), E>,
    ) -> Result<(), E> {
        let mut run: Option<(u64, u64)> = None;
        for d in self.iter().filter(Desc::is_ram) {
            match run.as_mut() {
                Some((b, e)) if (*b..=*e).contains(&d.base) => *e = (*e).max(d.end()),
                _ => {
                    if let Some((b, e)) = run.replace((d.base, d.end())) {
                        f(b, e - b)?;
                    }
                }
            }
        }
        match run {
            Some((b, e)) => f(b, e - b),
            None => Ok(()),
        }
    }

    /// Valt `[base, base + size)` helemaal in EfiConventionalMemory? Dat
    /// vraagt `AllocatePages` op een vast adres; de stub toetst er de
    /// vensterkandidaten van Go mee als zijn eigen venster bezet is.
    pub(crate) fn is_conventional(&self, base: u64, size: u64) -> bool {
        let end = base.saturating_add(size);
        let mut at = base;
        // Elke ronde schuift `at` naar het eind van een descriptor die hem
        // dekt, dus hoogstens één ronde per descriptor.
        for _ in 0..=self.iter().count() {
            if at >= end {
                return true;
            }
            match self
                .iter()
                .find(|d| d.ty == ty::CONVENTIONAL && d.base <= at && at < d.end())
            {
                Some(d) => at = d.end(),
                None => return false,
            }
        }
        at >= end
    }
}

/// Hoeveel vrije stukken de pool onderweg bijhoudt. Na het samenvoegen
/// zijn het er een handvol per bank, ook bij duizenden descriptors; elk
/// stuk dat de firmware als niet-vrij uitknipt, kost er één extra.
const SPANS: usize = 512;

/// Wat buiten de pool viel: stukken en bytes, voor de bootregel.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub(crate) struct Dropped {
    /// Hoeveel stukken.
    pub(crate) count: u64,
    /// Hoeveel bytes samen.
    pub(crate) bytes: u64,
}

impl Dropped {
    fn add(&mut self, bytes: u64) {
        self.count = self.count.saturating_add(1);
        self.bytes = self.bytes.saturating_add(bytes);
    }
}

/// Het vrije DRAM na de exit voor de pool, in de volgorde van Go's
/// `usablePool` (plan.go):
///
/// 1. alle vrije descriptors, samengevoegd tijdens het lezen (de Altra
///    levert er duizenden om en om, 14-07: wie eerst verzamelt en dan pas
///    samenvoegt, loopt vol);
/// 2. alles wat ergens als niet-vrij geboekt staat eruit: de O6N-firmware
///    levert overlappende descriptors (FreeBSD-meting jan. 2025), en een
///    vrije die een gereserveerde overlapt hoort niet in de pool;
/// 3. naar binnen afronden op [`GRAIN`], en de grootste `N` houden (de
///    pool van `abi::layout` heeft een plafond).
///
/// Wat onderweg wegvalt (een volle lijst, het plafond), telt [`Dropped`];
/// het afronden telt niet mee.
pub(crate) fn free_regions<const N: usize>(map: &Map) -> (BoundedVec<Region, N>, Dropped) {
    let mut lost = Dropped::default();
    let mut spans = free_spans(map, &mut lost);
    for d in map.iter().filter(|d| !d.is_free()) {
        cut(&mut spans, d.base, d.end(), &mut lost);
    }
    let mut out: BoundedVec<Region, N> = BoundedVec::new();
    for s in spans.iter() {
        let lo = s.base.next_multiple_of(GRAIN);
        let hi = (s.base + s.size) & !(GRAIN - 1);
        if hi <= lo {
            continue;
        }
        keep_largest(
            &mut out,
            Region {
                base: Pa(lo),
                size: hi - lo,
            },
            &mut lost,
        );
    }
    out.sort_unstable_by_key(|r| r.base.0);
    (out, lost)
}

/// Stap 1: de vrije descriptors, samengevoegd met elk stuk dat ze raken
/// (van achteren: een gesorteerde kaart raakt het laatste), en daarna
/// gesorteerd en nog eens samengevoegd (`abi::layout::coalesce`) voor een
/// kaart die de spec niet gesorteerd hoeft te leveren.
fn free_spans(map: &Map, lost: &mut Dropped) -> BoundedVec<abi::Region, SPANS> {
    let mut spans: BoundedVec<abi::Region, SPANS> = BoundedVec::new();
    for d in map.iter().filter(Desc::is_free) {
        let (b, e) = (d.base, d.end());
        if e <= b {
            continue;
        }
        let hit = spans
            .as_mut_slice()
            .iter_mut()
            .rev()
            .find(|s| b <= s.base + s.size && s.base <= e);
        match hit {
            Some(s) => {
                let end = (s.base + s.size).max(e);
                s.base = s.base.min(b);
                s.size = end - s.base;
            }
            None => {
                if spans.push(abi::Region::new(b, e - b)).is_err() {
                    lost.add(e - b);
                }
            }
        }
    }
    // Geen omloop: `Desc::end` verzadigt, dus base + size past altijd.
    let n = abi::layout::coalesce(spans.as_mut_slice()).unwrap_or(spans.len());
    spans.truncate(n);
    spans
}

/// Stap 2: knipt `[b, e)` uit de stukken (Go's `subtract`). Het linkerdeel
/// blijft op zijn plek, een rechterdeel komt achteraan; past dat niet meer,
/// dan telt het als verloren.
fn cut(spans: &mut BoundedVec<abi::Region, SPANS>, b: u64, e: u64, lost: &mut Dropped) {
    if e <= b {
        return;
    }
    let mut i = 0;
    while let Some(&s) = spans.get(i) {
        let s_end = s.base + s.size;
        if e <= s.base || b >= s_end {
            i += 1;
            continue;
        }
        let left = (b > s.base).then(|| abi::Region::new(s.base, b - s.base));
        let right = (e < s_end).then(|| abi::Region::new(e, s_end - e));
        let (keep, more) = match (left, right) {
            (Some(l), r) => (l, r),
            (None, Some(r)) => (r, None),
            (None, None) => {
                // Helemaal weg; de laatste schuift op `i` en komt nu aan bod.
                spans.swap_remove(i);
                continue;
            }
        };
        if let Some(slot) = spans.get_mut(i) {
            *slot = keep;
        }
        if let Some(r) = more
            && spans.push(r).is_err()
        {
            lost.add(r.size);
        }
        i += 1;
    }
}

/// Stap 3: `r` erbij; is de lijst vol, dan gaat de kleinste eruit als `r`
/// groter is.
fn keep_largest<const N: usize>(out: &mut BoundedVec<Region, N>, r: Region, lost: &mut Dropped) {
    let Err(bounded::Full(r)) = out.push(r) else {
        return;
    };
    let small = out
        .iter()
        .enumerate()
        .min_by_key(|(_, x)| x.size)
        .map(|(i, x)| (i, x.size));
    match small {
        Some((i, size)) if size < r.size => {
            lost.add(size);
            if let Some(slot) = out.get_mut(i) {
                *slot = r;
            }
        }
        _ => lost.add(r.size),
    }
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
        assert_eq!(dropped, Dropped::default());
        // Met plek voor twee blijven de twee grootste over.
        let (r, dropped) = free_regions::<2>(&map);
        let got: Vec<u64> = r.iter().map(|x| x.base.0).collect();
        assert_eq!(got, [0x4000_0000, 0x1_0000_0000]);
        assert_eq!(
            dropped,
            Dropped {
                count: 1,
                bytes: 4 * MB
            }
        );
    }

    /// De Altra (Go 14-07): duizenden vrije descriptors om en om, meer dan
    /// de lijst onderweg draagt. Samenvoegen tijdens het lezen maakt er één
    /// stuk van, zonder verlies; ook een ongesorteerde kaart.
    #[test]
    fn thousands_of_striped_descriptors_become_one_region() {
        let base = 0x80_0000_0000u64;
        let mut descs: Vec<(u32, u64, u64)> = (0..3000u64)
            .map(|i| {
                let t = [7, 4, 3][(i % 3) as usize];
                (t, base + i * MB, MB / PAGE)
            })
            .collect();
        let (_buf, map) = map_of(&descs);
        let (r, dropped) = free_regions::<4>(&map);
        let got: Vec<(u64, u64)> = r.iter().map(|x| (x.base.0, x.size)).collect();
        assert_eq!(got, [(base, 3000 * MB)]);
        assert_eq!(dropped, Dropped::default());
        // Omgekeerd en door elkaar: hetzelfde antwoord.
        descs.reverse();
        descs.swap(10, 2000);
        let (_buf, map) = map_of(&descs);
        let (r, dropped) = free_regions::<4>(&map);
        let got: Vec<(u64, u64)> = r.iter().map(|x| (x.base.0, x.size)).collect();
        assert_eq!(got, [(base, 3000 * MB)]);
        assert_eq!(dropped, Dropped::default());
    }

    /// De O6N (Go plan.go): een gereserveerde descriptor binnen een vrije,
    /// en een vrije die een andere vrije overlapt. Het gereserveerde stuk
    /// gaat eruit, het dubbele telt één keer.
    #[test]
    fn reserved_and_overlapping_descriptors_stay_out_of_the_pool() {
        let pages = |mb: u64| mb * MB / PAGE;
        let (_buf, map) = map_of(&[
            (7, 0x8000_0000, pages(64)),
            // Gereserveerd midden in de vrije: 0x8100_0000 + 4 MB.
            (0, 0x8100_0000, pages(4)),
            // Een vrije die over de eerste heen valt.
            (4, 0x8200_0000, pages(16)),
            // Runtime die over de staart van de vrije valt.
            (6, 0x83e0_0000, pages(2)),
        ]);
        let (r, dropped) = free_regions::<8>(&map);
        let got: Vec<(u64, u64)> = r.iter().map(|x| (x.base.0, x.size)).collect();
        assert_eq!(
            got,
            [(0x8000_0000, 16 * MB), (0x8140_0000, 42 * MB)],
            "{got:x?}"
        );
        assert_eq!(dropped, Dropped::default());
    }

    /// Meer losse stukken dan de lijst draagt: wat niet past, telt als
    /// verloren met stukken en bytes (de bootregel HOPOS_UEFI_MAP_DROPPED).
    #[test]
    fn what_does_not_fit_is_counted() {
        let n = SPANS as u64 + 10;
        let descs: Vec<(u32, u64, u64)> = (0..n)
            .flat_map(|i| {
                let b = 0x1_0000_0000 + i * 8 * MB;
                [(7, b, 4 * MB / PAGE), (0, b + 4 * MB, 4 * MB / PAGE)]
            })
            .collect();
        let (_buf, map) = map_of(&descs);
        let (r, dropped) = free_regions::<64>(&map);
        assert_eq!(r.len(), 64);
        assert_eq!(
            dropped,
            Dropped {
                count: n - 64,
                bytes: (n - 64) * 4 * MB
            }
        );
    }

    #[test]
    fn ram_runs_merge_what_touches() {
        let pages = |mb: u64| mb * MB / PAGE;
        let (_buf, map) = map_of(&[
            (7, 0x8000_0000, pages(1)),
            (4, 0x8010_0000, pages(1)),
            (6, 0x8020_0000, pages(1)),
            // Een gat van 1 MB, dan MMIO (geen RAM), dan weer RAM.
            (11, 0x8040_0000, pages(1)),
            (7, 0x8050_0000, pages(2)),
            (2, 0x8070_0000, pages(1)),
        ]);
        let mut runs = Vec::new();
        map.ram_runs(|b, size| {
            runs.push((b, size));
            Ok::<(), ()>(())
        })
        .unwrap();
        assert_eq!(runs, [(0x8000_0000, 3 * MB), (0x8050_0000, 3 * MB)]);
    }

    #[test]
    fn a_window_fits_only_in_conventional_memory() {
        let pages = |mb: u64| mb * MB / PAGE;
        let (_buf, map) = map_of(&[
            (7, 0x8000_0000, pages(256)),
            (7, 0x9000_0000, pages(256)),
            (4, 0xa000_0000, pages(256)),
        ]);
        // Over twee aansluitende conventionele descriptors heen.
        assert!(map.is_conventional(0x8800_0000, 320 * MB));
        // De staart valt in BootServicesData: AllocatePages weigert.
        assert!(!map.is_conventional(0x9800_0000, 320 * MB));
        // Buiten de kaart.
        assert!(!map.is_conventional(0x5000_0000, MB));
    }
}
