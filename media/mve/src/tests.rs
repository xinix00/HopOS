//! De host-tests, geport uit `mve_test.go`, `queue_test.go` en
//! `session_test.go`. De driver werkt overal met fysieke adressen; op de
//! ontwikkelmachine is dat een stuk heap, en dat is precies genoeg om de
//! tabellen, de indexen en het ringprotocol te bewijzen. Wat een test hier
//! NIET bewijst is de barrière-plaatsing en het cachegedrag: dat blijft het
//! board.

use super::*;
use crate::fwbin::{HEADER_LEN, TEXT_BASE};
use crate::hwreg::{
    ALLOC_NON_PROTECTED, CTRL_MAX_CORES_SHIFT, JOB_SLOTS, LSID_BASE, LSID_STRIDE, job_slot_lsid,
};
use crate::mmu::{
    ACCESS_EXEC, ACCESS_RO, ACCESS_RW, ATTR_PRIVATE, ATTR_SHARED_RW, Mmu, PAGE, PAGE_SHIFT,
    PTE_ATTR_SHIFT, PTE_PA_MASK, PTE_PA_SHIFT, PTES, pte,
};
use crate::proto::*;
use crate::queue::{
    Q_DATA_OFF, Q_IN_RPOS, Q_IN_WPOS, Q_OUT_WPOS, Q_RESERVED, Q_WORDS, Ring, word_at,
};
use crate::session::VaRegion;
use core::cell::Cell;
use driver_codec::Kind;

// ---------------------------------------------------------------------------
// De testwereld.
// ---------------------------------------------------------------------------

/// Een paginagelijnd stuk heap dat de test nooit teruggeeft: registers,
/// arena en buffers liggen erin. Eén blok, omdat het PTE-formaat maar
/// veertig bits fysiek adres draagt: op een ontwikkelmachine liggen
/// heap-adressen hoger, en alleen binnen één blok zijn die hoge bits voor
/// de hele test gelijk. Op ijzer speelt dit niet.
fn heap_pages(pages: usize) -> u64 {
    let words = (pages + 1) * PAGE as usize / 8;
    let v: &'static mut [u64] = Vec::leak(vec![0u64; words]);
    (v.as_mut_ptr() as u64 + PAGE - 1) & !(PAGE - 1)
}

fn arena_of(pages: u32) -> (Arena, u64) {
    let base = heap_pages(pages as usize);
    (Arena::new(base, u64::from(pages) * PAGE).unwrap(), base)
}

fn rd(pa: u64, n: usize) -> Vec<u8> {
    let mut v = vec![0u8; n];
    dev::copy_out(&mut v, Pa(pa));
    v
}

/// Het adres zoals de VPU het uit een PTE terugleest: veertig bits.
fn seen(pa: u64) -> u64 {
    pa & ((1 << 40) - 1)
}

thread_local! {
    /// Het registerblok van de nep-VPU van deze test (tests draaien elk op
    /// een eigen draad) en een klok die per lezing een tik verder staat.
    static FAKE: Cell<(u64, u32)> = const { Cell::new((0, 0)) };
    static TICKS: Cell<u64> = const { Cell::new(0) };
}

/// De klok van de driver in de test. Hij speelt ook het ijzer: het ijzer
/// wist het TERMINATE-bit zelf, en de driver wacht daarop. In Go deed een
/// goroutine dat; hier gebeurt het bij elke lezing, op dezelfde draad.
fn fake_now() -> u64 {
    let (base, nlsid) = FAKE.with(Cell::get);
    if base != 0 {
        for id in 0..u64::from(nlsid) {
            let at = Pa(base + LSID_BASE as u64 + id * LSID_STRIDE as u64 + 0x18);
            dev::write32(at, 0);
        }
    }
    TICKS.with(|t| {
        t.set(t.get() + 1000);
        t.get()
    })
}

/// Een klok waarop het ijzer nooit antwoordt.
fn stuck_now() -> u64 {
    TICKS.with(|t| {
        t.set(t.get() + 1_000_000);
        t.get()
    })
}

/// Wat HARDWARE_ID op de Orion O6N geeft (gemeten 22-09): het model in de
/// bovenste helft, een variant eronder.
const HW_ID_O6N_MEASURED: u32 = 0x5664_8002;

/// Een VPU van geheugen: een registerblok plus een firmware die de ringen
/// bedient. De firmware vertaalt met een ECHTE page walk door het geheugen,
/// niet via de boekhouding van de driver: anders slaagt de test terwijl de
/// tabellen die de VPU leest fout staan, en dat is op ijzer beeld vol groene
/// blokken.
struct FakeVpu {
    base: u64,
    hi: u64,
    next: u64,
    end: u64,
}

impl FakeVpu {
    fn new(ncores: u32, nlsid: u32) -> FakeVpu {
        let pages = 1024;
        let start = heap_pages(pages);
        let mut f = FakeVpu {
            base: 0,
            hi: start & !((1 << 40) - 1),
            next: start,
            end: start + pages as u64 * PAGE,
        };
        f.base = f.take_pages(2);
        dev::write32(Pa(f.base), HW_ID_O6N_MEASURED);
        dev::write32(Pa(f.base + 0x30), 0x1234);
        dev::write32(Pa(f.base + 0x08), ncores);
        dev::write32(Pa(f.base + 0x0c), nlsid);
        FAKE.with(|c| c.set((f.base, nlsid)));
        f
    }

    fn take_pages(&mut self, n: usize) -> u64 {
        let pa = self.next;
        self.next += n as u64 * PAGE;
        assert!(self.next <= self.end, "testwereld is op");
        pa
    }

    fn reg(&self, off: u64) -> u32 {
        dev::read32(Pa(self.base + off))
    }

    fn lsid_reg(&self, id: u8, off: u64) -> u32 {
        self.reg(LSID_BASE as u64 + u64::from(id) * LSID_STRIDE as u64 + off)
    }

    fn back(&self, p: u32) -> u64 {
        (u64::from((p >> PTE_PA_SHIFT) & PTE_PA_MASK) << PAGE_SHIFT) | self.hi
    }

    /// Vertaalt een firmware-adres met de tabellen zoals ze in het geheugen
    /// staan: L1 uit MMU_CTRL van het slot, dan L2, dan de pagina.
    fn walk(&self, lsid: u8, va: u32) -> Option<u64> {
        let ctrl = self.lsid_reg(lsid, 0x04);
        if ctrl == 0 {
            return None;
        }
        let l1 = self.back(ctrl);
        let e = dev::read32(Pa(l1 + u64::from((va >> 22) & 0x3ff) * 4));
        if e == 0 {
            return None;
        }
        let p = dev::read32(Pa(self.back(e) + u64::from((va >> 12) & 0x3ff) * 4));
        if p == 0 {
            return None;
        }
        Some(self.back(p) + u64::from(va & 0xfff))
    }

    /// De twee pagina's van een ringpaar, gevonden via de page walk.
    fn ring_at(&self, lsid: u8, host_va: u32, mve_va: u32) -> FakeFw {
        let host = self
            .walk(lsid, host_va)
            .expect("firmware vindt zijn host-ring");
        let mve = self
            .walk(lsid, mve_va)
            .expect("firmware vindt zijn mve-ring");
        FakeFw {
            r: Ring {
                host,
                mve,
                sum: 0,
                csum: true,
            },
            sum: 0,
        }
    }

    /// Een paginagelijnde buffer uit de testwereld.
    fn buffer(&mut self, pages: usize) -> Buffer {
        let pa = self.take_pages(pages);
        Buffer {
            pa,
            size: pages as u64 * PAGE,
        }
    }
}

/// De firmware-kant van een ringpaar: precies de regels die de echte
/// firmware volgt, zodat de test het protocol bewijst en niet onze eigen
/// aannames. Hij leest kop, checksumwoord en data, verifieert de lopende som
/// zoals de firmware, en kan zelf antwoorden schrijven.
struct FakeFw {
    r: Ring,
    sum: u32,
}

impl FakeFw {
    fn take(&mut self) -> (u16, Vec<u8>) {
        let mut rpos = u32::from(dev::read16(Pa(self.r.mve + Q_IN_RPOS)));
        let wpos = u32::from(dev::read16(Pa(self.r.host + Q_IN_WPOS)));
        assert_ne!(rpos, wpos, "firmware vond een lege ring");
        let mut word = || {
            let v = dev::read32(Pa(self.r.host + Q_DATA_OFF + u64::from(rpos) * 4));
            rpos = (rpos + 1) % Q_WORDS;
            v
        };
        let hdr = word();
        let (code, size) = (hdr as u16, (hdr >> 16) as usize);
        let mut sum = hdr;
        let csum = if self.r.csum { word() } else { 0 };
        let mut data = Vec::new();
        while data.len() < size {
            let w = word();
            sum = sum.wrapping_add(w);
            data.extend_from_slice(&w.to_le_bytes());
        }
        data.truncate(size);
        if self.r.csum {
            self.sum = self.sum.wrapping_add(sum);
            assert_eq!(csum, self.sum, "checksum van code {code}");
        }
        dev::write16(Pa(self.r.mve + Q_IN_RPOS), rpos as u16);
        (code, data)
    }

    fn is_empty(&self) -> bool {
        dev::read16(Pa(self.r.mve + Q_IN_RPOS)) == dev::read16(Pa(self.r.host + Q_IN_WPOS))
    }

    fn drain(&mut self) -> Vec<u16> {
        let mut codes = Vec::new();
        while !self.is_empty() {
            codes.push(self.take().0);
        }
        codes
    }

    fn give(&self, code: u16, data: &[u8]) {
        let mut wpos = u32::from(dev::read16(Pa(self.r.mve + Q_OUT_WPOS)));
        let mut put = |v: u32| {
            dev::write32(Pa(self.r.mve + Q_DATA_OFF + u64::from(wpos) * 4), v);
            wpos = (wpos + 1) % Q_WORDS;
        };
        put(u32::from(code) | ((data.len() as u32) << 16));
        for i in (0..data.len()).step_by(4) {
            put(word_at(data, i));
        }
        dev::write16(Pa(self.r.mve + Q_OUT_WPOS), wpos as u16);
    }
}

/// De blobs van deze test.
struct TestFw(Vec<u8>);

impl Firmware for TestFw {
    fn install(&mut self, _name: &'static str, bytes: Vec<u8>) -> Result {
        self.0 = bytes;
        Ok(())
    }
    fn load(&mut self, _name: &str) -> Option<&[u8]> {
        (!self.0.is_empty()).then_some(self.0.as_slice())
    }
}

/// Een geldige firmware-binary met een bss-bitmap.
fn fw_blob(text_len: usize, bss_start: u32, bits: &[u32]) -> Vec<u8> {
    let mut b = vec![0u8; text_len];
    b[0..4].copy_from_slice(&0xeb5eu32.to_le_bytes());
    b[4] = 5; // minor 5, major 3: zoals de O6N-blobs
    b[5] = 3;
    b[8..28].copy_from_slice(b"56648002TEST Decoder");
    b[64..71].copy_from_slice(b"TESTDEC");
    b[80..92].copy_from_slice(b"r0p0-sum0005");
    b[96..100].copy_from_slice(&(text_len as u32).to_le_bytes());
    b[100..104].copy_from_slice(&bss_start.to_le_bytes());
    let mut max = 0;
    for &i in bits {
        b[108 + (i / 8) as usize] |= 1 << (i % 8);
        max = max.max(i + 1);
    }
    b[104..108].copy_from_slice(&max.to_le_bytes());
    b
}

fn graves() -> &'static Graveyard {
    Box::leak(Box::new(Graveyard::new()))
}

type Dev = Device<TestFw>;

fn probe(f: &FakeVpu, arena: Arena, fw: Vec<u8>) -> Result<Dev> {
    // SAFETY: `f.base` is een registerblok van twee pagina's (meer dan
    // BLOCK_LEN) in geheugen dat de test nooit teruggeeft; alleen deze test
    // raakt het aan, via `dev`.
    unsafe { Device::probe(f.base, arena, TestFw(fw), graves(), fake_now) }
}

fn cfg(codec: Codec, pixel: Pixel) -> Config {
    Config {
        codec,
        dir: Direction::Decode,
        pixel,
        width: 0,
        height: 0,
    }
}

/// Een HEVC-decodesessie op een nagebootste VPU met twee sessies.
fn decode_setup(pixel: Pixel) -> (Dev, Session, FakeVpu) {
    let mut f = FakeVpu::new(4, 2);
    // Ruim: de firmware vraagt in deze tests zelf geheugen, net als echt.
    let a = f.take_pages(512);
    let arena = Arena::new(a, 512 * PAGE).unwrap();
    let mut d = probe(&f, arena, fw_blob(2 * PAGE as usize, 0x4d000, &[0, 1])).unwrap();
    let s = d.open(&cfg(Codec::Hevc, pixel)).unwrap();
    (d, s, f)
}

/// De streamparameters, de alloc-maat en de flush-handdruk: daarna neemt de
/// uitvoerpoort buffers aan.
fn negotiate(d: &mut Dev, s: &Session, f: &FakeVpu, depth: u8, min: u8, w: u16, h: u16) {
    let mut msg = f.ring_at(s.id(), VA_MSG_IN_Q, VA_MSG_OUT_Q);
    msg.drain();
    let mut seq = [0u8; 8];
    seq[1] = CHROMA_YUV420 as u8;
    seq[2] = depth;
    seq[3] = depth;
    seq[4] = min;
    msg.give(RESP_SEQ_PARAMS, &seq);
    let mut alloc = [0u8; 20];
    alloc[0..2].copy_from_slice(&w.to_le_bytes());
    alloc[2..4].copy_from_slice(&h.to_le_bytes());
    msg.give(RESP_FRAME_ALLOC_PARAM, &alloc);
    let _ = d.next_event(s);
    msg.drain();
    msg.give(RESP_OUTPUT_FLUSHED, &[]);
    let _ = d.next_event(s);
}

fn events(d: &mut Dev, s: &Session, max: usize) -> Vec<Event> {
    (0..max).map_while(|_| d.next_event(s)).collect()
}

// ---------------------------------------------------------------------------
// mve_test.go: arena, MMU en firmware.
// ---------------------------------------------------------------------------

#[test]
fn arena_geeft_uitgelijnde_aaneengesloten_paginas() {
    let (mut a, _) = arena_of(64);
    assert_eq!(a.pages(), (64, 64));
    // Een blok van 64 KB landt op een grens van 64 KB, ook met iets ervoor.
    a.alloc(1, PAGE_SHIFT as u8).unwrap();
    let pa = a.alloc(16, 16).unwrap();
    assert_eq!(pa & 0xffff, 0, "64KB-aanvraag landde op {pa:#x}");
    let used = 64 - a.pages().1;
    assert!(used == 17, "{used} pagina's in gebruik");
    // Vrijgeven geeft de ruimte echt terug, ook midden in de bitmap.
    a.free(pa, 16);
    assert_eq!(a.pages().1, 63);
}

#[test]
fn arena_weigert_wat_niet_past() {
    let (mut a, _) = arena_of(8);
    assert!(a.alloc(9, PAGE_SHIFT as u8).is_err());
    for _ in 0..2 {
        a.alloc(4, PAGE_SHIFT as u8).unwrap();
    }
    assert!(a.alloc(1, PAGE_SHIFT as u8).is_err());
}

#[test]
fn arena_wist_wat_hij_uitgeeft() {
    let (mut a, base) = arena_of(4);
    dev::copy_in(Pa(base), &[0xa5; 4 * 4096]);
    let pa = a.alloc(2, PAGE_SHIFT as u8).unwrap();
    assert_eq!(rd(pa, 8), vec![0; 8]);
}

#[test]
fn arena_dubbele_free_blaast_de_teller_niet_op() {
    let (mut a, _) = arena_of(8);
    let pa = a.alloc(2, PAGE_SHIFT as u8).unwrap();
    a.free(pa, 2);
    a.free(pa, 2);
    // Meer vrijgeven dan uitgegeven: de pagina's erachter waren van niemand.
    a.free(pa, 4);
    assert_eq!(a.pages(), (8, 8));
    for _ in 0..8 {
        a.alloc(1, PAGE_SHIFT as u8).unwrap();
    }
    assert!(a.alloc(1, PAGE_SHIFT as u8).is_err());
}

#[test]
fn arena_weigert_onzinnige_uitlijning() {
    // De uitlijning komt van de firmware (een u8). Vanaf 2^44 werd de stap
    // in Go nul en liep alloc eeuwig rond; vanaf 2^32 bestaat de grens niet
    // voor een VPU met 32-bit adressen.
    let (mut a, _) = arena_of(8);
    for align in [32u8, 44, 255] {
        assert!(a.alloc(1, align).is_err(), "2^{align} geaccepteerd");
    }
    assert_eq!(a.pages().1, 8);
}

#[test]
fn mmu_vertaalt_twee_niveaus_diep() {
    let (mut a, _) = arena_of(64);
    let mut m = Mmu::reserve().unwrap();
    m.build(&mut a).unwrap();
    // Twee adressen in verschillende L1-takken: 0x1000 en 0x70000000.
    let pa1 = m.alloc(&mut a, TEXT_BASE, 2, ACCESS_EXEC).unwrap();
    let pa2 = m.alloc(&mut a, VA_SPLIT_V3, 1, ACCESS_RW).unwrap();
    for (va, want) in [
        (TEXT_BASE, seen(pa1)),
        (TEXT_BASE + 0x1000, seen(pa1 + PAGE)),
        (TEXT_BASE + 0x123, seen(pa1) + 0x123),
        (VA_SPLIT_V3, seen(pa2)),
    ] {
        assert_eq!(m.lookup(va), Some(want), "va {va:#x}");
    }
    assert_eq!(m.lookup(VA_SPLIT_V3 + 0x1000), None);
}

#[test]
fn mmu_pte_draagt_attribuut_adres_en_recht() {
    // attr<<30 | pa[39:12]<<2 | ap. Een adres boven 4 GB moet er heel door:
    // de VPU adresseert 40 bits fysiek, ook al is zijn ruimte 32 bits.
    let pa = 0x1_2345_6000u64;
    let got = pte(ATTR_PRIVATE, pa, ACCESS_RW);
    assert_eq!(got, (0x12_3456 << 2) | ACCESS_RW);
    assert_eq!(
        u64::from((got >> PTE_PA_SHIFT) & PTE_PA_MASK) << PAGE_SHIFT,
        pa
    );
    assert_eq!(
        pte(ATTR_SHARED_RW, pa, ACCESS_RO) >> PTE_ATTR_SHIFT,
        ATTR_SHARED_RW
    );
}

#[test]
fn mmu_geeft_alles_terug_bij_sluiten() {
    let (mut a, _) = arena_of(64);
    let mut m = Mmu::reserve().unwrap();
    m.build(&mut a).unwrap();
    m.alloc(&mut a, VA_SPLIT_V3, 8, ACCESS_RW).unwrap();
    assert_ne!(a.pages().1, 64, "de sessie hield niets vast");
    m.destroy(&mut a);
    assert_eq!(a.pages().1, 64, "een gesloten sessie lekt");
    assert_eq!(PTES, 1024);
}

#[test]
fn firmware_kop_komt_overeen_met_de_blob() {
    let bin = fw_blob(3 * PAGE as usize + 1, 0x4d000, &[0, 3, 40]);
    let h = fwbin::parse(&bin).unwrap();
    assert_eq!((h.protocol_major, h.protocol_minor), (3, 5));
    assert_eq!(h.part(), b"TESTDEC");
    assert!(h.has_sum());
    assert_eq!(h.text_pages(), 4, "3 pagina's plus één byte");
    for (i, want) in [
        (0, true),
        (1, false),
        (3, true),
        (40, true),
        (41, false),
        (9999, false),
    ] {
        assert_eq!(h.bss_page(i), want, "bss_page({i})");
    }
}

#[test]
fn firmware_weigert_onzin() {
    let base = || fw_blob(PAGE as usize, 0x1000, &[]);
    let mut cases: Vec<(&str, Vec<u8>)> = vec![("te kort", vec![0; 10])];
    let mut b = base();
    b[0] = 0;
    cases.push(("geen magic", b));
    let mut b = base();
    b[96..100].copy_from_slice(&(4 * PAGE as u32).to_le_bytes());
    cases.push(("text groter dan blob", b));
    let mut b = base();
    b[100..104].copy_from_slice(&0x4d123u32.to_le_bytes());
    cases.push(("bss niet uitgelijnd", b));
    let mut b = base();
    b[5] = 9;
    cases.push(("onbekend protocol", b));
    for (naam, b) in cases {
        assert!(fwbin::parse(&b).is_err(), "{naam} werd geaccepteerd");
    }
}

#[test]
fn firmware_landt_op_de_juiste_adressen() {
    let (mut a, _) = arena_of(64);
    let mut m = Mmu::reserve().unwrap();
    m.build(&mut a).unwrap();
    let mut bin = fw_blob(2 * PAGE as usize, 0x4d000, &[0, 2]);
    for (i, b) in bin.iter_mut().enumerate().skip(HEADER_LEN) {
        *b = i as u8;
    }
    let h = fwbin::parse(&bin).unwrap();
    let pa = fwbin::load(&mut m, &mut a, &bin, &h).unwrap();
    assert_eq!(m.lookup(TEXT_BASE), Some(seen(pa)));
    assert_eq!(rd(pa, 8), bin[..8]);
    assert!(m.lookup(0x4d000).is_some(), "bss-pagina 0 ontbreekt");
    assert!(
        m.lookup(0x4e000).is_none(),
        "bss-pagina 1 stond niet in de bitmap"
    );
    assert!(m.lookup(0x4f000).is_some(), "bss-pagina 2 ontbreekt");
}

// ---------------------------------------------------------------------------
// queue_test.go: het ringprotocol.
// ---------------------------------------------------------------------------

fn test_ring(csum: bool) -> (Ring, FakeFw) {
    let base = heap_pages(2);
    let r = Ring {
        host: base,
        mve: base + PAGE,
        sum: 0,
        csum,
    };
    (r, FakeFw { r, sum: 0 })
}

#[test]
fn ring_bericht_komt_ongeschonden_aan() {
    let (mut r, mut fw) = test_ring(true);
    let payload = [1u8, 2, 3, 4, 5, 6, 7];
    r.send(REQ_JOB, &payload).unwrap();
    assert_eq!(fw.take(), (REQ_JOB, payload.to_vec()));
}

#[test]
fn ring_checksum_loopt_door_over_berichten_heen() {
    // De val voor een nieuwe driver: de som is cumulatief, niet per bericht.
    let (mut r, mut fw) = test_ring(true);
    for p in [&[0xaau8][..], &[], &[1, 2, 3, 4, 5]] {
        r.send(REQ_IDLE_ACK, p).unwrap();
        assert_eq!(fw.take().1, p);
    }
    // En de laatste som staat in reserved[2]: daar kijkt de firmware.
    assert_eq!(dev::read32(Pa(r.host + Q_RESERVED + 8)), fw.sum);
}

#[test]
fn ring_loopt_rond_zonder_bericht_te_breken() {
    // Na een paar honderd berichten is de ring van 1020 woorden meermalen
    // omgelopen; een bericht over de grens komt heel aan.
    let (mut r, mut fw) = test_ring(true);
    let mut payload = [0u8; 9];
    for n in 0..700usize {
        for (i, b) in payload.iter_mut().enumerate() {
            *b = (n + i) as u8;
        }
        r.send(REQ_IDLE_ACK, &payload).unwrap();
        assert_eq!(fw.take(), (REQ_IDLE_ACK, payload.to_vec()), "ronde {n}");
    }
}

#[test]
fn ring_weigert_wat_niet_past() {
    let (mut r, _) = test_ring(true);
    assert!(matches!(
        r.send(REQ_IDLE_ACK, &[0; Q_WORDS as usize * 4]),
        Err(Error::MsgTooBig { .. })
    ));
    // Zonder lezer loopt de ring vol en weigert dan; nooit stil overschrijven.
    let mut sent = 0;
    loop {
        match r.send(REQ_IDLE_ACK, &[0; 400]) {
            Ok(()) => sent += 1,
            Err(Error::QueueFull) => break,
            Err(e) => panic!("na {sent} berichten: {e}"),
        }
        assert!(sent < 2000, "ring weigerde nooit");
    }
    assert!(sent > 0);
}

#[test]
fn ring_leest_antwoorden_van_de_firmware() {
    let (mut r, fw) = test_ring(true);
    let body: Vec<u8> = (0..12).map(|i| 0x40 + i).collect();
    fw.give(RESP_SEQ_PARAMS, &body);
    fw.give(RESP_OUTPUT, &[]);
    let mut dst = [0u8; 64];
    assert_eq!(r.recv(&mut dst).unwrap(), Some((RESP_SEQ_PARAMS, 12)));
    assert_eq!(&dst[..12], &body[..]);
    assert_eq!(r.recv(&mut dst).unwrap(), Some((RESP_OUTPUT, 0)));
    assert_eq!(r.recv(&mut dst).unwrap(), None);
}

#[test]
fn ring_weigert_onzinnige_code() {
    let (mut r, fw) = test_ring(true);
    fw.give(42, &[]);
    assert_eq!(r.recv(&mut [0; 8]), Err(Error::MsgUnknown { code: 42 }));
}

#[test]
fn ring_weigert_positie_buiten_de_ring() {
    // De posities komen uit geheugen dat de firmware beschrijft; een woord
    // achter de 1020 ligt op de volgende pagina.
    let (mut r, _) = test_ring(true);
    dev::write16(Pa(r.mve + Q_OUT_WPOS), Q_WORDS as u16 + 5);
    assert!(matches!(r.recv(&mut [0; 64]), Err(Error::QueuePos { .. })));
    dev::write16(Pa(r.mve + Q_IN_RPOS), 0xffff);
    assert!(matches!(r.send(REQ_JOB, &[]), Err(Error::QueuePos { .. })));
}

// ---------------------------------------------------------------------------
// session_test.go: de sessie op een nep-VPU.
// ---------------------------------------------------------------------------

#[test]
fn probe_leest_de_geometrie_en_weigert_vreemd_ijzer() {
    let mut f = FakeVpu::new(4, 2);
    let a = f.take_pages(64);
    let d = probe(&f, Arena::new(a, 64 * PAGE).unwrap(), Vec::new()).unwrap();
    assert_eq!((d.cores(), d.sessions()), (4, 2));
    assert_eq!(f.reg(0x04), 1, "scheduler staat niet aan na probe");
    assert_eq!(f.reg(0x14), 0x0f0f_0f0f, "job queue niet leeggemaakt");
    dev::write32(Pa(f.base), 0x5650_0000); // een oudere Mali-V500
    let r = probe(&f, Arena::new(a, 64 * PAGE).unwrap(), Vec::new());
    assert!(matches!(r, Err(Error::Hardware { id: 0x5650_0000 })));
}

#[test]
fn open_zet_de_sessie_op_het_ijzer() {
    let (mut d, s, f) = decode_setup(Pixel::Nv12);
    let id = s.id();
    assert_eq!(f.lsid_reg(id, 0x0c), ALLOC_NON_PROTECTED);
    assert_eq!(f.lsid_reg(id, 0x14), 1, "niet ingepland");
    assert_eq!(f.lsid_reg(id, 0x08), 1, "niet in de gewone modus");
    assert_eq!((f.lsid_reg(id, 0x00) >> CTRL_MAX_CORES_SHIFT) & 0xf, 1);
    // De job staat in een plaats, met deze sessie en één core.
    let q = f.reg(0x14);
    let slot = (0..JOB_SLOTS).find(|&i| job_slot_lsid(q, i) == u32::from(id));
    let i = slot.expect("sessie staat niet in de job queue");
    assert_eq!((q >> (i * 8 + 4)) & 0xf, 1);
    // De firmware vindt zijn startsein en zijn eigen code.
    let mut msg = f.ring_at(id, VA_MSG_IN_Q, VA_MSG_OUT_Q);
    assert_eq!(msg.take().0, REQ_GO);
    let text = f
        .walk(id, TEXT_BASE)
        .expect("firmware niet op zijn laadadres");
    assert_eq!(rd(text, 4), 0xeb5eu32.to_le_bytes());
    // Twee sessies passen, een derde niet.
    let s2 = d.open(&cfg(Codec::H264, Pixel::Nv12)).unwrap();
    assert_eq!(
        d.open(&cfg(Codec::Vp9, Pixel::Nv12)).err(),
        Some(Error::Busy)
    );
    d.close(s2);
    d.close(s);
}

#[test]
fn decode_loopt_van_bitstream_naar_frame() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    let id = s.id();
    let mut msg = f.ring_at(id, VA_MSG_IN_Q, VA_MSG_OUT_Q);
    let mut bin = f.ring_at(id, VA_BUF_IN_Q, VA_BUF_IN_RQ);
    let mut bout = f.ring_at(id, VA_BUF_OUT_Q, VA_BUF_OUT_RQ);
    msg.take(); // het GO van open

    // 4K, 8 bit, 4:2:0, zes tegelijk vast te houden.
    let mut seq = [0u8; 8];
    seq[1] = CHROMA_YUV420 as u8;
    seq[2] = 8;
    seq[3] = 8;
    seq[4] = 6;
    msg.give(RESP_SEQ_PARAMS, &seq);
    let mut alloc = [0u8; 20];
    alloc[0..2].copy_from_slice(&3840u16.to_le_bytes());
    alloc[2..4].copy_from_slice(&2176u16.to_le_bytes()); // op macroblokken
    alloc[18..20].copy_from_slice(&16u16.to_le_bytes()); // 2160 zichtbaar
    msg.give(RESP_FRAME_ALLOC_PARAM, &alloc);

    let ev = d.next_event(&s).expect("geen formaat-event");
    assert_eq!(ev.kind, Kind::Format);
    // Vóór het eerste frame is de gealloceerde maat wat we weten.
    let l = ev.layout;
    assert_eq!(
        (l.width, l.height, l.alloc_width, l.alloc_height),
        (3840, 2176, 3840, 2176)
    );
    assert_eq!(l.frame_size, 3840 * 2176 * 3 / 2);
    assert_eq!(l.min_buffers, 6);
    assert_eq!((l.planes[0].stride, l.planes[1].off), (3840, 3840 * 2176));

    // Een stukje bitstream erin.
    let inb = f.buffer(2);
    dev::copy_in(Pa(inb.pa), &[0, 0, 1, 0x26, 0x01, 0x02]);
    d.feed(&s, inb, 6, Flags(0), 0xcafe).unwrap();
    let (code, desc) = bin.take();
    assert_eq!(code, BUF_BITSTREAM);
    assert_eq!(get64(&desc, BS_USER_TAG), 0xcafe);
    assert_eq!(get32(&desc, BS_FILLED_LEN), 6);
    // De firmware leest de bytes op het adres uit de descriptor: de echte
    // proef op de mapping van een buffer van de aanroeper.
    let src = f
        .walk(id, get32(&desc, BS_BUF_ADDR))
        .expect("invoer onvindbaar");
    assert_eq!(rd(src, 6), [0, 0, 1, 0x26, 0x01, 0x02]);

    // Na de streamparameters wacht de firmware op een output-flush; die moet
    // de driver uit zichzelf gestuurd hebben.
    assert!(msg.drain().contains(&REQ_OUTPUT_FLUSH), "geen output-flush");

    // Een framebuffer aanbieden: de poort staat nog stil.
    let outb = f.buffer(4);
    d.offer(&s, outb).unwrap();
    assert!(bout.is_empty(), "aangeboden terwijl de poort stilstond");
    msg.give(RESP_OUTPUT_FLUSHED, &[]);
    let _ = d.next_event(&s);
    let (code, mut fdesc) = bout.take();
    assert_eq!(code, BUF_FRAME);
    assert_eq!(get16(&fdesc, BF_FORMAT), FMT_NV12);

    // De firmware "decodeert": pixels op het luma-adres, buffer terug met de
    // zichtbare maat.
    let dst = f
        .walk(id, get32(&fdesc, BF_PLANE_TOP))
        .expect("frame onvindbaar");
    dev::copy_in(Pa(dst), &[0x10, 0x20, 0x30, 0x40]);
    put(&mut fdesc, BF_VISIBLE_WIDTH, &3840u16.to_le_bytes());
    put(&mut fdesc, BF_VISIBLE_HEIGHT, &2160u16.to_le_bytes());
    put(&mut fdesc, BF_USER_TAG, &0xcafeu64.to_le_bytes());
    bout.give(BUF_FRAME, &fdesc);
    bin.give(BUF_BITSTREAM, &desc);

    let (mut consumed, mut produced) = (false, false);
    for ev in events(&mut d, &s, 8) {
        match ev.kind {
            Kind::Consumed => {
                consumed = true;
                assert_eq!((ev.buf, ev.tag), (Some(inb), 0xcafe));
            }
            Kind::Produced => {
                produced = true;
                assert_eq!(ev.tag, 0xcafe, "tijdstempel van de invoer kwijt");
                assert_eq!(ev.buf, Some(outb));
                assert_eq!((ev.layout.width, ev.layout.height), (3840, 2160));
            }
            Kind::Fault => panic!("sessie viel om: {:?}", ev.fault),
            _ => {}
        }
    }
    assert!(
        consumed && produced,
        "consumed={consumed} produced={produced}"
    );
    assert_eq!(
        rd(outb.pa, 4),
        [0x10, 0x20, 0x30, 0x40],
        "pixels niet in de app-buffer"
    );
    d.close(s);
}

fn rpc_call(d: &mut Dev, s: &Session, f: &FakeVpu, call: u32, params: &[u32]) -> u32 {
    let rpc = f.walk(s.id(), VA_RPC).expect("RPC-pagina onvindbaar");
    dev::write32(Pa(rpc + RPC_CALL_ID), call);
    for (i, p) in params.iter().enumerate() {
        dev::write32(Pa(rpc + RPC_PARAMS + 4 * i as u64), *p);
    }
    dev::write32(Pa(rpc + RPC_STATE), RPC_STATE_PARAM);
    let _ = d.next_event(s);
    assert_eq!(
        dev::read32(Pa(rpc + RPC_STATE)),
        RPC_STATE_RETURN,
        "RPC {call}"
    );
    dev::read32(Pa(rpc + RPC_PARAMS))
}

#[test]
fn firmware_vraagt_geheugen_en_krijgt_het() {
    let (mut d, s, f) = decode_setup(Pixel::Nv12);
    // Zo vraagt een decoder zijn referentieframes: een blok nu, ruimte om te
    // groeien, uitgelijnd op 64 KB.
    let fb = u32::from(RPC_REGION_FRAMEBUF) | (16 << 8);
    let va = rpc_call(&mut d, &s, &f, RPC_ALLOC, &[8 * 4096, 32 * 4096, fb]);
    assert_ne!(va, 0, "firmware kreeg geen geheugen");
    assert_eq!(va & 0xffff, 0, "blok op {va:#x} niet op 64 KB");
    assert!((VA_SPLIT_V3..VA_FRAME_END_V3).contains(&va));
    let pa = f.walk(s.id(), va).expect("blok staat niet in de tabel");
    dev::write32(Pa(pa), 0x5a5a_5a5a);
    // Groeien binnen de span geeft hetzelfde adres: de firmware houdt de
    // oude inhoud vast.
    assert_eq!(rpc_call(&mut d, &s, &f, RPC_RESIZE, &[va, 20 * 4096]), va);
    assert!(f.walk(s.id(), va + 19 * 4096).is_some());
    assert_eq!(dev::read32(Pa(pa)), 0x5a5a_5a5a, "resize gooide inhoud weg");
    // Bij sluiten gaat ALLES terug: tabellen, firmware, ringen en wat de
    // firmware zelf vroeg.
    d.close(s);
    let (total, free) = d.arena().pages();
    assert_eq!(free, total, "de sessie lekt");
}

#[test]
fn sessie_weigert_verder_na_een_firmware_fout() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    let mut msg = f.ring_at(s.id(), VA_MSG_IN_Q, VA_MSG_OUT_Q);
    msg.take();
    let mut body = vec![0u8; 20];
    body[..4].copy_from_slice(&9u32.to_le_bytes()); // de watchdog van de firmware
    body[4..12].copy_from_slice(b"watchdog");
    msg.give(RESP_ERROR, &body);
    let ev = d.next_event(&s).expect("fout kwam niet door");
    assert_eq!(
        (ev.kind, ev.fault),
        (Kind::Fault, Some(Error::Firmware { code: 9 }))
    );
    let b = f.buffer(1);
    assert!(d.feed(&s, b, 1, Flags(0), 0).is_err(), "werk na een fout");
    d.close(s);
}

/// De tweedeling die de stille uitvoerring veroorzaakte: bitstream in
/// protected, pixels in framebuf, met de grens van v3 op 0x70000000.
#[test]
fn buffers_landen_in_de_juiste_firmware_regio() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    let mut bin = f.ring_at(s.id(), VA_BUF_IN_Q, VA_BUF_IN_RQ);
    let mut bout = f.ring_at(s.id(), VA_BUF_OUT_Q, VA_BUF_OUT_RQ);
    let inb = f.buffer(2);
    d.feed(&s, inb, 6, Flags(0), 1).unwrap();
    let va = get32(&bin.take().1, BS_BUF_ADDR);
    assert!(
        (VA_PROTECTED_BEG..VA_SPLIT_V3).contains(&va),
        "bitstream op {va:#x}"
    );
    let outb = f.buffer(4);
    d.offer(&s, outb).unwrap();
    let va = get32(&bout.take().1, BF_PLANE_TOP);
    assert!(
        (VA_SPLIT_V3..VA_FRAME_END_V3).contains(&va),
        "frame op {va:#x}"
    );
    d.close(s);
}

/// Dezelfde driver bedient een v2-blob nog; daar ligt de knip op 0x50000000.
#[test]
fn oude_firmware_krijgt_de_oude_grens() {
    let (v2, v3) = (regions_for(2), regions_for(3));
    assert_eq!(
        (v2.prot_end, v2.frame_beg, v2.frame_end),
        (VA_SPLIT_V2, VA_SPLIT_V2, VA_FRAME_END_V2)
    );
    assert_eq!(
        (v3.prot_end, v3.frame_beg, v3.frame_end),
        (VA_SPLIT_V3, VA_SPLIT_V3, VA_FRAME_END_V3)
    );
    assert_eq!(
        (v2.prot_beg, v3.prot_beg),
        (VA_PROTECTED_BEG, VA_PROTECTED_BEG)
    );
}

/// De handdruk uit `mve_protocol_def.h`: na SEQUENCE_PARAMETERS doet de
/// firmware niets tot een output-flush, geeft dan zijn uitvoerbuffers terug,
/// en pas ná OUTPUT_FLUSHED mag de host opnieuw aanbieden. Die teruggave is
/// GEEN beeld.
#[test]
fn uitvoerpoort_wacht_op_de_flush() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    let mut msg = f.ring_at(s.id(), VA_MSG_IN_Q, VA_MSG_OUT_Q);
    let mut bout = f.ring_at(s.id(), VA_BUF_OUT_Q, VA_BUF_OUT_RQ);
    msg.drain();
    let outb = f.buffer(4);
    d.offer(&s, outb).unwrap();
    let (code, first) = bout.take();
    assert_eq!(code, BUF_FRAME);
    let mut seq = [0u8; 8];
    seq[1] = CHROMA_YUV420 as u8;
    seq[2] = 8;
    seq[4] = 3;
    msg.give(RESP_SEQ_PARAMS, &seq);
    let _ = d.next_event(&s);
    assert!(msg.drain().contains(&REQ_OUTPUT_FLUSH), "geen output-flush");
    let second = f.buffer(4);
    d.offer(&s, second).unwrap();
    assert!(bout.is_empty(), "aangeboden terwijl de poort stilstond");
    bout.give(BUF_FRAME, &first);
    msg.give(RESP_OUTPUT_FLUSHED, &[]);
    if let Some(ev) = d.next_event(&s) {
        panic!(
            "teruggegeven flush-buffer kwam naar buiten als {:?}",
            ev.kind
        );
    }
    let mut seen = 0;
    while !bout.is_empty() {
        if bout.take().0 == BUF_FRAME {
            seen += 1;
        }
    }
    assert_eq!(seen, 2, "beide buffers horen na de flush aangeboden");
    d.close(s);
}

/// De firmware zet EOS ÓP het laatste beeld; wie daar alleen Done van maakt
/// levert elke stream één beeld te weinig.
#[test]
fn laatste_beeld_gaat_niet_verloren_op_eos() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    negotiate(&mut d, &s, &f, 8, 1, 64, 64);
    let mut bout = f.ring_at(s.id(), VA_BUF_OUT_Q, VA_BUF_OUT_RQ);
    let outb = f.buffer(4);
    d.offer(&s, outb).unwrap();
    let (code, mut desc) = bout.take();
    assert_eq!(code, BUF_FRAME);
    put(&mut desc, BF_FLAGS, &FR_FLAG_EOS.to_le_bytes());
    put(&mut desc, BF_VISIBLE_WIDTH, &64u16.to_le_bytes());
    put(&mut desc, BF_VISIBLE_HEIGHT, &64u16.to_le_bytes());
    bout.give(BUF_FRAME, &desc);
    let (mut frame, mut done) = (false, false);
    for ev in events(&mut d, &s, 8) {
        match ev.kind {
            Kind::Produced => {
                frame = true;
                assert_eq!(ev.buf.map(|b| b.pa), Some(outb.pa));
            }
            Kind::Done => {
                done = true;
                assert!(frame, "Done kwam vóór het laatste beeld");
            }
            _ => {}
        }
    }
    assert!(frame, "het laatste beeld ging verloren op EOS");
    assert!(done, "geen Done na het laatste beeld");
    d.close(s);
}

#[test]
fn corrupt_frame_fails_instead_of_publishing_pixels() {
    let (mut d, s, mut f) = decode_setup(Pixel::P010);
    negotiate(&mut d, &s, &f, 10, 1, 64, 64);
    let mut bout = f.ring_at(s.id(), VA_BUF_OUT_Q, VA_BUF_OUT_RQ);
    let b = f.buffer(4);
    d.offer(&s, b).unwrap();
    let (_, mut desc) = bout.take();
    put(&mut desc, BF_FLAGS, &FR_FLAG_CORRUPT.to_le_bytes());
    bout.give(BUF_FRAME, &desc);
    let evs = events(&mut d, &s, 8);
    assert!(
        evs.iter().all(|e| e.kind != Kind::Produced),
        "corrupte pixels gepubliceerd"
    );
    assert!(
        evs.iter().any(|e| e.fault == Some(Error::CorruptFrame)),
        "geen expliciete decoderfout"
    );
    d.close(s);
}

/// Wat een MMU ABORT op 4K kostte: dezelfde fysieke buffer houdt dezelfde
/// plek, veertig rondes lang.
#[test]
fn zelfde_buffer_krijgt_zelfde_adres() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    negotiate(&mut d, &s, &f, 8, 1, 64, 64);
    let mut bout = f.ring_at(s.id(), VA_BUF_OUT_Q, VA_BUF_OUT_RQ);
    let outb = f.buffer(4);
    let mut first = None;
    for round in 0..40 {
        d.offer(&s, outb).unwrap();
        let (code, mut desc) = bout.take();
        assert_eq!(code, BUF_FRAME, "ronde {round}");
        let va = get32(&desc, BF_PLANE_TOP);
        let pa = f.walk(s.id(), va).expect("niet in de tabel");
        match first {
            None => first = Some((va, pa)),
            Some(f0) => assert_eq!((va, pa), f0, "ronde {round}: de plek moet vast blijven"),
        }
        put(&mut desc, BF_VISIBLE_WIDTH, &64u16.to_le_bytes());
        put(&mut desc, BF_VISIBLE_HEIGHT, &64u16.to_le_bytes());
        bout.give(BUF_FRAME, &desc);
        let _ = events(&mut d, &s, 4);
    }
    d.close(s);
}

/// Blijft een plek eeuwig staan, dan loopt een aanroeper met telkens verse
/// buffers alsnog vast: wat niet bij het ijzer ligt moet kunnen wijken.
#[test]
fn ruimte_komt_vrij_voor_een_andere_buffer() {
    let mut r = VaRegion::default();
    r.reset(VA_SPLIT_V3, VA_SPLIT_V3 + 8 * 4096);
    let a = r.alloc(4).unwrap();
    let b = r.alloc(4).unwrap();
    assert_eq!(r.alloc(4), None, "ruimte uitgegeven die er niet is");
    r.put(a, 4);
    assert_eq!(r.alloc(4), Some(a), "vrijgegeven ruimte kwam niet terug");
    assert_ne!(a, b);
}

/// Elke resize hangt een eigen stuk achter het blok; de tweede gaat achter
/// de EERSTE verder, en bij vrijgeven geeft elk stuk precies zijn eigen
/// pagina's terug.
#[test]
fn twee_resizes_en_vrijgeven_kloppen_tot_de_pagina() {
    let (mut d, s, f) = decode_setup(Pixel::Nv12);
    let total = d.arena().pages().0;
    let fb = u32::from(RPC_REGION_FRAMEBUF) | (PAGE_SHIFT << 8);
    for b in 0..2 {
        let voor = d.arena().pages().1;
        let va = rpc_call(&mut d, &s, &f, RPC_ALLOC, &[4 * 4096, 32 * 4096, fb]);
        assert_ne!(va, 0, "blok {b}: geen geheugen");
        let na_alloc = d.arena().pages().1; // inclusief een eventuele L2-pagina
        let mark = |from: u32, to: u32| {
            for i in from..to {
                let pa = f
                    .walk(s.id(), va + i * 4096)
                    .expect("pagina niet in de tabel");
                dev::write32(Pa(pa), 0xb10c_0000 | i);
            }
        };
        mark(0, 4);
        for size in [8u32, 16] {
            assert_eq!(rpc_call(&mut d, &s, &f, RPC_RESIZE, &[va, size * 4096]), va);
            mark(size / 2, size);
        }
        let mut pas = Vec::new();
        for i in 0..16 {
            let pa = f.walk(s.id(), va + i * 4096).unwrap();
            assert!(
                !pas.contains(&pa),
                "blok {b}: pagina {i} deelt zijn fysieke pagina"
            );
            pas.push(pa);
            assert_eq!(
                dev::read32(Pa(pa)),
                0xb10c_0000 | i,
                "resize mapte over levend geheugen"
            );
        }
        assert_eq!(
            na_alloc - d.arena().pages().1,
            12,
            "twee resizes kostten 12 pagina's"
        );
        if b == 0 {
            rpc_call(&mut d, &s, &f, RPC_FREE, &[va]);
            let vrij = d.arena().pages().1;
            assert!(vrij == na_alloc + 4 && vrij <= voor, "na free {vrij} vrij");
        }
    }
    d.close(s);
    assert_eq!(d.arena().pages().1, total, "een geresized blok lekt");
}

/// Na close zijn de ringen en de RPC-pagina terug in de arena. In Go kon een
/// Poll daarna nog pompen; in Rust kán het niet (close neemt het handvat),
/// dus hier de rest van de belofte: de andere sessie raakt het vrijgegeven
/// geheugen niet aan, en de deurbel van "slot -1" (0x1e0) blijft stil.
#[test]
fn na_close_raakt_niemand_het_geheugen_nog_aan() {
    let (mut d, s, f) = decode_setup(Pixel::Nv12);
    let s2 = d.open(&cfg(Codec::H264, Pixel::Nv12)).unwrap();
    let rpc = f.walk(s.id(), VA_RPC).unwrap();
    let msg = f.ring_at(s.id(), VA_MSG_IN_Q, VA_MSG_OUT_Q);
    d.close(s);
    let vrij = d.arena().pages().1;
    dev::write32(Pa(rpc + RPC_CALL_ID), RPC_ALLOC);
    dev::write32(Pa(rpc + RPC_PARAMS), 4 * 4096);
    dev::write32(Pa(rpc + RPC_STATE), RPC_STATE_PARAM);
    msg.give(RESP_STATE_CHANGE, &[0; 4]);
    while d.next_event(&s2).is_some() {}
    assert_eq!(dev::read32(Pa(rpc + RPC_STATE)), RPC_STATE_PARAM);
    assert_eq!(d.arena().pages().1, vrij, "gesloten sessie alloceerde nog");
    assert_eq!(f.reg((LSID_BASE - LSID_STRIDE + 0x20) as u64), 0);
    d.close(s2);
}

/// Drop is de vrijgave: een handvat dat valt zonder close (een app die
/// omvalt, een levensduur die de kern opruimt) sluit de sessie bij de
/// volgende beurt van het device.
#[test]
fn een_gevallen_handvat_sluit_de_sessie() {
    let (mut d, s, _f) = decode_setup(Pixel::Nv12);
    let (total, _) = d.arena().pages();
    let id = s.id();
    drop(s);
    assert_ne!(
        d.arena().pages().1,
        total,
        "nog niet opgeruimd vóór de beurt"
    );
    d.reap();
    assert_eq!(d.arena().pages().1, total, "het gevallen handvat lekt");
    // Het slot is weer vrij: een nieuwe sessie krijgt het.
    let s = d.open(&cfg(Codec::Hevc, Pixel::Nv12)).unwrap();
    assert_eq!(s.id(), id);
    d.close(s);
}

/// Een handvat van een ander device is voor dit device dicht.
#[test]
fn een_vreemd_handvat_is_dicht() {
    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    let vreemd = Session::new(s.id(), graves());
    let b = f.buffer(1);
    assert_eq!(d.feed(&vreemd, b, 1, Flags(0), 0), Err(Error::Closed));
    assert!(d.next_event(&vreemd).is_none());
    d.close(vreemd);
    assert!(
        d.feed(&s, b, 1, Flags(0), 0).is_ok(),
        "de echte sessie ging dicht"
    );
    d.close(s);
}

/// Een slot dat niet afbreekt (het beeld van 27-09) is een fout met het
/// slotnummer, en de half gestarte sessie geeft alles terug.
#[test]
fn een_vast_slot_is_een_fout_en_lekt_niet() {
    let mut f = FakeVpu::new(4, 1);
    let a = f.take_pages(128);
    let fw = fw_blob(2 * PAGE as usize, 0x4d000, &[0]);
    // SAFETY: zie `probe`.
    let mut d = unsafe {
        Device::probe(
            f.base,
            Arena::new(a, 128 * PAGE).unwrap(),
            TestFw(fw),
            graves(),
            stuck_now,
        )
    }
    .unwrap();
    dev::write32(Pa(f.base + LSID_BASE as u64 + 0x18), 1);
    assert_eq!(
        d.open(&cfg(Codec::Hevc, Pixel::Nv12)).err(),
        Some(Error::Terminate { lsid: 0 })
    );
    assert_eq!(d.arena().pages().1, 128);
}

/// Maten en uitlijningen komen van de firmware of de aanroeper en lopen in
/// 32 bits makkelijk over; wat overloopt wordt geweigerd.
#[test]
fn onzinnige_maten_worden_geweigerd() {
    let mut r = VaRegion::default();
    r.reset(VA_SPLIT_V3, VA_FRAME_END_V3);
    assert_eq!(r.alloc(0x10_0001), None, "4GB+4KB liep over tot 4KB");
    assert_eq!(
        r.next, VA_SPLIT_V3,
        "geweigerde aanvraag schoof de regio op"
    );

    let (mut d, s, mut f) = decode_setup(Pixel::Nv12);
    let mut b = f.buffer(1);
    b.size = (1 << 32) + 4096; // afgekapt in 32 bits: één pagina
    assert!(d.feed(&s, b, 1, Flags(0), 0).is_err());

    let vrij = d.arena().pages().1;
    let fb = u32::from(RPC_REGION_FRAMEBUF);
    for (naam, size, max, tail) in [
        ("uitlijning 2^44", 4 * 4096, 4 * 4096, fb | (44 << 8)),
        ("uitlijning 2^32", 4 * 4096, 4 * 4096, fb | (32 << 8)),
        ("max_size bijna 4GB", 4096, 0xffff_ffff, fb | (16 << 8)),
        ("size bijna 4GB", 0xffff_f001, 0, fb | (12 << 8)),
    ] {
        assert_eq!(
            rpc_call(&mut d, &s, &f, RPC_ALLOC, &[size, max, tail]),
            0,
            "{naam}"
        );
        assert_eq!(d.arena().pages().1, vrij, "{naam}: kostte toch pagina's");
    }
    d.close(s);
}

#[test]
fn describe_en_state_dragen_de_getallen() {
    let (d, s, _f) = decode_setup(Pixel::Nv12);
    let line = format!("{}", driver_codec::Show(&d, |d, f| d.describe(f)));
    assert!(
        line.contains("0x56648002") && line.contains("2 sessions"),
        "{line}"
    );
    let st = format!("{}", driver_codec::Show(&d, |d, f| d.state(f)));
    assert!(st.contains("lsid0") && st.contains("sched=1"), "{st}");
    drop(s);
}

/// De lijst die de kern van het volume leest, is precies wat `fw_name` ooit
/// vraagt: geen blob te veel (die niemand opent) en geen te weinig (een
/// codec die altijd "no firmware" zou zeggen).
#[test]
fn de_firmwarelijst_is_die_van_fw_name() {
    let mut gevraagd: Vec<&str> = Vec::new();
    for dir in [Direction::Decode, Direction::Encode] {
        for c in Codec::ALL {
            if let Some(n) = fw_name(c, dir) {
                gevraagd.push(n);
            }
        }
    }
    assert_eq!(gevraagd.as_slice(), FIRMWARE.as_slice());
}

#[test]
fn missing_firmware_can_be_loaded_without_reprobing_the_device() {
    let mut f = FakeVpu::new(4, 2);
    let a = f.take_pages(512);
    let arena = Arena::new(a, 512 * PAGE).unwrap();
    let mut d = probe(&f, arena, Vec::new()).unwrap();
    let config = cfg(Codec::Hevc, Pixel::Nv12);
    assert_eq!(d.firmware_needed(&config), Some("hevcdec"));
    assert!(matches!(d.open(&config), Err(Error::NoFirmware)));
    assert!(d.install_firmware("hevcdec", vec![1; 300]).is_err());
    assert_eq!(d.firmware_needed(&config), Some("hevcdec"));
    let bin = fw_blob(2 * PAGE as usize, 0x4d000, &[0, 1]);
    assert!(d.install_firmware("../hevcdec", bin.clone()).is_err());
    d.install_firmware("hevcdec", bin).unwrap();
    assert_eq!(d.firmware_needed(&config), None);
    let s = d.open(&config).unwrap();
    d.close(s);
}
