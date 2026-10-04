//! Host-tests voor de ACPI-lezer. Er is geen Go-testdata voor deze
//! tabellen, dus de tests bouwen RSDP, XSDT en tabellen zelf, in een
//! nep-geheugen achter [`Phys`].

use super::*;

/// Het fysieke adres waar het nep-geheugen begint.
const BASE: u64 = 0x4000_0000;
/// De RSDP ligt op `BASE`, de XSDT hier.
const XSDT_OFF: usize = 0x100;
/// Tabellen beginnen hier.
const TABLES_OFF: usize = 0x1000;

/// Fysiek geheugen als een `Vec` op [`BASE`].
struct Ram {
    bytes: Vec<u8>,
}

impl Phys for Ram {
    fn read(&self, pa: u64, out: &mut [u8]) -> bool {
        let Some(off) = pa.checked_sub(BASE) else {
            return false;
        };
        let off = off as usize;
        match self.bytes.get(off..off + out.len()) {
            Some(s) => {
                out.copy_from_slice(s);
                true
            }
            None => false,
        }
    }
}

impl Ram {
    fn patch(&mut self, off: usize, data: &[u8]) {
        self.bytes[off..off + data.len()].copy_from_slice(data);
    }
}

/// Zet de checksum op byte 9 zodat de tabel op 0 uitkomt.
fn fix(t: &mut [u8]) {
    t[9] = 0;
    t[9] = 0u8.wrapping_sub(checksum(t));
}

/// Een SDT met header en checksum; `body` begint op offset 36.
fn sdt(sig: &Sig, body: &[u8]) -> Vec<u8> {
    let mut t = vec![0u8; HEADER_LEN];
    t[..4].copy_from_slice(sig);
    t[4..8].copy_from_slice(&((HEADER_LEN + body.len()) as u32).to_le_bytes());
    t[8] = 2;
    t[10..16].copy_from_slice(b"HOPOS ");
    t[16..24].copy_from_slice(b"TESTTBL ");
    t.extend_from_slice(body);
    fix(&mut t);
    t
}

/// Een SDT van `len` bytes met velden op absolute offsets.
fn sdt_with(sig: &Sig, len: usize, fields: &[(usize, &[u8])]) -> Vec<u8> {
    let mut t = sdt(sig, &vec![0u8; len - HEADER_LEN]);
    for (off, v) in fields {
        t[*off..*off + v.len()].copy_from_slice(v);
    }
    fix(&mut t);
    t
}

/// Een RSDP van 36 bytes met beide checksums.
fn rsdp(rev: u8, xsdt: u64) -> [u8; 36] {
    let mut b = [0u8; 36];
    b[..8].copy_from_slice(b"RSD PTR ");
    b[9..15].copy_from_slice(b"HOPOS ");
    b[15] = rev;
    b[20..24].copy_from_slice(&36u32.to_le_bytes());
    b[24..32].copy_from_slice(&xsdt.to_le_bytes());
    b[8] = 0u8.wrapping_sub(checksum(&b[..20]));
    b[32] = 0u8.wrapping_sub(checksum(&b));
    b
}

/// Bouwt een machine: tabellen in het geheugen, de XSDT die ze aanwijst
/// en de RSDP ervoor.
struct Machine {
    ram: Vec<u8>,
    entries: Vec<u64>,
    next: usize,
}

impl Machine {
    fn new() -> Self {
        Self {
            ram: vec![0; 0x2_0000],
            entries: Vec::new(),
            next: TABLES_OFF,
        }
    }

    /// Legt `t` in het geheugen zonder XSDT-entry (de DSDT).
    fn place(&mut self, t: &[u8]) -> u64 {
        let off = self.next;
        self.ram[off..off + t.len()].copy_from_slice(t);
        self.next = (off + t.len() + 0xff) & !0xff;
        BASE + off as u64
    }

    /// Legt `t` in het geheugen en zet hem in de XSDT.
    fn table(&mut self, t: &[u8]) -> u64 {
        let pa = self.place(t);
        self.entries.push(pa);
        pa
    }

    /// Een rauwe XSDT-entry, ook een kapotte.
    fn entry(&mut self, pa: u64) {
        self.entries.push(pa);
    }

    fn build(self) -> Ram {
        let body: Vec<u8> = self.entries.iter().flat_map(|e| e.to_le_bytes()).collect();
        let xsdt = sdt(b"XSDT", &body);
        assert!(XSDT_OFF + xsdt.len() <= TABLES_OFF, "too many entries");
        let mut ram = Ram { bytes: self.ram };
        ram.patch(XSDT_OFF, &xsdt);
        ram.patch(0, &rsdp(2, BASE + XSDT_OFF as u64));
        ram
    }
}

fn one(t: &[u8]) -> (Ram, Tables) {
    let mut m = Machine::new();
    m.table(t);
    let ram = m.build();
    let tables = Tables::parse(&ram, BASE).unwrap();
    (ram, tables)
}

#[test]
fn the_guid_is_efi_acpi_20_in_memory_order() {
    // 8868e871-e4f1-11d3-bc22-0080c73c8881: d1, d2, d3 little-endian.
    assert_eq!(ACPI_20_GUID[..4], 0x8868_e871u32.to_le_bytes());
    assert_eq!(ACPI_20_GUID[4..6], 0xe4f1u16.to_le_bytes());
    assert_eq!(ACPI_20_GUID[6..8], 0x11d3u16.to_le_bytes());
    assert_eq!(
        ACPI_20_GUID[8..],
        [0xbc, 0x22, 0x00, 0x80, 0xc7, 0x3c, 0x88, 0x81]
    );
}

#[test]
fn rsdp_errors() {
    let good = Machine::new().build();
    assert!(Tables::parse(&good, BASE).is_ok());
    assert_eq!(Tables::parse(&good, 0).err(), Some(Error::NoRsdp));
    assert_eq!(
        Tables::parse(&good, BASE + 2).err(),
        Some(Error::BadAddress {
            pa: BASE + 2,
            len: 36
        })
    );
    assert!(matches!(
        Tables::parse(&good, 0x800).err(),
        Some(Error::BadAddress { .. })
    ));
    assert!(matches!(
        Tables::parse(&good, 1 << 50).err(),
        Some(Error::BadAddress { .. })
    ));
    // Plausibel maar buiten het geheugen.
    assert!(matches!(
        Tables::parse(&good, 0x1000).err(),
        Some(Error::Unreadable { .. })
    ));

    // Signature.
    let mut ram = Machine::new().build();
    ram.patch(0, b"RSD PTX ");
    assert_eq!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::RsdpSignature(BASE))
    );

    // De 1.0-checksum (eerste 20 bytes).
    let mut ram = Machine::new().build();
    ram.bytes[8] ^= 1;
    assert!(matches!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::RsdpChecksum {
            part: "ACPI 1.0",
            ..
        })
    ));

    // De 2.0-checksum: een byte na 20 kapot, de 1.0-helft heel.
    let mut ram = Machine::new().build();
    ram.bytes[33] ^= 1;
    assert!(matches!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::RsdpChecksum {
            part: "ACPI 2.0",
            ..
        })
    ));

    // Revisie 0 (ACPI 1.0): geen XSDT.
    let mut ram = Machine::new().build();
    ram.patch(0, &rsdp(0, BASE + XSDT_OFF as u64));
    assert_eq!(Tables::parse(&ram, BASE).err(), Some(Error::Revision(0)));
}

#[test]
fn xsdt_errors() {
    // Een onplausibele XSDT-pointer.
    let mut ram = Machine::new().build();
    ram.patch(0, &rsdp(2, 0x10));
    assert!(matches!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::BadAddress { pa: 0x10, .. })
    ));

    // Een verkeerde signature.
    let mut ram = Machine::new().build();
    ram.patch(XSDT_OFF, b"RSDT");
    assert!(matches!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::Signature { got, .. }) if &got == b"RSDT"
    ));

    // Een lengte boven TABLE_MAX.
    let mut ram = Machine::new().build();
    ram.patch(XSDT_OFF + 4, &(5u32 << 20).to_le_bytes());
    assert_eq!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::Length {
            sig: *b"XSDT",
            len: 5 << 20,
            max: TABLE_MAX
        })
    );

    // Een lengte onder de header.
    let mut ram = Machine::new().build();
    ram.patch(XSDT_OFF + 4, &20u32.to_le_bytes());
    assert!(matches!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::Length { len: 20, .. })
    ));

    // De checksum.
    let mut m = Machine::new();
    m.table(&sdt(b"APIC", &[0; 8]));
    let mut ram = m.build();
    ram.bytes[XSDT_OFF + 20] ^= 0x40;
    assert_eq!(
        Tables::parse(&ram, BASE).err(),
        Some(Error::Checksum {
            sig: *b"XSDT",
            pa: BASE + XSDT_OFF as u64
        })
    );
}

#[test]
fn xsdt_walk_skips_broken_entries() {
    let mut m = Machine::new();
    let apic = m.table(&sdt(b"APIC", &[0; 8]));
    m.entry(0); // Nulpagina.
    let ssdt1 = m.table(&sdt(b"SSDT", &[1]));
    m.entry(BASE + 0x1002); // Niet gealigneerd.
    m.entry(1 << 50); // Buiten de PA-ruimte.
    let ssdt2 = m.table(&sdt(b"SSDT", &[2]));
    m.entry(0x10_0000); // Plausibel maar niet leesbaar.
    let facp = m.table(&sdt(b"FACP", &[0; 8]));
    let ram = m.build();

    let t = Tables::parse(&ram, BASE).unwrap();
    assert_eq!(t.revision(), 2);
    assert_eq!(t.oem_id(), "HOPOS");
    assert_eq!(
        t.sigs().collect::<Vec<_>>(),
        [*b"APIC", *b"SSDT", *b"SSDT", *b"FACP"]
    );
    assert_eq!(t.broken(), 4);
    assert_eq!(t.overflow(), 0);
    assert_eq!(t.find(b"APIC"), Some(apic));
    assert_eq!(t.find(b"SSDT"), Some(ssdt1));
    assert_eq!(t.find(b"FACP"), Some(facp));
    assert_eq!(t.find(b"MCFG"), None);
    assert_eq!(t.all(b"SSDT").collect::<Vec<_>>(), [ssdt1, ssdt2]);
}

#[test]
fn xsdt_overflow_is_counted() {
    let mut m = Machine::new();
    let pa = m.table(&sdt(b"SSDT", &[]));
    for _ in 1..MAX_TABLES + 6 {
        m.entry(pa);
    }
    let t = Tables::parse(&m.build(), BASE).unwrap();
    assert_eq!(t.sigs().count(), MAX_TABLES);
    assert_eq!(t.overflow(), 6);
}

#[test]
fn oem_id_that_is_not_utf8_is_empty() {
    let mut m = Machine::new();
    m.table(&sdt(b"APIC", &[]));
    let mut ram = m.build();
    ram.patch(XSDT_OFF + 10, &[0xff, 0xfe, 0, 0, 0, 0]);
    let mut x = ram.bytes[XSDT_OFF..XSDT_OFF + 44].to_vec();
    fix(&mut x);
    ram.patch(XSDT_OFF, &x);
    assert_eq!(Tables::parse(&ram, BASE).unwrap().oem_id(), "");
}

#[test]
fn load_checks_length_and_checksum() {
    let mut m = Machine::new();
    let good = m.table(&sdt(b"MCFG", &[0; 28]));
    let bad = m.table(&sdt(b"SPCR", &[0; 44]));
    let mut ram = m.build();
    ram.bytes[(bad - BASE) as usize + 50] ^= 1;
    let t = Tables::parse(&ram, BASE).unwrap();
    let mut buf = [0u8; 256];

    assert_eq!(t.load(&ram, b"MCFG", &mut buf).unwrap().len(), 64);
    assert_eq!(
        t.load(&ram, b"SPCR", &mut buf).err(),
        Some(Error::Checksum {
            sig: *b"SPCR",
            pa: bad
        })
    );
    assert_eq!(
        t.load(&ram, b"GTDT", &mut buf).err(),
        Some(Error::Missing(*b"GTDT"))
    );
    // Een buffer die te klein is, is een lengtefout, geen afgekapte tabel.
    let mut small = [0u8; 40];
    assert_eq!(
        t.load(&ram, b"MCFG", &mut small).err(),
        Some(Error::Length {
            sig: *b"MCFG",
            len: 64,
            max: 40
        })
    );
    // load_at zonder signature-eis, en met een onplausibel adres.
    assert_eq!(load_at(&ram, good, &mut buf).unwrap()[..4], *b"MCFG");
    assert!(matches!(
        load_at(&ram, 3, &mut buf).err(),
        Some(Error::BadAddress { pa: 3, .. })
    ));
}

/// Een GICC-entry van 80 bytes (ACPI 6.x).
fn gicc(uid: u32, mpidr: u64, enabled: bool, gicr: u64, class: u8) -> Vec<u8> {
    let mut e = vec![0u8; 80];
    e[0] = 0x0b;
    e[1] = 80;
    e[4..8].copy_from_slice(&uid.to_le_bytes());
    e[8..12].copy_from_slice(&uid.to_le_bytes());
    e[12..16].copy_from_slice(&u32::from(enabled).to_le_bytes());
    e[60..68].copy_from_slice(&gicr.to_le_bytes());
    e[68..76].copy_from_slice(&mpidr.to_le_bytes());
    e[76] = class;
    e
}

fn gicd(base: u64, version: u8) -> Vec<u8> {
    let mut e = vec![0u8; 24];
    e[0] = 0x0c;
    e[1] = 24;
    e[8..16].copy_from_slice(&base.to_le_bytes());
    e[20] = version;
    e
}

fn gicr(base: u64, len: u32) -> Vec<u8> {
    let mut e = vec![0u8; 16];
    e[0] = 0x0e;
    e[1] = 16;
    e[4..12].copy_from_slice(&base.to_le_bytes());
    e[12..16].copy_from_slice(&len.to_le_bytes());
    e
}

fn its(base: u64) -> Vec<u8> {
    let mut e = vec![0u8; 20];
    e[0] = 0x0f;
    e[1] = 20;
    e[8..16].copy_from_slice(&base.to_le_bytes());
    e
}

fn madt(entries: &[Vec<u8>]) -> Vec<u8> {
    let mut body = vec![0u8; 8]; // LocalInterruptControllerAddress, flags.
    for e in entries {
        body.extend_from_slice(e);
    }
    sdt(b"APIC", &body)
}

#[test]
fn madt_cpus_and_gic() {
    let (ram, t) = one(&madt(&[
        gicd(0x0800_0000, 3),
        gicc(0, 0x000, true, 0x080a_0000, 0),
        gicc(1, 0x100, true, 0x080c_0000, 1),
        gicc(2, 0x200, false, 0, 1),
        gicc(3, 0x300, true, 0x0810_0000, 2),
        gicr(0x080a_0000, 0x00f6_0000),
        its(0x0808_0000),
    ]));
    let mut buf = [0u8; 1024];
    let table = t.load(&ram, b"APIC", &mut buf).unwrap();
    let m = Madt::new(table).unwrap();

    let cpus: Vec<_> = m.cpus().collect();
    assert_eq!(cpus.len(), 4);
    assert_eq!(
        cpus[1],
        Gicc {
            mpidr: 0x100,
            enabled: true,
            gicr: 0x080c_0000,
            eff_class: 1
        }
    );
    assert!(!cpus[2].enabled);
    assert_eq!(cpus.iter().filter(|c| c.enabled).count(), 3);
    assert_eq!(cpus[3].eff_class, 2);
    assert_eq!(m.gicd(), Some((0x0800_0000, 3)));
    assert_eq!(
        m.gicr_ranges().collect::<Vec<_>>(),
        [(0x080a_0000, 0x00f6_0000)]
    );
    assert_eq!(m.its().collect::<Vec<_>>(), [0x0808_0000]);
    assert_eq!(m.its_ids().collect::<Vec<_>>(), [(0, 0x0808_0000)]);
}

#[test]
fn madt_short_entries() {
    // Een GICC van 76 bytes heeft geen eff_class; een GICD van 16 bytes
    // geen versie.
    let mut short = gicc(7, 0x700, true, 0, 9);
    short.truncate(76);
    short[1] = 76;
    let mut d = gicd(0x0800_0000, 3);
    d.truncate(16);
    d[1] = 16;
    let m_bytes = madt(&[short, d]);
    let m = Madt::new(&m_bytes).unwrap();
    assert_eq!(m.cpus().next().unwrap().eff_class, 0);
    assert_eq!(m.gicd(), Some((0x0800_0000, 0)));
}

#[test]
fn a_broken_madt_entry_ends_the_walk() {
    let mut zero = vec![0u8; 4];
    zero[0] = 0x0b; // Lengte 0: zou anders eeuwig op dezelfde plek blijven.
    let bytes = madt(&[
        gicc(0, 0, true, 0, 0),
        zero,
        gicc(1, 0x100, true, 0, 0),
        gicd(0x0800_0000, 3),
    ]);
    let m = Madt::new(&bytes).unwrap();
    assert_eq!(m.cpus().count(), 1);
    assert_eq!(m.gicd(), None);

    // Een entry die voorbij het einde loopt.
    let mut long = gicc(0, 0, true, 0, 0);
    long[1] = 200;
    let bytes = madt(&[gicc(5, 0x500, true, 0, 0), long]);
    let m = Madt::new(&bytes).unwrap();
    assert_eq!(m.cpus().map(|c| c.mpidr).collect::<Vec<_>>(), [0x500]);
}

#[test]
fn madt_refuses_the_wrong_table() {
    assert_eq!(
        Madt::new(&sdt(b"MCFG", &[0; 8])).err(),
        Some(Error::WrongTable {
            want: *b"APIC",
            got: *b"MCFG"
        })
    );
    assert_eq!(
        Madt::new(&sdt(b"APIC", &[0; 4])).err(),
        Some(Error::TooShort {
            sig: *b"APIC",
            len: 40,
            need: 44
        })
    );
    assert!(Madt::new(&[]).is_err());
    // Een gedeclareerde lengte voorbij de slice.
    let t = madt(&[gicc(0, 0, true, 0, 0)]);
    assert!(matches!(
        Madt::new(&t[..100]).err(),
        Some(Error::Length { max: 100, .. })
    ));
}

#[test]
fn mcfg_two_segments() {
    let mut body = vec![0u8; 8]; // Reserved.
    for (base, seg, start, end) in [
        (0x4010_0000_0000u64, 0u16, 0u8, 0xffu8),
        (0x3000_0000_0000, 1, 0x10, 0x7f),
    ] {
        body.extend_from_slice(&base.to_le_bytes());
        body.extend_from_slice(&seg.to_le_bytes());
        body.extend_from_slice(&[start, end, 0, 0, 0, 0]);
    }
    let t = sdt(b"MCFG", &body);
    let ecams: Vec<_> = mcfg(&t).unwrap().collect();
    assert_eq!(
        ecams,
        [
            Ecam {
                base: 0x4010_0000_0000,
                segment: 0,
                start_bus: 0,
                end_bus: 0xff
            },
            Ecam {
                base: 0x3000_0000_0000,
                segment: 1,
                start_bus: 0x10,
                end_bus: 0x7f
            },
        ]
    );
    assert!(mcfg(&sdt(b"APIC", &[0; 8])).is_err());
}

/// Een SPCR van 80 bytes (revisie 2).
fn spcr_table(if_type: u8, access: u8, base: u64, baud: u8) -> Vec<u8> {
    sdt_with(
        b"SPCR",
        80,
        &[
            (36, &[if_type]),
            (40, &[0, 32, 0, access]),
            (44, &base.to_le_bytes()),
            (58, &[baud]),
        ],
    )
}

#[test]
fn spcr_pl011_on_qemu() {
    let c = spcr(&spcr_table(0x03, 1, 0x0900_0000, 3)).unwrap();
    assert_eq!(
        c,
        Console {
            base: 0x0900_0000,
            if_type: 3,
            space: 0,
            shift: 0,
        }
    );
    assert!(!is_16550(c.if_type));
}

#[test]
fn spcr_16550_with_32_bit_stride_on_the_o6n() {
    // De O6N: een DesignWare 8250 met dword-toegang, dus shift 2.
    let c = spcr(&spcr_table(0x12, 3, 0x040d_0000, 7)).unwrap();
    assert_eq!(c.shift, 2);
    assert!(is_16550(c.if_type));
    // Access 0 (niet gezegd), en de SBSA-UART is geen 16550.
    let c = spcr(&spcr_table(0x0e, 0, 0x100, 0)).unwrap();
    assert_eq!(c.shift, 0);
    assert!(!is_16550(c.if_type));
    assert!(is_16550(0x00) && is_16550(0x01) && is_16550(0x02));
    assert!(!is_16550(0x0d));
    // Te kort.
    assert!(matches!(
        spcr(&sdt(b"SPCR", &[0; 10])).err(),
        Some(Error::TooShort { need: 52, .. })
    ));
}

#[test]
fn fadt_psci_conduits() {
    let smc = sdt_with(b"FACP", 276, &[(129, &1u16.to_le_bytes())]);
    assert_eq!(fadt_psci(&smc).unwrap(), Psci { hvc: false });
    let hvc = sdt_with(b"FACP", 276, &[(129, &3u16.to_le_bytes())]);
    assert_eq!(fadt_psci(&hvc).unwrap(), Psci { hvc: true });
    assert_eq!(
        fadt_psci(&sdt_with(b"FACP", 130, &[])).err(),
        Some(Error::TooShort {
            sig: *b"FACP",
            len: 130,
            need: 131
        })
    );
}

/// Een SBSA-watchdog-structuur (type 1, 28 bytes).
fn wdog(refresh: u64, control: u64, gsiv: u32, flags: u32) -> Vec<u8> {
    let mut e = vec![0u8; 28];
    e[0] = 1;
    e[1..3].copy_from_slice(&28u16.to_le_bytes());
    e[4..12].copy_from_slice(&refresh.to_le_bytes());
    e[12..20].copy_from_slice(&control.to_le_bytes());
    e[20..24].copy_from_slice(&gsiv.to_le_bytes());
    e[24..28].copy_from_slice(&flags.to_le_bytes());
    e
}

fn gtdt(count: u32, structs: &[Vec<u8>]) -> Vec<u8> {
    let mut body = vec![0u8; 96 - HEADER_LEN];
    body[88 - HEADER_LEN..92 - HEADER_LEN].copy_from_slice(&count.to_le_bytes());
    body[92 - HEADER_LEN..96 - HEADER_LEN].copy_from_slice(&96u32.to_le_bytes());
    for s in structs {
        body.extend_from_slice(s);
    }
    sdt(b"GTDT", &body)
}

#[test]
fn gtdt_skips_the_secure_watchdog() {
    let t = gtdt(
        2,
        &[
            wdog(0x1000_0000, 0x1001_0000, 60, 4),
            wdog(0x2000_0000, 0x2001_0000, 61, 0),
        ],
    );
    assert_eq!(
        gtdt_watchdog(&t),
        Some(Watchdog {
            refresh: 0x2000_0000,
            control: 0x2001_0000,
        })
    );
    // Alleen een secure watchdog: geen.
    assert_eq!(gtdt_watchdog(&gtdt(1, &[wdog(0x1000, 0x2000, 1, 4)])), None);
    // QEMU virt: geen platform-timers.
    assert_eq!(gtdt_watchdog(&gtdt(0, &[])), None);
}

#[test]
fn gtdt_broken_structures_end_the_walk() {
    // Een absurde telling met één structuur: de lus eindigt aan het einde.
    assert_eq!(gtdt_watchdog(&gtdt(u32::MAX, &[wdog(1, 2, 3, 4)])), None);
    // Lengte 0: geen eeuwige lus.
    let mut zero = wdog(1, 2, 3, 0);
    zero[1..3].copy_from_slice(&0u16.to_le_bytes());
    assert_eq!(gtdt_watchdog(&gtdt(u32::MAX, &[zero])), None);
    // Een structuur die voorbij het einde loopt.
    let mut long = wdog(1, 2, 3, 0);
    long[1..3].copy_from_slice(&200u16.to_le_bytes());
    assert_eq!(gtdt_watchdog(&gtdt(1, &[long])), None);
    // Een offset voorbij het einde, en een te korte tabel.
    let mut far = gtdt(1, &[wdog(1, 2, 3, 0)]);
    far[92..96].copy_from_slice(&0xffff_fff0u32.to_le_bytes());
    fix(&mut far);
    assert_eq!(gtdt_watchdog(&far), None);
    assert_eq!(gtdt_watchdog(&sdt(b"GTDT", &[0; 20])), None);
}

#[test]
fn dsdt_via_x_dsdt() {
    let dsdt = sdt(b"DSDT", b"\x10\x20AML!");
    let mut m = Machine::new();
    let pa = m.place(&dsdt);
    // Een 32-bit-pointer die nergens heen wijst: X_DSDT wint.
    m.table(&sdt_with(
        b"FACP",
        276,
        &[
            (40, &0xdead_0000u32.to_le_bytes()),
            (140, &pa.to_le_bytes()),
        ],
    ));
    let ram = m.build();
    let t = Tables::parse(&ram, BASE).unwrap();
    let mut buf = [0u8; 512];
    assert_eq!(t.load_dsdt(&ram, &mut buf).unwrap(), dsdt.as_slice());
}

#[test]
fn dsdt_via_the_32_bit_pointer() {
    let dsdt = sdt(b"DSDT", b"AML");
    let mut buf = [0u8; 512];

    // X_DSDT is 0 in een lange FADT.
    let mut m = Machine::new();
    let pa = m.place(&dsdt);
    m.table(&sdt_with(b"FACP", 276, &[(40, &(pa as u32).to_le_bytes())]));
    let ram = m.build();
    let t = Tables::parse(&ram, BASE).unwrap();
    assert_eq!(t.load_dsdt(&ram, &mut buf).unwrap(), dsdt.as_slice());

    // Een FADT korter dan 148: X_DSDT bestaat niet, ook al staat er iets.
    let mut m = Machine::new();
    let pa = m.place(&dsdt);
    m.table(&sdt_with(b"FACP", 116, &[(40, &(pa as u32).to_le_bytes())]));
    let ram = m.build();
    let t = Tables::parse(&ram, BASE).unwrap();
    assert_eq!(t.load_dsdt(&ram, &mut buf).unwrap(), dsdt.as_slice());
}

#[test]
fn dsdt_errors() {
    let mut buf = [0u8; 512];
    // Geen FADT.
    let (ram, t) = one(&sdt(b"APIC", &[0; 8]));
    assert_eq!(
        t.load_dsdt(&ram, &mut buf).err(),
        Some(Error::Missing(*b"FACP"))
    );
    // Beide pointers 0.
    let (ram, t) = one(&sdt_with(b"FACP", 276, &[]));
    assert_eq!(
        t.load_dsdt(&ram, &mut buf).err(),
        Some(Error::Missing(*b"DSDT"))
    );
    // De pointer wijst naar een andere tabel.
    let mut m = Machine::new();
    let pa = m.place(&sdt(b"SSDT", &[]));
    m.table(&sdt_with(b"FACP", 276, &[(140, &pa.to_le_bytes())]));
    let ram = m.build();
    let t = Tables::parse(&ram, BASE).unwrap();
    assert!(matches!(
        t.load_dsdt(&ram, &mut buf).err(),
        Some(Error::Signature { got, .. }) if &got == b"SSDT"
    ));
}

#[test]
fn errors_display_their_numbers() {
    let e = Error::Length {
        sig: *b"XSDT",
        len: 5 << 20,
        max: TABLE_MAX,
    };
    assert_eq!(
        e.to_string(),
        "acpi: XSDT length 5242880 outside 36..=4194304"
    );
    let e = Error::Signature {
        pa: 0x1000,
        want: *b"DSDT",
        got: [0, b'S', b'D', 0xff],
    };
    assert_eq!(
        e.to_string(),
        "acpi: DSDT signature missing at 0x1000 (found ?SD?)"
    );
}

/// Een IORT met een ITS-groep, een SMMUv3 en twee root-complexen: segment
/// 0 direct naar de ITS met basis 0x1_0000, segment 1 door de SMMU.
fn iort() -> Vec<u8> {
    let mut b = vec![0u8; 48 - 36];
    let node = |typ: u8, len: u16, nmap: u32, map_off: u32| {
        let mut n = vec![0u8; usize::from(len)];
        n[0] = typ;
        n[1..3].copy_from_slice(&len.to_le_bytes());
        n[8..12].copy_from_slice(&nmap.to_le_bytes());
        n[12..16].copy_from_slice(&map_off.to_le_bytes());
        n
    };
    let map = |base: u32, span: u32, out: u32, to: u32, flags: u32| {
        [base, span, out, to, flags]
            .iter()
            .flat_map(|v| v.to_le_bytes())
            .collect::<Vec<u8>>()
    };
    b[0..4].copy_from_slice(&4u32.to_le_bytes()); // vier knopen
    b[4..8].copy_from_slice(&48u32.to_le_bytes());
    // 48: de ITS-groep met één ITS, GIC ITS ID 7.
    let mut group = node(0, 24, 0, 0);
    group[16..20].copy_from_slice(&1u32.to_le_bytes());
    group[20..24].copy_from_slice(&7u32.to_le_bytes());
    b.extend(group);
    // 72: SMMUv3 op 0x4000_0000, eigen MSI (single) plus 0..0xffff naar de
    // ITS op 0x2_0000.
    let mut smmu = node(4, 24 + 40, 2, 24);
    smmu[16..24].copy_from_slice(&0x4000_0000u64.to_le_bytes());
    smmu[24..44].copy_from_slice(&map(0, 0, 0x99, 48, 1));
    smmu[44..64].copy_from_slice(&map(0, 0xffff, 0x2_0000, 48, 0));
    b.extend(smmu);
    // 136: root-complex segment 0.
    let mut rc0 = node(2, 36 + 20, 1, 36);
    rc0[28..32].copy_from_slice(&0u32.to_le_bytes());
    rc0[36..56].copy_from_slice(&map(0, 0xffff, 0x1_0000, 48, 0));
    b.extend(rc0);
    // 192: root-complex segment 1, door de SMMU.
    let mut rc1 = node(2, 36 + 20, 1, 36);
    rc1[28..32].copy_from_slice(&1u32.to_le_bytes());
    rc1[36..56].copy_from_slice(&map(0, 0xffff, 0x100, 72, 0));
    b.extend(rc1);
    sdt(b"IORT", &b)
}

#[test]
fn iort_maps_a_requester_id_to_its_device_id() {
    let t = iort();
    assert_eq!(iort_device_id(&t, 0, 0x3100), Some(0x1_3100));
    // Door de SMMU: RC 0x10 -> SMMU 0x110 -> ITS 0x2_0110.
    assert_eq!(iort_device_id(&t, 1, 0x10), Some(0x2_0110));
    assert_eq!(iort_device_id(&t, 2, 0x10), None);
    // Een kromme tabel is `None`, geen panic.
    assert_eq!(iort_device_id(&t[..60], 0, 0), None);
    // De weg zelf: de SMMU onderweg en de ITS die de groep noemt.
    let r = iort_route(&t, 1, 0x10).unwrap();
    assert_eq!(r.smmu, Some((4, 0x4000_0000)));
    assert_eq!(r.its, (1, Some(7)));
    assert_eq!(iort_route(&t, 0, 0x10).unwrap().smmu, None);
}

/// Eén type-`typ`-subkanaal van 62 bytes met herkenbare velden.
fn subspace(typ: u8, shmem: u64, db: u64, preserve: u64, write: u64, lat: u32) -> Vec<u8> {
    let mut e = vec![0u8; 62];
    e[0] = typ;
    e[1] = 62;
    e[8..16].copy_from_slice(&shmem.to_le_bytes());
    e[16..24].copy_from_slice(&0x100u64.to_le_bytes());
    e[25] = 32;
    e[28..36].copy_from_slice(&db.to_le_bytes());
    e[36..44].copy_from_slice(&preserve.to_le_bytes());
    e[44..52].copy_from_slice(&write.to_le_bytes());
    e[52..56].copy_from_slice(&lat.to_le_bytes());
    e
}

/// `pcct_test.go`: de ordinale nummering, de offsets uit de spec, en
/// nette afwijzing van ontbrekende indexen en extended types.
#[test]
fn pcct_subspaces_are_counted_in_order() {
    let mut t = vec![0u8; 48];
    t.extend(subspace(2, 0x8860_0000, 0x1000_0054_0010, !1, 1, 500));
    t.extend(subspace(1, 0x8860_1000, 0x1000_0054_0020, 0, 0x53, 100));
    t.extend(subspace(3, 0xdead, 0xbeef, 0, 0, 0));
    let p = pcct_subspace(&t, 0).unwrap();
    assert_eq!(
        p,
        Pcc {
            shmem: 0x8860_0000,
            shmem_len: 0x100,
            doorbell: 0x1000_0054_0010,
            doorbell_width: 32,
            preserve: !1,
            write: 1,
            latency_us: 500,
        }
    );
    let p = pcct_subspace(&t, 1).unwrap();
    assert_eq!((p.shmem, p.write), (0x8860_1000, 0x53));
    assert_eq!(pcct_subspace(&t, 2), None, "extended type");
    assert_eq!(pcct_subspace(&t, 9), None);
    assert_eq!(pcct_subspace(&[], 0), None);
    let mut broken = vec![0u8; 48];
    broken.extend([2, 200]);
    assert_eq!(pcct_subspace(&broken, 0), None);
}
