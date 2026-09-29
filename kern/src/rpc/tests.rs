//! De mount-resolutie, de padtoetsen en de hopfs-actor op een nep-schijf.

use super::*;
use crate::hopfs::Fs;
use crate::slots::tests::{FakeConsole, Obey, actor, start};
use abi::hopabi::STATUS_ERROR;
use std::vec;
use std::vec::Vec;

fn m(local: &str, shared: &str) -> Mount {
    Mount {
        local: local.as_bytes().to_vec(),
        shared: shared.as_bytes().to_vec(),
    }
}

fn s(i: usize) -> Slot {
    Slot::new(i).unwrap()
}

fn clean(p: &str) -> Result<Vec<u8>> {
    let mut b = PathBuf::new();
    clean_abs(p.as_bytes(), &mut b)?;
    Ok(b.as_bytes().to_vec())
}

fn res(slot: usize, t: &[Mount], p: &str) -> Result<(Vec<u8>, Option<usize>)> {
    let mut b = PathBuf::new();
    let v = resolve(s(slot), t, p.as_bytes(), &mut b)?;
    Ok((b.as_bytes().to_vec(), v))
}

#[test]
fn clean_abs_normalizes_and_refuses_dot_dot() {
    assert_eq!(clean("").unwrap(), b"/");
    assert_eq!(clean("/").unwrap(), b"/");
    assert_eq!(clean("a//b/./c/").unwrap(), b"/a/b/c");
    assert_eq!(clean("hallo.txt").unwrap(), b"/hallo.txt");
    assert_eq!(clean("../x"), Err(Error::Denied));
    assert_eq!(clean("/a/../../etc"), Err(Error::Denied));
    let long = "a/".repeat(MAX_PATH);
    assert!(matches!(clean(&long), Err(Error::TooLarge { .. })));
}

#[test]
fn own_root_per_slot() {
    assert_eq!(
        res(2, &[], "hallo.txt").unwrap(),
        (b"/.tasks/slot2/hallo.txt".to_vec(), None)
    );
    assert_eq!(res(2, &[], "/").unwrap(), (b"/.tasks/slot2".to_vec(), None));
    assert_eq!(res(128, &[], "/x").unwrap().0, b"/.tasks/slot128/x");
    // Een app kan niet naar de root van een ander: `..` is een weigering,
    // en een absoluut pad valt altijd onder de eigen root.
    assert_eq!(res(2, &[], "/../slot3/x"), Err(Error::Denied));
    assert_eq!(
        res(2, &[], "/.tasks/slot3/x").unwrap().0,
        b"/.tasks/slot2/.tasks/slot3/x"
    );
}

#[test]
fn mounts_resolve_longest_prefix_first() {
    let t = mount_table(&[
        m("/data", "/volumes/data"),
        m("/data/logs", "/volumes/logs"),
    ])
    .unwrap();
    assert_eq!(t[0].local, b"/data/logs", "longest local first");
    assert_eq!(
        res(1, &t, "/data/logs/a").unwrap(),
        (b"/volumes/logs/a".to_vec(), Some(0))
    );
    assert_eq!(
        res(1, &t, "/data/x").unwrap(),
        (b"/volumes/data/x".to_vec(), Some(1))
    );
    assert_eq!(
        res(1, &t, "/data").unwrap(),
        (b"/volumes/data".to_vec(), Some(1))
    );
    // Een prefix op een grens: /database is niet /data.
    assert_eq!(
        res(1, &t, "/database").unwrap(),
        (b"/.tasks/slot1/database".to_vec(), None)
    );
    // Hop: /hop is een volume, de rest zijn eigen root.
    let hop = mount_table(&[m("/hop/", "/volumes/hop")]).unwrap();
    assert_eq!(
        res(1, &hop, "/hop/agent-state.json").unwrap().0,
        b"/volumes/hop/agent-state.json"
    );
}

#[test]
fn mount_table_refuses_escapes() {
    assert_eq!(
        mount_table(&[m("/", "/x")]),
        Err(Error::Denied),
        "overmount /"
    );
    assert_eq!(
        mount_table(&[m("/a", "/")]),
        Err(Error::Denied),
        "share everything"
    );
    assert_eq!(
        mount_table(&[m("/a", "/.tasks/slot3")]),
        Err(Error::Denied),
        "another root"
    );
    assert_eq!(mount_table(&[m("/a", "/.tasks")]), Err(Error::Denied));
    assert_eq!(mount_table(&[m("/a/../b", "/x")]), Err(Error::Denied));
    assert_eq!(
        mount_table(&[m("/a", "/x"), m("a/", "/y")]),
        Err(Error::BadPath),
        "duplicate local"
    );
    // `/.tasksx` is geen taakroot.
    assert!(mount_table(&[m("/a", "/.tasksx")]).is_ok());
}

// ---------------------------------------------------------------------------
// De actor op een schijf in RAM.
// ---------------------------------------------------------------------------

/// Een nep-`BlockDevice`: een schijf in RAM die flushes telt.
pub(crate) struct Ram {
    data: Vec<u8>,
    flushes: usize,
}

impl BlockDevice for Ram {
    fn read(&mut self, lba: u64, buf: &mut [u8]) -> blkdev::Result {
        let o = lba as usize * 512;
        buf.copy_from_slice(
            self.data
                .get(o..o + buf.len())
                .ok_or(blkdev::Error::Io { lba })?,
        );
        Ok(())
    }
    fn write(&mut self, lba: u64, buf: &[u8]) -> blkdev::Result {
        let o = lba as usize * 512;
        self.data
            .get_mut(o..o + buf.len())
            .ok_or(blkdev::Error::Io { lba })?
            .copy_from_slice(buf);
        Ok(())
    }
    fn flush(&mut self) -> blkdev::Result {
        self.flushes += 1;
        Ok(())
    }
}

pub(crate) fn disk(mib: usize) -> (Fs<Ram>, crate::hopfs::Mounted) {
    let r = Ram {
        data: vec![0; mib << 20],
        flushes: 0,
    };
    let sectors = (mib << 20) as u64 / 512;
    Fs::mount(r, 0, sectors, 512, 1 << 20, false).unwrap()
}

/// Een call zoals een verbinding hem stuurt: pad en data in de callbuffer.
fn fs_call(
    slot: usize,
    generation: u32,
    op: u8,
    path: &str,
    off: u64,
    n: u64,
    data: &[u8],
) -> FsCall {
    let mut buf = vec![0u8; REQ_HEADER];
    buf.extend_from_slice(path.as_bytes());
    let p = REQ_HEADER..buf.len();
    buf.extend_from_slice(data);
    FsCall {
        slot: s(slot),
        generation,
        op,
        off,
        n,
        data: p.end..buf.len(),
        path: p,
        buf,
        out: vec![0u8; REQ_HEADER + (64 << 10)],
    }
}

fn data(c: &FsCall, n: usize) -> &[u8] {
    &c.out[REQ_HEADER..REQ_HEADER + n]
}

#[test]
fn actor_serves_the_file_calls_in_the_own_root() {
    let svc = Servicers::new();
    let con = FakeConsole::default();
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    start(&mut a, 2, 8, 1).unwrap();
    let g = svc.current(s(2)).unwrap();
    let (fs, _) = disk(64);
    let mut f = FsActor::new(fs, &svc, &con);

    let mut c = fs_call(2, g, OP_STAT, "hallo.txt", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Err(Error::NoEnt));
    let mut c = fs_call(2, g, OP_WRITE, "hallo.txt", 0, 0, b"hallo wereld");
    assert_eq!(f.handle(&mut c), Ok((12, 0)));
    let mut c = fs_call(2, g, OP_STAT, "/hallo.txt", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Ok((12, 0)));
    let mut c = fs_call(2, g, OP_READ, "hallo.txt", 6, 100, &[]);
    assert_eq!(f.handle(&mut c), Ok((6, 6)));
    assert_eq!(data(&c, 6), b"wereld");
    let mut c = fs_call(2, g, OP_WRITE, "sub/b", 0, 0, b"x");
    f.handle(&mut c).unwrap();
    let mut c = fs_call(2, g, OP_LIST, "/", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Ok((2, 14)));
    assert_eq!(data(&c, 14), b"hallo.txt\nsub/");
    let mut c = fs_call(2, g, OP_TRUNCATE, "hallo.txt", 0, 5, &[]);
    assert_eq!(f.handle(&mut c), Ok((5, 0)));
    let mut c = fs_call(2, g, OP_READ, "hallo.txt", 0, 100, &[]);
    assert_eq!(f.handle(&mut c), Ok((5, 5)));
    let mut c = fs_call(2, g, OP_REMOVE, "sub", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Err(Error::NotEmpty));
    let mut c = fs_call(2, g, OP_REMOVE, "hallo.txt", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Ok((0, 0)));
    let mut c = fs_call(2, g, OP_REMOVE, "/", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Err(Error::Denied), "the own root stays");
    let mut c = fs_call(2, g, OP_READ, "../slot1/x", 0, 1, &[]);
    assert_eq!(f.handle(&mut c), Err(Error::Denied));
    // Alles stond in de eigen root.
    assert_eq!(f.fs.stat(b"/.tasks/slot2/sub/b").unwrap(), (1, false));
}

#[test]
fn a_new_lifetime_starts_with_an_empty_root_and_keeps_its_volume() {
    let svc = Servicers::new();
    let con = FakeConsole::default();
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    crate::slots::tests::start_with_mounts(&mut a, 1, 8, 1, vec![m("/hop", "/volumes/hop")])
        .unwrap();
    let g = svc.current(s(1)).unwrap();
    let (fs, _) = disk(64);
    let mut f = FsActor::new(fs, &svc, &con);
    for (p, d) in [("/hop/agent-state.json", &b"{}"[..]), ("scratch", b"weg")] {
        let mut c = fs_call(1, g, OP_WRITE, p, 0, 0, d);
        f.handle(&mut c).unwrap();
    }
    assert!(con.saw("hopfs: slot 1 saved /hop/agent-state.json as /volumes/hop/agent-state.json"));
    assert_eq!(
        f.fs.stat(b"/volumes/hop/agent-state.json").unwrap(),
        (2, false)
    );
    let mut c = fs_call(1, g, OP_REMOVE, "/hop", 0, 0, &[]);
    assert_eq!(
        f.handle(&mut c),
        Err(Error::Denied),
        "the volume itself stays"
    );
    // Een oude generatie krijgt niets meer.
    let mut c = fs_call(1, g.wrapping_sub(1), OP_STAT, "scratch", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Err(Error::Denied));
    // Stop en opnieuw: de root is leeg, het volume niet.
    crate::slots::tests::stop(&mut a, 1).unwrap();
    crate::slots::tests::start_with_mounts(&mut a, 1, 8, 1, vec![m("/hop", "/volumes/hop")])
        .unwrap();
    let g2 = svc.current(s(1)).unwrap();
    assert_ne!(g, g2);
    let mut c = fs_call(1, g2, OP_STAT, "scratch", 0, 0, &[]);
    assert_eq!(f.handle(&mut c), Err(Error::NoEnt), "root wiped");
    let mut c = fs_call(1, g2, OP_READ, "hop/agent-state.json", 0, 64, &[]);
    assert_eq!(f.handle(&mut c), Ok((2, 2)));
    assert_eq!(data(&c, 2), b"{}");
}

#[test]
fn commit_logs_once_per_generation_and_survives_a_remount() {
    let svc = Servicers::new();
    let con = FakeConsole::default();
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    start(&mut a, 2, 8, 1).unwrap();
    let g = svc.current(s(2)).unwrap();
    let (fs, _) = disk(64);
    let mut f = FsActor::new(fs, &svc, &con);
    let mut c = fs_call(2, g, OP_WRITE, "blijft", 0, 0, b"data");
    f.handle(&mut c).unwrap();
    f.commit(CommitWhy::Stopped(s(2)));
    f.commit(CommitWhy::Periodic); // Niets veranderd: geen tweede regel.
    assert!(con.saw("hopfs: tree committed as generation 1 (slot 2 stopped) HOPOS_FS_COMMIT"));
    assert!(!con.saw("(every 10 s)"));
    let disk = f.fs.into_disk();
    let (mut g2, m) = Fs::mount(disk, 0, (64 << 20) / 512, 512, 1 << 20, false).unwrap();
    assert!(matches!(m, crate::hopfs::Mounted::Restored { .. }));
    assert_eq!(g2.stat(b"/.tasks/slot2/blijft").unwrap(), (4, false));
}

#[test]
fn list_resp_wire_limit() {
    // `TestListRespWireLimit`: precies op de grens past, één byte erover is
    // een nette fout en geen half antwoord.
    let (mut fs, _) = disk(64);
    let limit = 64usize;
    fs.write_at(&[b"/d/".as_slice(), &[b'x'; 64]].concat(), 0, b"1")
        .unwrap();
    let mut dst = vec![0u8; limit];
    assert_eq!(fs.list_into(b"/d", &mut dst).unwrap(), (1, 64));
    fs.write_at(b"/d/y", 0, b"1").unwrap();
    assert!(matches!(
        fs.list_into(b"/d", &mut dst),
        Err(Error::TooLarge { .. })
    ));
    let _ = STATUS_ERROR;
}

#[test]
fn unknown_and_store_ops_are_not_file_calls() {
    for op in [OP_STAT, OP_READ, OP_WRITE, OP_LIST, OP_REMOVE, OP_TRUNCATE] {
        assert!(is_fs_op(op));
    }
    for op in [0, 6, 8, 9, 10, 11, 14, 0x40] {
        assert!(!is_fs_op(op), "op {op}");
    }
}

/// De kern-flip: de bevriezing legt eerst vast (de nieuwe kern mount die
/// generatie) en zegt welke generatie het werd; zonder verandering blijft
/// de generatie staan.
#[test]
fn freeze_commits_first_and_names_the_generation() {
    let svc = Servicers::new();
    let con = FakeConsole::default();
    let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
    start(&mut a, 2, 8, 1).unwrap();
    let g = svc.current(s(2)).unwrap();
    let (fs, _) = disk(64);
    let mut f = FsActor::new(fs, &svc, &con);
    let mut c = fs_call(2, g, OP_WRITE, "voor-de-flip", 0, 0, b"staat");
    f.handle(&mut c).unwrap();
    assert_eq!(f.freeze(), Ok((1, 0)));
    assert!(f.frozen);
    assert!(con.saw("frozen for the kernel flip HOPOS_FS_FROZEN generation=1"));
    assert_eq!(
        f.freeze(),
        Ok((1, 0)),
        "nothing changed: the same generation"
    );
    let disk = f.fs.into_disk();
    let (mut g2, m) = Fs::mount(disk, 0, (64 << 20) / 512, 512, 1 << 20, false).unwrap();
    assert!(matches!(
        m,
        crate::hopfs::Mounted::Restored { generation: 1, .. }
    ));
    assert_eq!(g2.stat(b"/.tasks/slot2/voor-de-flip").unwrap(), (5, false));
}
