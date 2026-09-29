//! De firmware-kant van de boot: de UEFI-diensten die de stub gebruikt
//! zolang de boot services leven, en niets daarna.
//!
//! Een [`Efi`] bestaat alleen tussen de ingang en `ExitBootServices`: de
//! exit neemt hem als waarde ([`Efi::exit_boot_services`]), dus na de exit
//! is er geen handvat meer om de firmware mee aan te roepen. Dat is de
//! typestate van de regel "na de exit geen boot services".
//!
//! De offsets zijn die van de UEFI-spec 2.x, 64-bit, en die van de Go-stub
//! (`OLD/metal/board/uefi/init_body.h`), waar ze op QEMU/EDK2, de Altra en
//! de O6N bewezen zijn: SystemTable ConOut 0x40, BootServices 0x60,
//! NumberOfTableEntries 0x68, ConfigurationTable 0x70 (24 bytes per
//! ingang); BootServices AllocatePages 0x28, GetMemoryMap 0x38,
//! HandleProtocol 0x98, ExitBootServices 0xe8, SetWatchdogTimer 0x100.
//!
//! Wat de stub bewust NIET doet (Go-lessen, 13-07): het EFI_RNG_PROTOCOL
//! aanroepen (EDK2 zonder werkende TRNG pollt daar eeuwig), en iets
//! printen tussen de laatste `GetMemoryMap` en `ExitBootServices` (een
//! print mag geheugen alloceren en maakt de MapKey dan ongeldig).

use dev::Pa;

/// Een EFI_STATUS.
pub(crate) type Status = usize;

/// EFI_SUCCESS.
pub(crate) const SUCCESS: Status = 0;
/// Het foutbit van een EFI_STATUS.
const ERROR_BIT: usize = 1 << 63;
/// EFI_LOAD_ERROR, voor wat de stub zelf weigert.
pub(crate) const LOAD_ERROR: Status = ERROR_BIT | 1;
/// EFI_OUT_OF_RESOURCES.
pub(crate) const OUT_OF_RESOURCES: Status = ERROR_BIT | 9;
/// EFI_NOT_FOUND.
pub(crate) const NOT_FOUND: Status = ERROR_BIT | 14;

/// Het geheugentype waarmee de stub alloceert: EfiLoaderData. Het blijft
/// na de exit van ons en telt niet mee in de pool van de slots.
const LOADER_DATA: u32 = 2;

/// AllocatePages: een willekeurige plek.
const ALLOCATE_ANY: u32 = 0;
/// AllocatePages: onder een maximumadres.
const ALLOCATE_MAX: u32 = 1;
/// AllocatePages: precies hier.
const ALLOCATE_ADDRESS: u32 = 2;

/// De offsets in de firmware-tabellen.
mod off {
    pub(super) const ST_CON_OUT: u64 = 0x40;
    pub(super) const ST_BOOT: u64 = 0x60;
    pub(super) const ST_NTABLES: u64 = 0x68;
    pub(super) const ST_TABLES: u64 = 0x70;
    pub(super) const BS_ALLOCATE_PAGES: u64 = 0x28;
    pub(super) const BS_GET_MEMORY_MAP: u64 = 0x38;
    pub(super) const BS_HANDLE_PROTOCOL: u64 = 0x98;
    pub(super) const BS_EXIT_BOOT_SERVICES: u64 = 0xe8;
    pub(super) const BS_SET_WATCHDOG: u64 = 0x100;
    pub(super) const BS_LOCATE_PROTOCOL: u64 = 0x140;
    pub(super) const OUT_STRING: u64 = 0x08;
    pub(super) const LI_DEVICE: u64 = 0x18;
    pub(super) const LI_IMAGE_BASE: u64 = 0x40;
    pub(super) const LI_IMAGE_SIZE: u64 = 0x48;
    pub(super) const SFS_OPEN_VOLUME: u64 = 0x08;
    pub(super) const FILE_OPEN: u64 = 0x08;
    pub(super) const FILE_CLOSE: u64 = 0x10;
    pub(super) const FILE_READ: u64 = 0x20;
    pub(super) const FILE_GET_POSITION: u64 = 0x30;
    pub(super) const FILE_SET_POSITION: u64 = 0x38;
}

/// EFI_LOADED_IMAGE_PROTOCOL_GUID, in geheugenvolgorde.
const LOADED_IMAGE_GUID: [u8; 16] = [
    0xa1, 0x31, 0x1b, 0x5b, 0x62, 0x95, 0xd2, 0x11, 0x8e, 0x3f, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b,
];
/// EFI_SIMPLE_FILE_SYSTEM_PROTOCOL_GUID, in geheugenvolgorde.
const SIMPLE_FS_GUID: [u8; 16] = [
    0x22, 0x5b, 0x4e, 0x96, 0x59, 0x64, 0xd2, 0x11, 0x8e, 0x39, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b,
];

type OutputString = extern "efiapi" fn(this: u64, s: *const u16) -> Status;
type AllocatePages = extern "efiapi" fn(ty: u32, mem: u32, pages: usize, addr: *mut u64) -> Status;
type GetMemoryMap = extern "efiapi" fn(
    size: *mut usize,
    map: *mut u8,
    key: *mut usize,
    desc_size: *mut usize,
    desc_ver: *mut u32,
) -> Status;
type ExitBootServices = extern "efiapi" fn(image: u64, key: usize) -> Status;
type HandleProtocol = extern "efiapi" fn(handle: u64, guid: *const u8, iface: *mut u64) -> Status;
type SetWatchdog =
    extern "efiapi" fn(secs: usize, code: u64, len: usize, data: *const u16) -> Status;
type OpenVolume = extern "efiapi" fn(this: u64, root: *mut u64) -> Status;
type FileOpen =
    extern "efiapi" fn(this: u64, new: *mut u64, name: *const u16, mode: u64, attrs: u64) -> Status;
type FileClose = extern "efiapi" fn(this: u64) -> Status;
type FileRead = extern "efiapi" fn(this: u64, size: *mut usize, buf: *mut u8) -> Status;
type FileGetPosition = extern "efiapi" fn(this: u64, pos: *mut u64) -> Status;
type FileSetPosition = extern "efiapi" fn(this: u64, pos: u64) -> Status;

/// Wat `GetMemoryMap` teruggaf.
#[derive(Copy, Clone, Debug)]
pub(crate) struct MapInfo {
    /// De bytes die de kaart beslaat.
    pub(crate) size: usize,
    /// De sleutel voor `ExitBootServices`.
    pub(crate) key: usize,
    /// De stap tussen twee descriptors: altijd deze gebruiken, nooit
    /// `sizeof` (EDK2: 0x30, de struct is 0x28).
    pub(crate) desc_size: usize,
}

/// Een gelezen bestand van de ESP, in door de stub gealloceerd geheugen.
#[derive(Copy, Clone, Debug)]
pub(crate) struct File {
    /// Waar de bytes staan (EfiLoaderData).
    pub(crate) pa: u64,
    /// Hoeveel.
    pub(crate) len: u64,
}

/// De firmware zolang de boot services leven.
///
/// # Invariants
///
/// `st` is de EFI_SYSTEM_TABLE die de firmware aan de ingang gaf, `bs` zijn
/// BootServices, en `ExitBootServices` is nog niet geslaagd.
pub(crate) struct Efi {
    image: u64,
    st: u64,
    bs: u64,
}

impl Efi {
    /// De firmware zoals de ingang hem kreeg.
    ///
    /// # Safety
    ///
    /// `image` en `st` zijn x0 en x1 van de UEFI-ingang, de boot services
    /// leven nog, en er bestaat geen tweede `Efi`.
    pub(crate) unsafe fn new(image: u64, st: u64) -> Self {
        let bs = dev::read64(Pa(st + off::ST_BOOT));
        // INVARIANT: de voorwaarde van deze functie.
        Self { image, st, bs }
    }

    /// Het functieadres op `off` in de tabel op `table`.
    fn func(table: u64, off: u64) -> u64 {
        dev::read64(Pa(table + off))
    }

    /// De SystemTable, voor de console-haak.
    pub(crate) fn system_table(&self) -> u64 {
        self.st
    }

    /// Zet de watchdog van de BDS uit (die wapent vijf minuten vóór
    /// StartImage). `ExitBootServices` ontwapent hem ook, maar een stub die
    /// voor de exit blijft hangen, moet zichtbaar blijven hangen.
    pub(crate) fn watchdog_off(&self) {
        let f = Self::func(self.bs, off::BS_SET_WATCHDOG);
        // SAFETY: de invariant van `Efi`; argumenten volgens de spec.
        let _ = unsafe { core::mem::transmute::<u64, SetWatchdog>(f) }(0, 0, 0, core::ptr::null());
    }

    /// `pages` pagina's EfiLoaderData onder `max` (inclusief), of overal als
    /// `max` 0 is.
    pub(crate) fn allocate(&self, pages: u64, max: u64) -> Result<u64, Status> {
        let f = Self::func(self.bs, off::BS_ALLOCATE_PAGES);
        let mut addr = max;
        let ty = if max == 0 { ALLOCATE_ANY } else { ALLOCATE_MAX };
        let pages = usize::try_from(pages).map_err(|_| OUT_OF_RESOURCES)?;
        // SAFETY: de invariant van `Efi`; `addr` leeft over de call.
        let st = unsafe { core::mem::transmute::<u64, AllocatePages>(f) }(
            ty,
            LOADER_DATA,
            pages,
            &mut addr,
        );
        if st != SUCCESS {
            return Err(st);
        }
        Ok(addr)
    }

    /// `pages` pagina's EfiLoaderData precies op `addr`: de vraag "is dit
    /// venster vrij op dít board?".
    pub(crate) fn allocate_at(&self, addr: u64, pages: u64) -> Result<(), Status> {
        let f = Self::func(self.bs, off::BS_ALLOCATE_PAGES);
        let mut at = addr;
        let pages = usize::try_from(pages).map_err(|_| OUT_OF_RESOURCES)?;
        // SAFETY: de invariant van `Efi`; `at` leeft over de call.
        let st = unsafe { core::mem::transmute::<u64, AllocatePages>(f) }(
            ALLOCATE_ADDRESS,
            LOADER_DATA,
            pages,
            &mut at,
        );
        if st != SUCCESS {
            return Err(st);
        }
        Ok(())
    }

    /// De memory map in `buf` (`cap` bytes, door de stub gealloceerd).
    pub(crate) fn memory_map(&self, buf: u64, cap: usize) -> Result<MapInfo, Status> {
        let f = Self::func(self.bs, off::BS_GET_MEMORY_MAP);
        let (mut size, mut key, mut desc_size, mut ver) = (cap, 0usize, 0usize, 0u32);
        // SAFETY: de invariant van `Efi`; `buf` is `cap` bytes van ons, de
        // uit-parameters leven over de call.
        let st = unsafe { core::mem::transmute::<u64, GetMemoryMap>(f) }(
            &mut size,
            buf as usize as *mut u8,
            &mut key,
            &mut desc_size,
            &mut ver,
        );
        if st != SUCCESS {
            return Err(st);
        }
        Ok(MapInfo {
            size,
            key,
            desc_size,
        })
    }

    /// `ExitBootServices` met een verse kaart in `buf`, tot acht keer (de
    /// MapKey verloopt bij elke allocatie ertussen; het recept van de spec).
    /// Geeft de laatste kaart; bij falen de firmware terug met de status.
    pub(crate) fn exit_boot_services(
        self,
        buf: u64,
        cap: usize,
    ) -> Result<MapInfo, (Self, Status)> {
        let f = Self::func(self.bs, off::BS_EXIT_BOOT_SERVICES);
        let mut last = LOAD_ERROR;
        for _ in 0..8 {
            let map = match self.memory_map(buf, cap) {
                Ok(m) => m,
                Err(e) => return Err((self, e)),
            };
            // SAFETY: de invariant van `Efi`; na succes gebruikt niemand
            // `self` meer (hij gaat hier op).
            last = unsafe { core::mem::transmute::<u64, ExitBootServices>(f) }(self.image, map.key);
            if last == SUCCESS {
                return Ok(map);
            }
        }
        Err((self, last))
    }

    /// Het adres van de configuratietabel met `guid` (de RSDP: de ACPI
    /// 2.0-GUID).
    pub(crate) fn config_table(&self, guid: &[u8; 16]) -> Option<u64> {
        let n = dev::read64(Pa(self.st + off::ST_NTABLES));
        let tables = dev::read64(Pa(self.st + off::ST_TABLES));
        (0..n.min(256)).find_map(|i| {
            let e = tables + i * 24;
            let mut g = [0u8; 16];
            dev::copy_out(&mut g, Pa(e));
            (g == *guid).then(|| dev::read64(Pa(e + 16)))
        })
    }

    /// Een protocol op `handle`.
    fn protocol(&self, handle: u64, guid: &[u8; 16]) -> Option<u64> {
        let f = Self::func(self.bs, off::BS_HANDLE_PROTOCOL);
        let mut iface = 0u64;
        // SAFETY: de invariant van `Efi`; `guid` en `iface` leven over de
        // call.
        let st = unsafe { core::mem::transmute::<u64, HandleProtocol>(f) }(
            handle,
            guid.as_ptr(),
            &mut iface,
        );
        (st == SUCCESS && iface != 0).then_some(iface)
    }

    /// De eerste instantie van een protocol, ongeacht het handvat
    /// (`LocateProtocol`): de GOP van de console (gop.rs).
    #[cfg_attr(not(feature = "gui"), expect(dead_code))]
    pub(crate) fn locate_protocol(&self, guid: &[u8; 16]) -> Option<u64> {
        type LocateProtocol =
            extern "efiapi" fn(guid: *const u8, registration: u64, iface: *mut u64) -> Status;
        let f = Self::func(self.bs, off::BS_LOCATE_PROTOCOL);
        let mut iface = 0u64;
        // SAFETY: de invariant van `Efi`; `guid` en `iface` leven over de
        // call, en een registratie van nul is "geen" volgens de spec.
        let st =
            unsafe { core::mem::transmute::<u64, LocateProtocol>(f) }(guid.as_ptr(), 0, &mut iface);
        (st == SUCCESS && iface != 0).then_some(iface)
    }

    /// Waar de firmware ons laadde en hoe groot: `(basis, bytes)`.
    pub(crate) fn image(&self) -> Option<(u64, u64)> {
        let li = self.protocol(self.image, &LOADED_IMAGE_GUID)?;
        Some((
            dev::read64(Pa(li + off::LI_IMAGE_BASE)),
            dev::read64(Pa(li + off::LI_IMAGE_SIZE)),
        ))
    }

    /// Leest bestand `name` (UCS-2, NUL-afgesloten) van het volume waar
    /// deze PE vandaan kwam, in nieuw gealloceerde pagina's; hoogstens
    /// `max` bytes, naar `dst` als die er is (van de aanroeper, minstens
    /// `max` bytes). `None` als er geen bestand is of lezen faalt.
    ///
    /// De firmware leest zijn eigen FAT (SimpleFileSystem, dezelfde weg
    /// waarlangs deze PE geladen is); HopOS heeft geen FS-driver en neemt
    /// alleen de bytes over. Het cmdline.txt-model van de Pi (Go, 17-07).
    pub(crate) fn read_file(&self, name: &[u16], max: u64, dst: Option<u64>) -> Option<File> {
        let li = self.protocol(self.image, &LOADED_IMAGE_GUID)?;
        let device = dev::read64(Pa(li + off::LI_DEVICE));
        let fs = self.protocol(device, &SIMPLE_FS_GUID)?;
        let mut root = 0u64;
        let open_volume = Self::func(fs, off::SFS_OPEN_VOLUME);
        // SAFETY: de invariant van `Efi`; `fs` is een SimpleFileSystem van
        // de firmware en `root` leeft over de call.
        let st = unsafe { core::mem::transmute::<u64, OpenVolume>(open_volume) }(fs, &mut root);
        if st != SUCCESS || root == 0 {
            return None;
        }
        let got = self.read_from(root, name, max, dst);
        Self::close(root);
        got
    }

    /// Het leeswerk van [`read_file`](Self::read_file) onder een open root.
    fn read_from(&self, root: u64, name: &[u16], max: u64, dst: Option<u64>) -> Option<File> {
        if name.last() != Some(&0) {
            return None;
        }
        let mut file = 0u64;
        let open = Self::func(root, off::FILE_OPEN);
        // SAFETY: de invariant van `Efi`; `name` is NUL-afgesloten (net
        // getoetst), mode 1 = EFI_FILE_MODE_READ.
        let st = unsafe { core::mem::transmute::<u64, FileOpen>(open) }(
            root,
            &mut file,
            name.as_ptr(),
            1,
            0,
        );
        if st != SUCCESS || file == 0 {
            return None;
        }
        let got = self.read_all(file, max, dst);
        Self::close(file);
        got
    }

    /// De grootte via SetPosition(einde) en GetPosition, dan alles lezen.
    fn read_all(&self, file: u64, max: u64, dst: Option<u64>) -> Option<File> {
        let set = Self::func(file, off::FILE_SET_POSITION);
        let get = Self::func(file, off::FILE_GET_POSITION);
        let read = Self::func(file, off::FILE_READ);
        let mut size = 0u64;
        // SAFETY: de invariant van `Efi`; `file` is een open EFI_FILE en
        // de uit-parameters leven over de calls. Positie u64::MAX = einde.
        unsafe {
            if core::mem::transmute::<u64, FileSetPosition>(set)(file, u64::MAX) != SUCCESS
                || core::mem::transmute::<u64, FileGetPosition>(get)(file, &mut size) != SUCCESS
                || core::mem::transmute::<u64, FileSetPosition>(set)(file, 0) != SUCCESS
            {
                return None;
            }
        }
        if size == 0 || size > max {
            return None;
        }
        let pa = match dst {
            Some(d) => d,
            None => self.allocate(size.div_ceil(4096), 0).ok()?,
        };
        let mut len = usize::try_from(size).ok()?;
        // SAFETY: de invariant van `Efi`; `[pa, pa + size)` is net voor ons
        // gealloceerd, of is `dst` van de aanroeper (minstens `max` bytes).
        let st = unsafe { core::mem::transmute::<u64, FileRead>(read) }(
            file,
            &mut len,
            pa as usize as *mut u8,
        );
        (st == SUCCESS).then_some(File {
            pa,
            len: len as u64,
        })
    }

    /// Sluit een EFI_FILE; fouten slikken we.
    fn close(file: u64) {
        let f = Self::func(file, off::FILE_CLOSE);
        // SAFETY: `file` is een open EFI_FILE van de firmware.
        let _ = unsafe { core::mem::transmute::<u64, FileClose>(f) }(file);
    }
}

/// Schrijft `s` op de firmware-console (ConOut van de SystemTable `st`):
/// UCS-2, `\n` wordt `\r\n`, in stukken van 124 tekens. Fouten slikken
/// we: een headless firmware bestaat, en de echte console komt straks uit
/// de SPCR.
///
/// # Safety
///
/// `st` is de SystemTable van de UEFI-ingang en de boot services leven
/// nog.
pub(crate) unsafe fn con_out(st: u64, s: &str) {
    let out = dev::read64(Pa(st + off::ST_CON_OUT));
    if out == 0 {
        return;
    }
    let f = Efi::func(out, off::OUT_STRING);
    let mut buf = [0u16; 128];
    let mut n = 0;
    let flush = |buf: &mut [u16; 128], n: &mut usize| {
        if let Some(end) = buf.get_mut(*n) {
            *end = 0;
        }
        // SAFETY: de voorwaarde van deze functie: ConOut leeft, `f` is zijn
        // OutputString, en `buf` is NUL-afgesloten UCS-2 dat de call
        // overleeft.
        let _ = unsafe { core::mem::transmute::<u64, OutputString>(f) }(out, buf.as_ptr());
        *n = 0;
    };
    for b in s.bytes() {
        if n >= 124 {
            flush(&mut buf, &mut n);
        }
        if b == b'\n' {
            if let Some(c) = buf.get_mut(n) {
                *c = u16::from(b'\r');
            }
            n += 1;
        }
        if let Some(c) = buf.get_mut(n) {
            *c = if b.is_ascii() {
                u16::from(b)
            } else {
                u16::from(b'?')
            };
        }
        n += 1;
    }
    if n > 0 {
        flush(&mut buf, &mut n);
    }
}

/// Een ASCII-naam als UCS-2 met NUL, voor [`Efi::read_file`].
pub(crate) const fn ucs2<const N: usize>(s: &[u8; N]) -> [u16; N] {
    let mut out = [0u16; N];
    let mut i = 0;
    while i < N {
        out[i] = s[i] as u16;
        i += 1;
    }
    out
}
