//! De toetsen van de kaartbouwer: de 8.3-namen, de LFN, de geometrie en de
//! weigeringen. Byte voor byte tegen Go staat in `tests/go.rs`.

use super::*;

fn short(name: &str, seen: &mut HashSet<String>) -> (String, bool) {
    let (s, lfn) = short_name(name, seen).unwrap();
    (String::from_utf8(s.to_vec()).unwrap(), lfn)
}

fn card(size_mb: usize, start: usize) -> Card {
    Card {
        size_mb,
        start,
        label: "boot".to_owned(),
        vol_entry: false,
    }
}

fn file(name: &str, len: usize) -> File {
    File {
        name: name.to_owned(),
        data: vec![0x5A; len],
    }
}

#[test]
fn short_names_like_go() {
    let mut seen = HashSet::new();
    assert_eq!(
        short("config.txt", &mut seen),
        ("CONFIG  TXT".into(), false)
    );
    assert_eq!(short("extlinux", &mut seen), ("EXTLINUX   ".into(), false));
    assert_eq!(
        short("extlinux.conf", &mut seen),
        ("EXTLIN~1CON".into(), true)
    );
    assert_eq!(
        short("bcm2711-rpi-4-b.dtb", &mut seen),
        ("BCM271~1DTB".into(), true)
    );
    assert_eq!(
        short("bcm2712-rpi-5-b.dtb", &mut seen),
        ("BCM271~2DTB".into(), true)
    );
    // Geen extensie en te lang: de extensie wordt `_`, zoals Go.
    assert_eq!(
        short("averylongname", &mut seen),
        ("AVERYL~1_  ".into(), true)
    );
    // Twee punten: de stam tot de laatste punt, zonder de punt.
    assert_eq!(short("a.b.c", &mut seen), ("AB~1    C  ".into(), true));
    assert!(matches!(
        short_name("CONFIG.TXT", &mut seen),
        Err(Error::Collision { .. })
    ));
}

#[test]
fn lfn_of_thirteen_has_no_terminator() {
    let mut tbl = [0u8; 2 * ENTRY];
    write_lfn(&mut tbl, "extlinux.conf", b"EXTLIN~1CON");
    assert_eq!(tbl[0], 0x41);
    assert_eq!(tbl[11], 0x0F);
    // Het laatste teken op het laatste slot, en niets erachter.
    assert_eq!(&tbl[30..32], &[b'f', 0]);
    assert!(tbl[ENTRY..].iter().all(|&b| b == 0));
}

#[test]
fn lfn_pads_with_ffff() {
    let mut tbl = [0u8; 2 * ENTRY];
    write_lfn(&mut tbl, "bcm2711-rpi-4-b.dtb", b"BCM271~1DTB");
    // Twee entries, de laatste (bovenaan) met 0x40.
    assert_eq!(tbl[0], 0x42);
    assert_eq!(tbl[ENTRY], 0x01);
    // Dezelfde som in beide.
    assert_eq!(tbl[13], tbl[ENTRY + 13]);
    // 19 tekens: in de bovenste entry zes tekens, de nul, dan 0xFFFF.
    assert_eq!(&tbl[14..16], &[b'b', 0]);
    assert_eq!(&tbl[16..18], &[0, 0]);
    assert_eq!(&tbl[18..20], &[0xFF, 0xFF]);
}

#[test]
fn geometry_of_64_mb() {
    let g = geometry(64).unwrap();
    assert_eq!((g.secs, g.spf, g.clusters), (131_072, 128, 32_695));
    assert!(matches!(geometry(4), Err(Error::Clusters { .. })));
}

#[test]
fn empty_file_has_no_chain() {
    let img = build(&card(16, 1), &[], vec![file("empty", 0), file("one", 1)]).unwrap();
    let root = SECTOR + (RESERVED + NUM_FATS * geometry(16).unwrap().spf) * SECTOR;
    assert_eq!(&img[root + 26..root + 28], &[0, 0]);
    assert_eq!(&img[root + ENTRY + 26..root + ENTRY + 28], &[2, 0]);
}

#[test]
fn second_fat_is_a_copy() {
    let img = build(&card(16, 1), &[], vec![file("big.bin", 5000)]).unwrap();
    let spf = geometry(16).unwrap().spf * SECTOR;
    let fat0 = SECTOR + RESERVED * SECTOR;
    assert_eq!(img[fat0..fat0 + spf], img[fat0 + spf..fat0 + 2 * spf]);
    // De keten: 2 naar 3 naar 4 naar het einde.
    assert_eq!(&img[fat0 + 4..fat0 + 10], &[3, 0, 4, 0, 0xFF, 0xFF]);
}

#[test]
fn refusals() {
    let blob = |off, len| Blob {
        name: "b".to_owned(),
        off,
        data: vec![1; len],
    };
    assert!(matches!(
        build(&card(16, 8), &[blob(0, 1)], vec![file("f", 1)]),
        Err(Error::RawOverMbr { .. })
    ));
    assert!(matches!(
        build(&card(16, 8), &[blob(512, 4000)], vec![file("f", 1)]),
        Err(Error::RawInPartition { .. })
    ));
    assert!(matches!(
        build(&card(16, 1), &[], vec![file("a//b", 1)]),
        Err(Error::EmptyComponent { .. })
    ));
    assert!(matches!(
        build(&card(16, 1), &[], vec![file("huge", 17 << 20)]),
        Err(Error::Full { .. })
    ));
}
